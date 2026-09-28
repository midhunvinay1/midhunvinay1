// Package risk is the deterministic gate every order must pass. No model
// output can change its configuration or bypass it.
package risk

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
)

// State is persisted between live runs.
type State struct {
	PeakEquity     float64   `json:"peak_equity"`
	DayStartEquity float64   `json:"day_start_equity"`
	DayStartDate   time.Time `json:"day_start_date"`
	Halted         bool      `json:"halted"`
	HaltReason     string    `json:"halt_reason,omitempty"`
}

// Snapshot is the account and market view at decision time.
type Snapshot struct {
	Now         time.Time
	LastBarDate time.Time
	Equity      float64
	Cash        float64
	Held        map[string]float64
	Prices      map[string]float64
}

type Rejection struct {
	Order  portfolio.Order `json:"order"`
	Reason string          `json:"reason"`
}

type Engine struct {
	cfg       config.RiskConfig
	defensive string
	allowed   map[string]bool
	denied    map[string]bool
	haltFile  string // "" in backtests
	St        *State
}

func New(cfg *config.Config, st *State, haltFile string) *Engine {
	if st == nil {
		st = &State{}
	}
	e := &Engine{cfg: cfg.Risk, defensive: cfg.Defensive, allowed: map[string]bool{}, denied: map[string]bool{}, haltFile: haltFile, St: st}
	for _, s := range cfg.Universe {
		e.allowed[s] = true
	}
	for _, s := range cfg.IntradaySymbols() {
		e.allowed[s] = true
	}
	if cfg.Defensive != "" {
		e.allowed[cfg.Defensive] = true
	}
	for _, s := range cfg.Risk.DenySymbols {
		e.denied[strings.ToUpper(s)] = true
	}
	return e
}

// HaltFilePath is the kill-switch file inside a state directory.
func HaltFilePath(stateDir string) string { return filepath.Join(stateDir, "HALT") }

// Halted reports whether the kill switch is engaged.
func (e *Engine) Halted() (bool, string) {
	if e.St.Halted {
		return true, e.St.HaltReason
	}
	if e.haltFile != "" {
		if b, err := os.ReadFile(e.haltFile); err == nil {
			return true, strings.TrimSpace(string(b))
		}
	}
	return false, ""
}

// Halt engages the kill switch.
func (e *Engine) Halt(reason string) {
	e.St.Halted, e.St.HaltReason = true, reason
	if e.haltFile != "" {
		_ = os.MkdirAll(filepath.Dir(e.haltFile), 0o755)
		_ = os.WriteFile(e.haltFile, []byte(reason+"\n"), 0o644)
	}
}

// Update records equity for the drawdown and daily-loss breakers.
func (e *Engine) Update(equity float64, day time.Time) {
	if equity > e.St.PeakEquity {
		e.St.PeakEquity = equity
	}
	if !e.St.DayStartDate.Equal(day) {
		e.St.DayStartDate, e.St.DayStartEquity = day, equity
	}
	if e.St.PeakEquity > 0 && equity < e.St.PeakEquity*(1-e.cfg.HardDrawdown) {
		if h, _ := e.Halted(); !h {
			e.Halt(fmt.Sprintf("hard drawdown breaker: equity %.2f is more than %.0f%% below peak %.2f", equity, e.cfg.HardDrawdown*100, e.St.PeakEquity))
		}
	}
}

// Drawdown returns the current drawdown from peak (0..1).
func (e *Engine) Drawdown(equity float64) float64 {
	if e.St.PeakEquity <= 0 {
		return 0
	}
	return math.Max(0, 1-equity/e.St.PeakEquity)
}

// ExposureScale shrinks target exposure during drawdowns (0 when halted).
func (e *Engine) ExposureScale(equity float64) float64 {
	if h, _ := e.Halted(); h {
		return 0
	}
	if e.Drawdown(equity) >= e.cfg.SoftDrawdown {
		return e.cfg.SoftDrawdownScale
	}
	return 1
}

// Check validates orders in sequence (sells first) and returns the approved
// subset. Anything it is unsure about is rejected: it fails closed.
func (e *Engine) Check(orders []portfolio.Order, s Snapshot) (approved []portfolio.Order, rejected []Rejection) {
	reject := func(o portfolio.Order, format string, args ...any) {
		rejected = append(rejected, Rejection{Order: o, Reason: fmt.Sprintf(format, args...)})
	}
	halted, haltReason := e.Halted()
	stale := s.Now.Sub(s.LastBarDate) > time.Duration(e.cfg.MaxDataAgeDays)*24*time.Hour
	dailyLossHit := e.St.DayStartEquity > 0 && s.Equity < e.St.DayStartEquity*(1-e.cfg.DailyLossLimit)

	gross := 0.0
	for _, sym := range market.SortedKeys(s.Held) {
		gross += math.Abs(s.Held[sym]) * s.Prices[sym]
	}
	cash := s.Cash
	turnover := 0.0
	held := map[string]float64{}
	for k, v := range s.Held {
		held[k] = v
	}

	for _, o := range orders {
		isBuy := o.Side == portfolio.Buy
		switch {
		case s.Equity <= 0:
			reject(o, "equity is not positive")
			continue
		case stale:
			reject(o, "market data is stale (last bar %s)", s.LastBarDate.Format("2006-01-02"))
			continue
		case halted && (isBuy || !e.cfg.HaltAllowsExits):
			reject(o, "kill switch engaged: %s", haltReason)
			continue
		case isBuy && dailyLossHit:
			reject(o, "daily loss limit hit (%.1f%%)", e.cfg.DailyLossLimit*100)
			continue
		case o.Side != portfolio.Buy && o.Side != portfolio.Sell:
			reject(o, "invalid side %q", o.Side)
			continue
		case !(o.Qty > 0) || math.IsInf(o.Qty, 0):
			reject(o, "invalid quantity %v", o.Qty)
			continue
		case !(o.RefPrice > 0) || !(o.LimitPrice > 0):
			reject(o, "invalid price (ref %v, limit %v)", o.RefPrice, o.LimitPrice)
			continue
		case !e.allowed[o.Symbol]:
			reject(o, "symbol %s is not in the configured universe", o.Symbol)
			continue
		case e.denied[o.Symbol]:
			reject(o, "symbol %s is on the deny list", o.Symbol)
			continue
		case len(approved) >= e.cfg.MaxOrdersPerRun:
			reject(o, "max orders per run (%d) reached", e.cfg.MaxOrdersPerRun)
			continue
		}
		if dev := math.Abs(o.LimitPrice/o.RefPrice-1) * 1e4; dev > e.cfg.MaxPriceDeviationBps {
			reject(o, "limit %.2f deviates %.0f bps from reference %.2f", o.LimitPrice, dev, o.RefPrice)
			continue
		}
		notional := o.Qty * o.LimitPrice
		if turnover+notional > e.cfg.MaxTurnoverPerRun*s.Equity {
			reject(o, "turnover limit %.0f%% of equity reached", e.cfg.MaxTurnoverPerRun*100)
			continue
		}
		if isBuy {
			post := (held[o.Symbol] + o.Qty) * o.RefPrice / s.Equity
			if o.Symbol != e.defensive && post > e.cfg.MaxPositionWeight+1e-6 {
				reject(o, "position would be %.1f%% of equity (max %.0f%%)", post*100, e.cfg.MaxPositionWeight*100)
				continue
			}
			if gross+o.Qty*o.RefPrice > e.cfg.MaxGrossExposure*s.Equity*(1+1e-6) {
				reject(o, "gross exposure would exceed %.0f%% of equity", e.cfg.MaxGrossExposure*100)
				continue
			}
			if notional > cash+1e-6 {
				reject(o, "insufficient cash (%.2f needed, %.2f available)", notional, cash)
				continue
			}
			cash -= notional
			gross += o.Qty * o.RefPrice
			held[o.Symbol] += o.Qty
		} else {
			if o.Qty > held[o.Symbol]+1e-9 {
				reject(o, "sell of %.4f exceeds holding %.4f (no shorting)", o.Qty, held[o.Symbol])
				continue
			}
			cash += notional
			gross -= o.Qty * o.RefPrice
			held[o.Symbol] -= o.Qty
		}
		turnover += notional
		approved = append(approved, o)
	}
	return approved, rejected
}
