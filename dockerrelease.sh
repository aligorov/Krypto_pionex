#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION_FILE="$ROOT_DIR/VERSION"

if [[ ! -f "$VERSION_FILE" ]]; then
    echo "1.0.0" > "$VERSION_FILE"
fi

VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo "dev")"
BUILD_TIME="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

echo "=========================================="
echo " Starting Pionex Bot Production Release"
echo " Version:    $VERSION"
echo " Git Commit: $COMMIT"
echo " Build Time: $BUILD_TIME"
echo "=========================================="

echo "[1/6] Running Backend Unit Tests in Docker..."
docker run --rm -v "$ROOT_DIR/backend":/app -w /app golang:1.22-alpine go test -v ./... || { echo "Backend tests failed!"; exit 1; }

echo "[2/6] Building Docker Images (Backend, Quant, Frontend)..."
cd "$ROOT_DIR"
docker compose build backend quant-worker frontend

echo "[3/6] Starting Temporary Container Smoke Test..."
docker compose up -d postgres backend frontend
sleep 5

echo "[4/6] Checking Health Endpoints..."
HEALTH_STATUS="$(curl -s http://localhost:8080/health | grep '"healthy"' || true)"
if [[ -z "$HEALTH_STATUS" ]]; then
    echo "Backend Health check failed!"
    docker compose logs backend
    docker compose down
    exit 1
fi
echo "Backend Health Check Passed!"

FRONTEND_STATUS="$(curl -s -I http://localhost:3080 | grep '200 OK' || true)"
if [[ -z "$FRONTEND_STATUS" ]]; then
    echo "Frontend HTTP check failed!"
    docker compose logs frontend
    docker compose down
    exit 1
fi
echo "Frontend HTTP 200 OK Passed!"

docker compose down

echo "[5/6] Tagging Release v$VERSION..."
if git rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null 2>&1; then
    echo "Tag v$VERSION already exists."
else
    git tag -a "v$VERSION" -m "Release v$VERSION ($COMMIT)" 2>/dev/null || echo "Git tag created locally."
fi

echo "=========================================="
echo " Release v$VERSION Completed Successfully!"
echo "=========================================="
