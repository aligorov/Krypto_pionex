#!/usr/bin/env python3
"""
Train the regime classifier (LightGBM -> ONNX) for the Smart Grid Engine.

Reads features from PostgreSQL, labels each sample with the documented
rule-based regime definition, trains with walk-forward validation and
exports the model to ONNX for Go-side inference (backend/internal/marketdata).

Expected database schema (smart-data collector tables from
migrations/0023_smart_data.sql, plus a klines history table the collector
stack is expected to fill):

    klines (
        symbol   TEXT NOT NULL,
        ts       TIMESTAMPTZ NOT NULL,
        open     NUMERIC, high NUMERIC, low NUMERIC, close NUMERIC,
        volume   NUMERIC
    )
    funding_snapshots (
        symbol       VARCHAR(32) NOT NULL,
        exchange     VARCHAR(16) NOT NULL,
        funding_rate NUMERIC(12,10) NOT NULL,   -- per-8h rate, e.g. 0.0001
        captured_at  TIMESTAMPTZ NOT NULL
    )
    oi_history (
        symbol      VARCHAR(32) NOT NULL,
        exchange    VARCHAR(16) NOT NULL,
        oi_usd      NUMERIC(30,10),
        captured_at TIMESTAMPTZ NOT NULL
    )
    sentiment_snapshots (
        source      VARCHAR(16) NOT NULL DEFAULT 'fng',
        value       NUMERIC(8,2),          -- 0..100
        captured_at TIMESTAMPTZ NOT NULL
    )
    economic_events (
        title      VARCHAR(255) NOT NULL,
        event_time TIMESTAMPTZ NOT NULL,
        impact     VARCHAR(8) NOT NULL     -- 'High' | 'Medium' | 'Low'
    )

The feature vector MUST stay in sync with MLFeatures in
backend/internal/marketdata/regime_ml.go (FeatureNames order).

Usage:
    python3 training/train_regime.py \
        --db-url postgres://user:pass@localhost:5432/pionex \
        --days 90 --output regime_v1.onnx

Requirements:
    pip install psycopg2-binary numpy lightgbm scikit-learn \\
                onnx onnxmltools onnxconverter-common
"""

import argparse
import logging
import math
import sys
from datetime import datetime, timedelta, timezone

import numpy as np
import psycopg2
import psycopg2.extras
import lightgbm as lgb
from lightgbm import LGBMClassifier
from onnx import save_model as onnx_save_model
from onnxconverter_common.data_types import FloatTensorType
from onnxmltools import convert_lightgbm
from sklearn.metrics import accuracy_score, log_loss

log = logging.getLogger("train_regime")

# Canonical regime classes. The Go side decodes the same indices.
REGIME_CLASSES = {0: "RANGE", 1: "TREND_UP", 2: "TREND_DOWN", 3: "CRASH"}
NUM_CLASSES = len(REGIME_CLASSES)

# Canonical feature order — must match FeatureNames() in regime_ml.go.
FEATURE_NAMES = [
    "funding_avg",
    "funding_extreme",
    "oi_change_24h",
    "oi_rising",
    "realized_vol_daily",
    "har_forecast",
    "hurst_exponent",
    "adx14",
    "choppiness14",
    "bbw_percentile",
    "price_change_24h",
    "price_change_7d",
    "fear_greed",
    "high_impact_event",
]

REQUIRED_TABLES = (
    "klines",
    "funding_snapshots",
    "oi_history",
    "sentiment_snapshots",
    "economic_events",
)

# Sampling granularity: one training row per symbol per hour.
BUCKET = "1 hour"


def table_exists(conn, name: str) -> bool:
    """Return True when a relation with the given name exists in the public schema."""
    with conn.cursor() as cur:
        cur.execute(
            """
            SELECT EXISTS (
                SELECT 1 FROM information_schema.tables
                WHERE table_schema = 'public' AND table_name = %s
            )
            """,
            (name,),
        )
        return cur.fetchone()[0]


def require_tables(conn) -> None:
    """Fail fast with a readable message when collector tables are missing."""
    missing = [t for t in REQUIRED_TABLES if not table_exists(conn, t)]
    if missing:
        raise SystemExit(
            "Missing required tables: " + ", ".join(missing) +
            ". Start the market-data collectors first (funding/OI/sentiment/"
            "events/klines) — the trainer does not create schemas."
        )


# ---------------------------------------------------------------------------
# Technical indicators (numpy, mirroring the Go implementations where the Go
# side recomputes them at inference time).
# ---------------------------------------------------------------------------

def ema(values: np.ndarray, period: int) -> np.ndarray:
    """Exponential moving average over the full series."""
    alpha = 2.0 / (period + 1.0)
    out = np.empty_like(values)
    out[0] = values[0]
    for i in range(1, len(values)):
        out[i] = alpha * values[i] + (1.0 - alpha) * out[i - 1]
    return out


def adx(high: np.ndarray, low: np.ndarray, close: np.ndarray, period: int = 14) -> float:
    """Wilder Average Directional Index. Returns the latest ADX value (0..100)."""
    n = len(close)
    if n < period * 2 + 1:
        return 0.0
    prev_close = np.concatenate(([close[0]], close[:-1]))
    tr = np.maximum(high - low, np.maximum(np.abs(high - prev_close), np.abs(low - prev_close)))
    up = high - np.concatenate(([high[0]], high[:-1]))
    dn = np.concatenate(([low[0]], low[:-1])) - low
    plus_dm = np.where((up > dn) & (up > 0), up, 0.0)
    minus_dm = np.where((dn > up) & (dn > 0), dn, 0.0)

    tr_s = tr[1:period + 1].sum()
    p_s = plus_dm[1:period + 1].sum()
    m_s = minus_dm[1:period + 1].sum()
    dxs = []
    for i in range(period + 1, n):
        tr_s = tr_s - tr_s / period + tr[i]
        p_s = p_s - p_s / period + plus_dm[i]
        m_s = m_s - m_s / period + minus_dm[i]
        if tr_s > 0:
            pdi = 100.0 * p_s / tr_s
            mdi = 100.0 * m_s / tr_s
            if pdi + mdi > 0:
                dxs.append(min(100.0, abs(pdi - mdi) / (pdi + mdi) * 100.0))
    if not dxs:
        return 0.0
    if len(dxs) < period:
        return float(np.mean(dxs))
    adx_value = float(np.mean(dxs[:period]))
    for dx in dxs[period:]:
        adx_value = (adx_value * (period - 1) + dx) / period
    return min(100.0, max(0.0, adx_value))


def choppiness(high: np.ndarray, low: np.ndarray, close: np.ndarray, period: int = 14) -> float:
    """Dreiss Choppiness Index (>61.8 range, <38.2 trend)."""
    n = len(close)
    if n < period + 1:
        return 50.0
    window = slice(n - period, n)
    prev_close = close[n - period - 1:n - 1]
    tr = np.maximum(
        high[window] - low[window],
        np.maximum(np.abs(high[window] - prev_close), np.abs(low[window] - prev_close)),
    )
    hl = high[window].max() - low[window].min()
    if hl <= 0 or tr.sum() <= 0:
        return 50.0
    value = 100.0 * (math.log10(tr.sum() / hl) / math.log10(period))
    return min(100.0, max(0.0, value))


def bbw_percentile(close: np.ndarray, period: int = 20, rank_window: int = 240) -> float:
    """Percentile rank (0..100) of the current Bollinger Band Width in its window."""
    n = len(close)
    if n < period + 12:
        return 50.0

    def bbw(end: int) -> float:
        w = close[end - period:end]
        sma = w.mean()
        if sma <= 0:
            return 0.0
        return (4.0 * w.std(ddof=1) / sma) * 100.0

    current = bbw(n)
    samples = 0
    at_or_below = 0
    for end in range(period + 2, n + 1):
        samples += 1
        if bbw(end) <= current:
            at_or_below += 1
    if samples < 10:
        return 50.0
    return at_or_below / samples * 100.0


def realized_vol_daily(close: np.ndarray, bars_per_day: int) -> float:
    """Daily realized volatility in percent (sum of squared log returns over the last day)."""
    if len(close) < 2 or bars_per_day <= 0:
        return 0.0
    rets = np.diff(np.log(close[-(bars_per_day + 1):]))
    if len(rets) == 0:
        return 0.0
    return float(np.sqrt(np.sum(rets ** 2)) * 100.0)


def har_forecast(close: np.ndarray, bars_per_day: int) -> float:
    """Corsi HAR-RV forecast (daily RV) blended 0.5/0.3/0.2 over the
    daily/weekly/monthly realized-variance components, returned as daily vol %.
    Weights fixed to the literature defaults: the model learns around them."""
    if len(close) < 2 or bars_per_day <= 0:
        return 0.0
    rets = np.diff(np.log(close))
    per_bar = float(np.mean(rets ** 2)) if len(rets) else 0.0
    chunks = []
    for days in (1, 5, 22):
        take = min(len(rets), days * bars_per_day)
        if take <= 0:
            continue
        rv = float(np.sum(rets[-take:] ** 2) / days)
        chunks.append(rv)
    if not chunks:
        return math.sqrt(max(per_bar * bars_per_day, 0.0)) * 100.0
    # Weighted HAR combination over the components that actually have data.
    weights = {"d": 0.5, "w": 0.3, "m": 0.2}
    total_w, acc = 0.0, 0.0
    for rv, key in zip(chunks, ("d", "w", "m")):
        acc += weights[key] * rv
        total_w += weights[key]
    if total_w <= 0:
        return 0.0
    return math.sqrt(max(acc / total_w, 0.0)) * 100.0


def hurst_dfa(close: np.ndarray) -> float:
    """Hurst exponent via Detrended Fluctuation Analysis (DFA1) on log returns.
    Mirrors HurstDFA in Go: ~0.5 random walk, <0.45 mean-reverting, >0.58 trending."""
    n = len(close)
    if n < 64:
        return 0.5
    rets = np.diff(np.log(close))
    max_scale = min(len(rets) // 4, 128)
    scales = [s for s in (8, 16, 32, 64, 128) if s <= max_scale]
    if len(scales) < 3:
        return 0.5
    log_scales, log_f = [], []
    for scale in scales:
        usable = len(rets) - len(rets) % scale
        segments = rets[:usable].reshape(-1, scale)
        profile = np.cumsum(segments - segments.mean(axis=1, keepdims=True), axis=1)
        x = np.arange(scale, dtype=float)
        # Least-squares linear detrend per segment.
        xm, ym = x.mean(), profile.mean(axis=1)
        denom = np.sum((x - xm) ** 2)
        if denom == 0:
            continue
        slopes = np.sum((x - xm) * (profile - ym[:, None]), axis=1) / denom
        fitted = ym[:, None] + slopes[:, None] * x[None, :]
        rms = np.sqrt(np.mean((profile - fitted) ** 2))
        if rms <= 0:
            continue
        log_scales.append(math.log(scale))
        log_f.append(math.log(rms))
    if len(log_scales) < 3:
        return 0.5
    ls = np.array(log_scales)
    lf = np.array(log_f)
    denom = len(ls) * np.sum(ls * ls) - ls.sum() ** 2
    if denom == 0:
        return 0.5
    hurst = (len(ls) * np.sum(ls * lf) - ls.sum() * lf.sum()) / denom
    if not np.isfinite(hurst) or hurst < 0.1 or hurst > 1.2:
        return 0.5
    return float(min(1.0, max(0.1, hurst)))


def max_drawdown(close: np.ndarray) -> float:
    """Peak-to-trough drawdown of the window in percent (positive number)."""
    if len(close) < 2:
        return 0.0
    peak = np.maximum.accumulate(close)
    dd = (peak - close) / peak * 100.0
    return float(dd.max())


def price_change(close: np.ndarray, bars: int) -> float:
    """Percent change between the last close and the close `bars` back."""
    if len(close) < 2 or bars <= 0:
        return 0.0
    ref = close[max(0, len(close) - 1 - bars)]
    if ref <= 0:
        return 0.0
    return float((close[-1] - ref) / ref * 100.0)


# ---------------------------------------------------------------------------
# Feature loading
# ---------------------------------------------------------------------------

def load_features(conn, days: int = 90, symbol: str | None = None, bars_per_day: int = 96):
    """Load the feature matrix from PostgreSQL.

    Returns (features, labels, names, timestamps) where `timestamps` is the
    per-row bucket time used for the walk-forward split.
    """
    since = datetime.now(timezone.utc) - timedelta(days=days)
    symbol_filter = "AND k.symbol = %(symbol)s" if symbol else ""

    # Hourly OHLCV base frame.
    kline_sql = f"""
        SELECT k.symbol,
               date_trunc(%(bucket)s, k.ts) AS bucket,
               ARRAY_AGG(k.open ORDER BY k.ts)  AS opens,
               ARRAY_AGG(k.high ORDER BY k.ts)  AS highs,
               ARRAY_AGG(k.low ORDER BY k.ts)   AS lows,
               ARRAY_AGG(k.close ORDER BY k.ts) AS closes
        FROM klines k
        WHERE k.ts >= %(since)s {symbol_filter}
        GROUP BY k.symbol, bucket
        ORDER BY k.symbol, bucket
    """
    funding_sql = """
        SELECT symbol, date_trunc(%(bucket)s, captured_at) AS bucket, AVG(funding_rate) AS avg_rate
        FROM funding_snapshots
        WHERE captured_at >= %(since)s
        GROUP BY symbol, bucket
    """
    oi_sql = """
        SELECT symbol, date_trunc(%(bucket)s, captured_at) AS bucket, AVG(oi_usd) AS avg_oi
        FROM oi_history
        WHERE captured_at >= %(since)s AND oi_usd IS NOT NULL
        GROUP BY symbol, bucket
    """
    fng_sql = """
        SELECT date_trunc(%(bucket)s, captured_at) AS bucket, AVG(value) AS fng
        FROM sentiment_snapshots
        WHERE captured_at >= %(since)s AND source = 'fng' AND value IS NOT NULL
        GROUP BY bucket
    """
    event_sql = """
        SELECT date_trunc(%(bucket)s, event_time) AS bucket, COUNT(*) AS high_events
        FROM economic_events
        WHERE event_time >= %(since)s AND impact = 'High'
        GROUP BY bucket
    """

    params = {"bucket": BUCKET, "since": since}
    if symbol:
        params["symbol"] = symbol

    funding = {}
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(funding_sql, params)
        for row in cur.fetchall():
            funding[(row["symbol"], row["bucket"])] = float(row["avg_rate"])

    oi = {}
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(oi_sql, params)
        for row in cur.fetchall():
            oi[(row["symbol"], row["bucket"])] = float(row["avg_oi"])

    fng = {}
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(fng_sql, {"bucket": BUCKET, "since": since})
        for row in cur.fetchall():
            fng[row["bucket"]] = float(row["fng"])

    events = {}
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(event_sql, {"bucket": BUCKET, "since": since})
        for row in cur.fetchall():
            events[row["bucket"]] = int(row["high_events"])

    # klines ordered by (symbol, bucket); group once, then stream each
    # symbol's buckets through a running candle history (O(rows)).
    rows = []
    with conn.cursor(cursor_factory=psycopg2.extras.DictCursor) as cur:
        cur.execute(kline_sql, params)
        rows = cur.fetchall()

    by_symbol: dict[str, list] = {}
    for row in rows:
        by_symbol.setdefault(row["symbol"], []).append(row)

    min_candles = max(200, bars_per_day * 2)
    history_cap = bars_per_day * 30  # cap indicator memory at ~30 days
    features, labels, timestamps = [], [], []

    for sym, sym_rows in by_symbol.items():
        hist_close: list[float] = []
        hist_high: list[float] = []
        hist_low: list[float] = []
        oi_series: list[tuple[datetime, float]] = []

        for row in sym_rows:
            bucket = row["bucket"]
            hist_close.extend(float(v) for v in row["closes"])
            hist_high.extend(float(v) for v in row["highs"])
            hist_low.extend(float(v) for v in row["lows"])
            if len(hist_close) < min_candles:
                continue

            window = slice(len(hist_close) - history_cap, None)
            all_closes = np.array(hist_close[window], dtype=float)
            all_highs = np.array(hist_high[window], dtype=float)
            all_lows = np.array(hist_low[window], dtype=float)

            funding_avg = funding.get((sym, bucket), 0.0)
            funding_extreme = 1.0 if abs(funding_avg) > 0.001 else 0.0

            oi_series.append((bucket, oi.get((sym, bucket), float("nan"))))
            oi_now = oi_series[-1][1]
            oi_24h_ago = next(
                (value for ts, value in reversed(oi_series)
                 if (bucket - ts) >= timedelta(hours=20) and not math.isnan(value)),
                float("nan"),
            )
            if math.isnan(oi_now) or math.isnan(oi_24h_ago) or oi_24h_ago <= 0:
                oi_change, oi_rising = 0.0, 0.0
            else:
                oi_change = (oi_now - oi_24h_ago) / oi_24h_ago * 100.0
                oi_rising = 1.0 if oi_change > 0 else 0.0

            rv = realized_vol_daily(all_closes, bars_per_day)
            har = har_forecast(all_closes, bars_per_day)
            hurst = hurst_dfa(all_closes)
            adx14 = adx(all_highs, all_lows, all_closes, 14)
            chop14 = choppiness(all_highs, all_lows, all_closes, 14)
            bbw_pctl = bbw_percentile(all_closes)
            change24 = price_change(all_closes, bars_per_day)
            change7d = price_change(all_closes, bars_per_day * 7)
            fear_greed = fng.get(bucket, 50.0)
            event_flag = 1.0 if events.get(bucket, 0) > 0 else 0.0

            features.append([
                funding_avg, funding_extreme, oi_change, oi_rising,
                rv, har, hurst, adx14, chop14, bbw_pctl,
                change24, change7d, fear_greed, event_flag,
            ])

            # Ground-truth label inputs.
            ema20 = ema(all_closes, 20)
            ref = ema20[-min(len(ema20), 11)]
            ema_slope = (ema20[-1] - ref) / max(ref, 1e-12) * 100.0
            drawdown = max_drawdown(all_closes[-bars_per_day * 7:])
            labels.append(label_regime(change24, adx14, ema_slope, drawdown))
            # Naive UTC so numpy datetime64 conversion is always clean.
            timestamps.append(bucket.replace(tzinfo=None))

    if not features:
        raise SystemExit(
            "No feature rows built. Check that klines history covers the "
            f"requested --days window (days={days}) and tables are populated."
        )
    return (
        np.array(features, dtype=np.float64),
        np.array(labels, dtype=np.int64),
        list(FEATURE_NAMES),
        np.array(timestamps, dtype="datetime64[ns]"),
    )


def label_regime(returns_24h: float, adx: float, ema_slope: float, drawdown: float) -> int:
    """Rule-based regime labeling (ground truth for training).

    RANGE:      ADX < 20, |24h return| < 3%, |drawdown| < 5%
    TREND_UP:   24h return > 3%, EMA slope > 0, ADX > 20
    TREND_DOWN: 24h return < -3%, EMA slope < 0, ADX > 20
    CRASH:      24h return < -7%, or drawdown > 10%
    """
    if drawdown > 10 or returns_24h < -7:
        return 3  # CRASH
    elif returns_24h > 3 and ema_slope > 0 and adx > 20:
        return 1  # TREND_UP
    elif returns_24h < -3 and ema_slope < 0 and adx > 20:
        return 2  # TREND_DOWN
    else:
        return 0  # RANGE


# ---------------------------------------------------------------------------
# Training
# ---------------------------------------------------------------------------

def train_walk_forward(features, labels, timestamps,
                       train_days: int = 60, val_days: int = 15, test_days: int = 15):
    """Walk-forward training: train -> validate -> test on sequential windows.

    The split is strictly chronological: the validation window selects the
    early-stopping iteration and the test window is scored once. The final
    model is refit on train+val with the selected iteration count so the
    reported test metrics describe exactly the shipped artifact.
    """
    span_days = (timestamps[-1] - timestamps[0]) / np.timedelta64(1, "D")
    if span_days < train_days + val_days + test_days:
        # Not enough history for the default windows: fall back to the same
        # 70/15/15 proportions so the script still trains on short datasets.
        train_days = max(1, int(span_days * 0.70))
        val_days = max(1, int(span_days * 0.15))
        test_days = max(1, int(span_days * 0.15))
        log.warning(
            "history shorter than requested windows; using proportional split "
            "train=%dd val=%dd test=%dd", train_days, val_days, test_days,
        )

    t_end = timestamps[-1]
    test_start = t_end - np.timedelta64(test_days, "D")
    val_start = test_start - np.timedelta64(val_days, "D")

    train_mask = timestamps < val_start
    val_mask = (timestamps >= val_start) & (timestamps < test_start)
    test_mask = timestamps >= test_start

    x_train, y_train = features[train_mask], labels[train_mask]
    x_val, y_val = features[val_mask], labels[val_mask]
    x_test, y_test = features[test_mask], labels[test_mask]

    for name, arr in (("train", y_train), ("val", y_val), ("test", y_test)):
        if len(arr) == 0:
            raise SystemExit(f"walk-forward {name} window is empty; extend --days")
    if len(np.unique(y_train)) < NUM_CLASSES:
        present = ", ".join(REGIME_CLASSES[i] for i in np.unique(y_train))
        log.warning("training data covers only classes [%s]; CRASH is rare — "
                    "consider extending the lookback", present)

    base = LGBMClassifier(
        objective="multiclass",
        num_class=NUM_CLASSES,
        n_estimators=500,
        learning_rate=0.05,
        num_leaves=31,
        max_depth=-1,
        min_child_samples=30,
        subsample=0.9,
        subsample_freq=1,
        colsample_bytree=0.9,
        class_weight="balanced",
        random_state=42,
        verbose=-1,
    )
    base.fit(
        x_train, y_train,
        eval_set=[(x_val, y_val)],
        eval_metric="multi_logloss",
        callbacks=[lgb.early_stopping(50, verbose=False)],
    )
    best_iter = max(base.best_iteration_ or 0, 50)

    final = LGBMClassifier(**{**base.get_params(), "n_estimators": best_iter})
    final.fit(np.vstack([x_train, x_val]), np.concatenate([y_train, y_val]))

    proba = final.predict_proba(x_test)
    preds = final.predict(x_test)
    metrics = {
        "accuracy": float(accuracy_score(y_test, preds)),
        "logloss": float(log_loss(y_test, proba, labels=list(range(NUM_CLASSES)))),
        "best_iteration": int(best_iter),
        "train_rows": int(len(y_train)),
        "val_rows": int(len(y_val)),
        "test_rows": int(len(y_test)),
        "class_distribution": {
            REGIME_CLASSES[i]: int((labels == i).sum()) for i in range(NUM_CLASSES)
        },
    }
    return final, metrics


def export_onnx(model, output_path: str, feature_names=None) -> None:
    """Export the LightGBM model to ONNX (opset 15) for Go inference.

    Feature order and regime classes are attached as model metadata so the Go
    loader can validate the artifact against FeatureNames() at load time.
    """
    if feature_names is None:
        feature_names = FEATURE_NAMES
    onnx_model = convert_lightgbm(
        model,
        name="regime_classifier",
        initial_types=[("features", FloatTensorType([None, len(feature_names)]))],
        target_opset=15,
    )
    meta = onnx_model.metadata_props.add()
    meta.key, meta.value = "feature_names", ",".join(feature_names)
    meta = onnx_model.metadata_props.add()
    meta.key, meta.value = "regime_classes", ",".join(REGIME_CLASSES[i] for i in range(NUM_CLASSES))
    onnx_save_model(onnx_model, output_path)


def main():
    parser = argparse.ArgumentParser(description="Train the regime classifier (LightGBM -> ONNX).")
    parser.add_argument('--days', type=int, default=90, help="lookback window in days")
    parser.add_argument('--output', type=str, default='regime_v1.onnx', help="output ONNX path")
    parser.add_argument('--db-url', type=str, required=True, help="PostgreSQL connection string")
    parser.add_argument('--symbol', type=str, default=None, help="train a single symbol (default: all)")
    parser.add_argument('--train-days', type=int, default=60)
    parser.add_argument('--val-days', type=int, default=15)
    parser.add_argument('--test-days', type=int, default=15)
    parser.add_argument('--bars-per-day', type=int, default=96, help="candles per day (96 = 15m)")
    parser.add_argument('--verbose', action='store_true')
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    conn = psycopg2.connect(args.db_url)
    try:
        require_tables(conn)
        features, labels, names, timestamps = load_features(
            conn, days=args.days, symbol=args.symbol, bars_per_day=args.bars_per_day
        )
        log.info("loaded %d feature rows over %d features", len(features), len(names))
        model, metrics = train_walk_forward(
            features, labels, timestamps,
            train_days=args.train_days, val_days=args.val_days, test_days=args.test_days,
        )
        export_onnx(model, args.output, names)

        print(f"Model saved: {args.output}")
        print(f"Rows: train={metrics['train_rows']} val={metrics['val_rows']} test={metrics['test_rows']}")
        print(f"Class distribution: {metrics['class_distribution']}")
        print(f"Best iteration: {metrics['best_iteration']}")
        print(f"Test accuracy: {metrics['accuracy']:.3f}")
        print(f"Test logloss: {metrics['logloss']:.3f}")

        importance = model.booster_.feature_importance(importance_type='gain')
        ranked = sorted(zip(names, importance), key=lambda kv: kv[1], reverse=True)
        print("Top features by gain:")
        for name, gain in ranked[:10]:
            print(f"  {name:<20} {gain:.1f}")
    finally:
        conn.close()


if __name__ == '__main__':
    main()
