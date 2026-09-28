# Short-term mode: intraday opening-range breakout

`tradebot day` is the short-term mode. It trades breakouts during the session and is **always flat by 15:55 ET**. It sits alongside the swing mode (`tradebot run`).

## 1. "HFT" on Robinhood: what is and isn't possible

True high-frequency trading (HFT) is **not achievable** from a retail Robinhood account, and a bot that tries will lose money to spreads and latency. It's worth being explicit about why:

| What HFT firms have | What a Robinhood agentic account has |
|---|---|
| Co-located servers, order round trips in **microseconds** | Internet → Robinhood API → Robinhood routes the order to a market maker: **~100 ms–1 s+** |
| Direct market access, full order book (L2/L3) feeds | Limit/market orders only, no order-book data |
| Exchange rebates, sub-penny economics | Retail spread costs on every round trip |
| Strategies: market making, latency arbitrage | These are the firms that *fill* retail orders (payment for order flow) |

What this bot does instead is the fastest approach that has a documented edge **and** survives retail execution costs:
- intraday **momentum breakouts on "stocks in play"**;
- decisions evaluated every ~2 seconds;
- orders sent directly over Robinhood's official MCP API, with no LLM in the order path;
- all positions closed the same day.

| Stage | Latency |
|---|---|
| Price poll (Alpaca latest trades) | every 2 s (`poll_seconds`) |
| Engine decision (Go, in memory) | microseconds |
| Order submit, **native MCP** (`robinhood-native`) | network round trip to Robinhood (measure yours with `tradebot doctor` + the journal) |
| Order submit, Claude Code executor (`robinhood`) | 10–30 s. **Not allowed intraday** (the command refuses it) |
| Claude | **Not in the loop.** One batched pre-market review at ~09:15 ET |

## 2. Strategy: 5-minute opening-range breakout (ORB), long only

This is based on Zarattini & Aziz (2023), *"Can Day Trading Really Be Profitable?"*, which reports strong results for ORB on high-relative-volume "stocks in play". This bot adapts it to **long-only and no leverage**; the paper used both longs and shorts plus leverage, so expect lower returns.

1. **Before the open (09:15 ET):**
   - Compute each symbol's 14-day daily ATR and its average first-5-minute volume, using prior sessions only.
   - Claude reviews overnight news in **one batched call** and can reduce or veto symbols (e.g. an acquisition at a fixed cash price, a trading halt, a priced offering, a binary FDA ruling during the session).
2. **09:30–09:35:** build the opening range (OR).
   - **In play** = OR volume ≥ 1.0 × its 14-day average (RVOL), a bullish first candle, price ≥ $5, and ATR ≥ $0.50.
   - Keep the top 10 by RVOL.
3. **Entry:** the first trade above the OR high (+1¢). The order is a limit at +0.10%, cancelled if unfilled after 20 s. One attempt per symbol per day.
4. **Stop:** entry − 10% of the daily ATR, re-anchored to the actual fill price.
5. **Size:**
   - 1% of equity at risk per trade;
   - at most 25% of equity per position and 4 positions;
   - cash accounts use **settled cash only**.
6. **Exit:** the stop, an optional target or breakeven stop (off by default), or **flatten at 15:55**.
7. **Daily breakers:**
   - −2% on the day → flatten and stop for the day;
   - at most 8 entries a day;
   - no new entries after 15:00;
   - the global kill switch (`tradebot halt`) flattens immediately.

**Expected profile:** a low win rate (~15–30%) with winners several times larger than losers. Returns depend on a handful of trend days, so expect long flat or slightly losing stretches.

## 3. Base rates: read before trading real money

- Most retail day traders lose money:
  - Barber, Lee, Liu & Odean (2014): fewer than 1% of Taiwanese day traders are predictably profitable after costs.
  - Chague, De-Losso & Giovannetti (2019): 97% of Brazilian individuals who day traded futures for more than 300 days lost money.
- ORB is one of the few published intraday rules with positive results, but it is **cost-sensitive**. Run `backtest-intraday` with `slippage_bps` of 3, 5 and 10. If the edge disappears at 5–10 bps, don't go live.
- Free real-time data from Alpaca is **IEX-only** (a small share of volume). The last trade can lag the true market, which hurts breakout timing. For serious use, a SIP real-time feed (paid) is recommended: set `alpaca.price_feed: sip`.
- **Minute-bar backtests are optimistic.** Real fills, queue position and latency are worse. Paper trade for at least 4 weeks.

## 4. Rules and accounts (US)

- **Pattern-day-trader rule:** the SEC approved eliminating the $25,000 PDT minimum on April 14, 2026 (effective June 4, 2026). Brokers may phase the change in until **October 20, 2027**. If your broker still enforces the old rule, set `intraday.day_trade_limit_5d: 3`.
- **Cash accounts:** sale proceeds settle next business day (T+1). The bot only buys with settled cash (`account_type: cash`), which avoids good-faith violations but limits trades to roughly one use of your cash per day.
- **Short selling** isn't used: long only.

## 5. Running it

```bash
# Backtest on real minute data (Alpaca, ~2 years)
make fetch-intraday          # gzip CSVs in data/minute
make backtest-intraday       # metrics, trades.csv, equity.csv

# Paper trade live (runs 09:15–16:00 ET, one process per day)
./bin/tradebot day --broker alpaca-paper
sudo cp deploy/systemd/tradebot-day.* /etc/systemd/system/ && sudo systemctl enable --now tradebot-day.timer

# Robinhood (after the paper go/no-go)
./bin/tradebot rh-login      # browser OAuth once; token stored 0600
./bin/tradebot rh-tools      # list tools and schemas → fill robinhood.native in the config
./bin/tradebot doctor        # verifies data, keys, login and configured tool names
./bin/tradebot day --broker robinhood-native
```

**Paper go/no-go for intraday:**
- [ ] At least 20 sessions and at least 60 trades.
- [ ] Average R > 0.1 **after** real paper fills.
- [ ] No day beyond the loss limit.
- [ ] No positions left open overnight.
- [ ] No orders that were not in the journal.

**Stops are client-side.** Robinhood's MCP stop-order support is unverified, so the bot watches prices and sends the exit itself. If the process or the data feed goes down, open positions are unprotected until it recovers.
- Positions are checkpointed after every fill.
- The systemd unit restarts the process on failure, and a restarted session immediately sells what the crashed one left open.
- To stop a session, use `tradebot halt --broker intraday-<broker>`, which flattens. Don't use Ctrl-C.
- Keep `risk_per_trade` small until you trust the setup.

**Run one mode per account**, or give the swing and intraday modes disjoint universes. The intraday session only ever sells positions that it opened itself.

## References

- Zarattini, C. & Aziz, A. (2023). *Can Day Trading Really Be Profitable? Evidence of Sustainable Long-term Profits from Opening Range Breakout (ORB) Day Trading Strategy vs. Benchmark in the US Stock Market.* SSRN working paper.
- Barber, B., Lee, Y.-T., Liu, Y.-J. & Odean, T. (2014). *The cross-section of speculator skill: Evidence from day trading.* Journal of Financial Markets.
- Chague, F., De-Losso, R. & Giovannetti, B. (2019). *Day Trading for a Living?* SSRN working paper.
- FINRA, [Regulatory Notice 26-10](https://www.finra.org/rules-guidance/notices/26-10); Charles Schwab, [SEC Approves Scrapping $25,000 Day Trader Minimum](https://www.schwab.com/learn/story/sec-approves-scrapping-25000-day-trader-minimum).
