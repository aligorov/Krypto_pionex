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
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type Worker struct {
	db           *pgxpool.Pool
	service      *Service
	accounts     *accounts.Service
	risk         *risk.Engine
	scanner      *marketdata.Scanner
	publicClient *pionex.Client
	llm          *llm.Service
	logger       *slog.Logger
	owner        string
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
	llmService *llm.Service,
	logger *slog.Logger,
) *Worker {
	publicClient := service.PublicAPI()
	return &Worker{
		db: db, service: service, accounts: accountService, risk: riskEngine,
		scanner: marketdata.NewScanner(publicClient), publicClient: publicClient,
		llm:    llmService,
		logger: logger,
		owner:  fmt.Sprintf("autogrid-%d", time.Now().UnixNano()),
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
			interval, err := worker.reconcileAndManage(ctx)
			if err != nil {
				worker.logger.Error("reconcile and manage Pionex grids", "component", "autogrid_worker", "error", err)
			}
			if seconds := interval; seconds >= 15 && seconds <= 3600 {
				reconcileTicker.Reset(time.Duration(seconds) * time.Second)
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
	if settings.AIKitEnabled {
		if err := worker.enrichCandidatesWithAIKit(ctx, *settings, scanID); err != nil {
			worker.logger.Warn(
				"Pionex AI Kit enrichment failed (advisory only)",
				"component", "autogrid_worker", "scan_id", scanID, "error", err,
			)
		}
	}
	if worker.llm != nil {
		if err := worker.enrichAndAuditCandidatesWithLLM(ctx, *settings, scanID); err != nil {
			worker.logger.Warn(
				"LLM intelligence audit failed",
				"component", "autogrid_worker", "scan_id", scanID, "error", err,
			)
		}
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

// enrichCandidatesWithAIKit runs every scanned pair through the native Pionex
// AI Kit (GET /api/v1/bot/orders/spotGrid/aiStrategy). Per AGENTS.md the AI
// price parameters are Spot-only, so they are stored as advisory market
// intelligence and never applied to Futures Grid bots.
func (worker *Worker) enrichCandidatesWithAIKit(
	ctx context.Context,
	settings Settings,
	scanID string,
) error {
	accountID, err := worker.service.resolveAccount(ctx)
	if err != nil || accountID == nil {
		return err
	}
	client, err := worker.service.PrivateClient(ctx, worker.accounts, *accountID)
	if err != nil {
		return err
	}
	candidates, err := worker.service.listCandidates(ctx, scanID)
	if err != nil {
		return err
	}
	enriched := 0
	for _, candidate := range candidates {
		if enriched >= 5 {
			break
		}
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		base, quote, err := SplitPionexPerp(candidate.Symbol)
		if err != nil {
			continue
		}
		strategy, err := client.GetSpotGridAIStrategy(ctx, base, quote)
		if err != nil {
			worker.logger.Warn(
				"Pionex AI Kit lookup failed for candidate",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "error", err,
			)
			continue
		}
		if _, err := worker.db.Exec(ctx, `
			UPDATE autogrid_candidates
			SET model_assumptions = model_assumptions || $3::jsonb,
			    grid_num = CASE
					WHEN $4::INT BETWEEN 2 AND 500 THEN $4::INT
					ELSE grid_num
				END
			WHERE id = $1 AND scan_id = $2
		`, candidate.ID, scanID, map[string]any{
			"aiKit": map[string]any{
				"annualized":    strategy.Annualized,
				"volatility":    strategy.Volatility,
				"maxDrawDown":   strategy.MaxDrawDown,
				"spotHigh":      strategy.High,
				"spotLow":       strategy.Low,
				"gridCount":     strategy.GridCount,
				"gridCountSource": "pionex_ai_kit",
				"boundary": "AI_GRID_COUNT_ADOPTED_WITH_CLAMP_2_500_RANGE_STAYS_SCANNER_SR",
			},
		}, strategy.GridCount); err != nil {
			return fmt.Errorf("persist AI Kit advisory for %s: %w", candidate.Symbol, err)
		}
		enriched++
	}
	return nil
}

// computeBotTargets derives the per-bot PnL target and stop-out. In DYNAMIC
// mode the numbers come from the pair's own readings (native AI Kit estimate
// when enriched, otherwise the scanner sigma/ATR blend and model drawdown),
// scaled to the budget; FIXED mode returns the operator's amounts.
func computeBotTargets(settings Settings, candidate Candidate) (*decimal.Decimal, *decimal.Decimal) {
	if settings.PnLTargetMode != "DYNAMIC" {
		if settings.PnLTargetUSDT.IsZero() || settings.MaxLossUSDT.IsZero() {
			return nil, nil
		}
		target, loss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		return &target, &loss
	}
	var aiVol, aiDD float64
	if aiKit, ok := candidate.ModelAssumptions["aiKit"].(map[string]any); ok {
		aiVol = percentReading(aiKit["volatility"])
		aiDD = percentReading(aiKit["maxDrawDown"])
	}
	var atr float64
	if value, ok := candidate.ModelAssumptions["atrPct"].(float64); ok {
		atr = value
	}
	vol, _ := candidate.VolatilityPct.Float64()
	drawdown, _ := candidate.MaxDrawdownPct.Float64()
	targets := marketdata.ComputeDynamicTargets(marketdata.DynamicTargetsInput{
		Budget:               settings.BudgetUSDT.InexactFloat64(),
		AIVolatilityPct:      aiVol,
		AIDrawdownPct:        aiDD,
		ScannerVolatilityPct: vol,
		ScannerATRPct:        atr,
		ScannerDrawdownPct:   drawdown,
	})
	target := decimal.NewFromFloat(targets.TargetUSDT)
	loss := decimal.NewFromFloat(targets.MaxLossUSDT)
	return &target, &loss
}

// percentReading normalizes an AI Kit metric that may arrive as a ratio
// (0.05) or already as percent (5).
func percentReading(value any) float64 {
	number, ok := value.(float64)
	if !ok || number <= 0 {
		return 0
	}
	if number < 1 {
		return number * 100
	}
	if number <= 200 {
		return number
	}
	return 0
}

// isEntryTimingFavorable validates that the current price is positioned
// favorably within the channel structure before launching a directional grid.
func isEntryTimingFavorable(candidate Candidate) bool {
	rangePos := 50.0
	if val, ok := candidate.ModelAssumptions["rangePositionPct"].(float64); ok {
		rangePos = val
	} else if candidate.UpperPrice.GreaterThan(candidate.LowerPrice) && candidate.CurrentPrice.GreaterThan(decimal.Zero) {
		rangeSpan := candidate.UpperPrice.Sub(candidate.LowerPrice)
		currentOffset := candidate.CurrentPrice.Sub(candidate.LowerPrice)
		pos, _ := currentOffset.Div(rangeSpan).Mul(decimal.NewFromInt(100)).Float64()
		rangePos = pos
	}

	switch candidate.RecommendedTrend {
	case "long":
		return rangePos <= 70.0
	case "short":
		return rangePos >= 30.0
	default:
		return true
	}
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
	// Portfolio Circuit Breaker: if >= 3 stop-losses in the last 1 hour, pause new bot deployments
	var recentStopLossCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM paper_grid_bots
			WHERE settings_id = $1
			  AND status = 'COMPLETED'
			  AND closed_reason IN ('STOP_LOSS', 'STOP_LOSS_NATIVE')
			  AND closed_at > NOW() - INTERVAL '1 hour'
			UNION ALL
			SELECT 1 FROM grid_bots
			WHERE status = 'STOPPED'
			  AND closed_reason IN ('STOP_LOSS', 'STOP_LOSS_NATIVE', 'loss_stop')
			  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '1 hour'
		) recent_stops
	`, settings.ID).Scan(&recentStopLossCount); err == nil && recentStopLossCount >= 3 {
		worker.logger.Warn("Portfolio circuit breaker: recent stop-losses holding new deployments", "recentStopLossCount", recentStopLossCount)
		return nil
	}
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		if !isEntryTimingFavorable(candidate) {
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
		// Cooldown check: do not reopen a symbol if it stopped out within 2 hours
		var recentlyStopped bool
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM paper_grid_bots
				WHERE settings_id = $1 AND symbol = $2
				  AND status = 'COMPLETED'
				  AND closed_reason IN ('STOP_LOSS', 'STOP_LOSS_NATIVE')
				  AND closed_at > NOW() - INTERVAL '2 hours'
			)
		`, settings.ID, candidate.Symbol).Scan(&recentlyStopped); err == nil && recentlyStopped {
			continue
		}
		target, maxLoss := computeBotTargets(settings, candidate)
		lev := candidate.RecommendedLeverage
		if lev < 2 && settings.Leverage >= 2 {
			lev = 2
		}
		tag, err = worker.db.Exec(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, candidate_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, model_state,
				pnl_target_usdt, max_loss_usdt
			) VALUES (
				$1, $2, $3, 'RUNNING', $4, $5, $6, $7, $8, $9, $10, $11, $11,
				jsonb_build_object(
					'model', 'mark_to_market_directional_proxy_v1',
					'gridFillsSimulated', false,
					'pnlTargetSource', $12::TEXT,
					'warning', 'paper PnL is not a native Pionex grid backtest'
				),
				$13, $14
			)
			ON CONFLICT (settings_id, symbol) WHERE status = 'RUNNING'
			DO NOTHING
		`, settings.ID, candidate.ID, candidate.Symbol,
			databaseTrend(candidate.RecommendedTrend), gridType,
			candidate.LowerPrice, candidate.UpperPrice, candidate.GridNum,
			lev, settings.BudgetUSDT,
			candidate.CurrentPrice, settings.PnLTargetMode, target, maxLoss)
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
		resolved, err := worker.service.resolveAccount(ctx)
		if err != nil {
			return fmt.Errorf("REAL AutoGrid account is missing: %w", err)
		}
		settings.AccountID = resolved
	}
	client, err := worker.service.PrivateClient(ctx, worker.accounts, *settings.AccountID)
	if err != nil {
		return err
	}
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
	// Portfolio Circuit Breaker: if >= 3 stop-losses in the last 1 hour, pause new bot deployments
	var recentStopLossCountReal int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE status = 'STOPPED'
		  AND closed_reason IN ('STOP_LOSS', 'STOP_LOSS_NATIVE', 'loss_stop')
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '1 hour'
	`).Scan(&recentStopLossCountReal); err == nil && recentStopLossCountReal >= 3 {
		worker.logger.Warn("Portfolio circuit breaker: recent real stop-losses holding new deployments", "recentStopLossCountReal", recentStopLossCountReal)
		return nil
	}
	deployErrors := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" || activeCount >= settings.MaxActiveBots {
			continue
		}
		if !isEntryTimingFavorable(candidate) {
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
		// Cooldown check: do not reopen a symbol if it stopped out within 2 hours
		var recentlyStoppedReal bool
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM grid_bots
				WHERE account_id = $1 AND symbol = $2
				  AND status = 'STOPPED'
				  AND closed_reason IN ('STOP_LOSS', 'STOP_LOSS_NATIVE', 'loss_stop')
				  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '2 hours'
			)
		`, *settings.AccountID, candidate.Symbol).Scan(&recentlyStoppedReal); err == nil && recentlyStoppedReal {
			continue
		}
		realLev := candidate.RecommendedLeverage
		if realLev < 2 && settings.Leverage >= 2 {
			realLev = 2
		}
		if err := worker.risk.ValidateNewGrid(
			ctx, *settings.AccountID, candidate.Symbol,
			realLev, settings.BudgetUSDT,
		); err != nil {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: risk gate: %v", candidate.Symbol, err))
			continue
		}
		base, quote, err := SplitPionexPerp(candidate.Symbol)
		if err != nil {
			deployErrors = append(deployErrors, err.Error())
			continue
		}
		data := pionex.BUOrderData{
			Top: candidate.UpperPrice, Bottom: candidate.LowerPrice,
			Row: candidate.GridNum, GridType: mapGridType(settings.DensityGridEnabled),
			Trend:           candidate.RecommendedTrend,
			Leverage:        realLev,
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
		// Native exchange-side take-profit: the per-bot target (dynamic from
		// AI Kit/scanner readings, or the operator's fixed amount) is enforced
		// by Pionex itself (profit_amount), so it survives even if this
		// process is down. The management loop double-checks it locally.
		botTarget, botMaxLoss := computeBotTargets(settings, candidate)
		if botTarget != nil && botTarget.GreaterThan(decimal.Zero) {
			data.ProfitStopType = "profit_amount"
			data.ProfitStop = botTarget
		} else if settings.SmartPNLEnabled && candidate.RecommendedTrend != "no_trend" {
			profit := candidate.UpperPrice
			if candidate.RecommendedTrend == "short" {
				profit = candidate.LowerPrice
			}
			data.ProfitStopType = "price"
			data.ProfitStop = &profit
		}
		params := pionex.NativeFuturesGridCreateParams{
			Base: base, Quote: quote, BUOrderData: data,
		}
		// Native pre-flight validation: Pionex itself confirms the parameters
		// and minimum investment before any capital is committed.
		check, checkErr := client.CheckFuturesGridParams(ctx, params)
		if checkErr != nil {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: native checkParams failed: %v", candidate.Symbol, checkErr))
			continue
		}
		if check.MinInvestment.GreaterThan(decimal.Zero) &&
			settings.BudgetUSDT.LessThan(check.MinInvestment) {
			deployErrors = append(deployErrors, fmt.Sprintf(
				"%s: budget %s below Pionex minimum investment %s",
				candidate.Symbol, settings.BudgetUSDT, check.MinInvestment,
			))
			continue
		}
		if _, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
			AccountID:          *settings.AccountID,
			AutoGridSettingsID: &settings.ID,
			IdempotencyKey:     "autogrid:" + scanID + ":" + candidate.ID,
			Params:             params,
			PnLTargetUSDT:      botTarget,
			MaxLossUSDT:        botMaxLoss,
		}); createErr != nil {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: create failed: %v", candidate.Symbol, createErr))
			continue
		}
		activeCount++
	}
	if len(deployErrors) > 0 {
		worker.logger.Warn(
			"AutoGrid deploy skipped some candidates",
			"component", "autogrid_worker", "skipped", strings.Join(deployErrors, " | "),
		)
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
		SET status = 'STOPPED', closed_reason = 'AUTOGRID_STOP', closed_at = NOW(), updated_at = NOW()
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID); err != nil {
		return fmt.Errorf("stop paper AutoGrid bots: %w", err)
	}
	// Real grids get a durable stop intent; reconcileAndManage submits the
	// native Pionex cancel and verifies the terminal state remotely.
	if _, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED', closed_reason = COALESCE(closed_reason, 'AUTOGRID_STOP'), updated_at = NOW()
		WHERE autogrid_settings_id = $1
		  AND status IN ('RUNNING', 'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN')
	`, settings.ID); err != nil {
		return fmt.Errorf("request real AutoGrid stop: %w", err)
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
		SET status = 'EMERGENCY_STOPPED', closed_reason = 'EMERGENCY_STOP', closed_at = NOW(), updated_at = NOW()
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
	client, err := worker.service.PrivateClient(ctx, worker.accounts, *settings.AccountID)
	if err != nil {
		return err
	}
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
	cancelErrors := make([]string, 0)
	for _, target := range targets {
		if err := worker.cancelRealBot(ctx, client, target.id, target.remoteID, "autogrid emergency stop"); err != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("%s: %v", target.remoteID, err))
		}
	}
	if len(cancelErrors) > 0 {
		return fmt.Errorf("emergency stop cancelled %d/%d bots; failures: %s",
			len(targets)-len(cancelErrors), len(targets), strings.Join(cancelErrors, " | "))
	}
	return nil
}

// cancelRealBot submits the native Pionex cancel for one bot and records the
// submission state. Terminal confirmation happens in reconcileAndManage.
func (worker *Worker) cancelRealBot(
	ctx context.Context, client *pionex.Client, botID, remoteID, note string,
) error {
	_, _ = worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOPPING', reconciliation_state = 'CANCEL_SUBMITTING',
		    updated_at = NOW()
		WHERE id = $1
	`, botID)
	cancelErr := client.CancelFuturesGridBot(ctx, pionex.CancelFuturesGridParams{
		BUOrderID: remoteID, CloseNote: note,
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
		`, botID, state, cancelErr.Error())
		return fmt.Errorf("cancel Pionex grid %s: %w", remoteID, cancelErr)
	}
	_, _ = worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED',
		    reconciliation_state = 'CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING',
		    last_error = NULL, updated_at = NOW()
		WHERE id = $1
	`, botID)
	return nil
}

// reconcileAndManage is the bot supervision loop: it verifies remote state,
// persists PnL, closes bots on PnL targets/stop-outs/range breaks and adjusts
// native grid ranges when the market moves. It returns the durable manage
// interval so the caller can throttle the loop.
// autotuneIfDue re-samples the native AI Kit while RUNNING and nudges the
// whitelisted scanner settings toward the current market distribution.
func (worker *Worker) autotuneIfDue(ctx context.Context, settings Settings) {
	if !settings.AIAutotuneEnabled || !settings.AIKitEnabled {
		return
	}
	if settings.Status != "RUNNING" {
		return
	}
	due := settings.LastAutotuneAt == nil
	if !due {
		elapsed := time.Since(*settings.LastAutotuneAt).Seconds()
		due = elapsed >= float64(settings.AIAutotuneInterval)
	}
	if !due {
		return
	}
	suggestion, err := worker.service.AIKitSettingsFill(ctx, worker.accounts)
	if err != nil {
		worker.logger.Warn("AI autotune sampling failed",
			"component", "autogrid_worker", "error", err)
		return
	}
	if _, changes, err := worker.service.ApplyAutotune(ctx, suggestion.Suggested); err != nil {
		worker.logger.Error("AI autotune apply failed",
			"component", "autogrid_worker", "error", err)
		return
	} else if len(changes) > 0 {
		worker.logger.Info("AI autotune adjusted settings",
			"component", "autogrid_worker", "changes", strings.Join(changes, "; "))
	}
}

func (worker *Worker) reconcileAndManage(ctx context.Context) (int, error) {
	settings, err := worker.service.GetSettings(ctx)
	if err != nil {
		return 0, err
	}
	if err := worker.managePaperBots(ctx, *settings); err != nil {
		worker.logger.Error("manage paper bots", "component", "autogrid_worker", "error", err)
	}
	worker.autotuneIfDue(ctx, *settings)
	if settings.AccountID == nil {
		return clampInterval(settings.ManageIntervalSeconds), nil
	}
	var count int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID).Scan(&count); err != nil || count == 0 {
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	client, err := worker.service.PrivateClient(ctx, worker.accounts, *settings.AccountID)
	if err != nil {
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	priceBySymbol, priceErr := worker.priceMap(ctx)
	if priceErr != nil {
		worker.logger.Warn("fetch tickers for management", "component", "autogrid_worker", "error", priceErr)
	}
	rows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id, status, symbol, direction,
		       lower_price, upper_price, grid_num, adjustments_count,
		       pnl_target_usdt, max_loss_usdt, quote_investment
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		ORDER BY updated_at
	`, settings.ID)
	if err != nil {
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	type managedBot struct {
		id, remoteID, localStatus, symbol, direction string
		lower, upper                                  decimal.Decimal
		rowNum, adjustments                           int
		pnlTarget, maxLoss                            *decimal.Decimal
		investment                                    decimal.Decimal
	}
	bots := make([]managedBot, 0)
	for rows.Next() {
		var item managedBot
		if err := rows.Scan(
			&item.id, &item.remoteID, &item.localStatus, &item.symbol,
			&item.direction, &item.lower, &item.upper, &item.rowNum,
			&item.adjustments, &item.pnlTarget, &item.maxLoss, &item.investment,
		); err != nil {
			rows.Close()
			return clampInterval(settings.ManageIntervalSeconds), err
		}
		bots = append(bots, item)
	}
	rows.Close()

	for _, bot := range bots {
		remote, getErr := client.GetFuturesGridBot(ctx, bot.remoteID)
		if getErr != nil {
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET reconciliation_state = 'REMOTE_READ_FAILED',
				    last_error = $2, last_reconciled_at = NOW(), updated_at = NOW()
				WHERE id = $1
			`, bot.id, getErr.Error())
			continue
		}

		remoteStatus := remote.Status
		if remote.BUOrderData.Status != "" {
			remoteStatus = remote.BUOrderData.Status
		}
		reasonBy := remote.ReasonBy
		if remote.BUOrderData.ReasonBy != "" {
			reasonBy = remote.BUOrderData.ReasonBy
		}
		price := priceBySymbol[bot.symbol]
		realized := remote.BUOrderData.ProfitWithdrawn
		unrealized := decimal.Zero
		if !remote.BUOrderData.Position.IsZero() && price.GreaterThan(decimal.Zero) {
			unrealized = remote.BUOrderData.Position.Mul(price.Sub(remote.BUOrderData.PositionOpenPrice))
		}

		// A durable stop intent (grid.stop / autogrid.stop / manual close)
		// must reach the exchange. Cancel-state machine values survive the
		// remote-truth persist below so failed cancels keep retrying.
		cancelStates := "('CANCEL_SUBMITTING','CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING','CANCEL_FAILED','CANCEL_OUTCOME_UNKNOWN')"

		if terminalRemoteGridStatus(remoteStatus) {
			status, closedReason := terminalOutcome(reasonBy)
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET status = $2, closed_reason = $3,
				    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
				    last_remote_status = $4, realized_pnl_usdt = $5,
				    unrealized_pnl_usdt = 0, closed_at = NOW(),
				    last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
				WHERE id = $1
			`, bot.id, status, closedReason, remoteStatus, realized)
			worker.logger.Info(
				"Pionex grid reached terminal state",
				"component", "autogrid_worker", "symbol", bot.symbol,
				"status", status, "reason", closedReason, "realized_pnl", realized.String(),
			)
			continue
		}

		// Persist remote truth and PnL without reverting durable stop intents:
		// the local status is kept and in-flight cancel states are preserved.
		persistedReconciliation := "REMOTE_TERMINAL_PENDING"
		if bot.localStatus == "RUNNING" {
			persistedReconciliation = "REST_AUTHORITATIVE_OK"
		}
		_, _ = worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = $2,
			    reconciliation_state = CASE
					WHEN reconciliation_state IN `+cancelStates+` THEN reconciliation_state
					ELSE $3
				END,
			    last_remote_status = $4, realized_pnl_usdt = $5,
			    unrealized_pnl_usdt = $6, last_reconciled_at = NOW(),
			    last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, bot.id, bot.localStatus, persistedReconciliation, remoteStatus, realized, unrealized)

		if bot.localStatus != "RUNNING" {
			var reconciliation string
			if err := worker.db.QueryRow(ctx, `
				SELECT COALESCE(reconciliation_state, '') FROM grid_bots WHERE id = $1
			`, bot.id).Scan(&reconciliation); err == nil {
				needsCancel := bot.localStatus == "STOP_REQUESTED" ||
					reconciliation == "CANCEL_SUBMITTING" ||
					reconciliation == "CANCEL_FAILED"
				if needsCancel && reconciliation != "CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING" {
					if err := worker.cancelRealBot(ctx, client, bot.id, bot.remoteID, "autogrid stop"); err != nil {
						worker.logger.Error("submit native cancel", "component", "autogrid_worker", "bot_id", bot.id, "error", err)
					}
				}
			}
			continue
		}
		if price.IsZero() {
			continue
		}
		// Fetch the regime lazily: klines are only needed once price escapes
		// the grid range, otherwise the decision is HOLD anyway.
		regime := ""
		buffer := settings.RangeBreakBufferPct.Div(decimal.NewFromInt(100))
		if price.LessThan(bot.lower.Mul(decimal.NewFromInt(1).Sub(buffer))) ||
			price.GreaterThan(bot.upper.Mul(decimal.NewFromInt(1).Add(buffer))) {
			regime = worker.regimeForSymbol(ctx, bot.symbol)
		}
		botTarget, botMaxLoss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		if bot.pnlTarget != nil {
			botTarget = *bot.pnlTarget
		}
		if bot.maxLoss != nil {
			botMaxLoss = *bot.maxLoss
		}

		total := realized.Add(unrealized)
		peakPnL := total
		if realized.GreaterThan(peakPnL) {
			peakPnL = realized
		}
		if peakPnL.IsNegative() {
			peakPnL = decimal.Zero
		}

		decision := decideBotAction(botActionInput{
			Direction:        bot.direction,
			Lower:            bot.lower,
			Upper:            bot.upper,
			CurrentPrice:     price,
			RealizedPNL:      realized,
			UnrealizedPNL:    unrealized,
			PeakPNL:          peakPnL,
			Budget:           bot.investment,
			PnLTarget:        botTarget,
			MaxLoss:          botMaxLoss,
			RangeBreakBuffer: settings.RangeBreakBufferPct,
			AdjustmentsLeft:  settings.MaxAdjustmentsPerBot - bot.adjustments,
			Regime:           regime,
		})
		switch decision.Action {
		case ActionCloseTakeProfit, ActionCloseStopLoss, ActionCloseRangeBreak:
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET status = 'STOP_REQUESTED', closed_reason = $2, updated_at = NOW()
				WHERE id = $1 AND status = 'RUNNING'
			`, bot.id, decision.Reason)
			if err := worker.cancelRealBot(ctx, client, bot.id, bot.remoteID, "autogrid "+decision.Reason); err != nil {
				worker.logger.Error("close bot by management decision",
					"component", "autogrid_worker", "bot_id", bot.id,
					"reason", decision.Reason, "error", err)
			} else {
				worker.logger.Info("management closed bot",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"reason", decision.Reason, "pnl", realized.Add(unrealized).String())
			}
		case ActionAdjustUp, ActionAdjustDown:
			if err := client.AdjustFuturesGridBot(ctx, pionex.AdjustFuturesGridParams{
				BUOrderID: bot.remoteID, Type: "adjust_params",
				Bottom: decision.NewLower, Top: decision.NewUpper, Row: bot.rowNum,
			}); err != nil {
				worker.logger.Error("adjust native grid range",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			} else {
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET lower_price = $2, upper_price = $3,
					    adjustments_count = adjustments_count + 1, updated_at = NOW()
					WHERE id = $1
				`, bot.id, decision.NewLower, decision.NewUpper)
				worker.logger.Info("adjusted native grid range",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"lower", decision.NewLower.String(), "upper", decision.NewUpper.String())
			}
		}
	}
	return clampInterval(settings.ManageIntervalSeconds), nil
}

// managePaperBots marks paper bots to market and closes them when the same
// PnL rules that govern real bots are hit, so PAPER mode exercises the whole
// lifecycle.
func (worker *Worker) managePaperBots(ctx context.Context, settings Settings) error {
	priceBySymbol, err := worker.priceMap(ctx)
	if err != nil {
		return err
	}
	rows, err := worker.db.Query(ctx, `
		SELECT id, symbol, direction, entry_price, leverage, quote_investment,
		       lower_price, upper_price, pnl_target_usdt, max_loss_usdt,
		       grid_num, last_grid_level, realized_pnl_usdt
		FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID)
	if err != nil {
		return err
	}
	type paperBot struct {
		id, symbol, direction        string
		entry                        decimal.Decimal
		leverage                     int
		investment, lower, upper     decimal.Decimal
		pnlTarget, maxLoss            *decimal.Decimal
		gridNum                      int
		lastLevel                    *int
		realized                     decimal.Decimal
	}
	bots := make([]paperBot, 0)
	for rows.Next() {
		var item paperBot
		if err := rows.Scan(
			&item.id, &item.symbol, &item.direction, &item.entry,
			&item.leverage, &item.investment, &item.lower, &item.upper,
			&item.pnlTarget, &item.maxLoss, &item.gridNum, &item.lastLevel,
			&item.realized,
		); err != nil {
			rows.Close()
			return err
		}
		bots = append(bots, item)
	}
	rows.Close()

	for _, bot := range bots {
		price, ok := priceBySymbol[bot.symbol]
		if !ok || price.IsZero() {
			continue
		}
		realized := bot.realized
		unrealized := decimal.Zero
		currentLevel := 0
		if bot.direction == "NEUTRAL" {
			// Native-grid simulation: each level pair crossed accrues one
			// filled round trip; inventory below the midpoint is marked.
			currentLevel = gridLevelForPrice(bot.lower, bot.upper, bot.gridNum, price)
			previousLevel := currentLevel
			if bot.lastLevel != nil {
				previousLevel = *bot.lastLevel
			}
			var crossingProfit decimal.Decimal
			crossingProfit, unrealized = neutralGridPaperPNL(
				bot.lower, bot.upper, bot.gridNum, bot.investment,
				previousLevel, currentLevel, price, settings.FeeBps,
			)
			realized = realized.Add(crossingProfit)
		} else {
			switch bot.direction {
			case "LONG":
				unrealized = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(price.Div(bot.entry).Sub(decimal.NewFromInt(1)))
			case "SHORT":
				unrealized = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(decimal.NewFromInt(1).Sub(price.Div(bot.entry)))
			}
		}
		botTarget, botMaxLoss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		if bot.pnlTarget != nil {
			botTarget = *bot.pnlTarget
		}
		if bot.maxLoss != nil {
			botMaxLoss = *bot.maxLoss
		}
		total := realized.Add(unrealized)
		peakPnL := total
		if realized.GreaterThan(peakPnL) {
			peakPnL = realized
		}
		if peakPnL.IsNegative() {
			peakPnL = decimal.Zero
		}

		decision := decideBotAction(botActionInput{
			Direction:        bot.direction,
			Lower:            bot.lower,
			Upper:            bot.upper,
			CurrentPrice:     price,
			RealizedPNL:      realized,
			UnrealizedPNL:    unrealized,
			PeakPNL:          peakPnL,
			Budget:           bot.investment,
			PnLTarget:        botTarget,
			MaxLoss:          botMaxLoss,
			RangeBreakBuffer: settings.RangeBreakBufferPct,
			AdjustmentsLeft:  settings.MaxAdjustmentsPerBot,
			Regime:           "",
		})

		if decision.Action == ActionCloseTakeProfit || decision.Action == ActionCloseStopLoss || decision.Action == ActionCloseRangeBreak {
			_, err := worker.db.Exec(ctx, `
				UPDATE paper_grid_bots
				SET status = 'COMPLETED', closed_reason = $2,
				    realized_pnl_usdt = $3, unrealized_pnl_usdt = 0,
				    mark_price = $4, last_grid_level = $5,
				    closed_at = NOW(), updated_at = NOW()
				WHERE id = $1 AND status = 'RUNNING'
			`, bot.id, decision.Reason, total, price, currentLevel)
			if err != nil {
				return fmt.Errorf("close paper bot %s: %w", bot.symbol, err)
			}
			worker.logger.Info("paper bot closed by management",
				"component", "autogrid_worker", "symbol", bot.symbol,
				"reason", decision.Reason, "pnl", total.String())
			continue
		}
		_, _ = worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET mark_price = $2, unrealized_pnl_usdt = $3,
			    realized_pnl_usdt = $4, last_grid_level = $5,
			    updated_at = NOW()
			WHERE id = $1
		`, bot.id, price, unrealized, realized, currentLevel)
	}
	return nil
}

func (worker *Worker) priceMap(ctx context.Context) (map[string]decimal.Decimal, error) {
	tickers, err := worker.publicClient.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, err
	}
	prices := make(map[string]decimal.Decimal, len(tickers))
	for _, ticker := range tickers {
		if ticker.Close.GreaterThan(decimal.Zero) {
			prices[ticker.Symbol] = ticker.Close
		}
	}
	return prices, nil
}

// regimeForSymbol lazily recomputes the market regime for a managed symbol.
func (worker *Worker) regimeForSymbol(ctx context.Context, symbol string) string {
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "60M", 60)
	if err != nil || len(candles) < 30 {
		return ""
	}
	return marketdata.DetectRegime(candles).Regime
}

func clampInterval(seconds int) int {
	if seconds < 15 {
		return 15
	}
	if seconds > 3600 {
		return 3600
	}
	return seconds
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

func SplitPionexPerp(symbol string) (string, string, error) {
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

// terminalOutcome maps the native Pionex reasonBy to our durable lifecycle
// status so closed bots carry an explainable, auditable outcome.
func terminalOutcome(reasonBy string) (string, string) {
	normalized := strings.ToLower(strings.TrimSpace(reasonBy))
	switch {
	case strings.Contains(normalized, "profit_stop"):
		return "COMPLETED", "TAKE_PROFIT_NATIVE"
	case strings.Contains(normalized, "loss_stop"):
		return "STOPPED", "STOP_LOSS_NATIVE"
	case strings.Contains(normalized, "user_cancel"):
		return "STOPPED", "USER_CANCEL"
	case strings.Contains(normalized, "liquidat"):
		return "LIQUIDATED", "LIQUIDATION"
	case strings.Contains(normalized, "not_enough_balance"), strings.Contains(normalized, "create_failed"):
		return "FAILED", "REMOTE_FAILED"
	default:
		return "STOPPED", "EXTERNAL_CLOSE"
	}
}

// enrichAndAuditCandidatesWithLLM executes pre-flight evaluation on ACCEPTED
// candidates through the configured LLM provider (Gemini / Anthropic / OpenRouter).
func (worker *Worker) enrichAndAuditCandidatesWithLLM(
	ctx context.Context,
	settings Settings,
	scanID string,
) error {
	if worker.llm == nil {
		return nil
	}
	llmSettings, err := worker.llm.GetSettings(ctx)
	if err != nil || !llmSettings.Enabled || strings.TrimSpace(llmSettings.APIKey) == "" {
		return nil
	}
	candidates, err := worker.service.listCandidates(ctx, scanID)
	if err != nil {
		return err
	}
	auditedCount := 0
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" || auditedCount >= 5 {
			continue
		}
		candles, err := worker.publicClient.GetKlines(ctx, candidate.Symbol, "15M", 30)
		if err != nil {
			worker.logger.Warn("Failed to fetch klines for LLM candidate audit", "symbol", candidate.Symbol, "error", err)
			continue
		}
		candleSummaries := make([]llm.CandleSummary, 0, len(candles))
		for _, c := range candles {
			o, _ := c.Open.Float64()
			h, _ := c.High.Float64()
			l, _ := c.Low.Float64()
			cl, _ := c.Close.Float64()
			v, _ := c.Volume.Float64()
			candleSummaries = append(candleSummaries, llm.CandleSummary{
				Time:   time.Unix(c.Time/1000, 0).Format("15:04"),
				Open:   o,
				High:   h,
				Low:    l,
				Close:  cl,
				Volume: v,
			})
		}
		curPrice, _ := candidate.CurrentPrice.Float64()
		vol, _ := candidate.VolatilityPct.Float64()
		lowPrice, _ := candidate.LowerPrice.Float64()
		highPrice, _ := candidate.UpperPrice.Float64()
		vol24h, _ := candidate.Volume24h.Float64()

		input := llm.CandidateInput{
			Symbol:              candidate.Symbol,
			CurrentPrice:        curPrice,
			Volume24h:           vol24h,
			VolatilityParkinson: vol,
			RecommendedTrend:    candidate.RecommendedTrend,
			ProposedLowerPrice:  lowPrice,
			ProposedUpperPrice:  highPrice,
			ProposedGridCount:   candidate.GridNum,
			ProposedLeverage:    candidate.RecommendedLeverage,
			RecentCandles15m:    candleSummaries,
		}
		if candidate.ModelAssumptions != nil {
			if v, ok := candidate.ModelAssumptions["adx"].(float64); ok {
				input.ADX = v
			}
			if v, ok := candidate.ModelAssumptions["atrPct"].(float64); ok {
				input.ATRPct = v
			}
			if v, ok := candidate.ModelAssumptions["choppiness"].(float64); ok {
				input.Choppiness = v
			}
			if v, ok := candidate.ModelAssumptions["emaSlopePct"].(float64); ok {
				input.EMASlopePct = v
			}
			if v, ok := candidate.ModelAssumptions["isSqueeze"].(bool); ok {
				input.IsSqueeze = v
			}
		}

		decision, record, err := worker.llm.AuditCandidate(ctx, &candidate.ID, input)
		if err != nil {
			worker.logger.Warn("LLM candidate audit failed", "symbol", candidate.Symbol, "error", err)
			continue
		}
		auditedCount++

		if decision.Decision == "REJECTED" {
			reason := "Отклонено AI-моделью"
			if decision.RejectionReason != nil && *decision.RejectionReason != "" {
				reason = "AI: " + *decision.RejectionReason
			} else if decision.ReasoningSummary != "" {
				reason = "AI: " + decision.ReasoningSummary
			}
			_, _ = worker.db.Exec(ctx, `
				UPDATE autogrid_candidates
				SET decision = 'REJECTED',
				    rejection_reason = $3,
				    model_assumptions = model_assumptions || jsonb_build_object('llmAuditId', $4::TEXT, 'llmConfidence', $5::NUMERIC, 'llmReasoning', $6::TEXT)
				WHERE id = $1 AND scan_id = $2
			`, candidate.ID, scanID, reason, record.ID, decision.Confidence, decision.ReasoningSummary)
			worker.logger.Info("Candidate rejected by LLM intelligence", "symbol", candidate.Symbol, "reason", reason)
		} else {
			_, _ = worker.db.Exec(ctx, `
				UPDATE autogrid_candidates
				SET model_assumptions = model_assumptions || jsonb_build_object('llmAuditId', $3::TEXT, 'llmConfidence', $4::NUMERIC, 'llmReasoning', $5::TEXT, 'llmRegime', $6::TEXT)
				WHERE id = $1 AND scan_id = $2
			`, candidate.ID, scanID, record.ID, decision.Confidence, decision.ReasoningSummary, decision.Regime)
		}
	}
	return nil
}
