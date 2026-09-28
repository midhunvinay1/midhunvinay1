// Command trendbot is an LLM-free trading bot: diversified multi-asset trend
// following with momentum rotation and volatility targeting, rebalanced
// monthly, executed on Alpaca paper or Robinhood (direct MCP, no LLM).
//
// It shares the backtester, risk engine, brokers and runner with tradebot,
// but links no LLM code (`make check-llm-free` verifies this).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/alpaca"
	"github.com/midhunvinay1/tradebot/internal/backtest"
	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/cliutil"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/lock"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/notify"
	"github.com/midhunvinay1/tradebot/internal/risk"
	"github.com/midhunvinay1/tradebot/internal/runner"
	"github.com/midhunvinay1/tradebot/internal/trend"
)

const usage = `trendbot — LLM-free multi-asset trend following (momentum rotation + volatility targeting)

Usage:
  trendbot fetch    [--config C] [--start 2016-01-01] [--out data/etf]     daily ETF bars from Alpaca
  trendbot synth    [--config C] [--out data/etf-synth]                    synthetic data (pipeline test only)
  trendbot backtest [--config C] --data DIR [--start D] [--end D] [--out DIR]
  trendbot sweep    [--config C] --data DIR --split D                     in-sample vs out-of-sample grid
  trendbot run      [--config C] --broker dry-run|alpaca-paper|robinhood-native [--data alpaca|DIR] [--force]
  trendbot status | halt | resume [--config C] [--broker B] [--reason TEXT]
  trendbot rh-login | rh-tools [--config C]                                Robinhood direct MCP login / tool list

Default config: configs/trendbot.yaml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "fetch":
		err = cmdFetch(ctx, os.Args[2:])
	case "synth":
		err = cmdSynth(os.Args[2:])
	case "backtest":
		err = cmdBacktest(ctx, os.Args[2:])
	case "sweep":
		err = cmdSweep(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "status", "halt", "resume":
		err = cmdKill(os.Args[1], os.Args[2:])
	case "rh-login", "rh-tools":
		err = cmdRobinhood(ctx, os.Args[1], os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func load(fs *flag.FlagSet, args []string) (*config.Config, error) {
	path := fs.String("config", "configs/trendbot.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

func planner(cfg *config.Config) runner.PlannerFactory {
	return func(raw json.RawMessage) (runner.Planner, error) {
		st := &trend.State{}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, st); err != nil {
				return nil, fmt.Errorf("trend state: %w", err)
			}
		}
		return trend.New(cfg, st), nil
	}
}

func cmdFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	start := fs.String("start", "2016-01-01", "first date (Alpaca history starts in 2016)")
	out := fs.String("out", "data/etf", "output directory")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	c, err := alpaca.New(cfg.Alpaca)
	if err != nil {
		return err
	}
	s, err := cliutil.ParseDate(*start)
	if err != nil {
		return err
	}
	ds, err := c.DailyBars(ctx, cfg.Symbols(), s, market.TradingDate(time.Now()))
	if err != nil {
		return err
	}
	if err := data.SaveCSVDir(*out, ds); err != nil {
		return err
	}
	fmt.Printf("Saved %d symbols to %s\n", len(ds), *out)
	return nil
}

func cmdSynth(args []string) error {
	fs := flag.NewFlagSet("synth", flag.ExitOnError)
	out := fs.String("out", "data/etf-synth", "output directory")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	ds := data.Synthetic(cfg.Symbols(), cfg.Benchmark, time.Date(2008, 1, 2, 0, 0, 0, 0, time.UTC), 16, 21)
	if err := data.SaveCSVDir(*out, ds); err != nil {
		return err
	}
	fmt.Printf("Wrote SYNTHETIC data for %d symbols to %s (pipeline testing only; results are meaningless)\n", len(ds), *out)
	return nil
}

// sixtyForty is the classic balanced benchmark, run through the same engine.
func sixtyForty(cfg *config.Config) trend.Static {
	w := map[string]float64{cfg.Benchmark: 0.6}
	for _, bond := range []string{"IEF", "AGG", "BND", "TLT"} {
		for _, u := range cfg.Universe {
			if u == bond {
				w[bond] = 0.4
				return trend.Static{Weights: w, Warmup: 2, Bench: cfg.Benchmark}
			}
		}
	}
	w[cfg.Benchmark] = 1
	return trend.Static{Weights: w, Warmup: 2, Bench: cfg.Benchmark}
}

func cmdBacktest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	dir := fs.String("data", "data/etf", "CSV data directory")
	start := fs.String("start", "", "first date")
	end := fs.String("end", "", "last date")
	out := fs.String("out", "results/trendbot", "output directory")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	s, err := cliutil.ParseDate(*start)
	if err != nil {
		return err
	}
	e, err := cliutil.ParseDate(*end)
	if err != nil {
		return err
	}
	ds, err := data.LoadCSVDir(*dir, cfg.Symbols())
	if err != nil {
		return err
	}
	t0 := time.Now()
	res, err := backtest.RunWith(ctx, cfg, ds, trend.New(cfg, nil), backtest.Options{Start: s, End: e})
	if err != nil {
		return err
	}
	res.Report(os.Stdout)
	// Compare against 60/40 over exactly the same dates, costs and risk engine.
	bs, be := res.Metrics.Start, res.Metrics.End
	// The benchmark is a passive allocation: no position caps or breakers.
	bcfg := *cfg
	bcfg.Risk.MaxPositionWeight, bcfg.Risk.SoftDrawdown, bcfg.Risk.HardDrawdown, bcfg.Risk.DailyLossLimit = 1, 1, 0.999, 1
	bal, err := backtest.RunWith(ctx, &bcfg, ds, sixtyForty(cfg), backtest.Options{Start: bs, End: be})
	if err == nil {
		m := bal.Metrics
		fmt.Printf("\n60/40 (%v) same period: CAGR %.1f%% | vol %.1f%% | Sharpe %.2f | max drawdown %.1f%%\n",
			sixtyForty(cfg).Weights, m.CAGR*100, m.AnnVol*100, m.Sharpe, m.MaxDrawdown*100)
	}
	fmt.Printf("(%d days simulated in %s)\n", len(res.Equity), time.Since(t0).Round(time.Millisecond))
	return cliutil.WriteBacktest(*out, res)
}

func trendGrid() [][]backtest.Variant {
	var k, tv, lb, rb []backtest.Variant
	for _, n := range []int{3, 4, 5} {
		k = append(k, backtest.Variant{Name: fmt.Sprintf("top_k=%d", n), Apply: func(c *config.Config) { c.Trend.TopK = n }})
	}
	for _, v := range []float64{0.10, 0.15, 0.20} {
		tv = append(tv, backtest.Variant{Name: fmt.Sprintf("target_vol=%.2f", v), Apply: func(c *config.Config) { c.Trend.TargetVol = v }})
	}
	for _, set := range [][]int{{21, 63, 126, 252}, {63, 126, 252}, {126, 252}} {
		lb = append(lb, backtest.Variant{Name: fmt.Sprintf("lookbacks=%v", set), Apply: func(c *config.Config) { c.Trend.Lookbacks = set }})
	}
	for _, r := range []string{"month", "week"} {
		rb = append(rb, backtest.Variant{Name: "rebalance=" + r, Apply: func(c *config.Config) { c.Trend.Rebalance = r }})
	}
	return [][]backtest.Variant{k, tv, lb, rb}
}

func cmdSweep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	dir := fs.String("data", "data/etf", "CSV data directory")
	start := fs.String("start", "", "first in-sample date")
	split := fs.String("split", "", "first out-of-sample date (required)")
	end := fs.String("end", "", "last date")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	if *split == "" {
		return errors.New("--split is required (e.g. 2021-01-01)")
	}
	s, _ := cliutil.ParseDate(*start)
	sp, err := cliutil.ParseDate(*split)
	if err != nil {
		return err
	}
	e, _ := cliutil.ParseDate(*end)
	ds, err := data.LoadCSVDir(*dir, cfg.Symbols())
	if err != nil {
		return err
	}
	rows, err := backtest.SweepWith(ctx, cfg, ds, trendGrid(), func(c *config.Config) backtest.Planner { return trend.New(c, nil) },
		s, sp, e, runtime.NumCPU())
	if err != nil {
		return err
	}
	backtest.WriteSweep(os.Stdout, rows, 20)
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	brokerName := fs.String("broker", "dry-run", "dry-run | alpaca-paper | robinhood-native")
	dataSrc := fs.String("data", "alpaca", "alpaca, or a CSV directory")
	force := fs.Bool("force", false, "run even if already run today or the market is closed")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(cfg.StateDir, *brokerName)
	release, err := lock.Acquire(stateDir)
	if err != nil {
		return err
	}
	defer release()

	var ac *alpaca.Client
	if *dataSrc == "alpaca" || *brokerName == "alpaca-paper" {
		if ac, err = alpaca.New(cfg.Alpaca); err != nil {
			return err
		}
	}
	var src runner.DataSource = runner.CSVSource{Dir: *dataSrc}
	if *dataSrc == "alpaca" {
		src = runner.AlpacaSource{C: ac}
	}
	var b broker.Broker
	switch *brokerName {
	case "dry-run":
		b = broker.DryRun{Equity: cfg.Backtest.InitialEquity}
	case "alpaca-paper":
		b = broker.AlpacaPaper{C: ac}
	case "robinhood-native":
		n := broker.NewRobinhoodNative(cfg, false, os.Stdout)
		defer n.Close()
		b = n
	default:
		return fmt.Errorf("unknown broker %q (trendbot never routes orders through an LLM)", *brokerName)
	}
	j, err := journal.Open(stateDir)
	if err != nil {
		return err
	}
	r := &runner.Runner{Cfg: cfg, StateDir: stateDir, Broker: b, Data: src, NewPlanner: planner(cfg), Journal: j,
		Notify: notify.New(cfg.Notify.WebhookURLEnv), Now: time.Now, Force: *force, Out: os.Stdout}
	return r.Run(ctx)
}

func cmdKill(which string, args []string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	brokerName := fs.String("broker", "", "one broker's state (default: all)")
	reason := fs.String("reason", "manual halt", "reason recorded with the halt")
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	brokers := []string{"dry-run", "alpaca-paper", "robinhood-native"}
	if *brokerName != "" {
		brokers = []string{*brokerName}
	}
	for _, bn := range brokers {
		dir := filepath.Join(cfg.StateDir, bn)
		st, err := runner.LoadState(dir)
		if err != nil {
			return err
		}
		rk := risk.New(cfg, st.Risk, risk.HaltFilePath(dir))
		switch which {
		case "halt":
			rk.Halt(*reason + " @ " + time.Now().Format(time.RFC3339))
			fmt.Printf("%s: HALTED\n", bn)
		case "resume":
			rk.St.Halted, rk.St.HaltReason, rk.St.PeakEquity = false, "", 0
			_ = os.Remove(risk.HaltFilePath(dir))
			fmt.Printf("%s: resumed\n", bn)
		default:
			h, why := rk.Halted()
			var ts trend.State
			_ = json.Unmarshal(st.Strategy, &ts)
			var hold []string
			for _, s := range market.SortedKeys(ts.Targets) {
				hold = append(hold, fmt.Sprintf("%s %.0f%%", s, ts.Targets[s]*100))
			}
			fmt.Printf("%s: halted=%v %s | last run %s | last rebalance %s | targets: %s\n", bn, h, why,
				st.LastRunDate.Format("2006-01-02"), ts.LastRebalance.Format("2006-01-02"), strings.Join(hold, ", "))
			continue
		}
		st.Risk = rk.St
		if err := runner.SaveState(dir, st); err != nil {
			return err
		}
	}
	return nil
}

func cmdRobinhood(ctx context.Context, which string, args []string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	cfg, err := load(fs, args)
	if err != nil {
		return err
	}
	n := broker.NewRobinhoodNative(cfg, which == "rh-login", os.Stdout)
	defer n.Close()
	tools, err := n.ListTools(ctx)
	if err != nil {
		return err
	}
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema)
		fmt.Printf("• %s — %s\n    input: %s\n", t.Name, strings.SplitN(strings.TrimSpace(t.Description), "\n", 2)[0], schema)
	}
	fmt.Printf("\n%d tools. Map them in the robinhood.native section of the config.\n", len(tools))
	return nil
}
