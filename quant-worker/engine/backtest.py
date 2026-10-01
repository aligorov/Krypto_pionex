import math
from typing import Dict, List, Any, Optional
import numpy as np

def t_critical_95(df: int) -> float:
    """Two-sided Student's t critical value at alpha=0.05 for degrees of freedom df."""
    if df <= 0:
        return 1.96
    t_table = {
        1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571,
        6: 2.447, 7: 2.365, 8: 2.306, 9: 2.262, 10: 2.228,
        11: 2.201, 12: 2.179, 13: 2.160, 14: 2.145, 15: 2.131,
        16: 2.120, 17: 2.110, 18: 2.101, 19: 2.093, 20: 2.086,
        21: 2.080, 22: 2.074, 23: 2.069, 24: 2.064, 25: 2.060,
        26: 2.056, 27: 2.052, 28: 2.048, 29: 2.045, 30: 2.042,
    }
    return t_table.get(df, 1.96)


def detect_regime(candles: List[Dict[str, Any]]) -> str:
    """Classify the market regime of a sequence of candles."""
    if len(candles) < 2:
        return "RANGE"
    closes = [c["close"] for c in candles]
    highs = [c["high"] for c in candles]
    lows = [c["low"] for c in candles]
    
    start_p = closes[0]
    end_p = closes[-1]
    if start_p <= 0:
        return "RANGE"
    
    ret_pct = (end_p - start_p) / start_p * 100.0
    hl_spread = (max(highs) - min(lows)) / start_p * 100.0
    
    # Count directional changes (flips)
    flips = 0
    for i in range(2, len(closes)):
        diff1 = closes[i - 1] - closes[i - 2]
        diff2 = closes[i] - closes[i - 1]
        if diff1 * diff2 < 0:
            flips += 1
            
    flip_ratio = flips / max(len(closes) - 2, 1)
    
    if ret_pct > 2.5:
        return "TREND_UP"
    elif ret_pct < -2.5:
        return "TREND_DOWN"
    elif hl_spread > 8.0 and flip_ratio > 0.40:
        return "CHOPPY"
    elif hl_spread > 8.0:
        return "HIGH_VOL"
    return "RANGE"


def lag1_autocorr(series: List[float]) -> float:
    """Computes sample lag-1 autocorrelation rho_1 of a trade sequence."""
    if len(series) < 3:
        return 0.0
    arr = np.array(series, dtype=float)
    x = arr[:-1]
    y = arr[1:]
    std_x = float(np.std(x))
    std_y = float(np.std(y))
    if std_x == 0.0 or std_y == 0.0:
        return 0.0
    r = float(np.corrcoef(x, y)[0, 1])
    if np.isnan(r):
        return 0.0
    return max(-0.99, min(0.99, r))


def effective_sample_size(n: int, rho1: float) -> float:
    """
    Adjusts sample size for serial correlation / trade dependence:
    N_eff = N * (1 - rho1) / (1 + rho1).
    When rho1 > 0, trades are positively clustered/dependent -> N_eff < N.
    Conservative: we do not inflate N_eff above N for negative rho1.
    Bounded below by 2.0 (if N >= 2).
    """
    if n < 2:
        return float(n)
    if rho1 > 0:
        n_eff = float(n) * (1.0 - rho1) / (1.0 + rho1)
        return max(2.0, min(float(n), n_eff))
    return float(n)


class QuantBacktestEngine:
    """
    Event-driven backtesting engine with strict intrabar execution logic,
    maker/taker fee modeling, funding rate accounting, and Purged Walk-Forward OOS evaluation.
    """

    def __init__(self, maker_fee: float = 0.0002, taker_fee: float = 0.0005, slippage: float = 0.0005,
                 funding_rate_8h: float = 0.0001):
        self.maker_fee = maker_fee
        self.taker_fee = taker_fee
        self.slippage = slippage
        self.funding_rate_8h = funding_rate_8h

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
                "ci95_lower": 0.0,
                "ci95_upper": 0.0,
                "ci95_positive": False,
                "sample_sufficient": False,
                "autocorr_rho1": 0.0,
                "n_eff": 0.0,
                "is_proxy": True,
                "evaluation_status": "indicative_proxy",
            }

        pnls = [t.get("pnl", 0.0) for t in trades]
        wins = [p for p in pnls if p > 0]
        losses = [abs(p) for p in pnls if p < 0]

        total_trades = len(pnls)
        win_rate = len(wins) / total_trades if total_trades > 0 else 0.0
        expected_value = float(np.mean(pnls)) if total_trades > 0 else 0.0

        gross_profit = sum(wins)
        gross_loss = sum(losses)
        if gross_loss > 0:
            profit_factor = min(gross_profit / gross_loss, 99.0)
        else:
            profit_factor = gross_profit if gross_profit > 0 else 0.0

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

        # 95% Confidence Interval for Net EV with autocorrelation adjustment
        rho_1 = lag1_autocorr(pnls)
        n_eff = effective_sample_size(total_trades, rho_1)
        pnl_std = float(np.std(pnls, ddof=1)) if len(pnls) > 1 else 0.0
        se = (pnl_std / np.sqrt(n_eff)) if n_eff > 1 else 0.0
        df = max(1, int(round(n_eff)) - 1)
        t_crit = t_critical_95(df)
        ci95_lower = expected_value - t_crit * se
        ci95_upper = expected_value + t_crit * se
        ci95_positive = bool(ci95_lower > 0.0)
        sample_sufficient = bool(n_eff >= 15)

        return {
            "total_trades": total_trades,
            "expected_value": round(expected_value, 4),
            "sharpe_ratio": round(sharpe_ratio, 2),
            "sortino_ratio": round(sortino_ratio, 2),
            "max_drawdown": round(max_drawdown, 4),
            "profit_factor": round(profit_factor, 2),
            "win_rate": round(win_rate, 4),
            "ci95_lower": round(ci95_lower, 4),
            "ci95_upper": round(ci95_upper, 4),
            "ci95_positive": ci95_positive,
            "sample_sufficient": sample_sufficient,
            "autocorr_rho1": round(rho_1, 4),
            "n_eff": round(n_eff, 2),
            "is_proxy": True,
            "evaluation_status": "indicative_proxy",
        }


class GridSimulator:
    """
    Event-driven Pionex Futures Grid simulation on OHLCV candles with
    strict intrabar path reconstruction, maker/taker fee modeling, funding
    rate accrual, leverage notional, maintenance margin liquidation checks,
    and partial execution modeling.
    """

    def __init__(self, maker_fee: float = 0.0002, taker_fee: float = 0.0005, slippage: float = 0.0005,
                 funding_rate_8h: float = 0.0001, bar_hours: float = 1.0):
        self.maker_fee = maker_fee
        self.taker_fee = taker_fee
        self.slippage = slippage
        self.funding_rate_8h = funding_rate_8h
        self.bar_hours = bar_hours

    def _candle_path(self, candle):
        open_, high, low, close = candle["open"], candle["high"], candle["low"], candle["close"]
        if close < open_:
            return [open_, high, low, close]
        return [open_, low, high, close]

    def simulate(self, candles, lower, upper, levels, investment, leverage=1.0, direction="neutral",
                 stop_loss_pct=None):
        """
        Simulates grid execution against OHLCV candle sequence.
        
        NOTE: OHLCV simulation is an indicative proxy (is_proxy=True):
        1. Intrabar trajectory is reconstructed via _candle_path heuristic (O-H-L-C or O-L-H-C).
        2. Fills assume 0.5 touch / 1.0 penetration partial execution heuristics.
        3. Perpetual funding accrual uses a constant 8h rate proxy.
        This serves as a relative qualification filter, requiring paper or shadow verification
        before real capital allocation.
        """
        if upper <= lower or levels < 2 or investment <= 0 or not candles:
            return {"error": "invalid parameters", "is_proxy": True, "evaluation_status": "indicative_proxy"}

        leverage = max(float(leverage), 1.0)
        direction = str(direction or "neutral").lower()
        if direction in ("no_trend", "range"):
            direction = "neutral"

        step = (upper - lower) / levels
        total_notional = investment * leverage
        per_level_quote = total_notional / levels
        entry_price = candles[0]["open"]
        if not (lower < entry_price < upper):
            entry_price = min(max(entry_price, lower + step), upper - step)

        open_lots = {}       # level index -> list of (price, base_size, quote_size)
        base_held = 0.0      # signed: positive for long, negative for short
        quote_spent = 0.0
        realized = 0.0
        round_trips = 0
        fees_paid = 0.0
        funding_paid = 0.0
        equity_curve = []
        trades = []
        end_reason = "COMPLETED"

        # v2.0.170 FIX: a directional grid bot enters with an immediate
        # base position at creation (the native Pionex bot opens inventory
        # on the opposite side of entry). Without this, LONG/SHORT
        # candidates were simulated as flat (neutral) and directional
        # inventory/funding/liquidation risk was understated.
        if direction in ("long", "short"):
            base_notional = total_notional / 2.0  # half the notional opens now
            base_base = base_notional / entry_price
            base_fee = base_notional * self.taker_fee
            if direction == "long":
                base_held += base_base
            else:
                base_held -= base_base
            quote_spent += base_notional
            fees_paid += base_fee
            realized -= base_fee

        # Stop price determination
        stop_price = None
        if stop_loss_pct is not None and stop_loss_pct > 0:
            if direction == "short":
                stop_price = upper * (1 + stop_loss_pct / 100.0)
            else:
                stop_price = lower * (1 - stop_loss_pct / 100.0)

        last_price = entry_price
        stop_hit = False
        liquidated = False
        # v2.0.170 FIX: seed the curve at the initial capital so the
        # first-bar PnL enters the return/drawdown calculation.
        equity_curve.append(investment)

        def level_of(price):
            return int((price - lower) / step)

        def fill_between(prev, nxt, candle_high, candle_low, candle_vol):
            nonlocal base_held, quote_spent, realized, round_trips, fees_paid, trades
            if prev == nxt:
                return
            down = nxt < prev
            lo_level = max(0, level_of(min(prev, nxt)))
            hi_level = min(levels - 1, level_of(max(prev, nxt)))

            for lvl in range(lo_level + 1, hi_level + 1):
                price = lower + lvl * step
                if price <= 0:
                    continue

                # Realistic partial execution:
                # If price just touched the level at the candle extreme, partial fill factor 0.5.
                # If price penetrated through, full fill (1.0).
                fill_ratio = 1.0
                if (down and nxt == candle_low and nxt >= price) or (not down and nxt == candle_high and nxt <= price):
                    fill_ratio = 0.5

                notional_to_fill = per_level_quote * fill_ratio

                if direction == "short":
                    # SHORT grid mechanics:
                    # Upward movement sells to open short (or covers at higher levels)
                    # Downward movement buys to cover short
                    if not down:
                        # Upward crossing fills SHORT SELL
                        if open_lots.get(lvl):
                            continue
                        base = notional_to_fill / price
                        fee = notional_to_fill * self.maker_fee
                        open_lots.setdefault(lvl, []).append((price, base, notional_to_fill))
                        base_held -= base
                        quote_spent += notional_to_fill
                        fees_paid += fee
                        realized -= fee
                    else:
                        # Downward crossing covers short bought at level above
                        sell_lvl = lvl + 1
                        if open_lots.get(sell_lvl):
                            entry, lot_base, lot_quote = open_lots[sell_lvl].pop(0)
                            if not open_lots[sell_lvl]:
                                del open_lots[sell_lvl]
                            gross = lot_base * (entry - price) # short gain
                            fee = lot_quote * self.maker_fee + lot_base * price * self.maker_fee
                            fees_paid += fee
                            trade_net = gross - fee
                            realized += trade_net
                            round_trips += 1
                            base_held += lot_base
                            quote_spent -= lot_quote
                            trades.append({"pnl": round(trade_net, 6), "side": "SHORT"})
                else:
                    # NEUTRAL / LONG grid mechanics:
                    # Downward crossing fills BUY
                    # Upward crossing fills SELL
                    if down:
                        if open_lots.get(lvl):
                            continue
                        base = notional_to_fill / price
                        fee = notional_to_fill * self.maker_fee
                        # v2.0.170 FIX: store the ACTUAL fill size with the
                        # price — the close side must settle exactly this
                        # size, not re-derive from per_level_quote.
                        open_lots.setdefault(lvl, []).append((price, base, notional_to_fill))
                        base_held += base
                        quote_spent += notional_to_fill
                        fees_paid += fee
                        realized -= fee
                    else:
                        buy_lvl = lvl - 1
                        if open_lots.get(buy_lvl):
                            # v2.0.170 FIX: close exactly the stored size
                            entry, lot_base, lot_quote = open_lots[buy_lvl].pop(0)
                            if not open_lots[buy_lvl]:
                                del open_lots[buy_lvl]
                            gross = lot_base * (price - entry)
                            fee = lot_quote * self.maker_fee + lot_base * price * self.maker_fee
                            fees_paid += fee
                            trade_net = gross - fee
                            realized += trade_net
                            round_trips += 1
                            base_held -= lot_base
                            quote_spent -= lot_quote
                            trades.append({"pnl": round(trade_net, 6), "side": "LONG"})

        mmr = 0.01 # 1% maintenance margin rate

        for candle in candles:
            c_high = candle["high"]
            c_low = candle["low"]
            c_vol = candle.get("volume", 0.0)
            
            for point in self._candle_path(candle):
                # Stop Loss check
                is_stop = False
                if stop_price is not None:
                    if direction == "short" and point >= stop_price:
                        is_stop = True
                    elif direction != "short" and point <= stop_price:
                        is_stop = True

                if is_stop and not stop_hit and not liquidated:
                    stop_hit = True
                    end_reason = "STOP_LOSS"
                    if abs(base_held) > 0:
                        # Liquidate at taker fee + slippage
                        if base_held > 0:
                            liq_price = point * (1 - self.slippage)
                            avg_entry = quote_spent / base_held if base_held > 0 else point
                            gross = base_held * (liq_price - avg_entry)
                        else:
                            liq_price = point * (1 + self.slippage)
                            avg_entry = quote_spent / abs(base_held) if abs(base_held) > 0 else point
                            gross = abs(base_held) * (avg_entry - liq_price)
                        fee = abs(base_held) * liq_price * self.taker_fee
                        trade_net = gross - fee
                        realized += trade_net
                        fees_paid += fee
                        trades.append({"pnl": round(trade_net, 6), "side": "STOP"})
                        base_held = 0.0
                        quote_spent = 0.0
                        open_lots.clear()
                    last_price = point
                    break

                fill_between(last_price, point, c_high, c_low, c_vol)
                last_price = point

            if stop_hit:
                # v2.0.170 FIX: append the post-stop equity so final_equity
                # and return_pct INCLUDE the stop-loss loss (the old code
                # returned the pre-stop mark-to-market, overstating every
                # stopped run).
                equity_curve.append(max(0.0, investment + realized))
                break

            # Perpetual funding accrual per bar
            if self.funding_rate_8h and abs(base_held) > 0:
                # Longs pay positive funding; shorts receive positive funding
                held_notional = abs(base_held) * last_price
                sign = 1.0 if base_held > 0 else -1.0
                payment = sign * held_notional * self.funding_rate_8h * (self.bar_hours / 8.0)
                realized -= payment
                funding_paid += payment

            # Unrealized PnL
            if abs(base_held) > 0:
                if base_held > 0:
                    avg_entry = quote_spent / base_held
                    unrealized = base_held * (last_price - avg_entry)
                else:
                    avg_entry = quote_spent / abs(base_held)
                    unrealized = abs(base_held) * (avg_entry - last_price)
            else:
                unrealized = 0.0

            current_margin = investment + realized + unrealized
            current_held_notional = abs(base_held) * last_price
            
            # Liquidation check (maintenance margin exhaustion)
            if current_held_notional > 0 and current_margin <= current_held_notional * mmr:
                liquidated = True
                end_reason = "LIQUIDATION"
                # Position wiped out at market
                realized = -investment
                trades.append({"pnl": round(-investment, 6), "side": "LIQUIDATION"})
                equity_curve.append(0.0)
                break

            equity_curve.append(max(0.0, current_margin))

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
            "funding_paid": round(funding_paid, 6),
            "final_equity": round(final_equity, 6),
            "return_pct": round((final_equity / investment - 1) * 100, 4),
            "max_drawdown": round(max_dd, 6),
            "duration_bars": duration_bars,
            "equity_curve": equity_curve,
            "trades": trades,
            "is_proxy": True,
            "evaluation_status": "indicative_proxy",
            "proxy_warning": "OHLCV simulation relies on intra-candle path reconstruction, heuristic fills, and fixed funding; indicative only.",
        }


def derive_grid_params(candles, fee_bps=7.0, min_step_pct=0.35):
    """Market-derived grid parameters for a training window (fallback)."""
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
    fee_floor_pct = fee_bps / 100 * 5.0
    levels = int(max(6, min(500, range_pct / max(min_step_pct, fee_floor_pct))))
    return {"lower": lower, "upper": upper, "levels": levels}


def walk_forward(engine: QuantBacktestEngine, candles: List[Dict[str, Any]],
                 train_bars: int = 240, test_bars: int = 60, purge_bars: int = 6,
                 investment: float = 100.0, stop_loss_pct: float = 8.0,
                 bar_hours: float = 1.0, deployed_params: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
    """
    Purged walk-forward evaluation across sequential time periods and diverse market regimes.
    Historical fold parameters are calculated strictly on the training window without peeking into OOS.
    When deployed_params are provided, candidate exact parameters are separately evaluated on subsequent data.
    Trade dependence (autocorrelation) is incorporated into Net EV confidence interval (N_eff).
    OHLCV simulation is flagged as an indicative proxy (is_proxy=True).
    """
    if not candles:
        return {
            "folds": 0, "oos_return_pct": 0.0, "oos_max_drawdown": 0.0,
            "round_trips": 0, "stop_hits": 0, "net_ev": 0.0,
            "ci95_lower": 0.0, "ci95_upper": 0.0, "ci95_positive": False,
            "sample_sufficient": False, "oos_sharpe": 0.0, "oos_sharpe_valid": False,
            "oos_sortino": 0.0, "oos_sortino_valid": False, "win_rate": 0.0,
            "profit_factor": 0.0, "turnover": 0.0, "regimes_tested": [],
            "liquidity_ok": False, "liquidity_reason": "no candles provided",
            "worst_period": None, "is_proxy": True, "evaluation_status": "indicative_proxy",
            "proxy_warning": "OHLCV simulation relies on intra-candle path reconstruction, heuristic fills, and fixed funding; indicative only.",
        }

    folds = []
    all_trades = []
    all_equity = []

    total_bars = len(candles)
    eff_test_bars = test_bars
    eff_purge_bars = purge_bars
    eff_train_bars = train_bars
    if total_bars < eff_train_bars + eff_purge_bars + eff_test_bars and total_bars >= 40:
        eff_test_bars = max(10, min(test_bars, total_bars // 4))
        eff_purge_bars = min(purge_bars, max(1, eff_test_bars // 5))
        eff_train_bars = max(20, min(train_bars, (total_bars - eff_purge_bars - eff_test_bars) // 3))

    use_exact_params = bool(deployed_params and "lower" in deployed_params and "upper" in deployed_params and "levels" in deployed_params)

    # 1. Historical Purged Walk-Forward: parameters derived strictly on in-sample train window without peeking
    start = 0
    fold_idx = 0
    while start + eff_train_bars + eff_purge_bars + eff_test_bars <= total_bars:
        train = candles[start : start + eff_train_bars]
        test = candles[start + eff_train_bars + eff_purge_bars : start + eff_train_bars + eff_purge_bars + eff_test_bars]
        params = derive_grid_params(train, fee_bps=(engine.maker_fee + engine.slippage) * 1e4)
        if params:
            regime = detect_regime(test)
            sim = GridSimulator(
                maker_fee=engine.maker_fee, taker_fee=engine.taker_fee, slippage=engine.slippage,
                funding_rate_8h=engine.funding_rate_8h, bar_hours=bar_hours
            )
            # Historical walk-forward simulation of derived strategy parameters on out-of-sample test window
            res = sim.simulate(
                test, params["lower"], params["upper"], params["levels"], investment,
                stop_loss_pct=stop_loss_pct
            )
            if "error" not in res:
                fold_data = {
                    "fold_idx": fold_idx + 1,
                    "test_start": start + eff_train_bars + eff_purge_bars,
                    "test_end": start + eff_train_bars + eff_purge_bars + eff_test_bars,
                    "regime": regime,
                    "return_pct": res["return_pct"],
                    "max_drawdown": res["max_drawdown"],
                    "round_trips": res["round_trips"],
                    "stop_hit": res["end_reason"] in ("STOP_LOSS", "LIQUIDATION"),
                    "end_reason": res["end_reason"],
                    "realized_pnl": res["realized_pnl"],
                    "fees_paid": res["fees_paid"],
                    "funding_paid": res["funding_paid"],
                    "final_equity": res["final_equity"],
                    "trades": res.get("trades", []),
                }
                folds.append(fold_data)
                if not use_exact_params:
                    all_trades.extend(res.get("trades", []))
                    all_equity.extend(res.get("equity_curve", []))
        start += eff_test_bars
        fold_idx += 1

    # 2. Exact Candidate Parameter Evaluation on Subsequent / Out-Of-Sample Data
    candidate_evaluation = None
    if use_exact_params:
        cand_sim = GridSimulator(
            maker_fee=engine.maker_fee, taker_fee=engine.taker_fee, slippage=engine.slippage,
            funding_rate_8h=engine.funding_rate_8h, bar_hours=bar_hours
        )
        # Evaluate candidate across out-of-sample slices post initial train window
        cand_start = eff_train_bars + eff_purge_bars if total_bars > eff_train_bars + eff_purge_bars else 0
        cand_slice = candles[cand_start:]
        cand_res = cand_sim.simulate(
            cand_slice,
            lower=deployed_params["lower"],
            upper=deployed_params["upper"],
            levels=deployed_params["levels"],
            investment=deployed_params.get("investment", investment),
            leverage=deployed_params.get("leverage", 1.0),
            direction=deployed_params.get("direction", "neutral"),
            stop_loss_pct=deployed_params.get("stop_loss_pct", stop_loss_pct),
        )
        if "error" not in cand_res:
            c_trades = cand_res.get("trades", [])
            all_trades = c_trades
            all_equity = cand_res.get("equity_curve", [])
            cand_pnls = [t.get("pnl", 0.0) for t in c_trades]
            c_rho = lag1_autocorr(cand_pnls)
            c_neff = effective_sample_size(len(cand_pnls), c_rho)
            c_ev = float(np.mean(cand_pnls)) if cand_pnls else 0.0
            c_std = float(np.std(cand_pnls, ddof=1)) if len(cand_pnls) > 1 else 0.0
            c_se = c_std / np.sqrt(c_neff) if c_neff > 1 else 0.0
            c_tcrit = t_critical_95(max(1, int(round(c_neff)) - 1))
            candidate_evaluation = {
                "lower": deployed_params["lower"],
                "upper": deployed_params["upper"],
                "levels": deployed_params["levels"],
                "leverage": deployed_params.get("leverage", 1.0),
                "direction": deployed_params.get("direction", "neutral"),
                "investment": deployed_params.get("investment", investment),
                "return_pct": cand_res["return_pct"],
                "max_drawdown": cand_res["max_drawdown"],
                "round_trips": cand_res["round_trips"],
                "end_reason": cand_res["end_reason"],
                "realized_pnl": cand_res["realized_pnl"],
                "fees_paid": cand_res["fees_paid"],
                "funding_paid": cand_res["funding_paid"],
                "net_ev": round(c_ev, 4),
                "ci95_lower": round(c_ev - c_tcrit * c_se, 4),
                "ci95_upper": round(c_ev + c_tcrit * c_se, 4),
                "ci95_positive": bool((c_ev - c_tcrit * c_se) > 0.0),
                "autocorr_rho1": round(c_rho, 4),
                "n_eff": round(c_neff, 2),
                "is_proxy": True,
                "evaluation_status": "indicative_proxy",
            }

    if not folds:
        return {
            "folds": 0, "oos_return_pct": 0.0, "oos_max_drawdown": 0.0,
            "round_trips": 0, "stop_hits": 0, "net_ev": 0.0,
            "ci95_lower": 0.0, "ci95_upper": 0.0, "ci95_positive": False,
            "sample_sufficient": False, "oos_sharpe": 0.0, "oos_sharpe_valid": False,
            "oos_sortino": 0.0, "oos_sortino_valid": False, "win_rate": 0.0,
            "profit_factor": 0.0, "turnover": 0.0, "regimes_tested": [],
            "liquidity_ok": False, "liquidity_reason": "no valid walk-forward folds formed",
            "worst_period": None, "is_proxy": True, "evaluation_status": "indicative_proxy",
            "proxy_warning": "OHLCV simulation relies on intra-candle path reconstruction, heuristic fills, and fixed funding; indicative only.",
        }

    returns = [f["return_pct"] for f in folds]
    dds = [f["max_drawdown"] for f in folds]
    stop_hits = sum(1 for f in folds if f["stop_hit"])
    round_trips = sum(f["round_trips"] for f in folds)
    if use_exact_params and candidate_evaluation:
        round_trips = candidate_evaluation["round_trips"]
        # v2.0.172 (audit): when exact candidate parameters are evaluated,
        # the report's return/drawdown MUST come from that evaluation, NOT
        # from the historical fold averages (which used different grid
        # parameters). The old mixing showed DOGE 1.02% DD from historical
        # folds while the exact candidate's true DD was 17.92%.
        returns = [candidate_evaluation["return_pct"]]
        dds = [candidate_evaluation["max_drawdown"]]
        # v2.0.172b: stop_hits, worst_period and regimes must also come from
        # the exact candidate evaluation, not the historical folds (the old
        # mixing showed stop_hits=0 when the candidate actually stopped).
        stop_hits = 1 if candidate_evaluation.get("end_reason") in ("STOP_LOSS", "LIQUIDATION") else 0
        worst_period = {
            "fold": 1,
            "regime": detect_regime(candles[:eff_train_bars]) if total_bars > eff_train_bars else "RANGE",
            "return_pct": candidate_evaluation["return_pct"],
            "max_drawdown": candidate_evaluation["max_drawdown"],
            "round_trips": candidate_evaluation["round_trips"],
            "stop_hit": stop_hits > 0,
        }

    # Trade-level statistical metrics with autocorrelation (trade dependence) adjustment
    pnls = [t.get("pnl", 0.0) for t in all_trades]
    total_trades = len(pnls)
    expected_value = float(np.mean(pnls)) if total_trades > 0 else 0.0

    rho_1 = lag1_autocorr(pnls)
    n_eff = effective_sample_size(total_trades, rho_1)
    pnl_std = float(np.std(pnls, ddof=1)) if total_trades > 1 else 0.0
    se = (pnl_std / np.sqrt(n_eff)) if n_eff > 1 else 0.0
    df = max(1, int(round(n_eff)) - 1)
    t_crit = t_critical_95(df)
    ci95_lower = expected_value - t_crit * se
    ci95_upper = expected_value + t_crit * se
    ci95_positive = bool(ci95_lower > 0.0)
    sample_sufficient = bool(n_eff >= 15 and len(folds) >= 3)

    # Win rate & Profit factor
    wins = [p for p in pnls if p > 0]
    losses = [abs(p) for p in pnls if p < 0]
    win_rate = (len(wins) / total_trades * 100.0) if total_trades > 0 else 0.0
    win_rate_valid = bool(len(losses) > 0)

    gross_profit = sum(wins)
    gross_loss = sum(losses)
    if gross_loss > 0:
        profit_factor = min(gross_profit / gross_loss, 99.0)
        profit_factor_valid = True
    else:
        profit_factor = 0.0
        profit_factor_valid = False

    # Out-of-sample Sharpe and Sortino from equity curve
    oos_sharpe = 0.0
    oos_sharpe_valid = False
    oos_sortino = 0.0
    oos_sortino_valid = False

    if len(all_equity) > 2:
        eq = np.array(all_equity)
        denom = eq[:-1]
        denom[denom <= 0] = 1.0
        eq_returns = np.diff(eq) / denom
        std_ret = float(np.std(eq_returns))
        if std_ret > 0:
            oos_sharpe = float(np.mean(eq_returns)) / std_ret * np.sqrt(365 * 24 / max(bar_hours, 0.1))
            oos_sharpe_valid = True

        downside = eq_returns[eq_returns < 0]
        if len(downside) >= 2:
            std_down = float(np.std(downside))
            if std_down > 0:
                oos_sortino = float(np.mean(eq_returns)) / std_down * np.sqrt(365 * 24 / max(bar_hours, 0.1))
                oos_sortino_valid = True

    # Turnover
    active_inv = deployed_params.get("investment", investment) if deployed_params else investment
    active_lev = deployed_params.get("leverage", 1.0) if deployed_params else 1.0
    active_levels = deployed_params.get("levels", 20) if deployed_params else 20
    notional_per_level = (active_inv * active_lev) / max(active_levels, 1)
    turnover = (float(round_trips) * 2.0 * notional_per_level) / max(active_inv, 1.0)

    # Worst test period — v2.0.172c: when exact candidate params are used,
    # the candidate's worst_period was already set above; do NOT overwrite
    # it with the fold-based calculation (audit: candidate DD 79.97% showed
    # as worst_period DD 0.46% from a historical fold).
    if not (use_exact_params and candidate_evaluation):
        worst_fold = min(folds, key=lambda f: f["return_pct"])
        worst_period = {
            "fold": worst_fold["fold_idx"],
            "regime": worst_fold["regime"],
            "return_pct": worst_fold["return_pct"],
            "max_drawdown": worst_fold["max_drawdown"],
            "round_trips": worst_fold["round_trips"],
            "stop_hit": worst_fold["stop_hit"],
        }

    # Liquidity check: level notional vs candle volume
    avg_candle_vol = 0.0
    if candles:
        vols = [c.get("volume", 0.0) * c.get("close", 0.0) for c in candles]
        avg_candle_vol = float(np.mean(vols)) if vols else 0.0

    liquidity_ok = True
    liquidity_reason = "OK"
    if avg_candle_vol > 0 and notional_per_level > 0.10 * avg_candle_vol:
        liquidity_ok = False
        liquidity_reason = f"order size (${notional_per_level:.2f}) > 10% of avg candle volume (${avg_candle_vol:.2f})"

    # v2.0.172c: regimes from the candidate's own evaluation period when
    # exact params are used, not from historical folds.
    if use_exact_params and candidate_evaluation:
        regimes_tested = [worst_period.get("regime", "RANGE")]
    else:
        regimes_tested = sorted(list(set(f["regime"] for f in folds)))

    report = {
        "engine_version": "v172",
        "folds": len(folds),
        "oos_return_pct": round(float(np.mean(returns)), 4),
        "oos_max_drawdown": round(max(dds), 6),
        "round_trips": round_trips,
        "stop_hits": stop_hits,
        "net_ev": round(expected_value, 4),
        "ci95_lower": round(ci95_lower, 4),
        "ci95_upper": round(ci95_upper, 4),
        "ci95_positive": ci95_positive,
        "sample_sufficient": sample_sufficient,
        "oos_sharpe": round(oos_sharpe, 2),
        "oos_sharpe_valid": oos_sharpe_valid,
        "oos_sortino": round(oos_sortino, 2),
        "oos_sortino_valid": oos_sortino_valid,
        "win_rate": round(win_rate, 2),
        "profit_factor": round(profit_factor, 2),
        "turnover": round(turnover, 2),
        "worst_period": worst_period,
        "regimes_tested": regimes_tested,
        "liquidity_ok": liquidity_ok,
        "liquidity_reason": liquidity_reason,
        "is_proxy": True,
        "evaluation_status": "indicative_proxy",
        "proxy_warning": "OHLCV simulation relies on intra-candle path reconstruction, heuristic fills, and fixed funding; indicative only.",
        "autocorr_rho1": round(rho_1, 4),
        "n_eff": round(n_eff, 2),
        "historical_folds": folds,
    }
    if candidate_evaluation is not None:
        report["exact_candidate_evaluation"] = candidate_evaluation
    return report
