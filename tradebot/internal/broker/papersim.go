package broker

import (
	"context"
	"fmt"
	"sync"

	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

// PaperSim is an in-memory broker for dry runs of the intraday loop. Every
// order fills immediately at its reference price plus slippage, so results
// are optimistic; use Alpaca paper trading for realistic fills.
type PaperSim struct {
	mu          sync.Mutex
	cash        float64
	pos         map[string]Position
	orders      map[string]OrderStatus
	next        int
	SlippageBps float64
}

func NewPaperSim(equity, slippageBps float64) *PaperSim {
	return &PaperSim{cash: equity, pos: map[string]Position{}, orders: map[string]OrderStatus{}, SlippageBps: slippageBps}
}

func (p *PaperSim) Name() string { return "dry-run" }

func (p *PaperSim) Account(context.Context) (Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := Account{Cash: p.cash, Equity: p.cash, Positions: map[string]Position{}}
	for s, x := range p.pos {
		a.Positions[s] = x
		a.Equity += x.Qty * x.AvgPrice
	}
	return a, nil
}

func (p *PaperSim) Submit(_ context.Context, orders []portfolio.Order) ([]Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Result
	for _, o := range orders {
		p.next++
		id := fmt.Sprintf("sim-%d", p.next)
		px := o.RefPrice
		if o.Side == portfolio.Buy {
			px *= 1 + p.SlippageBps/1e4
			if px > o.LimitPrice {
				px = o.LimitPrice
			}
			if px*o.Qty > p.cash+1e-9 {
				p.orders[id] = OrderStatus{Status: "rejected"}
				out = append(out, Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Status: "failed", BrokerOrderID: id, Message: "insufficient cash"})
				continue
			}
			x := p.pos[o.Symbol]
			x.Symbol = o.Symbol
			x.AvgPrice = (x.AvgPrice*x.Qty + px*o.Qty) / (x.Qty + o.Qty)
			x.Qty += o.Qty
			p.pos[o.Symbol] = x
			p.cash -= px * o.Qty
		} else {
			px *= 1 - p.SlippageBps/1e4
			if px < o.LimitPrice {
				px = o.LimitPrice
			}
			x := p.pos[o.Symbol]
			q := o.Qty
			if q > x.Qty {
				q = x.Qty
			}
			x.Qty -= q
			if x.Qty <= 1e-9 {
				delete(p.pos, o.Symbol)
			} else {
				p.pos[o.Symbol] = x
			}
			p.cash += px * q
		}
		p.orders[id] = OrderStatus{Status: "filled", FilledQty: o.Qty, FilledPrice: px}
		out = append(out, Result{OrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Status: "submitted", BrokerOrderID: id})
	}
	return out, nil
}

func (p *PaperSim) OrderStatus(_ context.Context, id string) (OrderStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.orders[id]
	if !ok {
		return OrderStatus{}, fmt.Errorf("unknown order %s", id)
	}
	return s, nil
}

func (p *PaperSim) Cancel(context.Context, string) error { return nil }
