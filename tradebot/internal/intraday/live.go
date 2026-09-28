package intraday

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/alpaca"
	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/notify"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
	"github.com/midhunvinay1/tradebot/internal/risk"
)

// Quote is the latest trade for a symbol.
type Quote struct {
	Price float64
	Time  time.Time
}

// Feed supplies market data to the live loop.
type Feed interface {
	DailyBars(ctx context.Context, syms []string, start, end time.Time) (market.Dataset, error)
	IntradayBars(ctx context.Context, syms []string, timeframe string, start, end time.Time) (market.Dataset, error)
	LatestQuotes(ctx context.Context, syms []string) (map[string]Quote, error)
	NewsMulti(ctx context.Context, syms []string, since time.Time, perSymbol int) (map[string][]market.News, error)
	IsTradingDay(ctx context.Context, day time.Time) (bool, error)
}

// AlpacaFeed adapts the Alpaca client.
type AlpacaFeed struct{ C *alpaca.Client }

func (a AlpacaFeed) DailyBars(ctx context.Context, s []string, start, end time.Time) (market.Dataset, error) {
	return a.C.DailyBars(ctx, s, start, end)
}

// IntradayBars uses the real-time price feed for both today's opening range
// and its history, so relative volume compares the same feed.
func (a AlpacaFeed) IntradayBars(ctx context.Context, s []string, tf string, start, end time.Time) (market.Dataset, error) {
	return a.C.IntradayBarsFeed(ctx, s, tf, start, end, a.C.PriceFeed())
}
func (a AlpacaFeed) LatestQuotes(ctx context.Context, s []string) (map[string]Quote, error) {
	tr, err := a.C.LatestTrades(ctx, s)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Quote, len(tr))
	for k, v := range tr {
		out[k] = Quote{Price: v.Price, Time: v.Time}
	}
	return out, nil
}
func (a AlpacaFeed) NewsMulti(ctx context.Context, s []string, since time.Time, n int) (map[string][]market.News, error) {
	return a.C.NewsMulti(ctx, s, since, n)
}
func (a AlpacaFeed) IsTradingDay(ctx context.Context, d time.Time) (bool, error) {
	return a.C.IsTradingDay(ctx, d)
}

// Clock abstracts time so the loop can be tested.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// LiveState persists between sessions.
type LiveState struct {
	DayTrades   map[string]int `json:"day_trades"` // YYYY-MM-DD -> filled entries
	LastRunDate time.Time      `json:"last_run_date"`
	// OpenAtClose is what this mode itself failed to flatten last session. Only
	// these are sold as leftovers, never positions opened by other strategies.
	OpenAtClose map[string]float64 `json:"open_at_close,omitempty"`
	Risk        *risk.State        `json:"risk"`
}

func liveStatePath(dir string) string { return filepath.Join(dir, "intraday.json") }

func LoadLiveState(dir string) (*LiveState, error) {
	st := &LiveState{DayTrades: map[string]int{}, Risk: &risk.State{}}
	b, err := os.ReadFile(liveStatePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", liveStatePath(dir), err)
	}
	if st.DayTrades == nil {
		st.DayTrades = map[string]int{}
	}
	if st.Risk == nil {
		st.Risk = &risk.State{}
	}
	return st, nil
}

func SaveLiveState(dir string, st *LiveState) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := liveStatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, liveStatePath(dir))
}

// priorDayTrades counts filled entries in the 4 prior sessions (rolling 5-day window).
func (st *LiveState) priorDayTrades(today time.Time) int {
	n, sessions := 0, 0
	for d := today.AddDate(0, 0, -1); sessions < 4 && d.After(today.AddDate(0, 0, -14)); d = d.AddDate(0, 0, -1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		sessions++
		n += st.DayTrades[d.Format("2006-01-02")]
	}
	return n
}

// Live runs one intraday session end to end.
type Live struct {
	Cfg      *config.Config
	StateDir string
	Broker   broker.Tracker
	Feed     Feed
	Reviewer llm.PremarketReviewer // nil: no pre-market review
	Journal  *journal.Journal
	Notify   *notify.Notifier
	Clock    Clock
	Out      io.Writer
	Force    bool
}

type tracked struct {
	act       Action
	order     portfolio.Order
	leftover  bool
	submitted time.Time
	canceled  bool
}

type session struct {
	*Live
	eng        *Engine
	rk         *risk.Engine
	equity0    float64
	today      time.Time
	last       map[string]float64
	pending    map[string]*tracked
	leftovers  map[string]float64
	seq        int
	dayTrades  int
	baseTrades int // entries already recorded today (restart after a crash)
	lines      []string
	lastFail   map[string]string
	st         *LiveState
}

func (l *Live) logf(format string, a ...any) { fmt.Fprintf(l.Out, format+"\n", a...) }

func (s *session) note(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	s.lines = append(s.lines, line)
	s.logf("%s %s", s.Clock.Now().In(market.NewYork).Format("15:04:05"), line)
}

// Run trades one session. It returns after everything is flat (or 16:00 ET).
func (l *Live) Run(ctx context.Context) error {
	cfg, ic := l.Cfg, l.Cfg.Intraday
	now := l.Clock.Now()
	today := market.TradingDate(now)
	nyAt := func(h, m int) time.Time {
		return time.Date(today.Year(), today.Month(), today.Day(), h, m, 0, 0, market.NewYork)
	}
	st, err := LoadLiveState(l.StateDir)
	if err != nil {
		return err
	}
	if !l.Force && st.LastRunDate.Equal(today) {
		l.logf("Intraday session for %s already ran; use --force to run again.", today.Format("2006-01-02"))
		return nil
	}
	if ok, err := l.Feed.IsTradingDay(ctx, today); err != nil {
		l.logf("warning: calendar check failed: %v", err)
	} else if !ok && !l.Force {
		l.logf("Market is closed on %s.", today.Format("2006-01-02"))
		return nil
	}
	flattenMin, _ := config.ParseClock(ic.FlattenAt)
	if MinuteOfDay(now) >= flattenMin {
		l.logf("Too late to start today's session (after %s ET).", ic.FlattenAt)
		return nil
	}

	// ---- Pre-open ----
	if err := l.Clock.Sleep(ctx, nyAt(9, 15).Sub(l.Clock.Now())); err != nil {
		return err
	}
	rk := risk.New(cfg, st.Risk, risk.HaltFilePath(l.StateDir))
	if h, why := rk.Halted(); h && !cfg.Risk.HaltAllowsExits {
		l.logf("Kill switch engaged (%s); not trading.", why)
		return nil
	}
	acct, err := l.Broker.Account(ctx)
	if err != nil {
		return fmt.Errorf("broker account: %w", err)
	}
	rk.Update(acct.Equity, today)
	_ = l.Journal.Log("intraday_account", acct)

	universe := cfg.IntradaySymbols()
	ctxs, err := l.contexts(ctx, universe, today)
	if err != nil {
		return err
	}
	s := &session{Live: l, rk: rk, equity0: acct.Equity, today: today, last: map[string]float64{},
		pending: map[string]*tracked{}, leftovers: map[string]float64{}, lastFail: map[string]string{},
		st: st, baseTrades: st.DayTrades[today.Format("2006-01-02")]}
	mults := s.premarket(ctx, ctxs)
	buyingPower := math.Min(acct.Cash, acct.Equity)
	s.eng = NewEngine(ic, ctxs, acct.Equity, buyingPower, st.priorDayTrades(today)+s.baseTrades)
	for sym, m := range mults {
		s.eng.SetSizeMult(sym, m)
	}
	inUniverse := map[string]bool{}
	for _, u := range universe {
		inUniverse[u] = true
	}
	for sym, q := range st.OpenAtClose {
		if p, ok := acct.Positions[sym]; ok && inUniverse[sym] && p.Qty > 0 {
			s.leftovers[sym] = math.Min(q, p.Qty)
		}
	}
	s.note("prepared %d symbols, equity %.2f, buying power %.2f, leftovers %v", len(ctxs), acct.Equity, buyingPower, s.leftovers)

	// ---- Opening range ----
	if err := l.Clock.Sleep(ctx, nyAt(9, 30).Sub(l.Clock.Now())); err != nil {
		return err
	}
	orClose := nyAt(9, 30).Add(time.Duration(ic.OpeningRangeMinutes) * time.Minute)
	if err := l.Clock.Sleep(ctx, orClose.Add(5*time.Second).Sub(l.Clock.Now())); err != nil {
		return err
	}
	var ctxSyms []string
	for _, c := range ctxs {
		ctxSyms = append(ctxSyms, c.Symbol)
	}
	if len(ctxSyms) > 0 {
		orBars, err := l.Feed.IntradayBars(ctx, ctxSyms, "1Min", nyAt(9, 30), orClose)
		if err != nil {
			return fmt.Errorf("opening-range bars: %w", err)
		}
		for _, sym := range market.SortedKeys(orBars) {
			for _, b := range orBars[sym] {
				s.eng.AddORBar(sym, b)
			}
		}
	}
	inPlay := s.eng.FinalizeOpeningRange()
	var ip []string
	for _, sym := range inPlay {
		ip = append(ip, fmt.Sprintf("%s(RVOL %.1f)", sym, s.eng.RVOL(sym)))
	}
	s.note("stocks in play: %s", strings.Join(ip, ", "))
	_ = l.Journal.Log("intraday_in_play", inPlay)

	// ---- Trading loop ----
	flattenT := nyAt(0, 0).Add(time.Duration(flattenMin) * time.Minute)
	noEntryMin, _ := config.ParseClock(ic.NoEntriesAfter)
	hardStop := nyAt(16, 0)
	poll := time.Duration(ic.PollSeconds * float64(time.Second))
	maxAge := time.Duration(ic.MaxQuoteAgeSeconds * float64(time.Second))
	var queue []Action
	quoteErrors := 0
	for {
		now := l.Clock.Now()
		if !now.Before(hardStop) {
			break
		}
		if h, why := rk.Halted(); h {
			queue = append(queue, s.eng.Halt("kill switch: "+why, now, s.last)...)
		}
		watch := s.eng.Watchlist()
		for sym := range s.leftovers {
			watch = append(watch, sym)
		}
		if len(watch) > 0 {
			quotes, err := l.Feed.LatestQuotes(ctx, watch)
			if err != nil {
				quoteErrors++
				if quoteErrors == 1 || quoteErrors%30 == 0 {
					s.note("warning: quotes unavailable (%d in a row): %v", quoteErrors, err)
				}
			} else {
				quoteErrors = 0
				for _, sym := range watch {
					q, ok := quotes[sym]
					if !ok || q.Price <= 0 || now.Sub(q.Time) > maxAge {
						continue // stale quotes never trigger anything
					}
					s.last[sym] = q.Price
					queue = append(queue, s.eng.OnPrice(sym, q.Price, q.Price, now)...)
				}
			}
		}
		queue = append(queue, s.eng.CheckLossLimit(now, s.last)...)
		if !now.Before(flattenT) {
			queue = append(queue, s.eng.FlattenAll(now, s.last)...)
		}
		s.exitLeftovers(ctx, now)
		for _, a := range queue {
			s.execute(ctx, a, now)
		}
		queue = append(queue[:0], s.poll(ctx, now)...)

		idle := len(s.eng.OpenPositions()) == 0 && len(s.pending) == 0 && len(s.leftovers) == 0 && len(queue) == 0
		if idle && (!now.Before(flattenT) || MinuteOfDay(now) >= noEntryMin || len(inPlay) == 0) {
			break
		}
		if h, _ := s.eng.Halted(); h && idle {
			break
		}
		if err := l.Clock.Sleep(ctx, poll); err != nil {
			// Interrupted: record state but do not mark the day complete, so
			// a restart today will flatten anything still open.
			_ = s.wrapUp(context.Background(), st, false)
			return err
		}
	}
	return s.wrapUp(ctx, st, true)
}

// contexts builds pre-open contexts from daily and opening-range history.
func (l *Live) contexts(ctx context.Context, universe []string, today time.Time) ([]SymbolContext, error) {
	ic := l.Cfg.Intraday
	daily, err := l.Feed.DailyBars(ctx, universe, today.AddDate(0, 0, -(ic.ATRDays*2+30)), today)
	if err != nil {
		return nil, fmt.Errorf("daily bars: %w", err)
	}
	daily = daily.Until(today.AddDate(0, 0, -1))
	tf := "1Min"
	if 30%ic.OpeningRangeMinutes == 0 {
		tf = fmt.Sprintf("%dMin", ic.OpeningRangeMinutes)
	}
	orBars, err := l.Feed.IntradayBars(ctx, universe, tf, today.AddDate(0, 0, -(ic.RVOLLookbackDays*2+14)), today)
	if err != nil {
		return nil, fmt.Errorf("opening-range history: %w", err)
	}
	var out []SymbolContext
	for _, sym := range universe {
		vol := map[time.Time]float64{}
		for _, b := range orBars[sym] {
			if m := MinuteOfDay(b.Date); m >= rthOpen && m < rthOpen+ic.OpeningRangeMinutes {
				vol[market.TradingDate(b.Date)] += b.Volume
			}
		}
		h := History{Daily: daily[sym]}
		for _, d := range h.Daily {
			h.ORVolume = append(h.ORVolume, vol[d.Date])
		}
		if c, ok := h.Context(sym, today, ic); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// premarket asks Claude once to screen symbols with overnight news.
func (s *session) premarket(ctx context.Context, ctxs []SymbolContext) map[string]float64 {
	ic := s.Cfg.Intraday
	if !ic.PremarketReview || s.Reviewer == nil || len(ctxs) == 0 {
		return nil
	}
	byName := map[string]SymbolContext{}
	var syms []string
	for _, c := range ctxs {
		byName[c.Symbol] = c
		syms = append(syms, c.Symbol)
	}
	news, err := s.Feed.NewsMulti(ctx, syms, s.Clock.Now().Add(-time.Duration(ic.NewsLookbackHours)*time.Hour), 5)
	if err != nil {
		s.note("warning: news unavailable, skipping pre-market review: %v", err)
		return nil
	}
	var items []llm.PremarketItem
	for _, sym := range market.SortedKeys(news) {
		c, ok := byName[sym]
		if !ok || len(news[sym]) == 0 {
			continue
		}
		items = append(items, llm.PremarketItem{Symbol: sym, PrevClose: c.PrevClose, ATRPct: math.Round(c.ATR/c.PrevClose*1000) / 10, News: news[sym]})
	}
	if len(items) == 0 {
		return nil
	}
	fallback := 0.0
	if s.Cfg.LLM.OnError == "allow" {
		fallback = 1
	}
	verdicts, err := s.Reviewer.Premarket(ctx, s.today.Format("2006-01-02"), items)
	out := map[string]float64{}
	if err != nil {
		s.note("pre-market review failed (%v): %d symbols with news get multiplier %.0f", err, len(items), fallback)
		_ = s.Journal.Log("llm_error", map[string]any{"stage": "premarket", "error": err.Error()})
		for _, it := range items {
			out[it.Symbol] = fallback
		}
		return out
	}
	_ = s.Journal.Log("premarket_verdicts", verdicts)
	for _, it := range items {
		v, ok := verdicts[it.Symbol]
		if !ok {
			out[it.Symbol] = fallback
			continue
		}
		out[it.Symbol] = v.Multiplier()
		if v.Decision != "approve" {
			s.note("Claude %s %s ×%.2f %v: %s", v.Decision, it.Symbol, v.Multiplier(), v.RiskFlags, v.Rationale)
		}
	}
	return out
}

func (s *session) held() map[string]float64 {
	h := map[string]float64{}
	for sym, q := range s.leftovers {
		h[sym] += q
	}
	for _, sym := range s.eng.OpenPositions() {
		if q, _, _ := s.eng.Position(sym); q > 0 {
			h[sym] += q
		}
	}
	return h
}

func (s *session) orderID(sym, side string, now time.Time) string {
	s.seq++
	return fmt.Sprintf("tb-%s-%s-%s-%s-%d", s.today.Format("20060102"), now.In(market.NewYork).Format("150405"), sym, side, s.seq)
}

// execute turns an engine action into a risk-checked limit order.
func (s *session) execute(ctx context.Context, a Action, now time.Time) {
	ic := s.Cfg.Intraday
	px := s.last[a.Symbol]
	if px <= 0 {
		px = a.Price
	}
	o := portfolio.Order{Symbol: a.Symbol, Qty: a.Qty, Reason: a.Reason}
	cash := s.eng.BuyingPower()
	if a.Kind == ActEntry {
		o.Side, o.Kind, o.RefPrice = portfolio.Buy, portfolio.KindEntry, math.Max(px, a.Price)
		o.LimitPrice = market.RoundTick(a.Price * (1 + ic.EntryLimitBps/1e4))
		cash += o.Qty * o.LimitPrice // the engine already reserved this entry
	} else {
		o.Side, o.Kind, o.RefPrice = portfolio.Sell, portfolio.KindExit, px
		o.LimitPrice = market.RoundTick(px * (1 - ic.ExitLimitBps/1e4))
	}
	o.ID = s.orderID(a.Symbol, o.Side, now)
	fail := func(why string) {
		if s.lastFail[a.Symbol] != why {
			s.lastFail[a.Symbol] = why
			s.note("%s %s %g %s NOT sent: %s", strings.ToUpper(a.Kind), o.Side, o.Qty, o.Symbol, why)
		}
		if a.Kind == ActEntry {
			s.eng.OnEntryFailed(a.Symbol)
		} else {
			s.eng.OnExitFailed(a.Symbol)
		}
	}
	_, rejected := s.rk.Check([]portfolio.Order{o}, risk.Snapshot{
		Now: now, LastBarDate: now, Equity: s.equity0 + s.eng.PnL(s.last), Cash: cash, Held: s.held(), Prices: s.pricesWith(o),
	})
	if len(rejected) > 0 {
		fail("risk: " + rejected[0].Reason)
		return
	}
	res, err := s.Broker.Submit(ctx, []portfolio.Order{o})
	if err != nil || len(res) != 1 || res[0].Status != "submitted" || res[0].BrokerOrderID == "" {
		why := fmt.Sprint(err)
		if len(res) == 1 {
			why = res[0].Status + " " + res[0].Message
		}
		fail(why)
		return
	}
	s.pending[res[0].BrokerOrderID] = &tracked{act: a, order: o, submitted: now}
	_ = s.Journal.Log("intraday_order", map[string]any{"action": a, "order": o, "broker_order_id": res[0].BrokerOrderID})
	s.note("%s %s %g %s @ %.2f (%s)", strings.ToUpper(a.Kind), o.Side, o.Qty, o.Symbol, o.LimitPrice, a.Reason)
}

func (s *session) pricesWith(o portfolio.Order) map[string]float64 {
	p := map[string]float64{}
	for k, v := range s.last {
		p[k] = v
	}
	if p[o.Symbol] <= 0 {
		p[o.Symbol] = o.RefPrice
	}
	return p
}

// exitLeftovers sells positions carried over from a previous session.
func (s *session) exitLeftovers(ctx context.Context, now time.Time) {
	for _, sym := range market.SortedKeys(s.leftovers) {
		busy := false
		for _, tr := range s.pending {
			if tr.leftover && tr.order.Symbol == sym {
				busy = true
			}
		}
		px := s.last[sym]
		if busy || px <= 0 {
			continue
		}
		o := portfolio.Order{ID: s.orderID(sym, portfolio.Sell, now), Symbol: sym, Side: portfolio.Sell, Qty: s.leftovers[sym],
			RefPrice: px, LimitPrice: market.RoundTick(px * (1 - s.Cfg.Intraday.ExitLimitBps/1e4)), Kind: portfolio.KindExit, Reason: "leftover from a previous session"}
		res, err := s.Broker.Submit(ctx, []portfolio.Order{o})
		if err != nil || len(res) != 1 || res[0].BrokerOrderID == "" {
			s.note("leftover exit for %s failed: %v", sym, err)
			continue
		}
		s.pending[res[0].BrokerOrderID] = &tracked{order: o, leftover: true, submitted: now}
		s.note("EXIT leftover sell %g %s @ %.2f", o.Qty, sym, o.LimitPrice)
	}
}

// poll checks pending orders, applies fills and cancels stale orders.
func (s *session) poll(ctx context.Context, now time.Time) []Action {
	timeout := time.Duration(s.Cfg.Intraday.OrderTimeoutSeconds * float64(time.Second))
	var more []Action
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	filled := false
	defer func() {
		if filled {
			s.checkpoint()
		}
	}()
	for _, id := range ids {
		tr := s.pending[id]
		st, err := s.Broker.OrderStatus(ctx, id)
		if err != nil {
			s.note("warning: status of %s: %v", id, err)
			continue
		}
		if !st.Done() {
			if !tr.canceled && now.Sub(tr.submitted) > timeout {
				if err := s.Broker.Cancel(ctx, id); err != nil {
					s.note("warning: cancel %s: %v", id, err)
				}
				tr.canceled = true
			}
			continue
		}
		delete(s.pending, id)
		filled = true
		sym := tr.order.Symbol
		_ = s.Journal.Log("intraday_fill", map[string]any{"broker_order_id": id, "order": tr.order, "status": st})
		switch {
		case tr.leftover:
			s.leftovers[sym] -= st.FilledQty
			if s.leftovers[sym] <= 1e-9 {
				delete(s.leftovers, sym)
			}
		case tr.act.Kind == ActEntry && st.FilledQty > 0:
			s.eng.OnEntryFill(sym, st.FilledQty, st.FilledPrice)
			s.dayTrades++
			_, _, stop := s.eng.Position(sym)
			s.note("FILLED buy %g %s @ %.2f, stop %.2f", st.FilledQty, sym, st.FilledPrice, stop)
		case tr.act.Kind == ActEntry:
			s.eng.OnEntryFailed(sym)
			s.note("entry %s not filled (%s)", sym, st.Status)
		case st.FilledQty > 0:
			_, entry, _ := s.eng.Position(sym)
			more = append(more, s.eng.OnExitFill(sym, st.FilledQty, st.FilledPrice, now, s.last)...)
			s.note("FILLED sell %g %s @ %.2f (%s), P&L %.2f", st.FilledQty, sym, st.FilledPrice, tr.act.Reason, (st.FilledPrice-entry)*st.FilledQty)
		default:
			s.eng.OnExitFailed(sym)
			s.note("exit %s not filled (%s); retrying", sym, st.Status)
		}
	}
	return more
}

// checkpoint persists open positions and today's entry count after every
// fill, so a restarted process knows what to flatten.
func (s *session) checkpoint() {
	s.st.OpenAtClose = s.held()
	s.st.DayTrades[s.today.Format("2006-01-02")] = s.baseTrades + s.dayTrades
	if err := SaveLiveState(s.StateDir, s.st); err != nil {
		s.note("warning: checkpoint failed: %v", err)
	}
}

func (s *session) wrapUp(ctx context.Context, st *LiveState, completed bool) error {
	for id, tr := range s.pending {
		if tr.act.Kind == ActEntry {
			_ = s.Broker.Cancel(ctx, id)
		}
	}
	var open []string
	for sym, q := range s.held() {
		open = append(open, fmt.Sprintf("%s %g", sym, q))
	}
	sort.Strings(open)
	st.DayTrades[s.today.Format("2006-01-02")] = s.baseTrades + s.dayTrades
	st.OpenAtClose = s.held()
	st.Risk = s.rk.St
	if completed {
		st.LastRunDate = s.today
	}
	if err := SaveLiveState(s.StateDir, st); err != nil {
		return err
	}
	summary := fmt.Sprintf("tradebot intraday %s | %s | entries filled %d | realized P&L %.2f (%.2f%%)",
		s.Broker.Name(), s.today.Format("2006-01-02"), s.dayTrades, s.eng.Realized(), s.eng.Realized()/s.equity0*100)
	if h, why := s.eng.Halted(); h {
		summary += " | halted: " + why
	}
	if len(open) > 0 {
		summary += " | ⚠ STILL OPEN: " + strings.Join(open, ", ")
	}
	s.logf("%s", summary)
	_ = s.Journal.Log("intraday_summary", map[string]any{"summary": summary, "events": s.lines})
	if err := s.Notify.Send(ctx, summary); err != nil {
		s.logf("warning: notification failed: %v", err)
	}
	if len(open) > 0 {
		return fmt.Errorf("positions still open at the end of the session: %s", strings.Join(open, ", "))
	}
	return nil
}
