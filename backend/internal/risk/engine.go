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
	ErrLeverageExceeded       = errors.New("risk engine: leverage exceeds maximum allowed limit")
)

// RiskSettings represents durable settings loaded from PostgreSQL.
type RiskSettings struct {
	ID                    int             `db:"id"`
	KillSwitchEnabled     bool            `db:"kill_switch_enabled"`
	MaxAccountExposureUSD decimal.Decimal `db:"max_account_exposure_usd"`
	MaxSymbolExposureUSD  decimal.Decimal `db:"max_symbol_exposure_usd"`
	MaxDailyLossUSD       decimal.Decimal `db:"max_daily_loss_usd"`
	MaxLeverage           int             `db:"max_leverage"`
	MaxActiveGridBots     int             `db:"max_active_grid_bots"`
	MaxOpenPositions      int             `db:"max_open_positions"`
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
