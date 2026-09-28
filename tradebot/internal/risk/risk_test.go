package risk

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

var day = time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)

func snap() Snapshot {
	return Snapshot{
		Now: day.Add(14 * time.Hour), LastBarDate: day.AddDate(0, 0, -1), Equity: 10000, Cash: 10000,
		Held: map[string]float64{}, Prices: map[string]float64{"AAPL": 100, "MSFT": 100, "SGOV": 100},
	}
}

func buy(sym string, qty, px float64) portfolio.Order {
	return portfolio.Order{ID: sym, Symbol: sym, Side: portfolio.Buy, Qty: qty, RefPrice: px, LimitPrice: px * 1.003, Kind: portfolio.KindEntry}
}

func sell(sym string, qty, px float64) portfolio.Order {
	return portfolio.Order{ID: sym + "s", Symbol: sym, Side: portfolio.Sell, Qty: qty, RefPrice: px, LimitPrice: px * 0.99, Kind: portfolio.KindExit}
}

func onlyReason(t *testing.T, rej []Rejection, want string) {
	t.Helper()
	if len(rej) != 1 || !strings.Contains(rej[0].Reason, want) {
		t.Fatalf("want one rejection containing %q, got %+v", want, rej)
	}
}

func TestApprovesNormalOrder(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	ok, rej := e.Check([]portfolio.Order{buy("AAPL", 20, 100)}, snap())
	if len(ok) != 1 || len(rej) != 0 {
		t.Fatalf("expected approval, got ok=%v rej=%v", ok, rej)
	}
}

func TestRejectsOversizedPosition(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	_, rej := e.Check([]portfolio.Order{buy("AAPL", 40, 100)}, snap()) // 40% > 30%
	onlyReason(t, rej, "position would be")
}

func TestDefensiveAssetExemptFromPositionCap(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	ok, rej := e.Check([]portfolio.Order{buy("SGOV", 60, 100)}, snap())
	if len(ok) != 1 {
		t.Fatalf("defensive 60%% position should pass, got %v", rej)
	}
}

func TestRejectsUnknownAndDeniedSymbols(t *testing.T) {
	cfg := config.Defaults()
	cfg.Risk.DenySymbols = []string{"msft"}
	e := New(cfg, nil, "")
	s := snap()
	s.Prices["GME"] = 20
	_, rej := e.Check([]portfolio.Order{buy("GME", 1, 20), buy("MSFT", 1, 100)}, s)
	if len(rej) != 2 || !strings.Contains(rej[0].Reason, "not in the configured universe") || !strings.Contains(rej[1].Reason, "deny list") {
		t.Fatalf("unexpected: %+v", rej)
	}
}

func TestNoShorting(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	s := snap()
	s.Held["AAPL"] = 5
	_, rej := e.Check([]portfolio.Order{sell("AAPL", 6, 100)}, s)
	onlyReason(t, rej, "no shorting")
}

func TestHaltBlocksBuysAllowsExits(t *testing.T) {
	dir := t.TempDir()
	e := New(config.Defaults(), nil, HaltFilePath(dir))
	e.Halt("test")
	if h, _ := New(config.Defaults(), nil, filepath.Join(dir, "HALT")).Halted(); !h {
		t.Fatal("halt file should be visible to a fresh engine")
	}
	s := snap()
	s.Held["MSFT"] = 10
	ok, rej := e.Check([]portfolio.Order{sell("MSFT", 10, 100), buy("AAPL", 10, 100)}, s)
	if len(ok) != 1 || ok[0].Side != portfolio.Sell {
		t.Fatalf("exit should pass while halted: ok=%v", ok)
	}
	onlyReason(t, rej, "kill switch")
	if e.ExposureScale(10000) != 0 {
		t.Fatal("halted exposure scale must be 0")
	}
}

func TestStaleDataRejectsEverything(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	s := snap()
	s.LastBarDate = day.AddDate(0, 0, -10)
	_, rej := e.Check([]portfolio.Order{buy("AAPL", 1, 100)}, s)
	onlyReason(t, rej, "stale")
}

func TestLimitDeviationAndCash(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	o := buy("AAPL", 1, 100)
	o.LimitPrice = 110
	_, rej := e.Check([]portfolio.Order{o}, snap())
	onlyReason(t, rej, "deviates")

	s := snap()
	s.Cash = 500
	_, rej = e.Check([]portfolio.Order{buy("AAPL", 20, 100)}, s)
	onlyReason(t, rej, "insufficient cash")
}

func TestSellProceedsFundBuys(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	s := snap()
	s.Cash = 0
	s.Held["MSFT"] = 25
	ok, rej := e.Check([]portfolio.Order{sell("MSFT", 25, 100), buy("AAPL", 20, 100)}, s)
	if len(ok) != 2 {
		t.Fatalf("sell proceeds should fund the buy: rej=%v", rej)
	}
}

func TestBreakers(t *testing.T) {
	e := New(config.Defaults(), nil, "")
	e.Update(10000, day)
	e.Update(8400, day.AddDate(0, 0, 1)) // 16% drawdown -> soft
	if got := e.ExposureScale(8400); got != 0.5 {
		t.Fatalf("soft drawdown scale = %v, want 0.5", got)
	}
	e.Update(7400, day.AddDate(0, 0, 2)) // 26% -> hard halt
	if h, _ := e.Halted(); !h {
		t.Fatal("hard drawdown should halt")
	}

	e2 := New(config.Defaults(), nil, "")
	e2.Update(10000, day)
	s := snap()
	s.Equity = 9500 // -5% intraday vs day start
	e2.Update(9500, day)
	_, rej := e2.Check([]portfolio.Order{buy("AAPL", 1, 100)}, s)
	onlyReason(t, rej, "daily loss")
}
