# tradebot

A medium-risk swing-trading bot for **Robinhood** in **Go**. How it works:
- An evidence-based **momentum + mean-reversion** strategy with a market-regime filter and a volatility target decides the trades.
- **Claude** reviews each new entry for event risk and may only shrink or veto it.
- A hard **risk engine** and an **execution guard** protect the funded agentic account.

```text
  data (Alpaca) ─► strategy ─► orders ─► Claude review (≤1×) ─► risk engine ─► executor ─► Robinhood MCP
                      ▲                                                           │
                  backtest (same code, next-open limit fills)          PreToolUse guard: only approved orders
```

| Doc | What's inside |
|---|---|
| [docs/STRATEGY.md](docs/STRATEGY.md) | Which strategies were considered, the rules, the evidence, honest expectations |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components, daily cycle, Robinhood execution design, safety layers, why Go |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | Step by step: backtest → paper trading → deploy to Robinhood → daily ops |
| [docs/COMPLETE_BOT_PROMPT.md](docs/COMPLETE_BOT_PROMPT.md) | Prompts for Claude Code to finish the parts that need your accounts |

## Quick start

```bash
make build test            # build + unit tests (incl. no-look-ahead and guard tests)
make synth-backtest        # pipeline smoke test on synthetic data (numbers are meaningless)

set -a; . ./.env; set +a   # APCA_API_KEY_ID, APCA_API_SECRET_KEY, ANTHROPIC_API_KEY
make fetch backtest sweep  # real data: backtest + in-sample/out-of-sample parameter sweep
./bin/tradebot run --broker alpaca-paper     # paper trading (schedule daily at 09:40 ET)
./bin/tradebot run --broker robinhood        # live, only after the RUNBOOK go/no-go
./bin/tradebot halt | resume | status        # kill switch
```

## Commands

| Command | Purpose |
|---|---|
| `fetch` | Download split-adjusted daily bars from Alpaca to CSV |
| `synth` | Generate synthetic data (pipeline tests only) |
| `backtest` | Simulate with the live code path; writes `summary.json`, `equity.csv`, `trades.csv`, `fills.csv` |
| `sweep` | Parallel 81-combination grid, ranked in-sample, reported out-of-sample |
| `run` | One daily cycle with `dry-run`, `alpaca-paper` or `robinhood` |
| `halt` / `resume` / `status` | Kill switch and state |
| `hook pretooluse` | Claude Code guard (invoked by `deploy/robinhood-executor/.claude/settings.json`) |

> Not investment advice. Trading involves risk of loss. Fund the Robinhood agentic account only with money you can afford to lose.
