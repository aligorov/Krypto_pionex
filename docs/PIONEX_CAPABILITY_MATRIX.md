# PIONEX CAPABILITY MATRIX (VERIFIED OFFICIAL CONTRACTS)

Official documentation sources:
- [Futures Basic Info](https://www.pionex.com/docs/api-docs/futures-api/general-info/basic-info)
- [Futures Trade](https://www.pionex.com/docs/api-docs/futures-api/trade)
- [Futures Grid](https://www.pionex.com/docs/api-docs/bot-api/futures-grid)
- [Private Stream](https://www.pionex.com/docs/api-docs/futures-websocket/private-stream)

## 1. Official REST Endpoints

| Capability ID | Path | Method | Permission | Rate Limit Weight | Status | Description |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `MARKET_SYMBOLS` | `/api/v1/market/symbols` | `GET` | Public | 1 | `IMPLEMENTED` | Fetch all trading symbols and precisions |
| `MARKET_KLINES` | `/api/v1/market/klines` | `GET` | Public | 1 | `IMPLEMENTED` | Historical OHLCV candle data |
| `FUTURES_BALANCE` | `/api/v1/futures/balance` | `GET` | READ | 1 | `IMPLEMENTED` | Futures account wallet free and used balances |
| `FUTURES_POSITION`| `/uapi/v1/trade/position` | `GET` | READ | 1 | `IMPLEMENTED` | Active futures positions and liquidation prices |
| `FUTURES_ORDER` | `/uapi/v1/trade/order` | `POST` | TRADE | 1 | `IMPLEMENTED` | Place ordinary futures order for pattern trading |
| `GRID_CREATE` | `/api/v1/bot/orders/futuresGrid/create` | `POST` | BOT_TRADE | 1 | `IMPLEMENTED` | Create native Pionex Futures Grid bot (with price/amount SL/TP) |
| `GRID_CHECK_PARAMS` | `/api/v1/bot/orders/futuresGrid/checkParams` | `POST` | BOT_TRADE | 1 | `IMPLEMENTED` | Validate grid bot parameters & estimates before creation |
| `GRID_ADJUST_PARAMS` | `/api/v1/bot/orders/futuresGrid/adjustParams` | `POST` | BOT_TRADE | 1 | `IMPLEMENTED` | Modify grid range/levels with `keepInvestment=true` or add capital |
| `GRID_ADJUST_CHECK` | `/api/v1/bot/orders/futuresGrid/adjustParamsCheck` | `POST` | BOT_TRADE | 1 | `IMPLEMENTED` | Dry-run check for grid adjust parameters |
| `GRID_UPDATE_TP_SL` | `/api/v1/bot/orders/futuresGrid/updateTriggerProfitLoss` | `POST` | BOT_TRADE | 1 | `DOCUMENTED` | Set, update, or clear price-based TP/SL on running bot in real-time |
| `GRID_CANCEL` | `/api/v1/bot/orders/futuresGrid/cancel` | `POST` | BOT_TRADE | 5 | `IMPLEMENTED` | Cancel native grid bot and close position |
| `GRID_ORDER` | `/api/v1/bot/orders/futuresGrid/order` | `GET` | BOT_READ | 1 | `IMPLEMENTED` | Query single futures grid bot order by ID |
| `GRID_ORDERS` | `/api/v1/bot/orders` | `GET` | BOT_READ | 1 | `IMPLEMENTED` | Get active and historical grid bots |

## 2. Official WebSocket Streams
- **Private Stream Base URL**: `wss://ws.pionex.com/wsUA`
- **Topics**: `ORDER`, `FILL`, `POSITION`, `BALANCE`
