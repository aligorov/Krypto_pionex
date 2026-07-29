# AGENTS.md: Rules & Operating Principles for Standalone Pionex Bot

This repository (/Users/aleksey/Documents/Krypto_pionex) is a standalone, production-grade trading bot designed **exclusively for Pionex**.

## Strict Operating Rules

1. **Pionex-Only Sources**:
   - Every API call must adhere strictly to official Pionex documentation: `https://www.pionex.com/docs/api-docs`.
   - Never introduce fallback logic to Binance, Bybit, or CCXT.
   - Never construct invalid symbol strings. Verify all symbols against `/api/v1/market/symbols`.

2. **Zero-ENV Runtime Policy**:
   - All runtime configurations, API credentials, account states, strategy parameters, risk limits, and kill switches MUST be stored in PostgreSQL.
   - ENV variables are strictly reserved for low-level infrastructure connections (e.g. `DATABASE_URL`).

3. **Spot vs. Futures Boundary**:
   - Spot USDT balance MUST NOT be counted towards Futures margin.
   - Pionex Spot Grid AI strategy parameters MUST NOT be applied to Futures Grid bots.

4. **Native Futures Grid Lifecycle Integrity**:
   - Native Futures Grid Bots are created strictly via `/api/v1/bot/futuresGrid/create`.
   - A grid bot is never declared `RUNNING` until the remote `buOrderId` is received, validated, and persisted.

5. **Durable Risk Engine**:
   - Pre-flight checks inspect the database `risk_settings` table.
   - If `kill_switch_enabled` is true, all new entry orders and bot creations are rejected immediately.
