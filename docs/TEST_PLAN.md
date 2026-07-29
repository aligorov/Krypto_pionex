# TEST PLAN & QUALITY ASSURANCE

## Test Layers
1. **Go Backend Unit Tests**: Signing, rate limiting, pattern recognizers, risk validation (`go test ./...`).
2. **Python Quant Worker Unit Tests**: Backtest engine metrics, EV, Sharpe, Sortino calculation (`python3 -m unittest`).
3. **Database Migration Integration**: Clean PostgreSQL container execution.
4. **Smoke Tests**: `./dockerrelease.sh` automated container health check.
