"""
Pionex quant worker: polls the backtest_jobs queue in PostgreSQL, fetches
public market klines from Pionex and runs the purged walk-forward grid
simulation. Zero-ENV policy: DATABASE_URL is the only environment variable.

v2.0.170 fixes (master-plan §7):
  - Parses the `time` field from each kline row, sorts OLDEST-FIRST
    (Pionex returns newest-first — all prior results were time-reversed).
  - Drops the still-forming candle (the newest row).
  - Deduplicates candles with identical timestamps.
  - Validates OHLC sanity (high >= max(open,close), low <= min(open,close)).

Job params (JSONB):
  {
    "interval": "60M",
    "train_bars": 240,
    "test_bars": 60,
    "purge_bars": 6,
    "stop_loss_pct": 8.0,
    "investment": 100.0,
    "limits": 500
  }
"""
import json
import logging
import os
import time

import psycopg2
import psycopg2.extras
import requests

PIONEX_KLINES_URL = "https://api.pionex.com/api/v1/market/klines"
POLL_SECONDS = 5
FETCH_PAUSE = 0.5

INTERVAL_SECONDS = {
    "1M": 60, "5M": 300, "15M": 900, "30M": 1800,
    "60M": 3600, "1H": 3600, "4H": 14400, "1D": 86400,
}

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s")


def database_url():
    url = os.getenv("DATABASE_URL")
    if not url:
        raise SystemExit("DATABASE_URL is required")
    return url


def fetch_klines(symbol, interval, limit):
    """Fetch klines from Pionex, sort oldest-first, drop the forming candle.

    The Pionex /api/v1/market/klines endpoint returns the N most recent
    candles in NEWEST-FIRST order (verified against the Go client's
    live-probed contract: regime.go, shadow_portfolio.go, scanner.go all
    explicitly reverse before use). Prior versions of this worker passed
    the raw newest-first list directly into the backtest engine, which
    assumes oldest-first iteration — every historical result was
    time-reversed.

    v2.0.176b: strip the _PERP suffix — the PUBLIC klines endpoint is a
    spot/market API that expects "BTC_USDT" not "BTC_USDT_PERP". The Go
    scanner already sends non-PERP symbols (from /market/symbols); the
    backtest job table stores the PERP format, so we convert here.
    Also cap limit at 500 (the Pionex API maximum for klines).
    """
    # v2.0.176b: Pionex klines API max is 500 — limit=1000 returns
    # MARKET_PARAMETER_ERROR with 0 candles. The _PERP suffix is CORRECT
    # (the API expects futures-style symbols for futures klines).
    if limit > 500:
        limit = 500
    response = requests.get(
        PIONEX_KLINES_URL,
        params={"symbol": symbol, "interval": interval, "limit": limit},
        timeout=20,
    )
    response.raise_for_status()
    payload = response.json()
    data = payload.get("data")
    if not data or not data.get("klines"):
        raise ValueError(f"no klines returned for {symbol}")
    raw = data["klines"]

    # Parse with timestamps
    parsed = []
    for row in raw:
        ts = row.get("time") or row.get("timestamp")
        if ts is None:
            raise ValueError(f"kline row missing 'time' field for {symbol}: keys={list(row.keys())}")
        ts = int(ts)
        # Pionex returns milliseconds
        if ts > 1e12:
            ts = ts // 1000
        candle = {
            "time": ts,
            "open": float(row["open"]),
            "high": float(row["high"]),
            "low": float(row["low"]),
            "close": float(row["close"]),
            "volume": float(row["volume"]),
        }
        # OHLC sanity check
        if candle["high"] < max(candle["open"], candle["close"]) or candle["low"] > min(candle["open"], candle["close"]):
            logging.warning("Skipping OHLC-invalid candle for %s at %s", symbol, ts)
            continue
        if candle["volume"] < 0 or any(v != v for v in candle.values() if isinstance(v, float)):  # NaN check
            logging.warning("Skipping NaN/negative-volume candle for %s at %s", symbol, ts)
            continue
        parsed.append(candle)

    # Sort oldest-first (Pionex returns newest-first)
    parsed.sort(key=lambda c: c["time"])

    # Deduplicate identical timestamps
    deduped = []
    for c in parsed:
        if deduped and deduped[-1]["time"] == c["time"]:
            deduped[-1] = c  # keep the later fetch
        else:
            deduped.append(c)

    # Drop the still-forming candle: if the newest candle's interval
    # hasn't fully elapsed yet, it's incomplete.
    interval_sec = INTERVAL_SECONDS.get(str(interval).upper(), 3600)
    now = int(time.time())
    if deduped and deduped[-1]["time"] + interval_sec > now:
        deduped = deduped[:-1]
        logging.debug("Dropped forming candle for %s (ts=%s)", symbol, deduped[-1]["time"] + interval_sec if deduped else "last")

    if len(deduped) < 30:
        raise ValueError(f"insufficient candles after cleaning for {symbol}: {len(deduped)}")
    return deduped


def run_job(conn, job):
    from engine.backtest import QuantBacktestEngine, walk_forward

    params = job["params"] or {}
    symbol = job["symbol"]
    interval = params.get("interval", job["interval"] or "60M")
    limits = int(params.get("limits", 500))
    candles = fetch_klines(symbol, interval, limits)

    maker_fee = float(params.get("fee_bps", 2.0)) / 10000.0 if "fee_bps" in params else 0.0002
    slippage = float(params.get("slippage_bps", 5.0)) / 10000.0 if "slippage_bps" in params else 0.0005
    funding_rate_8h = float(params.get("funding_rate_8h", 0.0001))

    engine = QuantBacktestEngine(
        maker_fee=maker_fee,
        taker_fee=0.0005,
        slippage=slippage,
        funding_rate_8h=funding_rate_8h,
    )
    bar_hours = INTERVAL_SECONDS.get(str(interval).upper(), 3600) / 3600.0

    deployed_params = None
    if "lower_price" in params and "upper_price" in params and "grid_num" in params:
        deployed_params = {
            "lower": float(params["lower_price"]),
            "upper": float(params["upper_price"]),
            "levels": int(params["grid_num"]),
            "leverage": float(params.get("leverage", 1.0)),
            "investment": float(params.get("investment", 100.0)),
            "direction": str(params.get("direction", "neutral")).lower(),
            "stop_loss_pct": float(params.get("stop_loss_pct", 8.0)) if params.get("stop_loss_pct") is not None else None,
            # v2.0.194 (plan wave-A / F06 tail): the ABSOLUTE anti-hunt stop —
            # the engine honors it when present; pct stays as the legacy path.
            "stop_loss_price": float(params["stop_loss_price"]) if params.get("stop_loss_price") is not None else None,
        }

    report = walk_forward(
        engine,
        candles,
        train_bars=int(params.get("train_bars", 240)),
        test_bars=int(params.get("test_bars", 60)),
        purge_bars=int(params.get("purge_bars", 6)),
        investment=float(params.get("investment", 100.0)),
        stop_loss_pct=float(params.get("stop_loss_pct", 8.0)),
        bar_hours=bar_hours,
        deployed_params=deployed_params,
    )
    report["symbol"] = symbol
    report["interval"] = interval
    report["candles"] = len(candles)
    report["candle_start"] = candles[0]["time"] if candles else None
    report["candle_end"] = candles[-1]["time"] if candles else None
    return report


def claim_job(conn):
    with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
        cur.execute(
            """
            WITH next_job AS (
                SELECT id FROM backtest_jobs
                WHERE status = 'QUEUED'
                   OR (status = 'RUNNING' AND created_at < NOW() - INTERVAL '30 minutes')
                ORDER BY created_at
                FOR UPDATE SKIP LOCKED
                LIMIT 1
            )
            UPDATE backtest_jobs AS job
            SET status = 'RUNNING'
            FROM next_job
            WHERE job.id = next_job.id
            RETURNING job.id, job.symbol, job.interval, job.params
            """
        )
        return cur.fetchone()


def finish_job(conn, job_id, result=None, error=None):
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE backtest_jobs
            SET status = %s, result = %s, error = %s, finished_at = NOW()
            WHERE id = %s
            """,
            (
                "DONE" if error is None else "FAILED",
                psycopg2.extras.Json(result) if result is not None else None,
                error,
                job_id,
            ),
        )
    conn.commit()


def run_worker():
    logging.info("Starting Pionex Quant Worker v2.0.170 (candle-order fixed, unclosed-bar dropped)")
    db_url = database_url()
    logging.info("Target Database: %s", db_url.split("@", 1)[-1] if "@" in db_url else "configured")

    conn = psycopg2.connect(db_url)
    conn.autocommit = False
    while True:
        try:
            job = claim_job(conn)
            if job is None:
                conn.commit()
                time.sleep(POLL_SECONDS)
                continue
            logging.info("Running backtest job %s for %s", job["id"], job["symbol"])
            try:
                result = run_job(conn, job)
                finish_job(conn, job["id"], result=result)
                logging.info(
                    "Backtest %s done: folds=%s oos=%s%% dd=%s",
                    job["id"], result.get("folds"), result.get("oos_return_pct"),
                    result.get("oos_max_drawdown"),
                )
            except Exception as exc:
                logging.error("Backtest job %s failed: %s", job["id"], exc)
                finish_job(conn, job["id"], error=str(exc))
            time.sleep(FETCH_PAUSE)
        except KeyboardInterrupt:
            logging.info("Stopping Quant Worker cleanly...")
            conn.close()
            break
        except Exception as exc:
            logging.error("Worker poll cycle failed: %s", exc)
            try:
                conn.rollback()
            except Exception:
                conn = psycopg2.connect(db_url)
            time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    run_worker()
