// Command tradebot is a medium-risk swing-trading bot: a deterministic
// momentum + mean-reversion strategy, a Claude review overlay, a hard risk
// gate, backtesting, Alpaca paper trading and Robinhood execution.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/alpaca"
	"github.com/midhunvinay1/tradebot/internal/backtest"
	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/hook"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/notify"
	"github.com/midhunvinay1/tradebot/internal/risk"
	"github.com/midhunvinay1/tradebot/internal/runner"
)

const usage = `tradebot — momentum + mean-reversion swing bot with a Claude risk overlay

Usage:
  tradebot fetch    --config C --start 2012-01-01 [--out data/real]     download daily bars from Alpaca
  tradebot synth    --config C [--out data/synth] [--years 12]          synthetic data (pipeline test only)
  tradebot backtest --config C --data DIR [--start D] [--end D] [--out DIR]
  tradebot sweep    --config C --data DIR --split D [--start D] [--end D]
  tradebot run      --config C --broker dry-run|alpaca-paper|robinhood [--data alpaca|DIR] [--force]
  tradebot halt     --config C [--reason TEXT]                          engage the kill switch
  tradebot resume   --config C                                          release the kill switch
  tradebot status   --config C [--broker ...]
  tradebot hook pretooluse                                              Claude Code PreToolUse guard (reads stdin)
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
	case "halt", "resume", "status":
		err = cmdKill(os.Args[1], os.Args[2:])
	case "hook":
		os.Exit(cmdHook(os.Args[2:], os.Stdin, os.Stderr))
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

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, string, error) {
	path := fs.String("config", "configs/config.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return nil, "", err
	}
	abs, _ := filepath.Abs(*path)
	return cfg, abs, nil
}

func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse("2006-01-02", s)
	return market.Day(t), err
}

func cmdFetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	start := fs.String("start", "2012-01-01", "first date")
	end := fs.String("end", "", "last date (default: today)")
	out := fs.String("out", "data/real", "output directory")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	c, err := alpaca.New(cfg.Alpaca)
	if err != nil {
		return err
	}
	s, err := parseDate(*start)
	if err != nil {
		return err
	}
	e := market.TradingDate(time.Now())
	if *end != "" {
		if e, err = parseDate(*end); err != nil {
			return err
		}
	}
	ds, err := c.DailyBars(ctx, cfg.Symbols(), s, e)
	if err != nil {
		return err
	}
	for _, sym := range cfg.Symbols() {
		if len(ds[sym]) == 0 {
			fmt.Fprintf(os.Stderr, "warning: no bars for %s\n", sym)
		}
	}
	if err := data.SaveCSVDir(*out, ds); err != nil {
		return err
	}
	fmt.Printf("Saved %d symbols to %s\n", len(ds), *out)
	return nil
}

func cmdSynth(args []string) error {
	fs := flag.NewFlagSet("synth", flag.ExitOnError)
	out := fs.String("out", "data/synth", "output directory")
	years := fs.Int("years", 12, "years of data")
	seed := fs.Int64("seed", 7, "random seed")
	start := fs.String("start", "2012-01-02", "first date")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	st, err := parseDate(*start)
	if err != nil {
		return err
	}
	ds := data.Synthetic(cfg.Symbols(), cfg.Benchmark, st, *years, *seed)
	if err := data.SaveCSVDir(*out, ds); err != nil {
		return err
	}
	fmt.Printf("Wrote SYNTHETIC data for %d symbols to %s (for pipeline testing only; results are meaningless)\n", len(ds), *out)
	return nil
}

func cmdBacktest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	dir := fs.String("data", "data/real", "CSV data directory")
	start := fs.String("start", "", "first date")
	end := fs.String("end", "", "last date")
	out := fs.String("out", "results", "output directory for equity/trades CSV and summary JSON")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *start == "" {
		*start = cfg.Backtest.Start
	}
	if *end == "" {
		*end = cfg.Backtest.End
	}
	s, err := parseDate(*start)
	if err != nil {
		return err
	}
	e, err := parseDate(*end)
	if err != nil {
		return err
	}
	ds, err := data.LoadCSVDir(*dir, cfg.Symbols())
	if err != nil {
		return err
	}
	t0 := time.Now()
	res, err := backtest.Run(ctx, cfg, ds, backtest.Options{Start: s, End: e})
	if err != nil {
		return err
	}
	res.Report(os.Stdout)
	fmt.Printf("\n(%d bars simulated in %s)\n", len(res.Equity), time.Since(t0).Round(time.Millisecond))
	return writeResults(*out, res)
}

func writeResults(dir string, res *backtest.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), b, 0o644); err != nil {
		return err
	}
	f2 := func(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }
	rows := [][]string{{"date", "equity", "cash", "gross", "benchmark"}}
	for _, p := range res.Equity {
		rows = append(rows, []string{p.Date.Format("2006-01-02"), f2(p.Equity), f2(p.Cash), f2(p.Gross), f2(p.Benchmark)})
	}
	if err := writeCSV(filepath.Join(dir, "equity.csv"), rows); err != nil {
		return err
	}
	rows = [][]string{{"symbol", "entry_date", "exit_date", "cost", "pnl", "pnl_pct", "hold_days"}}
	for _, t := range res.Trades {
		rows = append(rows, []string{t.Symbol, t.EntryDate.Format("2006-01-02"), t.ExitDate.Format("2006-01-02"),
			f2(t.Cost), f2(t.PnL), f2(t.PnLPct), strconv.Itoa(t.HoldDays)})
	}
	if err := writeCSV(filepath.Join(dir, "trades.csv"), rows); err != nil {
		return err
	}
	rows = [][]string{{"date", "symbol", "side", "qty", "price", "kind", "reason"}}
	for _, f := range res.Fills {
		rows = append(rows, []string{f.Date.Format("2006-01-02"), f.Symbol, f.Side, f2(f.Qty), f2(f.Price), f.Kind, f.Reason})
	}
	if err := writeCSV(filepath.Join(dir, "fills.csv"), rows); err != nil {
		return err
	}
	fmt.Printf("Wrote %s/{summary.json,equity.csv,trades.csv,fills.csv}\n", dir)
	return nil
}

func writeCSV(path string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cmdSweep(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	dir := fs.String("data", "data/real", "CSV data directory")
	start := fs.String("start", "", "first in-sample date")
	split := fs.String("split", "", "first out-of-sample date (required)")
	end := fs.String("end", "", "last out-of-sample date")
	top := fs.Int("top", 20, "rows to print")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	if *split == "" {
		return errors.New("--split is required (e.g. 2021-01-01)")
	}
	s, err := parseDate(*start)
	if err != nil {
		return err
	}
	sp, err := parseDate(*split)
	if err != nil {
		return err
	}
	e, err := parseDate(*end)
	if err != nil {
		return err
	}
	ds, err := data.LoadCSVDir(*dir, cfg.Symbols())
	if err != nil {
		return err
	}
	t0 := time.Now()
	rows, err := backtest.Sweep(ctx, cfg, ds, backtest.DefaultGrid(), s, sp, e, runtime.NumCPU())
	if err != nil {
		return err
	}
	backtest.WriteSweep(os.Stdout, rows, *top)
	fmt.Printf("(%d backtests in %s)\n", 2*len(rows), time.Since(t0).Round(time.Millisecond))
	return nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	brokerName := fs.String("broker", "dry-run", "dry-run | alpaca-paper | robinhood")
	dataSrc := fs.String("data", "alpaca", "alpaca, or a CSV directory")
	force := fs.Bool("force", false, "run even if already run today or the market is closed")
	cfg, cfgPath, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(cfg.StateDir, *brokerName)

	var ac *alpaca.Client
	needAlpaca := *dataSrc == "alpaca" || *brokerName == "alpaca-paper"
	if needAlpaca {
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
	case "robinhood":
		b = &broker.Robinhood{Cfg: cfg.Robinhood, StateDir: stateDir, ConfigPath: cfgPath}
	default:
		return fmt.Errorf("unknown broker %q", *brokerName)
	}

	var rev llm.Reviewer = llm.ApproveAll{}
	if cfg.LLM.Enabled {
		c, err := llm.NewClaude(cfg.LLM, stateDir)
		if err != nil {
			return err
		}
		rev = c
	}
	j, err := journal.Open(stateDir)
	if err != nil {
		return err
	}
	r := &runner.Runner{
		Cfg: cfg, StateDir: stateDir, Broker: b, Data: src, Reviewer: rev, Journal: j,
		Notify: notify.New(cfg.Notify.WebhookURLEnv), Now: time.Now, Force: *force, Out: os.Stdout,
	}
	return r.Run(ctx)
}

func cmdKill(which string, args []string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	brokerName := fs.String("broker", "", "limit to one broker's state (default: all)")
	reason := fs.String("reason", "manual halt", "reason recorded with the halt")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	brokers := []string{"dry-run", "alpaca-paper", "robinhood"}
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
			rk.St.Halted, rk.St.HaltReason = false, ""
			_ = os.Remove(risk.HaltFilePath(dir))
			fmt.Printf("%s: resumed (peak equity reset to current on next run)\n", bn)
			rk.St.PeakEquity = 0
		case "status":
			h, why := rk.Halted()
			fmt.Printf("%s: halted=%v %s | peak equity %.2f | last run %s | momentum %d | mean-reversion %d\n", bn, h, why,
				st.Risk.PeakEquity, st.LastRunDate.Format("2006-01-02"), len(st.Strategy.Momentum), len(st.Strategy.MeanRev))
			continue
		}
		st.Risk = rk.St
		if err := runner.SaveState(dir, st); err != nil {
			return err
		}
	}
	return nil
}

// cmdHook implements the Claude Code PreToolUse hook. Exit 0 allows the
// tool call; exit 2 blocks it and shows stderr to Claude.
func cmdHook(args []string, stdin io.Reader, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "pretooluse" {
		fmt.Fprintln(stderr, "usage: tradebot hook pretooluse")
		return 2
	}
	deny := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "tradebot guard: "+format+"\n", a...)
		return 2
	}
	cfgPath, stateDir := os.Getenv("TRADEBOT_CONFIG"), os.Getenv("TRADEBOT_STATE_DIR")
	if cfgPath == "" || stateDir == "" {
		return deny("TRADEBOT_CONFIG/TRADEBOT_STATE_DIR not set; run Claude only via `tradebot run --broker robinhood`")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return deny("cannot load config: %v", err)
	}
	var call hook.Call
	dec := json.NewDecoder(stdin)
	dec.UseNumber()
	if err := dec.Decode(&call); err != nil {
		return deny("cannot parse hook input: %v", err)
	}
	intents, err := hook.ReadIntents(stateDir)
	if err != nil {
		intents = hook.IntentFile{RunID: "none", ExpiresAt: time.Unix(0, 0)}
	}
	j, _ := journal.Open(stateDir)
	d := hook.Evaluate(call, cfg.Robinhood, intents, func(id string) bool { return hook.IsClaimed(stateDir, intents.RunID, id) }, time.Now())
	if d.Allow && d.IntentID != "" {
		if err := hook.ClaimIntent(stateDir, intents.RunID, d.IntentID); err != nil {
			d.Allow, d.Reason = false, err.Error()
		}
	}
	_ = j.Log("guard", map[string]any{"tool": call.ToolName, "input": call.ToolInput, "allow": d.Allow, "reason": d.Reason, "intent": d.IntentID})
	if !d.Allow {
		return deny("%s", strings.TrimSpace(d.Reason))
	}
	return 0
}
