// Package runner executes one live (or paper) trading cycle. It is meant to
// run once per trading day, shortly after the open (e.g. 09:40 New York),
// using daily bars through the previous close, which matches the backtest
// timeline (decide at close, fill at the next open).
package runner

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
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/notify"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
	"github.com/midhunvinay1/tradebot/internal/risk"
	"github.com/midhunvinay1/tradebot/internal/strategy"
)

// DataSource supplies prices and news for a live run.
type DataSource interface {
	Bars(ctx context.Context, symbols []string, start, end time.Time) (market.Dataset, error)
	LatestPrices(ctx context.Context, symbols []string) (map[string]float64, error)
	News(ctx context.Context, symbol string, since time.Time, limit int) ([]market.News, error)
	IsTradingDay(ctx context.Context, day time.Time) (bool, error)
}

type AlpacaSource struct{ C *alpaca.Client }

func (a AlpacaSource) Bars(ctx context.Context, s []string, start, end time.Time) (market.Dataset, error) {
	return a.C.DailyBars(ctx, s, start, end)
}
func (a AlpacaSource) LatestPrices(ctx context.Context, s []string) (map[string]float64, error) {
	return a.C.LatestPrices(ctx, s)
}
func (a AlpacaSource) News(ctx context.Context, sym string, since time.Time, n int) ([]market.News, error) {
	return a.C.News(ctx, sym, since, n)
}
func (a AlpacaSource) IsTradingDay(ctx context.Context, d time.Time) (bool, error) {
	return a.C.IsTradingDay(ctx, d)
}

// CSVSource reads bars from a directory (no live prices or news).
type CSVSource struct{ Dir string }

func (c CSVSource) Bars(_ context.Context, s []string, _, _ time.Time) (market.Dataset, error) {
	return data.LoadCSVDir(c.Dir, s)
}
func (CSVSource) LatestPrices(context.Context, []string) (map[string]float64, error) { return nil, nil }
func (CSVSource) News(context.Context, string, time.Time, int) ([]market.News, error) {
	return nil, nil
}
func (CSVSource) IsTradingDay(_ context.Context, d time.Time) (bool, error) {
	return d.Weekday() != time.Saturday && d.Weekday() != time.Sunday, nil
}

// Persisted is the bot's state between runs.
type Persisted struct {
	Strategy    *strategy.State `json:"strategy"`
	Risk        *risk.State     `json:"risk"`
	LastRunDate time.Time       `json:"last_run_date"`
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

func LoadState(dir string) (*Persisted, error) {
	p := &Persisted{Strategy: strategy.NewState(), Risk: &risk.State{}}
	b, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", statePath(dir), err)
	}
	return p, nil
}

func SaveState(dir string, p *Persisted) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}

type Runner struct {
	Cfg      *config.Config
	StateDir string
	Broker   broker.Broker
	Data     DataSource
	Reviewer llm.Reviewer
	Journal  *journal.Journal
	Notify   *notify.Notifier
	Now      func() time.Time
	Force    bool
	Out      io.Writer
}

func (r *Runner) logf(format string, a ...any) { fmt.Fprintf(r.Out, format+"\n", a...) }

// Run performs one daily cycle.
func (r *Runner) Run(ctx context.Context) error {
	cfg := r.Cfg
	now := r.Now()
	today := market.TradingDate(now)
	st, err := LoadState(r.StateDir)
	if err != nil {
		return err
	}
	if !r.Force && st.LastRunDate.Equal(today) {
		r.logf("Already ran for %s; use --force to run again.", today.Format("2006-01-02"))
		return nil
	}
	if ok, err := r.Data.IsTradingDay(ctx, today); err != nil {
		r.logf("warning: calendar check failed: %v", err)
	} else if !ok && !r.Force {
		r.logf("Market is closed on %s; nothing to do.", today.Format("2006-01-02"))
		return nil
	}

	rk := risk.New(cfg, st.Risk, risk.HaltFilePath(r.StateDir))
	if halted, why := rk.Halted(); halted && !cfg.Risk.HaltAllowsExits {
		r.logf("Kill switch engaged (%s) and exits are disabled; doing nothing.", why)
		return nil
	}

	acct, err := r.Broker.Account(ctx)
	if err != nil {
		return fmt.Errorf("broker account: %w", err)
	}
	held := acct.Held()
	_ = r.Journal.Log("account", acct)

	ds, err := r.Data.Bars(ctx, cfg.Symbols(), today.AddDate(0, 0, -600), today)
	if err != nil {
		return fmt.Errorf("market data: %w", err)
	}
	ds = ds.Until(today.AddDate(0, 0, -1)) // never use today's partial bar
	bench, ok := ds[cfg.Benchmark]
	if !ok || len(bench) == 0 {
		return fmt.Errorf("no data for benchmark %s", cfg.Benchmark)
	}
	lastBar := bench.Last().Date

	strat := strategy.New(cfg, st.Strategy)
	strat.Reconcile(held)
	rk.Update(acct.Equity, today)
	plan, err := strat.Plan(ds)
	if err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	_ = r.Journal.Log("plan", plan)
	scale := rk.ExposureScale(acct.Equity)
	targets := map[string]float64{}
	for s, w := range plan.Weights {
		targets[s] = w * scale
	}

	prices := map[string]float64{}
	for s, ser := range ds {
		prices[s] = ser.Last().Close
	}
	if latest, err := r.Data.LatestPrices(ctx, cfg.Symbols()); err != nil {
		r.logf("warning: latest prices unavailable, using last close: %v", err)
	} else {
		for s, p := range latest {
			if c := prices[s]; c > 0 && math.Abs(p/c-1) <= 0.25 {
				prices[s] = p
			} else if c > 0 {
				r.logf("warning: ignoring implausible latest price for %s: %.2f vs close %.2f", s, p, c)
			}
		}
	}
	managed := map[string]bool{}
	for _, s := range cfg.Symbols() {
		managed[s] = true
	}
	orders := portfolio.Diff(portfolio.Input{
		Date: today, Targets: targets, Held: held, Prices: prices, Equity: acct.Equity, Managed: managed, Reasons: plan.OrderReasons(),
	}, cfg.Execution)

	// Claude overlay: review new entries only; it can shrink or veto, never grow.
	var holdings []string
	for s := range held {
		holdings = append(holdings, s)
	}
	sort.Strings(holdings)
	calls := 0
	kept := orders[:0]
	var verdictLines []string
	for _, o := range orders {
		if o.Kind == portfolio.KindEntry && o.Side == portfolio.Buy && o.Symbol != cfg.Defensive && cfg.LLM.Enabled && cfg.LLM.ReviewNewEntries {
			mult, line := r.review(ctx, &calls, o, plan, ds, holdings, now)
			verdictLines = append(verdictLines, line)
			o.Qty = portfolio.RoundQty(o.Qty*mult, cfg.Execution.AllowFractional)
			if o.Qty <= 0 {
				strat.MarkRejected(o.Symbol)
				continue
			}
		}
		kept = append(kept, o)
	}
	orders = kept

	// Positions outside the universe still count toward gross exposure (valued at cost).
	riskPrices := map[string]float64{}
	for sym, p := range acct.Positions {
		riskPrices[sym] = p.AvgPrice
	}
	for sym, p := range prices {
		riskPrices[sym] = p
	}
	approved, rejected := rk.Check(orders, risk.Snapshot{
		Now: now, LastBarDate: lastBar, Equity: acct.Equity, Cash: acct.Cash, Held: held, Prices: riskPrices,
	})
	for _, rj := range rejected {
		if rj.Order.Kind == portfolio.KindEntry {
			strat.MarkRejected(rj.Order.Symbol)
		}
	}
	_ = r.Journal.Log("orders", map[string]any{"approved": approved, "rejected": rejected})

	results, subErr := r.Broker.Submit(ctx, approved)
	_ = r.Journal.Log("results", map[string]any{"broker": r.Broker.Name(), "results": results, "error": errString(subErr)})

	st.Strategy, st.Risk, st.LastRunDate = strat.St, rk.St, today
	if err := SaveState(r.StateDir, st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	summary := r.summary(plan, scale, acct, approved, rejected, results, verdictLines, rk)
	r.logf("%s", summary)
	if err := r.Notify.Send(ctx, summary); err != nil {
		r.logf("warning: notification failed: %v", err)
	}
	if subErr != nil {
		return fmt.Errorf("submit: %w", subErr)
	}
	return nil
}

func (r *Runner) review(ctx context.Context, calls *int, o portfolio.Order, plan *strategy.Plan, ds market.Dataset, holdings []string, now time.Time) (float64, string) {
	cfg := r.Cfg.LLM
	fallback := 0.0
	if cfg.OnError == "allow" {
		fallback = 1
	}
	if *calls >= cfg.MaxCallsPerRun {
		return fallback, fmt.Sprintf("%s: review budget exhausted → multiplier %.2f", o.Symbol, fallback)
	}
	*calls++
	news, err := r.Data.News(ctx, o.Symbol, now.AddDate(0, 0, -cfg.NewsLookbackDays), cfg.MaxHeadlines)
	if err != nil {
		r.logf("warning: news for %s unavailable: %v", o.Symbol, err)
	}
	cand := llm.BuildCandidate(plan.Date, o.Symbol, plan.Sleeve[o.Symbol], plan.Reasons[o.Symbol], o.TargetWeight, ds[o.Symbol], plan.RiskOn, holdings, news)
	v, err := r.Reviewer.Review(ctx, cand)
	if err != nil {
		_ = r.Journal.Log("llm_error", map[string]any{"symbol": o.Symbol, "error": err.Error()})
		return fallback, fmt.Sprintf("%s: review failed (%v) → multiplier %.2f", o.Symbol, err, fallback)
	}
	_ = r.Journal.Log("llm_verdict", map[string]any{"candidate": cand, "verdict": v})
	return v.Multiplier(), fmt.Sprintf("%s: %s ×%.2f %v — %s", o.Symbol, v.Decision, v.Multiplier(), v.RiskFlags, v.Rationale)
}

func (r *Runner) summary(plan *strategy.Plan, scale float64, acct broker.Account, approved []portfolio.Order, rejected []risk.Rejection, results []broker.Result, verdicts []string, rk *risk.Engine) string {
	var b strings.Builder
	regime := "RISK-OFF"
	if plan.RiskOn {
		regime = "RISK-ON"
	}
	fmt.Fprintf(&b, "tradebot %s | %s | data through %s | equity %.2f cash %.2f | drawdown %.1f%%\n",
		r.Broker.Name(), regime, plan.Date.Format("2006-01-02"), acct.Equity, acct.Cash, rk.Drawdown(acct.Equity)*100)
	fmt.Fprintf(&b, "ex-ante vol %.1f%% → vol scale %.2f, drawdown scale %.2f\n", plan.ExAnteVol*100, plan.VolScale, scale)
	if h, why := rk.Halted(); h {
		fmt.Fprintf(&b, "⚠ KILL SWITCH: %s\n", why)
	}
	syms := make([]string, 0, len(plan.Weights))
	for s := range plan.Weights {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	b.WriteString("Targets:\n")
	for _, s := range syms {
		fmt.Fprintf(&b, "  %-6s %5.1f%%  %s\n", s, plan.Weights[s]*scale*100, plan.Reasons[s])
	}
	for s, why := range plan.Exits {
		fmt.Fprintf(&b, "  exit %-6s %s\n", s, why)
	}
	for _, v := range verdicts {
		fmt.Fprintf(&b, "Claude: %s\n", v)
	}
	for _, o := range approved {
		fmt.Fprintf(&b, "APPROVED %-4s %g %s @ %.2f (%s)\n", o.Side, o.Qty, o.Symbol, o.LimitPrice, o.Kind)
	}
	for _, rj := range rejected {
		fmt.Fprintf(&b, "REJECTED %-4s %g %s: %s\n", rj.Order.Side, rj.Order.Qty, rj.Order.Symbol, rj.Reason)
	}
	for _, res := range results {
		fmt.Fprintf(&b, "%s %s %s: %s %s\n", strings.ToUpper(res.Status), res.Side, res.Symbol, res.BrokerOrderID, res.Message)
	}
	if len(approved) == 0 {
		b.WriteString("No orders today.\n")
	}
	return b.String()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
