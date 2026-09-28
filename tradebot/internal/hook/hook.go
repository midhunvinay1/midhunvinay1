// Package hook is the Claude Code PreToolUse guard for Robinhood execution.
//
// Claude Code is the MCP client for Robinhood's official Agentic Trading
// server. This guard runs before every tool call Claude makes and allows
// only:
//   - read-only Robinhood tools, and
//   - order tools whose arguments exactly match an order intent that the
//     deterministic risk engine approved in this run (same symbol and side,
//     quantity no larger, limit price no worse). Each intent can be used once.
//
// Everything else, including any tool it cannot classify, is denied.
package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/midhunvinay1/tradebot/internal/config"
)

// Intent is one risk-approved order Claude is permitted to place.
type Intent struct {
	ID         string  `json:"id"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	Qty        float64 `json:"qty"`
	LimitPrice float64 `json:"limit_price"`
}

type IntentFile struct {
	RunID     string    `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Intents   []Intent  `json:"intents"`
}

func IntentDir(stateDir string) string  { return filepath.Join(stateDir, "intents") }
func IntentPath(stateDir string) string { return filepath.Join(IntentDir(stateDir), "current.json") }

// WriteIntents replaces the current intent file (an empty list revokes all).
func WriteIntents(stateDir string, f IntentFile) error {
	if err := os.MkdirAll(filepath.Join(IntentDir(stateDir), "used"), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := IntentPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, IntentPath(stateDir))
}

func ReadIntents(stateDir string) (IntentFile, error) {
	var f IntentFile
	b, err := os.ReadFile(IntentPath(stateDir))
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

// ClaimIntent atomically marks an intent as used; it fails if already used.
func ClaimIntent(stateDir, runID, id string) error {
	dir := filepath.Join(IntentDir(stateDir), "used")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, runID+"__"+id)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("intent %s was already used", id)
		}
		return err
	}
	return f.Close()
}

func IsClaimed(stateDir, runID, id string) bool {
	_, err := os.Stat(filepath.Join(IntentDir(stateDir), "used", runID+"__"+id))
	return err == nil
}

// Call is the subset of the PreToolUse payload the guard needs.
type Call struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

type Decision struct {
	Allow    bool
	Reason   string
	IntentID string // set when an order intent matched
}

// Evaluate classifies the tool call and checks order arguments against the
// intents. It is pure: claiming the intent is the caller's job.
func Evaluate(call Call, cfg config.RobinhoodConfig, intents IntentFile, claimed func(id string) bool, now time.Time) Decision {
	prefix := "mcp__" + cfg.MCPServer + "__"
	if !strings.HasPrefix(call.ToolName, prefix) {
		return Decision{Reason: fmt.Sprintf("tool %q is not a %s MCP tool; only Robinhood tools are allowed in the executor", call.ToolName, cfg.MCPServer)}
	}
	tokens := Tokenize(strings.TrimPrefix(call.ToolName, prefix))
	switch {
	case matchAny(tokens, cfg.DenyKeywords):
		return Decision{Reason: fmt.Sprintf("tool %q is on the deny list", call.ToolName)}
	case matchAny(tokens, cfg.OrderToolKeywords):
		return evaluateOrder(call, tokens, cfg, intents, claimed, now)
	case matchAny(tokens, cfg.ReadToolKeywords):
		return Decision{Allow: true, Reason: "read-only tool"}
	}
	return Decision{Reason: fmt.Sprintf("tool %q is not recognized as read-only or as an order tool; denied (fail closed)", call.ToolName)}
}

func evaluateOrder(call Call, tokens []string, cfg config.RobinhoodConfig, f IntentFile, claimed func(string) bool, now time.Time) Decision {
	deny := func(format string, a ...any) Decision { return Decision{Reason: fmt.Sprintf(format, a...)} }
	if now.After(f.ExpiresAt) {
		return deny("order intents expired at %s", f.ExpiresAt.Format(time.RFC3339))
	}
	sym, _ := findString(call.ToolInput, cfg.Fields["symbol"])
	sym = strings.ToUpper(strings.TrimSpace(sym))
	side, _ := findString(call.ToolInput, cfg.Fields["side"])
	side = strings.ToLower(strings.TrimSpace(side))
	if side == "" {
		switch {
		case contains(tokens, "buy"):
			side = "buy"
		case contains(tokens, "sell"):
			side = "sell"
		}
	}
	qty, okQty := findNumber(call.ToolInput, cfg.Fields["quantity"])
	limit, okLimit := findNumber(call.ToolInput, cfg.Fields["limit_price"])
	if k, bad := hasAnyKey(call.ToolInput, cfg.ForbiddenInputKeys); bad {
		return deny("order input contains forbidden field %q (options, dollar-amount and stop orders are not allowed)", k)
	}
	otype, _ := findString(call.ToolInput, cfg.Fields["order_type"])
	otype = strings.ToLower(otype)
	switch {
	case sym == "":
		return deny("order has no recognizable symbol field %v", cfg.Fields["symbol"])
	case side != "buy" && side != "sell":
		return deny("order side %q is not buy or sell", side)
	case !okQty || !(qty > 0):
		return deny("order must specify a positive share quantity (fields %v); dollar-amount orders are not allowed", cfg.Fields["quantity"])
	case otype != "" && otype != "limit":
		return deny("only limit orders are allowed (got %q)", otype)
	case !okLimit || !(limit > 0):
		return deny("order must specify a limit price (fields %v)", cfg.Fields["limit_price"])
	}
	tol := cfg.PriceToleranceBps / 1e4
	for _, in := range f.Intents {
		if in.Symbol != sym || in.Side != side || claimed(in.ID) {
			continue
		}
		if qty > in.Qty+1e-9 {
			continue
		}
		if side == "buy" && limit > in.LimitPrice*(1+tol) {
			continue
		}
		if side == "sell" && limit < in.LimitPrice*(1-tol) {
			continue
		}
		return Decision{Allow: true, IntentID: in.ID, Reason: fmt.Sprintf("matches approved intent %s", in.ID)}
	}
	return deny("no unused approved intent matches %s %g %s @ %.4f", side, qty, sym, limit)
}

// Tokenize splits snake_case, kebab-case, dotted and camelCase names into
// lowercase words: "placeEquityOrder" -> [place equity order].
func Tokenize(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// matchAny reports whether any keyword (itself possibly multi-word, e.g.
// "create_order") appears as a consecutive run of tokens.
func matchAny(tokens []string, keywords []string) bool {
	for _, k := range keywords {
		kt := Tokenize(k)
		if len(kt) == 0 {
			continue
		}
		for i := 0; i+len(kt) <= len(tokens); i++ {
			ok := true
			for j := range kt {
				if tokens[i+j] != kt[j] {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
	}
	return false
}

func contains(tokens []string, t string) bool {
	for _, x := range tokens {
		if x == t {
			return true
		}
	}
	return false
}

// hasAnyKey reports whether any key (at any depth) equals one of keys.
func hasAnyKey(in map[string]any, keys []string) (string, bool) {
	for ik, v := range in {
		for _, k := range keys {
			if strings.EqualFold(ik, k) {
				return ik, true
			}
		}
		if m, ok := v.(map[string]any); ok {
			if k, ok := hasAnyKey(m, keys); ok {
				return k, true
			}
		}
	}
	return "", false
}

// find searches the (possibly nested) input for the first key in keys.
func find(in map[string]any, keys []string) (any, bool) {
	for _, k := range keys {
		for ik, v := range in {
			if strings.EqualFold(ik, k) {
				return v, true
			}
		}
	}
	for _, v := range in {
		if m, ok := v.(map[string]any); ok {
			if r, ok := find(m, keys); ok {
				return r, true
			}
		}
	}
	return nil, false
}

func findString(in map[string]any, keys []string) (string, bool) {
	v, ok := find(in, keys)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func findNumber(in map[string]any, keys []string) (float64, bool) {
	v, ok := find(in, keys)
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(x, "$")), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
