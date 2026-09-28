package backtest

import (
	"context"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

func synth(cfg *config.Config) market.Dataset {
	return data.Synthetic(cfg.Symbols(), cfg.Benchmark, time.Date(2012, 1, 2, 0, 0, 0, 0, time.UTC), 6, 42)
}

func TestFillPrice(t *testing.T) {
	bar := market.Bar{Open: 100, High: 103, Low: 97, Close: 101}
	b := portfolio.Order{Side: portfolio.Buy, LimitPrice: 100.3}
	if p, ok := FillPrice(b, bar, 0.0005); !ok || math.Abs(p-100.05) > 1e-9 {
		t.Fatalf("buy at open+slip: %v %v", p, ok)
	}
	b.LimitPrice = 98 // open above limit, low trades through
	if p, ok := FillPrice(b, bar, 0.0005); !ok || p != 98 {
		t.Fatalf("buy at limit: %v %v", p, ok)
	}
	b.LimitPrice = 96 // never reached
	if _, ok := FillPrice(b, bar, 0.0005); ok {
		t.Fatal("buy should not fill")
	}
	s := portfolio.Order{Side: portfolio.Sell, LimitPrice: 99}
	if p, ok := FillPrice(s, bar, 0.0005); !ok || math.Abs(p-99.95) > 1e-9 {
		t.Fatalf("sell at open-slip: %v %v", p, ok)
	}
}

func TestBacktestRunsAndIsDeterministic(t *testing.T) {
	cfg := config.Defaults()
	ds := synth(cfg)
	a, err := Run(context.Background(), cfg, ds, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), cfg, ds, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Fills) == 0 || a.Metrics.Trades == 0 {
		t.Fatal("expected trades on synthetic data")
	}
	if !reflect.DeepEqual(a.Metrics, b.Metrics) || len(a.Fills) != len(b.Fills) {
		t.Fatal("backtest must be deterministic")
	}
	for _, p := range a.Equity {
		if p.Cash < -1e-6 {
			t.Fatalf("cash went negative on %s: %.2f", p.Date.Format("2006-01-02"), p.Cash)
		}
		if p.Equity > 0 && p.Gross/p.Equity > cfg.Risk.MaxGrossExposure+0.05 {
			t.Fatalf("gross exposure %.2f exceeded the limit on %s", p.Gross/p.Equity, p.Date.Format("2006-01-02"))
		}
	}
}

// Changing prices AFTER a date must not change any decision made before it.
func TestNoLookAhead(t *testing.T) {
	cfg := config.Defaults()
	ds := synth(cfg)
	cut := ds[cfg.Benchmark][len(ds[cfg.Benchmark])-300].Date

	perturbed := market.Dataset{}
	for sym, s := range ds {
		cp := make(market.Series, len(s))
		copy(cp, s)
		for i := range cp {
			if cp[i].Date.After(cut) {
				f := 0.5 + float64(i%7)/5 // wild, arbitrary future prices
				cp[i].Open *= f
				cp[i].High *= f * 1.1
				cp[i].Low *= f * 0.9
				cp[i].Close *= f
			}
		}
		perturbed[sym] = cp
	}
	a, err := Run(context.Background(), cfg, ds, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), cfg, perturbed, Options{})
	if err != nil {
		t.Fatal(err)
	}
	before := func(fs []Fill) []Fill {
		var out []Fill
		for _, f := range fs {
			if !f.Date.After(cut) {
				out = append(out, f)
			}
		}
		return out
	}
	fa, fb := before(a.Fills), before(b.Fills)
	if len(fa) == 0 || !reflect.DeepEqual(fa, fb) {
		t.Fatalf("fills before %s differ: %d vs %d (look-ahead leak)", cut.Format("2006-01-02"), len(fa), len(fb))
	}
}

func TestMetrics(t *testing.T) {
	d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := []EquityPoint{{Date: d, Equity: 100}, {Date: d.AddDate(0, 0, 1), Equity: 120}, {Date: d.AddDate(0, 0, 2), Equity: 90}, {Date: d.AddDate(1, 0, 0), Equity: 110}}
	m := ComputeMetrics(pts, func(p EquityPoint) float64 { return p.Equity }, 0)
	if math.Abs(m.MaxDrawdown-0.25) > 1e-9 {
		t.Fatalf("max drawdown = %v, want 0.25", m.MaxDrawdown)
	}
	if math.Abs(m.TotalReturn-0.1) > 1e-9 {
		t.Fatalf("total return = %v, want 0.1", m.TotalReturn)
	}
}
