# PIONEX CAPABILITY MATRIX

Official documentation source: https://www.pionex.com/docs/api-docs

## 1. REST Endpoints

| Capability ID | Path | Method | Permission | Rate Limit Weight | Description |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `MARKET_SYMBOLS` | `/api/v1/market/symbols` | `GET` | Public | 1 | Fetch all trading symbols and precisions |
| `MARKET_KLINES` | `/api/v1/market/klines` | `GET` | Public | 1 | Historical OHLCV candle data |
| `FUTURES_BALANCE` | `/api/v1/futures/balance` | `GET` | READ | 1 | Futures account wallet free and used balances |
| `FUTURES_POSITION`| `/api/v1/futures/position` | `GET` | READ | 1 | Active futures positions and liquidation prices |
| `FUTURES_ORDER` | `/api/v1/futures/order` | `POST` | TRADE | 1 | Place ordinary futures order for pattern trading |
| `GRID_CREATE` | `/api/v1/bot/futuresGrid/create` | `POST` | BOT_TRADE | 5 | Create native Pionex Futures Grid bot |
| `GRID_CANCEL` | `/api/v1/bot/futuresGrid/cancel` | `POST` | BOT_TRADE | 5 | Cancel native grid bot and close position |
| `GRID_ORDERS` | `/api/v1/bot/futuresGrid/orders` | `GET` | BOT_READ | 1 | Get active and historical grid bots |

## 2. WebSocket Topics (`wss://ws.pionex.com/ws/futures`)
- `ORDER`: Private order state updates.
- `FILL`: Real-time fill executions.
- `POSITION`: Margin and position changes.
- `BALANCE`: Wallet balance updates.
