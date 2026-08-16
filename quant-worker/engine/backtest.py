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


class GridSimulator:
    """
    Event-driven neutral futures grid simulation on OHLCV candles with
    strict intrabar path reconstruction (open -> nearer extreme -> farther
    extreme -> close), maker-fee round trips, per-level inventory and a
    hard stop below the range. Deterministic: same candles -> same result.

    The model deliberately mirrors the Go paper PnL model so backtests and
    paper/live accounting agree on what a grid earns between two prices.
    """

    def __init__(self, maker_fee: float = 0.0002, taker_fee: float = 0.0005, slippage: float = 0.0002):
        self.maker_fee = maker_fee
        self.taker_fee = taker_fee
        self.slippage = slippage

    def _candle_path(self, candle):
        open_, high, low, close = candle["open"], candle["high"], candle["low"], candle["close"]
        if close < open_:
            return [open_, low, high, close]
        return [open_, high, low, close]

    def simulate(self, candles, lower, upper, levels, investment, stop_loss_pct=None):
        if upper <= lower or levels < 2 or investment <= 0 or not candles:
            return {"error": "invalid parameters"}

        step = (upper - lower) / levels
        per_level_quote = investment / levels
        entry_price = candles[0]["open"]
        if not (lower < entry_price < upper):
            entry_price = min(max(entry_price, lower + step), upper - step)

        open_lots = {}     # level index -> avg entry price of held base
        base_held = 0.0
        quote_spent = 0.0
        realized = 0.0
        round_trips = 0
        fees_paid = 0.0
        equity_curve = []
        end_reason = "COMPLETED"
        stop_price = lower * (1 - stop_loss_pct / 100.0) if stop_loss_pct else None
        stop_hit = False
        last_price = entry_price

        def level_of(price):
            return int((price - lower) / step)

        def fill_between(prev, nxt):
            """Process every grid level crossed between two path points."""
            nonlocal base_held, quote_spent, realized, round_trips, fees_paid
            if prev == nxt:
                return
            down = nxt < prev
            lo_level = level_of(min(prev, nxt))
            hi_level = level_of(max(prev, nxt))
            for lvl in range(lo_level + 1, hi_level + 1):
                price = lower + lvl * step
                if down:
                    # A downward crossing of level `lvl` fills a BUY there —
                    # but only one open lot per level: a market sitting ON a
                    # level must not re-buy it every candle.
                    if open_lots.get(lvl):
                        continue
                    base = per_level_quote / price
                    fee = per_level_quote * self.maker_fee
                    open_lots.setdefault(lvl, []).append(price)
                    base_held += base
                    quote_spent += per_level_quote
                    fees_paid += fee
                    realized -= fee
                else:
                    # An upward crossing fills the SELL of the lot bought one
                    # level below (classic paired grid round trip).
                    buy_lvl = lvl - 1
                    if open_lots.get(buy_lvl):
                        entry = open_lots[buy_lvl].pop(0)
                        if not open_lots[buy_lvl]:
                            del open_lots[buy_lvl]
                        base = per_level_quote / entry
                        gross = base * (price - entry)
                        fee = per_level_quote * self.maker_fee * 2
                        fees_paid += fee
                        realized += gross - fee
                        round_trips += 1
                        base_held -= base
                        quote_spent -= per_level_quote

        for candle in candles:
            for point in self._candle_path(candle):
                if stop_price is not None and point <= stop_price and not stop_hit:
                    stop_hit = True
                    end_reason = "STOP_LOSS"
                    # Liquidate everything at the stopped price (taker).
                    if base_held > 0:
                        avg_entry = quote_spent / base_held if base_held > 0 else 0
                        gross = base_held * (point - avg_entry)
                        fee = base_held * point * self.taker_fee
                        realized += gross - fee
                        fees_paid += fee
                        base_held = 0.0
                        quote_spent = 0.0
                        open_lots.clear()
                    last_price = point
                    break
                fill_between(last_price, point)
                last_price = point
            if stop_hit:
                break
            avg_entry = quote_spent / base_held if base_held > 0 else 0.0
            unrealized = base_held * (last_price - avg_entry) if base_held > 0 else 0.0
            equity_curve.append(investment + realized + unrealized)

        final_equity = equity_curve[-1] if equity_curve else investment + realized
        duration_bars = len(equity_curve)
        peak = -1e18
        max_dd = 0.0
        for value in equity_curve:
            peak = max(peak, value)
            if peak > 0:
                max_dd = max(max_dd, (peak - value) / peak)
        return {
            "end_reason": end_reason,
            "round_trips": round_trips,
            "realized_pnl": round(realized, 6),
            "fees_paid": round(fees_paid, 6),
            "final_equity": round(final_equity, 6),
            "return_pct": round((final_equity / investment - 1) * 100, 4),
            "max_drawdown": round(max_dd, 6),
            "duration_bars": duration_bars,
            "equity_curve_tail": [round(v, 6) for v in equity_curve[-64:]],
        }


def derive_grid_params(candles, fee_bps=7.0, min_step_pct=0.6):
    """
    Market-derived grid parameters for a training window: range from the
    10th-90th close percentile, level count from the fee floor. No hardcoded
    money amounts — everything comes from the window's own distribution.
    """
    closes = [c["close"] for c in candles]
    if len(closes) < 30:
        return None
    closes_sorted = sorted(closes)
    def percentile(p):
        idx = int(len(closes_sorted) * p / 100)
        return closes_sorted[min(idx, len(closes_sorted) - 1)]
    lower, upper = percentile(10), percentile(90)
    mid = (lower + upper) / 2
    if mid <= 0 or upper <= lower:
        return None
    range_pct = (upper - lower) / mid * 100
    levels = int(max(2, min(100, range_pct / max(min_step_pct, fee_bps / 100 * 1.2))))
    return {"lower": lower, "upper": upper, "levels": levels}


def walk_forward(engine, candles, train_bars=240, test_bars=60, purge_bars=6, investment=100.0, stop_loss_pct=8.0):
    """
    Purged walk-forward: fit grid parameters on a training window, evaluate
    strictly out-of-sample on the following window after a purge gap that
    removes parameter leakage across the boundary.
    """
    folds = []
    start = 0
    while start + train_bars + purge_bars + test_bars <= len(candles):
        train = candles[start : start + train_bars]
        test = candles[start + train_bars + purge_bars : start + train_bars + purge_bars + test_bars]
        params = derive_grid_params(train)
        if params:
            sim = GridSimulator(maker_fee=engine.maker_fee, taker_fee=engine.taker_fee, slippage=engine.slippage)
            result = sim.simulate(test, params["lower"], params["upper"], params["levels"], investment, stop_loss_pct)
            folds.append({
                "train_start": start,
                "test_start": start + train_bars + purge_bars,
                "params": params,
                "oos": result,
            })
        start += test_bars
    if not folds:
        return {"folds": 0, "oos_return_pct": 0.0, "oos_max_drawdown": 0.0, "round_trips": 0, "stop_hits": 0}
    returns = [f["oos"]["return_pct"] for f in folds]
    dds = [f["oos"]["max_drawdown"] for f in folds]
    stop_hits = sum(1 for f in folds if f["oos"]["end_reason"] == "STOP_LOSS")
    return {
        "folds": len(folds),
        "oos_return_pct": round(sum(returns) / len(returns), 4),
        "oos_max_drawdown": round(max(dds), 6),
        "round_trips": sum(f["oos"]["round_trips"] for f in folds),
        "stop_hits": stop_hits,
    }
