import pytest
from engine.backtest import QuantBacktestEngine

def test_backtest_metrics_calculation():
    engine = QuantBacktestEngine()
    equity_curve = [100.0, 105.0, 103.0, 108.0, 110.0]
    trades = [
        {"pnl": 5.0},
        {"pnl": -2.0},
        {"pnl": 5.0},
        {"pnl": 2.0},
    ]

    metrics = engine.calculate_metrics(equity_curve, trades)

    assert metrics["total_trades"] == 4
    assert metrics["win_rate"] == 0.75
    assert metrics["expected_value"] == 2.5
    assert metrics["profit_factor"] == 6.0
    assert metrics["max_drawdown"] > 0
