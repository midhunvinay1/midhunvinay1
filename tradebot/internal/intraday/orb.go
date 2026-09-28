// Package intraday implements the short-term (day-trading) mode: a long-only
// opening-range breakout (ORB) on "stocks in play", flat by the close.
//
// Rules (after Zarattini & Aziz, 2023, adapted to long-only, no leverage):
//   - Opening range (OR) = the first N minutes (default 5) after 09:30 ET.
//   - Stocks in play: OR volume / average OR volume of the prior 14 sessions
//     (relative volume, RVOL) >= min_rvol, a bullish OR candle, price and
//     daily-ATR filters. Keep the top-N by RVOL.
//   - Entry: price trades above the OR high (buy-stop logic).
//   - Stop: entry - 10% of the 14-day daily ATR. Size so the stop costs
//     risk_per_trade of equity, capped by max_position_weight and buying power.
//   - Exit: stop, optional target / breakeven, or flatten at 15:55 ET.
//
// The Engine is a pure state machine: the backtester feeds it minute bars,
// the live loop feeds it polled prices, and both apply the same rules.
package intraday

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
)

const (
	ActEntry = "entry"
	ActExit  = "exit"

	ReasonStop      = "stop"
	ReasonTarget    = "target"
	ReasonFlatten   = "flatten"
	ReasonHalt      = "halt"
	ReasonLossLimit = "daily-loss-limit"
)

// SymbolContext is computed before the open from PRIOR sessions only.
type SymbolContext struct {
	Symbol      string  `json:"symbol"`
	ATR         float64 `json:"atr"`           // daily ATR (dollars)
	AvgORVolume float64 `json:"avg_or_volume"` // mean opening-range volume
	PrevClose   float64 `json:"prev_close"`
	SizeMult    float64 `json:"size_mult"` // Claude pre-market multiplier in [0,1]
}

type OpeningRange struct {
	Open, High, Low, Close, Volume float64
	Bars                           int
}

// Action is an order the engine wants. Price is the modeled trigger level
// (entry trigger, stop level, target level) or the last price for flattening.
type Action struct {
	Kind   string    `json:"kind"`
	Symbol string    `json:"symbol"`
	Qty    float64   `json:"qty"`
	Price  float64   `json:"price"`
	Stop   float64   `json:"stop,omitempty"`
	Reason string    `json:"reason"`
	Time   time.Time `json:"time"`
}

type symState struct {
	ctx          SymbolContext
	or           OpeningRange
	rvol         float64
	inPlay       bool
	traded       bool // one entry attempt per symbol per day
	qty          float64
	entry        float64
	stop         float64
	riskPerShare float64
	reserved     float64 // buying power held for a pending entry
	pendingEntry bool
	pendingExit  bool
}

// Engine holds one session's state.
type Engine struct {
	cfg          config.IntradayConfig
	syms         map[string]*symState
	orStart      int // minutes after midnight NY
	orEnd        int
	noEntryAfter int
	flattenAt    int
	equity0      float64
	buyingPower  float64
	realized     float64
	trades       int
	dayTrades5D  int // day trades in the prior 4 sessions
	orDone       bool
	halted       bool
	haltReason   string
	InPlay       []string
}

// NewEngine starts a session. buyingPower is settled cash for a cash account.
func NewEngine(cfg config.IntradayConfig, ctxs []SymbolContext, equity, buyingPower float64, priorDayTrades int) *Engine {
	na, _ := config.ParseClock(cfg.NoEntriesAfter)
	fl, _ := config.ParseClock(cfg.FlattenAt)
	e := &Engine{
		cfg: cfg, syms: map[string]*symState{}, orStart: 9*60 + 30, noEntryAfter: na, flattenAt: fl,
		equity0: equity, buyingPower: math.Min(buyingPower, equity), dayTrades5D: priorDayTrades,
	}
	e.orEnd = e.orStart + cfg.OpeningRangeMinutes
	for _, c := range ctxs {
		if c.SizeMult == 0 && c.Symbol != "" {
			c.SizeMult = 1
		}
		e.syms[c.Symbol] = &symState{ctx: c}
	}
	return e
}

// MinuteOfDay returns minutes after midnight in New York.
func MinuteOfDay(t time.Time) int {
	n := t.In(market.NewYork)
	return n.Hour()*60 + n.Minute()
}

// InOpeningRange reports whether a bar starting at t belongs to the OR window.
func (e *Engine) InOpeningRange(t time.Time) bool {
	m := MinuteOfDay(t)
	return m >= e.orStart && m < e.orEnd
}

// ORDone reports whether the opening range has been finalized.
func (e *Engine) ORDone() bool { return e.orDone }

// ORClose is when the opening range is complete (as NY minutes).
func (e *Engine) ORClose() int { return e.orEnd }

// FlattenMinute is when all positions are closed (as NY minutes).
func (e *Engine) FlattenMinute() int { return e.flattenAt }

// SetSizeMult applies a pre-market review multiplier (clamped to [0,1]).
func (e *Engine) SetSizeMult(sym string, m float64) {
	if st, ok := e.syms[sym]; ok {
		st.ctx.SizeMult = math.Max(0, math.Min(1, m))
	}
}

// AddORBar feeds one minute bar from the opening-range window.
func (e *Engine) AddORBar(sym string, b market.Bar) {
	st, ok := e.syms[sym]
	if !ok || e.orDone || !e.InOpeningRange(b.Date) {
		return
	}
	o := &st.or
	if o.Bars == 0 {
		o.Open, o.High, o.Low = b.Open, b.High, b.Low
	}
	o.High = math.Max(o.High, b.High)
	o.Low = math.Min(o.Low, b.Low)
	o.Close = b.Close
	o.Volume += b.Volume
	o.Bars++
}

// FinalizeOpeningRange selects the stocks in play. Call once, after the OR window.
func (e *Engine) FinalizeOpeningRange() []string {
	if e.orDone {
		return e.InPlay
	}
	e.orDone = true
	type cand struct {
		sym  string
		rvol float64
	}
	var cands []cand
	for sym, st := range e.syms {
		c, o := st.ctx, st.or
		if o.Bars == 0 || c.AvgORVolume <= 0 || c.ATR <= 0 || c.SizeMult <= 0 {
			continue
		}
		st.rvol = o.Volume / c.AvgORVolume
		if st.rvol < e.cfg.MinRVOL || o.Close <= o.Open || o.Close < e.cfg.MinPrice || c.ATR < e.cfg.MinATRDollars {
			continue
		}
		cands = append(cands, cand{sym, st.rvol})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rvol != cands[j].rvol {
			return cands[i].rvol > cands[j].rvol
		}
		return cands[i].sym < cands[j].sym
	})
	if len(cands) > e.cfg.TopNInPlay {
		cands = cands[:e.cfg.TopNInPlay]
	}
	e.InPlay = e.InPlay[:0]
	for _, c := range cands {
		e.syms[c.sym].inPlay = true
		e.InPlay = append(e.InPlay, c.sym)
	}
	return e.InPlay
}

// OpenPositions returns symbols with a position or a pending order.
func (e *Engine) OpenPositions() []string {
	var out []string
	for sym, st := range e.syms {
		if st.qty > 0 || st.pendingEntry || st.pendingExit {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// Watchlist is every symbol the live loop needs prices for.
func (e *Engine) Watchlist() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, e.InPlay...), e.OpenPositions()...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) openCount() int {
	n := 0
	for _, st := range e.syms {
		if st.qty > 0 || st.pendingEntry {
			n++
		}
	}
	return n
}

// Halted reports whether new entries are blocked for the rest of the day.
func (e *Engine) Halted() (bool, string) { return e.halted, e.haltReason }

// Halt blocks new entries and returns exits for every open position.
func (e *Engine) Halt(reason string, t time.Time, prices map[string]float64) []Action {
	if !e.halted {
		e.halted, e.haltReason = true, reason
	}
	return e.exitAll(ReasonHalt, t, prices)
}

func (e *Engine) exitAll(reason string, t time.Time, prices map[string]float64) []Action {
	var acts []Action
	for _, sym := range market.SortedKeys(e.syms) {
		st := e.syms[sym]
		if st.qty > 0 && !st.pendingExit {
			st.pendingExit = true
			px := prices[sym]
			if px <= 0 {
				px = st.entry
			}
			acts = append(acts, Action{Kind: ActExit, Symbol: sym, Qty: st.qty, Price: px, Reason: reason, Time: t})
		}
	}
	return acts
}

// OnPrice processes the price range [low, high] observed for sym up to time t
// (a minute bar in backtests, the latest trade live) and returns actions.
// Exits are evaluated before entries.
func (e *Engine) OnPrice(sym string, high, low float64, t time.Time) []Action {
	st, ok := e.syms[sym]
	if !ok || !e.orDone || high <= 0 || low <= 0 {
		return nil
	}
	m := MinuteOfDay(t)
	var acts []Action

	if st.qty > 0 && !st.pendingExit {
		target := 0.0
		if e.cfg.TakeProfitR > 0 {
			target = st.entry + e.cfg.TakeProfitR*st.riskPerShare
		}
		switch {
		case low <= st.stop:
			st.pendingExit = true
			acts = append(acts, Action{Kind: ActExit, Symbol: sym, Qty: st.qty, Price: st.stop, Reason: ReasonStop, Time: t})
		case target > 0 && high >= target:
			st.pendingExit = true
			acts = append(acts, Action{Kind: ActExit, Symbol: sym, Qty: st.qty, Price: target, Reason: ReasonTarget, Time: t})
		case m >= e.flattenAt:
			st.pendingExit = true
			acts = append(acts, Action{Kind: ActExit, Symbol: sym, Qty: st.qty, Price: low, Reason: ReasonFlatten, Time: t})
		case e.cfg.BreakevenAtR > 0 && high >= st.entry+e.cfg.BreakevenAtR*st.riskPerShare:
			st.stop = math.Max(st.stop, st.entry)
		}
		return acts
	}

	if !st.inPlay || st.traded || st.qty > 0 || st.pendingEntry || e.halted || m >= e.noEntryAfter || m >= e.flattenAt {
		return acts
	}
	trigger := market.RoundTick(st.or.High + 0.01)
	if high < trigger {
		return acts
	}
	st.traded = true // no second attempt today, whatever happens next
	switch {
	case e.openCount() >= e.cfg.MaxPositions,
		e.cfg.MaxTradesPerDay > 0 && e.trades >= e.cfg.MaxTradesPerDay,
		e.cfg.DayTradeLimit5D > 0 && e.dayTrades5D+e.trades >= e.cfg.DayTradeLimit5D:
		return acts
	}
	stopDist := e.cfg.StopATRFraction * st.ctx.ATR
	if stopDist <= 0 {
		return acts
	}
	limitPx := trigger * (1 + e.cfg.EntryLimitBps/1e4)
	qty := math.Floor(math.Min(
		e.cfg.RiskPerTrade*e.equity0/stopDist,
		math.Min(e.cfg.MaxPositionWeight*e.equity0/trigger, e.buyingPower/limitPx),
	) * st.ctx.SizeMult)
	if qty < 1 {
		return acts
	}
	st.pendingEntry = true
	st.reserved = qty * limitPx
	e.buyingPower -= st.reserved
	e.trades++
	return append(acts, Action{Kind: ActEntry, Symbol: sym, Qty: qty, Price: trigger, Stop: market.RoundTick(trigger - stopDist), Reason: fmt.Sprintf("ORB breakout above %.2f (RVOL %.1f)", st.or.High, st.rvol), Time: t})
}

// OnEntryFill records a (possibly partial) entry fill. The stop is re-anchored
// to the actual fill price so slippage does not widen the risk.
func (e *Engine) OnEntryFill(sym string, qty, price float64) {
	st := e.syms[sym]
	if st == nil || qty <= 0 {
		e.OnEntryFailed(sym)
		return
	}
	stopDist := e.cfg.StopATRFraction * st.ctx.ATR
	e.buyingPower += st.reserved - qty*price
	st.reserved, st.pendingEntry = 0, false
	st.qty, st.entry = qty, price
	st.riskPerShare = stopDist
	st.stop = market.RoundTick(price - stopDist)
}

// OnEntryFailed releases an entry that did not fill (it is not retried).
func (e *Engine) OnEntryFailed(sym string) {
	st := e.syms[sym]
	if st == nil || !st.pendingEntry {
		return
	}
	e.buyingPower += st.reserved
	st.reserved, st.pendingEntry = 0, false
	e.trades--
}

// OnExitFill records an exit fill and applies the daily loss limit.
// It returns exits for the remaining positions if the loss limit is breached.
func (e *Engine) OnExitFill(sym string, qty, price float64, t time.Time, prices map[string]float64) []Action {
	st := e.syms[sym]
	if st == nil {
		return nil
	}
	qty = math.Min(qty, st.qty)
	e.realized += (price - st.entry) * qty
	st.qty -= qty
	st.pendingExit = false
	if e.cfg.AccountType == "margin" {
		e.buyingPower += qty * price // cash accounts wait for settlement
	}
	if st.qty <= 1e-9 {
		st.qty = 0
	}
	if !e.halted && e.cfg.DailyLossLimit > 0 && e.PnL(prices) <= -e.cfg.DailyLossLimit*e.equity0 {
		return e.Halt(ReasonLossLimit, t, prices)
	}
	return nil
}

// OnExitFailed lets the next price update re-issue the exit.
func (e *Engine) OnExitFailed(sym string) {
	if st := e.syms[sym]; st != nil {
		st.pendingExit = false
	}
}

// PnL is realized plus unrealized P&L at the given prices.
func (e *Engine) PnL(prices map[string]float64) float64 {
	pnl := e.realized
	for _, sym := range market.SortedKeys(e.syms) {
		st := e.syms[sym]
		if st.qty > 0 {
			if px := prices[sym]; px > 0 {
				pnl += (px - st.entry) * st.qty
			}
		}
	}
	return pnl
}

// CheckLossLimit halts the session if P&L (incl. unrealized) breaches the limit.
func (e *Engine) CheckLossLimit(t time.Time, prices map[string]float64) []Action {
	if e.halted || e.cfg.DailyLossLimit <= 0 || e.PnL(prices) > -e.cfg.DailyLossLimit*e.equity0 {
		return nil
	}
	return e.Halt(ReasonLossLimit, t, prices)
}

// FlattenAll exits everything (end of day).
func (e *Engine) FlattenAll(t time.Time, prices map[string]float64) []Action {
	return e.exitAll(ReasonFlatten, t, prices)
}

// BuyingPower is what is left for new entries (after reservations).
func (e *Engine) BuyingPower() float64 { return e.buyingPower }

// Realized returns the realized P&L so far.
func (e *Engine) Realized() float64 { return e.realized }

// Trades returns the number of entries attempted (filled or pending).
func (e *Engine) Trades() int { return e.trades }

// Position returns the open quantity and entry price for sym.
func (e *Engine) Position(sym string) (qty, entry, stop float64) {
	if st := e.syms[sym]; st != nil {
		return st.qty, st.entry, st.stop
	}
	return 0, 0, 0
}

// RVOL returns the relative opening volume for sym (after the OR is finalized).
func (e *Engine) RVOL(sym string) float64 {
	if st := e.syms[sym]; st != nil {
		return st.rvol
	}
	return 0
}
