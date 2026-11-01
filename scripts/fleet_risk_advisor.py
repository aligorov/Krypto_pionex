#!/usr/bin/env python3
"""Fleet Risk & Correlation Advisor for Krypto_pionex.

Applies quantitative trading_skills methods (cross-asset correlation matrices,
Value-at-Risk, annualized volatility, and ADX/EMA trend health) to the
Pionex Futures Grid fleet to protect against joint-drawdown cascades.

Master-Plan §4 compliance:
- Detects high correlation (>0.80) among active/candidate bots to prevent
  simultaneous liquidation when the market trends against the inventory.
- Flags trend vs range conflicts (ADX > 25 with directional bias).
- Computes parametric VaR (95% / 99%) for leverage and stop-loss calibration.
"""
import argparse
import json
import os
import sys

PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VENV_LIB = os.path.join(PROJECT_ROOT, "trading_skills", "src")
if VENV_LIB not in sys.path:
    sys.path.insert(0, VENV_LIB)

try:
    from trading_skills.correlation import compute_correlation
    from trading_skills.risk import calculate_risk_metrics
    from trading_skills.technicals import compute_indicators
    from trading_skills.utils import generated_at_str, normalize_symbol
except ImportError as e:
    sys.stderr.write(f"Error importing trading_skills: {e}\n")
    sys.exit(1)


def evaluate_fleet(symbols: list[str], benchmark: str = "BTC_USDT", period: str = "3mo") -> dict:
    """Analyze a fleet of candidate/active symbols against benchmark and each other."""
    # Ensure benchmark is in the list for correlation
    all_symbols = list(dict.fromkeys([benchmark] + symbols))

    report = {
        "generated_at": generated_at_str(),
        "benchmark": benchmark,
        "fleet_symbols": symbols,
        "period": period,
        "benchmark_regime": {},
        "candidates": {},
        "correlation_warnings": [],
        "fleet_recommendations": [],
    }

    # 1. Benchmark Health (Macro Direction)
    try:
        btc_tech = compute_indicators(benchmark, period=period)
        if "error" not in btc_tech:
            btc_price = btc_tech.get("price", {})
            btc_ind = btc_tech.get("indicators", {})
            rsi = btc_ind.get("rsi", {}).get("value")
            adx = btc_ind.get("adx", {}).get("adx")
            ema_x = btc_ind.get("ema", {}).get("crossover")
            macd_x = btc_ind.get("macd", {}).get("crossover")

            report["benchmark_regime"] = {
                "price": btc_price.get("current"),
                "change_pct": btc_price.get("change_pct"),
                "rsi": rsi,
                "adx": adx,
                "ema_crossover": ema_x,
                "macd_crossover": macd_x,
                "macro_bias": "BEARISH_LEAN" if (rsi and rsi < 45) else ("BULLISH_LEAN" if (rsi and rsi > 55) else "NEUTRAL"),
            }
    except Exception as e:
        report["benchmark_regime"] = {"error": str(e)}

    # 2. Multi-Asset Correlation
    corr_res = compute_correlation(all_symbols, period=period)
    matrix = corr_res.get("correlation_matrix", {})
    report["correlation_matrix"] = matrix

    # Check for excessive cross-pair correlation among fleet candidates
    for i in range(len(symbols)):
        for j in range(i + 1, len(symbols)):
            s1, s2 = symbols[i], symbols[j]
            c_val = matrix.get(s1, {}).get(s2)
            if c_val is not None and c_val >= 0.80:
                report["correlation_warnings"].append({
                    "pair": f"{s1} <-> {s2}",
                    "correlation": c_val,
                    "risk": "HIGH_JOINT_DRAWDOWN",
                    "guidance": f"Do not deploy both {s1} and {s2} simultaneously with high leverage. A drop in one triggers a drop in the other.",
                })

    # 3. Individual Asset Diagnostics
    for sym in symbols:
        c_diag = {}
        # Technicals
        try:
            tech = compute_indicators(sym, period=period)
            if "error" not in tech:
                ind = tech.get("indicators", {})
                rsi = ind.get("rsi", {}).get("value")
                adx = ind.get("adx", {}).get("adx")
                dmp = ind.get("adx", {}).get("dmp")
                dmn = ind.get("adx", {}).get("dmn")
                c_diag["rsi"] = rsi
                c_diag["adx"] = adx
                c_diag["adx_direction"] = "BULLISH_TREND" if (dmp and dmn and dmp > dmn) else "BEARISH_TREND"
                c_diag["ema_crossover"] = ind.get("ema", {}).get("crossover")
                c_diag["macd_crossover"] = ind.get("macd", {}).get("crossover")

                # Grid Suitability
                if adx and adx > 30:
                    c_diag["grid_suitability"] = "UNFAVORABLE_TRENDING"
                    c_diag["warning"] = f"ADX is {adx:.1f} (>30). Strong trend may break neutral grid boundaries."
                elif adx and adx < 22:
                    c_diag["grid_suitability"] = "EXCELLENT_RANGE"
                else:
                    c_diag["grid_suitability"] = "MODERATE_RANGE"
        except Exception as e:
            c_diag["technical_error"] = str(e)

        # Risk metrics
        try:
            risk = calculate_risk_metrics(sym, period=period)
            if "error" not in risk:
                c_diag["annual_volatility_pct"] = risk.get("volatility", {}).get("annual")
                c_diag["var_95_daily_pct"] = risk.get("var", {}).get("var_95_daily")
                c_diag["var_99_daily_pct"] = risk.get("var", {}).get("var_99_daily")
                c_diag["sharpe_ratio"] = risk.get("sharpe_ratio")
                c_diag["max_drawdown_pct"] = risk.get("max_drawdown")

                # Suggested Leverage based on VaR
                var99 = abs(risk.get("var", {}).get("var_99_daily") or 10.0)
                if var99 > 15.0:
                    c_diag["recommended_max_leverage"] = 2
                elif var99 > 8.0:
                    c_diag["recommended_max_leverage"] = 3
                else:
                    c_diag["recommended_max_leverage"] = 4
        except Exception as e:
            c_diag["risk_error"] = str(e)

        # Correlation with Benchmark
        c_diag["btc_correlation"] = matrix.get(sym, {}).get(benchmark)
        report["candidates"][sym] = c_diag

    # Overall Fleet Verdict
    corr_warn_count = len(report["correlation_warnings"])
    if corr_warn_count > 0:
        report["fleet_recommendations"].append(
            f"Detected {corr_warn_count} highly correlated pair(s). Stagger entries or cap allocation."
        )
    else:
        report["fleet_recommendations"].append(
            "Fleet correlation is well-diversified. Low joint liquidation clustering."
        )

    return report


def main():
    parser = argparse.ArgumentParser(description="Fleet Risk & Correlation Advisor for Pionex Grid Bots")
    parser.add_argument("--symbols", nargs="+", default=["QNT_USDT", "ZEC_USDT", "SOL_USDT", "UNI_USDT"], help="Symbols to analyze")
    parser.add_argument("--benchmark", default="BTC_USDT", help="Benchmark symbol (default: BTC_USDT)")
    parser.add_argument("--period", default="3mo", help="Lookback period (default: 3mo)")
    parser.add_argument("--format", choices=["json", "summary"], default="summary", help="Output format")

    args = parser.parse_args()
    report = evaluate_fleet(args.symbols, benchmark=args.benchmark, period=args.period)

    if args.format == "json":
        print(json.dumps(report, indent=2, default=str))
    else:
        print("=" * 70)
        print("  PIONEX FLEET QUANTITATIVE RISK & CORRELATION ADVISOR")
        print("  Powered by trading_skills Quantitative Analysis Engine")
        print("=" * 70)
        print(f"Generated at: {report['generated_at']}")
        print(f"Benchmark:    {report['benchmark']} (Bias: {report.get('benchmark_regime', {}).get('macro_bias')})")
        print(f"Lookback:     {report['period']}")
        print("-" * 70)

        print("\n[1] BENCHMARK MACRO REGIME:")
        bm = report.get("benchmark_regime", {})
        print(f"  Price: ${bm.get('price')} (24h: {bm.get('change_pct')}%) | RSI: {bm.get('rsi')} | ADX: {bm.get('adx')}")
        if bm.get("ema_crossover"):
            print(f"  EMA9/21 Crossover: {bm['ema_crossover'].get('direction')} ({bm['ema_crossover'].get('days_ago')} bars ago)")

        print("\n[2] ASSET RISK & GRID SUITABILITY:")
        for sym, d in report.get("candidates", {}).items():
            print(f"\n  • {sym}:")
            print(f"    - Grid Suitability: {d.get('grid_suitability')} (RSI: {d.get('rsi')}, ADX: {d.get('adx')})")
            if d.get("warning"):
                print(f"    - WARNING: {d.get('warning')}")
            print(f"    - Volatility (Ann): {d.get('annual_volatility_pct')}% | Max DD: {d.get('max_drawdown_pct')}%")
            print(f"    - VaR 95% Daily:    {d.get('var_95_daily_pct')}% | VaR 99%: {d.get('var_99_daily_pct')}%")
            print(f"    - BTC Correlation:   {d.get('btc_correlation')} | Recommended Max Lev: {d.get('recommended_max_leverage')}x")

        print("\n[3] CROSS-ASSET CORRELATION & CONCENTRATION RISK:")
        if report.get("correlation_warnings"):
            for w in report["correlation_warnings"]:
                print(f"  [!] {w['pair']} -> Correlation: {w['correlation']:.4f} ({w['risk']})")
                print(f"      Advice: {w['guidance']}")
        else:
            print("  [✓] No dangerous (>0.80) correlations among fleet candidates. Portfolio is diversified.")

        print("\n[4] FLEET RECOMMENDATIONS:")
        for rec in report.get("fleet_recommendations", []):
            print(f"  → {rec}")
        print("=" * 70)


if __name__ == "__main__":
    main()
