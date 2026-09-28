// Package strategy implements the "Regime-filtered Momentum + Mean-Reversion"
// ensemble with volatility targeting.
//
// The strategy sees only bars up to the decision date (the caller truncates
// the dataset), and decides at a daily close. Orders are filled at the next
// session's open, so the backtest and live trading follow the same timeline.
//
//   - Regime filter: benchmark close vs its 200-day SMA (with a hysteresis band).
//   - Momentum sleeve: risk-adjusted multi-horizon momentum (12-1 style),
//     top-N, inverse-volatility weights, monthly rebalance, ATR trailing stop.
//     In a risk-off regime this sleeve moves to the defensive asset (or cash).
//   - Mean-reversion sleeve: RSI(2) pullbacks in long-term uptrends
//     (close > 200-day SMA), exiting on close > 5-day SMA, a time stop or an ATR stop.
//   - The combined weights are scaled down to an ex-ante volatility target.
package strategy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	ind "github.com/midhunvinay1/tradebot/internal/indicators"
	"github.com/midhunvinay1/tradebot/internal/market"
)

const (
	SleeveMomentum      = "momentum"
	SleeveMeanReversion = "mean_reversion"
)

// ErrWarmup means there is not yet enough history to decide.
var ErrWarmup = errors.New("strategy: not enough history (warm-up)")

// Holding is the strategy's record of one sleeve position.
type Holding struct {
	Sleeve     string    `json:"sleeve"`
	Weight     float64   `json:"weight"`      // sleeve weight before volatility scaling
	SignalDate time.Time `json:"signal_date"` // decision date that opened it
	EntryRef   float64   `json:"entry_ref"`   // close on the signal date
	ATRAtEntry float64   `json:"atr_at_entry"`
	HighWater  float64   `json:"high_water"`
	Pending    bool      `json:"pending"` // entry not yet confirmed by a fill
	Stopped    bool      `json:"stopped"` // momentum: stopped/vetoed until the next rebalance
	Reason     string    `json:"reason"`
}

// State is everything the strategy remembers between decisions. It is
// persisted as JSON in live trading.
type State struct {
	Momentum      map[string]*Holding `json:"momentum"`
	MeanRev       map[string]*Holding `json:"mean_reversion"`
	LastRebalance time.Time           `json:"last_rebalance"`
	RiskOn        *bool               `json:"risk_on,omitempty"`
}

func NewState() *State {
	return &State{Momentum: map[string]*Holding{}, MeanRev: map[string]*Holding{}}
}

// Plan is the output of one decision.
type Plan struct {
	Date       time.Time          `json:"date"`
	RiskOn     bool               `json:"risk_on"`
	Rebalanced bool               `json:"rebalanced"`
	ExAnteVol  float64            `json:"ex_ante_vol"`
	VolScale   float64            `json:"vol_scale"`
	Weights    map[string]float64 `json:"weights"` // final target weights (fraction of equity)
	Sleeve     map[string]string  `json:"sleeve"`  // symbol -> sleeve(s)
	Reasons    map[string]string  `json:"reasons"` // symbol -> why it is targeted
	Exits      map[string]string  `json:"exits"`   // symbol -> why the strategy exited it
}

// OrderReasons merges target reasons with exit reasons, for order annotations.
func (p *Plan) OrderReasons() map[string]string {
	out := map[string]string{}
	for s, r := range p.Exits {
		out[s] = r
	}
	for s, r := range p.Reasons {
		out[s] = r
	}
	return out
}

type Engine struct {
	cfg      *config.Config
	St       *State
	lookback int // bars of history any indicator needs
}

func New(cfg *config.Config, st *State) *Engine {
	if st == nil {
		st = NewState()
	}
	if st.Momentum == nil {
		st.Momentum = map[string]*Holding{}
	}
	if st.MeanRev == nil {
		st.MeanRev = map[string]*Holding{}
	}
	e := &Engine{cfg: cfg, St: st}
	e.lookback = e.warmupBars() + 80
	return e
}

// Reconcile confirms pending entries that were filled. A mean-reversion entry
// that did not fill is dropped, because its signal is stale by the next day.
// A momentum entry that did not fill is retried until the next rebalance.
func (e *Engine) Reconcile(held map[string]float64) {
	for sym, h := range e.St.MeanRev {
		if !h.Pending {
			continue
		}
		if held[sym] > 0 {
			h.Pending = false
		} else {
			delete(e.St.MeanRev, sym)
		}
	}
	for sym, h := range e.St.Momentum {
		if h.Pending && held[sym] > 0 {
			h.Pending = false
			h.HighWater = math.Max(h.HighWater, h.EntryRef)
		}
	}
}

// MarkRejected records that a planned entry was vetoed (by Claude or the risk
// engine) so it is not retried until a fresh signal.
func (e *Engine) MarkRejected(sym string) {
	if h, ok := e.St.MeanRev[sym]; ok && h.Pending {
		delete(e.St.MeanRev, sym)
	}
	if h, ok := e.St.Momentum[sym]; ok && h.Pending {
		h.Stopped = true
	}
}

func (e *Engine) warmupBars() int {
	s := e.cfg.Strategy
	n := s.RegimeSMA
	for _, lb := range s.Momentum.Lookbacks {
		n = max(n, lb+1)
	}
	n = max(n, s.MeanReversion.TrendSMA+1, s.Momentum.TrendSMA+1, s.VolWindow+1)
	return n
}

// eligible applies the price and liquidity filters.
func (e *Engine) eligible(s market.Series, minBars int) bool {
	c := e.cfg.Strategy
	if len(s) < minBars {
		return false
	}
	if s.Last().Close < c.MinPrice {
		return false
	}
	adv := ind.AvgDollarVolume(s, c.LiquidityWindow)
	return !math.IsNaN(adv) && adv >= c.MinDollarVolume
}

// barsSince counts bars dated strictly after t.
func barsSince(s market.Series, t time.Time) int {
	i := sort.Search(len(s), func(i int) bool { return s[i].Date.After(t) })
	return len(s) - i
}

// Plan makes the decision at the close of the last benchmark bar in data.
// data must already be truncated at the decision date.
func (e *Engine) Plan(data market.Dataset) (*Plan, error) {
	cfg := e.cfg
	sc := cfg.Strategy
	bench, ok := data[cfg.Benchmark]
	if !ok || len(bench) < e.warmupBars() {
		return nil, ErrWarmup
	}
	date := bench.Last().Date
	p := &Plan{
		Date: date, Weights: map[string]float64{}, Sleeve: map[string]string{},
		Reasons: map[string]string{}, Exits: map[string]string{},
	}

	// 1. Regime with hysteresis.
	bc := bench.Tail(e.lookback).Closes()
	sma := ind.SMA(bc, sc.RegimeSMA)
	px := bc[len(bc)-1]
	var riskOn bool
	switch {
	case e.St.RiskOn == nil:
		riskOn = px > sma
	case *e.St.RiskOn:
		riskOn = px >= sma*(1-sc.RegimeBand)
	default:
		riskOn = px > sma*(1+sc.RegimeBand)
	}
	regimeChanged := e.St.RiskOn != nil && *e.St.RiskOn != riskOn
	e.St.RiskOn = &riskOn
	p.RiskOn = riskOn

	// 2. Update high-water marks of confirmed momentum holdings.
	for sym, h := range e.St.Momentum {
		if s, ok := data[sym]; ok && !h.Pending {
			h.HighWater = math.Max(h.HighWater, s.Last().Close)
		}
	}

	e.planMomentum(data, bench, date, riskOn, regimeChanged, p)
	e.planMeanReversion(data, date, riskOn, p)

	// 3. Combine the sleeves, cap per-symbol weight, then volatility-target.
	raw := map[string]float64{}
	for sym, h := range e.St.Momentum {
		if !h.Stopped {
			raw[sym] += h.Weight
			p.Sleeve[sym] = SleeveMomentum
			p.Reasons[sym] = h.Reason
		}
	}
	for sym, h := range e.St.MeanRev {
		raw[sym] += h.Weight
		if p.Sleeve[sym] != "" {
			p.Sleeve[sym] += "+" + SleeveMeanReversion
			p.Reasons[sym] += "; " + h.Reason
		} else {
			p.Sleeve[sym] = SleeveMeanReversion
			p.Reasons[sym] = h.Reason
		}
	}
	for sym, w := range raw {
		if sym != cfg.Defensive { // the defensive asset is a cash equivalent
			raw[sym] = math.Min(w, sc.MaxSymbolWeight)
		}
	}
	p.ExAnteVol = ind.PortfolioVol(raw, data, sc.VolWindow)
	p.VolScale = 1
	if sc.TargetVol > 0 && p.ExAnteVol > sc.TargetVol {
		p.VolScale = sc.TargetVol / p.ExAnteVol
	}
	for sym, w := range raw {
		if w > 0 {
			p.Weights[sym] = w * p.VolScale
		}
	}
	return p, nil
}

func (e *Engine) planMomentum(data market.Dataset, bench market.Series, date time.Time, riskOn, regimeChanged bool, p *Plan) {
	m := e.cfg.Strategy.Momentum
	if m.Weight <= 0 {
		return
	}
	rebalance := e.St.LastRebalance.IsZero() || barsSince(bench, e.St.LastRebalance) >= m.RebalanceDays || regimeChanged
	if !rebalance {
		// Between rebalances: ATR trailing stops only.
		for sym, h := range e.St.Momentum {
			if h.Pending || h.Stopped || sym == e.cfg.Defensive {
				continue
			}
			s, ok := data[sym]
			if !ok {
				continue
			}
			atr := ind.ATR(s, m.ATRWindow)
			if c := s.Last().Close; !math.IsNaN(atr) && c < h.HighWater-m.TrailATRMult*atr {
				h.Stopped = true
				p.Exits[sym] = fmt.Sprintf("momentum trailing stop: close %.2f < high %.2f - %.1f*ATR %.2f", c, h.HighWater, m.TrailATRMult, atr)
			}
		}
		return
	}

	p.Rebalanced = true
	next := map[string]*Holding{}
	if riskOn {
		type cand struct {
			sym          string
			score, vol   float64
			riskAdjusted float64
		}
		maxLB := 0
		for _, lb := range m.Lookbacks {
			maxLB = max(maxLB, lb)
		}
		var cands []cand
		for _, sym := range e.cfg.Universe {
			if sym == e.cfg.Defensive {
				continue
			}
			s, ok := data[sym]
			if !ok || !s.Last().Date.Equal(date) || !e.eligible(s, max(maxLB+1, m.TrendSMA, m.VolWindow+1)) {
				continue
			}
			cl := s.Tail(e.lookback).Closes()
			score := 0.0
			for _, lb := range m.Lookbacks {
				score += ind.Return(cl, lb, m.Skip)
			}
			score /= float64(len(m.Lookbacks))
			absMom := ind.Return(cl, maxLB, m.Skip)
			vol := ind.AnnualizedVol(cl, m.VolWindow)
			if math.IsNaN(score) || math.IsNaN(vol) || vol <= 0 || absMom <= 0 || cl[len(cl)-1] <= ind.SMA(cl, m.TrendSMA) {
				continue
			}
			cands = append(cands, cand{sym, score, vol, score / vol})
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].riskAdjusted != cands[j].riskAdjusted {
				return cands[i].riskAdjusted > cands[j].riskAdjusted
			}
			return cands[i].sym < cands[j].sym
		})
		if len(cands) > m.TopN {
			cands = cands[:m.TopN]
		}
		invSum := 0.0
		for _, c := range cands {
			invSum += 1 / c.vol
		}
		for rank, c := range cands {
			s := data[c.sym]
			h := &Holding{
				Sleeve: SleeveMomentum, Weight: m.Weight * (1 / c.vol) / invSum, SignalDate: date,
				EntryRef: s.Last().Close, ATRAtEntry: ind.ATR(s, m.ATRWindow), HighWater: s.Last().Close, Pending: true,
				Reason: fmt.Sprintf("momentum rank %d/%d: blended return %.1f%%, vol %.0f%%", rank+1, len(cands), c.score*100, c.vol*100),
			}
			if old, ok := e.St.Momentum[c.sym]; ok && !old.Pending && !old.Stopped {
				h.SignalDate, h.EntryRef, h.HighWater, h.Pending = old.SignalDate, old.EntryRef, old.HighWater, false
			}
			next[c.sym] = h
		}
	} else if d := e.cfg.Defensive; d != "" {
		if s, ok := data[d]; ok && s.Last().Date.Equal(date) {
			h := &Holding{Sleeve: SleeveMomentum, Weight: m.Weight, SignalDate: date, EntryRef: s.Last().Close,
				HighWater: s.Last().Close, Pending: true, Reason: "risk-off regime: defensive asset"}
			if old, ok := e.St.Momentum[d]; ok && !old.Pending {
				h.Pending, h.SignalDate = false, old.SignalDate
			}
			next[d] = h
		}
	}
	for sym, h := range e.St.Momentum {
		if _, keep := next[sym]; !keep && !h.Stopped {
			p.Exits[sym] = "momentum rebalance: no longer selected"
		}
	}
	e.St.Momentum = next
	e.St.LastRebalance = date
}

func (e *Engine) planMeanReversion(data market.Dataset, date time.Time, riskOn bool, p *Plan) {
	r := e.cfg.Strategy.MeanReversion
	if r.Weight <= 0 || r.MaxPositions <= 0 {
		return
	}
	exited := map[string]bool{}
	for sym, h := range e.St.MeanRev {
		if h.Pending {
			continue
		}
		s, ok := data[sym]
		if !ok {
			delete(e.St.MeanRev, sym)
			exited[sym] = true
			p.Exits[sym] = "mean-reversion: no data"
			continue
		}
		cl := s.Tail(e.lookback).Closes()
		c := cl[len(cl)-1]
		held := barsSince(s, h.SignalDate)
		var why string
		switch {
		case c > ind.SMA(cl, r.ExitSMA):
			why = fmt.Sprintf("mean-reversion target: close %.2f > SMA%d", c, r.ExitSMA)
		case held >= r.MaxHoldDays:
			why = fmt.Sprintf("mean-reversion time stop: held %d bars", held)
		case h.ATRAtEntry > 0 && c < h.EntryRef-r.StopATRMult*h.ATRAtEntry:
			why = fmt.Sprintf("mean-reversion ATR stop: close %.2f < %.2f", c, h.EntryRef-r.StopATRMult*h.ATRAtEntry)
		}
		if why != "" {
			delete(e.St.MeanRev, sym)
			exited[sym] = true
			p.Exits[sym] = why
		}
	}
	if !riskOn && r.RequireRiskOn {
		return
	}
	slots := r.MaxPositions - len(e.St.MeanRev)
	if slots <= 0 {
		return
	}
	type cand struct {
		sym string
		rsi float64
	}
	var cands []cand
	for _, sym := range e.cfg.Universe {
		if sym == e.cfg.Defensive || exited[sym] {
			continue
		}
		if _, held := e.St.MeanRev[sym]; held {
			continue
		}
		s, ok := data[sym]
		if !ok || !s.Last().Date.Equal(date) || !e.eligible(s, max(r.TrendSMA+1, r.ATRWindow+1)) {
			continue
		}
		cl := s.Tail(e.lookback).Closes()
		if cl[len(cl)-1] <= ind.SMA(cl, r.TrendSMA) {
			continue
		}
		if rsi := ind.RSI(cl, r.RSIPeriod); rsi < r.EntryRSI {
			cands = append(cands, cand{sym, rsi})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rsi != cands[j].rsi {
			return cands[i].rsi < cands[j].rsi
		}
		return cands[i].sym < cands[j].sym
	})
	for i := 0; i < len(cands) && i < slots; i++ {
		s := data[cands[i].sym]
		e.St.MeanRev[cands[i].sym] = &Holding{
			Sleeve: SleeveMeanReversion, Weight: r.Weight / float64(r.MaxPositions), SignalDate: date,
			EntryRef: s.Last().Close, ATRAtEntry: ind.ATR(s, r.ATRWindow), HighWater: s.Last().Close, Pending: true,
			Reason: fmt.Sprintf("mean-reversion: RSI(%d)=%.1f in uptrend (close > SMA%d)", r.RSIPeriod, cands[i].rsi, r.TrendSMA),
		}
	}
}
