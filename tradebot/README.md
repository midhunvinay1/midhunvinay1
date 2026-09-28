# tradebot

A **Go** trading bot for **Robinhood** with two modes:
- **Swing** (`tradebot run`, once a day): regime-filtered momentum plus mean reversion, with a volatility target.
- **Intraday** (`tradebot day`, short-term): a 5-minute opening-range breakout on "stocks in play", with ~2 s decisions, always flat by the close.

In both modes, deterministic code decides the trades. **Claude** can only shrink or veto them (per entry in swing mode, once before the open in intraday mode). A hard **risk engine** and an **execution guard** protect the funded agentic account. Orders go to Robinhood's official MCP server, either directly (`robinhood-native`, no LLM in the order path) or through Claude Code.

```text
  Alpaca data ─► strategy / ORB engine ─► orders ─► Claude (≤1×) ─► risk engine ─► guard ─► Robinhood MCP
                         ▲                                                            (native client or Claude Code)
                  backtests run the same engine code
```

> Real HFT (microsecond, co-located) is not possible from a retail Robinhood account. [docs/INTRADAY.md](docs/INTRADAY.md) explains why, and what the intraday mode does instead.

| Doc | What's inside |
|---|---|
| [docs/STRATEGY.md](docs/STRATEGY.md) | Which strategies were considered, the rules, the evidence, honest expectations |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Components, daily cycle, Robinhood execution design, safety layers, why Go |
| [docs/INTRADAY.md](docs/INTRADAY.md) | Short-term mode: ORB rules, latency, why not real HFT, base rates, PDT/settlement |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | Step by step: backtest → paper trading → deploy to Robinhood → daily ops |
| [docs/COMPLETE_BOT_PROMPT.md](docs/COMPLETE_BOT_PROMPT.md) | Prompts for Claude Code to finish the parts that need your accounts |

## Quick start

```bash
make build test            # build + unit tests (incl. no-look-ahead, guard, OAuth and live-loop tests)
make synth-backtest        # pipeline smoke test on synthetic data (numbers are meaningless)

set -a; . ./.env; set +a   # APCA_API_KEY_ID, APCA_API_SECRET_KEY, ANTHROPIC_API_KEY
make fetch backtest sweep  # real data: backtest + in-sample/out-of-sample parameter sweep
./bin/tradebot run --broker alpaca-paper     # paper trading (schedule daily at 09:40 ET)
./bin/tradebot run --broker robinhood        # live, only after the RUNBOOK go/no-go
./bin/tradebot halt | resume | status        # kill switch

make synth-intraday                          # intraday pipeline smoke test (synthetic minutes)
make fetch-intraday backtest-intraday        # intraday on real minute data
./bin/tradebot day --broker alpaca-paper     # one intraday session (09:15–16:00 ET)
./bin/tradebot rh-login && ./bin/tradebot rh-tools && ./bin/tradebot doctor
./bin/tradebot day --broker robinhood-native # live intraday, only after the paper go/no-go
```

## Commands

| Command | Purpose |
|---|---|
| `fetch` | Download split-adjusted daily bars from Alpaca to CSV |
| `synth` | Generate synthetic data (pipeline tests only) |
| `backtest` | Simulate with the live code path; writes `summary.json`, `equity.csv`, `trades.csv`, `fills.csv` |
| `sweep` | Parallel 81-combination grid, ranked in-sample, reported out-of-sample |
| `run` | One daily swing cycle with `dry-run`, `alpaca-paper`, `robinhood` or `robinhood-native` |
| `halt` / `resume` / `status` | Kill switch and state |
| `hook pretooluse` | Claude Code guard (invoked by `deploy/robinhood-executor/.claude/settings.json`) |
| `fetch-intraday` / `synth-intraday` / `backtest-intraday` | Minute data and the intraday ORB backtest |
| `day` | One intraday session with `dry-run`, `alpaca-paper` or `robinhood-native` |
| `rh-login` / `rh-tools` | Native Robinhood MCP: one-time OAuth login; list tools and schemas |
| `doctor` | Pre-flight checks: keys, data, calendar, Claude Code, Robinhood login and tool names |

> Not investment advice. Trading involves risk of loss. Fund the Robinhood agentic account only with money you can afford to lose.
