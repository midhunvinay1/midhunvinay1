package backtest

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	ind "github.com/midhunvinay1/tradebot/internal/indicators"
)

type Metrics struct {
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	StartEquity  float64   `json:"start_equity"`
	EndEquity    float64   `json:"end_equity"`
	TotalReturn  float64   `json:"total_return"`
	CAGR         float64   `json:"cagr"`
	AnnVol       float64   `json:"ann_vol"`
	Sharpe       float64   `json:"sharpe"`
	Sortino      float64   `json:"sortino"`
	MaxDrawdown  float64   `json:"max_drawdown"`
	MaxDDDate    time.Time `json:"max_dd_date"`
	Calmar       float64   `json:"calmar"`
	Trades       int       `json:"trades"`
	WinRate      float64   `json:"win_rate"`
	AvgWinPct    float64   `json:"avg_win_pct"`
	AvgLossPct   float64   `json:"avg_loss_pct"`
	ProfitFactor float64   `json:"profit_factor"`
	AvgHoldDays  float64   `json:"avg_hold_days"`
	AvgExposure  float64   `json:"avg_exposure"`
	Turnover     float64   `json:"annual_turnover"`
}

// ComputeMetrics derives return/risk statistics from a daily value series.
func ComputeMetrics(pts []EquityPoint, value func(EquityPoint) float64, rf float64) Metrics {
	m := Metrics{}
	if len(pts) < 2 {
		return m
	}
	m.Start, m.End = pts[0].Date, pts[len(pts)-1].Date
	m.StartEquity, m.EndEquity = value(pts[0]), value(pts[len(pts)-1])
	m.TotalReturn = m.EndEquity/m.StartEquity - 1
	years := m.End.Sub(m.Start).Hours() / 24 / 365.25
	if years > 0 && m.EndEquity > 0 {
		m.CAGR = math.Pow(m.EndEquity/m.StartEquity, 1/years) - 1
	}
	rets := make([]float64, 0, len(pts)-1)
	peak := value(pts[0])
	for i := 1; i < len(pts); i++ {
		prev, cur := value(pts[i-1]), value(pts[i])
		if prev > 0 {
			rets = append(rets, cur/prev-1-rf/252)
		}
		if cur > peak {
			peak = cur
		}
		if dd := 1 - cur/peak; dd > m.MaxDrawdown {
			m.MaxDrawdown, m.MaxDDDate = dd, pts[i].Date
		}
	}
	sd := ind.StdDev(rets)
	mean := ind.Mean(rets)
	m.AnnVol = sd * math.Sqrt(252)
	if sd > 0 {
		m.Sharpe = mean / sd * math.Sqrt(252)
	}
	down := 0.0
	for _, r := range rets {
		if r < 0 {
			down += r * r
		}
	}
	if down > 0 {
		m.Sortino = mean / math.Sqrt(down/float64(len(rets))) * math.Sqrt(252)
	}
	if m.MaxDrawdown > 0 {
		m.Calmar = m.CAGR / m.MaxDrawdown
	}
	return m
}

// AddTrades fills in the round-trip trade statistics.
func (m *Metrics) AddTrades(trades []Trade) {
	m.Trades = len(trades)
	if len(trades) == 0 {
		return
	}
	var wins, grossWin, grossLoss, winPct, lossPct, hold float64
	var nLoss float64
	for _, t := range trades {
		hold += float64(t.HoldDays)
		if t.PnL > 0 {
			wins++
			grossWin += t.PnL
			winPct += t.PnLPct
		} else {
			nLoss++
			grossLoss -= t.PnL
			lossPct += t.PnLPct
		}
	}
	m.WinRate = wins / float64(len(trades))
	if wins > 0 {
		m.AvgWinPct = winPct / wins
	}
	if nLoss > 0 {
		m.AvgLossPct = lossPct / nLoss
	}
	if grossLoss > 0 {
		m.ProfitFactor = grossWin / grossLoss
	}
	m.AvgHoldDays = hold / float64(len(trades))
}

// AddExposure fills in average gross exposure and annual turnover.
func (m *Metrics) AddExposure(pts []EquityPoint, fills []Fill) {
	if len(pts) == 0 {
		return
	}
	exp, eq := 0.0, 0.0
	for _, p := range pts {
		if p.Equity > 0 {
			exp += p.Gross / p.Equity
		}
		eq += p.Equity
	}
	m.AvgExposure = exp / float64(len(pts))
	traded := 0.0
	for _, f := range fills {
		traded += f.Qty * f.Price
	}
	years := pts[len(pts)-1].Date.Sub(pts[0].Date).Hours() / 24 / 365.25
	if years > 0 && eq > 0 {
		m.Turnover = traded / (eq / float64(len(pts))) / years / 2
	}
}

type YearRow struct {
	Year        int     `json:"year"`
	Return      float64 `json:"return"`
	Benchmark   float64 `json:"benchmark"`
	MaxDrawdown float64 `json:"max_drawdown"`
}

// Yearly splits performance by calendar year.
func Yearly(pts []EquityPoint) []YearRow {
	var rows []YearRow
	if len(pts) == 0 {
		return rows
	}
	startEq, startBm := pts[0].Equity, pts[0].Benchmark
	peak := pts[0].Equity
	cur := YearRow{Year: pts[0].Date.Year()}
	for i, p := range pts {
		if p.Date.Year() != cur.Year {
			prev := pts[i-1]
			cur.Return, cur.Benchmark = prev.Equity/startEq-1, prev.Benchmark/startBm-1
			rows = append(rows, cur)
			startEq, startBm, peak = prev.Equity, prev.Benchmark, prev.Equity
			cur = YearRow{Year: p.Date.Year()}
		}
		if p.Equity > peak {
			peak = p.Equity
		}
		if dd := 1 - p.Equity/peak; dd > cur.MaxDrawdown {
			cur.MaxDrawdown = dd
		}
	}
	last := pts[len(pts)-1]
	cur.Return, cur.Benchmark = last.Equity/startEq-1, last.Benchmark/startBm-1
	return append(rows, cur)
}

func pct(x float64) string { return fmt.Sprintf("%6.1f%%", x*100) }

// Report writes a human-readable summary.
func (r *Result) Report(w io.Writer) {
	m, b := r.Metrics, r.Benchmark
	fmt.Fprintf(w, "Backtest %s → %s\n", m.Start.Format("2006-01-02"), m.End.Format("2006-01-02"))
	fmt.Fprintln(w, strings.Repeat("─", 52))
	fmt.Fprintf(w, "%-22s %12s %12s\n", "", "Strategy", "Benchmark")
	row := func(name string, a, bv string) { fmt.Fprintf(w, "%-22s %12s %12s\n", name, a, bv) }
	row("Start equity", fmt.Sprintf("%.0f", m.StartEquity), fmt.Sprintf("%.0f", b.StartEquity))
	row("End equity", fmt.Sprintf("%.0f", m.EndEquity), fmt.Sprintf("%.0f", b.EndEquity))
	row("Total return", pct(m.TotalReturn), pct(b.TotalReturn))
	row("CAGR", pct(m.CAGR), pct(b.CAGR))
	row("Annual volatility", pct(m.AnnVol), pct(b.AnnVol))
	row("Sharpe", fmt.Sprintf("%.2f", m.Sharpe), fmt.Sprintf("%.2f", b.Sharpe))
	row("Sortino", fmt.Sprintf("%.2f", m.Sortino), fmt.Sprintf("%.2f", b.Sortino))
	row("Max drawdown", pct(m.MaxDrawdown), pct(b.MaxDrawdown))
	row("Calmar", fmt.Sprintf("%.2f", m.Calmar), fmt.Sprintf("%.2f", b.Calmar))
	fmt.Fprintln(w, strings.Repeat("─", 52))
	fmt.Fprintf(w, "Round trips %d | win rate %.0f%% | avg win %.1f%% | avg loss %.1f%% | profit factor %.2f\n",
		m.Trades, m.WinRate*100, m.AvgWinPct*100, m.AvgLossPct*100, m.ProfitFactor)
	fmt.Fprintf(w, "Avg hold %.1f days | avg exposure %.0f%% | annual turnover %.1fx\n", m.AvgHoldDays, m.AvgExposure*100, m.Turnover)
	fmt.Fprintf(w, "Orders rejected by risk %d | unfilled limits %d | vetoed by reviewer %d\n", r.Rejections, r.Unfilled, r.Vetoed)
	if r.HaltedOn != nil {
		fmt.Fprintf(w, "⚠ Kill switch engaged on %s (hard drawdown); trading stopped\n", r.HaltedOn.Format("2006-01-02"))
	}
	fmt.Fprintln(w, "\nYear   Strategy  Benchmark   MaxDD")
	for _, y := range r.Yearly {
		fmt.Fprintf(w, "%d  %s    %s  %s\n", y.Year, pct(y.Return), pct(y.Benchmark), pct(y.MaxDrawdown))
	}
}
