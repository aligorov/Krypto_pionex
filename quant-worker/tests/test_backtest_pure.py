import unittest
import math

def calculate_pure_metrics(equity_curve, trades):
    if not trades or len(equity_curve) < 2:
        return {"total_trades": 0, "win_rate": 0.0, "expected_value": 0.0}

    pnls = [t.get("pnl", 0.0) for t in trades]
    wins = [p for p in pnls if p > 0]
    total_trades = len(pnls)
    win_rate = len(wins) / total_trades if total_trades > 0 else 0.0
    expected_value = sum(pnls) / total_trades if total_trades > 0 else 0.0

    return {
        "total_trades": total_trades,
        "win_rate": win_rate,
        "expected_value": expected_value
    }

class TestPureQuantEngine(unittest.TestCase):
    def test_pure_metrics(self):
        equity_curve = [100.0, 105.0, 103.0, 108.0, 110.0]
        trades = [{"pnl": 5.0}, {"pnl": -2.0}, {"pnl": 5.0}, {"pnl": 2.0}]
        res = calculate_pure_metrics(equity_curve, trades)
        self.assertEqual(res["total_trades"], 4)
        self.assertEqual(res["win_rate"], 0.75)
        self.assertEqual(res["expected_value"], 2.5)

if __name__ == "__main__":
    unittest.main()
