package risk

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

var (
	ErrKillSwitchActive      = errors.New("risk engine: kill switch is ACTIVE - new entries blocked")
	ErrExposureLimitExceeded = errors.New("risk engine: account exposure limit exceeded")
	ErrLeverageExceeded      = errors.New("risk engine: leverage exceeds maximum allowed limit")
)

// Breaker ownership modes for risk_settings.breaker_mode (migration 0034):
// AUTO lets the autopilot re-derive max_daily_loss_usd from the fleet design
// (N × budget × leverage × stop floor × 1.25); MANUAL is an operator pin the
// derivation must never overwrite.
const (
	BreakerModeAuto   = "AUTO"
	BreakerModeManual = "MANUAL"
)

// RiskSettings represents durable settings loaded from PostgreSQL.
type RiskSettings struct {
	ID                    int             `db:"id" json:"id"`
	KillSwitchEnabled     bool            `db:"kill_switch_enabled" json:"killSwitchEnabled"`
	MaxAccountExposureUSD decimal.Decimal `db:"max_account_exposure_usd" json:"maxAccountExposureUsd"`
	MaxSymbolExposureUSD  decimal.Decimal `db:"max_symbol_exposure_usd" json:"maxSymbolExposureUsd"`
	MaxDailyLossUSD       decimal.Decimal `db:"max_daily_loss_usd" json:"maxDailyLossUsd"`
	BreakerMode           string          `db:"breaker_mode" json:"breakerMode"`
	MaxLeverage           int             `db:"max_leverage" json:"maxLeverage"`
	MaxActiveGridBots     int             `db:"max_active_grid_bots" json:"maxActiveGridBots"`
	MaxOpenPositions      int             `db:"max_open_positions" json:"maxOpenPositions"`
}

// Engine enforces portfolio safety rules.
type Engine struct {
	db *pgxpool.Pool
}

// NewEngine initializes the Risk Engine.
func NewEngine(db *pgxpool.Pool) *Engine {
	return &Engine{db: db}
}

// LoadSettings fetches risk rules directly from PostgreSQL.
func (e *Engine) LoadSettings(ctx context.Context) (*RiskSettings, error) {
	query := `
		SELECT id, kill_switch_enabled, max_account_exposure_usd, max_symbol_exposure_usd,
		       max_daily_loss_usd, breaker_mode, max_leverage, max_active_grid_bots, max_open_positions
		FROM risk_settings WHERE id = 1
	`
	row := e.db.QueryRow(ctx, query)
	var s RiskSettings
	err := row.Scan(
		&s.ID, &s.KillSwitchEnabled, &s.MaxAccountExposureUSD, &s.MaxSymbolExposureUSD,
		&s.MaxDailyLossUSD, &s.BreakerMode, &s.MaxLeverage, &s.MaxActiveGridBots, &s.MaxOpenPositions,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query risk settings: %w", err)
	}
	return &s, nil
}

// ValidateNewOrder performs pre-flight safety checks for a new position or bot.
func (e *Engine) ValidateNewOrder(ctx context.Context, requestedLeverage int, investmentUSD decimal.Decimal) error {
	settings, err := e.LoadSettings(ctx)
	if err != nil {
		return err
	}

	if settings.KillSwitchEnabled {
		return ErrKillSwitchActive
	}

	if requestedLeverage > settings.MaxLeverage {
		return fmt.Errorf("%w: requested %d, max allowed %d", ErrLeverageExceeded, requestedLeverage, settings.MaxLeverage)
	}

	if investmentUSD.GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf("%w: requested %s USD, max allowed %s USD", ErrExposureLimitExceeded, investmentUSD.String(), settings.MaxAccountExposureUSD.String())
	}

	return nil
}

// ValidateNewGrid adds durable portfolio-state checks to the static limits.
// Only non-terminal native grids for the selected Pionex account are counted.
// Exposure is NOTIONAL (quote_investment × leverage, v2.0.27): the fleet
// carries $2600 notional on $1600 invested, and investment-based sums
// understate risk by exactly the leverage factor.
func (e *Engine) ValidateNewGrid(
	ctx context.Context,
	accountID, symbol string,
	requestedLeverage int,
	investmentUSD decimal.Decimal,
) error {
	if err := e.ValidateNewOrder(ctx, requestedLeverage, investmentUSD); err != nil {
		return err
	}
	if err := e.ValidateDailyLoss(ctx); err != nil {
		return err
	}
	settings, err := e.LoadSettings(ctx)
	if err != nil {
		return err
	}
	var accountExposure, symbolExposure decimal.Decimal
	var activeBots int
	err = e.db.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(quote_investment * leverage), 0),
			COALESCE(SUM(quote_investment * leverage) FILTER (WHERE symbol = $2), 0),
			COUNT(*)
		FROM grid_bots
		WHERE account_id = $1
		  AND status IN (
			'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING',
			'STOP_REQUESTED', 'STOPPING'
		  )
	`, accountID, symbol).Scan(&accountExposure, &symbolExposure, &activeBots)
	if err != nil {
		return fmt.Errorf("risk engine: load active grid exposure: %w", err)
	}
	if activeBots >= settings.MaxActiveGridBots {
		return fmt.Errorf(
			"risk engine: active grid limit reached: current %d, max %d",
			activeBots, settings.MaxActiveGridBots,
		)
	}
	nextAccountExposure := accountExposure.Add(investmentUSD.Mul(decimal.NewFromInt(int64(requestedLeverage))))
	if nextAccountExposure.GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf(
			"%w: current %s + requested, max %s",
			ErrExposureLimitExceeded, accountExposure, settings.MaxAccountExposureUSD,
		)
	}
	nextSymbolExposure := symbolExposure.Add(investmentUSD.Mul(decimal.NewFromInt(int64(requestedLeverage))))
	if nextSymbolExposure.GreaterThan(settings.MaxSymbolExposureUSD) {
		return fmt.Errorf(
			"risk engine: symbol exposure limit exceeded: current %s + requested, max %s",
			symbolExposure, settings.MaxSymbolExposureUSD,
		)
	}
	return nil
}

// ValidateNewPaperGrid enforces the same durable gates for PAPER deployments
// (v2.0.27 — paper used to bypass the risk engine entirely: no kill switch,
// no MaxLeverage, no exposure caps; prod CRWVX #366 deployed at 4x
// unchecked). Exposure is notional like ValidateNewGrid; the requested
// notional uses the FULL slot budget because the 24h tranche time-box
// commits the second half anyway.
func (e *Engine) ValidateNewPaperGrid(
	ctx context.Context,
	symbol string,
	requestedLeverage int,
	slotBudgetUSD decimal.Decimal,
) error {
	if err := e.ValidateNewOrder(ctx, requestedLeverage, slotBudgetUSD); err != nil {
		return err
	}
	// v2.0.46: PAPER is exempt from max_daily_loss_usd (operator decision
	// 2026-08-31). The simulation exists to keep collecting gate statistics —
	// a losing paper day used to freeze every entry for the rest of the UTC
	// day exactly when the data mattered most. Runaway loops stay covered by
	// the portfolio circuit breaker (3 protective closes/hour) and the
	// escalating per-symbol cooldown; the daily-loss brake protects REAL
	// capital only.
	settings, err := e.LoadSettings(ctx)
	if err != nil {
		return err
	}
	var accountNotional, symbolNotional decimal.Decimal
	var activeBots int
	err = e.db.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(quote_investment * leverage), 0),
			COALESCE(SUM(quote_investment * leverage) FILTER (WHERE symbol = $1), 0),
			COUNT(*)
		FROM paper_grid_bots
		WHERE status = 'RUNNING'
	`, symbol).Scan(&accountNotional, &symbolNotional, &activeBots)
	if err != nil {
		return fmt.Errorf("risk engine: load paper exposure: %w", err)
	}
	if activeBots >= settings.MaxActiveGridBots {
		return fmt.Errorf(
			"risk engine: paper active grid limit reached: current %d, max %d",
			activeBots, settings.MaxActiveGridBots,
		)
	}
	requestedNotional := slotBudgetUSD.Mul(decimal.NewFromInt(int64(requestedLeverage)))
	if accountNotional.Add(requestedNotional).GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf(
			"%w (paper notional): current %s + requested %s, max %s",
			ErrExposureLimitExceeded, accountNotional, requestedNotional, settings.MaxAccountExposureUSD,
		)
	}
	if symbolNotional.Add(requestedNotional).GreaterThan(settings.MaxSymbolExposureUSD) {
		return fmt.Errorf(
			"risk engine: paper symbol exposure (notional) exceeded: current %s + requested %s, max %s",
			symbolNotional, requestedNotional, settings.MaxSymbolExposureUSD,
		)
	}
	return nil
}

// ValidateGridTopUp validates additional margin on an EXISTING grid (the
// tranche-2 invest_in path). It applies every durable gate ValidateNewGrid
// enforces EXCEPT the active-bot count: a top-up does not create a bot, and
// counting the funded bot against MaxActiveGridBots would reject every
// top-up in a full fleet.
func (e *Engine) ValidateGridTopUp(
	ctx context.Context,
	accountID, symbol string,
	requestedLeverage int,
	additionalUSD decimal.Decimal,
) error {
	if err := e.ValidateNewOrder(ctx, requestedLeverage, additionalUSD); err != nil {
		return err
	}
	if err := e.ValidateDailyLoss(ctx); err != nil {
		return err
	}
	settings, err := e.LoadSettings(ctx)
	if err != nil {
		return err
	}
	var accountExposure, symbolExposure decimal.Decimal
	err = e.db.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(quote_investment * leverage), 0),
			COALESCE(SUM(quote_investment * leverage) FILTER (WHERE symbol = $2), 0)
		FROM grid_bots
		WHERE account_id = $1
		  AND status IN (
			'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING',
			'STOP_REQUESTED', 'STOPPING'
		  )
	`, accountID, symbol).Scan(&accountExposure, &symbolExposure)
	if err != nil {
		return fmt.Errorf("risk engine: load active grid exposure: %w", err)
	}
	requestedNotional := additionalUSD.Mul(decimal.NewFromInt(int64(requestedLeverage)))
	if accountExposure.Add(requestedNotional).GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf(
			"%w: current %s + requested %s, max %s",
			ErrExposureLimitExceeded, accountExposure, requestedNotional,
			settings.MaxAccountExposureUSD,
		)
	}
	if symbolExposure.Add(requestedNotional).GreaterThan(settings.MaxSymbolExposureUSD) {
		return fmt.Errorf(
			"risk engine: symbol exposure limit exceeded: current %s + requested %s, max %s",
			symbolExposure, requestedNotional, settings.MaxSymbolExposureUSD,
		)
	}
	return nil
}

// UpdateSettings persists the complete risk policy atomically.
func (e *Engine) UpdateSettings(ctx context.Context, settings RiskSettings) (*RiskSettings, error) {
	if settings.MaxLeverage < 1 || settings.MaxLeverage > 100 {
		return nil, errors.New("risk engine: max leverage must be between 1 and 100")
	}
	if settings.MaxActiveGridBots < 0 || settings.MaxOpenPositions < 0 {
		return nil, errors.New("risk engine: position limits cannot be negative")
	}
	if settings.MaxAccountExposureUSD.IsNegative() ||
		settings.MaxSymbolExposureUSD.IsNegative() ||
		settings.MaxDailyLossUSD.IsNegative() {
		return nil, errors.New("risk engine: monetary limits cannot be negative")
	}
	if settings.MaxSymbolExposureUSD.GreaterThan(settings.MaxAccountExposureUSD) {
		return nil, errors.New("risk engine: symbol exposure cannot exceed account exposure")
	}
	// Empty means a legacy caller that never heard of breaker ownership —
	// preserve the stored mode so a full-row replace cannot silently unpin
	// an operator's MANUAL value (explicit AUTO/MANUAL still switches it).
	if settings.BreakerMode == "" {
		if err := e.db.QueryRow(ctx,
			`SELECT breaker_mode FROM risk_settings WHERE id = $1`, settings.ID,
		).Scan(&settings.BreakerMode); err != nil || settings.BreakerMode == "" {
			settings.BreakerMode = BreakerModeAuto
		}
	}
	if settings.BreakerMode != BreakerModeAuto && settings.BreakerMode != BreakerModeManual {
		return nil, fmt.Errorf("risk engine: breaker mode must be %s or %s", BreakerModeAuto, BreakerModeManual)
	}

	_, err := e.db.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = $2,
		    max_account_exposure_usd = $3,
		    max_symbol_exposure_usd = $4,
		    max_daily_loss_usd = $5,
		    breaker_mode = $9,
		    max_leverage = $6,
		    max_active_grid_bots = $7,
		    max_open_positions = $8,
		    updated_at = NOW()
		WHERE id = $1
	`, settings.ID, settings.KillSwitchEnabled, settings.MaxAccountExposureUSD,
		settings.MaxSymbolExposureUSD, settings.MaxDailyLossUSD, settings.MaxLeverage,
		settings.MaxActiveGridBots, settings.MaxOpenPositions, settings.BreakerMode)
	if err != nil {
		return nil, fmt.Errorf("update risk settings: %w", err)
	}
	return e.LoadSettings(ctx)
}

// SetKillSwitch updates only the durable kill switch state.
func (e *Engine) SetKillSwitch(ctx context.Context, enabled bool) error {
	tag, err := e.db.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = $1, updated_at = NOW()
		WHERE id = 1
	`, enabled)
	if err != nil {
		return fmt.Errorf("set kill switch: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("risk engine: risk settings row is missing")
	}
	return nil
}

// ValidateDailyLoss checks the last 24h cumulative REALIZED REAL-bot loss
// against max_daily_loss_usd. Since v2.0.46 it counts REAL closes only:
// PAPER results are simulation statistics, not capital — a bad paper day
// must not freeze the experiment (and must not gate REAL capital either).
func (e *Engine) ValidateDailyLoss(ctx context.Context) error {
	settings, err := e.LoadSettings(ctx)
	if err != nil {
		return err
	}
	if settings.MaxDailyLossUSD.IsZero() || settings.MaxDailyLossUSD.IsNegative() {
		return nil
	}
	var dailyRealizedLoss decimal.Decimal
	err = e.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN realized_pnl_usdt < 0 THEN -realized_pnl_usdt ELSE 0 END), 0)
		FROM grid_bots
		WHERE COALESCE(closed_at, updated_at) > NOW() - INTERVAL '24 hours'
		  AND status IN ('STOPPED', 'LIQUIDATED', 'FAILED')
	`).Scan(&dailyRealizedLoss)
	if err != nil {
		return fmt.Errorf("risk engine: check daily loss: %w", err)
	}
	if dailyRealizedLoss.GreaterThanOrEqual(settings.MaxDailyLossUSD) {
		return fmt.Errorf("risk engine: daily loss limit reached ($%s / max $%s) - new entries paused", dailyRealizedLoss.StringFixed(2), settings.MaxDailyLossUSD.StringFixed(2))
	}
	return nil
}
