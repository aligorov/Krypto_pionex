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
#
# v2.0.119 review hardening (agent pass): (a) the go-test container ALSO
# mounts the repo's migrations/ — the ledger_fees migration-consistency test
# resolves ../../migrations and silently skipped without it, the exact
# pathology this gate exists to kill; (b) the disposable postgres lives on a
# PRIVATE docker network with NO published port — a trust-auth DB on
# 0.0.0.0:55499 for minutes was an open window on any public build host;
# (c) an existing tag pointing elsewhere than HEAD fails the release instead
# of silently no-op'ing the deploy; (d) a dirty working tree fails the
# release — update.sh deploys the TAGGED commit, so the suite must test
# exactly that tree.
cd "$ROOT_DIR"
if ! git diff --quiet || ! git diff --cached --quiet ||
    [[ -n "$(git ls-files --others --exclude-standard -- backend frontend quant-worker migrations docker docker-compose.yml VERSION update.sh dockerrelease.sh)" ]]; then
    echo "Working tree is dirty — the suite would test a tree the tag will not carry. Commit first."
    exit 1
fi
if git rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null 2>&1; then
    if [[ "$(git rev-parse "refs/tags/v$VERSION^{commit}")" != "$(git rev-parse HEAD)" ]]; then
        echo "Tag v$VERSION already exists on a DIFFERENT commit — bump VERSION or move the tag on purpose; a silent no-deploy is not an option."
        exit 1
    fi
fi

TEST_PG_NAME="pionex-release-test-pg"
TEST_PG_NET="pionex-release-test-net"
TEST_DB_URL="postgres://pionex@${TEST_PG_NAME}:5432/pionex?sslmode=disable"

docker rm -f "$TEST_PG_NAME" >/dev/null 2>&1 || true
docker network rm "$TEST_PG_NET" >/dev/null 2>&1 || true
docker network create "$TEST_PG_NET" >/dev/null
docker run -d --name "$TEST_PG_NAME" \
    --network "$TEST_PG_NET" \
    -e POSTGRES_USER=pionex \
    -e POSTGRES_HOST_AUTH_METHOD=trust \
    postgres:16-alpine >/dev/null

RELEASE_TEST_CLEANUP() {
    docker rm -f "$TEST_PG_NAME" >/dev/null 2>&1 || true
    docker network rm "$TEST_PG_NET" >/dev/null 2>&1 || true
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
    --network "$TEST_PG_NET" \
    -v "$ROOT_DIR/backend":/app -w /app \
    -v "$ROOT_DIR/migrations":/app/migrations \
    -e PIONEX_TEST_DATABASE_URL="$TEST_DB_URL" \
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
