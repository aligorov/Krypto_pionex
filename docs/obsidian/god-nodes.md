# God Nodes & Core Entry Points: pionex-bot

## Core Entry Points
1. `backend/cmd/pionex-bot/main.go` — Entry point for Go backend service, HTTP endpoints (`/health`, `/api/version`, `/api/risk/settings`).
2. `backend/internal/pionex/client.go` — Primary REST API transport for Pionex.
3. `backend/internal/risk/engine.go` — Pre-flight Risk Engine & Durable Kill Switch.
4. `quant-worker/worker.py` — Entry point for Python event-driven quant worker.
5. `dockerrelease.sh` — Production release orchestration script.
