# DEPLOYMENT RUNBOOK

## Production Deployment Steps
1. Navigate to target project directory:
   ```bash
   cd /Users/aleksey/Documents/Krypto_pionex
   ```
2. Execute the release script:
   ```bash
   ./dockerrelease.sh
   ```
3. Confirm running services:
   ```bash
   docker compose ps
   ```
4. Verify backend health endpoint:
   ```bash
   curl http://localhost:8080/health
   ```
