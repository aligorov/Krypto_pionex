package grid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

const (
	StateDraft             = "DRAFT"
	StatePendingSubmission = "PENDING_SUBMISSION"
	StateSubmissionUnknown = "SUBMISSION_UNKNOWN"
	StateRunning           = "RUNNING"
	StateStopping          = "STOPPING"
	StateStopped           = "STOPPED"
	StateFailed            = "FAILED"
)

type LifecycleManager struct {
	db           *pgxpool.Pool
	pionexClient *pionex.Client
}

type CreateInput struct {
	AccountID          string
	AutoGridSettingsID *string
	IdempotencyKey     string
	Params             pionex.NativeFuturesGridCreateParams
	// PnLTargetUSDT / MaxLossUSDT capture the deploy-time dynamic targets so
	// the supervision loop uses the numbers the market offered at open.
	PnLTargetUSDT *decimal.Decimal
	MaxLossUSDT   *decimal.Decimal
}

func NewLifecycleManager(db *pgxpool.Pool, pionexClient *pionex.Client) *LifecycleManager {
	return &LifecycleManager{db: db, pionexClient: pionexClient}
}

// CreateGridBot validates the symbol against Pionex, persists submission intent,
// and marks RUNNING only after a non-empty remote buOrderId is durably stored.
func (manager *LifecycleManager) CreateGridBot(
	ctx context.Context,
	input CreateInput,
) (string, error) {
	if err := validateCreateInput(input); err != nil {
		return "", err
	}
	baseCoin := strings.TrimSuffix(strings.TrimSuffix(input.Params.Base, ".PERP"), "_PERP")
	symbol := fmt.Sprintf("%s_%s_PERP", baseCoin, input.Params.Quote)
	input.Params.Base = fmt.Sprintf("%s.PERP", baseCoin)
	if err := manager.validatePionexSymbol(ctx, symbol); err != nil {
		return "", err
	}
	fingerprint, err := requestFingerprint(input)
	if err != nil {
		return "", err
	}
	var gridID string
	err = manager.db.QueryRow(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, stop_loss, take_profit,
			request_fingerprint, execution_mode, reconciliation_state,
			pnl_target_usdt, max_loss_usdt
		) VALUES (
			$1, $2, $3, 'PENDING_SUBMISSION', $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, 'REAL', 'PENDING', $15, $16
		)
		ON CONFLICT (request_fingerprint) DO NOTHING
		RETURNING id
	`, input.AccountID, input.AutoGridSettingsID, symbol,
		databaseDirection(input.Params.BUOrderData.Trend),
		strings.ToUpper(input.Params.BUOrderData.GridType),
		input.Params.BUOrderData.Bottom, input.Params.BUOrderData.Top,
		input.Params.BUOrderData.Row, input.Params.BUOrderData.Leverage,
		input.Params.BUOrderData.QuoteInvestment,
		input.Params.BUOrderData.ExtraMargin,
		input.Params.BUOrderData.LossStop,
		input.Params.BUOrderData.ProfitStop,
		fingerprint,
	).Scan(&gridID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = manager.db.QueryRow(ctx, `
			SELECT id FROM grid_bots WHERE request_fingerprint = $1
		`, fingerprint).Scan(&gridID)
		if err != nil {
			return "", fmt.Errorf("load idempotent grid submission: %w", err)
		}
		return gridID, nil
	}
	if err != nil {
		return "", fmt.Errorf("record pending grid bot: %w", err)
	}

	result, remoteErr := manager.pionexClient.CreateFuturesGridBot(ctx, input.Params)
	if remoteErr != nil {
		state := StateFailed
		reconciliation := "FAILED_AUTHORITATIVE"
		if pionex.IsOutcomeUnknown(remoteErr) {
			state = StateSubmissionUnknown
			reconciliation = "REMOTE_OUTCOME_UNKNOWN"
		}
		_, _ = manager.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = $2, reconciliation_state = $3, last_error = $4,
			    updated_at = NOW()
			WHERE id = $1
		`, gridID, state, reconciliation, remoteErr.Error())
		return "", fmt.Errorf("create remote Pionex futures grid: %w", remoteErr)
	}
	if result == nil || strings.TrimSpace(result.BUOrderID) == "" {
		message := "Pionex create response did not contain buOrderId"
		_, _ = manager.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'SUBMISSION_UNKNOWN',
			    reconciliation_state = 'REMOTE_ID_MISSING',
			    last_error = $2, updated_at = NOW()
			WHERE id = $1
		`, gridID, message)
		return "", errors.New(message)
	}

	tag, err := manager.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'RUNNING', bu_order_id = $2,
		    reconciliation_state = 'REMOTE_ID_PERSISTED',
		    last_remote_status = 'CREATED', last_error = NULL,
		    last_reconciled_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'PENDING_SUBMISSION'
	`, gridID, result.BUOrderID)
	if err != nil {
		return "", fmt.Errorf("persist remote Pionex buOrderId: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return "", errors.New("grid state changed before remote id could be persisted")
	}
	return gridID, nil
}

func (manager *LifecycleManager) validatePionexSymbol(
	ctx context.Context,
	target string,
) error {
	symbols, err := manager.pionexClient.GetMarketSymbols(ctx, "PERP")
	if err != nil {
		return fmt.Errorf("validate Pionex symbol: %w", err)
	}
	for _, symbol := range symbols {
		if symbol.Symbol == target && symbol.Type == "PERP" && symbol.IsTrading() {
			return nil
		}
	}
	return fmt.Errorf("symbol %s is not an enabled Pionex PERP symbol", target)
}

func validateCreateInput(input CreateInput) error {
	if strings.TrimSpace(input.AccountID) == "" {
		return errors.New("Pionex account id is required")
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return errors.New("grid idempotency key is required")
	}
	data := input.Params.BUOrderData
	if strings.TrimSpace(input.Params.Base) == "" || strings.TrimSpace(input.Params.Quote) == "" {
		return errors.New("grid base and quote are required")
	}
	if !data.Top.GreaterThan(data.Bottom) || !data.Bottom.GreaterThan(decimal.Zero) {
		return errors.New("grid top must be greater than positive bottom")
	}
	if data.Row < 2 || data.Row > 500 {
		return errors.New("grid row must be between 2 and 500")
	}
	if data.Leverage < 1 || data.Leverage > 100 {
		return errors.New("grid leverage must be between 1 and 100")
	}
	if !data.QuoteInvestment.GreaterThan(decimal.Zero) {
		return errors.New("grid quote investment must be positive")
	}
	if data.GridType != "arithmetic" && data.GridType != "geometric" {
		return errors.New("grid_type must be arithmetic or geometric")
	}
	switch data.Trend {
	case "long", "short", "no_trend":
	default:
		return errors.New("grid trend must be long, short or no_trend")
	}
	return nil
}

func requestFingerprint(input CreateInput) (string, error) {
	body, err := json.Marshal(struct {
		AccountID      string                               `json:"accountId"`
		IdempotencyKey string                               `json:"idempotencyKey"`
		Params         pionex.NativeFuturesGridCreateParams `json:"params"`
	}{
		AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
		Params: input.Params,
	})
	if err != nil {
		return "", fmt.Errorf("marshal grid fingerprint: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func databaseDirection(trend string) string {
	switch trend {
	case "long":
		return "LONG"
	case "short":
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}
