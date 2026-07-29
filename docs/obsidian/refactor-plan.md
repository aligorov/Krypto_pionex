# Refactor Plan & Evolution Roadmap: pionex-bot

## 1. Phase 1 (Completed)
- Native Pionex Futures Grid SDK (`/api/v1/bot/orders/futuresGrid/create`).
- Ordinary Futures Order API (`/uapi/v1/trade/order`).
- Private WebSocket stream (`wss://ws.pionex.com/wsUA`).
- Durable Risk Engine with PostgreSQL Kill Switch.
- Transactional Outbox for Telegram Notifications.

## 2. Phase 2 (Enhancements)
- Multi-account secret key rotation.
- Real-time WebSocket orderbook depth ingestion.
- Purged Walk-Forward statistical OOS expansion in Python worker.
