package indicators

import (
	"math"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/market"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSMA(t *testing.T) {
	if got := SMA([]float64{1, 2, 3, 4, 5}, 2); !near(got, 4.5) {
		t.Fatalf("SMA = %v, want 4.5", got)
	}
	if !math.IsNaN(SMA([]float64{1}, 2)) {
		t.Fatal("SMA with insufficient data should be NaN")
	}
}

func TestRSI(t *testing.T) {
	up := []float64{1, 2, 3, 4, 5, 6}
	if got := RSI(up, 2); got != 100 {
		t.Fatalf("RSI of rising series = %v, want 100", got)
	}
	down := []float64{6, 5, 4, 3, 2, 1}
	if got := RSI(down, 2); got != 0 {
		t.Fatalf("RSI of falling series = %v, want 0", got)
	}
	// Wilder with n=2: seed gains/losses from first 2 changes, then smooth.
	// closes 10, 11, 10, 12: changes +1, -1, +2
	// seed: gain 0.5, loss 0.5; then gain=(0.5+2)/2=1.25, loss=(0.5+0)/2=0.25 -> RS 5 -> RSI 83.33
	if got := RSI([]float64{10, 11, 10, 12}, 2); math.Abs(got-83.3333333) > 1e-4 {
		t.Fatalf("RSI = %v, want 83.33", got)
	}
}

func TestATRConstantRange(t *testing.T) {
	var s market.Series
	d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		s = append(s, market.Bar{Date: d.AddDate(0, 0, i), Open: 100, High: 101, Low: 99, Close: 100})
	}
	if got := ATR(s, 14); !near(got, 2) {
		t.Fatalf("ATR = %v, want 2", got)
	}
}

func TestReturnSkipsRecent(t *testing.T) {
	cl := []float64{100, 110, 120, 130, 200}
	// lookback 4, skip 1: closes[3]/closes[0]-1 = 0.3 (ignores the last bar)
	if got := Return(cl, 4, 1); !near(got, 0.3) {
		t.Fatalf("Return = %v, want 0.3", got)
	}
}

func TestPortfolioVolDiversifies(t *testing.T) {
	d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(sign float64) market.Series {
		var s market.Series
		p := 100.0
		for i := 0; i < 80; i++ {
			if i%2 == 0 {
				p *= 1 + 0.01*sign
			} else {
				p *= 1 - 0.01*sign
			}
			s = append(s, market.Bar{Date: d.AddDate(0, 0, i), Close: p})
		}
		return s
	}
	ds := market.Dataset{"A": mk(1), "B": mk(-1)}
	single := PortfolioVol(map[string]float64{"A": 1}, ds, 63)
	hedged := PortfolioVol(map[string]float64{"A": 0.5, "B": 0.5}, ds, 63)
	if !(single > 0.1) || !(hedged < single*0.1) {
		t.Fatalf("expected perfectly anti-correlated assets to cancel: single %.4f hedged %.4f", single, hedged)
	}
}
