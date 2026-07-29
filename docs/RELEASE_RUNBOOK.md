# RELEASE RUNBOOK

## Automated Release Workflow (`./dockerrelease.sh`)
1. Run backend unit tests.
2. Build production Docker images without cache.
3. Spin up local containers and execute health checks (`http://localhost:8080/health`).
4. Generate annotated Git tag `vX.Y.Z`.
5. Deploy containers to production.
