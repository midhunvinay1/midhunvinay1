package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/midhunvinay1/tradebot/internal/broker"
	"github.com/midhunvinay1/tradebot/internal/config"
	"github.com/midhunvinay1/tradebot/internal/data"
	"github.com/midhunvinay1/tradebot/internal/market"
	"github.com/midhunvinay1/tradebot/internal/portfolio"
	"github.com/midhunvinay1/tradebot/internal/strategy"
)

type memSource struct{ ds market.Dataset }

func (m memSource) Bars(context.Context, []string, time.Time, time.Time) (market.Dataset, error) {
	return m.ds, nil
}
func (memSource) LatestPrices(context.Context, []string) (map[string]float64, error) { return nil, nil }
func (memSource) News(context.Context, string, time.Time, int) ([]market.News, error) {
	return nil, nil
}
func (memSource) IsTradingDay(context.Context, time.Time) (bool, error) { return true, nil }

type recBroker struct {
	broker.DryRun
	got []portfolio.Order
}

func (r *recBroker) Submit(ctx context.Context, o []portfolio.Order) ([]broker.Result, error) {
	r.got = append(r.got, o...)
	return r.DryRun.Submit(ctx, o)
}

type vetoAll struct{ calls int }

func (v *vetoAll) ReviewEntry(context.Context, Entry) (float64, string) {
	v.calls++
	return 0, "veto (test)"
}

// setup returns synthetic data ending the day before `now` whose last
// decision produces new entries.
func setup(t *testing.T) (*config.Config, market.Dataset, time.Time) {
	cfg := config.Defaults()
	cfg.LLM.Enabled = false
	now := time.Date(2026, 9, 28, 13, 40, 0, 0, time.UTC) // 09:40 New York
	ds := data.Synthetic(cfg.Symbols(), cfg.Benchmark, time.Date(2014, 9, 29, 0, 0, 0, 0, time.UTC), 12, 11)
	return cfg, ds, now
}

func TestRunPlacesApprovedOrdersOncePerDay(t *testing.T) {
	cfg, ds, now := setup(t)
	b := &recBroker{DryRun: broker.DryRun{Equity: 10000}}
	var out bytes.Buffer
	r := &Runner{Cfg: cfg, StateDir: t.TempDir(), Broker: b, Data: memSource{ds}, NewPlanner: SwingPlanner(cfg),
		Now: func() time.Time { return now }, Out: &out}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.got) == 0 {
		t.Fatalf("expected orders:\n%s", out.String())
	}
	for _, o := range b.got {
		if o.Side != portfolio.Buy || o.LimitPrice <= 0 || !strings.HasPrefix(o.ID, "tb-20260928-") {
			t.Fatalf("unexpected order %+v", o)
		}
	}
	n := len(b.got)
	out.Reset()
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.got) != n || !strings.Contains(out.String(), "Already ran") {
		t.Fatalf("second run on the same day must do nothing:\n%s", out.String())
	}
}

func TestClaudeVetoDropsEntries(t *testing.T) {
	cfg, ds, now := setup(t)
	cfg.LLM.Enabled = true
	b := &recBroker{DryRun: broker.DryRun{Equity: 10000}}
	v := &vetoAll{}
	var out bytes.Buffer
	r := &Runner{Cfg: cfg, StateDir: t.TempDir(), Broker: b, Data: memSource{ds}, NewPlanner: SwingPlanner(cfg), Reviewer: v,
		Now: func() time.Time { return now }, Out: &out}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v.calls == 0 {
		t.Fatal("reviewer was not consulted")
	}
	for _, o := range b.got {
		if o.Kind == portfolio.KindEntry {
			t.Fatalf("vetoed entry was still sent: %+v", o)
		}
	}
	st, err := LoadState(r.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var ss strategy.State
	if err := json.Unmarshal(st.Strategy, &ss); err != nil {
		t.Fatal(err)
	}
	if len(ss.MeanRev) != 0 {
		t.Fatal("vetoed mean-reversion entries must be dropped from strategy state")
	}
}
