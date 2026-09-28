// Package trend implements trendbot's strategy: diversified multi-asset
// trend following with momentum rotation and volatility targeting. It uses
// no LLM and no discretionary input: every decision is a pure function of
// past prices.
//
// Rules (evaluated on the first trading day of each month by default):
//  1. Momentum score per asset = the average of its 1-, 3-, 6- and 12-month
//     total returns (Keller & Keuning's "13612U" blend).
//  2. Absolute momentum: an asset is eligible only if its score beats the
//     cash asset's score (Antonacci's dual momentum).
//  3. Relative momentum: hold the top_k eligible assets (default 4 slots).
//     Empty slots stay in cash, so exposure falls automatically as trends
//     break across asset classes.
//  4. Weights: inverse 63-day volatility within the filled slots, capped per
//     asset, then scaled down if ex-ante portfolio volatility exceeds the
//     target. Everything not invested sits in the cash ETF (T-bills).
package trend

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	ind "github.com/midhunvinay1/tradebot/internal/indicators"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/strategy"
)

const Sleeve = "trend"

// State is persisted between live runs.
type State struct {
	LastRebalance time.Time          `json:"last_rebalance"`
	Targets       map[string]float64 `json:"targets"`
	Reasons       map[string]string  `json:"reasons"`
	RiskOn        bool               `json:"risk_on"`
	ExAnteVol     float64            `json:"ex_ante_vol"`
	VolScale      float64            `json:"vol_scale"`
}

type Strategy struct {
	cfg      *config.Config
	St       *State
	lookback int
}

func New(cfg *config.Config, st *State) *Strategy {
	if st == nil {
		st = &State{}
	}
	if st.Targets == nil {
		st.Targets = map[string]float64{}
	}
	if st.Reasons == nil {
		st.Reasons = map[string]string{}
	}
	lb := cfg.Trend.VolWindow + 1
	for _, l := range cfg.Trend.Lookbacks {
		lb = max(lb, l+1)
	}
	return &Strategy{cfg: cfg, St: st, lookback: lb}
}

func (s *Strategy) State() any { return s.St }

// Reconcile is a no-op: targets persist until the next rebalance, and any
// unfilled order is simply re-sent by the next day's diff.
func (s *Strategy) Reconcile(map[string]float64) {}

// MarkRejected is a no-op for the same reason.
func (s *Strategy) MarkRejected(string) {}

func (s *Strategy) due(date time.Time) bool {
	last := s.St.LastRebalance
	if last.IsZero() {
		return true
	}
	if s.cfg.Trend.Rebalance == "week" {
		y1, w1 := last.ISOWeek()
		y2, w2 := date.ISOWeek()
		return y1 != y2 || w1 != w2
	}
	return last.Year() != date.Year() || last.Month() != date.Month()
}

// Score is the blended momentum of closes (NaN without enough history).
func Score(closes []float64, lookbacks []int) float64 {
	sum := 0.0
	for _, lb := range lookbacks {
		r := ind.Return(closes, lb, 0)
		if math.IsNaN(r) {
			return math.NaN()
		}
		sum += r
	}
	return sum / float64(len(lookbacks))
}

// Plan returns target weights at the close of the last benchmark bar.
func (s *Strategy) Plan(data market.Dataset) (*strategy.Plan, error) {
	cfg := s.cfg
	bench, ok := data[cfg.Benchmark]
	if !ok || len(bench) < s.lookback {
		return nil, strategy.ErrWarmup
	}
	date := bench.Last().Date
	p := &strategy.Plan{Date: date, Weights: map[string]float64{}, Sleeve: map[string]string{}, Reasons: map[string]string{}, Exits: map[string]string{}}

	if s.due(date) {
		old := s.St.Targets
		if err := s.rebalance(data, date); err != nil {
			return nil, err
		}
		p.Rebalanced = true
		for sym := range old {
			if _, keep := s.St.Targets[sym]; !keep {
				p.Exits[sym] = "rebalance: no longer in the top slots or trend is down"
			}
		}
	}
	for sym, w := range s.St.Targets {
		p.Weights[sym] = w
		p.Sleeve[sym] = Sleeve
		p.Reasons[sym] = s.St.Reasons[sym]
	}
	p.RiskOn, p.ExAnteVol, p.VolScale = s.St.RiskOn, s.St.ExAnteVol, s.St.VolScale
	return p, nil
}

func (s *Strategy) rebalance(data market.Dataset, date time.Time) error {
	cfg, tc := s.cfg, s.cfg.Trend
	cashScore := 0.0
	if c, ok := data[cfg.Defensive]; ok && cfg.Defensive != "" {
		if sc := Score(c.Tail(s.lookback).Closes(), tc.Lookbacks); !math.IsNaN(sc) {
			cashScore = sc
		}
	}
	type cand struct {
		sym        string
		score, vol float64
	}
	var cands []cand
	for _, sym := range cfg.Universe {
		if sym == cfg.Defensive {
			continue
		}
		ser, ok := data[sym]
		if !ok || len(ser) < s.lookback || !ser.Last().Date.Equal(date) {
			continue
		}
		cl := ser.Tail(s.lookback).Closes()
		sc := Score(cl, tc.Lookbacks)
		vol := ind.AnnualizedVol(cl, tc.VolWindow)
		if math.IsNaN(sc) || math.IsNaN(vol) || vol <= 0 || sc <= cashScore {
			continue
		}
		cands = append(cands, cand{sym, sc, vol})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].sym < cands[j].sym
	})
	if len(cands) > tc.TopK {
		cands = cands[:tc.TopK]
	}

	targets := map[string]float64{}
	reasons := map[string]string{}
	inv := 0.0
	for _, c := range cands {
		inv += 1 / c.vol
	}
	filled := float64(len(cands)) / float64(tc.TopK)
	for rank, c := range cands {
		w := filled * (1 / c.vol) / inv
		targets[c.sym] = math.Min(w, tc.MaxWeight)
		reasons[c.sym] = fmt.Sprintf("rank %d: momentum %.1f%% vs cash %.1f%%, vol %.0f%%", rank+1, c.score*100, cashScore*100, c.vol*100)
	}
	s.St.ExAnteVol = ind.PortfolioVol(targets, data, tc.VolWindow)
	s.St.VolScale = 1
	if tc.TargetVol > 0 && s.St.ExAnteVol > tc.TargetVol {
		s.St.VolScale = tc.TargetVol / s.St.ExAnteVol
	}
	risky := 0.0
	for _, sym := range market.SortedKeys(targets) {
		targets[sym] *= s.St.VolScale
		risky += targets[sym]
	}
	if cfg.Defensive != "" && 1-risky > 1e-6 {
		if c, ok := data[cfg.Defensive]; ok && c.Last().Date.Equal(date) {
			targets[cfg.Defensive] = 1 - risky
			reasons[cfg.Defensive] = fmt.Sprintf("cash: %d of %d slots empty, vol scale %.2f", tc.TopK-len(cands), tc.TopK, s.St.VolScale)
		}
	}
	if risky > 1+1e-9 {
		return errors.New("trend: risky weights exceed 100% (bug)")
	}
	s.St.Targets, s.St.Reasons, s.St.RiskOn, s.St.LastRebalance = targets, reasons, len(cands) > 0, date
	return nil
}

// Static holds fixed weights (e.g. a 60/40 benchmark run through the same
// backtester, costs and risk engine).
type Static struct {
	Weights map[string]float64
	Warmup  int
	Bench   string
}

func (Static) Reconcile(map[string]float64) {}
func (Static) MarkRejected(string)          {}
func (s Static) Plan(data market.Dataset) (*strategy.Plan, error) {
	b, ok := data[s.Bench]
	if !ok || len(b) < s.Warmup {
		return nil, strategy.ErrWarmup
	}
	p := &strategy.Plan{Date: b.Last().Date, Weights: map[string]float64{}, Reasons: map[string]string{}}
	for sym, w := range s.Weights {
		if ser, ok := data[sym]; ok && ser.Last().Date.Equal(p.Date) {
			p.Weights[sym] = w
			p.Reasons[sym] = "static benchmark allocation"
		}
	}
	return p, nil
}
