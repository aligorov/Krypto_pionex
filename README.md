# Standalone Pionex Trading Bot

Pionex-only trading system with a PostgreSQL-backed control plane, authenticated React interface and MCP management server.

## What is implemented

- DB-backed users, bcrypt passwords, VIEWER/OPERATOR/ADMIN roles, session revocation and lockout.
- HttpOnly session cookies, SameSite=Strict and CSRF validation for every web mutation.
- User settings, runtime configuration and feature flags stored in PostgreSQL.
- Durable risk limits and kill switch.
- Structured application logs with secret redaction and an audit trail for web and MCP actions.
- Streamable HTTP MCP endpoint and local stdio MCP server with scoped hashed tokens.
- Idempotent control commands and two-phase confirmation of dangerous operations.
- React screens for dashboard, users, settings, risk, accounts, grids, orders, logs, audit, MCP tokens and command history.

Real Grid and ordinary Futures execution are disabled by default. A queued control command is not proof of execution on Pionex. Remote identifiers, partial fills, terminal states and flat-position verification remain authoritative.

## Start

```bash
docker compose up -d --build
```

Open [http://localhost:8080](http://localhost:8080).

On the first start, create the administrator locally. The password is read without echo and is never passed through an environment variable:

```bash
docker compose run --rm backend /app/pionex-admin create-user \
  --username admin \
  --display-name "Administrator" \
  --role ADMIN
```

Useful checks:

```bash
curl http://localhost:8080/health
curl http://localhost:8080/ready
docker compose ps
```

## MCP

1. Sign in as an administrator.
2. Open the **MCP** screen.
3. Create a token with only the required scopes.
4. Configure the client to use `http://localhost:8080/mcp` and the HTTP header `Authorization: Bearer <token>`.

For a local stdio client, save the token to a protected file and run:

```bash
docker compose run --rm \
  -v /absolute/path/to/token:/run/secrets/pionex-mcp-token:ro \
  backend /app/pionex-mcp --token-file /run/secrets/pionex-mcp-token
```

See [docs/CONTROL_PLANE.md](docs/CONTROL_PLANE.md) for roles, scopes, tools and safety gates.

## Runtime policy

`DATABASE_URL` is the only infrastructure environment variable. Users, sessions, settings, risk policy, feature gates, MCP tokens and control state live in PostgreSQL. No default web password exists.

## Verification

```bash
docker run --rm -v "$PWD/backend:/src" -w /src golang:1.25-alpine go test ./...
npm --prefix frontend run build
npm --prefix frontend audit --audit-level=high
docker compose build backend
```
