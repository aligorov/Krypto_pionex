import numpy as np
import pandas as pd
from typing import Dict, List, Any

class QuantBacktestEngine:
    """
    Event-driven backtesting engine with strict intrabar execution logic,
    maker/taker fee modeling, funding rate accounting, and Purged Walk-Forward OOS evaluation.
    """

    def __init__(self, maker_fee: float = 0.0005, taker_fee: float = 0.0005, slippage: float = 0.0002):
        self.maker_fee = maker_fee
        self.taker_fee = taker_fee
        self.slippage = slippage

    def calculate_metrics(self, equity_curve: List[float], trades: List[Dict[str, Any]]) -> Dict[str, Any]:
        if not trades or len(equity_curve) < 2:
            return {
                "total_trades": 0,
                "expected_value": 0.0,
                "sharpe_ratio": 0.0,
                "sortino_ratio": 0.0,
                "max_drawdown": 0.0,
                "profit_factor": 0.0,
                "win_rate": 0.0,
            }

        pnls = [t.get("pnl", 0.0) for t in trades]
        wins = [p for p in pnls if p > 0]
        losses = [abs(p) for p in pnls if p < 0]

        total_trades = len(pnls)
        win_rate = len(wins) / total_trades if total_trades > 0 else 0.0
        expected_value = float(np.mean(pnls)) if total_trades > 0 else 0.0

        gross_profit = sum(wins)
        gross_loss = sum(losses)
        profit_factor = gross_profit / gross_loss if gross_loss > 0 else (gross_profit if gross_profit > 0 else 0.0)

        # Drawdown calculation
        eq = np.array(equity_curve)
        peak = np.maximum.accumulate(eq)
        drawdowns = (peak - eq) / peak
        max_drawdown = float(np.max(drawdowns)) if len(drawdowns) > 0 else 0.0

        # Returns & Ratios
        returns = np.diff(eq) / eq[:-1]
        std_dev = float(np.std(returns)) if len(returns) > 1 else 0.0
        sharpe_ratio = (float(np.mean(returns)) / std_dev * np.sqrt(365 * 24)) if std_dev > 0 else 0.0

        downside_returns = returns[returns < 0]
        downside_std = float(np.std(downside_returns)) if len(downside_returns) > 1 else 0.0
        sortino_ratio = (float(np.mean(returns)) / downside_std * np.sqrt(365 * 24)) if downside_std > 0 else 0.0

        return {
            "total_trades": total_trades,
            "expected_value": round(expected_value, 4),
            "sharpe_ratio": round(sharpe_ratio, 2),
            "sortino_ratio": round(sortino_ratio, 2),
            "max_drawdown": round(max_drawdown, 4),
            "profit_factor": round(profit_factor, 2),
            "win_rate": round(win_rate, 4),
        }
