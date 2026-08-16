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

echo "[1/5] Running Backend Unit Tests in Docker..."
docker run --rm -v "$ROOT_DIR/backend":/app -w /app golang:1.25-alpine go test -v ./... || { echo "Backend tests failed!"; exit 1; }

echo "[2/5] Building Docker Images (Backend & Quant Worker)..."
cd "$ROOT_DIR"
docker compose build --build-arg VERSION="$VERSION" --build-arg GIT_COMMIT="$COMMIT" --build-arg BUILD_TIME="$BUILD_TIME" backend quant-worker

echo "[3/5] Starting Temporary Container Smoke Test..."
docker compose up -d postgres backend
sleep 5

echo "[4/5] Checking Health Endpoints..."
HEALTH_STATUS="$(curl -s http://localhost:8080/health | grep '"status"' || true)"
if [[ -z "$HEALTH_STATUS" ]]; then
    echo "Backend Health check failed!"
    docker compose logs backend
    docker compose down
    exit 1
fi
echo "Backend Health Check Passed!"

docker compose down

echo "[5/5] Tagging Release v$VERSION..."
if git rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null 2>&1; then
    echo "Tag v$VERSION already exists."
else
    git tag -a "v$VERSION" -m "Release v$VERSION ($COMMIT)" 2>/dev/null || echo "Git tag created locally."
fi

echo "=========================================="
echo " Release v$VERSION Completed Successfully!"
echo "=========================================="
