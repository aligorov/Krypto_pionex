# TELEGRAM NOTIFICATIONS & COMMAND GATEWAY

## Outbox Pattern Flow
```
Domain Event -> PostgreSQL notification_outbox -> Outbox Dispatcher -> Telegram Bot API
```

## Supported Commands
- `/status` — General system health and database status
- `/health` — Circuit breaker and WebSocket connectivity
- `/positions` — Active futures positions
- `/orders` — Open pattern orders
- `/grids` — Active native grid bots
- `/risk` — Risk engine status and exposure metrics
- `/killswitch_on` — Emergency activation of Kill Switch (Requires ADMIN role)
- `/killswitch_off` — Deactivation of Kill Switch (Requires ADMIN role & confirmation)
