#!/bin/bash
set -e

echo "=================================================="
echo "      UPDATING PIONEX AUTOGRID BOT SYSTEM         "
echo "=================================================="

# Force fetch all tags and branches from origin
git fetch -f --tags origin

# Detect latest version tag
LATEST_TAG=$(git tag -l "v1.*" "1.0.0.*" | sort -V | tail -n 1)
if [ -z "$LATEST_TAG" ]; then
    LATEST_TAG="main"
fi

echo ">> Switching to latest release: $LATEST_TAG"
git checkout -f "$LATEST_TAG"

# One-time migration to passwordless network-trust auth (Zero-ENV / zero
# secrets): volumes initialized before v1.1.2 carry a password-based host
# auth line; switch it to trust while the old container is still running.
# The compose network is the boundary — the postgres container never
# publishes a port. Idempotent: fresh and converted volumes are skipped.
if docker exec pionex_postgres sh -c "grep -qE '^host all all all (scram-sha-256|md5|password)$' /var/lib/postgresql/data/pg_hba.conf" 2>/dev/null; then
    echo ">> Switching PostgreSQL host auth to trust (network boundary)..."
    docker exec pionex_postgres sh -c "sed -i -E 's/^host all all all (scram-sha-256|md5|password)/host all all all trust/' /var/lib/postgresql/data/pg_hba.conf"
    docker restart pionex_postgres >/dev/null 2>&1 || true
    sleep 5
fi

echo ">> Stopping existing containers..."
docker compose down

echo ">> Building backend and web UI..."
RELEASE_VERSION=$(tr -d '[:space:]' < VERSION 2>/dev/null || echo dev)
docker compose build --build-arg VERSION="$RELEASE_VERSION" backend

echo ">> Starting all services in background..."
docker compose up -d

echo "=================================================="
echo ">> Checking container status..."
docker compose ps
echo "=================================================="
echo ">> Recent bot logs:"
sleep 2
docker logs --tail 25 pionex_backend
echo "=================================================="
echo ">> DONE! Open https://pcrypto.ligam.org and refresh (F5)"
