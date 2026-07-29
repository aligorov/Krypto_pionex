import unittest
import sys
import os

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

from engine.backtest import QuantBacktestEngine

class TestQuantEngine(unittest.TestCase):
    def test_backtest_metrics_calculation(self):
        engine = QuantBacktestEngine()
        equity_curve = [100.0, 105.0, 103.0, 108.0, 110.0]
        trades = [
            {"pnl": 5.0},
            {"pnl": -2.0},
            {"pnl": 5.0},
            {"pnl": 2.0},
        ]

        metrics = engine.calculate_metrics(equity_curve, trades)

        self.assertEqual(metrics["total_trades"], 4)
        self.assertEqual(metrics["win_rate"], 0.75)
        self.assertEqual(metrics["expected_value"], 2.5)
        self.assertEqual(metrics["profit_factor"], 6.0)
        self.assertGreater(metrics["max_drawdown"], 0)

if __name__ == "__main__":
    unittest.main()
