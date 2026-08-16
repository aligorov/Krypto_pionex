"""
Pionex quant worker: polls the backtest_jobs queue in PostgreSQL, fetches
public market klines from Pionex and runs the purged walk-forward grid
simulation. Zero-ENV policy: DATABASE_URL is the only environment variable.

Job params (JSONB):
  {
    "interval": "60M",           # optional override of the column
    "train_bars": 240,           # optional overrides
    "test_bars": 60,
    "purge_bars": 6,
    "stop_loss_pct": 8.0,
    "investment": 100.0,
    "limits": 500
  }
Result (JSONB): walk-forward report (folds, oos_return_pct, oos_max_drawdown,
round_trips, stop_hits) plus a per-fold detail sample.
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
FETCH_PAUSE = 0.5  # stay far below the shared 10 req/s public budget

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s")


def database_url():
    url = os.getenv("DATABASE_URL")
    if not url:
        raise SystemExit("DATABASE_URL is required")
    return url


def fetch_klines(symbol, interval, limit):
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
    candles = []
    for row in data["klines"]:
        # Pionex returns kline OBJECTS with string-typed numbers.
        candles.append({
            "open": float(row["open"]),
            "high": float(row["high"]),
            "low": float(row["low"]),
            "close": float(row["close"]),
            "volume": float(row["volume"]),
        })
    return candles


def run_job(conn, job):
    from engine.backtest import QuantBacktestEngine, walk_forward

    params = job["params"] or {}
    symbol = job["symbol"]
    interval = params.get("interval", job["interval"] or "60M")
    limits = int(params.get("limits", 500))
    candles = fetch_klines(symbol, interval, limits)

    engine = QuantBacktestEngine(
        maker_fee=0.0002,
        taker_fee=0.0005,
        slippage=0.0002,
    )
    report = walk_forward(
        engine,
        candles,
        train_bars=int(params.get("train_bars", 240)),
        test_bars=int(params.get("test_bars", 60)),
        purge_bars=int(params.get("purge_bars", 6)),
        investment=float(params.get("investment", 100.0)),
        stop_loss_pct=float(params.get("stop_loss_pct", 8.0)),
    )
    report["symbol"] = symbol
    report["interval"] = interval
    report["candles"] = len(candles)
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
    logging.info("Starting Pionex Quant Worker (walk-forward grid backtest engine)")
    db_url = database_url()
    # Never log credentials: keep only the host part of the URL.
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
            except Exception as exc:  # noqa: BLE001 - job isolation boundary
                logging.error("Backtest job %s failed: %s", job["id"], exc)
                finish_job(conn, job["id"], error=str(exc))
            time.sleep(FETCH_PAUSE)
        except KeyboardInterrupt:
            logging.info("Stopping Quant Worker cleanly...")
            conn.close()
            break
        except Exception as exc:  # noqa: BLE001 - poll loop isolation boundary
            logging.error("Worker poll cycle failed: %s", exc)
            try:
                conn.rollback()
            except Exception:  # noqa: BLE001
                conn = psycopg2.connect(db_url)
            time.sleep(POLL_SECONDS)


if __name__ == "__main__":
    run_worker()
