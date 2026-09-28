package intraday

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/backtest"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
)

// Trade is one completed intraday round trip.
type Trade struct {
	Symbol    string    `json:"symbol"`
	EntryTime time.Time `json:"entry_time"`
	ExitTime  time.Time `json:"exit_time"`
	Qty       float64   `json:"qty"`
	Entry     float64   `json:"entry"`
	Exit      float64   `json:"exit"`
	PnL       float64   `json:"pnl"`
	R         float64   `json:"r"` // P&L in units of initial risk
	Reason    string    `json:"reason"`
}

type Result struct {
	Equity         []backtest.EquityPoint `json:"-"`
	Trades         []Trade                `json:"-"`
	Metrics        backtest.Metrics       `json:"metrics"`
	Benchmark      backtest.Metrics       `json:"benchmark"`
	Yearly         []backtest.YearRow     `json:"yearly"`
	Days           int                    `json:"days"`
	DaysTraded     int                    `json:"days_traded"`
	TradesPerDay   float64                `json:"trades_per_day"`
	AvgR           float64                `json:"avg_r"`
	AvgHoldMinutes float64                `json:"avg_hold_minutes"`
	ExitReasons    map[string]int         `json:"exit_reasons"`
	Unfilled       int                    `json:"unfilled_entries"`
	HaltDays       int                    `json:"loss_limit_days"`
}

type BacktestOptions struct {
	Start, End    time.Time
	InitialEquity float64
	Benchmark     string // symbol for buy-and-hold comparison ("" = none)
}

type openTrade struct {
	t           time.Time
	qty, px, rp float64
}

type event struct {
	t   time.Time
	sym string
	bar market.Bar
}

// Backtest simulates the ORB engine minute by minute. Entries fill at
// max(open, trigger) if that is within the entry limit, stops at
// min(open, stop), and everything is flat by the flatten time. Same-bar
// ambiguity is resolved pessimistically (a bar that triggers an entry and
// also trades through the stop is a loss).
func Backtest(ctx context.Context, cfg *config.Config, minutes market.Dataset, opt BacktestOptions) (*Result, error) {
	ic := cfg.Intraday
	slip := ic.SlippageBps / 1e4
	equity := opt.InitialEquity
	if equity <= 0 {
		return nil, fmt.Errorf("initial equity must be positive")
	}

	syms := market.SortedKeys(minutes)
	sessions := map[string][]Session{}
	hist := map[string]History{}
	idx := map[string]map[time.Time]int{}
	daySet := map[time.Time]bool{}
	for _, s := range syms {
		sess := SplitSessions(minutes[s])
		sessions[s] = sess
		hist[s] = HistoryFromSessions(sess, ic.OpeningRangeMinutes)
		idx[s] = map[time.Time]int{}
		for i, x := range sess {
			idx[s][x.Date] = i
			daySet[x.Date] = true
		}
	}
	var days []time.Time
	for d := range daySet {
		if (opt.Start.IsZero() || !d.Before(opt.Start)) && (opt.End.IsZero() || !d.After(opt.End)) {
			days = append(days, d)
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })
	if len(days) == 0 {
		return nil, fmt.Errorf("no sessions in range")
	}

	trading := map[string]bool{}
	for _, s := range cfg.IntradaySymbols() {
		trading[s] = true
	}
	res := &Result{ExitReasons: map[string]int{}}
	var dayTrades []int
	var benchShares float64

	for _, d := range days {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var ctxs []SymbolContext
		for _, s := range syms {
			if _, ok := idx[s][d]; !ok || !trading[s] {
				continue
			}
			if c, ok := hist[s].Context(s, d, ic); ok {
				ctxs = append(ctxs, c)
			}
		}
		prior := 0
		for i := max(0, len(dayTrades)-4); i < len(dayTrades); i++ {
			prior += dayTrades[i]
		}
		eng := NewEngine(ic, ctxs, equity, equity, prior)

		var evs []event
		for _, c := range ctxs {
			for _, b := range sessions[c.Symbol][idx[c.Symbol][d]].Bars {
				evs = append(evs, event{b.Date, c.Symbol, b})
			}
		}
		sort.Slice(evs, func(i, j int) bool {
			if !evs[i].t.Equal(evs[j].t) {
				return evs[i].t.Before(evs[j].t)
			}
			return evs[i].sym < evs[j].sym
		})

		open := map[string]*openTrade{}
		last := map[string]float64{}
		filledEntries := 0
		exitFill := func(a Action, px float64, t time.Time) []Action {
			ot := open[a.Symbol]
			more := eng.OnExitFill(a.Symbol, a.Qty, px, t, last)
			if ot != nil {
				tr := Trade{Symbol: a.Symbol, EntryTime: ot.t, ExitTime: t, Qty: a.Qty, Entry: ot.px, Exit: px,
					PnL: (px - ot.px) * a.Qty, Reason: a.Reason}
				if ot.rp > 0 {
					tr.R = (px - ot.px) / ot.rp
				}
				res.Trades = append(res.Trades, tr)
				res.ExitReasons[a.Reason]++
				delete(open, a.Symbol)
			}
			return more
		}
		// fillAtMark exits at the last known price (loss limit, halt, end of day).
		var fillAtMark func(acts []Action, t time.Time)
		fillAtMark = func(acts []Action, t time.Time) {
			for _, a := range acts {
				px := last[a.Symbol]
				if px <= 0 {
					px = a.Price
				}
				fillAtMark(exitFill(a, px*(1-slip), t), t)
			}
		}

		for i := 0; i < len(evs); {
			t := evs[i].t
			if !eng.ORDone() && MinuteOfDay(t) >= eng.ORClose() {
				eng.FinalizeOpeningRange()
			}
			j := i
			for ; j < len(evs) && evs[j].t.Equal(t); j++ {
				ev := evs[j]
				if eng.InOpeningRange(t) {
					eng.AddORBar(ev.sym, ev.bar)
					last[ev.sym] = ev.bar.Close
					continue
				}
				pending := eng.OnPrice(ev.sym, ev.bar.High, ev.bar.Low, t)
				for len(pending) > 0 {
					a := pending[0]
					pending = pending[1:]
					if a.Symbol != ev.sym { // e.g. loss-limit exits of other positions
						fillAtMark([]Action{a}, t)
						continue
					}
					if a.Kind == ActEntry {
						px := math.Max(ev.bar.Open, a.Price)
						if px > a.Price*(1+ic.EntryLimitBps/1e4) {
							eng.OnEntryFailed(a.Symbol) // gapped through the limit
							res.Unfilled++
							continue
						}
						px *= 1 + slip
						eng.OnEntryFill(a.Symbol, a.Qty, px)
						_, entry, stop := eng.Position(a.Symbol)
						open[a.Symbol] = &openTrade{t: t, qty: a.Qty, px: entry, rp: entry - stop}
						filledEntries++
						// Pessimistic same-bar check: only the stop, against this bar's low.
						pending = append(pending, eng.OnPrice(a.Symbol, px, ev.bar.Low, t)...)
						continue
					}
					var px float64
					switch a.Reason {
					case ReasonStop:
						px = math.Min(ev.bar.Open, a.Price) * (1 - slip)
					case ReasonTarget:
						px = math.Max(ev.bar.Open, a.Price) * (1 - slip)
					default:
						px = ev.bar.Open * (1 - slip)
					}
					pending = append(pending, exitFill(a, px, t)...)
				}
				last[ev.sym] = ev.bar.Close
			}
			fillAtMark(eng.CheckLossLimit(t, last), t)
			i = j
		}
		if h, _ := eng.Halted(); h {
			res.HaltDays++
		}
		endT := time.Date(d.Year(), d.Month(), d.Day(), 16, 0, 0, 0, market.NewYork)
		fillAtMark(eng.FlattenAll(endT, last), endT)

		equity += eng.Realized()
		dayTrades = append(dayTrades, filledEntries)
		if filledEntries > 0 {
			res.DaysTraded++
		}
		res.Days++
		pt := backtest.EquityPoint{Date: d, Equity: equity, Cash: equity}
		if b := opt.Benchmark; b != "" {
			if k, ok := idx[b][d]; ok {
				c := sessions[b][k].Bars[len(sessions[b][k].Bars)-1].Close
				if benchShares == 0 {
					benchShares = opt.InitialEquity / c
				}
				pt.Benchmark = benchShares * c
			}
		}
		res.Equity = append(res.Equity, pt)
	}

	res.Metrics = backtest.ComputeMetrics(res.Equity, func(p backtest.EquityPoint) float64 { return p.Equity }, cfg.Backtest.RiskFreeRate)
	var bts []backtest.Trade
	var sumR, hold float64
	for _, t := range res.Trades {
		bts = append(bts, backtest.Trade{Symbol: t.Symbol, EntryDate: t.EntryTime, ExitDate: t.ExitTime, Cost: t.Entry * t.Qty, PnL: t.PnL, PnLPct: t.PnL / (t.Entry * t.Qty)})
		sumR += t.R
		hold += t.ExitTime.Sub(t.EntryTime).Minutes()
	}
	res.Metrics.AddTrades(bts)
	res.Metrics.AvgHoldDays = 0
	if n := float64(len(res.Trades)); n > 0 {
		res.AvgR, res.AvgHoldMinutes = sumR/n, hold/n
	}
	if res.Days > 0 {
		res.TradesPerDay = float64(len(res.Trades)) / float64(res.Days)
	}
	if opt.Benchmark != "" && benchShares > 0 {
		var pts []backtest.EquityPoint
		for _, p := range res.Equity {
			if p.Benchmark > 0 {
				pts = append(pts, p)
			}
		}
		res.Benchmark = backtest.ComputeMetrics(pts, func(p backtest.EquityPoint) float64 { return p.Benchmark }, cfg.Backtest.RiskFreeRate)
	}
	res.Yearly = backtest.Yearly(res.Equity)
	return res, nil
}

// Report writes a human-readable summary.
func (r *Result) Report(w io.Writer) {
	m, b := r.Metrics, r.Benchmark
	pct := func(x float64) string { return fmt.Sprintf("%6.1f%%", x*100) }
	fmt.Fprintf(w, "Intraday ORB backtest %s → %s (%d sessions)\n", m.Start.Format("2006-01-02"), m.End.Format("2006-01-02"), r.Days)
	fmt.Fprintln(w, strings.Repeat("─", 52))
	fmt.Fprintf(w, "%-22s %12s %12s\n", "", "Strategy", "Buy & hold")
	row := func(n, a, c string) { fmt.Fprintf(w, "%-22s %12s %12s\n", n, a, c) }
	row("End equity", fmt.Sprintf("%.0f", m.EndEquity), fmt.Sprintf("%.0f", b.EndEquity))
	row("Total return", pct(m.TotalReturn), pct(b.TotalReturn))
	row("CAGR", pct(m.CAGR), pct(b.CAGR))
	row("Annual volatility", pct(m.AnnVol), pct(b.AnnVol))
	row("Sharpe", fmt.Sprintf("%.2f", m.Sharpe), fmt.Sprintf("%.2f", b.Sharpe))
	row("Max drawdown", pct(m.MaxDrawdown), pct(b.MaxDrawdown))
	fmt.Fprintln(w, strings.Repeat("─", 52))
	fmt.Fprintf(w, "Trades %d (%.2f/day, traded on %d days) | win rate %.0f%% | avg R %.2f | profit factor %.2f\n",
		m.Trades, r.TradesPerDay, r.DaysTraded, m.WinRate*100, r.AvgR, m.ProfitFactor)
	fmt.Fprintf(w, "Avg hold %.0f min | unfilled entries %d | daily-loss-limit days %d | exits %v\n",
		r.AvgHoldMinutes, r.Unfilled, r.HaltDays, r.ExitReasons)
	if len(r.Yearly) > 0 {
		fmt.Fprintln(w, "\nYear   Strategy  Buy&hold   MaxDD")
		for _, y := range r.Yearly {
			fmt.Fprintf(w, "%d  %s    %s  %s\n", y.Year, pct(y.Return), pct(y.Benchmark), pct(y.MaxDrawdown))
		}
	}
}
