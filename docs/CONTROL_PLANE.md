# Control plane and MCP

## Access model

| Role | Read state | Change personal settings | Operational commands | Users and global config |
|---|---:|---:|---:|---:|
| VIEWER | yes | yes | no | no |
| OPERATOR | yes | yes | yes | no |
| ADMIN | yes | yes | yes | yes |

Web sessions use an HttpOnly cookie and a separate CSRF token. Five failed password attempts lock the account for 15 minutes. A password reset revokes every active session.

MCP tokens are displayed once and stored only as SHA-256 hashes. Available scopes:

- `mcp:read` — status, settings, accounts, trading records, logs, audit and command history.
- `mcp:write` — personal settings and non-trading mutations permitted by the user's role.
- `mcp:trade` — prepare and confirm operational commands.
- `mcp:admin` — users, global configuration, feature flags and token administration.

The database flag `mcp_write_enabled` can stop all MCP mutations without disabling reads.

## MCP transports

Streamable HTTP:

```text
POST http://localhost:8080/mcp
Authorization: Bearer pxmcp_...
Content-Type: application/json
```

Stdio:

```bash
/app/pionex-mcp --token-file /run/secrets/pionex-mcp-token
```

Stdio protocol data uses stdout; diagnostic messages use stderr.

## MCP tools

Read tools:

- `system_status`
- `users_list`
- `user_settings_get`
- `config_list`
- `feature_flags_list`
- `risk_get`
- `accounts_list`
- `grids_list`
- `orders_list`
- `logs_list`
- `audit_list`
- `commands_list`
- `api_tokens_list`

Mutation tools:

- `users_create`
- `users_update`
- `users_password_reset`
- `user_settings_update`
- `config_set`
- `feature_flag_set`
- `risk_update`
- `command_prepare`
- `command_confirm`
- `api_token_create`
- `api_token_revoke`

## Operational command model

Every command requires a caller-provided `idempotencyKey`. Reusing the key returns the existing command and does not execute the action twice.

Supported command types:

- `kill_switch.set`
- `account.set_enabled`
- `scanner.run`
- `grid.create`
- `grid.stop`
- `grid.reduce`
- `pattern.cancel`
- `position.emergency_close`

`scanner.run` and enabling the kill switch execute without a confirmation step. Disabling the kill switch, account changes and trading operations require a six-digit confirmation code that expires in five minutes when `mcp_dangerous_confirmation_required` is enabled.

## Execution gates

New Grid creation is denied unless all conditions hold:

1. `risk_settings.kill_switch_enabled = false`
2. `app_config.real_grid_execution_enabled = true`
3. `feature_flags.real_native_grid = true`
4. caller role and MCP scope permit the operation
5. two-phase confirmation succeeds

The equivalent pattern-order gates are `real_pattern_execution_enabled` and `real_pattern_execution`.

The control plane records accepted domain commands as `QUEUED`. The domain worker must still perform symbol validation against Pionex, rate limiting, timestamp synchronization, retry/idempotency handling and remote reconciliation. A Grid bot becomes `RUNNING` only after a valid remote `buOrderId` is persisted.

## First administrator

There is deliberately no default username or password:

```bash
docker compose run --rm backend /app/pionex-admin create-user \
  --username admin \
  --display-name "Administrator" \
  --role ADMIN
```

Additional commands:

```bash
docker compose run --rm backend /app/pionex-admin list-users
docker compose run --rm backend /app/pionex-admin reset-password --user-id <uuid>
```
