import math
from engine.backtest import GridSimulator, derive_grid_params, walk_forward, QuantBacktestEngine


def make_oscillator(n=300, low=100.0, high=110.0, period=24):
    """Deterministic range oscillation between low and high."""
    candles = []
    for i in range(n):
        phase = 2 * math.pi * (i % period) / period
        price = (low + high) / 2 + (high - low) / 2 * math.sin(phase)
        candles.append({
            "open": price,
            "high": price * 1.001,
            "low": price * 0.999,
            "close": price,
            "volume": 100.0,
        })
    return candles


def test_grid_sim_profitable_in_range():
    candles = make_oscillator()
    sim = GridSimulator(maker_fee=0.0002)
    result = sim.simulate(candles, lower=100.0, upper=110.0, levels=20, investment=100.0)
    assert result["end_reason"] == "COMPLETED"
    assert result["round_trips"] > 0, "oscillation must produce round trips"
    assert result["return_pct"] > 0, "in-range oscillation must be net positive after fees"


def test_grid_sim_stop_loss_on_breakdown():
    candles = make_oscillator(n=120)
    # Append a collapse below the range.
    price = 100.0
    for i in range(30):
        price *= 0.97
        candles.append({"open": price, "high": price * 1.001, "low": price * 0.999,
                        "close": price, "volume": 100.0})
    sim = GridSimulator(maker_fee=0.0002)
    result = sim.simulate(candles, lower=100.0, upper=110.0, levels=20, investment=100.0, stop_loss_pct=5.0)
    assert result["end_reason"] == "STOP_LOSS"
    assert result["duration_bars"] < len(candles), "simulation must stop at the break"


def test_grid_sim_dead_market_no_trades():
    flat = [{"open": 105.0, "high": 105.05, "low": 104.95, "close": 105.0, "volume": 10.0}] * 100
    sim = GridSimulator()
    result = sim.simulate(flat, lower=100.0, upper=110.0, levels=20, investment=100.0)
    assert result["round_trips"] == 0
    assert abs(result["return_pct"]) < 0.01


def test_derive_grid_params_market_driven():
    candles = make_oscillator(low=100.0, high=120.0)
    params = derive_grid_params(candles)
    assert params is not None
    assert 95 <= params["lower"] <= 105, "lower anchor near window lows"
    assert 115 <= params["upper"] <= 125, "upper anchor near window highs"
    assert 2 <= params["levels"] <= 100


def test_walk_forward_structure():
    candles = make_oscillator(n=600)
    engine = QuantBacktestEngine()
    report = walk_forward(engine, candles, train_bars=200, test_bars=60, purge_bars=6)
    assert report["folds"] >= 3, "600 bars must fold at least 3 times"
    assert report["oos_return_pct"] > -1.0
    assert "oos_max_drawdown" in report


def test_walk_forward_survives_trend_regime():
    """A monotone trend must not crash the folds; stops cap the damage."""
    candles = []
    price = 100.0
    for i in range(500):
        price *= 1.002
        candles.append({"open": price, "high": price * 1.001, "low": price * 0.999,
                        "close": price, "volume": 100.0})
    engine = QuantBacktestEngine()
    report = walk_forward(engine, candles, train_bars=200, test_bars=60, purge_bars=6)
    assert report["folds"] >= 1
    assert report["oos_return_pct"] < 5.0, "trending regime must not print fantasy returns"
