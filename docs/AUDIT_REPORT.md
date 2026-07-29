# AUDIT REPORT: crypto-bot vs pionex-bot

## Executive Summary
This audit evaluated the codebase at `/Users/aleksey/Documents/crypto-bot` to establish the requirements for the standalone Pionex trading bot at `/Users/aleksey/Documents/Krypto_pionex`.

## 1. Key Vulnerabilities in Legacy `crypto-bot`
1. **Multi-Exchange Fallbacks**: Legacy code mixed Binance and Bybit REST/WebSocket adapters as fallbacks when Pionex endpoints returned missing candle data. This caused price discrepancies, invalid tick sizes, and execution rejections.
2. **Ambiguous Grid Bot Lifecycle**: In `crypto-bot`, grid bot status was marked as `RUNNING` before receiving confirmation of the remote `buOrderId` from Pionex API.
3. **Spot vs. Futures Misalignment**: Spot Grid AI parameters were wrongly mapped to Futures Grid bots, risking liquidation due to uncalculated leverage exposure.
4. **OOS Backtest Leakage**: The legacy backtest engine suffered from same-bar optimistic execution and look-ahead bias without proper Walk-Forward purging.

## 2. Decision Matrix
- **REUSE**: Pure indicator math (EMA, RSI, ATR), pattern recognizer logic (BOS, CHoCH, FVG, Engulfing), financial formulas (Sharpe, Sortino, Drawdown, Profit Factor).
- **REWRITE**: Pionex Client SDK (in Go), Grid Bot Engine, Pattern Trading Engine, Risk Engine, Telegram Outbox, Python Quant Worker.
- **REJECT**: All CCXT, Binance, Bybit adapters, Spot AI Futures mapping, and `.env` runtime configurations.
