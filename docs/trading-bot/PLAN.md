# Agentic Trading Bot: Delivery Plan

Companion to [ARCHITECTURE.md](./ARCHITECTURE.md). No code is written until Phase 0 decisions are recorded.

## Decisions needed from you

| # | Decision | Recommended default |
|---|---|---|
| 1 | Trading style | Intraday-to-swing (5-min to daily bars), liquid large caps + ETFs |
| 2 | Asset classes at launch | Equities only; options in Phase 7; crypto after that |
| 3 | Live pilot capital | An amount you can afford to lose entirely (fund the Robinhood agentic sub-account with just that) |
| 4 | Fast judge | Jev + a local LightGBM model, compared head to head |
| 5 | Human approval | Every order for the first 20 live trades, then only above a notional threshold |
| 6 | Hosting | One small cloud VM with Docker Compose |
| 7 | Alerts/approvals channel | Telegram or Slack |

## Phases

### Phase 0: Discovery and decisions (week 1)
- Open the Robinhood Agentic account and connect with `claude mcp add robinhood-trading --transport http https://agent.robinhood.com/mcp/trading`.
- **Enumerate the MCP tools**: schemas, order types (limit/stop?), quotes, rate limits.
- Test OAuth from a plain Python MCP client. **This decides Option A vs B** (ARCHITECTURE §7.4).
- Get API keys: Anthropic, OpenRouter (Jev), Alpaca (data + paper).
- Run a Jev smoke test on about 10 hand-built state snapshots.
- Draft `risk.yaml` v0 and the strategy one-pager.
- **Exit:** ADRs (architecture decision records) written for the execution path, data vendor, strategy and fast judge.

### Phase 1: Foundations (weeks 2–3)
- Repo skeleton, domain types, config loading, Postgres schema, append-only audit log.
- Market data ingest, feature builder, market-calendar scheduler.
- **Exit:** watchlist bars and features are stored continuously, and a day can be replayed from the DB.

### Phase 2: Rules baseline and backtest (weeks 3–5)
- T0 setups and deterministic exits.
- vectorbt research notebooks.
- Event-driven replay harness and simulator broker; metrics report.
- **Exit:** T0-only baseline with documented out-of-sample results. This is the bar the LLM tiers must beat.

### Phase 3: Decision providers and agents (weeks 5–7)
- `DecisionProvider` interface, with `JevProvider`, `LocalModelProvider` and `HaikuProvider`.
- Claude strategist, trader and news-triage agents with the tool catalog. Versioned prompts, decision cache, cost meter.
- Agent eval suite: golden scenarios (hallucinated ticker, no-trade list, stale data, injected news, risk-dial sizing).
- **Exit:** the eval suite passes, and a post-training-cutoff replay compares T0 vs T0+T1 vs T0+T1+T2.

### Phase 4: Risk engine and execution (weeks 6–8, overlaps Phase 3)
- Risk engine with property-based tests.
- Execution adapters: simulator, Alpaca paper, and a Robinhood MCP dry-run.
- Order state machine, reconciler, kill switch, dead-man heartbeat, MCP tool-schema pinning.
- Approvals bot.
- **Exit:** chaos tests all **fail closed**: broker timeout, duplicate submit, partial fill, stale quote, MCP schema drift, process crash mid-order.

### Phase 5: Paper trading (at least 4–8 weeks)
- The full system on Alpaca paper with live data. Daily auto-report and a weekly Opus review.
- **Go/no-go for live:**
  - At least 40 closed trades.
  - Positive expectancy **after** LLM and slippage costs.
  - Max drawdown within limits.
  - Zero risk-engine bypasses and zero unresolved reconciliation mismatches.
  - Beats the T0-only baseline.

### Phase 6: Live pilot on Robinhood (4 weeks)
- Small funded agentic budget; equities only; human approval on every order at first.
- **Exit:** live slippage and hit rate within tolerance of paper; no operational incidents.

### Phase 7: Scale and extend (ongoing)
- Raise the budget gradually, only after green monthly reviews.
- Add defined-risk options, then crypto (Robinhood MCP or Crypto API).
- Monthly model and prompt evals before any model upgrade. Consider a TradingAgents-style bull/bear debate in T3.

## Milestone checklist

- [ ] Phase 0 ADRs approved
- [ ] Data pipeline live
- [ ] T0 baseline report
- [ ] Agents pass eval suite
- [ ] Risk/execution chaos tests green
- [ ] Paper go/no-go passed
- [ ] Live pilot complete
