# PATTERN TRADING EXECUTION SPECIFICATION

Pattern trading uses ordinary Pionex Futures Orders (`/api/v1/futures/order`) and NEVER native grid bot APIs.

## Supported Pattern Recognizers
1. **BOS (Break of Structure)**
2. **CHoCH (Change of Character)**
3. **Liquidity Sweep**
4. **FVG (Fair Value Gap)**
5. **Order Block**
6. **Engulfing**
7. **Pin Bar**

## Execution Pipeline
```
Closed Candle Data -> Pure Pattern Recognizer -> Quality Score Gate -> Risk Engine Gate -> OrderIntent -> Pionex Futures Order -> Reconciliation
```

## Idempotency
Each order uses a deterministic `clientOrderId` formatted as `PAT_<PAT_TYPE>_<TIMESTAMP_MS>_<RANDOM_HEX>`.
