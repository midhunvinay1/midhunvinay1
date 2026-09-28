package backtest

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
)

// SweepRow is one parameter combination scored in-sample and out-of-sample.
type SweepRow struct {
	Params string
	IS     Metrics
	OOS    Metrics
}

// Variant mutates a copy of the config and describes the change.
type Variant struct {
	Name  string
	Apply func(*config.Config)
}

// DefaultGrid is a small robustness grid around the default parameters.
func DefaultGrid() [][]Variant {
	var topN, rsi, tv, mix []Variant
	for _, n := range []int{3, 5, 8} {
		topN = append(topN, Variant{fmt.Sprintf("top_n=%d", n), func(c *config.Config) { c.Strategy.Momentum.TopN = n }})
	}
	for _, r := range []float64{5, 10, 15} {
		rsi = append(rsi, Variant{fmt.Sprintf("entry_rsi=%.0f", r), func(c *config.Config) { c.Strategy.MeanReversion.EntryRSI = r }})
	}
	for _, v := range []float64{0.14, 0.18, 0.22} {
		tv = append(tv, Variant{fmt.Sprintf("target_vol=%.2f", v), func(c *config.Config) { c.Strategy.TargetVol = v }})
	}
	for _, w := range []float64{0.5, 0.6, 0.7} {
		mix = append(mix, Variant{fmt.Sprintf("mom_w=%.1f", w), func(c *config.Config) {
			c.Strategy.Momentum.Weight, c.Strategy.MeanReversion.Weight = w, 1-w
		}})
	}
	return [][]Variant{topN, rsi, tv, mix}
}

// Sweep runs the cartesian product of the grid in parallel. Each combination is
// ranked on the in-sample window [start, split) and reported on the
// out-of-sample window [split, end]. Pick parameters from a broad stable
// region, never from the single best in-sample row.
func Sweep(ctx context.Context, base *config.Config, data market.Dataset, grid [][]Variant, start, split, end time.Time, workers int) ([]SweepRow, error) {
	combos := [][]Variant{{}}
	for _, dim := range grid {
		var next [][]Variant
		for _, c := range combos {
			for _, v := range dim {
				next = append(next, append(append([]Variant{}, c...), v))
			}
		}
		combos = next
	}
	rows := make([]SweepRow, len(combos))
	errs := make([]error, len(combos))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < max(1, workers); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				cfg := *base
				cfg.Strategy.Momentum.Lookbacks = append([]int{}, base.Strategy.Momentum.Lookbacks...)
				name := ""
				for _, v := range combos[i] {
					v.Apply(&cfg)
					name += v.Name + " "
				}
				is, err := Run(ctx, &cfg, data, Options{Start: start, End: split.AddDate(0, 0, -1)})
				if err != nil {
					errs[i] = err
					continue
				}
				oos, err := Run(ctx, &cfg, data, Options{Start: split, End: end})
				if err != nil {
					errs[i] = err
					continue
				}
				rows[i] = SweepRow{Params: name, IS: is.Metrics, OOS: oos.Metrics}
			}
		}()
	}
	for i := range combos {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].IS.Sharpe > rows[j].IS.Sharpe })
	return rows, nil
}

// WriteSweep prints the ranked table.
func WriteSweep(w io.Writer, rows []SweepRow, limit int) {
	fmt.Fprintf(w, "%-52s | %6s %6s %6s | %6s %6s %6s\n", "params (ranked by in-sample Sharpe)", "IS SR", "CAGR", "MaxDD", "OOS SR", "CAGR", "MaxDD")
	for i, r := range rows {
		if i >= limit {
			break
		}
		fmt.Fprintf(w, "%-52s | %6.2f %5.1f%% %5.1f%% | %6.2f %5.1f%% %5.1f%%\n", r.Params,
			r.IS.Sharpe, r.IS.CAGR*100, r.IS.MaxDrawdown*100, r.OOS.Sharpe, r.OOS.CAGR*100, r.OOS.MaxDrawdown*100)
	}
	if n := len(rows); n > 0 {
		med := make([]float64, n)
		for i, r := range rows {
			med[i] = r.OOS.Sharpe
		}
		sort.Float64s(med)
		fmt.Fprintf(w, "\n%d combinations | median OOS Sharpe %.2f | worst %.2f | best %.2f\n", n, med[n/2], med[0], med[n-1])
	}
}
