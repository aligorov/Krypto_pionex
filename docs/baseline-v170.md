# BASELINE SNAPSHOT v170 (§1 Master Plan)

**Date**: 2026-10-01T07:00Z | **Commit**: d1ba9c2 (v2.0.170) | **Build**: 2026-10-01T07:00:09Z

## Version chain deployed (all tags in both remotes, prod on d1ba9c2)
v2.0.160 (TP 2% harvest) → 161 (liq-guard + reserve 25% + direction unlock) → 162 (symmetric unlock + regime preservation) → 163 (wide grid 8% + step 0.504%) → 164 (precision fix + WS cap + knife REST + storm 5min + trailing directional) → 165 (outcome cohorts + candidate_id) → 166 (ETC-short breach fixes) → 167 (WAIT-class + feed alarm + smoke isolation) → 168 (checkParams wire fix + fail-closed) → 169 (RSI floor + outcome update + REST confirm) → 170 (backtest critical repair)

## Settings snapshot
| Key | Value |
|---|---|
| execution_mode | REAL |
| budget_usdt | 150 |
| max_active_bots | 6 |
| leverage | 4 |
| min_risk_reward | 1.8 |
| pnl_target_mode | DYNAMIC |
| stop_loss_mode | ADAPTIVE_ATR |
| candle_interval | 15M |
| lookback_candles | 192 |
| scan_interval_seconds | 240 |
| min_volume_24h | 250,000 |
| vol range | 2.0–15.0% |
| max_drawdown_pct | 8.0 |
| min_profit_factor | 1.05 |
| fee_bps / slippage_bps | 5 / 2 |
| manage_interval_seconds | 45 |
| margin_reserve_pct | 25 |
| tranche_deploy | true |
| radar_autoclose | STRICT |
| stop_forecast | ACTIVE |
| scan_mode | FULL |
| dgt_redeploy | true |
| smart_exit / ofi_harvest / ou_rotation | true / true / true |
| knife_pause / orderbook_profiler | true / true |
| wick_shield | false |
| fleet_max_net_delta_usdt | 1200 |
| universe_scan_cap | 250 |
| margin_reserve_pct | 25 |

## Active fleet (5 bots, $525 deployed)
| # | Symbol | Dir | Lev | Invest | Range | Grid | PnL Target | Max Loss | Realized | Unreal |
|---|---|---|---|---|---|---|---|---|---|---|
| 1521 | KORUX | NEUTRAL | 4x | $150 | 19.81–22.37 | 25 | $3.00 | $30.00 | +$1.30 | −$4.13 |
| 1524 | XPL | NEUTRAL | 4x | $150 | 0.0934–0.1010 | 9 | $3.00 | $26.40 | $0.00 | −$4.23 |
| 1525 | LTC | NEUTRAL | 4x | $75 | 64.67–70.05 | 6 | $1.50 | $12.87 | $0.00 | $0.00 |
| 1526 | ARB | NEUTRAL | 4x | $75 | 0.1971–0.2135 | 8 | $1.50 | $13.33 | $0.00 | $0.00 |
| 1527 | GRAM | NEUTRAL | 4x | $75 | 1.455–1.577 | 11 | $1.50 | $12.97 | $0.00 | $0.00 |

## Risk engine
| Key | Value |
|---|---|
| kill_switch | false |
| max_account_exposure | $10,000 |
| max_symbol_exposure | $1,000 |
| max_daily_loss | $375 (MANUAL) |
| max_leverage | 10 |
| max_active_grid_bots | 20 |
| max_open_positions | 50 |

## Migrations applied
0001–0060 (0058: liq columns, 0059: candidate_id, 0060: outcome_revision)

## Backtest status
ALL walk-forward results prior to v2.0.170 are INVALID (time-reversed candles, equity-after-stop missing, unbounded levels). New scans from v2.0.170 onward use the repaired engine.
