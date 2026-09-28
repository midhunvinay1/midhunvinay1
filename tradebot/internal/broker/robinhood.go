package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/hook"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

// Robinhood executes through Robinhood's official Agentic Trading MCP server,
// using Claude Code in headless mode as the MCP client.
//
// Safety model: this bot's deterministic code decides every order. Claude
// Code only transcribes approved orders into MCP tool calls. A PreToolUse
// hook (`tradebot hook pretooluse`, configured in the executor directory)
// denies any tool call that is not read-only or does not match an approved
// intent. Robinhood additionally caps losses at the agentic account's funded
// balance and sends a push notification for every trade.
type Robinhood struct {
	Cfg        config.RobinhoodConfig
	StateDir   string
	ConfigPath string // absolute path to the bot config (passed to the hook)
	Now        func() time.Time
}

func (r *Robinhood) Name() string { return "robinhood" }

type claudeResult struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	IsError bool    `json:"is_error"`
	Result  string  `json:"result"`
	CostUSD float64 `json:"total_cost_usd"`
}

func (r *Robinhood) run(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.Cfg.TimeoutMinutes)*time.Minute)
	defer cancel()
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(r.Cfg.ExecutorDir)
	if err != nil {
		return "", err
	}
	stateDir, err := filepath.Abs(r.StateDir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, r.Cfg.ClaudeBin,
		"-p", prompt,
		"--output-format", "json",
		"--allowedTools", "mcp__"+r.Cfg.MCPServer,
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"TRADEBOT_BIN="+self,
		"TRADEBOT_CONFIG="+r.ConfigPath,
		"TRADEBOT_STATE_DIR="+stateDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude executor failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res claudeResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return "", fmt.Errorf("claude executor: unexpected output: %w", err)
	}
	if res.IsError {
		return "", fmt.Errorf("claude executor error (%s): %s", res.Subtype, res.Result)
	}
	return res.Result, nil
}

// extractJSON returns the outermost JSON object/array in s.
func extractJSON(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	closer := byte('}')
	if s[start] == '[' {
		closer = ']'
	}
	end := strings.LastIndexByte(s, closer)
	if end < start {
		return ""
	}
	return s[start : end+1]
}

const accountPrompt = `Use the Robinhood agentic trading MCP tools (read-only tools only) to look up the agentic account's current total equity (portfolio value), cash available for trading, and every open stock position.
Do not place, modify or cancel any order.
Respond with ONLY this JSON and nothing else:
{"equity": <number>, "cash": <number>, "positions": [{"symbol": "<TICKER>", "quantity": <number of shares>, "average_price": <number>}]}`

func (r *Robinhood) Account(ctx context.Context) (Account, error) {
	// Revoke any leftover order intents before a read-only session.
	if err := hook.WriteIntents(r.StateDir, hook.IntentFile{RunID: "none", ExpiresAt: time.Unix(0, 0)}); err != nil {
		return Account{}, err
	}
	out, err := r.run(ctx, accountPrompt)
	if err != nil {
		return Account{}, err
	}
	var raw struct {
		Equity    float64    `json:"equity"`
		Cash      float64    `json:"cash"`
		Positions []Position `json:"positions"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &raw); err != nil {
		return Account{}, fmt.Errorf("robinhood account: could not parse executor answer: %w", err)
	}
	acct := Account{Equity: raw.Equity, Cash: raw.Cash, Positions: map[string]Position{}}
	for _, p := range raw.Positions {
		p.Symbol = strings.ToUpper(strings.TrimSpace(p.Symbol))
		if p.Symbol == "" || p.Qty < 0 {
			return Account{}, fmt.Errorf("robinhood account: invalid position %+v", p)
		}
		if p.Qty > 0 {
			acct.Positions[p.Symbol] = p
		}
	}
	// Sanity checks: the numbers come through an LLM transcription, so verify them.
	if !(acct.Equity > 0) || acct.Cash < 0 || acct.Cash > acct.Equity*1.01 {
		return Account{}, fmt.Errorf("robinhood account: implausible equity %.2f / cash %.2f", acct.Equity, acct.Cash)
	}
	return acct, nil
}

func (r *Robinhood) Submit(ctx context.Context, orders []portfolio.Order) ([]Result, error) {
	if len(orders) == 0 {
		return nil, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	runID := now.UTC().Format("20060102T150405")
	f := hook.IntentFile{RunID: runID, CreatedAt: now, ExpiresAt: now.Add(time.Duration(r.Cfg.IntentTTLMinutes) * time.Minute)}
	for _, o := range orders {
		f.Intents = append(f.Intents, hook.Intent{ID: o.ID, Symbol: o.Symbol, Side: o.Side, Qty: o.Qty, LimitPrice: o.LimitPrice})
	}
	if err := hook.WriteIntents(r.StateDir, f); err != nil {
		return nil, err
	}
	// Always revoke intents when done, whatever happens.
	defer func() {
		_ = hook.WriteIntents(r.StateDir, hook.IntentFile{RunID: "none", ExpiresAt: time.Unix(0, 0)})
	}()

	list, _ := json.MarshalIndent(f.Intents, "", "  ")
	prompt := fmt.Sprintf(`You are an order-entry clerk for a Robinhood agentic account. A deterministic trading system has already decided and risk-checked the orders below. Your only job is to place each one exactly once using the Robinhood agentic trading MCP tools.

For each order:
- Place a LIMIT order, time in force DAY (good for the day), for exactly the given whole-share quantity and limit price, on the given side.
- Do not change the symbol, side, quantity or price. Do not place any other order. Do not cancel or modify anything.
- If a tool call is denied or fails, do not retry more than once; record the failure and move on.
- Place sell orders before buy orders.

Orders (JSON):
%s

When finished, respond with ONLY a JSON array, one element per order:
[{"order_id": "<id from above>", "status": "submitted" | "failed", "broker_order_id": "<Robinhood order id if known>", "message": "<short note>"}]`, list)

	out, err := r.run(ctx, prompt)
	results := make([]Result, 0, len(orders))
	var parsed []Result
	if err == nil {
		err = json.Unmarshal([]byte(extractJSON(out)), &parsed)
	}
	byID := map[string]Result{}
	for _, p := range parsed {
		byID[p.OrderID] = p
	}
	for _, o := range orders {
		res := Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Status: "failed"}
		// The hook's claim record is the ground truth for whether a call was allowed.
		claimed := hook.IsClaimed(r.StateDir, runID, o.ID)
		if p, ok := byID[o.ID]; ok {
			res.Status, res.BrokerOrderID, res.Message = p.Status, p.BrokerOrderID, p.Message
		}
		if res.Status == "submitted" && !claimed {
			res.Status, res.Message = "failed", "executor reported success but the guard never allowed a matching order call"
		}
		if res.Status != "submitted" && claimed {
			res.Status, res.Message = "unknown", "guard allowed an order call; verify in the Robinhood app ("+res.Message+")"
		}
		results = append(results, res)
	}
	return results, err
}
