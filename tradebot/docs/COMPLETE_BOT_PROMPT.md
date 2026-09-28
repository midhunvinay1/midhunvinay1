# Prompts to finish the bot with Claude Code

The bot is functional: strategy, risk engine, backtester, sweep, Alpaca paper trading, the Robinhood executor with its guard, and tests. The tasks below need **your** credentials, data or accounts, so they could not be finished in the build sandbox.

How to use this file:
- Open Claude Code in the `tradebot/` directory.
- Paste the **Master prompt** first, then one task prompt at a time.
- Review each diff before accepting it.

---

## Master prompt (paste first)

```text
You are working on `tradebot`, a Go swing-trading bot in this directory. Read docs/ARCHITECTURE.md, docs/STRATEGY.md and docs/RUNBOOK.md before changing anything.

Non-negotiable invariants. Never weaken these, and add tests when you touch related code:
1. The deterministic strategy decides trades. Claude (internal/llm) may only approve, reduce or veto NEW entries; Verdict.Multiplier() must stay clamped to [0,1].
2. internal/risk is the final gate for every order in backtest, paper and live. No code path may submit an order that did not pass risk.Check.
3. Backtest and live use the same strategy/portfolio/risk functions. Decisions only see data.Until(decisionDate). TestNoLookAhead must keep passing.
4. Robinhood orders go only through the PreToolUse guard (internal/hook). Unknown tools are denied. No options, shorts, margin, dollar-amount or market orders.
5. Fail closed on any error. No secrets in code, logs, prompts or git.

Workflow for every task:
- Make a short plan and show it to me.
- Implement in small commits, with `gofmt`, `go vet ./...` and `go test ./...` passing.
- Update the docs you affect.
- Report exactly what you verified and what you could not.
```

---

## Task 1: Real-data backtest and robustness report

```text
Load my environment (.env) and run: make fetch (start 2012-01-01, set defensive: BIL for the long history), make backtest, make sweep (--split 2020-01-01).
Then:
- Run the same backtest with an ETF-only universe (SPY QQQ IWM DIA the XL* sector ETFs SMH GLD TLT) to measure survivorship bias.
- Run each sleeve alone (momentum weight 1.0 / mean-reversion weight 1.0) and the combination.
- Run with slippage_bps 5, 15 and 30 to test cost sensitivity.
Write docs/BACKTEST_REPORT.md with:
- the tables;
- the out-of-sample vs in-sample comparison;
- the median and worst out-of-sample Sharpe across the sweep;
- a recommendation for parameters from a broad stable plateau (not the best row).
Do NOT change defaults based on out-of-sample results; only recommend. Flag anything that looks too good to be true.
```

## Task 2: Robinhood MCP discovery and guard hardening

```text
Goal: make the Robinhood execution guard match the REAL robinhood-trading MCP tools.
1. Start `claude` interactively in deploy/robinhood-executor, list all robinhood-trading tools with exact names and full JSON input schemas. Do NOT call any tool that places, modifies or cancels orders. Save them to docs/robinhood_tools.json (no account data).
2. Update configs/config.yaml → robinhood.order_tool_keywords / read_tool_keywords / deny_keywords / fields / forbidden_input_keys to match exactly. Prefer exact tool names over keywords if the schema allows (add an `order_tools` / `read_tools` exact-name allowlist to config and hook.Evaluate, keeping keywords as a fallback that still denies unknown tools).
3. Add table tests in internal/hook using the real schemas: the approved order passes; wrong qty, price, side or symbol, market orders, option fields and every write tool other than the order tool are denied.
4. If the order tool supports time_in_force / extended hours, require DAY and regular hours in the guard.
5. Update the executor prompt in internal/broker/robinhood.go to name the exact tool and argument names.
6. Add a read-only `tradebot rh-orders` command that asks the executor for today's order statuses (JSON), and have the runner reconcile it at the start of the next run: log any order whose status differs from what we submitted.
```

## Task 3: Native MCP client (removes the LLM from execution)

```text
Investigate whether Robinhood's Agentic Trading MCP OAuth flow works with a custom (non-Claude-Code) client. If it does, implement broker.RobinhoodNative using the official Go MCP SDK (github.com/modelcontextprotocol/go-sdk):
- one-time interactive OAuth login command (`tradebot rh-login`) storing refresh tokens encrypted at rest (0600, key from env);
- Account() and Submit() calling the MCP tools directly with the exact schemas from Task 2, running the same intent checks as internal/hook in-process before each call;
- idempotency (never blind-retry a submit; query order status first);
- tests with an in-memory fake MCP server.
Keep the Claude Code executor as a fallback selectable by config. If OAuth for custom clients is not supported, write up the findings and stop.
```

## Task 4: Deterministic earnings blackout

```text
Add an earnings-calendar data source (evaluate Alpaca corporate actions/calendar availability, or a free/cheap provider; document limits and cost). Add strategy config `earnings_blackout_days` (default 2):
- no NEW mean-reversion entries if earnings fall within N trading days;
- momentum entries within N days get size ×0.5.
It must be point-in-time in backtests (use announcement dates known at decision time, or disable in backtest with a clear note). Add tests. Keep Claude's news review as a second layer.
```

## Task 5: Survivorship-bias-free universe (optional, for research)

```text
Add support for a point-in-time universe file (CSV: date,symbol for membership changes, e.g. S&P 100 history). The backtest only allows symbols that were members at the decision date; live uses the latest membership. Add a data-loading test and re-run Task 1's stock-universe backtest to quantify the bias.
```

## Task 6: Weekly Claude review and performance tracking

```text
Add `tradebot report --broker <name> [--weeks 1]`:
- Rebuild the live equity curve and closed trades from journal.jsonl and account snapshots.
- Compute the same metrics as the backtest (Sharpe, drawdown, win rate, slippage vs limit and vs next open).
- For every Claude veto/reduction, compute the hypothetical outcome of the unvetoed trade over its typical holding period, to measure whether the overlay helps.
- Send a compact summary to Claude (same SDK patterns as internal/llm: structured output, cached system prompt, refusal fallbacks, fail closed) and ask for a critique and anomalies. It is advisory only: it must not change config or state.
Write the report to state/<broker>/reports/YYYY-MM-DD.md and post a summary to the webhook.
```

## Task 7: Pre-live hardening checklist

```text
Audit the codebase for live readiness and fix the gaps:
- context timeouts on every network call;
- retries with backoff only for idempotent reads;
- clean handling of partial fills and orders still open from yesterday (cancel via guard-approved intents only if needed);
- a file lock so two `tradebot run` processes can never overlap;
- structured logging;
- `tradebot doctor`, which checks keys, the data feed, the calendar, Claude Code auth, the MCP connection (read-only) and clock/timezone.
Add tests where possible. Then re-run `go test -race ./...` and the synthetic smoke test.
```
