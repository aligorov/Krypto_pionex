# Standalone Pionex Trading Bot (`pionex-bot`)

Production-grade, modular monolith trading bot specifically engineered for **Pionex REST API, Futures WebSocket, Native Futures Grid Bots, and Pattern Futures Trading**.

## Key Features

- **Exclusive Pionex API**: Built strictly according to official Pionex documentation (`https://www.pionex.com/docs/api-docs`).
- **Native Futures Grid Engine**: State-machine lifecycle with strict `buOrderId` confirmation.
- **Pattern Trading Engine**: Isolated technical pattern recognizers (BOS, CHoCH, FVG, OrderBlock) executing via ordinary Futures Orders (`/api/v1/futures/order`) with deterministic `clientOrderId`.
- **Zero-ENV Runtime Config**: All accounts, credentials, risk limits, and kill-switches stored in PostgreSQL.
- **Durable Risk Engine**: Automated pre-flight validation preventing over-exposure, excessive leverage, or trading during kill-switch activation.
- **Event-Driven Backtest & OOS**: Isolated Python quant worker providing Purged K-Fold & Walk-Forward evaluation.
- **Telegram Transactional Outbox**: Reliable notification delivery with RBAC permissions and 2FA command confirmation.

## Quick Start

### 1. Launch Services via Docker Compose
```bash
docker compose up -d --build
```

### 2. Run Automated Release
```bash
./dockerrelease.sh
```

## Documentation

- [AGENTS.md](file:///Users/aleksey/Documents/Krypto_pionex/AGENTS.md): Operating rules and constraints.
- [Initial Schema](file:///Users/aleksey/Documents/Krypto_pionex/migrations/0001_initial.sql): PostgreSQL database schema.
