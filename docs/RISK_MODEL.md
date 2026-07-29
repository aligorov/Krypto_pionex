# RISK MODEL & DURABLE KILL SWITCH

All risk parameters live in PostgreSQL table `risk_settings`.

## Pre-Flight Risk Check Matrix
1. **Kill Switch Gate**: If `kill_switch_enabled == true`, ALL new orders and grid bots are immediately rejected.
2. **Account Exposure Gate**: Total open position value must not exceed `max_account_exposure_usd`.
3. **Symbol Exposure Gate**: Total position value on a single symbol must not exceed `max_symbol_exposure_usd`.
4. **Leverage Gate**: Requested leverage must not exceed `max_leverage`.
5. **Daily Drawdown Gate**: Cumulative daily loss must not exceed `max_daily_loss_usd`.
