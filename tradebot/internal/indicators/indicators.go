// Package indicators implements the technical indicators the strategy uses.
// Every function looks only at the slice it is given (the caller truncates
// history at the decision date), and returns NaN when there is not enough data.
package indicators

import (
	"math"

	"github.com/midhunvinay1/tradebot/internal/market"
)

var nan = math.NaN()

// SMA is the simple moving average of the last n values.
func SMA(xs []float64, n int) float64 {
	if n <= 0 || len(xs) < n {
		return nan
	}
	sum := 0.0
	for _, x := range xs[len(xs)-n:] {
		sum += x
	}
	return sum / float64(n)
}

// RSI is Wilder's relative strength index over period n, evaluated at the last
// value. It warms up over a bounded window so it is O(window), not O(history).
func RSI(closes []float64, n int) float64 {
	window := 10*n + 50
	if n <= 0 || len(closes) < n+1 {
		return nan
	}
	if len(closes) > window {
		closes = closes[len(closes)-window:]
	}
	var gain, loss float64
	for i := 1; i <= n; i++ {
		d := closes[i] - closes[i-1]
		if d > 0 {
			gain += d
		} else {
			loss -= d
		}
	}
	gain /= float64(n)
	loss /= float64(n)
	for i := n + 1; i < len(closes); i++ {
		d := closes[i] - closes[i-1]
		g, l := 0.0, 0.0
		if d > 0 {
			g = d
		} else {
			l = -d
		}
		gain = (gain*float64(n-1) + g) / float64(n)
		loss = (loss*float64(n-1) + l) / float64(n)
	}
	if loss == 0 {
		if gain == 0 {
			return 50
		}
		return 100
	}
	rs := gain / loss
	return 100 - 100/(1+rs)
}

// ATR is Wilder's average true range over period n at the last bar.
func ATR(bars market.Series, n int) float64 {
	window := 5*n + 50
	if n <= 0 || len(bars) < n+1 {
		return nan
	}
	if len(bars) > window {
		bars = bars[len(bars)-window:]
	}
	tr := func(i int) float64 {
		h, l, pc := bars[i].High, bars[i].Low, bars[i-1].Close
		return math.Max(h-l, math.Max(math.Abs(h-pc), math.Abs(l-pc)))
	}
	atr := 0.0
	for i := 1; i <= n; i++ {
		atr += tr(i)
	}
	atr /= float64(n)
	for i := n + 1; i < len(bars); i++ {
		atr = (atr*float64(n-1) + tr(i)) / float64(n)
	}
	return atr
}

// Return is closes[t-skip]/closes[t-lookback]-1, where t is the last index.
func Return(closes []float64, lookback, skip int) float64 {
	t := len(closes) - 1
	if lookback <= skip || t-lookback < 0 {
		return nan
	}
	from := closes[t-lookback]
	if from <= 0 {
		return nan
	}
	return closes[t-skip]/from - 1
}

// LogReturns returns the last n daily log returns.
func LogReturns(closes []float64, n int) []float64 {
	if len(closes) < n+1 {
		return nil
	}
	c := closes[len(closes)-n-1:]
	out := make([]float64, n)
	for i := 1; i < len(c); i++ {
		if c[i-1] <= 0 || c[i] <= 0 {
			return nil
		}
		out[i-1] = math.Log(c[i] / c[i-1])
	}
	return out
}

// AnnualizedVol is the annualized standard deviation of the last n daily log returns.
func AnnualizedVol(closes []float64, n int) float64 {
	r := LogReturns(closes, n)
	if len(r) < 2 {
		return nan
	}
	return StdDev(r) * math.Sqrt(252)
}

// AvgDollarVolume is the mean of close*volume over the last n bars.
func AvgDollarVolume(bars market.Series, n int) float64 {
	if len(bars) < n || n <= 0 {
		return nan
	}
	sum := 0.0
	for _, b := range bars[len(bars)-n:] {
		sum += b.Close * b.Volume
	}
	return sum / float64(n)
}

func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return nan
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// StdDev is the sample standard deviation.
func StdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return nan
	}
	m := Mean(xs)
	ss := 0.0
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// PortfolioVol returns the annualized ex-ante volatility of a weight vector
// using the sample covariance of the last n daily log returns of each asset.
// Assets without enough history are ignored (treated as zero-variance cash).
func PortfolioVol(weights map[string]float64, data market.Dataset, n int) float64 {
	type asset struct {
		w float64
		r []float64
	}
	var assets []asset
	for _, sym := range market.SortedKeys(weights) {
		w := weights[sym]
		if w == 0 {
			continue
		}
		s, ok := data[sym]
		if !ok {
			continue
		}
		if r := LogReturns(s.Tail(n+1).Closes(), n); r != nil {
			assets = append(assets, asset{w, r})
		}
	}
	if len(assets) == 0 {
		return 0
	}
	means := make([]float64, len(assets))
	for i, a := range assets {
		means[i] = Mean(a.r)
	}
	variance := 0.0
	for i := range assets {
		for j := range assets {
			cov := 0.0
			for k := 0; k < n; k++ {
				cov += (assets[i].r[k] - means[i]) * (assets[j].r[k] - means[j])
			}
			cov /= float64(n - 1)
			variance += assets[i].w * assets[j].w * cov
		}
	}
	if variance <= 0 {
		return 0
	}
	return math.Sqrt(variance * 252)
}
