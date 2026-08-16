import time
import os
import json
import logging

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

def run_worker():
    logging.info("Starting Pionex Quant Worker (Event-Driven Backtest & Walk-Forward OOS Engine)...")
    db_url = os.getenv("DATABASE_URL")
    # Never log credentials: keep only the host part of the URL.
    safe_target = "unset"
    if db_url and "@" in db_url:
        safe_target = db_url.split("@", 1)[-1]
    logging.info(f"Target Database: {safe_target}")

    while True:
        try:
            # Poll backtest jobs queue from PostgreSQL
            time.sleep(5)
        except KeyboardInterrupt:
            logging.info("Stopping Quant Worker cleanly...")
            break

if __name__ == "__main__":
    run_worker()
