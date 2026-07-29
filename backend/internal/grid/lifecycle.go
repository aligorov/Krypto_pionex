package grid

import (
	"context"
	"errors"
	"fmt"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State Constants
const (
	StateDraft             = "DRAFT"
	StatePendingSubmission = "PENDING_SUBMISSION"
	StateSubmitted         = "SUBMITTED"
	StateRunning           = "RUNNING"
	StateStopping          = "STOPPING"
	StateStopped           = "STOPPED"
	StateFailed            = "FAILED"
)

// LifecycleManager controls state transitions for Native Futures Grid Bots.
type LifecycleManager struct {
	db           *pgxpool.Pool
	pionexClient *pionex.Client
}

// NewLifecycleManager creates a LifecycleManager instance.
func NewLifecycleManager(db *pgxpool.Pool, pionexClient *pionex.Client) *LifecycleManager {
	return &LifecycleManager{
		db:           db,
		pionexClient: pionexClient,
	}
}

// CreateGridBot validates parameters, records PENDING_SUBMISSION state, and executes /api/v1/bot/orders/futuresGrid/create.
func (lm *LifecycleManager) CreateGridBot(ctx context.Context, accountID string, params pionex.NativeFuturesGridCreateParams) (string, error) {
	symbol := fmt.Sprintf("%s_%s_PERP", params.Base, params.Quote)

	// 1. Record submission intent in PostgreSQL
	var gridID string
	query := `
		INSERT INTO grid_bots (account_id, symbol, status, lower_price, upper_price, grid_num, leverage, quote_investment, request_fingerprint)
		VALUES ($1, $2, 'PENDING_SUBMISSION', $3, $4, $5, $6, $7, $8)
		RETURNING id
	`
	fingerprint := fmt.Sprintf("%s_%s_%s_%d", accountID, symbol, params.BUOrderData.LowerPrice.String(), params.BUOrderData.GridNum)
	err := lm.db.QueryRow(ctx, query, accountID, symbol, params.BUOrderData.LowerPrice, params.BUOrderData.UpperPrice, params.BUOrderData.GridNum, params.BUOrderData.Leverage, params.BUOrderData.QuoteInvestment, fingerprint).Scan(&gridID)
	if err != nil {
		return "", fmt.Errorf("failed to record pending grid bot: %w", err)
	}

	// 2. Execute remote Pionex API creation
	result, err := lm.pionexClient.CreateFuturesGridBot(ctx, params)
	if err != nil {
		_, _ = lm.db.Exec(ctx, "UPDATE grid_bots SET status = 'FAILED' WHERE id = $1", gridID)
		return "", fmt.Errorf("remote grid creation failed: %w", err)
	}

	if result.BUOrderID == "" {
		_, _ = lm.db.Exec(ctx, "UPDATE grid_bots SET status = 'SUBMISSION_UNKNOWN' WHERE id = $1", gridID)
		return "", errors.New("remote grid created but buOrderId is missing - setting SUBMISSION_UNKNOWN")
	}

	// 3. Update state to RUNNING only after remote buOrderId is confirmed
	_, err = lm.db.Exec(ctx, "UPDATE grid_bots SET status = 'RUNNING', bu_order_id = $1, updated_at = NOW() WHERE id = $2", result.BUOrderID, gridID)
	if err != nil {
		return "", fmt.Errorf("failed to update grid bot to RUNNING state: %w", err)
	}

	return gridID, nil
}
