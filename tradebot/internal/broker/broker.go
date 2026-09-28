// Package broker abstracts order execution: a dry run, Alpaca paper
// trading, and Robinhood (through its official Agentic Trading MCP server).
package broker

import (
	"context"
	"fmt"
	"strconv"

	"github.com/midhunvinay1/tradebot/internal/alpaca"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

type Position struct {
	Symbol   string  `json:"symbol"`
	Qty      float64 `json:"quantity"`
	AvgPrice float64 `json:"average_price"`
}

type Account struct {
	Equity    float64             `json:"equity"`
	Cash      float64             `json:"cash"`
	Positions map[string]Position `json:"positions"`
}

func (a Account) Held() map[string]float64 {
	out := map[string]float64{}
	for s, p := range a.Positions {
		out[s] = p.Qty
	}
	return out
}

type Result struct {
	OrderID       string `json:"order_id"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Status        string `json:"status"` // submitted | failed | skipped
	BrokerOrderID string `json:"broker_order_id,omitempty"`
	Message       string `json:"message,omitempty"`
}

type Broker interface {
	Name() string
	Account(ctx context.Context) (Account, error)
	Submit(ctx context.Context, orders []portfolio.Order) ([]Result, error)
}

// ---- Dry run ----

// DryRun never sends anything. It reports a flat account with fixed equity.
type DryRun struct{ Equity float64 }

func (d DryRun) Name() string { return "dry-run" }
func (d DryRun) Account(context.Context) (Account, error) {
	return Account{Equity: d.Equity, Cash: d.Equity, Positions: map[string]Position{}}, nil
}
func (d DryRun) Submit(_ context.Context, orders []portfolio.Order) ([]Result, error) {
	out := make([]Result, len(orders))
	for i, o := range orders {
		out[i] = Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Status: "skipped", Message: "dry run"}
	}
	return out, nil
}

// ---- Alpaca paper ----

type AlpacaPaper struct{ C *alpaca.Client }

func (a AlpacaPaper) Name() string { return "alpaca-paper" }

func (a AlpacaPaper) Account(ctx context.Context) (Account, error) {
	acct, err := a.C.Account(ctx)
	if err != nil {
		return Account{}, err
	}
	if acct.Blocked {
		return Account{}, fmt.Errorf("alpaca account is blocked from trading")
	}
	ps, err := a.C.Positions(ctx)
	if err != nil {
		return Account{}, err
	}
	out := Account{Equity: acct.Equity, Cash: acct.Cash, Positions: map[string]Position{}}
	for _, p := range ps {
		out.Positions[p.Symbol] = Position{Symbol: p.Symbol, Qty: p.Qty, AvgPrice: p.AvgPrice}
	}
	return out, nil
}

func (a AlpacaPaper) Submit(ctx context.Context, orders []portfolio.Order) ([]Result, error) {
	var out []Result
	for _, o := range orders {
		r := Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side}
		resp, err := a.C.SubmitOrder(ctx, alpaca.OrderRequest{
			Symbol: o.Symbol, Qty: strconv.FormatFloat(o.Qty, 'f', -1, 64), Side: o.Side, Type: "limit",
			TimeInForce: "day", LimitPrice: strconv.FormatFloat(o.LimitPrice, 'f', -1, 64), ClientOrderID: o.ID,
		})
		if err != nil {
			r.Status, r.Message = "failed", err.Error()
		} else {
			r.Status, r.BrokerOrderID = "submitted", resp.ID
		}
		out = append(out, r)
	}
	return out, nil
}
