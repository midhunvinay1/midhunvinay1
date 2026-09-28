package hook

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
)

var now = time.Date(2024, 3, 5, 14, 40, 0, 0, time.UTC)

func intents() IntentFile {
	return IntentFile{RunID: "r1", ExpiresAt: now.Add(30 * time.Minute), Intents: []Intent{
		{ID: "tb-1-AAPL-buy", Symbol: "AAPL", Side: "buy", Qty: 10, LimitPrice: 100.30},
		{ID: "tb-1-MSFT-sell", Symbol: "MSFT", Side: "sell", Qty: 5, LimitPrice: 396.00},
	}}
}

func eval(tool string, input map[string]any, claimed ...string) Decision {
	cfg := config.Defaults().Robinhood
	used := map[string]bool{}
	for _, c := range claimed {
		used[c] = true
	}
	return Evaluate(Call{ToolName: tool, ToolInput: input}, cfg, intents(), func(id string) bool { return used[id] }, now)
}

const rh = "mcp__robinhood-trading__"

func TestTokenize(t *testing.T) {
	for in, want := range map[string][]string{
		"place_equity_order": {"place", "equity", "order"},
		"placeEquityOrder":   {"place", "equity", "order"},
		"get-buying-power":   {"get", "buying", "power"},
		"quotes.get":         {"quotes", "get"},
	} {
		if got := Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClassification(t *testing.T) {
	cases := []struct {
		tool  string
		allow bool
		why   string
	}{
		{"Bash", false, "not a robinhood-trading MCP tool"},
		{"mcp__other__get_quote", false, "not a robinhood-trading MCP tool"},
		{rh + "get_quote", true, ""},
		{rh + "get_buying_power", true, ""}, // "buying" must not match the "buy" order keyword
		{rh + "list_positions", true, ""},
		{rh + "transfer_funds", false, "deny list"},
		{rh + "cancel_order", false, "deny list"},
		{rh + "place_option_order", false, "deny list"},
		{rh + "frobnicate", false, "fail closed"},
	}
	for _, c := range cases {
		d := eval(c.tool, map[string]any{})
		if d.Allow != c.allow || (c.why != "" && !strings.Contains(d.Reason, c.why)) {
			t.Errorf("%s: allow=%v reason=%q, want allow=%v containing %q", c.tool, d.Allow, d.Reason, c.allow, c.why)
		}
	}
}

func TestOrderMatchesIntent(t *testing.T) {
	d := eval(rh+"place_order", map[string]any{"symbol": "aapl", "side": "BUY", "quantity": 10.0, "type": "limit", "limit_price": "100.30"})
	if !d.Allow || d.IntentID != "tb-1-AAPL-buy" {
		t.Fatalf("expected match, got %+v", d)
	}
	// Smaller size and a better price are fine; json.Number inputs parse.
	d = eval(rh+"place_order", map[string]any{"order": map[string]any{"ticker": "MSFT", "action": "sell", "qty": json.Number("3"), "price": 397.5}})
	if !d.Allow || d.IntentID != "tb-1-MSFT-sell" {
		t.Fatalf("expected nested match, got %+v", d)
	}
	// Side inferred from the tool name.
	d = eval(rh+"buy_stock", map[string]any{"symbol": "AAPL", "quantity": 10.0, "limit_price": 100.0})
	if !d.Allow {
		t.Fatalf("expected buy_stock match, got %+v", d)
	}
}

func TestOrderRejections(t *testing.T) {
	cases := map[string]map[string]any{
		"no unused approved intent":   {"symbol": "AAPL", "side": "buy", "quantity": 11.0, "limit_price": 100.3}, // too many shares
		"no unused approved intent ":  {"symbol": "AAPL", "side": "buy", "quantity": 10.0, "limit_price": 101.5}, // price too high
		"no unused approved intent  ": {"symbol": "TSLA", "side": "buy", "quantity": 1.0, "limit_price": 100.0},  // not approved
		"only limit orders":           {"symbol": "AAPL", "side": "buy", "quantity": 10.0, "type": "market"},     // market order
		"limit price":                 {"symbol": "AAPL", "side": "buy", "quantity": 10.0},                       // no price
		"positive share quantity":     {"symbol": "AAPL", "side": "buy", "limit_price": 100.0},                   // no qty
		"forbidden field":             {"symbol": "AAPL", "side": "buy", "quantity": 1.0, "limit_price": 1.0, "strike": 100.0},
		"not buy or sell":             {"symbol": "AAPL", "side": "short", "quantity": 1.0, "limit_price": 100.0},
	}
	for want, input := range cases {
		d := eval(rh+"place_order", input)
		if d.Allow || !strings.Contains(d.Reason, strings.TrimSpace(want)) {
			t.Errorf("input %v: got %+v, want denial containing %q", input, d, want)
		}
	}
	// Already-claimed intent cannot be reused.
	d := eval(rh+"place_order", map[string]any{"symbol": "AAPL", "side": "buy", "quantity": 10.0, "limit_price": 100.3}, "tb-1-AAPL-buy")
	if d.Allow {
		t.Fatal("claimed intent must not match again")
	}
}

func TestExpiredIntents(t *testing.T) {
	cfg := config.Defaults().Robinhood
	f := intents()
	f.ExpiresAt = now.Add(-time.Minute)
	d := Evaluate(Call{ToolName: rh + "place_order", ToolInput: map[string]any{"symbol": "AAPL", "side": "buy", "quantity": 1.0, "limit_price": 100.0}},
		cfg, f, func(string) bool { return false }, now)
	if d.Allow || !strings.Contains(d.Reason, "expired") {
		t.Fatalf("expected expiry denial, got %+v", d)
	}
}

func TestClaimIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteIntents(dir, intents()); err != nil {
		t.Fatal(err)
	}
	if err := ClaimIntent(dir, "r1", "x"); err != nil {
		t.Fatal(err)
	}
	if err := ClaimIntent(dir, "r1", "x"); err == nil {
		t.Fatal("second claim must fail")
	}
	if !IsClaimed(dir, "r1", "x") || IsClaimed(dir, "r2", "x") {
		t.Fatal("claims must be scoped to the run")
	}
}
