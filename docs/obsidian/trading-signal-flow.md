# Trading Signal Flow: pionex-bot

## 1. Pattern Trading Signal Pipeline
```
[ Closed Klines ] 
        │
        ▼
[ Pure Pattern Recognizer ] ─── (Detects BOS, CHoCH, FVG, Engulfing)
        │
        ▼
[ Risk Engine Validation ]  ─── (Checks Kill Switch, Exposure, Leverage)
        │
        ▼
[ OrderIntent Generation ]  ─── (Assigns unique clientOrderId)
        │
        ▼
[ Pionex REST API ]         ─── (POST /uapi/v1/trade/order)
        │
        ▼
[ Execution Reconciliation ]─── (WebSocket wsUA stream + REST verification)
```

## 2. Native Futures Grid Pipeline
```
[ Market Scanner Candidates ]
        │
        ▼
[ Immutable GridStrategySpec ]
        │
        ▼
[ Risk Engine Validation ]
        │
        ▼
[ Submission Intent (Postgres) ]
        │
        ▼
[ Pionex Grid API ]         ─── (POST /api/v1/bot/orders/futuresGrid/create)
        │
        ▼
[ Remote buOrderId Confirmation ] ─── (Transitions state to RUNNING)
```
