package risk

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// Tx is the executor surface the in-transaction admission re-checks need.
// Both *pgxpool.Pool and pgx.Tx satisfy it.
type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrStaleSettings is returned by UpdateSettings when the caller's copy of the
// row predates a concurrent write (v2.0.196).
var ErrStaleSettings = errors.New("risk engine: risk settings changed since they were loaded - reload and retry")

// openGridStatuses are the grid_bots states that hold live capital: the bot is
// on the exchange, or its state there is not yet known.
const openGridStatuses = `'PENDING_SUBMISSION','SUBMISSION_UNKNOWN','RUNNING','STOP_REQUESTED','STOPPING'`

// LockEntryGate serializes the admission window for one scope. The read-then-
// insert the deploy paths perform is not atomic: ValidateNewGrid counted N
// active bots, then the exchange call ran for seconds, then the row landed —
// two overlapping deploys (the autopilot scan and a manual PUT, or two scans
// on different symbols of one account) both saw N and both wrote, so the
// caps were advisory under concurrency. The lock is TRANSACTION-scoped, so it
// cannot leak past a commit, a rollback or a dropped connection.
//
// Call it inside the tx that writes the row, before re-running the caps with
// RecheckGridEntryCaps / RecheckPaperEntryCaps.
func LockEntryGate(ctx context.Context, tx Tx, scope, key string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, scope, key); err != nil {
		return fmt.Errorf("risk engine: acquire entry gate lock: %w", err)
	}
	return nil
}

// RecheckGridEntryCaps re-runs the durable REAL admission gates inside the
// create transaction, under LockEntryGate, so the numbers it reads cannot go
// stale before the intent row commits. It is the enforcement point;
// ValidateNewGrid stays as the early, cheap refusal that keeps a doomed
// deploy from ever calling the exchange.
func RecheckGridEntryCaps(
	ctx context.Context,
	tx Tx,
	accountID, symbol string,
	requestedLeverage int,
	investmentUSD decimal.Decimal,
) error {
	settings, err := loadSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	if settings.KillSwitchEnabled {
		return ErrKillSwitchActive
	}
	if requestedLeverage > settings.MaxLeverage {
		return fmt.Errorf("%w: requested %d, max allowed %d",
			ErrLeverageExceeded, requestedLeverage, settings.MaxLeverage)
	}
	var accountExposure, symbolExposure decimal.Decimal
	var activeBots, openPositions int
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(quote_investment * leverage), 0),
			COALESCE(SUM(quote_investment * leverage) FILTER (WHERE symbol = $2), 0),
			COUNT(*)
		FROM grid_bots
		WHERE account_id = $1
		  AND execution_mode = 'REAL'
		  AND status IN (`+openGridStatuses+`)
	`, accountID, symbol).Scan(&accountExposure, &symbolExposure, &activeBots); err != nil {
		return fmt.Errorf("risk engine: re-read active grid exposure (fail-closed): %w", err)
	}
	// max_open_positions was stored, validated non-negative and displayed in
	// the UI, but nothing ever counted against it (v2.0.196). Positions here
	// are the account's live orders: grids plus pattern orders still open.
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM pattern_orders
		WHERE account_id = $1
		  AND status IN ('CREATED', 'SUBMITTED', 'PARTIALLY_FILLED')
	`, accountID).Scan(&openPositions); err != nil {
		return fmt.Errorf("risk engine: re-read open pattern positions (fail-closed): %w", err)
	}
	openPositions += activeBots
	if activeBots >= settings.MaxActiveGridBots {
		return fmt.Errorf("risk engine: active grid limit reached: current %d, max %d",
			activeBots, settings.MaxActiveGridBots)
	}
	if settings.MaxOpenPositions > 0 && openPositions >= settings.MaxOpenPositions {
		return fmt.Errorf("risk engine: open position limit reached: current %d, max %d",
			openPositions, settings.MaxOpenPositions)
	}
	requestedNotional := investmentUSD.Mul(decimal.NewFromInt(int64(requestedLeverage)))
	if accountExposure.Add(requestedNotional).GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf("%w: current %s + requested %s, max %s",
			ErrExposureLimitExceeded, accountExposure, requestedNotional,
			settings.MaxAccountExposureUSD)
	}
	if symbolExposure.Add(requestedNotional).GreaterThan(settings.MaxSymbolExposureUSD) {
		return fmt.Errorf("risk engine: symbol exposure limit exceeded: current %s + requested %s, max %s",
			symbolExposure, requestedNotional, settings.MaxSymbolExposureUSD)
	}
	return nil
}

// RecheckPaperEntryCaps is the PAPER twin of RecheckGridEntryCaps: same
// transactional admission, minus the daily-loss brake ValidateNewPaperGrid
// exempts paper from.
func RecheckPaperEntryCaps(
	ctx context.Context,
	tx Tx,
	symbol string,
	requestedLeverage int,
	slotBudgetUSD decimal.Decimal,
) error {
	settings, err := loadSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	if settings.KillSwitchEnabled {
		return ErrKillSwitchActive
	}
	if requestedLeverage > settings.MaxLeverage {
		return fmt.Errorf("%w: requested %d, max allowed %d",
			ErrLeverageExceeded, requestedLeverage, settings.MaxLeverage)
	}
	var accountNotional, symbolNotional decimal.Decimal
	var activeBots int
	if err := tx.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(quote_investment * leverage), 0),
			COALESCE(SUM(quote_investment * leverage) FILTER (WHERE symbol = $1), 0),
			COUNT(*)
		FROM paper_grid_bots
		WHERE status = 'RUNNING'
	`, symbol).Scan(&accountNotional, &symbolNotional, &activeBots); err != nil {
		return fmt.Errorf("risk engine: re-read paper exposure (fail-closed): %w", err)
	}
	if activeBots >= settings.MaxActiveGridBots {
		return fmt.Errorf("risk engine: paper active grid limit reached: current %d, max %d",
			activeBots, settings.MaxActiveGridBots)
	}
	requestedNotional := slotBudgetUSD.Mul(decimal.NewFromInt(int64(requestedLeverage)))
	if accountNotional.Add(requestedNotional).GreaterThan(settings.MaxAccountExposureUSD) {
		return fmt.Errorf("%w (paper notional): current %s + requested %s, max %s",
			ErrExposureLimitExceeded, accountNotional, requestedNotional,
			settings.MaxAccountExposureUSD)
	}
	if symbolNotional.Add(requestedNotional).GreaterThan(settings.MaxSymbolExposureUSD) {
		return fmt.Errorf("risk engine: paper symbol exposure (notional) exceeded: current %s + requested %s, max %s",
			symbolNotional, requestedNotional, settings.MaxSymbolExposureUSD)
	}
	return nil
}

func loadSettingsTx(ctx context.Context, tx Tx) (*RiskSettings, error) {
	var s RiskSettings
	if err := tx.QueryRow(ctx, `
		SELECT id, kill_switch_enabled, max_account_exposure_usd, max_symbol_exposure_usd,
		       max_daily_loss_usd, breaker_mode, max_leverage, max_active_grid_bots,
		       max_open_positions, updated_at
		FROM risk_settings WHERE id = 1
	`).Scan(
		&s.ID, &s.KillSwitchEnabled, &s.MaxAccountExposureUSD, &s.MaxSymbolExposureUSD,
		&s.MaxDailyLossUSD, &s.BreakerMode, &s.MaxLeverage, &s.MaxActiveGridBots,
		&s.MaxOpenPositions, &s.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("failed to query risk settings: %w", err)
	}
	return &s, nil
}
