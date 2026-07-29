import time
import os
import json
import logging

logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

def run_worker():
    logging.info("Starting Pionex Quant Worker (Event-Driven Backtest & Walk-Forward OOS Engine)...")
    db_url = os.getenv("DATABASE_URL", "postgres://pionex:pionex@localhost:5432/pionex_bot")
    logging.info(f"Target Database: {db_url}")

    while True:
        try:
            # Poll backtest jobs queue from PostgreSQL
            time.sleep(5)
        except KeyboardInterrupt:
            logging.info("Stopping Quant Worker cleanly...")
            break

if __name__ == "__main__":
    run_worker()
