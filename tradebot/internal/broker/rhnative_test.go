package broker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

// fakeRobinhood is an in-memory MCP server with Robinhood-like tools.
type fakeRobinhood struct {
	mu       sync.Mutex
	orders   []map[string]any
	canceled []string
}

func text(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

func (f *fakeRobinhood) server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-robinhood", Version: "1"}, nil)
	obj := map[string]any{"type": "object"}
	add := func(name string, h func(args map[string]any) *mcp.CallToolResult) {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: obj}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			_ = json.Unmarshal(req.Params.Arguments, &args)
			return h(args), nil
		})
	}
	add("get_portfolio", func(map[string]any) *mcp.CallToolResult {
		return text(map[string]any{"account": map[string]any{"total_equity": "$10,250.50", "cash_available": 4000.25},
			"holdings": []any{map[string]any{"ticker": "aapl", "shares": "5", "average_buy_price": "180.10"}}})
	})
	add("place_equity_order", func(args map[string]any) *mcp.CallToolResult {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.orders = append(f.orders, args)
		return text(map[string]any{"order": map[string]any{"id": "rh-1", "state": "queued"}})
	})
	add("get_order", func(map[string]any) *mcp.CallToolResult {
		return text(map[string]any{"state": "filled", "cumulative_quantity": "10", "average_price": "100.40"})
	})
	add("cancel_order", func(args map[string]any) *mcp.CallToolResult {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.canceled = append(f.canceled, args["order_id"].(string))
		return text(map[string]any{"ok": true})
	})
	return s
}

func nativeFor(t *testing.T, f *fakeRobinhood, mutate func(*config.RobinhoodConfig)) *RobinhoodNative {
	t.Helper()
	cfg := config.Defaults().Robinhood
	cfg.Native.AccountTool = "get_portfolio"
	cfg.Native.PlaceOrderTool = "place_equity_order"
	cfg.Native.OrderStatusTool = "get_order"
	cfg.Native.CancelOrderTool = "cancel_order"
	cfg.Native.OrderArgs["client_order_id"] = "ref_id"
	if mutate != nil {
		mutate(&cfg)
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := f.server().Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	n := &RobinhoodNative{Cfg: cfg, StateDir: t.TempDir(), Transport: ct}
	t.Cleanup(func() { n.Close() })
	return n
}

func TestNativeAccountParsing(t *testing.T) {
	n := nativeFor(t, &fakeRobinhood{}, nil)
	a, err := n.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Equity != 10250.50 || a.Cash != 4000.25 {
		t.Fatalf("equity/cash = %v/%v", a.Equity, a.Cash)
	}
	if p := a.Positions["AAPL"]; p.Qty != 5 || p.AvgPrice != 180.10 {
		t.Fatalf("positions = %+v", a.Positions)
	}
}

func TestNativeOrderFlow(t *testing.T) {
	f := &fakeRobinhood{}
	n := nativeFor(t, f, nil)
	ctx := context.Background()
	o := portfolio.Order{ID: "tb-1", Symbol: "MSFT", Side: portfolio.Buy, Qty: 10, LimitPrice: 100.456, RefPrice: 100.2}
	res, err := n.Submit(ctx, []portfolio.Order{o})
	if err != nil || len(res) != 1 || res[0].Status != "submitted" || res[0].BrokerOrderID != "rh-1" {
		t.Fatalf("submit: %+v %v", res, err)
	}
	got := f.orders[0]
	want := map[string]any{"symbol": "MSFT", "side": "buy", "quantity": 10.0, "limit_price": 100.46, "type": "limit", "time_in_force": "gfd", "ref_id": "tb-1"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("arg %s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
	st, err := n.OrderStatus(ctx, "rh-1")
	if err != nil || st.Status != "filled" || st.FilledQty != 10 || st.FilledPrice != 100.40 {
		t.Fatalf("status = %+v %v", st, err)
	}
	if err := n.Cancel(ctx, "someone-elses-order"); err == nil || !strings.Contains(err.Error(), "not placed by this process") {
		t.Fatalf("cancel of a foreign order must be refused: %v", err)
	}
	if err := n.Cancel(ctx, "rh-1"); err != nil || len(f.canceled) != 1 {
		t.Fatalf("cancel own order: %v %v", err, f.canceled)
	}
}

func TestNativeGuardBlocksBadConfig(t *testing.T) {
	f := &fakeRobinhood{}
	n := nativeFor(t, f, func(c *config.RobinhoodConfig) { c.Native.FixedArgs["type"] = "market" })
	res, _ := n.Submit(context.Background(), []portfolio.Order{{ID: "x", Symbol: "MSFT", Side: portfolio.Buy, Qty: 1, LimitPrice: 100, RefPrice: 100}})
	if len(res) != 1 || res[0].Status != "failed" || !strings.Contains(res[0].Message, "only limit orders") || len(f.orders) != 0 {
		t.Fatalf("a market-order config must be blocked before reaching Robinhood: %+v, sent %v", res, f.orders)
	}
}
