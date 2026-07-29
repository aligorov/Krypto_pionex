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

// RiskSettings represents durable settings loaded from PostgreSQL.
type RiskSettings struct {
	ID                    int             `db:"id" json:"id"`
	KillSwitchEnabled     bool            `db:"kill_switch_enabled" json:"killSwitchEnabled"`
	MaxAccountExposureUSD decimal.Decimal `db:"max_account_exposure_usd" json:"maxAccountExposureUsd"`
	MaxSymbolExposureUSD  decimal.Decimal `db:"max_symbol_exposure_usd" json:"maxSymbolExposureUsd"`
	MaxDailyLossUSD       decimal.Decimal `db:"max_daily_loss_usd" json:"maxDailyLossUsd"`
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
		       max_daily_loss_usd, max_leverage, max_active_grid_bots, max_open_positions
		FROM risk_settings WHERE id = 1
	`
	row := e.db.QueryRow(ctx, query)
	var s RiskSettings
	err := row.Scan(
		&s.ID, &s.KillSwitchEnabled, &s.MaxAccountExposureUSD, &s.MaxSymbolExposureUSD,
		&s.MaxDailyLossUSD, &s.MaxLeverage, &s.MaxActiveGridBots, &s.MaxOpenPositions,
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

	_, err := e.db.Exec(ctx, `
		UPDATE risk_settings
		SET kill_switch_enabled = $2,
		    max_account_exposure_usd = $3,
		    max_symbol_exposure_usd = $4,
		    max_daily_loss_usd = $5,
		    max_leverage = $6,
		    max_active_grid_bots = $7,
		    max_open_positions = $8,
		    updated_at = NOW()
		WHERE id = $1
	`, settings.ID, settings.KillSwitchEnabled, settings.MaxAccountExposureUSD,
		settings.MaxSymbolExposureUSD, settings.MaxDailyLossUSD, settings.MaxLeverage,
		settings.MaxActiveGridBots, settings.MaxOpenPositions)
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
