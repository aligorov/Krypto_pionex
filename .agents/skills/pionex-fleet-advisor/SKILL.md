---
name: pionex-fleet-advisor
description: Analyze Pionex grid bot candidates and fleet risk using quantitative methods from trading_skills (cross-asset correlation matrices, Value-at-Risk, ADX trend health, and joint-drawdown hazards). Use when assessing whether to deploy, adjust, or avoid prospective grid bot symbols.
dependencies: ["trading-skills"]
---

# Pionex Fleet Risk & Correlation Advisor

Applies quantitative methods from `trading_skills` to safeguard the Pionex Futures Grid fleet from joint-liquidation cascades and unfavorable trending regimes.

## Instructions

Run the advisor from the workspace root:

```bash
trading_skills/.venv/bin/python scripts/fleet_risk_advisor.py --symbols SYMBOL_1 SYMBOL_2 [SYMBOL_3...] [--benchmark BTC_USDT] [--period 3mo] [--format summary|json]
```

## Key Output Metrics & Decision Rules

1. **Benchmark Macro Regime**:
   - Evaluates BTC_USDT momentum via RSI, ADX, and EMA9/EMA21 crossovers.
   - If BTC has an active EMA crossover down or RSI < 40, market regime has high downward drift risk.

2. **Cross-Asset Correlation Matrix ($Corr \ge 0.80$)**:
   - **Hazard Rule**: Never deploy multiple bots with pairwise correlation $\ge 0.80$ simultaneously with high leverage. A macro cascade will liquidate both positions in tandem.
   - **Diversifier Rule**: Seek assets with correlation $\le 0.65$ relative to BTC and other active bots.

3. **ADX & Trend Boundaries**:
   - $ADX < 22$: Excellent Range (ideal for Mean Reversion neutral grids).
   - $ADX \in [22, 28]$: Moderate Range / Transitional.
   - $ADX > 30$: Strong Trend (unfavorable for neutral grid bots, high boundary breakout risk).

4. **Value-at-Risk (VaR 99%) & Leverage Sizing**:
   - Daily $VaR_{99} > 15\%$: Maximum recommended leverage is **2x**.
   - Daily $VaR_{99} \in [8\%, 15\%]$: Maximum recommended leverage is **3x**.
   - Daily $VaR_{99} < 8\%$: Maximum recommended leverage is **4x**.

## Examples

```bash
# Evaluate current candidates against BTC
trading_skills/.venv/bin/python scripts/fleet_risk_advisor.py --symbols QNT_USDT ZEC_USDT SOL_USDT

# JSON output for automated integration
trading_skills/.venv/bin/python scripts/fleet_risk_advisor.py --symbols QNT_USDT ZEC_USDT --format json
```
