# Prompts to finish the bot with Claude Code

## Status

| # | Task | Status |
|---|---|---|
| 3 | Native MCP client (no LLM in the order path) | ✅ **Done**: `robinhood-native`, `rh-login`, `rh-tools`, with an OAuth flow test |
| 7 | Pre-live hardening | ✅ **Mostly done**: single-instance lock, `doctor`, read retries with backoff, per-call timeouts, order status/cancel. **Remaining:** structured logging |
| — | Short-term / intraday mode | ✅ **Done**: ORB engine, minute backtester, live loop (`tradebot day`), pre-market Claude review. See [INTRADAY.md](INTRADAY.md) |
| 1 | Real-data backtests (both modes) | ⏳ Needs your Alpaca keys (market data was blocked in the build sandbox) |
| 2 | Map the real Robinhood tools | ⏳ Needs your Robinhood login; now a 10-minute job with `rh-tools` |
| 4 | Deterministic earnings blackout | ⏳ Needs an earnings-calendar source |
| 5 | Survivorship-free universe | ⏳ Optional research |
| 6 | Weekly report and Claude critique | ⏳ |
| 8 | Streaming quotes (websocket) for intraday | ⏳ |
| 9 | Intraday parameter sweep and walk-forward | ⏳ |

How to use this file:
- Open Claude Code in `tradebot/`.
- Paste the **Master prompt**, then one task at a time.
- Review every diff.

---

## Master prompt (paste first)

```text
You are working on `tradebot`, a Go trading bot in this directory with two modes: swing (`tradebot run`) and intraday opening-range breakout (`tradebot day`). Read docs/ARCHITECTURE.md, docs/STRATEGY.md, docs/INTRADAY.md and docs/RUNBOOK.md before changing anything.

Non-negotiable invariants. Never weaken these, and add tests when you touch related code:
1. Deterministic code decides trades. Claude (internal/llm) may only approve, reduce or veto; every multiplier stays clamped to [0,1]. Claude is never called inside the intraday price loop.
2. internal/risk (plus the intraday engine's own limits) gates every order in backtest, paper and live. No code path may submit an order that skipped them.
3. Backtests and live trading share the same strategy/engine code and only see data available at decision time. TestNoLookAhead and TestBacktestDeterministicAndNoLookAhead must keep passing.
4. Robinhood orders pass the guard (internal/hook), whether through Claude Code or in-process in the native broker. Unknown tools are denied. No options, shorts, margin, dollar-amount or market orders. The native broker cancels only orders it placed.
5. Intraday positions are always flat by the close; the intraday session never sells positions it did not open.
6. Fail closed on any error. No secrets in code, logs, prompts or git.

Workflow for every task:
- Show a short plan first.
- Implement in small commits, with `gofmt`, `go vet ./...` and `go test -race ./...` passing.
- Update the docs you affect.
- Report exactly what you verified and what you could not.
```

---

## Task 1: Real-data backtests and robustness report (both modes)

```text
Load .env. Swing mode: make fetch (start 2012-01-01, defensive: BIL), make backtest, make sweep (--split 2020-01-01), plus an ETF-only universe run and slippage 5/15/30 bps.
Intraday: make fetch-intraday (start 2023-01-01) then make backtest-intraday with slippage_bps 3, 5 and 10; also run it separately for each calendar half-year.
Write docs/BACKTEST_REPORT.md with the tables, in-sample vs out-of-sample, cost sensitivity, and a recommendation from broad stable parameter regions. Do not change defaults based on out-of-sample results. Flag anything that looks too good to be true (e.g. intraday Sharpe > 3 usually means a fill-model problem).
```

## Task 2: Map the real Robinhood tools

```text
Run `./bin/tradebot rh-tools` (after `rh-login`). Using state/robinhood-native/tools.json:
1. Fill robinhood.native.{account_tool,positions_tool,place_order_tool,order_status_tool,cancel_order_tool}, order_args, fixed_args (limit + day time-in-force), numbers_as_strings and result_keys in configs/config.yaml.
2. Align robinhood.order_tool_keywords / read_tool_keywords / deny_keywords / fields with the real names, so the guard classifies every real tool correctly.
3. Add tests in internal/broker and internal/hook that use recorded (anonymized) example responses and the real schemas: account parsing, order args, status parsing, and denial of every write tool other than place/cancel.
4. Run `./bin/tradebot doctor` until every check passes. Then do a guard drill: swing `run --broker robinhood-native` with risk.max_orders_per_run: 1 and the smallest possible position, and confirm the order in the Robinhood app.
Never place orders outside that single drill, and never call cancel/transfer tools manually.
```

## Task 4: Deterministic earnings blackout

```text
Add an earnings-calendar source (evaluate Alpaca corporate actions and a free/cheap provider; document limits and cost). Add `earnings_blackout_days` (default 2) for swing mean-reversion entries (skip) and momentum entries (×0.5).
For intraday: skip symbols whose earnings release is scheduled DURING today's session; pre-market releases are fine, since those are the catalysts.
It must be point-in-time in backtests, or disabled there with a clear note. Add tests.
```

## Task 5: Survivorship-free universe (optional)

```text
Support a point-in-time universe file (CSV date,symbol membership changes). Backtests only trade names that were members at the decision date. Quantify the bias against Task 1.
```

## Task 6: Weekly report and Claude critique

```text
Add `tradebot report --broker <name> [--weeks 1]` for both modes:
- Rebuild the equity curve and trades from the journals.
- Compare against the backtest's expectations (win rate, avg R, slippage vs limit).
- For each Claude veto/reduction, compute what the trade would have done.
- Ask Claude for an advisory critique using the patterns in internal/llm (structured output, cached system prompt, refusal fallback, fail closed). The critique never changes config or state.
- Write the report to state/<dir>/reports/YYYY-MM-DD.md and post a summary to the webhook.
```

## Task 8: Streaming quotes for intraday

```text
Add an Alpaca market-data websocket feed (trades for the watchlist, feed from config: iex or sip) implementing intraday.Feed.LatestQuotes from an in-memory cache.
- Reconnect with backoff.
- Detect staleness: no update within max_quote_age_seconds means stale.
- Fall back to REST polling if the stream is down.
Keep the engine unchanged (OnPrice). Add tests with a fake websocket server. Measure and log quote-to-order latency in the journal.
```

## Task 9: Intraday parameter sweep and walk-forward

```text
Add `tradebot sweep-intraday`: a parallel grid over opening_range_minutes {5,15,30}, stop_atr_fraction {0.05,0.10,0.20}, min_rvol {1,1.5,2}, top_n_in_play {5,10,20}, and take_profit_r {0,3}.
- Rank on an in-sample window and report out-of-sample, like backtest.Sweep.
- Reuse loaded minute data across runs (load once, share read-only).
- Print the median and worst out-of-sample results.
```

## Remaining hardening (from Task 7)

```text
Replace fmt-based logging with log/slog (JSON to state/<dir>/tradebot.log, text to stdout). Keep journal.jsonl as the audit record. Include order IDs and latencies in every order/fill log line.
```
