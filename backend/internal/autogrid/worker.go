package autogrid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"sort"
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

// trancheFlag converts the deploy-time tranche marker for model_state.
func trancheFlag(on bool) int {
	if on {
		return 1
	}
	return 0
}

// protectiveCloseExemptReasons is the single source of truth for close
// reasons that must NOT arm the circuit breaker or the per-symbol cooldown.
// Every operator-driven stop path must be listed here: CloseAllActiveBots
// writes the status string itself ('STOPPED'/'EMERGENCY_STOPPED' from
// SetStatus, 'AUTOGRID_STOP'/'EMERGENCY_STOP' from the HTTP handlers), and
// manual closes write 'MANUAL_CLOSE'/'MCP_MANUAL_CLOSE'. A missing entry
// turns a routine fleet stop into N "protective closes" that freeze deploys
// for an hour plus 2h per-symbol cooldowns.
const protectiveCloseExemptReasons = `'TAKE_PROFIT', 'TAKE_PROFIT_NATIVE', 'TRAILING_TAKE_PROFIT', 'BREAKEVEN_LOCK',
	'MANUAL_CLOSE', 'MCP_MANUAL_CLOSE', 'USER_CANCEL', 'ALREADY_CLOSED', 'EXTERNAL_CLOSE', 'REMOTE_FAILED',
	'STOPPED', 'EMERGENCY_STOPPED', 'AUTOGRID_STOP', 'EMERGENCY_STOP'`

type Worker struct {
	db           *pgxpool.Pool
	service      *Service
	accounts     *accounts.Service
	risk         *risk.Engine
	scanner      *marketdata.Scanner
	publicClient *pionex.Client
	market       *marketdata.Service
	llm          *llm.Service
	logger       *slog.Logger
	owner        string
	// trancheTBRegime throttles the trend check behind the tranche-2
	// time-box: without it every pending bot older than 24h re-fetches a
	// 60M klines batch per manage tick for as long as the tape trends.
	// The manage loop is single-goroutine, so a plain map is race-free.
	trancheTBRegime map[string]trancheTBTrend
}

type trancheTBTrend struct {
	checkedAt time.Time
	trending  bool
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
		market:          marketdata.NewService(db),
		llm:             llmService,
		logger:          logger,
		owner:           fmt.Sprintf("autogrid-%d", time.Now().UnixNano()),
		trancheTBRegime: make(map[string]trancheTBTrend),
	}
}

func (worker *Worker) Run(ctx context.Context) {
	commandTicker := time.NewTicker(time.Second)
	scheduleTicker := time.NewTicker(10 * time.Second)
	reconcileTicker := time.NewTicker(30 * time.Second)
	defer commandTicker.Stop()
	defer scheduleTicker.Stop()
	defer reconcileTicker.Stop()
	worker.sweepRestartGhosts(ctx)
	worker.logger.Info("AutoGrid worker started", "component", "autogrid_worker")
	for {
		select {
		case <-ctx.Done():
			worker.logger.Info("AutoGrid worker stopped", "component", "autogrid_worker")
			return
		case <-commandTicker.C:
			worker.runGuarded("command", func() {
				if err := worker.processNext(ctx); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					worker.logger.Error("AutoGrid command failed", "component", "autogrid_worker", "error", err)
				}
			})
		case <-scheduleTicker.C:
			worker.runGuarded("schedule", func() {
				if err := worker.scheduleDueScan(ctx); err != nil {
					worker.logger.Error("schedule AutoGrid scan", "component", "autogrid_worker", "error", err)
				}
			})
		case <-reconcileTicker.C:
			worker.runGuarded("reconcile", func() {
				interval, err := worker.reconcileAndManage(ctx)
				if err != nil {
					worker.logger.Error("reconcile and manage Pionex grids", "component", "autogrid_worker", "error", err)
				}
				if seconds := interval; seconds >= 15 && seconds <= 3600 {
					reconcileTicker.Reset(time.Duration(seconds) * time.Second)
				}
			})
		}
	}
}

// sweepRestartGhosts closes what a restart killed mid-flight: scans stuck
// RUNNING and commands stuck EXECUTING from a previous process would
// otherwise lie to the UI forever ("scan in progress") and block scan
// scheduling via the dedup guard.
func (worker *Worker) sweepRestartGhosts(ctx context.Context) {
	tag, err := worker.db.Exec(ctx, `
		UPDATE autogrid_scan_runs
		SET status = 'FAILED', error_message = 'interrupted by backend restart'
		WHERE status = 'RUNNING' AND started_at < NOW() - INTERVAL '2 minutes'
	`)
	if err == nil && tag.RowsAffected() > 0 {
		worker.logger.Warn("marked interrupted scan runs as FAILED",
			"component", "autogrid_worker", "count", tag.RowsAffected())
	}
	tag, err = worker.db.Exec(ctx, `
		UPDATE control_commands
		SET status = 'EXPIRED', lease_owner = NULL, lease_expiry = NULL
		WHERE (status = 'QUEUED' AND created_at < NOW() - INTERVAL '15 minutes')
		   OR (status = 'EXECUTING' AND lease_expiry IS NOT NULL AND lease_expiry < NOW())
	`)
	if err == nil && tag.RowsAffected() > 0 {
		worker.logger.Warn("expired stale control commands",
			"component", "autogrid_worker", "count", tag.RowsAffected())
	}
}

// runGuarded keeps a single poisoned row or unexpected panic from killing
// the whole process: this goroutine also supervises real-money grids.
func (worker *Worker) runGuarded(source string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			worker.logger.Error("AutoGrid worker panic recovered",
				"component", "autogrid_worker", "panic_source", source,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

func (worker *Worker) processNext(ctx context.Context) error {
	command, err := worker.claim(ctx)
	if err != nil {
		return err
	}
	executionCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	result := map[string]any{}
	worker.logger.Info("Executing AutoGrid command", "component", "autogrid_worker", "command_type", command.CommandType, "command_id", command.ID)
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
	// Auto-expire stale commands from crashed processes, old queue items (>15 min), or excessive retries
	_, _ = worker.db.Exec(ctx, `
		UPDATE control_commands
		SET status = 'EXPIRED', updated_at = NOW()
		WHERE (status = 'QUEUED' AND created_at < NOW() - INTERVAL '15 minutes')
		   OR (status = 'EXECUTING' AND lease_expiry IS NOT NULL AND lease_expiry < NOW())
		   OR (attempts >= 5 AND status IN ('QUEUED', 'EXECUTING'))
	`)
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
		    lease_expiry = NOW() + INTERVAL '15 minutes',
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
	// Instantly set status to RUNNING so the UI never hangs in STARTING
	if err := worker.service.SetStatus(ctx, "RUNNING", nil); err != nil {
		return err
	}
	// Run scan and deploy in background without blocking the state transition
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if _, err := worker.scanAndDeploy(bgCtx, command); err != nil {
			worker.logger.Error("initial AutoGrid scanAndDeploy failed", "component", "autogrid_worker", "error", err)
		}
	}()
	return nil
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
	worker.hydrateCandidatesFunding(ctx, candidates)
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
	// Always reload live settings right before deployment so fresh status and parameters are used
	if live, getErr := worker.service.GetSettings(ctx); getErr == nil && live != nil {
		settings = live
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

// hydrateCandidatesFunding stamps the latest cross-exchange funding rate on
// every scanned candidate in one batched query, so the UI column, the
// persisted audit trail and the deploy gates all see the same number.
// Symbols without collector coverage (Pionex-exclusive listings) keep a nil
// rate — the funding column stays empty for them.
func (worker *Worker) hydrateCandidatesFunding(ctx context.Context, candidates []marketdata.ScannerCandidate) {
	if len(candidates) == 0 || worker.market == nil {
		return
	}
	symbols := make([]string, 0, len(candidates))
	for _, c := range candidates {
		symbols = append(symbols, c.Symbol)
	}
	funding, err := worker.market.GetCurrentFundingBatch(ctx, symbols)
	if err != nil {
		worker.logger.Warn("funding hydration skipped",
			"component", "autogrid_worker", "error", err)
		return
	}
	hydrated := 0
	for i := range candidates {
		info := funding[candidates[i].Symbol]
		if info == nil {
			continue
		}
		rate := decimal.NewFromFloat(info.AverageRate)
		candidates[i].FundingRate = &rate
		if candidates[i].ModelAssumptions != nil {
			candidates[i].ModelAssumptions["fundingIncluded"] = true
			candidates[i].ModelAssumptions["fundingExtreme"] = info.IsExtreme
		}
		hydrated++
	}
	if hydrated > 0 {
		worker.logger.Info("funding hydrated on candidates",
			"component", "autogrid_worker", "hydrated", hydrated, "total", len(candidates))
	}
}

// noteDeployBlock persists a deployment-freeze reason into
// autogrid_settings.last_error so a silent gate (economic event, cascade,
// circuit breaker) is visible in the UI instead of living only in docker
// logs — the GBP-CPI incident class froze deployments for hours with no
// durable trace.
func (worker *Worker) noteDeployBlock(ctx context.Context, reason string) {
	_, _ = worker.db.Exec(ctx, `
		UPDATE autogrid_settings SET last_error = $1, updated_at = NOW()
	`, reason)
}

// confluenceConfidence maps the confluence engine's strength (0..1) onto a
// 0.5..1.0 confidence scale for the direction selector; absent confluence
// data degrades to the conservative 0.6 default.
func confluenceConfidence(assumptions map[string]any) float64 {
	if c, ok := assumptions["confluence"].(map[string]any); ok {
		if s, ok := c["strength"].(float64); ok && s > 0 {
			conf := 0.5 + s*0.5
			if conf > 1 {
				conf = 1
			}
			return conf
		}
	}
	return 0.6
}

// candidateHurst extracts the scanner's real Hurst exponent from the
// candidate assumptions; 0.5 (random walk) is the neutral fallback.
func candidateHurst(assumptions map[string]any) float64 {
	if v, ok := assumptions["hurst"].(float64); ok {
		return v
	}
	return 0.5
}

// candidateRegime extracts the detected regime with a safe fallback.
func candidateRegime(assumptions map[string]any) string {
	if v, ok := assumptions["regime"].(string); ok && v != "" {
		return v
	}
	return "RANGE"
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
		spotBase := strings.TrimSuffix(strings.TrimSuffix(base, ".PERP"), "_PERP")
		strategy, err := client.GetSpotGridAIStrategy(ctx, spotBase, quote)
		if err != nil {
			worker.logger.Debug(
				"Pionex AI Kit not available for symbol (futures-only or no spot AI)",
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
				"annualized":      strategy.Annualized,
				"volatility":      strategy.Volatility,
				"maxDrawDown":     strategy.MaxDrawDown,
				"spotHigh":        strategy.High,
				"spotLow":         strategy.Low,
				"gridCount":       strategy.GridCount,
				"gridCountSource": "pionex_ai_kit",
				"boundary":        "AI_GRID_COUNT_ADOPTED_WITH_CLAMP_2_500_RANGE_STAYS_SCANNER_SR",
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
// favorably within the channel structure before launching a grid:
// - NEUTRAL: must be within the core median channel (35% to 65%) to avoid boundary traps.
// - LONG: must be in the lower pullback accumulation zone (15% to 45%).
// - SHORT: must be in the upper relief rebound zone (55% to 85%).
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
		// Accumulation / Dip buying: enter from lower bounce (10%) up to channel median (65%)
		return rangePos >= 10.0 && rangePos <= 65.0
	case "short":
		// Distribution / Rip selling: enter from channel median (35%) up to upper resistance (90%)
		return rangePos >= 35.0 && rangePos <= 90.0
	default:
		// Neutral range: trade healthy channel (20% to 80%), avoiding extreme 20% boundary traps
		return rangePos >= 20.0 && rangePos <= 80.0
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
	// Portfolio Circuit Breaker: >= 3 protective closes in the last 1 hour
	// pauses new deployments. Every loss exit counts — stop-loss, structural
	// invalidation, range break, liquidation; only profit takes and
	// operator/exchange-driven closes are excluded.
	var recentStopLossCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM paper_grid_bots
			WHERE settings_id = $1
			  AND status = 'COMPLETED'
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND closed_at > NOW() - INTERVAL '1 hour'
			UNION ALL
			SELECT 1 FROM grid_bots
			WHERE status IN ('STOPPED', 'LIQUIDATED')
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '1 hour'
		) recent_stops
	`, settings.ID).Scan(&recentStopLossCount); err == nil && recentStopLossCount >= 3 {
		worker.logger.Warn("Portfolio circuit breaker: recent stop-losses holding new deployments", "recentStopLossCount", recentStopLossCount)
		worker.noteDeployBlock(ctx, fmt.Sprintf("circuit breaker: %d защитных закрытий за последний час — новые деплои на паузе", recentStopLossCount))
		return nil
	}
	// Smart Grid Engine v2.0 macro gates — PAPER runs the same exam as REAL
	// capital, otherwise paper statistics prove a pipeline REAL would have
	// blocked. High-impact economic events ahead or an ongoing liquidation
	// cascade pause all entries.
	blocked, blockReason := worker.CheckEconomicEvents(ctx, 2)
	if blocked {
		worker.logger.Warn("paper deploy blocked by economic event",
			"component", "autogrid_worker", "reason", blockReason)
		worker.noteDeployBlock(ctx, "деплой заблокирован: макро-событие USD «"+blockReason+"» (окно T−2ч…T+1ч)")
		return nil
	}
	if cascade, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000); cascade {
		worker.logger.Warn("paper deploy blocked by liquidation cascade",
			"component", "autogrid_worker", "usd_1h", cascadeUSD)
		worker.noteDeployBlock(ctx, fmt.Sprintf("деплой заблокирован: каскад ликвидаций $%.0fM/час", cascadeUSD/1_000_000))
		return nil
	}
	fng, _ := worker.GetFearGreed(ctx)
	backtestGateOn := worker.backtestGateEnabled(ctx)
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		if !isEntryTimingFavorable(candidate) {
			continue
		}
		atrPct := 2.0
		if val, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && val > 0 {
			atrPct = val
		}
		regime := "RANGE"
		if val, ok := candidate.ModelAssumptions["regime"].(string); ok && val != "" {
			regime = val
		}

		// v2.0.3 Smart Direction (paper): regime + cross-exchange funding +
		// sentiment choose the direction and its leverage instead of the
		// scanner's default neutral. Symbols without funding coverage
		// (Pionex-exclusive listings) fall back to the scanner decision.
		smartTrend, smartLev, smartReason := "", 0, ""
		if fundingCtx, fundErr := worker.GetFundingForSymbol(ctx, candidate.Symbol); fundErr == nil {
			smart := SelectDirection(
				RegimeContext{
					Regime:     regime,
					Confidence: confluenceConfidence(candidate.ModelAssumptions),
					HurstValue: candidateHurst(candidate.ModelAssumptions),
				},
				FundingContext{
					AverageRate: fundingCtx.AverageRate,
					IsExtreme:   fundingCtx.IsExtreme,
				},
				EventContext{FearGreedExtreme: fng},
			)
			if smart.Direction == "WAIT" || smart.Direction == "CLOSE_ALL" {
				worker.logger.Info("v2.0 smart direction: skip",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"reason", smart.Reason)
				continue
			}
			smartTrend = strings.ToLower(smart.Direction)
			smartLev = smart.Leverage
			smartReason = smart.Reason
		}

		// Entry gate (v2.0.12): funding extreme + falling OI = forced
		// deleveraging in progress. This is the CAUSE of falling knives —
		// RSI/ADX/Hurst all lag a fresh dump. Block only while the flush
		// runs; once OI stabilizes the gate lifts on its own.
		if blocked, why := worker.fundingFlushBlocked(ctx, candidate.Symbol, candidate.FundingRate); blocked {
			worker.logger.Info("entry gate: funding flush in progress, skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", why)
			continue
		}

		// Supervision mark first: a symbol with a RUNNING bot (or a full
		// portfolio) needs no geometry work — skipping the candle fetch here
		// saves the shared Pionex rate budget every scan.
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
		// Cooldown: do not reopen a symbol within 2 hours of ANY protective
		// close (stop-loss, structural invalidation, range break) — only
		// take-profit exits may redeploy immediately.
		var recentlyStopped bool
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM paper_grid_bots
				WHERE settings_id = $1 AND symbol = $2
				  AND status = 'COMPLETED'
				  AND COALESCE(closed_reason, '') NOT IN (
				      `+protectiveCloseExemptReasons+`)
				  AND closed_at > NOW() - INTERVAL '2 hours'
			)
		`, settings.ID, candidate.Symbol).Scan(&recentlyStopped); err == nil && recentlyStopped {
			continue
		}
		// Entry gate (v2.0.12): anchor to the LIVE price. The scan price is
		// captured at scan start and ages through AI Kit calls, LLM audits
		// and backtest waits; deploying a grid centered minutes in the past
		// is how bots open already outside their range (ENSO class). A drift
		// beyond half an ATR means the candidate itself is stale.
		if freshPrice, ok := worker.revalidateFreshPrice(ctx, &candidate, atrPct); ok {
			candidate.CurrentPrice = freshPrice
		} else {
			worker.logger.Info("entry gate: stale candidate price, skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"scan_price", candidate.CurrentPrice.String(), "live_price", freshPrice.String())
			continue
		}
		// Walk-forward backtest gate — PAPER runs the same exam as REAL
		// capital: otherwise paper statistics prove a pipeline that REAL
		// would have gated (prod: TUT walked into paper while its 30M
		// walk-forward showed OOS −24.5% / DD 81%). Missing jobs are
		// auto-enqueued and awaited in-cycle; the candidate is reconsidered
		// on the next scan if results are still missing.
		if backtestGateOn {
			verdict := worker.backtestGate(ctx, settings, candidate.Symbol)
			if verdict.Pending {
				worker.logger.Info("backtest gate: awaiting walk-forward results",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
			}
			if !verdict.Allowed {
				worker.rejectCandidate(ctx, candidate, verdict.Reason, map[string]any{
					"backtestGate": map[string]any{
						"allowed": verdict.Allowed, "reason": verdict.Reason,
						"traded": verdict.Traded, "neighbors": verdict.Neighbors,
					},
				})
				worker.logger.Info("backtest gate rejected candidate",
					"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", verdict.Reason)
				continue
			}
		}

		// v2.0 HAR-RV geometry: forecast next-day volatility from daily
		// candles and derive range width / level count / vol-inverse
		// leverage. Falls back to the ATR adaptive mesh when history or fit
		// quality is insufficient — the paper fleet then still validates the
		// exact pipeline REAL runs.
		// v2.0.13 tranches: commit HALF the budget up front; the manage loop
		// tops up after a confirmed adverse excursion or the 24h time-box.
		// Knife inventory drag is quadratic in depth, so the initial half
		// halves the damage of every un-timed entry. Geometry (levels, $5
		// per-level floor) must size against the actually-committed amount.
		investAmount := settings.BudgetUSDT
		trancheOn := settings.TrancheDeployEnabled
		if trancheOn {
			investAmount = settings.BudgetUSDT.Div(decimal.NewFromInt(2))
		}
		harGeo := worker.harGridGeometry(ctx, candidate.Symbol, decimalFloat(settings.FeeBps.Add(settings.SlippageBps)), investAmount.InexactFloat64())
		mesh := ComputeAdaptiveMesh(
			candidate.LowerPrice, candidate.UpperPrice, candidate.CurrentPrice,
			atrPct, regime, investAmount, settings.Leverage, 0.30,
		)
		// Entry gate (v2.0.12): block deployment into an active volatility
		// expansion — a fixed-step grid holds per-pair edge constant while
		// inventory risk grows with sigma² (A–S penalty). Since v2.0.13 the
		// gate covers HAR-less symbols too (new listings) via a 24h
		// self-baseline.
		forecastPct := 0.0
		if harGeo != nil {
			forecastPct = harGeo.forecastPct
		}
		if blocked, ratio := worker.volExpansionBlocked(ctx, candidate.Symbol, forecastPct); blocked {
			worker.logger.Info("entry gate: volatility expansion, skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"rv_ref_ratio", math.Round(ratio*100)/100)
			continue
		}
		if harGeo != nil {
			harGeo.applyToMesh(candidate.CurrentPrice, &mesh)
		}

		trend := strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))
		if trend == "no_trend" || trend == "" {
			trend = "neutral"
		}
		if smartTrend != "" && smartTrend != trend {
			worker.logger.Info("v2.0 smart direction override",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"scanner_trend", trend, "smart_direction", smartTrend,
				"reason", smartReason)
			trend = smartTrend
		}
		atrPrice := candidate.CurrentPrice.Mul(decimal.NewFromFloat(atrPct / 100.0))
		antiHuntStop := ComputeAntiHuntStop(
			trend, mesh.LowerPrice, mesh.UpperPrice,
			candidate.CurrentPrice, atrPrice, 1.5,
		)

		// Pre-deploy distance check — same exam as REAL (v2.0.8 parity fix):
		// deploying with price hugging the anti-hunt stop means an instant
		// STRUCT_INVALID close, whose protective close then feeds the circuit
		// breaker and cooldowns — paper used to manufacture its own
		// deployment freezes (prod: ENSO same-second exit, REAL-side fix
		// v1.3.14; paper lagged).
		if trend != "short" {
			if candidate.CurrentPrice.Sub(antiHuntStop).LessThan(atrPrice.Mul(decimal.NewFromFloat(1.5))) {
				worker.logger.Info("skip paper deploy: price too close to anti-hunt stop",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"price", candidate.CurrentPrice.String(), "stop", antiHuntStop.String())
				continue
			}
		} else {
			if antiHuntStop.Sub(candidate.CurrentPrice).LessThan(atrPrice.Mul(decimal.NewFromFloat(1.5))) {
				worker.logger.Info("skip paper deploy: price too close to anti-hunt stop",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"price", candidate.CurrentPrice.String(), "stop", antiHuntStop.String())
				continue
			}
		}

		// Leverage precedence: HAR vol-inverse cap, then Smart Direction's
		// conservative cap — either may only scale DOWN from the operator's
		// setting. Without HAR/smart inputs the ATR adaptive ladder applies.
		botLev := settings.Leverage
		levReason := fmt.Sprintf("Фиксированное (%dx)", settings.Leverage)
		levMode := "FIXED"
		if harGeo != nil && harGeo.geo.Leverage < botLev {
			// HAR may only scale DOWN from the operator's setting (same rule
			// as deployReal; v2.0.8 fix — it used to REPLACE the setting,
			// letting paper run 3x where REAL would cap to the operator's 2x).
			botLev = harGeo.geo.Leverage
			levReason = fmt.Sprintf("HAR σ=%.0f%%/год R²=%.2f (%dx)",
				harGeo.forecastPct, harGeo.geo.Confidence, botLev)
			levMode = "HAR"
		}
		if smartLev > 0 && smartLev < botLev {
			botLev = smartLev
			levReason = fmt.Sprintf("Smart Direction (%dx): %s", smartLev, smartReason)
			levMode = "SMART"
		}
		if harGeo == nil && smartLev == 0 && settings.AdaptiveLeverageEnabled {
			dyn := ComputeDynamicLeverage(atrPct, settings.Leverage)
			botLev = dyn.Leverage
			levReason = dyn.Reason
			levMode = "ADAPTIVE"
		}

		confluence := EvaluateConfluence(candidate, nil, nil)

		gridType := mesh.GridType
		target, maxLoss := computeBotTargets(settings, candidate)
		if trancheOn {
			// TP/SL govern the bot as deployed: half capital → half target
			// and half max loss for tranche 1 (tranche 2's top-up doubles
			// them back on the manage side).
			if target != nil {
				half := target.Div(decimal.NewFromInt(2))
				target = &half
			}
			if maxLoss != nil {
				half := maxLoss.Div(decimal.NewFromInt(2))
				maxLoss = &half
			}
		}

		var botID string
		var botNumber int
		err = worker.db.QueryRow(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, candidate_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, model_state,
				pnl_target_usdt, max_loss_usdt,
				grid_step_pct, confluence_score, anti_hunt_stop_price
			) VALUES (
				$1, $2, $3, 'RUNNING', $4, $5, $6, $7, $8, $9, $10, $11, $11,
				jsonb_build_object(
					'model', 'adaptive_confluence_mesh_v2',
					'gridFillsSimulated', false,
					'pnlTargetSource', $12::TEXT,
					'confluenceStatus', $15::TEXT,
					'leverageReason', $19::TEXT,
					'leverageMode', $20::TEXT,
					'baseLeverage', $21::INT,
					'trancheDeployed', $22::INT,
					'trancheBase', $23::TEXT,
					'atrPctEntry', $24::FLOAT8,
					'warning', 'paper PnL is not a native Pionex grid backtest'
				),
				$13, $14,
				$16, $17, $18
			)
			ON CONFLICT (settings_id, symbol) WHERE status = 'RUNNING'
			DO NOTHING
			RETURNING id, bot_number
	`, settings.ID, candidate.ID, candidate.Symbol,
			databaseTrend(trend), gridType,
			mesh.LowerPrice, mesh.UpperPrice, mesh.GridNum,
			botLev, investAmount,
			candidate.CurrentPrice, settings.PnLTargetMode, target, maxLoss,
			confluence.Status, mesh.GridStepPct, confluence.Score, antiHuntStop,
			levReason, levMode, settings.Leverage,
			trancheFlag(trancheOn), settings.BudgetUSDT.String(), atrPct).Scan(&botID, &botNumber)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return fmt.Errorf("deploy paper grid %s: %w", candidate.Symbol, err)
		}
		activeCount++

		_ = LogBotEvent(ctx, worker.db, botID, botNumber, "PAPER", candidate.Symbol, "CREATED", &candidate.CurrentPrice, nil, map[string]any{
			"leverage": botLev, "gridNum": mesh.GridNum, "lowerPrice": mesh.LowerPrice, "upperPrice": mesh.UpperPrice, "budget": settings.BudgetUSDT,
		})
		_ = QueueTelegramEvent(ctx, worker.db, "BOT_CREATED", map[string]any{
			"bot_number": botNumber, "symbol": candidate.Symbol, "direction": databaseTrend(trend),
			"leverage": botLev, "lower_price": mesh.LowerPrice, "upper_price": mesh.UpperPrice,
			"grid_num": mesh.GridNum, "quote_investment": settings.BudgetUSDT,
		})
	}
	// A successful paper deploy round clears a stale deploy-block note —
	// without this the circuit-breaker message from a past close wave sits
	// in the UI for hours while the fleet is live and farming (prod:
	// 19:10Z breaker note still displayed at 03:00Z with 10 bots RUNNING).
	if activeCount > 0 {
		_, _ = worker.db.Exec(ctx, `
			UPDATE autogrid_settings SET last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, settings.ID)
	}
	return nil
}

// revalidateCandidateTrend recomputes the market regime from fresh candles
// immediately before real capital is committed. A full scan takes minutes,
// so a direction decided at scan start can be stale — or outright wrong —
// by deploy time. The planned trend must survive a fresh look.
func (worker *Worker) revalidateCandidateTrend(
	ctx context.Context,
	candidate *Candidate,
	settings Settings,
) (bool, string) {
	candles, err := worker.publicClient.GetKlines(ctx, candidate.Symbol, settings.CandleInterval, settings.LookbackCandles)
	if err != nil || len(candles) < 30 {
		return false, "fresh candle fetch failed; refusing to deploy on stale data"
	}
	regime := marketdata.DetectRegime(candles)
	freshTrend := regime.RecommendedTrend()

	atrPct := 2.0
	if val, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && val > 0 {
		atrPct = val
	}
	tickers, tickerErr := worker.publicClient.GetTickers(ctx, candidate.Symbol, "PERP")
	if tickerErr == nil && len(tickers) > 0 && tickers[0].Open.GreaterThan(decimal.Zero) {
		// v2.0.12: the trend was re-checked — now also the PRICE. The scan
		// price ages through the whole enrichment pipeline; geometry, entry
		// and the anti-hunt stop must anchor to the live price, and a drift
		// beyond half an ATR voids the candidate itself.
		if fresh, ok := worker.revalidateFreshPrice(ctx, candidate, atrPct); ok {
			candidate.CurrentPrice = fresh
		} else {
			return false, fmt.Sprintf("price drifted beyond 0.5 ATR since scan (%s → %s)",
				candidate.CurrentPrice.StringFixed(6), fresh.StringFixed(6))
		}
		change24h, _ := tickers[0].Close.Sub(tickers[0].Open).
			Div(tickers[0].Open).Mul(decimal.NewFromInt(100)).Float64()
		if change24h >= 3.0 && freshTrend == "short" {
			freshTrend = "no_trend"
		} else if change24h <= -3.0 && freshTrend == "long" {
			freshTrend = "no_trend"
		}
		if freshTrend == "no_trend" && (math.Abs(change24h) > 8.0) {
			return false, fmt.Sprintf("fresh 24h change %+.1f%% too strong for a neutral grid", change24h)
		}
	}

	planned := strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))
	if planned == "" {
		planned = "no_trend"
	}
	if planned != freshTrend {
		return false, fmt.Sprintf("trend changed since scan: planned %s, fresh %s", planned, freshTrend)
	}
	if freshTrend == "no_trend" && (regime.ADX > 32.0 || math.Abs(regime.EMASlopePct) > 3.0) {
		return false, fmt.Sprintf("fresh regime too strong for neutral grid (ADX %.1f, EMA slope %.2f%%)", regime.ADX, regime.EMASlopePct)
	}
	return true, freshTrend
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
		// Pin the resolved account so reconcile/stop flows and future deploys
		// cannot diverge from the account the bots were created under.
		_, _ = worker.db.Exec(ctx, `
			UPDATE autogrid_settings
			SET account_id = $2, updated_at = NOW()
			WHERE id = $1 AND account_id IS NULL
		`, settings.ID, *resolved)
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
	// Portfolio Circuit Breaker (REAL): >= 3 protective closes in the last
	// hour pauses deployments. Counts every loss exit — stop-loss, structural
	// invalidation, range break, liquidation (the manage loop writes decision
	// reasons as closed_reason, so an IN-list silently missed them; the paper
	// breaker was fixed in v1.3.16, this one lagged). Profit takes and
	// operator/exchange-driven closes are exempt.
	var recentStopLossCountReal int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE status IN ('STOPPED', 'LIQUIDATED')
		  AND COALESCE(closed_reason, '') NOT IN (
		      `+protectiveCloseExemptReasons+`)
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '1 hour'
	`).Scan(&recentStopLossCountReal); err == nil && recentStopLossCountReal >= 3 {
		worker.logger.Warn("Portfolio circuit breaker: recent real stop-losses holding new deployments", "recentStopLossCountReal", recentStopLossCountReal)
		worker.noteDeployBlock(ctx, fmt.Sprintf("REAL circuit breaker: %d защитных закрытий за последний час — деплои на паузе", recentStopLossCountReal))
		return nil
	}
	deployErrors := make([]string, 0)
	backtestGateOn := worker.backtestGateEnabled(ctx)

	// Smart Grid Engine v2.0: Smart direction selection + economic gates.
	// Replaces the old "always use scanner trend" with regime-aware choice:
	// TREND_DOWN + positive funding → SHORT, TREND_UP + negative funding → LONG,
	// RANGE + high confidence → NEUTRAL at 2x. Economic events and liquidation
	// cascades block all entries.
	blocked, blockReason := worker.CheckEconomicEvents(ctx, 2)
	if blocked {
		worker.logger.Warn("deploy blocked by economic event", "component", "autogrid_worker", "reason", blockReason)
		worker.noteDeployBlock(ctx, "REAL деплой заблокирован: макро-событие USD «"+blockReason+"» (окно T−2ч…T+1ч)")
		return nil
	}
	cascade, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000)
	if cascade {
		worker.logger.Warn("deploy blocked by liquidation cascade", "component", "autogrid_worker", "usd_1h", cascadeUSD)
		worker.noteDeployBlock(ctx, fmt.Sprintf("REAL деплой заблокирован: каскад ликвидаций $%.0fM/час", cascadeUSD/1_000_000))
		return nil
	}
	// When the LLM brain is enabled, an UNAUDITED candidate is not
	// deployable — regardless of why the audit is missing (beyond the
	// per-scan audit cap, transport failure, timeout). This is the hard
	// structural guarantee that closes the top-5 bypass.
	llmBrainEnabled := false
	if llmSettings, err := worker.llm.GetSettings(ctx); err == nil {
		llmBrainEnabled = llmSettings.Enabled && strings.TrimSpace(llmSettings.APIKey) != ""
	}
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" || activeCount >= settings.MaxActiveBots {
			continue
		}
		if !isEntryTimingFavorable(candidate) {
			continue
		}
		if llmBrainEnabled && candidate.ModelAssumptions["llmAuditId"] == nil {
			worker.logger.Warn("skip real deploy: no completed LLM audit for candidate",
				"component", "autogrid_worker", "symbol", candidate.Symbol)
			worker.rejectCandidate(ctx, candidate, "AI-аудит не завершён для этого кандидата (кап аудита/сбой LLM) — деплой заблокирован", nil)
			continue
		}
		// Entry gate (v2.0.12): funding extreme + falling OI = forced
		// deleveraging in progress (mirror of the paper path).
		if blocked, why := worker.fundingFlushBlocked(ctx, candidate.Symbol, candidate.FundingRate); blocked {
			worker.logger.Info("entry gate: funding flush in progress, skip real deploy",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", why)
			continue
		}
		if ok, reason := worker.revalidateCandidateTrend(ctx, &candidate, settings); !ok {
			worker.logger.Info("skip real deploy after fresh trend revalidation",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", reason)
			continue
		}

		// v2.0 Smart Direction: regime + funding + FNG → direction override.
		// v2.0.3: feeds the scanner's REAL Hurst (was hardcoded 0.5, which
		// dead-locked RANGE candidates into WAIT), reads the regime safely,
		// and the decision now actually overrides trend + leverage below.
		smartTrend, smartLev, smartReason := "", 0, ""
		if fundingCtx, fundErr := worker.GetFundingForSymbol(ctx, candidate.Symbol); fundErr == nil {
			fng, _ := worker.GetFearGreed(ctx)
			smartDir := SelectDirection(
				RegimeContext{
					Regime:     candidateRegime(candidate.ModelAssumptions),
					Confidence: confluenceConfidence(candidate.ModelAssumptions),
					HurstValue: candidateHurst(candidate.ModelAssumptions),
				},
				FundingContext{
					AverageRate: fundingCtx.AverageRate,
					IsExtreme:   fundingCtx.IsExtreme,
				},
				EventContext{FearGreedExtreme: fng},
			)
			if smartDir.Direction == "WAIT" || smartDir.Direction == "CLOSE_ALL" {
				worker.logger.Info("v2.0 smart direction: skip",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"reason", smartDir.Reason)
				continue
			}
			smartTrend = strings.ToLower(smartDir.Direction)
			smartLev = smartDir.Leverage
			smartReason = smartDir.Reason
		}
		// Walk-forward backtest gate: the traded TF must have earned a fresh
		// non-negative OOS verdict with bounded drawdown, and no neighbor TF
		// may be catastrophic. Missing jobs are auto-enqueued; the candidate
		// is reconsidered on the next scan once results arrive.
		if backtestGateOn {
			verdict := worker.backtestGate(ctx, settings, candidate.Symbol)
			if verdict.Pending {
				worker.logger.Info("backtest gate: awaiting walk-forward results",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
			}
			if !verdict.Allowed {
				worker.rejectCandidate(ctx, candidate, verdict.Reason, map[string]any{
					"backtestGate": map[string]any{
						"allowed": verdict.Allowed, "reason": verdict.Reason,
						"traded": verdict.Traded, "neighbors": verdict.Neighbors,
					},
				})
				worker.logger.Info("backtest gate rejected candidate",
					"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", verdict.Reason)
				continue
			}
			worker.logger.Info("backtest gate passed",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"reason", verdict.Reason, "potential_pct", verdict.PotentialPct)
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
		// Cooldown: do not reopen a symbol within 2 hours of ANY protective
		// close (stop-loss, structural invalidation, range break, liquidation);
		// profit takes and operator/exchange-driven closes are exempt.
		var recentlyStoppedReal bool
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM grid_bots
				WHERE account_id = $1 AND symbol = $2
				  AND status IN ('STOPPED', 'LIQUIDATED')
				  AND COALESCE(closed_reason, '') NOT IN (
				      `+protectiveCloseExemptReasons+`)
				  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '2 hours'
			)
		`, *settings.AccountID, candidate.Symbol).Scan(&recentlyStoppedReal); err == nil && recentlyStoppedReal {
			continue
		}
		atrPct := 2.0
		if val, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && val > 0 {
			atrPct = val
		}
		regime := "RANGE"
		if val, ok := candidate.ModelAssumptions["regime"].(string); ok && val != "" {
			regime = val
		}

		// v2.0.13 tranches: REAL commits HALF the budget at create; the
		// manage loop tops up via the native invest_in endpoint after a
		// confirmed adverse excursion or the 24h time-box (paper mirror).
		investAmount := settings.BudgetUSDT
		if settings.TrancheDeployEnabled {
			investAmount = settings.BudgetUSDT.Div(decimal.NewFromInt(2))
		}
		mesh := ComputeAdaptiveMesh(
			candidate.LowerPrice, candidate.UpperPrice, candidate.CurrentPrice,
			atrPct, regime, investAmount, settings.Leverage, 0.30,
		)

		// v2.0 HAR-RV geometry — the same sizing the paper fleet validates:
		// forecast next-day volatility from daily candles, derive range width
		// / level count / vol-inverse leverage. Falls back to the ATR mesh
		// when history or fit quality is insufficient.
		harGeo := worker.harGridGeometry(ctx, candidate.Symbol, decimalFloat(settings.FeeBps.Add(settings.SlippageBps)), investAmount.InexactFloat64())
		// Entry gate (v2.0.12, v2.0.13): volatility expansion block, with the
		// 24h self-baseline fallback covering HAR-less new listings.
		forecastPct := 0.0
		if harGeo != nil {
			forecastPct = harGeo.forecastPct
		}
		if blocked, ratio := worker.volExpansionBlocked(ctx, candidate.Symbol, forecastPct); blocked {
			worker.logger.Info("entry gate: volatility expansion, skip real deploy",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"rv_ref_ratio", math.Round(ratio*100)/100)
			continue
		}
		if harGeo != nil {
			harGeo.applyToMesh(candidate.CurrentPrice, &mesh)
			worker.logger.Info("v2.0 har geometry applied",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"forecast_vol_pct", harGeo.forecastPct, "r2", harGeo.geo.Confidence,
				"range_pct", harGeo.geo.RangePct, "grid_num", mesh.GridNum,
				"leverage_cap", harGeo.geo.Leverage)
		}

		pricePrecision := 6
		if p, ok := candidate.ModelAssumptions["pricePrecision"].(float64); ok && p > 0 {
			pricePrecision = int(p)
		} else if pInt, ok := candidate.ModelAssumptions["pricePrecision"].(int); ok && pInt > 0 {
			pricePrecision = pInt
		} else if candidate.CurrentPrice.GreaterThan(decimal.Zero) {
			exp := candidate.CurrentPrice.Exponent()
			if exp < 0 {
				pricePrecision = int(-exp)
			}
		}
		if pricePrecision > 8 {
			pricePrecision = 8
		}

		lowerPrice := mesh.LowerPrice.Round(int32(pricePrecision))
		upperPrice := mesh.UpperPrice.Round(int32(pricePrecision))
		if upperPrice.LessThanOrEqual(lowerPrice) {
			minStep := decimal.New(1, -int32(pricePrecision))
			upperPrice = lowerPrice.Add(minStep.Mul(decimal.NewFromInt(int64(mesh.GridNum))))
		}

		trend := strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))
		if trend == "neutral" || trend == "" {
			trend = "no_trend"
		}
		if smartTrend != "" {
			smartParam := smartTrend
			if smartParam == "neutral" {
				smartParam = "no_trend"
			}
			if smartParam != trend {
				worker.logger.Info("v2.0 smart direction override",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"scanner_trend", trend, "smart_direction", smartTrend,
					"reason", smartReason)
			}
			trend = smartParam
		}

		atrPrice := candidate.CurrentPrice.Mul(decimal.NewFromFloat(atrPct / 100.0))
		antiHuntStop := ComputeAntiHuntStop(
			trend, lowerPrice, upperPrice,
			candidate.CurrentPrice, atrPrice, 1.5,
		).Round(int32(pricePrecision))

		// Safety check on StopLoss positioning
		if trend == "short" {
			if antiHuntStop.LessThanOrEqual(upperPrice) {
				antiHuntStop = upperPrice.Mul(decimal.NewFromFloat(1.02)).Round(int32(pricePrecision))
			}
		} else {
			// LONG or NEUTRAL: stop must be below lower boundary
			if antiHuntStop.GreaterThanOrEqual(lowerPrice) {
				antiHuntStop = lowerPrice.Mul(decimal.NewFromFloat(0.98)).Round(int32(pricePrecision))
			}
		}

		// Pre-deploy distance check: the current price must have room to
		// the anti-hunt stop BEFORE the bot opens — deploying with price
		// hugging the lower bound means instant STRUCT_INVALID and a
		// wasted create fee (prod: ENSO closed in the same second).
		if trend != "short" {
			minDistance := atrPrice.Mul(decimal.NewFromFloat(1.5))
			if candidate.CurrentPrice.Sub(antiHuntStop).LessThan(minDistance) {
				deployErrors = append(deployErrors, fmt.Sprintf(
					"%s: price %s too close to anti-hunt stop %s (< 1.5 ATR room) — skipped",
					candidate.Symbol, candidate.CurrentPrice.String(), antiHuntStop.String()))
				continue
			}
		} else {
			minDistance := atrPrice.Mul(decimal.NewFromFloat(1.5))
			if antiHuntStop.Sub(candidate.CurrentPrice).LessThan(minDistance) {
				deployErrors = append(deployErrors, fmt.Sprintf(
					"%s: price %s too close to anti-hunt stop %s (< 1.5 ATR room) — skipped",
					candidate.Symbol, candidate.CurrentPrice.String(), antiHuntStop.String()))
				continue
			}
		}

		// Leverage precedence: HAR vol-inverse cap, then Smart Direction's
		// conservative cap — either may only scale DOWN from the operator's
		// setting. Without HAR/smart inputs the ATR adaptive ladder applies.
		botLev := settings.Leverage
		if harGeo != nil && harGeo.geo.Leverage < botLev {
			botLev = harGeo.geo.Leverage
		}
		if smartLev > 0 && smartLev < botLev {
			botLev = smartLev
		}
		if harGeo == nil && smartLev == 0 && settings.AdaptiveLeverageEnabled {
			dyn := ComputeDynamicLeverage(atrPct, settings.Leverage)
			botLev = dyn.Leverage
		}

		if err := worker.risk.ValidateNewGrid(
			ctx, *settings.AccountID, candidate.Symbol,
			botLev, settings.BudgetUSDT,
		); err != nil {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: risk gate: %v", candidate.Symbol, err))
			continue
		}
		base, quote, err := SplitPionexPerp(candidate.Symbol)
		if err != nil {
			deployErrors = append(deployErrors, err.Error())
			continue
		}
		gridTypeStr := mapGridType(settings.DensityGridEnabled)
		data := pionex.BUOrderData{
			Top: upperPrice, Bottom: lowerPrice,
			Row: mesh.GridNum, GridType: gridTypeStr,
			Trend:           trend,
			Leverage:        botLev,
			QuoteInvestment: investAmount.Round(2),
		}
		if settings.StopLossMode == "ADAPTIVE_ATR" {
			data.LossStopType = "price"
			data.LossStop = &antiHuntStop
		}
		// Native exchange-side take-profit: the per-bot target (dynamic from
		// AI Kit/scanner readings, or the operator's fixed amount) is enforced
		// by Pionex itself (profit_amount), so it survives even if this
		// process is down. The management loop double-checks it locally.
		botTarget, botMaxLoss := computeBotTargets(settings, candidate)
		if botTarget != nil && botTarget.GreaterThan(decimal.Zero) {
			targetVal := botTarget.Round(2)
			data.ProfitStopType = "profit_amount"
			data.ProfitStop = &targetVal
		} else if settings.SmartPNLEnabled && trend != "no_trend" {
			profit := upperPrice
			if trend == "short" {
				profit = lowerPrice
			}
			data.ProfitStopType = "price"
			data.ProfitStop = &profit
		}
		futuresBase := base
		if !strings.HasSuffix(futuresBase, ".PERP") && !strings.HasSuffix(futuresBase, "_PERP") {
			futuresBase = fmt.Sprintf("%s.PERP", base)
		}
		params := pionex.NativeFuturesGridCreateParams{
			Base: futuresBase, Quote: quote, BUOrderData: data,
		}
		// Native pre-flight validation: check parameters against Pionex estimation
		check, checkErr := client.CheckFuturesGridParams(ctx, params)
		if checkErr != nil {
			worker.logger.Warn(
				"Pionex checkParams estimation returned warning, attempting direct creation",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "error", checkErr,
			)
		} else if check != nil && check.GetMinInvestment().GreaterThan(decimal.Zero) &&
			investAmount.LessThan(check.GetMinInvestment()) {
			deployErrors = append(deployErrors, fmt.Sprintf(
				"%s: budget %s below Pionex minimum investment %s",
				candidate.Symbol, settings.BudgetUSDT, check.GetMinInvestment(),
			))
			continue
		}
		botID, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
			AccountID:          *settings.AccountID,
			AutoGridSettingsID: &settings.ID,
			IdempotencyKey:     "autogrid:" + scanID + ":" + candidate.ID,
			Params:             params,
			PnLTargetUSDT:      botTarget,
			MaxLossUSDT:        botMaxLoss,
			// Persist the deploy-time invalidation level and thesis so the
			// supervision loop can exit before the exchange stop is swept.
			// Confluence readings ride in model_assumptions JSONB.
			AntiHuntStop:  &antiHuntStop,
			StructContext: deployStructContext(candidate, antiHuntStop),
		})
		if createErr != nil {
			if errors.Is(createErr, grid.ErrDuplicateActiveBot) {
				// A concurrent scan deployed this symbol first; not an error.
				worker.logger.Info("skip real deploy: active grid already exists",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
			}
			deployErrors = append(deployErrors, fmt.Sprintf("%s: create failed: %v", candidate.Symbol, createErr))
			continue
		}
		activeCount++

		var botNum int
		_ = worker.db.QueryRow(ctx, `SELECT COALESCE(bot_number, 0) FROM grid_bots WHERE id = $1`, botID).Scan(&botNum)

		// v2.0.13: per-bot tranche markers (audit F1/F6) — deriving the
		// pending tranche from live settings would auto-inject real margin
		// into every old bot on any budget raise. Markers match the paper
		// model_state contract.
		_, _ = worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET model_state = jsonb_build_object(
			        'trancheDeployed', $2::INT,
			        'trancheBase', $3::TEXT,
			        'trancheEntry', $4::TEXT,
			        'atrPctEntry', $5::FLOAT8
			    ),
			    updated_at = NOW()
			WHERE id = $1
		`, botID, trancheFlag(settings.TrancheDeployEnabled), settings.BudgetUSDT.String(),
			candidate.CurrentPrice.String(), atrPct)

		_ = LogBotEvent(ctx, worker.db, botID, botNum, "REAL", candidate.Symbol, "CREATED", &candidate.CurrentPrice, nil, map[string]any{
			"leverage": botLev, "gridNum": mesh.GridNum, "lowerPrice": lowerPrice, "upperPrice": upperPrice, "budget": settings.BudgetUSDT,
		})
		_ = QueueTelegramEvent(ctx, worker.db, "BOT_CREATED", map[string]any{
			"bot_number": botNum, "symbol": candidate.Symbol, "direction": strings.ToUpper(trend),
			"leverage": botLev, "lower_price": lowerPrice, "upper_price": upperPrice,
			"grid_num": mesh.GridNum, "quote_investment": settings.BudgetUSDT, "source": "REAL",
		})
		// Rate limit protection: 1.2s delay between bot creations
		time.Sleep(1200 * time.Millisecond)
	}
	if len(deployErrors) > 0 {
		worker.logger.Warn(
			"AutoGrid deploy skipped some candidates",
			"component", "autogrid_worker", "skipped", strings.Join(deployErrors, " | "),
		)
		if activeCount == 0 {
			_, _ = worker.db.Exec(ctx, `UPDATE autogrid_settings SET last_error = $1 WHERE id = $2`, deployErrors[0], settings.ID)
		}
	} else if activeCount > 0 {
		_, _ = worker.db.Exec(ctx, `UPDATE autogrid_settings SET last_error = NULL WHERE id = $1`, settings.ID)
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
	// Real bots may live under an implicitly resolved account (settings
	// account never selected); cancel them regardless of the settings value.
	if err := worker.cancelRealBots(ctx, settings); err != nil {
		_ = worker.service.SetStatus(ctx, "EMERGENCY_STOPPED", err)
		return err
	}
	return worker.service.SetStatus(ctx, "EMERGENCY_STOPPED", nil)
}

func (worker *Worker) cancelRealBots(ctx context.Context, settings *Settings) error {
	rows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id, account_id
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID)
	if err != nil {
		return fmt.Errorf("list real grids for emergency stop: %w", err)
	}
	defer rows.Close()
	type target struct{ id, remoteID, accountID string }
	targets := make([]target, 0)
	for rows.Next() {
		var item target
		if err := rows.Scan(&item.id, &item.remoteID, &item.accountID); err != nil {
			return err
		}
		targets = append(targets, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	clients := make(map[string]*pionex.Client)
	clientFor := func(accountID string) (*pionex.Client, error) {
		if cached, ok := clients[accountID]; ok {
			return cached, nil
		}
		client, err := worker.service.PrivateClient(ctx, worker.accounts, accountID)
		if err != nil {
			return nil, err
		}
		clients[accountID] = client
		return client, nil
	}
	cancelErrors := make([]string, 0)
	for _, item := range targets {
		client, err := clientFor(item.accountID)
		if err != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("%s: resolve client: %v", item.remoteID, err))
			continue
		}
		if err := worker.cancelRealBot(ctx, client, item.id, item.remoteID, "autogrid emergency stop"); err != nil {
			cancelErrors = append(cancelErrors, fmt.Sprintf("%s: %v", item.remoteID, err))
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
		CloseSellMode: "TO_USDT", Immediate: true,
	})
	if cancelErr != nil {
		errStr := strings.ToLower(cancelErr.Error())
		if strings.Contains(errStr, "already_closed") || strings.Contains(errStr, "already closed") ||
			strings.Contains(errStr, "not_found") || strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "not_exist") || strings.Contains(errStr, "invalid_order") ||
			strings.Contains(errStr, "forbidden current state") || strings.Contains(errStr, "can not cancel") {
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET status = 'STOPPED', closed_reason = 'ALREADY_CLOSED',
				    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
				    closed_at = NOW(), last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
				WHERE id = $1
			`, botID)
			worker.logger.Info("Pionex grid already closed remotely, marked STOPPED",
				"component", "autogrid_worker", "bot_id", botID, "remote_id", remoteID)
			return nil
		}
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

// rejectCandidate records a late-stage rejection so the UI shows WHY a
// previously accepted candidate never deployed.
func (worker *Worker) rejectCandidate(
	ctx context.Context, candidate Candidate, reason string, assumptions map[string]any,
) {
	_, _ = worker.db.Exec(ctx, `
		UPDATE autogrid_candidates
		SET decision = 'REJECTED', rejection_reason = $2,
		    model_assumptions = model_assumptions || $3::jsonb
		WHERE id = $1
	`, candidate.ID, reason, assumptions)
}

// deployStructContext snapshots the market thesis a bot is opened under:
// confluence readings from the candidate's model_assumptions plus the
// invalidation level the supervision loop will act on.
func deployStructContext(candidate Candidate, antiHuntStop decimal.Decimal) map[string]any {
	context := map[string]any{
		"invalidation": antiHuntStop.String(),
		"deployedAt":   time.Now().UTC().Format(time.RFC3339),
	}
	if candidate.ModelAssumptions != nil {
		if hurst, ok := candidate.ModelAssumptions["hurst"].(float64); ok {
			context["hurst"] = hurst
		}
		if confluence, ok := candidate.ModelAssumptions["confluence"].(map[string]any); ok {
			if verdict, ok := confluence["verdict"].(string); ok {
				context["confluenceVerdict"] = verdict
			}
			if strength, ok := confluence["strength"].(float64); ok {
				context["confluenceStrength"] = strength
			}
		}
	}
	return context
}

// pinManagedAccount persists the implicitly resolved AutoGrid account into
// the settings row so deploys and supervision can never diverge (manual and
// autopilot REAL deploys already run under the resolved account). Best-effort:
// supervision itself follows each bot's own account_id, so a failure here is
// logged but never blocks management.
func (worker *Worker) pinManagedAccount(ctx context.Context, settings *Settings) {
	if settings.AccountID != nil {
		return
	}
	resolved, err := worker.service.resolveAccount(ctx)
	if err != nil {
		return
	}
	if _, err := worker.db.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, updated_at = NOW()
		WHERE id = $1 AND account_id IS NULL
	`, settings.ID, *resolved); err != nil {
		worker.logger.Warn("persist resolved AutoGrid account",
			"component", "autogrid_worker", "error", err)
		return
	}
	settings.AccountID = resolved
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
	// Pin the implicitly resolved account into settings when possible so
	// deploys and supervision stay on the same account. Supervision itself
	// must not depend on it: real bots carry their own account_id and are
	// managed regardless (orphaned bots must still receive stop requests).
	worker.pinManagedAccount(ctx, settings)
	var count int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID).Scan(&count); err != nil || count == 0 {
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	priceBySymbol, priceErr := worker.priceMap(ctx)
	if priceErr != nil {
		worker.logger.Warn("fetch tickers for management", "component", "autogrid_worker", "error", priceErr)
	}
	rows, err := worker.db.Query(ctx, `
		SELECT id, account_id, bu_order_id, status, symbol, direction,
		       lower_price, upper_price, grid_num, adjustments_count,
		       pnl_target_usdt, max_loss_usdt, quote_investment,
		       anti_hunt_stop_price, COALESCE(bot_number, 0),
		       COALESCE(peak_pnl_usdt, 0), created_at,
		       COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       NULLIF(model_state->>'trancheBase',''),
		       NULLIF(model_state->>'trancheEntry',''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0),
		       NULLIF(model_state->>'trancheFailAt','')
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		ORDER BY updated_at
	`, settings.ID)
	if err != nil {
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	type managedBot struct {
		id, accountID, remoteID, localStatus, symbol, direction string
		lower, upper                                            decimal.Decimal
		rowNum, adjustments                                     int
		pnlTarget, maxLoss                                      *decimal.Decimal
		antiHuntStop                                            *decimal.Decimal
		investment                                              decimal.Decimal
		botNumber                                               int
		peak                                                    decimal.Decimal
		createdAt                                               time.Time
		trancheDeployed                                         int
		trancheBase                                             *string
		trancheEntry                                            *string
		atrEntry                                                float64
		trancheFailAt                                           *string
	}
	bots := make([]managedBot, 0)
	for rows.Next() {
		var item managedBot
		if err := rows.Scan(
			&item.id, &item.accountID, &item.remoteID, &item.localStatus, &item.symbol,
			&item.direction, &item.lower, &item.upper, &item.rowNum,
			&item.adjustments, &item.pnlTarget, &item.maxLoss, &item.investment,
			&item.antiHuntStop, &item.botNumber,
			&item.peak, &item.createdAt,
			&item.trancheDeployed, &item.trancheBase, &item.trancheEntry,
			&item.atrEntry, &item.trancheFailAt,
		); err != nil {
			rows.Close()
			return clampInterval(settings.ManageIntervalSeconds), err
		}
		bots = append(bots, item)
	}
	rows.Close()

	clients := make(map[string]*pionex.Client)
	clientFor := func(accountID string) (*pionex.Client, error) {
		if cached, ok := clients[accountID]; ok {
			return cached, nil
		}
		client, err := worker.service.PrivateClient(ctx, worker.accounts, accountID)
		if err != nil {
			return nil, err
		}
		clients[accountID] = client
		return client, nil
	}

	for _, bot := range bots {
		client, clientErr := clientFor(bot.accountID)
		if clientErr != nil {
			worker.logger.Error("resolve Pionex client for managed bot",
				"component", "autogrid_worker", "bot_id", bot.id,
				"account_id", bot.accountID, "error", clientErr)
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET reconciliation_state = 'REMOTE_READ_FAILED',
				    last_error = $2, last_reconciled_at = NOW(), updated_at = NOW()
				WHERE id = $1
			`, bot.id, clientErr.Error())
			continue
		}
		if bot.localStatus != "RUNNING" {
			var reconciliation string
			_ = worker.db.QueryRow(ctx, `SELECT COALESCE(reconciliation_state, '') FROM grid_bots WHERE id = $1`, bot.id).Scan(&reconciliation)
			if reconciliation != "CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING" && reconciliation != "REMOTE_TERMINAL_CONFIRMED" {
				if err := worker.cancelRealBot(ctx, client, bot.id, bot.remoteID, "autogrid stop"); err != nil {
					worker.logger.Error("submit native cancel", "component", "autogrid_worker", "bot_id", bot.id, "error", err)
				}
			}
		}

		remote, getErr := client.GetFuturesGridBot(ctx, bot.remoteID)
		if getErr != nil {
			errStr := strings.ToLower(getErr.Error())
			if strings.Contains(errStr, "not_found") || strings.Contains(errStr, "not found") ||
				strings.Contains(errStr, "already_closed") || strings.Contains(errStr, "already closed") ||
				strings.Contains(errStr, "not_exist") || strings.Contains(errStr, "404") || strings.Contains(errStr, "invalid_order") ||
				strings.Contains(errStr, "forbidden current state") || strings.Contains(errStr, "can not cancel") {
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET status = 'STOPPED', closed_reason = 'ALREADY_CLOSED',
					    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
					    closed_at = NOW(), last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
					WHERE id = $1
				`, bot.id)
				worker.logger.Info("Pionex grid not found or already closed on exchange, marked STOPPED",
					"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id)
				continue
			}
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
		price, ok := priceBySymbol[bot.symbol]
		if !ok || price.IsZero() {
			trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(bot.symbol), "_PERP"), ".PERP")
			price, ok = priceBySymbol[trimmed]
			if !ok || price.IsZero() {
				price = priceBySymbol[trimmed+"_PERP"]
			}
		}
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
				SET status = $2,
				    closed_reason = COALESCE(NULLIF(closed_reason, ''), $3),
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
			    unrealized_pnl_usdt = $6,
			    peak_pnl_usdt = GREATEST(COALESCE(peak_pnl_usdt, 0), $5 + $6),
			    last_reconciled_at = NOW(),
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

		// v2.0.13 tranche 2 (REAL): paper-parity trigger — marker-based (a
		// settings budget raise must never auto-inject margin into old
		// bots), adverse measured from the stored entry against 0.75×ATR(1h)
		// with two confirming 15m closes, or the 24h time-box. invest_in is
		// at-least-once on the exchange: a failed attempt arms a 1h backoff
		// marker so a persist failure cannot machine-gun real margin.
		trancheBackoff := false
		if bot.trancheFailAt != nil {
			if failedAt, pErr := time.Parse(time.RFC3339, strings.TrimSpace(*bot.trancheFailAt)); pErr == nil &&
				time.Since(failedAt) < time.Hour {
				trancheBackoff = true
			}
		}
		if !trancheBackoff && bot.trancheDeployed == 1 && bot.trancheBase != nil && bot.localStatus == "RUNNING" && price.IsPositive() {
			if base, bErr := decimal.NewFromString(*bot.trancheBase); bErr == nil && base.GreaterThan(bot.investment) {
				entry := price
				if bot.trancheEntry != nil {
					if e, eErr := decimal.NewFromString(*bot.trancheEntry); eErr == nil && e.IsPositive() {
						entry = e
					}
				}
				topUp := ""
				if time.Since(bot.createdAt) >= trancheTimeBox {
					// v2.0.14: the unconditional top-up must not fire into a
					// confirmed trend either way — adding margin at stretched
					// highs (or into a falling knife) is exactly what the
					// signal path exists to gate. Defer until the tape is
					// not strongly trending; the confirmed-adverse path and
					// the next cycles stay available.
					if worker.trancheTimeBoxTrending(ctx, bot.symbol) {
						topUp = ""
					} else {
						topUp = "time-box 24h"
					}
				} else if bot.atrEntry > 0 {
					adverse := price.Sub(entry).Abs().Div(entry)
					limit := decimal.NewFromFloat(bot.atrEntry * 2.0 * 0.75 / 100.0)
					if adverse.GreaterThanOrEqual(limit) && worker.trancheTurnConfirmed(ctx, bot.symbol, price, entry) {
						topUp = "подтверждённый adverse 0.75×ATR(1h)"
					}
				}
				if topUp != "" {
					pending := base.Sub(bot.investment)
					if _, err := worker.service.AdjustBot(ctx, worker.accounts, settings.ID, bot.id, AdjustBotInput{
						Mode:            "invest_in",
						QuoteInvestment: pending.Round(2),
					}); err != nil {
						worker.logger.Error("tranche 2 invest_in failed",
							"component", "autogrid_worker", "bot_id", bot.id, "error", err)
						_, _ = worker.db.Exec(ctx, `
							UPDATE grid_bots
							SET model_state = jsonb_set(model_state, '{trancheFailAt}', to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
							    updated_at = NOW()
							WHERE id = $1
						`, bot.id)
					} else {
						_, _ = worker.db.Exec(ctx, `
							UPDATE grid_bots
							SET pnl_target_usdt = pnl_target_usdt * 2,
							    max_loss_usdt = max_loss_usdt * 2,
							    model_state = jsonb_set(model_state, '{trancheDeployed}', '2'::jsonb),
							    updated_at = NOW()
							WHERE id = $1
						`, bot.id)
						worker.logger.Info("tranche 2 deployed (REAL invest_in)",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", topUp)
						_ = QueueTelegramEvent(ctx, worker.db, "TRANCHE_2", map[string]any{
							"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": topUp,
						})
					}
				}
			}
		}

		peakNow := bot.peak
		if current := realized.Add(unrealized); current.GreaterThan(peakNow) {
			peakNow = current
		}
		decision := decideBotAction(botActionInput{
			Direction:        bot.direction,
			Lower:            bot.lower,
			Upper:            bot.upper,
			CurrentPrice:     price,
			RealizedPNL:      realized,
			UnrealizedPNL:    unrealized,
			PeakPNL:          peakNow,
			Budget:           bot.investment,
			PnLTarget:        botTarget,
			MaxLoss:          botMaxLoss,
			RangeBreakBuffer: settings.RangeBreakBufferPct,
			AdjustmentsLeft:  settings.MaxAdjustmentsPerBot - bot.adjustments,
			Regime:           regime,
			AntiHuntStop:     bot.antiHuntStop,
		})
		switch decision.Action {
		case ActionCloseTakeProfit, ActionCloseStopLoss, ActionCloseRangeBreak, ActionCloseStructInvalid:
			totalPnL := realized.Add(unrealized)
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
					"reason", decision.Reason, "pnl", totalPnL.String())

				eventType := "STOP_LOSS"
				if decision.Action == ActionCloseTakeProfit {
					eventType = "TAKE_PROFIT"
				}
				pnlPct := decimal.Zero
				if !bot.investment.IsZero() {
					pnlPct = totalPnL.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
				}
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, eventType, &price, &totalPnL, map[string]any{
					"reason": decision.Reason, "pnlPct": pnlPct,
				})
				_ = QueueTelegramEvent(ctx, worker.db, eventType, map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol,
					"pnl_usdt": totalPnL.StringFixed(4), "pnl_pct": pnlPct.StringFixed(2),
					"reason": decision.Reason,
				})
			}
		case ActionAdjustUp, ActionAdjustDown:
			if !price.GreaterThan(decimal.Zero) {
				worker.logger.Error("adjust native grid range skipped: no live price",
					"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol)
				break
			}
			if err := client.AdjustFuturesGridBot(ctx, pionex.AdjustFuturesGridParams{
				BUOrderID: bot.remoteID, Type: "adjust_params",
				ExtraMargin: false, OpenPrice: &price,
				Bottom: &decision.NewLower, Top: &decision.NewUpper, Row: bot.rowNum,
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

				totalPnL := realized.Add(unrealized)
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, "ADJUST_RANGE", &price, &totalPnL, map[string]any{
					"reason": decision.Reason, "new_lower": decision.NewLower.String(), "new_upper": decision.NewUpper.String(),
				})
				_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol,
					"lower_price": decision.NewLower.StringFixed(6), "upper_price": decision.NewUpper.StringFixed(6),
					"reason": decision.Reason, "adjustments_count": bot.adjustments + 1,
				})
			}
		}
	}

	// Synchronize closed bots with exchange history to fill accurate realized PnL.
	// The old query excluded zero-PnL REMOTE_TERMINAL_CONFIRMED bots — exactly
	// the population written by the ALREADY_CLOSED paths without PnL — which
	// permanently understated realized results. Bounded to a 48h window so the
	// per-cycle work stays finite.
	closedRows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id, account_id
		FROM grid_bots
		WHERE bu_order_id IS NOT NULL
		  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
		  AND (realized_pnl_usdt IS NULL OR realized_pnl_usdt = 0)
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '48 hours'
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err == nil {
		type closedBotItem struct {
			id, remoteID, accountID string
		}
		var unSynced []closedBotItem
		for closedRows.Next() {
			var item closedBotItem
			if err := closedRows.Scan(&item.id, &item.remoteID, &item.accountID); err == nil {
				unSynced = append(unSynced, item)
			}
		}
		closedRows.Close()
		for _, item := range unSynced {
			historyClient, clientErr := clientFor(item.accountID)
			if clientErr != nil {
				continue
			}
			if remote, remoteErr := historyClient.GetFuturesGridBot(ctx, item.remoteID); remoteErr == nil && remote != nil {
				profit := remote.BUOrderData.ProfitWithdrawn
				if profit.IsZero() {
					profit = remote.BUOrderData.TotalProfit
				}
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET realized_pnl_usdt = $2,
					    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
					    updated_at = NOW()
					WHERE id = $1
				`, item.id, profit)
			}
		}
	}

	// Unknown submissions are adopted per account: each pending row carries
	// the account its client must authenticate as.
	unknownAccountRows, err := worker.db.Query(ctx, `
		SELECT DISTINCT account_id FROM grid_bots
		WHERE bu_order_id IS NULL
		  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION')
		  AND created_at > NOW() - INTERVAL '48 hours'
	`)
	if err == nil {
		for unknownAccountRows.Next() {
			var accountID string
			if scanErr := unknownAccountRows.Scan(&accountID); scanErr == nil {
				if unknownClient, clientErr := clientFor(accountID); clientErr == nil {
					worker.reconcileUnknownSubmissions(ctx, unknownClient)
				}
			}
		}
		unknownAccountRows.Close()
	}

	return clampInterval(settings.ManageIntervalSeconds), nil
}

// reconcileUnknownSubmissions resolves grid bots whose create outcome is
// unknown or that crashed between intent and submission. Without it, a
// transport failure after POST futuresGrid/create can leave a live exchange
// grid running forever with no stop-loss, no PnL accounting and no way to
// redeploy the symbol. Remote orders from the documented
// GET /api/v1/bot/orders list are matched by symbol and creation time:
// a unique match adopts the remote buOrderId; a provably absent bot (lists
// fully paginated without error) is cleared so the symbol is freed.
func (worker *Worker) reconcileUnknownSubmissions(ctx context.Context, client *pionex.Client) {
	rows, err := worker.db.Query(ctx, `
		SELECT id, symbol, quote_investment, EXTRACT(EPOCH FROM created_at) * 1000
		FROM grid_bots
		WHERE bu_order_id IS NULL
		  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION')
		  AND created_at > NOW() - INTERVAL '48 hours'
		  AND created_at < NOW() - INTERVAL '90 seconds'
		ORDER BY created_at
		LIMIT 10
	`)
	if err != nil {
		return
	}
	type unknownBot struct {
		id, symbol string
		investment decimal.Decimal
		createdMS  float64
	}
	pending := make([]unknownBot, 0, 10)
	for rows.Next() {
		var item unknownBot
		if err := rows.Scan(&item.id, &item.symbol, &item.investment, &item.createdMS); err == nil {
			pending = append(pending, item)
		}
	}
	rows.Close()
	if len(pending) == 0 {
		return
	}

	remoteOrders := make([]pionex.BotOrder, 0, 64)
	listsComplete := true
	for _, listStatus := range []string{"running", "finished"} {
		token := ""
		for page := 0; page < 10; page++ {
			orders, next, listErr := client.ListBotOrders(ctx, listStatus, token)
			if listErr != nil {
				worker.logger.Warn("list bot orders for unknown-submission reconciliation",
					"component", "autogrid_worker", "status", listStatus, "error", listErr)
				listsComplete = false
				break
			}
			remoteOrders = append(remoteOrders, orders...)
			if next == "" {
				break
			}
			token = next
			if page == 9 {
				// Page budget exhausted with a continuation token: the listing
				// is NOT complete — a live bot may sit on the next page.
				listsComplete = false
			}
		}
	}

	for _, bot := range pending {
		matches := make([]pionex.BotOrder, 0, 2)
		for _, order := range remoteOrders {
			if order.BUOrderID == "" {
				continue
			}
			if strings.ToUpper(order.Base+"_"+order.Quote+"_PERP") != strings.ToUpper(bot.symbol) {
				continue
			}
			if order.CreateTimeMS <= 0 || math.Abs(float64(order.CreateTimeMS)-bot.createdMS) > 10*60*1000 {
				continue
			}
			if investment, ok := order.GridInvestment(); ok && investment.GreaterThan(decimal.Zero) &&
				bot.investment.GreaterThan(decimal.Zero) {
				tolerance := bot.investment.Div(decimal.NewFromInt(50)) // 2%
				diff := investment.Sub(bot.investment).Abs()
				if diff.GreaterThan(tolerance) {
					continue
				}
			}
			matches = append(matches, order)
		}
		if len(matches) == 1 {
			tag, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET bu_order_id = $2, status = 'RUNNING',
				    reconciliation_state = 'REMOTE_ID_PERSISTED',
				    last_remote_status = 'ADOPTED_AFTER_UNKNOWN_SUBMISSION',
				    last_error = NULL, updated_at = NOW()
				WHERE id = $1 AND bu_order_id IS NULL
				  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION')
			`, bot.id, matches[0].BUOrderID)
			if err == nil && tag.RowsAffected() == 1 {
				worker.logger.Info("adopted remotely created grid after unknown submission",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"bu_order_id", matches[0].BUOrderID)
				_ = QueueTelegramEvent(ctx, worker.db, "EMERGENCY", map[string]any{
					"message": fmt.Sprintf("Bot %s: create outcome was unknown; exchange bot %s adopted and is now managed",
						bot.symbol, matches[0].BUOrderID),
				})
			}
			continue
		}
		if len(matches) > 1 {
			worker.logger.Warn("ambiguous remote matches for unknown submission; manual review required",
				"component", "autogrid_worker", "symbol", bot.symbol, "matches", len(matches))
			continue
		}
		// No running or finished order matches. Only clear the row when both
		// lists paginated to the end without errors, otherwise the bot may
		// simply live on a page we could not reach.
		if !listsComplete || time.Since(time.UnixMilli(int64(bot.createdMS))) < 30*time.Minute {
			continue
		}
		tag, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'FAILED', closed_reason = 'NOT_CREATED_ON_EXCHANGE',
			    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
			    last_error = NULL, closed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND bu_order_id IS NULL
			  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION')
		`, bot.id)
		if err == nil && tag.RowsAffected() == 1 {
			worker.logger.Info("cleared unknown submission: no matching exchange bot exists",
				"component", "autogrid_worker", "symbol", bot.symbol)
		}
	}
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
		SELECT id, COALESCE(bot_number, 0), symbol, direction, entry_price, leverage, quote_investment,
		       lower_price, upper_price, pnl_target_usdt, max_loss_usdt,
		       grid_num, last_grid_level, realized_pnl_usdt, COALESCE(adjustments_count, 0),
		       anti_hunt_stop_price, opened_at, last_funding_at,
		       COALESCE(peak_pnl_usdt, 0),
		       COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       NULLIF(model_state->>'trancheBase',''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0)
		FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID)
	if err != nil {
		return err
	}
	type paperBot struct {
		id                       string
		botNumber                int
		symbol, direction        string
		entry                    decimal.Decimal
		leverage                 int
		investment, lower, upper decimal.Decimal
		pnlTarget, maxLoss       *decimal.Decimal
		antiHuntStop             *decimal.Decimal
		gridNum                  int
		lastLevel                *int
		realized                 decimal.Decimal
		adjustmentsCount         int
		openedAt                 time.Time
		lastFundingAt            *time.Time
		peak                     decimal.Decimal
		trancheDeployed          int
		trancheBase              *string
		atrEntry                 float64
	}
	bots := make([]paperBot, 0)
	for rows.Next() {
		var item paperBot
		if err := rows.Scan(
			&item.id, &item.botNumber, &item.symbol, &item.direction, &item.entry,
			&item.leverage, &item.investment, &item.lower, &item.upper,
			&item.pnlTarget, &item.maxLoss, &item.gridNum, &item.lastLevel,
			&item.realized, &item.adjustmentsCount, &item.antiHuntStop,
			&item.openedAt, &item.lastFundingAt,
			&item.peak, &item.trancheDeployed, &item.trancheBase, &item.atrEntry,
		); err != nil {
			rows.Close()
			return err
		}
		bots = append(bots, item)
	}
	rows.Close()

	effectiveFeeBps := settings.FeeBps.Add(settings.SlippageBps)
	feeRate := effectiveFeeBps.Div(decimal.NewFromInt(10000))

	// Real cross-exchange funding (v2.0.6): per-symbol signed 8h rate from
	// the collector replaces the flat PaperFundingRateBps default whenever
	// the symbol has coverage — negative rates credit long inventories
	// exactly like the exchanges do.
	fundingRateBySymbol := map[string]decimal.Decimal{}
	if len(bots) > 0 && worker.market != nil {
		symbols := make([]string, 0, len(bots))
		for _, bot := range bots {
			symbols = append(symbols, bot.symbol)
		}
		if rates, err := worker.market.GetCurrentFundingBatch(ctx, symbols); err == nil {
			for symbol, info := range rates {
				if info != nil {
					fundingRateBySymbol[symbol] = decimal.NewFromFloat(info.AverageRate).Mul(decimal.NewFromInt(10000))
				}
			}
		}
	}

	for _, bot := range bots {
		price, ok := priceBySymbol[bot.symbol]
		if !ok || price.IsZero() {
			trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(bot.symbol), "_PERP"), ".PERP")
			price, ok = priceBySymbol[trimmed]
			if !ok || price.IsZero() {
				price, ok = priceBySymbol[trimmed+"_PERP"]
			}
		}
		if !ok || price.IsZero() {
			continue
		}
		// A zero entry price cannot be divided against; repair it from the
		// live price instead of panicking the supervision goroutine.
		if bot.direction != "NEUTRAL" && !bot.entry.GreaterThan(decimal.Zero) {
			worker.logger.Warn("repairing zero paper entry price from live tick",
				"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id)
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots SET entry_price = $2, mark_price = $2 WHERE id = $1
			`, bot.id, price)
			continue
		}
		realized := bot.realized
		unrealized := decimal.Zero
		currentLevel := 0
		// Funding exposure: leveraged notional that funding settles on. Long
		// inventory/positions pay the (positive) rate, short ones receive it —
		// the standard perpetual convention Pionex applies every 8 hours.
		fundingExposure := decimal.Zero
		fundingPays := true
		// Notional a real close would have to sell/buy back — taker fee +
		// slippage apply on it (v2.0.6 honesty fix: crystallized marks must
		// be net of the exit cost, otherwise every protective close slightly
		// overstates PnL).
		exitNotional := decimal.Zero
		if bot.direction == "NEUTRAL" {
			// Native-grid simulation (v1.3.22): realized profit accrues only
			// on crossings that close previously accumulated inventory
			// (completed buy/sell pairs, mirroring the exchange's own Grid
			// Profit attribution); the uniform ladder is marked with leveraged
			// per-level notional. Fees and slippage apply per completed pair.
			currentLevel = gridLevelForPrice(bot.lower, bot.upper, bot.gridNum, price)
			previousLevel := currentLevel
			if bot.lastLevel != nil {
				previousLevel = *bot.lastLevel
			}
			var pairProfit, inventoryNotional decimal.Decimal
			pairProfit, unrealized, inventoryNotional = neutralGridPaperPNL(
				bot.lower, bot.upper, bot.gridNum, bot.investment, bot.leverage,
				previousLevel, currentLevel, price, effectiveFeeBps,
			)
			realized = realized.Add(pairProfit)
			fundingExposure = inventoryNotional
			fundingPays = price.LessThan(bot.lower.Add(bot.upper).Div(decimal.NewFromInt(2)))
			exitNotional = inventoryNotional
		} else {
			// Directional grid: account for entry taker fee and slippage
			entryCost := bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(feeRate)
			fundingExposure = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage)))
			fundingPays = bot.direction == "LONG"
			exitNotional = fundingExposure
			switch bot.direction {
			case "LONG":
				gross := bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(price.Div(bot.entry).Sub(decimal.NewFromInt(1)))
				unrealized = gross.Sub(entryCost)
			case "SHORT":
				gross := bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(decimal.NewFromInt(1).Sub(price.Div(bot.entry)))
				unrealized = gross.Sub(entryCost)
			}
		}
		// Exit-fee honesty: the mark a close could crystallize is net of the
		// taker fee + slippage a real close pays on the open notional. Applied
		// before decisions so stop/target thresholds see the true close value.
		if exitNotional.IsPositive() {
			unrealized = unrealized.Sub(exitNotional.Mul(feeRate))
		}
		botFundingRateBps := settings.PaperFundingRateBps
		if realRate, hasRate := fundingRateBySymbol[bot.symbol]; hasRate {
			botFundingRateBps = realRate
		}
		if fundingDelta, nextFundingAt := fundingAccrual(
			fundingExposure, botFundingRateBps,
			bot.openedAt, bot.lastFundingAt, time.Now(),
		); fundingDelta != nil {
			if fundingPays {
				realized = realized.Sub(*fundingDelta)
			} else {
				realized = realized.Add(*fundingDelta)
			}
			// Persist the accrual anchor separately: realized itself flows to
			// the mark/close/adjust UPDATE below, and a crash in between can
			// lose at most one 8h accrual — never double-count.
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots SET last_funding_at = $2, updated_at = NOW() WHERE id = $1
			`, bot.id, *nextFundingAt)
			_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "FUNDING", &price, fundingDelta, map[string]any{
				"pays": fundingPays, "funding_usdt": fundingDelta.StringFixed(6),
			})
		}
		botTarget, botMaxLoss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		if bot.pnlTarget != nil {
			botTarget = *bot.pnlTarget
		}
		if bot.maxLoss != nil {
			botMaxLoss = *bot.maxLoss
		}
		total := realized.Add(unrealized)
		// v2.0.13: persisted peak makes TRAILING_TAKE_PROFIT / BREAKEVEN_LOCK
		// stateful — the old in-memory approximation reset with every cycle,
		// so a bot that touched 90% of target and rolled over never trailed.
		peakPnL := bot.peak
		if total.GreaterThan(peakPnL) {
			peakPnL = total
		}
		if peakPnL.IsNegative() {
			peakPnL = decimal.Zero
		}

		// Lazily detect regime only when price escapes the range
		regime := ""
		buffer := settings.RangeBreakBufferPct.Div(decimal.NewFromInt(100))
		if price.LessThan(bot.lower.Mul(decimal.NewFromInt(1).Sub(buffer))) ||
			price.GreaterThan(bot.upper.Mul(decimal.NewFromInt(1).Add(buffer))) {
			regime = worker.regimeForSymbol(ctx, bot.symbol)
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
			AdjustmentsLeft:  settings.MaxAdjustmentsPerBot - bot.adjustmentsCount,
			Regime:           regime,
			AntiHuntStop:     bot.antiHuntStop,
		})

		if decision.Action == ActionCloseTakeProfit || decision.Action == ActionCloseStopLoss ||
			decision.Action == ActionCloseRangeBreak || decision.Action == ActionCloseStructInvalid {
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

			eventType := "STOP_LOSS"
			if decision.Action == ActionCloseTakeProfit {
				eventType = "TAKE_PROFIT"
			}
			pnlPct := decimal.Zero
			if !bot.investment.IsZero() {
				pnlPct = total.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
			}

			_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, eventType, &price, &total, map[string]any{
				"reason": decision.Reason, "pnlPct": pnlPct,
			})
			_ = QueueTelegramEvent(ctx, worker.db, eventType, map[string]any{
				"bot_number": bot.botNumber, "symbol": bot.symbol,
				"pnl_usdt": total.StringFixed(4), "pnl_pct": pnlPct.StringFixed(2),
				"reason": decision.Reason,
			})
			continue
		}

		if decision.Action == ActionAdjustUp || decision.Action == ActionAdjustDown {
			// Reset the pair-counting baseline under the NEW geometry. The old
			// code persisted currentLevel (computed against the OLD bounds),
			// so the next manage tick saw a phantom half-grid traverse and
			// booked gridNum/2 fictional completed pairs. Recomputing the
			// level against the shifted bounds makes the first tick after the
			// shift a no-op baseline (price sits at the new mid).
			newLevel := gridLevelForPrice(decision.NewLower, decision.NewUpper, bot.gridNum, price)
			// Crystallize the underwater inventory mark into realized. The
			// paper model is stateless: without this the pre-shift inventory
			// loss silently vanishes when the grid recenters. A real
			// adjust_params keeps the position, so the exit-fee component
			// charged into `unrealized` for close decisions is added BACK —
			// no position is exited on a shift, and repeated shifts must not
			// stack phantom exit fees (v2.0.13 audit fix).
			shiftRealized := realized.Add(unrealized)
			if exitNotional.IsPositive() {
				shiftRealized = shiftRealized.Add(exitNotional.Mul(feeRate))
			}
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots
				SET lower_price = $2, upper_price = $3,
				    adjustments_count = adjustments_count + 1,
				    mark_price = $4, unrealized_pnl_usdt = 0,
				    realized_pnl_usdt = $5, last_grid_level = $6,
				    updated_at = NOW()
				WHERE id = $1
			`, bot.id, decision.NewLower, decision.NewUpper, price, shiftRealized, newLevel)

			worker.logger.Info("adjusted paper grid range on the fly",
				"component", "autogrid_worker", "symbol", bot.symbol,
				"lower", decision.NewLower.String(), "upper", decision.NewUpper.String())

			_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "ADJUST_RANGE", &price, &total, map[string]any{
				"reason": decision.Reason, "new_lower": decision.NewLower.String(), "new_upper": decision.NewUpper.String(),
			})
			_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
				"bot_number": bot.botNumber, "symbol": bot.symbol,
				"lower_price": decision.NewLower.StringFixed(6), "upper_price": decision.NewUpper.StringFixed(6),
				"reason": decision.Reason, "adjustments_count": bot.adjustmentsCount + 1,
			})
			continue
		}
		_, _ = worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET mark_price = $2, unrealized_pnl_usdt = $3,
			    realized_pnl_usdt = $4, last_grid_level = $5,
			    peak_pnl_usdt = GREATEST(peak_pnl_usdt, $3 + $4),
			    updated_at = NOW()
			WHERE id = $1
		`, bot.id, price, unrealized, realized, currentLevel)

		// v2.0.13 tranche 2 (paper): top the bot up to its full base after a
		// CONFIRMED adverse excursion (>= 0.75x ATR(1h) from entry with two
		// consecutive 15m closes turning back) or the 24h time-box. Runs
		// AFTER the mark so this tick's funding accrual and PnL persist; the
		// next cycle re-marks with the full investment (the stateless ladder
		// simply doubles per-level notional; last_grid_level keeps the pair
		// baseline). Targets double with NULL propagating — a NULL target
		// means "follow settings", and 0x2 would pin it off.
		if bot.trancheDeployed == 1 && bot.trancheBase != nil {
			if base, bErr := decimal.NewFromString(*bot.trancheBase); bErr == nil && base.GreaterThan(bot.investment) {
				trancheReason := ""
				if time.Since(bot.openedAt) >= trancheTimeBox {
					// v2.0.14: no blind top-ups inside confirmed trends
					// (either direction) — see the REAL-path comment.
					if !worker.trancheTimeBoxTrending(ctx, bot.symbol) {
						trancheReason = "time-box 24h"
					}
				} else if bot.atrEntry > 0 && price.IsPositive() {
					adverse := price.Sub(bot.entry).Abs().Div(bot.entry)
					// ATR(1h) ≈ 2 × ATR(15m) — the entry-time scanner figure.
					limit := decimal.NewFromFloat(bot.atrEntry * 2.0 * 0.75 / 100.0)
					if adverse.GreaterThanOrEqual(limit) && worker.trancheTurnConfirmed(ctx, bot.symbol, price, bot.entry) {
						trancheReason = "подтверждённый adverse 0.75×ATR(1h)"
					}
				}
				if trancheReason != "" {
					tag, tErr := worker.db.Exec(ctx, `
						UPDATE paper_grid_bots
						SET quote_investment = $2,
						    pnl_target_usdt = pnl_target_usdt * 2,
						    max_loss_usdt = max_loss_usdt * 2,
						    model_state = jsonb_set(model_state, '{trancheDeployed}', '2'::jsonb),
						    updated_at = NOW()
						WHERE id = $1 AND status = 'RUNNING'
						  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
					`, bot.id, base)
					if tErr == nil && tag.RowsAffected() == 1 {
						worker.logger.Info("tranche 2 deployed",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", trancheReason)
						_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "TRANCHE_2", &price, nil, map[string]any{
							"reason": trancheReason, "investment": base.String(),
						})
						_ = QueueTelegramEvent(ctx, worker.db, "TRANCHE_2", map[string]any{
							"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": trancheReason,
						})
					}
				}
			}
		}
	}
	return nil
}

func (worker *Worker) priceMap(ctx context.Context) (map[string]decimal.Decimal, error) {
	tickers, err := worker.publicClient.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, err
	}
	prices := make(map[string]decimal.Decimal, len(tickers)*4)
	for _, ticker := range tickers {
		if ticker.Close.GreaterThan(decimal.Zero) {
			sym := strings.ToUpper(strings.TrimSpace(ticker.Symbol))
			prices[sym] = ticker.Close
			trimmed := strings.TrimSuffix(strings.TrimSuffix(sym, "_PERP"), ".PERP")
			prices[trimmed] = ticker.Close
			prices[trimmed+"_PERP"] = ticker.Close
			prices[trimmed+".PERP"] = ticker.Close
		}
	}
	return prices, nil
}

// trancheTimeBoxTrending reports whether the tape is strongly trending for
// the tranche-2 time-box, fetching the regime at most once per 5 minutes per
// symbol (the check would otherwise run every manage tick for every 24h+
// pending bot).
func (worker *Worker) trancheTimeBoxTrending(ctx context.Context, symbol string) bool {
	if cached, ok := worker.trancheTBRegime[symbol]; ok && time.Since(cached.checkedAt) < 5*time.Minute {
		return cached.trending
	}
	regime := worker.regimeForSymbol(ctx, symbol)
	trending := regime == "TREND_UP" || regime == "TREND_DOWN"
	worker.trancheTBRegime[symbol] = trancheTBTrend{checkedAt: time.Now(), trending: trending}
	return trending
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
		AND NOT EXISTS (
			SELECT 1 FROM control_commands
			WHERE command_type = 'autogrid.scan'
			  AND status IN ('QUEUED', 'EXECUTING')
			  AND created_at > NOW() - INTERVAL '30 minutes'
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

// terminalRemoteGridStatus reports whether the remote status is FINAL.
// Transitional states ("stopping", "canceling", "closing") must stay under
// supervision until the exchange reports a final reason — a substring match
// here would finalize a bot whose close is still being worked on, dropping
// it from management while margin is still locked. Unknown status values
// fall back to the not-found/already-closed error path on the next cycle.
func terminalRemoteGridStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "finished", "canceled", "cancelled", "closed", "stopped",
		"stop_by_user", "stopped_by_user", "liquidated", "expired",
		"inactive", "terminated", "completed", "failed":
		return true
	default:
		return false
	}
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
	case strings.Contains(normalized, "user_cancel"), strings.Contains(normalized, "user"):
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
	// Audit in deploy priority order (score desc): the cap below must bite
	// on the LEAST likely deploys, never on the ones deployReal would take.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score.GreaterThan(candidates[j].Score)
	})
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
			if v, ok := candidate.ModelAssumptions["hurst"].(float64); ok {
				input.Hurst = v
			}
			if confluence, ok := candidate.ModelAssumptions["confluence"].(map[string]any); ok {
				if verdict, ok := confluence["verdict"].(string); ok {
					input.ConfluenceVerdict = verdict
				}
			}
			// Pass the operator's scanner floors so the LLM doesn't
			// re-reject what already passed them.
			input.ScannerFloor = llm.ScannerFloor{
				MinVolume24hUSD:  settings.MinVolume24h.InexactFloat64(),
				MinVolatilityPct: settings.MinVolatilityPct.InexactFloat64(),
				MaxVolatilityPct: settings.MaxVolatilityPct.InexactFloat64(),
				MinSharpe:        settings.MinSharpe.InexactFloat64(),
			}
		}

		// Grounded audits run a live google_search before answering and
		// routinely exceed 15s; cutting them mid-flight used to fail-close
		// candidates that would have passed (prod: TUT, context deadline).
		auditCtx, auditCancel := context.WithTimeout(ctx, 45*time.Second)
		decision, record, err := worker.llm.AuditCandidate(auditCtx, &candidate.ID, input)
		auditCancel()
		if err != nil {
			worker.logger.Warn("LLM candidate audit failed", "symbol", candidate.Symbol, "error", err)
			// Fail-closed: when the operator requires a completed audit for
			// REAL money, a transport failure blocks the candidate instead
			// of letting it pass unchecked.
			if llmSettings.RequireAuditForReal && settings.ExecutionMode == "REAL" {
				_, _ = worker.db.Exec(ctx, `
					UPDATE autogrid_candidates
					SET decision = 'REJECTED',
					    rejection_reason = $3,
					    model_assumptions = model_assumptions || jsonb_build_object('llmAuditError', $4::TEXT)
					WHERE id = $1 AND scan_id = $2
				`, candidate.ID, scanID, "LLM audit unavailable (fail-closed)", err.Error())
			}
			continue
		}
		auditedCount++

		// News-catalyst veto: a HIGH/CRITICAL catalyst overrides the model's
		// own verdict — news may only block an entry, never create one.
		if decision.NewsCatalyst.BlocksEntry() {
			vetoReason := fmt.Sprintf("AI news veto [%s/%s]: %s",
				decision.NewsCatalyst.Type, decision.NewsCatalyst.Severity, decision.NewsCatalyst.Summary)
			_, _ = worker.db.Exec(ctx, `
				UPDATE autogrid_candidates
				SET decision = 'REJECTED',
				    rejection_reason = $3,
				    model_assumptions = model_assumptions || jsonb_build_object('llmCatalyst', $4::JSONB)
				WHERE id = $1 AND scan_id = $2
			`, candidate.ID, scanID, vetoReason, decision.NewsCatalyst)
			worker.logger.Info("Candidate vetoed by news catalyst",
				"symbol", candidate.Symbol, "type", decision.NewsCatalyst.Type,
				"severity", decision.NewsCatalyst.Severity)
			continue
		}

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
