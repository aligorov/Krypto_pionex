# SECURITY POLICY & CREDENTIAL PROTECTION

## Rules
1. **Zero ENV Credentials**: All Pionex API keys and Telegram tokens are stored encrypted at rest in PostgreSQL.
2. **Redacted Logs**: Credentials, signatures, and API keys are strictly redacted from structured logs.
3. **Least Privilege**: Pionex API keys should have IP whitelisting enabled and only the specific permissions needed (READ, FUTURES_TRADE, BOT_TRADE).
