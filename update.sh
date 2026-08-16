#!/bin/bash
set -e

echo "=================================================="
echo "      UPDATING PIONEX AUTOGRID BOT SYSTEM         "
echo "=================================================="

# Fail closed: compose requires POSTGRES_PASSWORD from the local .env
if [ ! -f .env ]; then
    echo ">> ERROR: .env is missing. Copy .env.example to .env and set POSTGRES_PASSWORD."
    exit 1
fi

# Force fetch all tags and branches from origin
git fetch -f --tags origin

# Detect latest version tag
LATEST_TAG=$(git tag -l "v1.*" "1.0.0.*" | sort -V | tail -n 1)
if [ -z "$LATEST_TAG" ]; then
    LATEST_TAG="main"
fi

echo ">> Switching to latest release: $LATEST_TAG"
git checkout -f "$LATEST_TAG"

echo ">> Stopping existing containers..."
docker compose down

echo ">> Building backend and web UI..."
docker compose build backend

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
