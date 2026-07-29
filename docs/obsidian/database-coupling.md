# Database Coupling & Zero-ENV Policy: pionex-bot

## 1. Zero-ENV Policy
No trading parameters, API keys, risk limits, or kill switch states are read from `.env`. PostgreSQL is the single source of truth.

## 2. Table Dependency Graph
```
[ pionex_accounts ] ──┬──> [ grid_bots ]
                     ├──> [ pattern_orders ]
                     └──> [ account_permission_health ]

[ risk_settings ]   ───> Enforced by [ backend/internal/risk/engine.go ]

[ notification_outbox ] ──> Asynchronous Dispatcher -> Telegram API
```
