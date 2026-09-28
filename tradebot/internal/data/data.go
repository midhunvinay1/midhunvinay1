// Package data loads and stores daily bars (CSV) and generates synthetic
// data for pipeline tests.
package data

import (
	"encoding/csv"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/market"
)

// LoadCSVDir loads <SYMBOL>.csv files (header: date,open,high,low,close,volume).
// If symbols is non-empty only those files are read, and missing files are errors.
func LoadCSVDir(dir string, symbols []string) (market.Dataset, error) {
	ds := market.Dataset{}
	if len(symbols) == 0 {
		files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			symbols = append(symbols, strings.TrimSuffix(filepath.Base(f), ".csv"))
		}
	}
	for _, sym := range symbols {
		s, err := LoadCSV(filepath.Join(dir, sym+".csv"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sym, err)
		}
		ds[sym] = s
	}
	return ds, nil
}

func LoadCSV(path string) (market.Series, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	var s market.Series
	for i, r := range rows {
		if i == 0 && strings.EqualFold(strings.TrimSpace(r[0]), "date") {
			continue
		}
		if len(r) < 6 {
			return nil, fmt.Errorf("line %d: want 6 columns", i+1)
		}
		d, err := time.Parse("2006-01-02", strings.TrimSpace(r[0]))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		var v [5]float64
		for j := 0; j < 5; j++ {
			if v[j], err = strconv.ParseFloat(strings.TrimSpace(r[j+1]), 64); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
		s = append(s, market.Bar{Date: market.Day(d), Open: v[0], High: v[1], Low: v[2], Close: v[3], Volume: v[4]})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Date.Before(s[j].Date) })
	return s, nil
}

// SaveCSVDir writes one CSV per symbol.
func SaveCSVDir(dir string, ds market.Dataset) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for sym, s := range ds {
		f, err := os.Create(filepath.Join(dir, sym+".csv"))
		if err != nil {
			return err
		}
		w := csv.NewWriter(f)
		_ = w.Write([]string{"date", "open", "high", "low", "close", "volume"})
		for _, b := range s {
			_ = w.Write([]string{b.Date.Format("2006-01-02"), ff(b.Open), ff(b.High), ff(b.Low), ff(b.Close), ff(b.Volume)})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func ff(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }

// Synthetic generates correlated random-walk data with bull/bear regimes.
// It is ONLY for exercising the pipeline; results on it mean nothing.
func Synthetic(symbols []string, benchmark string, start time.Time, years int, seed int64) market.Dataset {
	rng := rand.New(rand.NewSource(seed))
	var days []time.Time
	for d := start; d.Before(start.AddDate(years, 0, 0)); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			days = append(days, market.Day(d))
		}
	}
	// Market factor with regime switching.
	mkt := make([]float64, len(days))
	bull := true
	for i := range days {
		if rng.Float64() < 1.0/250 {
			bull = !bull
		}
		mu, sd := 0.0006, 0.009
		if !bull {
			mu, sd = -0.0008, 0.018
		}
		mkt[i] = mu + sd*rng.NormFloat64()
	}
	ds := market.Dataset{}
	for k, sym := range symbols {
		beta, idio, drift := 1.0, 0.0, 0.0
		price := 50 + 100*rng.Float64()
		if sym != benchmark {
			beta = 0.6 + 0.8*rng.Float64()
			idio = 0.008 + 0.012*rng.Float64()
			drift = (rng.Float64() - 0.4) * 0.0006
		}
		if sym == "SGOV" || sym == "BIL" {
			beta, idio, drift, price = 0, 0.0002, 0.00015, 100
		}
		vol := 5e6 + 2e7*rng.Float64()
		s := make(market.Series, len(days))
		for i, d := range days {
			r := drift + beta*mkt[i] + idio*rng.NormFloat64()
			open := price * (1 + 0.002*rng.NormFloat64())
			price *= math.Exp(r)
			hi := math.Max(open, price) * (1 + math.Abs(0.006*rng.NormFloat64()))
			lo := math.Min(open, price) * (1 - math.Abs(0.006*rng.NormFloat64()))
			s[i] = market.Bar{Date: d, Open: open, High: hi, Low: lo, Close: price, Volume: vol * (0.7 + 0.6*rng.Float64())}
		}
		ds[sym] = s
		_ = k
	}
	return ds
}
