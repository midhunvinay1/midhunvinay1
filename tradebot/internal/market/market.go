// Package market holds the core price data types.
package market

import (
	"sort"
	"time"
)

// Bar is one daily OHLCV bar. Date is the trading date at 00:00 UTC.
type Bar struct {
	Date   time.Time `json:"date"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume float64   `json:"volume"`
}

// Series is a date-ascending list of daily bars for one symbol.
type Series []Bar

// Until returns the prefix of bars with Date <= d. It never copies.
func (s Series) Until(d time.Time) Series {
	i := sort.Search(len(s), func(i int) bool { return s[i].Date.After(d) })
	return s[:i]
}

// At returns the bar dated exactly d.
func (s Series) At(d time.Time) (Bar, bool) {
	i := sort.Search(len(s), func(i int) bool { return !s[i].Date.Before(d) })
	if i < len(s) && s[i].Date.Equal(d) {
		return s[i], true
	}
	return Bar{}, false
}

func (s Series) Closes() []float64 {
	out := make([]float64, len(s))
	for i, b := range s {
		out[i] = b.Close
	}
	return out
}

func (s Series) Last() Bar { return s[len(s)-1] }

// Tail returns the last n bars (or all of them if there are fewer).
func (s Series) Tail(n int) Series {
	if n >= len(s) {
		return s
	}
	return s[len(s)-n:]
}

// News is one headline. Its text is untrusted external content.
type News struct {
	Time     time.Time `json:"time"`
	Source   string    `json:"source"`
	Headline string    `json:"headline"`
	Summary  string    `json:"summary,omitempty"`
}

// Dataset maps symbol -> series.
type Dataset map[string]Series

// Until returns a view of every series truncated at d (no look-ahead).
func (ds Dataset) Until(d time.Time) Dataset {
	out := make(Dataset, len(ds))
	for sym, s := range ds {
		if t := s.Until(d); len(t) > 0 {
			out[sym] = t
		}
	}
	return out
}

// SortedKeys returns map keys in ascending order. Use it wherever floats are
// summed over a map so results are bit-for-bit reproducible.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Day normalizes t to its calendar date at 00:00 UTC.
func Day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// NewYork is the exchange time zone (falls back to fixed EST if tzdata is missing).
var NewYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.FixedZone("EST", -5*3600)
	}
	return loc
}()

// TradingDate returns the New York calendar date of t at 00:00 UTC.
func TradingDate(t time.Time) time.Time { return Day(t.In(NewYork)) }
