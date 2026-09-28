# Runbook: backtest → paper trade → deploy to Robinhood

Follow the steps in order. Don't skip the paper-trading gate.

## 0. Prerequisites

| Need | Why | Where |
|---|---|---|
| Go 1.24+ | Build the bot | https://go.dev/dl |
| Alpaca account (free) | Market data, news, **paper trading** | alpaca.markets → Paper Trading → API keys |
| Anthropic API key | Claude entry reviews (a few cents per day) | console.anthropic.com |
| Robinhood account with **Agentic Trading** | Live execution | Robinhood app → Agentic Trading |
| Claude Code | MCP client for Robinhood (live only) | `npm install -g @anthropic-ai/claude-code` |
| An always-on machine | The daily 09:40 ET run | Small cloud VM, or a Mac mini / home server |

```bash
cd tradebot
make build test          # compiles and runs the test suite
cp deploy/env.example .env && chmod 600 .env   # fill in your keys
set -a && source .env && set +a
```

## 1. Smoke test (no keys needed)

```bash
make synth-backtest
```

This runs the whole pipeline on **synthetic** random data. The numbers are meaningless; it only proves the build works.

## 2. Backtest on real data

```bash
# For long history, use BIL as the defensive asset (SGOV only starts in 2020):
#   edit configs/config.yaml → defensive: BIL
make fetch        # downloads adjusted daily bars 2012→today into data/real
make backtest     # prints metrics + yearly table; writes results/real/{summary.json,equity.csv,trades.csv,fills.csv}
make sweep        # 81 parameter combos: in-sample ≤ 2019, out-of-sample 2020→today
```

If `fetch` returns 403 for SIP data, set `alpaca.data_feed: iex`.

**How to read the output:**
- Compare **Sharpe, max drawdown and Calmar** against the SPY benchmark column, not just CAGR.
- In the yearly table, check that the strategy protected capital in 2018 Q4, 2020 and 2022.
- In `sweep`, look at the **median out-of-sample Sharpe**. If only a few settings do well, the edge is fragile.
- For a survivorship-bias check, re-run with an ETF-only universe.

**Minimum bar to continue:**
- Out-of-sample Sharpe ≥ SPY's.
- Max drawdown clearly below SPY's.
- More than 100 round trips.

## 3. Paper trading (at least 8 weeks)

```bash
./bin/tradebot run --broker alpaca-paper     # one daily cycle; prints the plan, Claude verdicts and orders
./bin/tradebot status
```

**Schedule it** for every weekday at 09:40 New York time:
- **systemd:** copy the repo to `/opt/tradebot`, then:
  ```bash
  sudo cp deploy/systemd/tradebot.* /etc/systemd/system/
  sudo systemctl enable --now tradebot.timer
  ```
- **cron:** add `CRON_TZ=America/New_York` then `40 9 * * 1-5 cd /path/tradebot && set -a && . ./.env && ./bin/tradebot run --broker alpaca-paper >> state/cron.log 2>&1`.
- **Docker:** `docker build -t tradebot . && docker run --env-file .env -v $PWD/state:/app/state tradebot`.

**Every week:**
- Read `state/alpaca-paper/journal.jsonl` (plans, Claude verdicts, rejections).
- Compare the Alpaca paper equity curve with the backtest's typical behavior.

**Go/no-go for live money:**
- [ ] At least 8 weeks and at least 20 closed round trips.
- [ ] No unexplained orders, and no rejections caused by bugs.
- [ ] Slippage vs limit prices is in line with the backtest assumptions.
- [ ] Drawdown within the backtest's historical range.
- [ ] You have reviewed at least 10 Claude vetoes/reductions and agree they were reasonable.

## 4. Deploy to Robinhood

### 4.1 Create and fund the agentic account
In the Robinhood app, enable **Agentic Trading** and create the agentic account. **Fund it only with money you can afford to lose.** That balance is the hard ceiling on what the bot can lose. Start small; you can add more after a good month.

### 4.2 Connect Claude Code to Robinhood's MCP server
```bash
claude mcp add --scope user --transport http robinhood-trading https://agent.robinhood.com/mcp/trading
cd deploy/robinhood-executor
claude            # interactive once: run /mcp → robinhood-trading → Authenticate → finish OAuth in the browser
```
Headless runs need Claude Code itself to be authenticated: either log in once interactively, or set `ANTHROPIC_API_KEY` in `.env`.

### 4.3 Verify the tool names (required before live)
In the same interactive session, ask:
> "List every robinhood-trading MCP tool with its exact name and full input schema. Do not call any tool that places, modifies or cancels orders."

Then update `robinhood:` in `configs/config.yaml` so that:
- `order_tool_keywords` matches the order-placing tool names;
- `read_tool_keywords` matches the read-only tools;
- `fields` lists the exact argument names for symbol, side, quantity, limit price and order type.

Anything unrecognized is denied, so a mismatch fails safely. To have Claude do this and add tests for the real schemas, use Task 2 in [COMPLETE_BOT_PROMPT.md](COMPLETE_BOT_PROMPT.md).

### 4.4 Guard drill (no real orders)
```bash
./bin/tradebot run --broker robinhood --data alpaca --force   # with risk.max_orders_per_run: 1 for the first live day
```
Check `state/robinhood/journal.jsonl` for `"kind":"guard"` lines: read calls should be allowed and the order call matched to an intent. Confirm the order in the Robinhood app, where you also get a push notification per trade.

### 4.5 Schedule live runs
Change the systemd `ExecStart` (or your cron line) to `--broker robinhood`, and keep `claude` on the service user's `PATH`.

## 5. Daily operations

| Situation | Action |
|---|---|
| Normal day | Read the run summary (webhook or log). Robinhood also notifies each fill. |
| Something looks wrong | `./bin/tradebot halt --reason "investigating"`. This blocks buys; exits still run (set `risk.halt_allows_exits: false` to freeze everything). |
| Emergency | In the Robinhood app, **disconnect the agent**. This cuts MCP access immediately. |
| Resume after a halt or drawdown breaker | Find the cause first, then `./bin/tradebot resume`. |
| Re-run a failed day | `./bin/tradebot run --broker robinhood --force`. Order IDs are deterministic per day, but check the app for partial submissions first. |
| Change parameters | Edit `configs/config.yaml`, re-run `make backtest sweep`, and paper-trade the change before going live. |

## 6. Tax and regulatory notes (US)

- All holdings are held overnight or longer, so the bot does not day trade. A same-day run guard prevents accidental round trips.
- Frequent trading creates **short-term capital gains** and possible **wash sales** (mean-reversion re-entries within 30 days of a loss). Keep the broker's 1099 and talk to a tax professional.
- In a **cash account**, buying with unsettled sale proceeds and selling before settlement can trigger good-faith violations. Keep a cash buffer, or check your account type.
- This software is not investment advice. You are responsible for every order it places.

## 7. Troubleshooting

| Symptom | Fix |
|---|---|
| `alpaca: set APCA_API_KEY_ID…` | Load `.env` (`set -a; . ./.env; set +a`) |
| `llm: ANTHROPIC_API_KEY is not set` | Set it, or `llm.enabled: false` (not recommended live) |
| All orders `REJECTED … stale` | The data feed didn't return yesterday's bar; check the Alpaca status / feed setting |
| Robinhood `executor failed` | Run `claude` in `deploy/robinhood-executor` and re-authenticate via `/mcp` |
| Guard denies every order | Tool or field names differ from the config. Redo step 4.3 and inspect the `guard` lines in the journal |
| `implausible equity` | Claude mis-read the account. Nothing was traded; re-run with `--force`, or investigate |
