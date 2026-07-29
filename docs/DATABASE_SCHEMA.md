# DATABASE SCHEMA SPECIFICATION

Database: PostgreSQL 16+

## Tables Overview
1. `pionex_accounts`: API Keys (encrypted), permissions, paper/real mode.
2. `risk_settings`: Single-row table (`id=1`) for Kill Switch, exposure limits, leverage caps.
3. `market_symbols`: Active Pionex PERP symbols and precision metadata.
4. `grid_bots`: State machine for native grid bots with `bu_order_id`.
5. `pattern_orders`: Ordinary futures orders for technical patterns.
6. `notification_outbox`: Transactional outbox for Telegram notifications.
7. `audit_events`: Immutable audit logging.
