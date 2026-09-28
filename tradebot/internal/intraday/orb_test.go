package intraday

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
)

var day = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func at(h, m int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, market.NewYork)
}

// engineWithOR returns an engine whose opening range for AAA is
// open 100, high 101.5, close 101, volume 3000 (RVOL 3).
func engineWithOR(t *testing.T, mutate func(*config.IntradayConfig), bullish bool) *Engine {
	t.Helper()
	cfg := config.Defaults().Intraday
	if mutate != nil {
		mutate(&cfg)
	}
	e := NewEngine(cfg, []SymbolContext{{Symbol: "AAA", ATR: 2, AvgORVolume: 1000, PrevClose: 99}}, 10000, 10000, 0)
	closeLast := 101.0
	if !bullish {
		closeLast = 99.5
	}
	for i := 0; i < 5; i++ {
		b := market.Bar{Date: at(9, 30+i), Open: 100, High: 100.5, Low: 99.8, Close: 100.2, Volume: 600}
		if i == 2 {
			b.High = 101.5
		}
		if i == 4 {
			b.Close = closeLast
		}
		e.AddORBar("AAA", b)
	}
	return e
}

func TestOpeningRangeSelection(t *testing.T) {
	e := engineWithOR(t, nil, true)
	if got := e.FinalizeOpeningRange(); !reflect.DeepEqual(got, []string{"AAA"}) {
		t.Fatalf("in play = %v", got)
	}
	if math.Abs(e.RVOL("AAA")-3) > 1e-9 {
		t.Fatalf("RVOL = %v, want 3", e.RVOL("AAA"))
	}
	if got := engineWithOR(t, nil, false).FinalizeOpeningRange(); len(got) != 0 {
		t.Fatalf("bearish opening candle must not be in play (long-only): %v", got)
	}
	if got := engineWithOR(t, func(c *config.IntradayConfig) { c.MinRVOL = 4 }, true).FinalizeOpeningRange(); len(got) != 0 {
		t.Fatalf("RVOL filter ignored: %v", got)
	}
}

func TestBreakoutEntrySizingAndStop(t *testing.T) {
	e := engineWithOR(t, nil, true)
	e.FinalizeOpeningRange()
	if acts := e.OnPrice("AAA", 101.5, 101.0, at(9, 40)); len(acts) != 0 {
		t.Fatalf("no entry before the price exceeds the OR high: %v", acts)
	}
	acts := e.OnPrice("AAA", 101.6, 101.2, at(9, 41))
	if len(acts) != 1 || acts[0].Kind != ActEntry {
		t.Fatalf("expected one entry, got %v", acts)
	}
	a := acts[0]
	// risk qty = 1% * 10000 / (0.10*2) = 500; weight cap = 25% * 10000 / 101.51 = 24.6 -> 24
	if a.Qty != 24 || a.Price != 101.51 || a.Stop != 101.31 {
		t.Fatalf("entry = %+v", a)
	}
	e.OnEntryFill("AAA", 24, 101.55)
	if _, _, stop := e.Position("AAA"); stop != 101.35 {
		t.Fatalf("stop must re-anchor to the fill: %v", stop)
	}
	acts = e.OnPrice("AAA", 101.6, 101.30, at(9, 45))
	if len(acts) != 1 || acts[0].Reason != ReasonStop {
		t.Fatalf("expected stop exit, got %v", acts)
	}
	e.OnExitFill("AAA", 24, 101.35, at(9, 45), nil)
	if math.Abs(e.Realized()-(-4.8)) > 1e-9 {
		t.Fatalf("realized = %v", e.Realized())
	}
	if acts := e.OnPrice("AAA", 103, 102, at(10, 0)); len(acts) != 0 {
		t.Fatal("only one entry per symbol per day")
	}
}

func TestCutoffFlattenAndLossLimit(t *testing.T) {
	e := engineWithOR(t, nil, true)
	e.FinalizeOpeningRange()
	if acts := e.OnPrice("AAA", 102, 101.6, at(15, 1)); len(acts) != 0 {
		t.Fatal("no entries after no_entries_after")
	}

	e = engineWithOR(t, nil, true)
	e.FinalizeOpeningRange()
	e.OnPrice("AAA", 101.6, 101.5, at(10, 0))
	e.OnEntryFill("AAA", 24, 101.52)
	acts := e.OnPrice("AAA", 102, 101.9, at(15, 55))
	if len(acts) != 1 || acts[0].Reason != ReasonFlatten {
		t.Fatalf("expected flatten, got %v", acts)
	}

	e = engineWithOR(t, func(c *config.IntradayConfig) { c.DailyLossLimit = 0.0001 }, true)
	e.FinalizeOpeningRange()
	e.OnPrice("AAA", 101.6, 101.5, at(10, 0))
	e.OnEntryFill("AAA", 24, 101.52)
	acts = e.CheckLossLimit(at(10, 1), map[string]float64{"AAA": 101.40})
	if len(acts) != 1 || acts[0].Reason != ReasonHalt {
		t.Fatalf("loss limit should flatten: %v", acts)
	}
	if h, why := e.Halted(); !h || why != ReasonLossLimit {
		t.Fatal("loss limit should halt new entries")
	}
}

func TestDayTradeLimitAndCashAccount(t *testing.T) {
	cfg := config.Defaults().Intraday
	cfg.DayTradeLimit5D = 3
	e := NewEngine(cfg, []SymbolContext{{Symbol: "AAA", ATR: 2, AvgORVolume: 1000}}, 10000, 10000, 3)
	for i := 0; i < 5; i++ {
		e.AddORBar("AAA", market.Bar{Date: at(9, 30+i), Open: 100, High: 101, Low: 99.9, Close: 100.5 + float64(i)*0.1, Volume: 600})
	}
	e.FinalizeOpeningRange()
	if acts := e.OnPrice("AAA", 102, 101, at(10, 0)); len(acts) != 0 {
		t.Fatal("day-trade limit must block entries")
	}

	cfg = config.Defaults().Intraday
	e = NewEngine(cfg, []SymbolContext{{Symbol: "AAA", ATR: 2, AvgORVolume: 1000}}, 10000, 500, 0)
	for i := 0; i < 5; i++ {
		e.AddORBar("AAA", market.Bar{Date: at(9, 30+i), Open: 100, High: 101, Low: 99.9, Close: 100.5 + float64(i)*0.1, Volume: 600})
	}
	e.FinalizeOpeningRange()
	acts := e.OnPrice("AAA", 102, 101, at(10, 0))
	if len(acts) != 1 || acts[0].Qty*acts[0].Price > 500 {
		t.Fatalf("entries must fit in settled cash: %v", acts)
	}
}

func TestBacktestDeterministicAndNoLookAhead(t *testing.T) {
	cfg := config.Defaults()
	syms := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF", "SPY"}
	cfg.Intraday.Universe = syms
	ds := SyntheticMinutes(syms, time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC), 80, 3)
	opt := BacktestOptions{InitialEquity: 10000, Benchmark: "SPY"}
	a, err := Backtest(context.Background(), cfg, ds, opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Backtest(context.Background(), cfg, ds, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Trades) == 0 {
		t.Fatal("expected trades on synthetic data")
	}
	if !reflect.DeepEqual(a.Trades, b.Trades) {
		t.Fatal("intraday backtest must be deterministic")
	}
	for _, tr := range a.Trades {
		if !market.TradingDate(tr.EntryTime).Equal(market.TradingDate(tr.ExitTime)) {
			t.Fatalf("position held overnight: %+v", tr)
		}
		if MinuteOfDay(tr.EntryTime) < 9*60+35 {
			t.Fatalf("entry inside the opening range: %+v", tr)
		}
	}

	// Perturb everything after a cut time; earlier trades must be identical.
	cut := a.Equity[len(a.Equity)/2].Date.Add(24 * time.Hour)
	p := market.Dataset{}
	for s, ser := range ds {
		cp := make(market.Series, len(ser))
		copy(cp, ser)
		for i := range cp {
			if cp[i].Date.After(cut) {
				f := 0.7 + float64(i%5)/10
				cp[i].Open, cp[i].High, cp[i].Low, cp[i].Close = cp[i].Open*f, cp[i].High*f*1.02, cp[i].Low*f*0.98, cp[i].Close*f
				cp[i].Volume *= 3
			}
		}
		p[s] = cp
	}
	c, err := Backtest(context.Background(), cfg, p, opt)
	if err != nil {
		t.Fatal(err)
	}
	before := func(ts []Trade) []Trade {
		var out []Trade
		for _, x := range ts {
			if x.ExitTime.Before(cut) {
				out = append(out, x)
			}
		}
		return out
	}
	if !reflect.DeepEqual(before(a.Trades), before(c.Trades)) {
		t.Fatal("trades before the cut changed: look-ahead leak")
	}
}
