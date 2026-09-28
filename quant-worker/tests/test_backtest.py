from engine.backtest import QuantBacktestEngine, GridSimulator, walk_forward, detect_regime, t_critical_95

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
    assert "ci95_lower" in metrics
    assert "ci95_upper" in metrics
    assert metrics["ci95_upper"] > metrics["ci95_lower"]


def test_t_critical():
    assert t_critical_95(15) == 2.131
    assert t_critical_95(30) == 2.042
    assert t_critical_95(100) == 1.96


def test_regime_detection():
    # Trend up candles
    up_candles = [
        {"open": 100 + i, "high": 101 + i, "low": 99.5 + i, "close": 100.8 + i}
        for i in range(20)
    ]
    assert detect_regime(up_candles) == "TREND_UP"

    # Trend down candles
    down_candles = [
        {"open": 120 - i, "high": 120.5 - i, "low": 119 - i, "close": 119.2 - i}
        for i in range(20)
    ]
    assert detect_regime(down_candles) == "TREND_DOWN"


def test_grid_simulator_leverage_and_fees():
    sim = GridSimulator(maker_fee=0.0002, taker_fee=0.0005, slippage=0.0005, funding_rate_8h=0.0001)
    candles = [
        {"open": 100.0, "high": 105.0, "low": 95.0, "close": 102.0, "volume": 1000.0},
        {"open": 102.0, "high": 108.0, "low": 98.0, "close": 100.0, "volume": 1000.0},
        {"open": 100.0, "high": 104.0, "low": 96.0, "close": 101.0, "volume": 1000.0},
    ]
    res = sim.simulate(candles, lower=90.0, upper=110.0, levels=10, investment=50.0, leverage=4.0)
    assert "error" not in res
    assert res["round_trips"] > 0
    assert res["fees_paid"] > 0
    assert len(res["trades"]) > 0


def test_walk_forward_deployed_params():
    engine = QuantBacktestEngine()
    # Generate 150 alternating candles
    candles = []
    price = 100.0
    for i in range(150):
        low = price - 2.0
        high = price + 2.0
        close = price + (1.0 if i % 2 == 0 else -1.0)
        candles.append({"open": price, "high": high, "low": low, "close": close, "volume": 5000.0})
        price = close

    deployed = {
        "lower": 90.0,
        "upper": 110.0,
        "levels": 10,
        "leverage": 2.0,
        "investment": 100.0,
        "direction": "neutral",
        "stop_loss_pct": 10.0,
    }

    report = walk_forward(engine, candles, test_bars=30, deployed_params=deployed)
    assert report["folds"] >= 3
    assert "ci95_lower" in report
    assert "worst_period" in report
    assert report["worst_period"] is not None
    assert "regimes_tested" in report
    assert "turnover" in report
    assert report["is_proxy"] is True
    assert report["evaluation_status"] == "indicative_proxy"
    assert "historical_folds" in report
    assert "exact_candidate_evaluation" in report
    assert report["exact_candidate_evaluation"]["levels"] == 10


def test_autocorr_effective_sample_size():
    from engine.backtest import lag1_autocorr, effective_sample_size
    # Perfectly alternating series has negative autocorrelation -> no artificial inflation
    alt = [1.0, -1.0, 1.0, -1.0, 1.0, -1.0]
    rho_alt = lag1_autocorr(alt)
    assert rho_alt < 0
    assert effective_sample_size(len(alt), rho_alt) == float(len(alt))

    # Strongly clustered series has high positive autocorrelation -> N_eff < N
    clustered = [1.0, 1.1, 1.05, 0.95, 1.0, 0.9, -1.0, -1.1, -1.05, -0.95, -1.0, -0.9]
    rho_clust = lag1_autocorr(clustered)
    assert rho_clust > 0.5
    neff = effective_sample_size(len(clustered), rho_clust)
    assert neff < len(clustered) / 2
    assert neff >= 2.0


def test_simulation_marked_as_proxy():
    sim = GridSimulator()
    candles = [
        {"open": 100.0, "high": 105.0, "low": 95.0, "close": 102.0, "volume": 1000.0},
        {"open": 102.0, "high": 108.0, "low": 98.0, "close": 100.0, "volume": 1000.0},
    ]
    res = sim.simulate(candles, lower=90.0, upper=110.0, levels=10, investment=50.0)
    assert res["is_proxy"] is True
    assert res["evaluation_status"] == "indicative_proxy"
    assert "proxy_warning" in res

