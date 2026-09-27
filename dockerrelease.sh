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

echo "[1/5] Running Backend Test Suite in Docker (unit + DB integration)..."

# v2.0.119 release gate: the integration suite (radar, heals, reconcile —
# the money-path tests) silently SKIPPED without a database, and releases
# shipped red (v2.0.113-118 all tagged with failing TestRadar*/TestHealV113).
# A disposable PostgreSQL now backs the suite; POSTGRES_USER is NOT postgres
# because migration 0016 (security hardening) revokes its LOGIN mid-stream
# and would brick the run from migration 0017 onward. Migrations are piped
# in a single session for the same reason. Any failure blocks the release.
TEST_PG_NAME="pionex-release-test-pg"
TEST_PG_PORT="55499"
# The go-test container reaches the host-published port via
# host.docker.internal (mapped to the host gateway below); the disposable
# psql calls run inside the postgres container itself.
TEST_DB_URL="postgres://pionex@host.docker.internal:${TEST_PG_PORT}/pionex?sslmode=disable"

docker rm -f "$TEST_PG_NAME" >/dev/null 2>&1 || true
docker run -d --name "$TEST_PG_NAME" \
    -e POSTGRES_USER=pionex \
    -e POSTGRES_HOST_AUTH_METHOD=trust \
    -p "${TEST_PG_PORT}:5432" \
    postgres:16-alpine >/dev/null

RELEASE_TEST_CLEANUP() {
    docker rm -f "$TEST_PG_NAME" >/dev/null 2>&1 || true
}
trap RELEASE_TEST_CLEANUP EXIT

PG_READY=0
for _ in $(seq 1 45); do
    if docker exec "$TEST_PG_NAME" psql -U pionex -tAc "SELECT 1" >/dev/null 2>&1; then
        PG_READY=1
        break
    fi
    sleep 2
done
if [[ "$PG_READY" != "1" ]]; then
    echo "Disposable PostgreSQL never became ready — cannot run integration suite."
    exit 1
fi

if ! cat "$ROOT_DIR"/migrations/*.sql | docker exec -i "$TEST_PG_NAME" psql -U pionex -v ON_ERROR_STOP=1 -q >/dev/null; then
    echo "Migrations failed on the disposable database — cannot run integration suite."
    exit 1
fi

if ! docker run --rm \
    -v "$ROOT_DIR/backend":/app -w /app \
    -e PIONEX_TEST_DATABASE_URL="$TEST_DB_URL" \
    --add-host=host.docker.internal:host-gateway \
    golang:1.25-alpine go test ./...; then
    echo "Backend test suite FAILED (unit or integration) — release blocked."
    exit 1
fi
echo "Backend test suite passed (unit + integration)."

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
