// Package portfolio turns target weights into limit orders.
package portfolio

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
)

const (
	Buy  = "buy"
	Sell = "sell"

	KindEntry    = "entry"
	KindIncrease = "increase"
	KindDecrease = "decrease"
	KindExit     = "exit"
)

// Order is a limit order proposal. ID is deterministic per day, symbol and
// side, so resubmitting the same plan cannot create duplicate broker orders.
type Order struct {
	ID           string  `json:"id"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	Qty          float64 `json:"qty"`
	LimitPrice   float64 `json:"limit_price"`
	RefPrice     float64 `json:"ref_price"`
	Kind         string  `json:"kind"`
	TargetWeight float64 `json:"target_weight"`
	Reason       string  `json:"reason"`
}

func (o Order) Notional() float64 { return o.Qty * o.RefPrice }

// Input is everything Diff needs.
type Input struct {
	Date    time.Time
	Targets map[string]float64 // symbol -> weight of equity
	Held    map[string]float64 // symbol -> shares
	Prices  map[string]float64 // symbol -> reference price
	Equity  float64
	Managed map[string]bool // symbols the bot is allowed to trade
	Reasons map[string]string
}

// Diff computes the orders that move current holdings to the targets.
// Sells come first so their proceeds can fund buys.
func Diff(in Input, cfg config.ExecutionConfig) []Order {
	syms := map[string]bool{}
	for s := range in.Targets {
		syms[s] = true
	}
	for s, q := range in.Held {
		if q != 0 && in.Managed[s] {
			syms[s] = true
		}
	}
	var orders []Order
	for sym := range syms {
		if !in.Managed[sym] {
			continue
		}
		px := in.Prices[sym]
		if px <= 0 || math.IsNaN(px) {
			continue
		}
		tw := in.Targets[sym]
		cur := in.Held[sym]
		desired := 0.0
		if tw > 0 {
			desired = RoundQty(tw*in.Equity/px, cfg.AllowFractional)
		}
		delta := desired - cur
		if math.Abs(delta) < 1e-9 {
			continue
		}
		var kind string
		switch {
		case cur == 0:
			kind = KindEntry
		case desired == 0:
			kind = KindExit
		case delta > 0:
			kind = KindIncrease
		default:
			kind = KindDecrease
		}
		notional := math.Abs(delta) * px
		if (kind == KindIncrease || kind == KindDecrease) && notional < cfg.RebalanceBand*in.Equity {
			continue
		}
		if kind != KindExit && notional < cfg.MinOrderNotional {
			continue
		}
		o := Order{Symbol: sym, Qty: math.Abs(delta), RefPrice: px, Kind: kind, TargetWeight: tw, Reason: in.Reasons[sym]}
		if delta > 0 {
			o.Side = Buy
			o.LimitPrice = RoundPrice(px * (1 + cfg.EntryLimitBps/1e4))
		} else {
			o.Side = Sell
			o.LimitPrice = RoundPrice(px * (1 - cfg.ExitLimitBps/1e4))
		}
		o.ID = fmt.Sprintf("tb-%s-%s-%s", in.Date.Format("20060102"), sym, o.Side)
		orders = append(orders, o)
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].Side != orders[j].Side {
			return orders[i].Side == Sell
		}
		return orders[i].Symbol < orders[j].Symbol
	})
	return orders
}

// RoundQty floors to whole shares, or to 1e-4 shares when fractional trading is allowed.
func RoundQty(q float64, fractional bool) float64 {
	if fractional {
		return math.Floor(q*1e4) / 1e4
	}
	return math.Floor(q)
}

// RoundPrice rounds to a valid US equity tick: $0.01 at or above $1, $0.0001 below.
func RoundPrice(p float64) float64 {
	if p >= 1 {
		return math.Round(p*100) / 100
	}
	return math.Round(p*1e4) / 1e4
}
