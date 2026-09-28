package intraday

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	ind "github.com/midhunvinay1/tradebot/internal/indicators"
	"github.com/midhunvinay1/tradebot/internal/market"
)

const (
	rthOpen  = 9*60 + 30
	rthClose = 16 * 60
)

// IsRTH reports whether a bar starting at t is in regular trading hours.
func IsRTH(t time.Time) bool {
	m := MinuteOfDay(t)
	return m >= rthOpen && m < rthClose
}

// Session is one regular-hours trading day of minute bars.
type Session struct {
	Date time.Time
	Bars market.Series
}

// SplitSessions groups minute bars (sorted by time) into RTH sessions.
func SplitSessions(s market.Series) []Session {
	var out []Session
	for _, b := range s {
		if !IsRTH(b.Date) {
			continue
		}
		d := market.TradingDate(b.Date)
		if len(out) == 0 || !out[len(out)-1].Date.Equal(d) {
			out = append(out, Session{Date: d})
		}
		out[len(out)-1].Bars = append(out[len(out)-1].Bars, b)
	}
	return out
}

// Daily aggregates a session into a daily bar dated at the session date.
func (s Session) Daily() market.Bar {
	b := market.Bar{Date: s.Date, Open: s.Bars[0].Open, High: s.Bars[0].High, Low: s.Bars[0].Low, Close: s.Bars[len(s.Bars)-1].Close}
	for _, x := range s.Bars {
		b.High = math.Max(b.High, x.High)
		b.Low = math.Min(b.Low, x.Low)
		b.Volume += x.Volume
	}
	return b
}

// ORVolume is the volume traded in the first n minutes of the session.
func (s Session) ORVolume(n int) float64 {
	v := 0.0
	for _, b := range s.Bars {
		if m := MinuteOfDay(b.Date); m >= rthOpen && m < rthOpen+n {
			v += b.Volume
		}
	}
	return v
}

// History is the per-symbol daily data needed to build pre-open contexts.
type History struct {
	Daily    market.Series // one bar per prior session
	ORVolume []float64     // opening-range volume aligned with Daily
}

// Context computes the pre-open context for `day` from sessions strictly
// before it. ok is false when there is not enough history.
func (h History) Context(sym string, day time.Time, cfg config.IntradayConfig) (SymbolContext, bool) {
	i := sort.Search(len(h.Daily), func(i int) bool { return !h.Daily[i].Date.Before(day) })
	if i < cfg.ATRDays+1 || i < cfg.RVOLLookbackDays {
		return SymbolContext{}, false
	}
	atr := ind.ATR(h.Daily[:i], cfg.ATRDays)
	sum, n := 0.0, 0
	for _, v := range h.ORVolume[i-cfg.RVOLLookbackDays : i] {
		if v > 0 {
			sum += v
			n++
		}
	}
	if n == 0 || math.IsNaN(atr) {
		return SymbolContext{}, false
	}
	return SymbolContext{Symbol: sym, ATR: atr, AvgORVolume: sum / float64(n), PrevClose: h.Daily[i-1].Close, SizeMult: 1}, true
}

// HistoryFromSessions builds History from minute sessions.
func HistoryFromSessions(sess []Session, orMinutes int) History {
	h := History{}
	for _, s := range sess {
		h.Daily = append(h.Daily, s.Daily())
		h.ORVolume = append(h.ORVolume, s.ORVolume(orMinutes))
	}
	return h
}

// ---- Minute CSV storage (optionally gzip) ----

// LoadMinuteDir loads <SYMBOL>.csv.gz or <SYMBOL>.csv minute files
// (header: timestamp,open,high,low,close,volume; RFC3339 timestamps).
func LoadMinuteDir(dir string, symbols []string) (market.Dataset, error) {
	ds := market.Dataset{}
	for _, sym := range symbols {
		path := filepath.Join(dir, sym+".csv.gz")
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(dir, sym+".csv")
		}
		s, err := LoadMinuteCSV(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sym, err)
		}
		ds[sym] = s
	}
	return ds, nil
}

func LoadMinuteCSV(path string) (market.Series, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	var s market.Series
	for line := 1; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if line == 1 && strings.EqualFold(rec[0], "timestamp") {
			continue
		}
		if len(rec) < 6 {
			return nil, fmt.Errorf("line %d: want 6 columns", line)
		}
		t, err := time.Parse(time.RFC3339, rec[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		var v [5]float64
		for j := 0; j < 5; j++ {
			if v[j], err = strconv.ParseFloat(rec[j+1], 64); err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
		}
		s = append(s, market.Bar{Date: t.UTC(), Open: v[0], High: v[1], Low: v[2], Close: v[3], Volume: v[4]})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Date.Before(s[j].Date) })
	return s, nil
}

// SaveMinuteDir writes one gzip CSV per symbol.
func SaveMinuteDir(dir string, ds market.Dataset) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for sym, s := range ds {
		f, err := os.Create(filepath.Join(dir, sym+".csv.gz"))
		if err != nil {
			return err
		}
		gz := gzip.NewWriter(f)
		w := csv.NewWriter(gz)
		_ = w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"})
		for _, b := range s {
			_ = w.Write([]string{b.Date.UTC().Format(time.RFC3339), f4(b.Open), f4(b.High), f4(b.Low), f4(b.Close), f4(b.Volume)})
		}
		w.Flush()
		if err := w.Error(); err != nil {
			f.Close()
			return err
		}
		if err := gz.Close(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

func f4(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }

// SyntheticMinutes generates random minute bars (RTH only) with occasional
// high-volume trend days. It is ONLY for exercising the pipeline.
func SyntheticMinutes(symbols []string, start time.Time, days int, seed int64) market.Dataset {
	rng := rand.New(rand.NewSource(seed))
	ds := market.Dataset{}
	prices := map[string]float64{}
	baseVol := map[string]float64{}
	for _, s := range symbols {
		prices[s] = 20 + 200*rng.Float64()
		baseVol[s] = 2e4 + 2e5*rng.Float64()
	}
	d := market.Day(start)
	for n := 0; n < days; d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		n++
		open := time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, market.NewYork)
		for _, s := range symbols {
			p := prices[s] * math.Exp(0.01*rng.NormFloat64()) // overnight gap
			inPlay := rng.Float64() < 0.12
			drift, volMult := 0.0, 1.0
			if inPlay {
				volMult = 2 + 3*rng.Float64()
				drift = (rng.Float64() - 0.35) * 0.00008 // mild continuation bias
			}
			sigma := 0.0009
			for i := 0; i < 390; i++ {
				o := p
				p *= math.Exp(drift + sigma*rng.NormFloat64())
				hi := math.Max(o, p) * (1 + math.Abs(0.0004*rng.NormFloat64()))
				lo := math.Min(o, p) * (1 - math.Abs(0.0004*rng.NormFloat64()))
				u := float64(i) / 389
				shape := 0.6 + 2.5*math.Pow(2*u-1, 4) // U-shaped intraday volume
				vm := 1.0
				if i < 30 {
					vm = volMult
				}
				ds[s] = append(ds[s], market.Bar{Date: open.Add(time.Duration(i) * time.Minute).UTC(),
					Open: o, High: hi, Low: lo, Close: p, Volume: math.Round(baseVol[s] * shape * vm * (0.6 + 0.8*rng.Float64()))})
			}
			prices[s] = p
		}
	}
	return ds
}
