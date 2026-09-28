package autogrid

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Legacy bot_aggregate rows used the Futures trader wallet. Never reuse them
// as proof of Spot funding. Capture runs independently of admission, so a
// missing snapshot can safely defer entry until the next manage pass.
func loadBotFundingReserve(ctx context.Context, db *pgxpool.Pool, accountID string) (decimal.Decimal, decimal.Decimal, decimal.Decimal, error) {
	var equity, committed, available, invested decimal.Decimal
	var captured time.Time
	err := db.QueryRow(ctx, `
		SELECT equity_usdt, captured_at, available_usdt, assets_usdt FROM account_equity_snapshots
		WHERE account_id = $1 AND source = $2
		ORDER BY captured_at DESC LIMIT 1
	`, accountID, equitySnapshotSourceBotAggregate).Scan(&equity, &captured, &available, &invested)
	if err != nil {
		return equity, committed, decimal.Zero, fmt.Errorf("read Spot funding snapshot: %w", err)
	}
	// 10-minute future tolerance: captured_at is DB NOW() compared against
	// app-process time, and cross-container clock skew must not read fresh
	// Spot evidence as invalid.
	if time.Since(captured) > 2*equitySnapshotMinSpacing || captured.After(time.Now().Add(10*time.Minute)) {
		return equity, committed, decimal.Zero, fmt.Errorf("Spot funding snapshot is stale or has invalid timestamp")
	}
	// trancheBase is the FULL planned slot, while quote_investment holds
	// only tranche 1 until top-up. Reserve the unpaid remainder as well.
	// A corrupt marker degrades to the 2× fallback instead of poisoning
	// the whole SUM (one bad row must not freeze the fleet's math).
	err = db.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN model_state->>'trancheDeployed' = '1'
		    THEN GREATEST(quote_investment, COALESCE(CASE WHEN model_state->>'trancheBase' ~ '^[0-9]+(\.[0-9]+)?$'
		        THEN (model_state->>'trancheBase')::NUMERIC END, quote_investment * 2))
			    ELSE quote_investment END), 0)
		FROM grid_bots WHERE account_id = $1 AND status IN
		('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, accountID).Scan(&committed)
	if err != nil {
		return equity, committed, decimal.Zero, fmt.Errorf("read bot funding commitments: %w", err)
	}
	// Deduct commitments not already included in the snapshot's invested
	// capital: new bots plus unpaid tranches. Do not spend locked bot PnL.
	spendable := decimal.Max(decimal.Zero, available.Sub(decimal.Max(decimal.Zero, committed.Sub(invested))))
	return equity, committed, spendable, nil
}

func fitBotFundingBudget(equity, committed, budget decimal.Decimal, trancheOn bool) (decimal.Decimal, bool) {
	if !equity.IsPositive() || !budget.IsPositive() || committed.IsNegative() {
		return decimal.Zero, true
	}
	room := equity.Mul(decimal.NewFromFloat(0.70)).Sub(committed).Floor()
	// Budget is already the FULL slot. deployReal halves it at creation
	// when tranches are enabled; dividing it again reserves the slot twice.
	fit := decimal.Min(budget, room)
	minimum := decimal.NewFromInt(10)
	if !trancheOn {
		minimum = decimal.NewFromInt(5)
	}
	if fit.LessThan(minimum) {
		return decimal.Zero, true
	}
	return fit, !fit.Equal(budget)
}
