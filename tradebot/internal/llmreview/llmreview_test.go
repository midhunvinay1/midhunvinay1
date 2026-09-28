package llmreview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/llm"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
	"github.com/midhunvinay1/tradebot/internal/runner"
	"github.com/midhunvinay1/tradebot/internal/strategy"
)

type fake struct {
	v   llm.Verdict
	err error
}

func (f fake) Review(context.Context, llm.Candidate) (llm.Verdict, error) { return f.v, f.err }

func entry() runner.Entry {
	var s market.Series
	d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 260; i++ {
		s = append(s, market.Bar{Date: d.AddDate(0, 0, i), Open: 100, High: 101, Low: 99, Close: 100 + float64(i%3), Volume: 1e6})
	}
	return runner.Entry{Order: portfolio.Order{Symbol: "AAPL", Qty: 10}, Plan: &strategy.Plan{Date: d, Sleeve: map[string]string{}, Reasons: map[string]string{}},
		Data: market.Dataset{"AAPL": s}, Now: d}
}

func TestPolicies(t *testing.T) {
	cfg := config.Defaults().LLM
	ctx := context.Background()
	if m, _ := (&Reviewer{Cfg: cfg, LLM: fake{v: llm.Verdict{Decision: "reduce", SizeMultiplier: 0.5}}}).ReviewEntry(ctx, entry()); m != 0.5 {
		t.Fatalf("reduce → %v", m)
	}
	if m, _ := (&Reviewer{Cfg: cfg, LLM: fake{err: errors.New("down")}}).ReviewEntry(ctx, entry()); m != 0 {
		t.Fatalf("on_error skip must fail closed, got %v", m)
	}
	cfg.OnError = "allow"
	if m, _ := (&Reviewer{Cfg: cfg, LLM: fake{err: errors.New("down")}}).ReviewEntry(ctx, entry()); m != 1 {
		t.Fatalf("on_error allow → %v", m)
	}
	cfg.OnError, cfg.MaxCallsPerRun = "skip", 1
	r := &Reviewer{Cfg: cfg, LLM: fake{v: llm.Verdict{Decision: "approve"}}}
	r.ReviewEntry(ctx, entry())
	if m, _ := r.ReviewEntry(ctx, entry()); m != 0 {
		t.Fatalf("over budget must use the on_error policy, got %v", m)
	}
}
