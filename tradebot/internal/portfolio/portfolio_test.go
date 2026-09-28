package portfolio

import (
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
)

func TestDiff(t *testing.T) {
	cfg := config.Defaults().Execution
	in := Input{
		Date:    time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC),
		Targets: map[string]float64{"AAPL": 0.20, "MSFT": 0.101, "NVDA": 0.10},
		Held:    map[string]float64{"MSFT": 10, "XOM": 5, "OTHER": 7},
		Prices:  map[string]float64{"AAPL": 100, "MSFT": 100, "NVDA": 1000, "XOM": 50, "OTHER": 10},
		Equity:  10000,
		Managed: map[string]bool{"AAPL": true, "MSFT": true, "NVDA": true, "XOM": true},
	}
	orders := Diff(in, cfg)
	got := map[string]Order{}
	for _, o := range orders {
		got[o.Symbol] = o
	}
	if o := got["AAPL"]; o.Side != Buy || o.Qty != 20 || o.Kind != KindEntry || o.LimitPrice != 100.3 || o.ID != "tb-20240305-AAPL-buy" {
		t.Fatalf("AAPL: %+v", o)
	}
	if _, ok := got["MSFT"]; ok {
		t.Fatal("MSFT change is inside the rebalance band and should be skipped")
	}
	if o := got["NVDA"]; o.Qty != 1 {
		t.Fatalf("NVDA should floor to 1 whole share: %+v", o)
	}
	if o := got["XOM"]; o.Side != Sell || o.Qty != 5 || o.Kind != KindExit || o.LimitPrice != 49.5 {
		t.Fatalf("XOM exit: %+v", o)
	}
	if _, ok := got["OTHER"]; ok {
		t.Fatal("unmanaged positions must never be traded")
	}
	if orders[0].Side != Sell {
		t.Fatal("sells must come first")
	}
}
