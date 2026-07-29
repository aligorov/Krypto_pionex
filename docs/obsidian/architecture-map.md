# Architecture Map: pionex-bot

## 1. Modular Monolith Topology
The `pionex-bot` project is structured as a modular monolith in Go with an isolated Python Quant Worker for backtesting and OOS analysis.

- **Go Backend Service**: `backend/cmd/pionex-bot/main.go`
  - `pionex`: Strict API SDK client (`client.go`, `signer.go`, `clock.go`, `ratelimiter.go`, `trading.go`, `websocket.go`).
  - `grid`: Native Futures Grid state machine (`lifecycle.go`).
  - `patterns`: Pure technical pattern recognizers (`recognizers.go`).
  - `risk`: Durable risk engine & kill switch (`engine.go`).
  - `telegram`: Transactional outbox notification dispatcher (`outbox.go`).
  - `ai`: Pionex Spot AI advisory boundary (`advisor.go`).
- **Python Quant Worker**: `quant-worker/engine/backtest.py`
- **React Frontend**: `frontend/src/App.tsx`
- **Database**: PostgreSQL 16+ (`migrations/0001_initial.sql`).
