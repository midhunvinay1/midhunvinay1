// Package cliutil holds small helpers shared by the tradebot and trendbot CLIs.
package cliutil

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/midhunvinay1/tradebot/internal/backtest"
	"github.com/midhunvinay1/tradebot/internal/market"
)

// ParseDate parses YYYY-MM-DD ("" = zero time).
func ParseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	return market.Day(t), err
}

// WriteBacktest writes summary.json, equity.csv, trades.csv and fills.csv.
func WriteBacktest(dir string, res *backtest.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		return err
	}
	f2 := func(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }
	rows := [][]string{{"date", "equity", "cash", "gross", "benchmark"}}
	for _, p := range res.Equity {
		rows = append(rows, []string{p.Date.Format("2006-01-02"), f2(p.Equity), f2(p.Cash), f2(p.Gross), f2(p.Benchmark)})
	}
	if err := WriteCSV(filepath.Join(dir, "equity.csv"), rows); err != nil {
		return err
	}
	rows = [][]string{{"symbol", "entry_date", "exit_date", "cost", "pnl", "pnl_pct", "hold_days"}}
	for _, t := range res.Trades {
		rows = append(rows, []string{t.Symbol, t.EntryDate.Format("2006-01-02"), t.ExitDate.Format("2006-01-02"),
			f2(t.Cost), f2(t.PnL), f2(t.PnLPct), strconv.Itoa(t.HoldDays)})
	}
	if err := WriteCSV(filepath.Join(dir, "trades.csv"), rows); err != nil {
		return err
	}
	rows = [][]string{{"date", "symbol", "side", "qty", "price", "kind", "reason"}}
	for _, f := range res.Fills {
		rows = append(rows, []string{f.Date.Format("2006-01-02"), f.Symbol, f.Side, f2(f.Qty), f2(f.Price), f.Kind, f.Reason})
	}
	if err := WriteCSV(filepath.Join(dir, "fills.csv"), rows); err != nil {
		return err
	}
	fmt.Printf("Wrote %s/{summary.json,equity.csv,trades.csv,fills.csv}\n", dir)
	return nil
}

// WriteCSV writes rows to path.
func WriteCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
