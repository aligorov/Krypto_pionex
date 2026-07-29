package autogrid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/grid"
	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Worker struct {
	db       *pgxpool.Pool
	service  *Service
	accounts *accounts.Service
	risk     *risk.Engine
	scanner  *marketdata.Scanner
	logger   *slog.Logger
	owner    string
}

type queuedCommand struct {
	ID          string
	CommandType string
	ActorID     *string
}

func NewWorker(
	db *pgxpool.Pool,
	service *Service,
	accountService *accounts.Service,
	riskEngine *risk.Engine,
	logger *slog.Logger,
) *Worker {
	publicClient := pionex.NewClient("", "", "")
	return &Worker{
		db: db, service: service, accounts: accountService, risk: riskEngine,
		scanner: marketdata.NewScanner(publicClient), logger: logger,
		owner: fmt.Sprintf("autogrid-%d", time.Now().UnixNano()),
	}
}

func (worker *Worker) Run(ctx context.Context) {
	commandTicker := time.NewTicker(time.Second)
	scheduleTicker := time.NewTicker(10 * time.Second)
	reconcileTicker := time.NewTicker(30 * time.Second)
	defer commandTicker.Stop()
	defer scheduleTicker.Stop()
	defer reconcileTicker.Stop()
	worker.logger.Info("AutoGrid worker started", "component", "autogrid_worker")
	for {
		select {
		case <-ctx.Done():
			worker.logger.Info("AutoGrid worker stopped", "component", "autogrid_worker")
			return
		case <-commandTicker.C:
			if err := worker.processNext(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				worker.logger.Error("AutoGrid command failed", "component", "autogrid_worker", "error", err)
			}
		case <-scheduleTicker.C:
			if err := worker.scheduleDueScan(ctx); err != nil {
				worker.logger.Error("schedule AutoGrid scan", "component", "autogrid_worker", "error", err)
			}
		case <-reconcileTicker.C:
			if err := worker.reconcileRealBots(ctx); err != nil {
				worker.logger.Error("reconcile Pionex grids", "component", "autogrid_worker", "error", err)
			}
		}
	}
}

func (worker *Worker) processNext(ctx context.Context) error {
	command, err := worker.claim(ctx)
	if err != nil {
		return err
	}
	executionCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result := map[string]any{}
	switch command.CommandType {
	case "autogrid.start":
		err = worker.start(executionCtx, command)
		result["status"] = "RUNNING"
	case "autogrid.scan":
		var scanID string
		scanID, err = worker.scanAndDeploy(executionCtx, command)
		result["scanId"] = scanID
	case "autogrid.stop":
		err = worker.stop(executionCtx)
		result["status"] = "STOPPED"
	case "autogrid.emergency_stop":
		err = worker.emergencyStop(executionCtx)
		result["status"] = "EMERGENCY_STOPPED"
	default:
		err = fmt.Errorf("unsupported AutoGrid command %q", command.CommandType)
	}
	if err != nil {
		worker.finishCommand(ctx, command.ID, "FAILED", result, err)
		return err
	}
	worker.finishCommand(ctx, command.ID, "SUCCEEDED", result, nil)
	return nil
}

func (worker *Worker) claim(ctx context.Context) (*queuedCommand, error) {
	var command queuedCommand
	err := worker.db.QueryRow(ctx, `
		WITH next_command AS (
			SELECT id
			FROM control_commands
			WHERE status = 'QUEUED'
			  AND command_type IN (
				'autogrid.start', 'autogrid.scan',
				'autogrid.stop', 'autogrid.emergency_stop'
			  )
			  AND (next_retry IS NULL OR next_retry <= NOW())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE control_commands AS command
		SET status = 'EXECUTING', lease_owner = $1,
		    lease_expiry = NOW() + INTERVAL '2 minutes',
		    attempts = attempts + 1, updated_at = NOW()
		FROM next_command
		WHERE command.id = next_command.id
		RETURNING command.id, command.command_type, command.actor_id
	`, worker.owner).Scan(&command.ID, &command.CommandType, &command.ActorID)
	if err != nil {
		return nil, err
	}
	return &command, nil
}

func (worker *Worker) finishCommand(
	ctx context.Context,
	commandID, status string,
	result map[string]any,
	commandErr error,
) {
	var message *string
	if commandErr != nil {
		value := commandErr.Error()
		message = &value
	}
	_, err := worker.db.Exec(ctx, `
		UPDATE control_commands
		SET status = $2, result = $3, error_message = $4,
		    executed_at = NOW(), lease_owner = NULL, lease_expiry = NULL,
		    updated_at = NOW()
		WHERE id = $1
	`, commandID, status, result, message)
	if err != nil {
		worker.logger.Error(
			"finalize AutoGrid command",
			"component", "autogrid_worker", "command_id", commandID, "error", err,
		)
	}
}

func (worker *Worker) start(ctx context.Context, command *queuedCommand) error {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil {
		return err
	}
	if settings.Status == "RUNNING" {
		return nil
	}
	if settings.ExecutionMode == "REAL" {
		if err := worker.realExecutionAllowed(ctx, *settings); err != nil {
			_ = worker.service.SetStatus(ctx, "STOPPED", err)
			return err
		}
	}
	if err := worker.service.SetStatus(ctx, "STARTING", nil); err != nil {
		return err
	}
	if _, err := worker.scanAndDeploy(ctx, command); err != nil {
		_ = worker.service.SetStatus(ctx, "STOPPED", err)
		return err
	}
	return worker.service.SetStatus(ctx, "RUNNING", nil)
}

func (worker *Worker) scanAndDeploy(
	ctx context.Context,
	command *queuedCommand,
) (string, error) {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil {
		return "", err
	}
	requestedBy := ""
	if command.ActorID != nil {
		requestedBy = *command.ActorID
	}
	scanID, err := worker.service.BeginScan(ctx, settings.ID, requestedBy)
	if err != nil {
		return "", err
	}
	candidates, err := worker.scanner.ScanMarkets(ctx, worker.service.scannerConfig(*settings))
	if err != nil {
		worker.service.FailScan(ctx, scanID, err)
		return scanID, err
	}
	if err := worker.service.CompleteScan(ctx, scanID, candidates); err != nil {
		worker.service.FailScan(ctx, scanID, err)
		return scanID, err
	}
	if settings.Status == "RUNNING" || settings.Status == "STARTING" {
		if settings.ExecutionMode == "PAPER" {
			if err := worker.deployPaper(ctx, *settings, scanID); err != nil {
				return scanID, err
			}
		} else {
			if err := worker.deployReal(ctx, *settings, scanID); err != nil {
				return scanID, err
			}
		}
	}
	return scanID, nil
}

func (worker *Worker) deployPaper(
	ctx context.Context,
	settings Settings,
	scanID string,
) error {
	candidates, err := worker.service.listCandidates(ctx, scanID)
	if err != nil {
		return err
	}
	var activeCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID).Scan(&activeCount); err != nil {
		return fmt.Errorf("count active paper grids: %w", err)
	}
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		gridType := "ARITHMETIC"
		if settings.DensityGridEnabled {
			gridType = "GEOMETRIC"
		}
		tag, err := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET candidate_id = $3, mark_price = $4,
			    unrealized_pnl_usdt = CASE
					WHEN direction = 'LONG' THEN
						quote_investment * leverage * ($4 / entry_price - 1)
					WHEN direction = 'SHORT' THEN
						quote_investment * leverage * (1 - $4 / entry_price)
					ELSE 0
				END,
				updated_at = NOW()
			WHERE settings_id = $1 AND symbol = $2 AND status = 'RUNNING'
		`, settings.ID, candidate.Symbol, candidate.ID, candidate.CurrentPrice)
		if err != nil {
			return fmt.Errorf("mark paper grid %s: %w", candidate.Symbol, err)
		}
		if tag.RowsAffected() > 0 || activeCount >= settings.MaxActiveBots {
			continue
		}
		tag, err = worker.db.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, candidate_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, model_state
			) VALUES (
				$1, $2, $3, 'RUNNING', $4, $5, $6, $7, $8, $9, $10, $11, $11,
				jsonb_build_object(
					'model', 'mark_to_market_directional_proxy_v1',
					'gridFillsSimulated', false,
					'warning', 'paper PnL is not a native Pionex grid backtest'
				)
			)
			ON CONFLICT (settings_id, symbol) WHERE status = 'RUNNING'
			DO NOTHING
		`, settings.ID, candidate.ID, candidate.Symbol,
			databaseTrend(candidate.RecommendedTrend), gridType,
			candidate.LowerPrice, candidate.UpperPrice, candidate.GridNum,
			candidate.RecommendedLeverage, settings.BudgetUSDT,
			candidate.CurrentPrice)
		if err != nil {
			return fmt.Errorf("deploy paper grid %s: %w", candidate.Symbol, err)
		}
		if tag.RowsAffected() > 0 {
			activeCount++
		}
	}
	return nil
}

func (worker *Worker) deployReal(
	ctx context.Context,
	settings Settings,
	scanID string,
) error {
	if err := worker.realExecutionAllowed(ctx, settings); err != nil {
		return err
	}
	if settings.AccountID == nil {
		return errors.New("REAL AutoGrid account is missing")
	}
	credentials, err := worker.accounts.Credentials(ctx, *settings.AccountID, true)
	if err != nil {
		return err
	}
	client := pionex.NewClient("", credentials.APIKey, credentials.APISecret)
	manager := grid.NewLifecycleManager(worker.db, client)
	candidates, err := worker.service.listCandidates(ctx, scanID)
	if err != nil {
		return err
	}
	var activeCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE account_id = $1
		  AND status IN (
			'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING',
			'STOP_REQUESTED', 'STOPPING'
		  )
	`, *settings.AccountID).Scan(&activeCount); err != nil {
		return fmt.Errorf("count active real grids: %w", err)
	}
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" || activeCount >= settings.MaxActiveBots {
			continue
		}
		var exists bool
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM grid_bots
				WHERE account_id = $1 AND symbol = $2
				  AND status IN (
					'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING',
					'STOP_REQUESTED', 'STOPPING'
				  )
			)
		`, *settings.AccountID, candidate.Symbol).Scan(&exists); err != nil {
			return fmt.Errorf("check duplicate real grid: %w", err)
		}
		if exists {
			continue
		}
		if err := worker.risk.ValidateNewGrid(
			ctx, *settings.AccountID, candidate.Symbol,
			candidate.RecommendedLeverage, settings.BudgetUSDT,
		); err != nil {
			return err
		}
		base, quote, err := splitPionexPerp(candidate.Symbol)
		if err != nil {
			return err
		}
		data := pionex.BUOrderData{
			Top: candidate.UpperPrice, Bottom: candidate.LowerPrice,
			Row: candidate.GridNum, GridType: mapGridType(settings.DensityGridEnabled),
			Trend:           candidate.RecommendedTrend,
			Leverage:        candidate.RecommendedLeverage,
			QuoteInvestment: settings.BudgetUSDT,
			InvestCoin:      "USDT", InvestmentFrom: "USER",
		}
		if settings.StopLossMode == "ADAPTIVE_ATR" {
			stop := candidate.LowerPrice.Mul(decimal.NewFromFloat(0.98))
			if candidate.RecommendedTrend == "short" {
				stop = candidate.UpperPrice.Mul(decimal.NewFromFloat(1.02))
			}
			data.LossStopType = "price"
			data.LossStop = &stop
		}
		if settings.SmartPNLEnabled && candidate.RecommendedTrend != "no_trend" {
			profit := candidate.UpperPrice
			if candidate.RecommendedTrend == "short" {
				profit = candidate.LowerPrice
			}
			data.ProfitStopType = "price"
			data.ProfitStop = &profit
		}
		_, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
			AccountID:          *settings.AccountID,
			AutoGridSettingsID: &settings.ID,
			IdempotencyKey:     "autogrid:" + scanID + ":" + candidate.ID,
			Params: pionex.NativeFuturesGridCreateParams{
				Base: base, Quote: quote, BUOrderData: data,
			},
		})
		if createErr != nil {
			return createErr
		}
		activeCount++
	}
	return nil
}

func (worker *Worker) stop(ctx context.Context) error {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil {
		return err
	}
	if _, err := worker.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET status = 'STOPPED', closed_at = NOW(), updated_at = NOW()
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID); err != nil {
		return fmt.Errorf("stop paper AutoGrid bots: %w", err)
	}
	return worker.service.SetStatus(ctx, "STOPPED", nil)
}

func (worker *Worker) emergencyStop(ctx context.Context) error {
	if err := worker.risk.SetKillSwitch(ctx, true); err != nil {
		return err
	}
	settings, err := worker.service.GetSettings(ctx)
	if err != nil {
		return err
	}
	_, _ = worker.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET status = 'EMERGENCY_STOPPED', closed_at = NOW(), updated_at = NOW()
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID)
	if settings.AccountID != nil {
		if err := worker.cancelRealBots(ctx, settings); err != nil {
			_ = worker.service.SetStatus(ctx, "EMERGENCY_STOPPED", err)
			return err
		}
	}
	return worker.service.SetStatus(ctx, "EMERGENCY_STOPPED", nil)
}

func (worker *Worker) cancelRealBots(ctx context.Context, settings *Settings) error {
	if settings.AccountID == nil {
		return nil
	}
	credentials, err := worker.accounts.Credentials(ctx, *settings.AccountID, true)
	if err != nil {
		return err
	}
	client := pionex.NewClient("", credentials.APIKey, credentials.APISecret)
	rows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID)
	if err != nil {
		return fmt.Errorf("list real grids for emergency stop: %w", err)
	}
	defer rows.Close()
	type target struct{ id, remoteID string }
	targets := make([]target, 0)
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.remoteID); err != nil {
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, target := range targets {
		_, _ = worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOPPING', reconciliation_state = 'CANCEL_SUBMITTING',
			    updated_at = NOW()
			WHERE id = $1
		`, target.id)
		cancelErr := client.CancelFuturesGridBot(ctx, pionex.CancelFuturesGridParams{
			BUOrderID: target.remoteID, CloseNote: "autogrid emergency stop",
			CloseSellMode: "TO_QUOTE", Immediate: true,
		})
		if cancelErr != nil {
			state := "CANCEL_FAILED"
			if pionex.IsOutcomeUnknown(cancelErr) {
				state = "CANCEL_OUTCOME_UNKNOWN"
			}
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET reconciliation_state = $2, last_error = $3, updated_at = NOW()
				WHERE id = $1
			`, target.id, state, cancelErr.Error())
			return fmt.Errorf("cancel Pionex grid %s: %w", target.remoteID, cancelErr)
		}
		_, _ = worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOP_REQUESTED',
			    reconciliation_state = 'CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING',
			    last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, target.id)
	}
	return nil
}

func (worker *Worker) reconcileRealBots(ctx context.Context) error {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil || settings.AccountID == nil {
		return err
	}
	var count int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID).Scan(&count); err != nil || count == 0 {
		return err
	}
	credentials, err := worker.accounts.Credentials(ctx, *settings.AccountID, true)
	if err != nil {
		return err
	}
	client := pionex.NewClient("", credentials.APIKey, credentials.APISecret)
	rows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id, status FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		ORDER BY updated_at
	`, settings.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type target struct{ id, remoteID, localStatus string }
	targets := make([]target, 0)
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.remoteID, &item.localStatus); err != nil {
			return err
		}
		targets = append(targets, item)
	}
	for _, target := range targets {
		remote, getErr := client.GetFuturesGridBot(ctx, target.remoteID)
		if getErr != nil {
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET reconciliation_state = 'REMOTE_READ_FAILED',
				    last_error = $2, updated_at = NOW()
				WHERE id = $1
			`, target.id, getErr.Error())
			continue
		}
		remoteStatus := remote.Status
		if remote.BUOrderData.Status != "" {
			remoteStatus = remote.BUOrderData.Status
		}
		localStatus := "RUNNING"
		reconciliation := "REST_AUTHORITATIVE_OK"
		if target.localStatus != "RUNNING" {
			localStatus = "STOPPING"
			reconciliation = "REMOTE_TERMINAL_PENDING"
		}
		if terminalRemoteGridStatus(remoteStatus) {
			localStatus = "STOPPED"
			reconciliation = "REMOTE_TERMINAL_CONFIRMED"
		}
		_, _ = worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = $2, reconciliation_state = $3,
			    last_remote_status = $4, last_reconciled_at = NOW(),
			    last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, target.id, localStatus, reconciliation, remoteStatus)
	}
	return rows.Err()
}

func (worker *Worker) scheduleDueScan(ctx context.Context) error {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil || settings.Status != "RUNNING" {
		return err
	}
	var due bool
	err = worker.db.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT completed_at < NOW() - ($2 * INTERVAL '1 second')
			 FROM autogrid_scan_runs
			 WHERE settings_id = $1 AND status = 'SUCCEEDED'
			 ORDER BY completed_at DESC LIMIT 1),
			true
		)
	`, settings.ID, settings.ScanIntervalSeconds).Scan(&due)
	if err != nil || !due {
		return err
	}
	bucket := time.Now().Unix() / int64(settings.ScanIntervalSeconds)
	_, err = worker.db.Exec(ctx, `
		INSERT INTO control_commands (
			actor_type, command_type, resource_type, resource_id,
			arguments, sanitized_arguments, idempotency_key, status
		) VALUES (
			'SYSTEM', 'autogrid.scan', 'autogrid', $1,
			'{}'::jsonb, '{}'::jsonb, $2, 'QUEUED'
		)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, settings.ID, fmt.Sprintf("autogrid-scheduled-%s-%d", settings.ID, bucket))
	if err != nil {
		return fmt.Errorf("queue scheduled AutoGrid scan: %w", err)
	}
	return nil
}

func (worker *Worker) realExecutionAllowed(ctx context.Context, settings Settings) error {
	if settings.AccountID == nil {
		return errors.New("REAL AutoGrid requires a Pionex account")
	}
	if err := worker.risk.ValidateNewOrder(ctx, settings.Leverage, settings.BudgetUSDT); err != nil {
		return err
	}
	var configEnabled, featureEnabled bool
	err := worker.db.QueryRow(ctx, `
		SELECT
			COALESCE((
				SELECT (value #>> '{}')::BOOLEAN
				FROM app_config WHERE key = 'real_grid_execution_enabled'
			), false),
			COALESCE((
				SELECT enabled FROM feature_flags WHERE name = 'real_native_grid'
			), false)
	`).Scan(&configEnabled, &featureEnabled)
	if err != nil {
		return fmt.Errorf("load real grid execution gates: %w", err)
	}
	if !configEnabled || !featureEnabled {
		return errors.New("REAL AutoGrid is blocked by real_grid_execution_enabled or real_native_grid")
	}
	var enabled, readPermission, futuresPermission, botPermission bool
	err = worker.db.QueryRow(ctx, `
		SELECT is_enabled, has_read_permission,
		       has_futures_permission, has_bot_permission
		FROM pionex_accounts WHERE id = $1
	`, *settings.AccountID).Scan(
		&enabled, &readPermission, &futuresPermission, &botPermission,
	)
	if err != nil {
		return fmt.Errorf("load REAL AutoGrid account: %w", err)
	}
	if !enabled || !readPermission || !futuresPermission || !botPermission {
		return errors.New("REAL AutoGrid account is not verified and enabled for declared Futures/Bot permissions")
	}
	return nil
}

func splitPionexPerp(symbol string) (string, string, error) {
	const suffix = "_PERP"
	if !strings.HasSuffix(symbol, suffix) {
		return "", "", fmt.Errorf("invalid Pionex PERP symbol %q", symbol)
	}
	pair := strings.TrimSuffix(symbol, suffix)
	separator := strings.LastIndex(pair, "_")
	if separator <= 0 || separator == len(pair)-1 {
		return "", "", fmt.Errorf("invalid Pionex PERP symbol %q", symbol)
	}
	return pair[:separator], pair[separator+1:], nil
}

func databaseTrend(trend string) string {
	switch trend {
	case "long":
		return "LONG"
	case "short":
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}

func terminalRemoteGridStatus(status string) bool {
	normalized := strings.ToLower(strings.TrimSpace(status))
	for _, marker := range []string{"cancel", "closed", "finish", "stopped", "liquidat", "expired"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
