package intraday

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if d > 0 {
		c.t = c.t.Add(d)
	}
	return ctx.Err()
}

// fakeFeed replays minute bars and never reveals a bar before it completes.
type fakeFeed struct {
	ds    market.Dataset
	clock *fakeClock
	news  map[string][]market.News
}

func (f *fakeFeed) visible(sym string) market.Series {
	return f.ds[sym].Until(f.clock.Now().Add(-time.Minute))
}

func (f *fakeFeed) DailyBars(_ context.Context, syms []string, start, end time.Time) (market.Dataset, error) {
	out := market.Dataset{}
	for _, s := range syms {
		for _, sess := range SplitSessions(f.visible(s)) {
			if !sess.Date.Before(start) && sess.Date.Before(end) {
				out[s] = append(out[s], sess.Daily())
			}
		}
	}
	return out, nil
}

func (f *fakeFeed) IntradayBars(_ context.Context, syms []string, tf string, start, end time.Time) (market.Dataset, error) {
	var n int
	if _, err := fmt.Sscanf(tf, "%dMin", &n); err != nil {
		return nil, err
	}
	out := market.Dataset{}
	for _, s := range syms {
		for _, b := range f.visible(s) {
			if b.Date.Before(start) || !b.Date.Before(end) {
				continue
			}
			bucket := b.Date.Truncate(time.Duration(n) * time.Minute)
			ser := out[s]
			if len(ser) > 0 && ser[len(ser)-1].Date.Equal(bucket) {
				last := &ser[len(ser)-1]
				last.High, last.Low = max(last.High, b.High), min(last.Low, b.Low)
				last.Close, last.Volume = b.Close, last.Volume+b.Volume
			} else {
				b.Date = bucket
				ser = append(ser, b)
			}
			out[s] = ser
		}
	}
	return out, nil
}

func (f *fakeFeed) LatestQuotes(_ context.Context, syms []string) (map[string]Quote, error) {
	out := map[string]Quote{}
	for _, s := range syms {
		if v := f.visible(s); len(v) > 0 {
			b := v[len(v)-1]
			out[s] = Quote{Price: b.Close, Time: b.Date.Add(59 * time.Second)}
		}
	}
	return out, nil
}

func (f *fakeFeed) NewsMulti(_ context.Context, syms []string, _ time.Time, _ int) (map[string][]market.News, error) {
	return f.news, nil
}

func (f *fakeFeed) IsTradingDay(_ context.Context, d time.Time) (bool, error) {
	return d.Weekday() != time.Saturday && d.Weekday() != time.Sunday, nil
}

type vetoOne struct{ sym string }

func (v vetoOne) Premarket(_ context.Context, _ string, items []llm.PremarketItem) (map[string]llm.PremarketVerdict, error) {
	out := map[string]llm.PremarketVerdict{}
	for _, it := range items {
		d := "approve"
		if it.Symbol == v.sym {
			d = "veto"
		}
		out[it.Symbol] = llm.PremarketVerdict{Symbol: it.Symbol, Decision: d, SizeMultiplier: 1}
	}
	return out, nil
}

func TestLiveSessionEndToEnd(t *testing.T) {
	cfg := config.Defaults()
	syms := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF", "GGG", "HHH"}
	cfg.Intraday.Universe = syms
	cfg.Intraday.MaxQuoteAgeSeconds = 120 // the fake feed updates once a minute
	cfg.LLM.OnError = "skip"
	ds := SyntheticMinutes(syms, time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC), 45, 5)

	// Pick the busiest late session in a backtest so the live run has breakouts.
	bt, err := Backtest(context.Background(), cfg, ds, BacktestOptions{InitialEquity: 10000})
	if err != nil {
		t.Fatal(err)
	}
	perDay := map[time.Time][]string{}
	for _, tr := range bt.Trades {
		d := market.TradingDate(tr.EntryTime)
		perDay[d] = append(perDay[d], tr.Symbol)
	}
	var days []time.Time
	for d := range perDay {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool {
		if len(perDay[days[i]]) != len(perDay[days[j]]) {
			return len(perDay[days[i]]) > len(perDay[days[j]])
		}
		return days[i].Before(days[j])
	})
	if len(days) == 0 {
		t.Fatal("no backtest trades to replay")
	}
	day := days[0]
	vetoed := perDay[day][0]

	clock := &fakeClock{t: time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, market.NewYork)}
	feed := &fakeFeed{ds: ds, clock: clock, news: map[string][]market.News{
		vetoed: {{Time: clock.t, Source: "test", Headline: "Company agrees to be acquired for $50.00 per share in cash"}},
	}}
	sim := broker.NewPaperSim(10000, 3)
	dir := t.TempDir()
	j, _ := journal.Open(dir)
	var out bytes.Buffer
	l := &Live{Cfg: cfg, StateDir: dir, Broker: sim, Feed: feed, Reviewer: vetoOne{vetoed}, Journal: j, Clock: clock, Out: &out}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	log := out.String()
	if !strings.Contains(log, "FILLED buy") {
		t.Fatalf("expected at least one entry fill:\n%s", log)
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "FILLED buy") && strings.Contains(line, " "+vetoed+" ") {
			t.Fatalf("vetoed symbol %s was bought:\n%s", vetoed, log)
		}
	}
	t.Log(log)
	acct, _ := sim.Account(context.Background())
	if len(acct.Positions) != 0 {
		t.Fatalf("must be flat after the session, holding %v\n%s", acct.Positions, log)
	}
	st, err := LoadLiveState(dir)
	if err != nil || st.DayTrades[day.Format("2006-01-02")] == 0 || !st.LastRunDate.Equal(day) {
		t.Fatalf("state not saved: %+v %v", st, err)
	}
	if MinuteOfDay(clock.t) > 16*60 {
		t.Fatalf("loop ran past the close: %s", clock.t.In(market.NewYork))
	}

	out.Reset()
	if err := l.Run(context.Background()); err != nil || !strings.Contains(out.String(), "already ran") {
		t.Fatalf("second run must be a no-op: %v %s", err, out.String())
	}
}

func TestLeftoversFromCrashAreFlattened(t *testing.T) {
	cfg := config.Defaults()
	syms := []string{"AAA", "BBB", "CCC"}
	cfg.Intraday.Universe = syms
	cfg.Intraday.MaxQuoteAgeSeconds = 120
	cfg.Intraday.PremarketReview = false
	ds := SyntheticMinutes(syms, time.Date(2025, 3, 3, 0, 0, 0, 0, time.UTC), 30, 9)
	day := market.TradingDate(ds["AAA"][len(ds["AAA"])-1].Date)
	clock := &fakeClock{t: time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, market.NewYork)}

	sim := broker.NewPaperSim(10000, 3)
	// A previous session crashed holding 5 BBB; another strategy holds 3 CCC.
	for _, o := range []portfolio.Order{
		{ID: "a", Symbol: "BBB", Side: portfolio.Buy, Qty: 5, RefPrice: 50, LimitPrice: 51},
		{ID: "b", Symbol: "CCC", Side: portfolio.Buy, Qty: 3, RefPrice: 50, LimitPrice: 51},
	} {
		if _, err := sim.Submit(context.Background(), []portfolio.Order{o}); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if err := SaveLiveState(dir, &LiveState{DayTrades: map[string]int{}, OpenAtClose: map[string]float64{"BBB": 5}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	l := &Live{Cfg: cfg, StateDir: dir, Broker: sim, Feed: &fakeFeed{ds: ds, clock: clock}, Clock: clock, Out: &out}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	acct, _ := sim.Account(context.Background())
	if _, ok := acct.Positions["BBB"]; ok {
		t.Fatalf("crashed-session leftover BBB was not sold:\n%s", out.String())
	}
	if acct.Positions["CCC"].Qty != 3 {
		t.Fatalf("positions opened by other strategies must never be touched: %v\n%s", acct.Positions, out.String())
	}
}
