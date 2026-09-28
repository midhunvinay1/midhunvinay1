package trend

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/backtest"
	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/runner"
)

var start = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// series makes n weekday bars growing at `daily` with a +/-`wiggle` zigzag.
func series(n int, daily, wiggle float64) market.Series {
	var s market.Series
	p := 100.0
	for d := start; len(s) < n; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		p *= 1 + daily
		c := p * (1 + wiggle*float64(len(s)%2*2-1))
		s = append(s, market.Bar{Date: d, Open: c, High: c * 1.001, Low: c * 0.999, Close: c, Volume: 1e6})
	}
	return s
}

func testCfg() *config.Config {
	cfg := config.Defaults()
	cfg.Universe = []string{"UP1", "UP2", "UP3", "FLAT", "DOWN1", "DOWN2"}
	cfg.Benchmark = "UP1"
	cfg.Defensive = "CASH"
	return cfg
}

func dataset(n int) market.Dataset {
	return market.Dataset{
		"UP1": series(n, 0.0010, 0.002), "UP2": series(n, 0.0008, 0.004), "UP3": series(n, 0.0006, 0.001),
		"FLAT": series(n, 0.00005, 0.001), "DOWN1": series(n, -0.0008, 0.002), "DOWN2": series(n, -0.0004, 0.003),
		"CASH": series(n, 0.0001, 0),
	}
}

func TestSelectsUptrendsWeightsAndCash(t *testing.T) {
	cfg := testCfg()
	cfg.Trend.TargetVol = 0 // isolate selection and weights
	s := New(cfg, nil)
	p, err := s.Plan(dataset(300))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"FLAT", "DOWN1", "DOWN2"} {
		if _, ok := p.Weights[bad]; ok {
			t.Fatalf("%s trails cash and must not be held: %v", bad, p.Weights)
		}
	}
	sum := 0.0
	for sym, w := range p.Weights {
		sum += w
		if sym != "CASH" && w > cfg.Trend.MaxWeight+1e-9 {
			t.Fatalf("%s weight %.3f exceeds max_weight", sym, w)
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("weights must sum to 1 (cash included), got %.6f: %v", sum, p.Weights)
	}
	// 3 of 4 slots filled -> 75% risky before caps; lower vol gets more weight.
	if !(p.Weights["UP3"] > p.Weights["UP1"] && p.Weights["UP1"] > p.Weights["UP2"]) {
		t.Fatalf("inverse-vol ordering wrong: %v", p.Weights)
	}
	if p.Weights["CASH"] < 0.25-1e-9 {
		t.Fatalf("an empty slot must stay in cash: %v", p.Weights)
	}
}

func TestAllDownGoesToCash(t *testing.T) {
	cfg := testCfg()
	ds := dataset(300)
	for _, sym := range []string{"UP1", "UP2", "UP3", "FLAT"} {
		ds[sym] = series(300, -0.0005, 0.002)
	}
	p, err := New(cfg, nil).Plan(ds)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Weights) != 1 || math.Abs(p.Weights["CASH"]-1) > 1e-9 || p.RiskOn {
		t.Fatalf("with every trend down the bot must hold only T-bills: %v", p.Weights)
	}
}

func TestVolatilityTargetScalesDown(t *testing.T) {
	cfg := testCfg()
	cfg.Trend.TargetVol = 0.02
	p, err := New(cfg, nil).Plan(dataset(300))
	if err != nil {
		t.Fatal(err)
	}
	if !(p.VolScale < 1) || math.Abs(p.ExAnteVol*p.VolScale-0.02) > 0.002 {
		t.Fatalf("vol scale %.3f, ex-ante vol %.3f", p.VolScale, p.ExAnteVol)
	}
}

func TestRebalancesMonthlyOnly(t *testing.T) {
	cfg := testCfg()
	s := New(cfg, nil)
	ds := dataset(320)
	bench := ds["UP1"]
	rebalances := 0
	var lastMonth time.Month
	for i := 260; i < len(bench); i++ {
		p, err := s.Plan(ds.Until(bench[i].Date))
		if err != nil {
			t.Fatal(err)
		}
		if p.Rebalanced {
			rebalances++
			if bench[i].Date.Month() == lastMonth {
				t.Fatalf("rebalanced twice in %s", lastMonth)
			}
			lastMonth = bench[i].Date.Month()
		}
	}
	if rebalances < 2 || rebalances > 4 {
		t.Fatalf("expected one rebalance per month (~3), got %d", rebalances)
	}
}

func TestBacktestNoLookAheadAndDeterministic(t *testing.T) {
	cfg := config.Defaults()
	cfg.Universe = []string{"SPY", "QQQ", "IWM", "EFA", "TLT", "IEF", "GLD", "DBC"}
	cfg.Benchmark, cfg.Defensive = "SPY", "BIL"
	ds := data.Synthetic(cfg.Symbols(), cfg.Benchmark, time.Date(2010, 1, 4, 0, 0, 0, 0, time.UTC), 8, 3)
	a, err := backtest.RunWith(context.Background(), cfg, ds, New(cfg, nil), backtest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := backtest.RunWith(context.Background(), cfg, ds, New(cfg, nil), backtest.Options{})
	if len(a.Fills) == 0 || !reflect.DeepEqual(a.Fills, b.Fills) {
		t.Fatal("expected deterministic trades")
	}
	cut := ds["SPY"][len(ds["SPY"])-400].Date
	p := market.Dataset{}
	for sym, s := range ds {
		cp := append(market.Series{}, s...)
		for i := range cp {
			if cp[i].Date.After(cut) {
				f := 0.5 + float64(i%9)/6
				cp[i].Open, cp[i].High, cp[i].Low, cp[i].Close = cp[i].Open*f, cp[i].High*f*1.05, cp[i].Low*f*0.95, cp[i].Close*f
			}
		}
		p[sym] = cp
	}
	c, err := backtest.RunWith(context.Background(), cfg, p, New(cfg, nil), backtest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := func(fs []backtest.Fill) []backtest.Fill {
		var out []backtest.Fill
		for _, f := range fs {
			if !f.Date.After(cut) {
				out = append(out, f)
			}
		}
		return out
	}
	if !reflect.DeepEqual(before(a.Fills), before(c.Fills)) {
		t.Fatal("fills before the cut changed: look-ahead leak")
	}
	for _, pt := range a.Equity {
		if pt.Cash < -1e-6 || (pt.Equity > 0 && pt.Gross/pt.Equity > 1.02) {
			t.Fatalf("leverage or negative cash on %s", pt.Date.Format("2006-01-02"))
		}
	}
}

type memSource struct{ ds market.Dataset }

func (m memSource) Bars(context.Context, []string, time.Time, time.Time) (market.Dataset, error) {
	return m.ds, nil
}
func (memSource) LatestPrices(context.Context, []string) (map[string]float64, error) { return nil, nil }
func (memSource) News(context.Context, string, time.Time, int) ([]market.News, error) {
	return nil, nil
}
func (memSource) IsTradingDay(context.Context, time.Time) (bool, error) { return true, nil }

func factory(cfg *config.Config) runner.PlannerFactory {
	return func(raw json.RawMessage) (runner.Planner, error) {
		st := &State{}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, st); err != nil {
				return nil, err
			}
		}
		return New(cfg, st), nil
	}
}

func TestLiveRunnerCycleWithoutLLM(t *testing.T) {
	cfg := testCfg()
	ds := dataset(300)
	last := ds["UP1"].Last().Date
	now := time.Date(last.Year(), last.Month(), last.Day()+1, 14, 40, 0, 0, time.UTC)
	for now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		now = now.AddDate(0, 0, 1)
	}
	dir := t.TempDir()
	var out bytes.Buffer
	r := &runner.Runner{Cfg: cfg, StateDir: dir, Broker: broker.DryRun{Equity: 100000}, Data: memSource{ds},
		NewPlanner: factory(cfg), Now: func() time.Time { return now }, Out: &out}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "APPROVED buy") || strings.Contains(log, "Review:") {
		t.Fatalf("expected rule-only orders and no reviewer:\n%s", log)
	}
	st, err := runner.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ts State
	if err := json.Unmarshal(st.Strategy, &ts); err != nil || ts.LastRebalance.IsZero() || len(ts.Targets) == 0 {
		t.Fatalf("trend state not persisted: %+v %v", ts, err)
	}
}
