# trendbot: LLM-free multi-asset trend following

`trendbot` is a separate binary that trades **without any LLM**. No model is involved in signals, sizing, review or order placement, and the binary does not even link LLM code: `make check-llm-free` verifies its dependency graph in CI. It reuses the tested backtester, risk engine, brokers and runner shared with `tradebot`.

## 1. Why this strategy

I compared the candidates on the things that decide whether a retail strategy keeps working **after** you start trading it:

| Criterion | Multi-asset trend + momentum (trendbot) | Single-stock momentum/mean reversion (tradebot swing) | Intraday ORB (tradebot day) |
|---|---|---|---|
| Depth of evidence | Very high: century-long, dozens of markets (Hurst, Ooi & Pedersen 2017; Moskowitz, Ooi & Pedersen 2012) | High for momentum; mean reversion has decayed | One recent study; most day traders lose |
| Diversification | Stocks, bonds, credit, real estate, gold, commodities, cash | US large-cap equities only | A few stocks per day |
| Crash behavior | Historically moved to bonds/cash/gold in long bear markets | Regime filter helps; still equity-only | Flat overnight, but gap and slippage risk |
| Turnover and costs | Low (monthly; ~1–2× a year) | Medium | Very high; very cost-sensitive |
| Overfitting risk | Low: 4 simple parameters, all literature defaults | Medium | Medium–high |
| Data needs | Free daily ETF bars | Free daily bars | Real-time data (paid SIP recommended) |
| Survivorship bias | None (ETFs, fixed asset classes) | Yes (today's winners) | Some |

The case for it is **robustness, not the best backtest**. Anything tuned to top a backtest usually disappoints live. Trend following across asset classes has the broadest, longest out-of-sample record of any systematic approach a retail account can run. It also tends to earn most when equities crash hardest ("crisis alpha"), which is what makes medium risk with competitive returns plausible.

**Honest weaknesses:**
- It lags at sharp V-shaped reversals (e.g. March–April 2020).
- It can whipsaw in trendless years.
- It trails a pure S&P 500 in long, calm bull markets.
- Some years it will look worse than doing nothing.

## 2. The rules

Evaluated on the **first trading day of each month**, using the previous close. Orders are placed as limits at the next session.

1. **Universe:** 11 ETFs across asset classes: SPY, QQQ, IWM, EFA, EEM, VNQ, TLT, IEF, LQD, GLD and DBC. SGOV (T-bills) is the cash asset.
2. **Momentum score:** the average of the 1-, 3-, 6- and 12-month total returns (Keller & Keuning's 13612U blend; several horizons reduce timing luck).
3. **Absolute momentum:** an asset qualifies only if its score beats **T-bills' score** (Antonacci's dual momentum). This is the crash filter.
4. **Relative momentum:** fill up to **4 slots** with the highest-scoring qualifiers. Empty slots stay in T-bills, so exposure falls as trends break.
5. **Risk parity weights:**
   - Each asset is weighted by inverse 63-day volatility within the filled slots, capped at 35%.
   - If the portfolio's ex-ante volatility (63-day covariance) is above **15%/yr**, everything is scaled down (never levered up).
   - The remainder goes to T-bills.
6. **Between rebalances:** no trading unless weights drift more than 2% of equity, the risk engine intervenes, or you halt.
7. **Risk engine (shared):**
   - no leverage or shorting;
   - limit orders only;
   - price and stale-data checks;
   - exposure halves at a 15% drawdown;
   - the kill switch triggers at 25% (sell to T-bills; `trendbot resume` to restart).

`trendbot sweep` tests the robustness grid (top_k 3–5, vol target 10–20%, three look-back sets, monthly vs weekly) in-sample vs out-of-sample. Keep the defaults unless a *broad region* of the grid clearly beats them out of sample.

## 3. Running it

```bash
make trendbot                          # builds bin/trendbot
make check-llm-free                    # proves no LLM code is linked

# Backtest (Alpaca history starts in 2016; for longer history put CSVs from any source in data/etf,
# columns date,open,high,low,close,volume, split/dividend-adjusted, and use defensive: BIL)
./bin/trendbot fetch --start 2016-01-01
./bin/trendbot backtest --data data/etf        # prints metrics vs SPY and vs 60/40 on the same engine
./bin/trendbot sweep --data data/etf --split 2021-01-01

# Paper trading: run daily at ~09:40 ET (trades about once a month)
./bin/trendbot run --broker alpaca-paper
sudo cp deploy/systemd/trendbot.* /etc/systemd/system/ && sudo systemctl enable --now trendbot.timer

# Robinhood: direct MCP only; no Claude Code executor path exists in trendbot
./bin/trendbot rh-login && ./bin/trendbot rh-tools    # then fill robinhood.native in configs/trendbot.yaml
./bin/trendbot run --broker robinhood-native

./bin/trendbot status | halt | resume
```

**Go/no-go for real money:**
- [ ] The backtest (ideally with 15+ years of CSV history) shows a Sharpe ratio at or above 60/40's, with a clearly smaller max drawdown than SPY.
- [ ] At least 3 monthly rebalances on Alpaca paper, with orders matching the plan.
- [ ] `robinhood.native` is mapped and a one-order drill has been confirmed in the Robinhood app.

**Account size:** with whole shares, allocations track well from about **$5,000**. Below that, rounding leaves more in cash.

**Taxes:** monthly rotation realizes short-term gains in a taxable account. Consider an IRA if Robinhood's agentic account type allows it.

## References

- Hurst, B., Ooi, Y. H. & Pedersen, L. H. (2017). *A Century of Evidence on Trend-Following Investing.* Journal of Portfolio Management.
- Moskowitz, T., Ooi, Y. H. & Pedersen, L. H. (2012). *Time Series Momentum.* Journal of Financial Economics.
- Antonacci, G. (2014). *Dual Momentum Investing.* McGraw-Hill.
- Faber, M. (2007). *A Quantitative Approach to Tactical Asset Allocation.* Journal of Wealth Management.
- Keller, W. & Keuning, J. W. (2016–2018). *Protective / Defensive Asset Allocation* (SSRN), the source of the 13612 momentum blend.
- Moreira, A. & Muir, T. (2017). *Volatility-Managed Portfolios.* Journal of Finance.
