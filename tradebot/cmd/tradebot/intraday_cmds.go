package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/alpaca"
	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/intraday"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/lock"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/notify"
)

// nativeBroker builds the direct Robinhood MCP client. Its OAuth token lives in
// state/robinhood-native whatever mode uses it.
func nativeBroker(cfg *config.Config, interactive bool) *broker.RobinhoodNative {
	return &broker.RobinhoodNative{Cfg: cfg.Robinhood, StateDir: filepath.Join(cfg.StateDir, "robinhood-native"),
		Interactive: interactive, Out: os.Stdout}
}

func cmdFetchIntraday(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fetch-intraday", flag.ExitOnError)
	start := fs.String("start", "2024-01-01", "first date")
	end := fs.String("end", "", "last date (default: today)")
	out := fs.String("out", "data/minute", "output directory")
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
	syms := append([]string{}, cfg.IntradaySymbols()...)
	if !contains(syms, cfg.Benchmark) {
		syms = append(syms, cfg.Benchmark)
	}
	// One symbol at a time keeps pages small and progress visible.
	for i, sym := range syms {
		ds, err := c.IntradayBars(ctx, []string{sym}, "1Min", s, e)
		if err != nil {
			return fmt.Errorf("%s: %w", sym, err)
		}
		if err := intraday.SaveMinuteDir(*out, ds); err != nil {
			return err
		}
		fmt.Printf("[%d/%d] %s: %d minute bars\n", i+1, len(syms), sym, len(ds[sym]))
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func cmdSynthIntraday(args []string) error {
	fs := flag.NewFlagSet("synth-intraday", flag.ExitOnError)
	out := fs.String("out", "data/minute-synth", "output directory")
	days := fs.Int("days", 250, "trading days")
	seed := fs.Int64("seed", 7, "random seed")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	syms := append([]string{}, cfg.IntradaySymbols()...)
	if !contains(syms, cfg.Benchmark) {
		syms = append(syms, cfg.Benchmark)
	}
	ds := intraday.SyntheticMinutes(syms, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), *days, *seed)
	if err := intraday.SaveMinuteDir(*out, ds); err != nil {
		return err
	}
	fmt.Printf("Wrote SYNTHETIC minute data for %d symbols to %s (pipeline testing only; results are meaningless)\n", len(ds), *out)
	return nil
}

func cmdBacktestIntraday(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backtest-intraday", flag.ExitOnError)
	dir := fs.String("data", "data/minute", "minute data directory")
	start := fs.String("start", "", "first date")
	end := fs.String("end", "", "last date")
	out := fs.String("out", "results/intraday", "output directory")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	s, err := parseDate(*start)
	if err != nil {
		return err
	}
	e, err := parseDate(*end)
	if err != nil {
		return err
	}
	syms := append([]string{}, cfg.IntradaySymbols()...)
	if !contains(syms, cfg.Benchmark) {
		syms = append(syms, cfg.Benchmark)
	}
	t0 := time.Now()
	ds, err := intraday.LoadMinuteDir(*dir, syms)
	if err != nil {
		return err
	}
	bars := 0
	for _, x := range ds {
		bars += len(x)
	}
	loaded := time.Since(t0)
	t1 := time.Now()
	res, err := intraday.Backtest(ctx, cfg, ds, intraday.BacktestOptions{Start: s, End: e,
		InitialEquity: cfg.Backtest.InitialEquity, Benchmark: cfg.Benchmark})
	if err != nil {
		return err
	}
	res.Report(os.Stdout)
	fmt.Printf("\n(%d minute bars: loaded in %s, simulated in %s)\n", bars, loaded.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "summary.json"), b, 0o644); err != nil {
		return err
	}
	rows := [][]string{{"symbol", "entry_time", "exit_time", "qty", "entry", "exit", "pnl", "r", "reason"}}
	f := func(x float64) string { return fmt.Sprintf("%.4f", x) }
	for _, t := range res.Trades {
		rows = append(rows, []string{t.Symbol, t.EntryTime.Format(time.RFC3339), t.ExitTime.Format(time.RFC3339),
			f(t.Qty), f(t.Entry), f(t.Exit), f(t.PnL), f(t.R), t.Reason})
	}
	if err := writeCSV(filepath.Join(*out, "trades.csv"), rows); err != nil {
		return err
	}
	rows = [][]string{{"date", "equity", "benchmark"}}
	for _, p := range res.Equity {
		rows = append(rows, []string{p.Date.Format("2006-01-02"), f(p.Equity), f(p.Benchmark)})
	}
	if err := writeCSV(filepath.Join(*out, "equity.csv"), rows); err != nil {
		return err
	}
	fmt.Printf("Wrote %s/{summary.json,trades.csv,equity.csv}\n", *out)
	return nil
}

func cmdDay(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("day", flag.ExitOnError)
	brokerName := fs.String("broker", "dry-run", "dry-run | alpaca-paper | robinhood-native")
	force := fs.Bool("force", false, "run even if already run today or the market is closed")
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	stateDir := filepath.Join(cfg.StateDir, "intraday-"+*brokerName)
	release, err := lock.Acquire(stateDir)
	if err != nil {
		return err
	}
	defer release()

	if *brokerName == "robinhood" {
		return fmt.Errorf("intraday trading needs sub-second order handling: use --broker robinhood-native (Claude Code sessions take 10-30 s per order)")
	}
	ac, err := alpaca.New(cfg.Alpaca) // market data for every broker
	if err != nil {
		return err
	}
	var b broker.Tracker
	switch *brokerName {
	case "dry-run":
		b = broker.NewPaperSim(cfg.Backtest.InitialEquity, cfg.Intraday.SlippageBps)
	case "alpaca-paper":
		b = broker.AlpacaPaper{C: ac}
	case "robinhood-native":
		n := nativeBroker(cfg, false)
		defer n.Close()
		b = n
	default:
		return fmt.Errorf("unknown broker %q", *brokerName)
	}
	var rev llm.PremarketReviewer
	if cfg.LLM.Enabled && cfg.Intraday.PremarketReview {
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
	l := &intraday.Live{Cfg: cfg, StateDir: stateDir, Broker: b, Feed: intraday.AlpacaFeed{C: ac}, Reviewer: rev,
		Journal: j, Notify: notify.New(cfg.Notify.WebhookURLEnv), Clock: intraday.RealClock{}, Out: os.Stdout, Force: *force}
	return l.Run(ctx)
}

func cmdRobinhoodNative(ctx context.Context, which string, args []string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	n := nativeBroker(cfg, which == "rh-login")
	defer n.Close()
	tools, err := n.ListTools(ctx)
	if err != nil {
		return err
	}
	if which == "rh-login" {
		fmt.Printf("Logged in: %d Robinhood MCP tools available. Token stored under %s\n", len(tools), filepath.Join(cfg.StateDir, "robinhood-native"))
		return nil
	}
	for _, t := range tools {
		desc := strings.SplitN(strings.TrimSpace(t.Description), "\n", 2)[0]
		schema, _ := json.Marshal(t.InputSchema)
		fmt.Printf("• %s — %s\n    input: %s\n", t.Name, desc, schema)
	}
	path := filepath.Join(cfg.StateDir, "robinhood-native", "tools.json")
	b, _ := json.MarshalIndent(tools, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("\nSaved full schemas to %s. Map them in configs/config.yaml → robinhood.native.\n", path)
	return nil
}

// cmdDoctor checks everything a live run depends on, without trading.
func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfg, _, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	ok := true
	check := func(name string, err error) {
		if err != nil {
			ok = false
			fmt.Printf("✗ %-34s %v\n", name, err)
		} else {
			fmt.Printf("✓ %s\n", name)
		}
	}
	check("config", nil)
	check("time zone America/New_York", func() error {
		if market.NewYork.String() != "America/New_York" {
			return fmt.Errorf("tzdata missing; using a fixed EST offset (DST will be wrong)")
		}
		return nil
	}())
	check("state dir writable", func() error {
		if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
			return err
		}
		f := filepath.Join(cfg.StateDir, ".doctor")
		if err := os.WriteFile(f, []byte("ok"), 0o644); err != nil {
			return err
		}
		return os.Remove(f)
	}())
	if cfg.LLM.Enabled {
		check("Anthropic API key ("+cfg.LLM.APIKeyEnv+")", func() error {
			if os.Getenv(cfg.LLM.APIKeyEnv) == "" {
				return fmt.Errorf("not set")
			}
			return nil
		}())
	}
	ac, err := alpaca.New(cfg.Alpaca)
	check("Alpaca keys", err)
	if err == nil {
		today := market.TradingDate(time.Now())
		open, err := ac.IsTradingDay(ctx, today)
		check(fmt.Sprintf("Alpaca calendar (today open: %v)", open), err)
		_, err = ac.LatestTrades(ctx, []string{cfg.Benchmark})
		check("Alpaca latest trades ("+cfg.Alpaca.PriceFeed+")", err)
		_, err = ac.DailyBars(ctx, []string{cfg.Benchmark}, today.AddDate(0, 0, -10), today)
		check("Alpaca daily bars ("+cfg.Alpaca.DataFeed+")", err)
		_, err = ac.Account(ctx)
		check("Alpaca paper account", err)
	}
	_, err = exec.LookPath(cfg.Robinhood.ClaudeBin)
	check("Claude Code CLI ("+cfg.Robinhood.ClaudeBin+") for --broker robinhood", err)
	n := nativeBroker(cfg, false)
	defer n.Close()
	if !n.HasToken() {
		check("Robinhood native login", fmt.Errorf("no token yet: run `tradebot rh-login`"))
	} else {
		tools, err := n.ListTools(ctx)
		check("Robinhood native MCP connection", err)
		if err == nil {
			names := map[string]bool{}
			for _, t := range tools {
				names[t.Name] = true
			}
			nc := cfg.Robinhood.Native
			for label, tool := range map[string]string{"account_tool": nc.AccountTool, "positions_tool": nc.PositionsTool,
				"place_order_tool": nc.PlaceOrderTool, "order_status_tool": nc.OrderStatusTool, "cancel_order_tool": nc.CancelOrderTool} {
				var e error
				if tool == "" {
					e = fmt.Errorf("not configured")
				} else if !names[tool] {
					e = fmt.Errorf("%q is not a tool on the server", tool)
				}
				check("robinhood.native."+label, e)
			}
		}
	}
	if !ok {
		return fmt.Errorf("some checks failed")
	}
	fmt.Println("All checks passed.")
	return nil
}
