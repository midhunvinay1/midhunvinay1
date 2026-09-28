# Strategy: Regime-Filtered Momentum + Mean Reversion, Vol-Targeted, Claude-Reviewed

**Goal:** medium risk (equity-like volatility or less, with controlled drawdowns) and the highest return that has credible evidence behind it. Tradeable through Robinhood with daily decisions.

> **Honest expectation-setting.** No strategy can promise high returns. Published anomalies lose about a quarter of their strength out of sample and about half after publication (McLean & Pontiff, 2016). Backtests overstate live results. The aim here is **better risk-adjusted returns than buy-and-hold with smaller drawdowns**, validated on *your* real-data backtest and paper trading before any real money is used.

## 1. What was considered

| Candidate | Evidence | Fit for Robinhood + medium risk | Verdict |
|---|---|---|---|
| **Cross-sectional / time-series momentum** | Very strong: Jegadeesh & Titman (1993); Moskowitz, Ooi & Pedersen (2012); robust across markets and centuries | Daily/monthly cadence, liquid names, long-only possible | ✅ **Core sleeve** |
| **Trend/regime filter (200-day MA)** | Faber (2007); Antonacci dual momentum (2014): large drawdown reduction | Trivial to implement; cuts the biggest losses | ✅ **Regime gate** |
| **Volatility targeting** | Moreira & Muir (2017); Barroso & Santa-Clara (2015) ("momentum has its moments"): raises Sharpe and reduces momentum crashes (Daniel & Moskowitz, 2016) | Pure position sizing | ✅ **Sizing** |
| **Short-term mean reversion (RSI(2) in uptrends)** | Connors & Alvarez (2009); long-documented short-term reversal effect | High win rate, holds for days; diversifies momentum | ✅ **Second sleeve** |
| LLM news sentiment as the alpha | Lopez-Lira & Tang (2023): headline sentiment predicts next-day returns, mostly in small caps, and the effect decays quickly | Next-day horizon, small caps, fast decay; can't be backtested honestly | ⚠️ Used only as a **risk filter** |
| Opening-range breakout day trading | Zarattini & Aziz (2023) report strong results on "stocks in play" | Needs intraday data, fast execution, and $25k to avoid PDT limits; per-trade MCP round trips | ❌ Not a fit |
| Selling options (premium harvesting) | Positive carry, but short-volatility tail risk | Not "medium risk"; approval levels | ❌ |
| Pure "Claude picks stocks" | No robust evidence; untestable because of look-ahead contamination | none | ❌ |

## 2. The rules (defaults in `configs/config.yaml`)

**Regime (every day):**
- Risk-on when SPY closes above its 200-day SMA.
- A 1% hysteresis band prevents flip-flopping.

**Momentum sleeve (60% of capital):**
- **Rebalance:** monthly (every 21 trading days), or immediately when the regime flips.
- **Score:** the average of the 3-, 6- and 12-month returns, skipping the most recent month (to avoid short-term reversal), divided by 63-day volatility.
- **Filters:** 12-1 month return > 0 (absolute momentum), price above the 100-day SMA, price ≥ $5, and 20-day average dollar volume ≥ $20M.
- **Selection:** hold the top 5, weighted by inverse volatility.
- **Exit between rebalances:** an ATR chandelier stop at 3 × ATR(20) below the highest close since entry.
- **Risk-off:** the whole sleeve moves to **SGOV** (a T-bill ETF, cash-like), or to cash.

**Mean-reversion sleeve (40% of capital, risk-on only):**
- **Entry:** RSI(2) < 10 while the close is above the 200-day SMA (a dip in an uptrend).
- **Position size:** at most 4 positions at 10% each; the most oversold names go first.
- **Exit:** close above the 5-day SMA (the bounce), a 10-day time stop, or a stop 2.5 × ATR below entry.

**Portfolio:**
- Sleeves are added together, with each stock capped at 25%.
- If the portfolio's ex-ante volatility (63-day covariance) is above **18%/yr**, all weights are scaled down to hit 18%.
- No leverage, no shorting, whole shares, limit orders.

**Claude overlay (new entries only):** Claude sees the candidate's statistics and the last 7 days of headlines, then answers:
- `approve` (×1);
- `reduce` (×0.25–0.75), for example earnings within about 3 days, or an unexplained violent move;
- `veto` (×0), for example fraud, bankruptcy risk, an agreed acquisition, or a guidance cut behind a "dip".

Claude cannot add trades or increase size.

**Risk engine (always):**
- Position ≤ 30%, gross exposure ≤ 100%.
- No new buys after a 4% down day.
- Exposure halves at a 15% drawdown; the **kill switch** triggers at 25% (sell to cash; a human must `tradebot resume`).

## 3. Why the combination works

- **Momentum** earns in persistent trends but crashes in sharp reversals. The **regime filter** and **vol targeting** address exactly those crashes.
- **Mean reversion** earns in choppy, range-bound uptrends, where momentum struggles. Its trades are short and its returns only weakly correlated with momentum.
- The **defensive asset** keeps the momentum capital earning T-bill yield in bear markets instead of fighting the trend.
- **Claude** addresses the main known weakness of rule-based dip buying: buying a "dip" whose cause is fundamental (fraud, guidance withdrawal) rather than noise.

## 4. Where medium risk comes from

| Control | Effect |
|---|---|
| 18% volatility target | Roughly the long-run volatility of the S&P 500; scaled down, never levered up |
| 200-day regime filter | Historically sidesteps most of the big bear-market drawdowns |
| Stops (ATR trailing, ATR fixed, time) | Caps single-position damage |
| 15% / 25% drawdown breakers | Hard portfolio-level limits |
| Robinhood funded-budget ceiling | Absolute cap on what can be lost live |

## 5. Tuning without overfitting

`tradebot sweep` runs 81 parameter combinations (top N, RSI threshold, vol target, sleeve mix). It ranks them on an **in-sample** window and reports each one's **out-of-sample** result.

Pick parameters from a **broad plateau** where many neighbours perform similarly. Never pick the single best in-sample row, and never tune on the out-of-sample years.

## 6. Known biases to correct for

- **Survivorship:** the default universe is today's winners, which inflates backtests of the stock sleeve. Mitigations:
  - trust the ETF-only results more (set `universe` to the ETFs to compare);
  - use point-in-time index membership (see the completion prompt).
- **SGOV history:** SGOV starts in mid-2020. For longer backtests set `defensive: BIL`.
- **Costs:** slippage is modeled at 5 bps plus limit-order non-fills. Commissions are $0 at Robinhood, but spreads are real.
- **LLM look-ahead:** the Claude overlay is **off in backtests** by design. Measure its value in paper trading by comparing vetoed entries with what those trades would have done (the journal records both).

## References

- Jegadeesh, N. & Titman, S. (1993). *Returns to Buying Winners and Selling Losers.* Journal of Finance.
- Moskowitz, T., Ooi, Y. H. & Pedersen, L. H. (2012). *Time Series Momentum.* Journal of Financial Economics.
- Faber, M. (2007). *A Quantitative Approach to Tactical Asset Allocation.* Journal of Wealth Management.
- Antonacci, G. (2014). *Dual Momentum Investing.* McGraw-Hill.
- Barroso, P. & Santa-Clara, P. (2015). *Momentum Has Its Moments.* Journal of Financial Economics.
- Daniel, K. & Moskowitz, T. (2016). *Momentum Crashes.* Journal of Financial Economics.
- Moreira, A. & Muir, T. (2017). *Volatility-Managed Portfolios.* Journal of Finance.
- Connors, L. & Alvarez, C. (2009). *Short Term Trading Strategies That Work.* TradingMarkets.
- Lopez-Lira, A. & Tang, Y. (2023). *Can ChatGPT Forecast Stock Price Movements? Return Predictability and Large Language Models.* Working paper.
- Zarattini, C. & Aziz, A. (2023). *Can Day Trading Really Be Profitable?* Working paper.
- McLean, R. D. & Pontiff, J. (2016). *Does Academic Research Destroy Stock Return Predictability?* Journal of Finance.
