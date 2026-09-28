// Package llmreview adapts the Claude entry reviewer (internal/llm) to the
// runner's optional EntryReviewer hook. Only the `tradebot` binary links it;
// `trendbot` does not, so it contains no LLM code at all.
package llmreview

import (
	"context"
	"fmt"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/journal"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/runner"
)

// NewsFunc fetches recent headlines for a symbol.
type NewsFunc func(ctx context.Context, symbol string, since time.Time, limit int) ([]market.News, error)

// Reviewer reviews new entries with Claude, within a per-run call budget,
// applying the configured on_error policy (fail closed by default).
type Reviewer struct {
	Cfg     config.LLMConfig
	LLM     llm.Reviewer
	News    NewsFunc
	Journal *journal.Journal
	calls   int
}

func (r *Reviewer) ReviewEntry(ctx context.Context, e runner.Entry) (float64, string) {
	o := e.Order
	fallback := 0.0
	if r.Cfg.OnError == "allow" {
		fallback = 1
	}
	if r.calls >= r.Cfg.MaxCallsPerRun {
		return fallback, fmt.Sprintf("%s: review budget exhausted → multiplier %.2f", o.Symbol, fallback)
	}
	r.calls++
	var news []market.News
	if r.News != nil {
		var err error
		if news, err = r.News(ctx, o.Symbol, e.Now.AddDate(0, 0, -r.Cfg.NewsLookbackDays), r.Cfg.MaxHeadlines); err != nil {
			_ = r.Journal.Log("news_error", map[string]any{"symbol": o.Symbol, "error": err.Error()})
		}
	}
	cand := llm.BuildCandidate(e.Plan.Date, o.Symbol, e.Plan.Sleeve[o.Symbol], e.Plan.Reasons[o.Symbol], o.TargetWeight, e.Data[o.Symbol], e.Plan.RiskOn, e.Holdings, news)
	v, err := r.LLM.Review(ctx, cand)
	if err != nil {
		_ = r.Journal.Log("llm_error", map[string]any{"symbol": o.Symbol, "error": err.Error()})
		return fallback, fmt.Sprintf("%s: review failed (%v) → multiplier %.2f", o.Symbol, err, fallback)
	}
	_ = r.Journal.Log("llm_verdict", map[string]any{"candidate": cand, "verdict": v})
	return v.Multiplier(), fmt.Sprintf("%s: Claude %s ×%.2f %v — %s", o.Symbol, v.Decision, v.Multiplier(), v.RiskFlags, v.Rationale)
}
