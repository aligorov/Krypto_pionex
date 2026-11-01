#!/usr/bin/env python3
"""Market Analytics CLI for Krypto_pionex using trading_skills engine.

Provides read-only market telemetry, technical indicator computation, risk metrics,
correlation matrices, and momentum scanners for Pionex crypto assets and macro feeds.

Strictly compliant with AGENTS.md:
- Used exclusively for read-only market telemetry and gating inputs.
- Never used to construct order parameters or override Pionex execution prices.
"""
import argparse
import json
import os
import sys

# Ensure trading_skills package from local venv or project can be imported
PROJECT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
VENV_LIB = os.path.join(PROJECT_ROOT, "trading_skills", "src")
if VENV_LIB not in sys.path:
    sys.path.insert(0, VENV_LIB)

try:
    from trading_skills.quote import get_quote
    from trading_skills.technicals import compute_indicators, compute_multi_symbol
    from trading_skills.risk import calculate_risk_metrics
    from trading_skills.correlation import compute_correlation
    from trading_skills.scanner_bullish import scan_symbols
    from trading_skills.utils import generated_at_str, normalize_symbol
except ImportError as e:
    sys.stderr.write(f"Error importing trading_skills: {e}\n")
    sys.stderr.write("Please run: (cd trading_skills && uv sync)\n")
    sys.exit(1)


def main():
    parser = argparse.ArgumentParser(description="Market Analytics CLI for Pionex Trading Bot")
    subparsers = parser.add_subparsers(dest="command", required=True)

    # Quote
    quote_parser = subparsers.add_parser("quote", help="Get real-time quote for symbol")
    quote_parser.add_argument("symbol", help="Ticker symbol (e.g. BTC_USDT, ETH_USDT)")

    # Technicals
    tech_parser = subparsers.add_parser("technicals", help="Compute technical indicators (RSI, MACD, BB, EMA, SMA, ATR, ADX)")
    tech_parser.add_argument("symbol", help="Ticker symbol or comma-separated list")
    tech_parser.add_argument("--period", default="3mo", help="Period (1mo, 3mo, 6mo, 1y)")
    tech_parser.add_argument("--indicators", default=None, help="Comma-separated indicators (rsi,macd,bb,sma,ema,atr,adx)")

    # Risk
    risk_parser = subparsers.add_parser("risk", help="Calculate risk metrics (volatility, Sharpe, beta, VaR, drawdown)")
    risk_parser.add_argument("symbol", help="Ticker symbol (e.g. BTC_USDT)")
    risk_parser.add_argument("--period", default="1y", help="Period (default: 1y)")
    risk_parser.add_argument("--position-size", type=float, default=None, help="Optional position size in USDT")

    # Correlation
    corr_parser = subparsers.add_parser("correlation", help="Compute correlation matrix between multiple symbols")
    corr_parser.add_argument("symbols", nargs="+", help="Symbols list (e.g. BTC_USDT ETH_USDT SOL_USDT)")
    corr_parser.add_argument("--period", default="3mo", help="Period (default: 3mo)")

    # Scanner
    scan_parser = subparsers.add_parser("scan", help="Scan and rank symbols by bullish momentum")
    scan_parser.add_argument("symbols", nargs="+", help="Symbols to scan")
    scan_parser.add_argument("--period", default="3mo", help="Period (default: 3mo)")
    scan_parser.add_argument("--top", type=int, default=5, help="Number of top symbols to return")

    args = parser.parse_args()

    if args.command == "quote":
        res = get_quote(args.symbol)
        res["generated_at"] = generated_at_str()
        print(json.dumps(res, indent=2, default=str))

    elif args.command == "technicals":
        indicators = args.indicators.split(",") if args.indicators else None
        symbols = [s.strip() for s in args.symbol.split(",")]
        if len(symbols) == 1:
            res = compute_indicators(symbols[0], args.period, indicators)
        else:
            res = compute_multi_symbol(symbols, args.period, indicators)
        res["generated_at"] = generated_at_str()
        print(json.dumps(res, indent=2, default=str))

    elif args.command == "risk":
        res = calculate_risk_metrics(args.symbol, args.period, args.position_size)
        res["generated_at"] = generated_at_str()
        print(json.dumps(res, indent=2, default=str))

    elif args.command == "correlation":
        res = compute_correlation(args.symbols, args.period)
        res["generated_at"] = generated_at_str()
        print(json.dumps(res, indent=2, default=str))

    elif args.command == "scan":
        results = scan_symbols(args.symbols, top_n=args.top, period=args.period)
        res = {
            "symbols_scanned": len(args.symbols),
            "top_count": len(results),
            "results": results,
            "generated_at": generated_at_str(),
        }
        print(json.dumps(res, indent=2, default=str))


if __name__ == "__main__":
    main()
