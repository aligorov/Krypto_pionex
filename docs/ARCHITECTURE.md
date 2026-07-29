# ARCHITECTURE SPECIFICATION: pionex-bot

## 1. System Overview
`pionex-bot` is structured as a **Modular Monolith** in Go, accompanied by an isolated **Python Quant Worker** and a **React Dashboard**.

```
                         +-------------------------+
                         |  React + TS Dashboard   |
                         +------------+------------+
                                      |
                                      v
+-------------------------------------+-------------------------------------+
|                              Go BackendMonolith                           |
|  +--------------------+  +--------------------+  +---------------------+  |
|  | Pionex Client SDK  |  |  Grid Engine (Go)  |  | Pattern Engine (Go) |  |
|  +---------+----------+  +---------+----------+  +----------+----------+  |
|            |                       |                        |             |
|            +-----------------------+------------------------+             |
|                                    |                                      |
|                        +-----------v-----------+                          |
|                        |  Durable Risk Engine  |                          |
|                        +-----------+-----------+                          |
|                                    |                                      |
+------------------------------------+--------------------------------------+
                                     |
                                     v
                        +------------+------------+
                        |   PostgreSQL Database   |
                        +------------+------------+
                                     ^
                                     |
                         +-----------+-----------+
                         | Python Quant Worker   |
                         +-----------------------+
```

## 2. Guiding Principles
- **Pionex-Only**: Zero external exchange fallbacks.
- **Zero-ENV Runtime Config**: All strategy, risk, API keys, and kill-switch states live in PostgreSQL.
- **Transactional Outbox**: Notifications to Telegram are queued in Postgres outbox tables.
