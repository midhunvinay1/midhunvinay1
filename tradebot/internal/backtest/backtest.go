// Package backtest replays history through the exact same strategy,
// portfolio and risk code used live. Decisions are made at each close and
// filled at the next session's open, as limit orders with slippage.
package backtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
	"github.com/midhunvinay1/tradebot/internal/risk"
	"github.com/midhunvinay1/tradebot/internal/strategy"
)

type EquityPoint struct {
	Date      time.Time `json:"date"`
	Equity    float64   `json:"equity"`
	Cash      float64   `json:"cash"`
	Gross     float64   `json:"gross"`
	Benchmark float64   `json:"benchmark"`
}

type Fill struct {
	Date   time.Time `json:"date"`
	Symbol string    `json:"symbol"`
	Side   string    `json:"side"`
	Qty    float64   `json:"qty"`
	Price  float64   `json:"price"`
	Kind   string    `json:"kind"`
	Reason string    `json:"reason"`
}

// Trade is one round trip: from flat to flat in a symbol.
type Trade struct {
	Symbol    string    `json:"symbol"`
	EntryDate time.Time `json:"entry_date"`
	ExitDate  time.Time `json:"exit_date"`
	Cost      float64   `json:"cost"`
	PnL       float64   `json:"pnl"`
	PnLPct    float64   `json:"pnl_pct"`
	HoldDays  int       `json:"hold_days"`
}

type Result struct {
	Equity     []EquityPoint `json:"-"`
	Fills      []Fill        `json:"-"`
	Trades     []Trade       `json:"-"`
	Metrics    Metrics       `json:"metrics"`
	Benchmark  Metrics       `json:"benchmark"`
	Yearly     []YearRow     `json:"yearly"`
	Rejections int           `json:"rejections"`
	Unfilled   int           `json:"unfilled"`
	Vetoed     int           `json:"vetoed"`
	HaltedOn   *time.Time    `json:"halted_on,omitempty"`
}

// Reviewer optionally filters new entries (the Claude overlay). It returns a
// size multiplier in [0,1]; 0 vetoes the entry.
type Reviewer func(ctx context.Context, date time.Time, o portfolio.Order, plan *strategy.Plan, view market.Dataset) float64

type Options struct {
	Start, End time.Time // zero = full range
	Reviewer   Reviewer  // nil = no overlay (the default: LLMs cannot be backtested honestly, see docs)
}

type lot struct {
	qty, cost, realized, bought float64
	entry                       time.Time
}

// Planner is any strategy the backtester (and the live runner) can drive:
// it sees data truncated at the decision date and returns target weights.
type Planner interface {
	Reconcile(held map[string]float64)
	MarkRejected(sym string)
	Plan(data market.Dataset) (*strategy.Plan, error)
}

// Run backtests the swing strategy.
func Run(ctx context.Context, cfg *config.Config, data market.Dataset, opt Options) (*Result, error) {
	return RunWith(ctx, cfg, data, strategy.New(cfg, nil), opt)
}

// RunWith backtests any Planner with the same fills, risk engine and metrics.
func RunWith(ctx context.Context, cfg *config.Config, data market.Dataset, strat Planner, opt Options) (*Result, error) {
	bench, ok := data[cfg.Benchmark]
	if !ok || len(bench) == 0 {
		return nil, fmt.Errorf("benchmark %s has no data", cfg.Benchmark)
	}
	var dates []time.Time
	for _, b := range bench {
		if (opt.Start.IsZero() || !b.Date.Before(opt.Start)) && (opt.End.IsZero() || !b.Date.After(opt.End)) {
			dates = append(dates, b.Date)
		}
	}
	if len(dates) < 2 {
		return nil, fmt.Errorf("not enough benchmark bars in range")
	}

	rk := risk.New(cfg, nil, "")
	managed := map[string]bool{}
	for _, s := range cfg.Symbols() {
		managed[s] = true
	}
	slip := cfg.Backtest.SlippageBps / 1e4
	cash := cfg.Backtest.InitialEquity
	held := map[string]float64{}
	lots := map[string]*lot{}
	lastClose := map[string]float64{}
	res := &Result{}
	var pending []portfolio.Order
	var benchShares float64
	started := false

	for _, d := range dates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 1. Fill yesterday's orders at today's open.
		for _, o := range pending {
			bar, ok := data[o.Symbol].At(d)
			if !ok {
				res.Unfilled++
				continue
			}
			price, filled := FillPrice(o, bar, slip)
			if !filled {
				res.Unfilled++
				continue
			}
			qty := o.Qty
			if o.Side == portfolio.Buy {
				if qty*price > cash {
					qty = portfolio.RoundQty(cash/price, cfg.Execution.AllowFractional)
				}
				if qty <= 0 {
					res.Unfilled++
					continue
				}
				cash -= qty * price
				held[o.Symbol] += qty
				l := lots[o.Symbol]
				if l == nil || l.qty == 0 {
					l = &lot{entry: d}
					lots[o.Symbol] = l
				}
				l.qty += qty
				l.cost += qty * price
				l.bought += qty * price
			} else {
				qty = math.Min(qty, held[o.Symbol])
				if qty <= 0 {
					continue
				}
				cash += qty * price
				held[o.Symbol] -= qty
				if l := lots[o.Symbol]; l != nil && l.qty > 0 {
					avg := l.cost / l.qty
					l.realized += (price - avg) * qty
					l.cost -= avg * qty
					l.qty -= qty
					if l.qty <= 1e-9 {
						res.Trades = append(res.Trades, closeTrade(o.Symbol, l, d))
						delete(lots, o.Symbol)
						delete(held, o.Symbol)
					}
				}
			}
			res.Fills = append(res.Fills, Fill{Date: d, Symbol: o.Symbol, Side: o.Side, Qty: qty, Price: price, Kind: o.Kind, Reason: o.Reason})
		}
		pending = nil

		// 2. Mark to market at the close.
		gross := 0.0
		for sym := range data {
			if b, ok := data[sym].At(d); ok {
				lastClose[sym] = b.Close
			}
		}
		for _, sym := range market.SortedKeys(held) {
			gross += held[sym] * lastClose[sym]
		}
		equity := cash + gross
		if !started {
			benchShares = cfg.Backtest.InitialEquity / lastClose[cfg.Benchmark]
			started = true
		}
		res.Equity = append(res.Equity, EquityPoint{Date: d, Equity: equity, Cash: cash, Gross: gross, Benchmark: benchShares * lastClose[cfg.Benchmark]})

		// 3. Decide at the close.
		view := data.Until(d)
		strat.Reconcile(held)
		rk.Update(equity, d)
		if h, _ := rk.Halted(); h && res.HaltedOn == nil {
			hd := d
			res.HaltedOn = &hd
		}
		plan, err := strat.Plan(view)
		if errors.Is(err, strategy.ErrWarmup) {
			continue
		} else if err != nil {
			return nil, err
		}
		scale := rk.ExposureScale(equity)
		targets := map[string]float64{}
		for sym, w := range plan.Weights {
			targets[sym] = w * scale
		}
		orders := portfolio.Diff(portfolio.Input{
			Date: d, Targets: targets, Held: held, Prices: lastClose, Equity: equity, Managed: managed, Reasons: plan.OrderReasons(),
		}, cfg.Execution)
		if opt.Reviewer != nil {
			kept := orders[:0]
			for _, o := range orders {
				if o.Kind == portfolio.KindEntry && o.Side == portfolio.Buy {
					m := math.Max(0, math.Min(1, opt.Reviewer(ctx, d, o, plan, view)))
					o.Qty = portfolio.RoundQty(o.Qty*m, cfg.Execution.AllowFractional)
					if o.Qty <= 0 {
						res.Vetoed++
						strat.MarkRejected(o.Symbol)
						continue
					}
				}
				kept = append(kept, o)
			}
			orders = kept
		}
		approved, rejected := rk.Check(orders, risk.Snapshot{
			Now: d, LastBarDate: d, Equity: equity, Cash: cash, Held: held, Prices: lastClose,
		})
		for _, r := range rejected {
			if r.Order.Kind == portfolio.KindEntry {
				strat.MarkRejected(r.Order.Symbol)
			}
		}
		res.Rejections += len(rejected)
		pending = approved
	}

	// Close open lots at the final mark for trade statistics only.
	last := dates[len(dates)-1]
	for sym, l := range lots {
		if l.qty > 0 {
			l.realized += (lastClose[sym] - l.cost/l.qty) * l.qty
			l.cost = 0
			res.Trades = append(res.Trades, closeTrade(sym, l, last))
		}
	}
	sort.Slice(res.Trades, func(i, j int) bool { return res.Trades[i].ExitDate.Before(res.Trades[j].ExitDate) })
	res.Metrics = ComputeMetrics(res.Equity, func(p EquityPoint) float64 { return p.Equity }, cfg.Backtest.RiskFreeRate)
	res.Metrics.AddTrades(res.Trades)
	res.Metrics.AddExposure(res.Equity, res.Fills)
	res.Benchmark = ComputeMetrics(res.Equity, func(p EquityPoint) float64 { return p.Benchmark }, cfg.Backtest.RiskFreeRate)
	res.Yearly = Yearly(res.Equity)
	return res, nil
}

func closeTrade(sym string, l *lot, exit time.Time) Trade {
	t := Trade{Symbol: sym, EntryDate: l.entry, ExitDate: exit, Cost: l.bought, PnL: l.realized,
		HoldDays: int(exit.Sub(l.entry).Hours() / 24)}
	if l.bought > 0 {
		t.PnLPct = l.realized / l.bought
	}
	return t
}

// FillPrice simulates a day limit order against the next session's bar.
// A buy fills at the open (plus slippage, capped at the limit) if the open is
// at or below the limit, or at the limit if the low trades through it.
// A sell is symmetric. Otherwise the order expires unfilled.
func FillPrice(o portfolio.Order, bar market.Bar, slip float64) (float64, bool) {
	if o.Side == portfolio.Buy {
		switch {
		case bar.Open <= o.LimitPrice:
			return math.Min(bar.Open*(1+slip), o.LimitPrice), true
		case bar.Low <= o.LimitPrice:
			return o.LimitPrice, true
		}
		return 0, false
	}
	switch {
	case bar.Open >= o.LimitPrice:
		return math.Max(bar.Open*(1-slip), o.LimitPrice), true
	case bar.High >= o.LimitPrice:
		return o.LimitPrice, true
	}
	return 0, false
}
