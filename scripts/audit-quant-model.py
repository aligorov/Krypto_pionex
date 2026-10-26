"""Read-only synthetic probes for the 2026-10-05 audit; no DB or API access.

Run from any directory with the project's Python/numpy runtime. Exit code zero
means the diagnostic ran, NOT that the engine passed correctness tests.
"""

from __future__ import annotations

import importlib.util
import json
import math
import sys
from pathlib import Path
from types import ModuleType
from typing import Any


def load_engine() -> ModuleType:
    project = Path(__file__).resolve().parents[1]
    source = project / "quant-worker" / "engine" / "backtest.py"
    spec = importlib.util.spec_from_file_location("audited_backtest", source)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"Cannot load engine: {source}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def fee_probe(module: ModuleType) -> dict[str, Any]:
    candles = [
        {"open": 100, "high": 100, "low": 98.5, "close": 100, "volume": 1000},
        {"open": 100, "high": 102, "low": 100, "close": 102, "volume": 1000},
    ]
    result = module.GridSimulator(
        maker_fee=0.0002, taker_fee=0, slippage=0, funding_rate_8h=0
    ).simulate(candles, lower=90, upper=110, levels=10, investment=100)
    # One $10 opening lot at 100 and its exit at 102. Actual execution fees
    # are $10*0.0002 + ($10/100)*102*0.0002, each exactly once.
    expected_fees = 10 * 0.0002 + (10 / 100) * 102 * 0.0002
    expected_net = (10 / 100) * (102 - 100) - expected_fees
    observed_fees = float(result["fees_paid"])
    observed_net = float(result["realized_pnl"])
    return {
        "round_trips": result["round_trips"],
        "observed_fees": observed_fees,
        "expected_fees": round(expected_fees, 6),
        "observed_realized_pnl": observed_net,
        "expected_realized_pnl": round(expected_net, 6),
        "defect_detected": not (
            math.isclose(observed_fees, expected_fees, abs_tol=0.000001)
            and math.isclose(observed_net, expected_net, abs_tol=0.000001)
        ),
        "interpretation": "Opening maker fee must be charged exactly once.",
    }


def volume_probe(module: ModuleType) -> dict[str, Any]:
    candles = [
        {
            "open": 100 + 3 * math.sin(index / 8),
            "high": 104,
            "low": 96,
            "close": 100 + 3 * math.sin((index + 1) / 8),
            "volume": 0,
        }
        for index in range(500)
    ]
    engine = module.QuantBacktestEngine(funding_rate_8h=0)
    result = module.walk_forward(
        engine, candles, train_bars=240, test_bars=60, purge_bars=6
    )
    return {
        "candle_count": len(candles),
        "all_volume_zero": True,
        "round_trips": result["round_trips"],
        "liquidity_ok": result["liquidity_ok"],
        "liquidity_reason": result["liquidity_reason"],
        "defect_detected": bool(result["liquidity_ok"]),
        "interpretation": "Unavailable liquidity cannot prove executable fills.",
    }


def main() -> None:
    sys.dont_write_bytecode = True
    module = load_engine()
    print(json.dumps({
        "diagnostic_only": True,
        "opening_fee": fee_probe(module),
        "zero_volume": volume_probe(module),
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
