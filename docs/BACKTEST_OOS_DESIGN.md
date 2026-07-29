# BACKTEST & WALK-FORWARD OOS DESIGN

The quant engine runs as an isolated Python worker process.

## Key Principles
1. **Zero Look-Ahead Bias**: Signals are evaluated strictly on CLOSED candles.
2. **Purged Walk-Forward Evaluation**: Train, Validation, and Test folds are separated by an embargo buffer to prevent data leakage.
3. **Execution Modeling**: Realistic accounting for maker/taker fees, slippage, and funding rates.
4. **Calculated Metrics**: Expected Value (EV), Sharpe Ratio, Sortino Ratio, Maximum Drawdown, Profit Factor, Win Rate, Turnover.
