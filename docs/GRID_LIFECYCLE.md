# NATIVE FUTURES GRID BOT LIFECYCLE

All endpoints below are the official Pionex bot-order routes (AGENTS.md #1). The
pre-`/orders/` spellings this document carried before v2.0.196 do not exist.

## Endpoints

| Purpose | Method & path |
| --- | --- |
| Pre-flight geometry check | `POST /api/v1/bot/orders/futuresGrid/check` |
| Create | `POST /api/v1/bot/orders/futuresGrid/create` |
| Detail | `GET /api/v1/bot/orders/futuresGrid/order?buOrderId=…` |
| Cancel | `POST /api/v1/bot/orders/futuresGrid/cancel` |
| Invest-in / adjust params | `POST /api/v1/bot/orders/futuresGrid/adjust` |
| Stop-loss / take-profit update | `POST /api/v1/bot/orders/futuresGrid/update` |
| Finished-bot list (final PnL source) | `GET /api/v1/bot/orders?buOrderTypes=futures_grid&status=finished` |

## State Machine

```
[ DRAFT ] --> [ PENDING_SUBMISSION ] --> [ RUNNING ]
                     |                       |
                     v                       v
           [ SUBMISSION_UNKNOWN ]     [ STOP_REQUESTED ] --> [ STOPPING ]
                     |                       |                    |
                     v                       v                    v
              [ RECONCILIATION ]        [ STOPPED ]          [ STOPPED ]

terminal, reachable from any live state:
   STOPPED | COMPLETED | LIQUIDATED | EMERGENCY_STOPPED | FAILED | CANCELLED
```

There is no `SUBMITTED` and no `REJECTED` row: the intent is written as
`PENDING_SUBMISSION` and moves straight to `RUNNING`, or to
`SUBMISSION_UNKNOWN` when the create call gave no usable answer.

## State Rules

1. **DRAFT**: parameters validated locally; nothing exists on the exchange.
2. **PENDING_SUBMISSION**: intent row recorded in PostgreSQL `grid_bots`, inside
   the create transaction that re-runs the durable risk caps
   (`risk.RecheckGridEntryCaps`) under an account-scoped advisory lock, after a
   last-line re-read of `risk_settings.kill_switch_enabled`.
3. **RUNNING**: transitions ONLY when a non-empty remote `buOrderId` is
   received, validated and durably stored (AGENTS.md #4).
4. **SUBMISSION_UNKNOWN**: the outcome of the create is unknown. The row stays
   until the reconcile sweep finds the bot on the exchange or proves it never
   landed; it counts against the exposure caps the whole time.
5. **STOP_REQUESTED → STOPPING → STOPPED**: a durable close intent
   (`close_intent`), submitted as `POST …/futuresGrid/cancel` with
   `immediate: true`, then verified against the remote detail.
6. **EMERGENCY_STOPPED**: the kill switch or a liquidation guard closed the bot.
   It is a terminal close and draws from `max_daily_loss_usd` like any other.
7. **COMPLETED / LIQUIDATED / CANCELLED / FAILED**: terminal finals reported by
   the exchange. A final PnL is written only when the settlement ladder below
   settles it — otherwise `realized_pnl_usdt` stays NULL and the UI shows
   «финал неизвестен».

## Settlement ladder (terminal finals)

1. `BUOrderDataResponse.SettledProfit()` — the exchange's own settled total.
2. `gateSettledProfit` — the honesty gate; a refusal yields UNVERIFIABLE, never
   a zero.
3. `unlockIdentityFinal` — the unlock fields minus OUR final `quote_investment`,
   accepted only inside the `|net| < investment` band.
4. `terminalTelemetryEstimate` — the last telemetry total before close, net of
   the close cost and floored at `−maxLoss − closeCost` for stop-class closes.
   The closed-bot table and the epoch headline both read this figure.
5. NULL — unknown. The 48h sweep in `terminal_recheck.go` retries the ladder.
