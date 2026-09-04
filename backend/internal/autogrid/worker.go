package autogrid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
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

// realFundingReconcileInterval bounds the funding-fee history fetch for REAL
// bots: funding settles at most every 8h, so a 30-minute anchor keeps each
// window tiny while capping the signed-endpoint weight per manage pass.
const realFundingReconcileInterval = 30 * time.Minute

// protectiveCloseExemptReasons is the single source of truth for close
// reasons that must NOT arm the circuit breaker or the per-symbol cooldown.
// Every operator-driven stop path must be listed here: CloseAllActiveBots
// writes the status string itself ('STOPPED'/'EMERGENCY_STOPPED' from
// SetStatus, 'AUTOGRID_STOP'/'EMERGENCY_STOP' from the HTTP handlers), and
// manual closes write 'MANUAL_CLOSE'/'MCP_MANUAL_CLOSE'. A missing entry
// turns a routine fleet stop into N "protective closes" that freeze deploys
// for an hour plus 2h per-symbol cooldowns.
const protectiveCloseExemptReasons = `'TAKE_PROFIT', 'TAKE_PROFIT_NATIVE', 'TRAILING_TAKE_PROFIT', 'BREAKEVEN_LOCK',
	'RANGE_BREAK_UP_PROFIT_TAKE',
	'MANUAL_CLOSE', 'MCP_MANUAL_CLOSE', 'USER_CANCEL', 'ALREADY_CLOSED', 'EXTERNAL_CLOSE', 'REMOTE_FAILED',
	'STOPPED', 'EMERGENCY_STOPPED', 'AUTOGRID_STOP', 'EMERGENCY_STOP',
	'DELISTED_NO_PRICE'`

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
	// betaRegime caches the BTC market regime behind the v2.0.21 beta
	// gate (same single-goroutine argument as trancheTBRegime).
	betaRegime betaRegimeCache
	// dataAlarmAt dedups data-health alarms to one per feed per 24h
	// (same single-goroutine argument as trancheTBRegime).
	dataAlarmAt map[string]time.Time
}

type trancheTBTrend struct {
	checkedAt time.Time
	trending  bool
}

type betaRegimeCache struct {
	checkedAt time.Time
	regime    string
	adx       float64
	emaSlope  float64
}

type queuedCommand struct {
	ID          string
	CommandType string
	ActorID     *string
	Arguments   map[string]any
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
		dataAlarmAt:     make(map[string]time.Time),
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
	// Drift self-heal: an operator editing N/budget/leverage directly in the
	// DB bypasses Service.UpdateSettings; re-derive the AUTO breaker once at
	// startup so the fleet design and the risk engine never disagree for a
	// whole process lifetime.
	worker.runGuarded("derived_breaker", func() {
		settings, err := worker.service.GetSettings(ctx)
		if err != nil {
			worker.logger.Error("derived breaker: load settings failed",
				"component", "autogrid_worker", "error", err)
			return
		}
		worker.service.SyncDerivedBreaker(ctx, *settings)
	})
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
		RETURNING command.id, command.command_type, command.actor_id, command.arguments
	`, worker.owner).Scan(&command.ID, &command.CommandType, &command.ActorID, &command.Arguments)
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
	// v2.0.21 cascade-triggered scan: an out-of-turn scan queued while a
	// long-liquidation cascade runs — shorts are the product of this pass.
	cascadeShort := false
	if command.Arguments != nil {
		if flag, ok := command.Arguments["cascadeShort"].(bool); ok {
			cascadeShort = flag
		}
	}
	requestedBy := ""
	if command.ActorID != nil {
		requestedBy = *command.ActorID
	}
	scanID, err := worker.service.BeginScan(ctx, settings.ID, requestedBy)
	if err != nil {
		return "", err
	}
	scanConfig := worker.service.scannerConfig(*settings)
	scanConfig.CascadeShortMode = cascadeShort
	candidates, err := worker.scanner.ScanMarkets(ctx, scanConfig)
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
			if err := worker.deployPaper(ctx, *settings, scanID, cascadeShort); err != nil {
				return scanID, err
			}
		} else {
			if err := worker.deployReal(ctx, *settings, scanID, cascadeShort); err != nil {
				return scanID, err
			}
		}
	}
	return scanID, nil
}

// hydrateCandidatesFunding stamps the latest funding rate on every scanned
// candidate in one batched query, so the UI column, the persisted audit
// trail and the deploy gates all see the same number. Cross-exchange
// average first, then the Pionex-native rate overlays it (v2.0.58 F6) —
// Pionex-exclusive listings the collector can never cover finally get a
// real number instead of a nil that silently disarms the flush gate and
// the smart-direction funding context.
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
		funding = map[string]*marketdata.FundingInfo{}
	}
	native := worker.nativeFundingRates(ctx)
	hydrated := 0
	for i := range candidates {
		sym := candidates[i].Symbol
		var rate *decimal.Decimal
		extreme := false
		if n, ok := native[sym]; ok {
			r := n
			rate = &r
			extreme = n.Abs().GreaterThan(decimal.NewFromFloat(0.001))
		} else if info := funding[sym]; info != nil {
			r := decimal.NewFromFloat(info.AverageRate)
			rate = &r
			extreme = info.IsExtreme
		}
		if rate == nil {
			continue
		}
		candidates[i].FundingRate = rate
		if candidates[i].ModelAssumptions != nil {
			candidates[i].ModelAssumptions["fundingIncluded"] = true
			candidates[i].ModelAssumptions["fundingExtreme"] = extreme
		}
		hydrated++
	}
	if hydrated > 0 {
		worker.logger.Info("funding hydrated on candidates",
			"component", "autogrid_worker", "hydrated", hydrated, "total", len(candidates))
	}
}

// nativeFundingRates returns the venue-authoritative next 8h funding rate
// per symbol (fraction) from Pionex's public /market/indexes — one call for
// the whole universe, covering Pionex-exclusive listings the cross-exchange
// collector can never see. Best-effort: an empty map keeps prior behavior.
func (worker *Worker) nativeFundingRates(ctx context.Context) map[string]decimal.Decimal {
	rates := make(map[string]decimal.Decimal, 512)
	indexes, err := worker.publicClient.GetIndexes(ctx, "")
	if err != nil {
		return rates
	}
	for _, idx := range indexes {
		if !idx.NextFundingRate.IsZero() {
			rates[strings.ToUpper(strings.TrimSpace(idx.Symbol))] = idx.NextFundingRate
		}
	}
	return rates
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

// candidateConfluenceVerdict reads the persisted confluence verdict from
// model_assumptions (NEUTRAL when absent).
func candidateConfluenceVerdict(assumptions map[string]any) string {
	if confMap, ok := assumptions["confluence"].(map[string]any); ok {
		if v, ok := confMap["verdict"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "NEUTRAL"
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
// scaled to the budget and the FINAL leverage (v2.0.19: the PnL model marks
// directional positions on budget×leverage notional — unscaled stops died in
// noise, prod SKHY #328); FIXED mode returns the operator's amounts verbatim.
// rangeSpanPct is the deployed mesh span in % — it floors the stop-out above
// a full normal traverse (v2.0.24); 0 skips the floor.
func computeBotTargets(settings Settings, candidate Candidate, leverage int, rangeSpanPct float64) (*decimal.Decimal, *decimal.Decimal) {
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
		Leverage:             leverage,
		AIVolatilityPct:      aiVol,
		AIDrawdownPct:        aiDD,
		ScannerVolatilityPct: vol,
		ScannerATRPct:        atr,
		ScannerDrawdownPct:   drawdown,
		RangeSpanPct:         rangeSpanPct,
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
// - NEUTRAL: strictly within central healthy channel (25% to 75%), avoiding boundary traps.
// - LONG: Golden Pocket pullback, or lower accumulation zone (10% to 60%, extended to 72% with MACD/StochRSI momentum).
// - SHORT: Golden Pocket relief rally, or upper distribution zone (40% to 90%, extended to 28% with MACD/StochRSI momentum).
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

	fibInPocket := false
	var macdUp, macdDown, stochUp, stochDown bool
	var srNearSup, srNearRes float64
	if confMap, ok := candidate.ModelAssumptions["confluence"].(map[string]any); ok {
		if v, ok := confMap["fibInGoldenPocket"].(bool); ok {
			fibInPocket = v
		}
		if v, ok := confMap["macdCrossedUp"].(bool); ok {
			macdUp = v
		}
		if v, ok := confMap["macdCrossedDown"].(bool); ok {
			macdDown = v
		}
		if v, ok := confMap["stochCrossedUp"].(bool); ok {
			stochUp = v
		}
		if v, ok := confMap["stochCrossedDown"].(bool); ok {
			stochDown = v
		}
		if v, ok := confMap["srNearestSupport"].(float64); ok {
			srNearSup = v
		}
		if v, ok := confMap["srNearestResist"].(float64); ok {
			srNearRes = v
		}
	}

	switch candidate.RecommendedTrend {
	case "long":
		// Golden Pocket entry is top-tier priority
		if fibInPocket {
			return true
		}
		// S/R wall check: don't buy directly under an immediate resistance wall (< 0.4% above)
		if srNearRes > 0 && candidate.CurrentPrice.GreaterThan(decimal.Zero) {
			currP, _ := candidate.CurrentPrice.Float64()
			if currP > 0 && (srNearRes-currP)/currP*100 < 0.4 {
				return false
			}
		}
		// Momentum confirmation allows upper channel entries (up to 72%)
		if macdUp || stochUp {
			return rangePos >= 10.0 && rangePos <= 72.0
		}
		// Normal accumulation & pullback zone: 10% to 60% of channel
		return rangePos >= 10.0 && rangePos <= 60.0

	case "short":
		// Golden Pocket relief bounce is top-tier priority
		if fibInPocket {
			return true
		}
		// S/R floor check: don't short directly above an immediate support floor (< 0.4% below)
		if srNearSup > 0 && candidate.CurrentPrice.GreaterThan(decimal.Zero) {
			currP, _ := candidate.CurrentPrice.Float64()
			if currP > 0 && (currP-srNearSup)/currP*100 < 0.4 {
				return false
			}
		}
		// Momentum confirmation allows lower channel entries (down to 28%)
		if macdDown || stochDown {
			return rangePos >= 28.0 && rangePos <= 90.0
		}
		// Normal distribution & relief zone: 40% to 90% of channel
		return rangePos >= 40.0 && rangePos <= 90.0

	default:
		// Neutral range: central healthy channel (25% to 75%), avoiding boundary traps
		return rangePos >= 25.0 && rangePos <= 75.0
	}
}

func (worker *Worker) deployPaper(
	ctx context.Context,
	settings Settings,
	scanID string,
	cascadeShort bool,
) error {
	candidates, err := worker.service.listCandidates(ctx, scanID)
	if err != nil {
		return err
	}
	// Macro context (CoinGecko): loaded once per deploy round — the beta /
	// alt-drain vetoes below share the same reading.
	macroCtx := worker.loadMacroContext(ctx)
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
	// v2.0.19: a LONG-side cascade (forced long unwinding) pauses LONG and
	// NEUTRAL entries but not SHORT — the unwind window is precisely when
	// short participation pays, and the detector itself is already scoped to
	// side='long' (v2.0.14). The per-candidate cut happens after the final
	// direction (smart override included) is known below.
	cascadeLong, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000)
	if cascadeLong {
		worker.logger.Warn("liquidation cascade: LONG/NEUTRAL paper deploys paused, SHORT stay live",
			"component", "autogrid_worker", "usd_1h", cascadeUSD)
		worker.noteDeployBlock(ctx, fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL деплои на паузе, SHORT доступны", cascadeUSD/1_000_000))
	}
	fng, _ := worker.GetFearGreed(ctx)
	// v2.0.21 global beta gate: BTC's tape gates altcoin NEUTRAL/LONG
	// entries — the 2026-08-20 stops were local-RANGE alts loading long
	// inventory into a market-wide bleed.
	betaName, betaADX, betaSlope := worker.marketBetaRegime(ctx)
	betaDown, betaUp := betaGateTrend(betaName, betaADX, betaSlope)
	backtestGateOn := worker.backtestGateEnabled(ctx)
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		if !isEntryTimingFavorable(candidate) {
			worker.rejectCandidate(ctx, candidate,
				"вход-тайминг: текущая позиция в канале вне благоприятной зоны для этого направления", nil)
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
		// scanner's default neutral. v2.0.15: SelectDirection now ALWAYS
		// runs — with a zero FundingContext when coverage is missing — so
		// sentiment gates (euphoria/panic) can no longer be bypassed by a
		// funding-data gap, and the fallback is governed (2x clamp) instead
		// of the raw scanner trend with the adaptive ladder.
		smartTrend, smartLev, smartReason := "", 0, ""
		fundingKnown := false
		fundingInput := FundingContext{}
		if fundingCtx, fundErr := worker.GetFundingForSymbol(ctx, candidate.Symbol); fundErr == nil {
			fundingInput = FundingContext{
				AverageRate: fundingCtx.AverageRate,
				IsExtreme:   fundingCtx.IsExtreme,
			}
			fundingKnown = true
		}
		// v2.0.21 carry picture: 48h stable funding turns RANGE into a
		// paid-to-hold directional pick (paper mirror of the REAL path).
		fundingInput.Avg48h, _, fundingInput.Stable48h = worker.fundingStats48h(ctx, candidate.Symbol)
		smart := SelectDirection(
			RegimeContext{
				Regime:     regime,
				Confidence: confluenceConfidence(candidate.ModelAssumptions),
				HurstValue: candidateHurst(candidate.ModelAssumptions),
			},
			fundingInput,
			EventContext{FearGreedExtreme: fng, LiquidationCascade: cascadeShort},
		)
		if smart.Direction == "WAIT" || smart.Direction == "CLOSE_ALL" {
			worker.rejectCandidate(ctx, candidate, "smart direction: "+smart.Reason, nil)
			worker.logger.Info("v2.0 smart direction: skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"reason", smart.Reason)
			continue
		}
		// v2.0.19: a directional smart pick is a funding-informed conviction
		// (squeeze fuel, crowded carry). With NO funding coverage at all —
		// Pionex-exclusive listings the cross-exchange collector never sees —
		// that conviction is blind; the scanner's own anti-FOMO-vetted trend
		// governs instead of the override (prod: SKHY #328).
		if fundingKnown || smart.Direction == "NEUTRAL" {
			smartTrend = strings.ToLower(smart.Direction)
			smartLev = smart.Leverage
			smartReason = smart.Reason
		} else {
			worker.logger.Info("smart direction needs funding context; scanner trend governs",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"smart", smart.Direction)
		}
		// v2.0.15 demotion guard: the scanner demotes counter-tape trends
		// (24h ±3% against the direction) to no_trend, but the regime string
		// survives — SelectDirection would re-arm the demoted direction and
		// the override below would deploy against the tape the guard exists
		// to filter. A directional smart pick over a demoted scanner trend
		// is a skip, not an override.
		if (smartTrend == "long" || smartTrend == "short") &&
			(strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)) == "no_trend" ||
				strings.TrimSpace(candidate.RecommendedTrend) == "") {
			worker.logger.Info("smart direction contradicts demoted scanner trend: skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"smart", smartTrend, "scanner", candidate.RecommendedTrend)
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("smart direction (%s) против демотированного тренда сканера (no_trend после 24h ±3%% против направления)", smartTrend), nil)
			continue
		}

		// Entry gate (v2.0.12): funding extreme + falling OI = forced
		// deleveraging in progress. This is the CAUSE of falling knives —
		// RSI/ADX/Hurst all lag a fresh dump. Block only while the flush
		// runs; once OI stabilizes the gate lifts on its own.
		// v2.0.21: in a cascade-short window the flush is exactly the short
		// entry — the gate would block the trade it exists to enable.
		flushBlocked, flushWhy := worker.fundingFlushBlocked(ctx, candidate.Symbol, candidate.FundingRate)
		scannerShort := strings.EqualFold(strings.TrimSpace(candidate.RecommendedTrend), "short")
		if flushBlocked && !(cascadeShort && scannerShort) {
			worker.logger.Info("entry gate: funding flush in progress, skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", flushWhy)
			worker.rejectCandidate(ctx, candidate, "entry gate: "+flushWhy, nil)
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
						quote_investment * leverage * ($4::NUMERIC / entry_price - 1)
					WHEN direction = 'SHORT' THEN
						quote_investment * leverage * (1 - $4::NUMERIC / entry_price)
					ELSE 0
				END,
			    updated_at = NOW()
			WHERE settings_id = $1 AND symbol = $2 AND status = 'RUNNING'
		`, settings.ID, candidate.Symbol, candidate.ID, candidate.CurrentPrice)
		if err != nil {
			return fmt.Errorf("mark paper grid %s: %w", candidate.Symbol, err)
		}
		// A skip here used to be silent: the candidate stayed ACCEPTED with
		// no reason, invisible to telemetry and to the shadow portfolio. Both
		// branches now leave an honest rejection — which also feeds the shadow
		// capture the counterfactual "what the candidate would have done had
		// the fleet not been full / the symbol not been taken".
		if tag.RowsAffected() > 0 {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("символ уже в работе: RUNNING-бот по %s обновлён (mark), повторный деплой не нужен", candidate.Symbol), nil)
			continue
		}
		if activeCount >= settings.MaxActiveBots {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("портфель полон (%d/%d) — слот занят, вход отложен до освобождения", activeCount, settings.MaxActiveBots), nil)
			continue
		}
		// Cooldown with escalation (v2.0.28): no re-entry within the window
		// of ANY protective close (stop-loss, structural invalidation, range
		// break); each additional protective close in the trailing 24h
		// DOUBLES the window (2h → 4h → 8h → 24h cap) — a pair that keeps
		// dying on the same tape must stay out longer than one that died
		// once. Only take-profit exits redeploy immediately.
		var protectiveCloses int
		var lastProtectiveAt *time.Time
		if err := worker.db.QueryRow(ctx, `
			SELECT COUNT(*), MAX(closed_at)
			FROM paper_grid_bots
			WHERE settings_id = $1 AND symbol = $2
			  AND status = 'COMPLETED'
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND closed_at > NOW() - INTERVAL '24 hours'
		`, settings.ID, candidate.Symbol).Scan(&protectiveCloses, &lastProtectiveAt); err == nil &&
			protectiveCloses > 0 && lastProtectiveAt != nil {
			window := time.Duration(cooldownHours(protectiveCloses)) * time.Hour
			if time.Since(*lastProtectiveAt) < window {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("cooldown: %d защитных закрытий за 24ч, окно %s с последнего — повторный вход отложен",
						protectiveCloses, window), nil)
				continue
			}
		}
		// v2.0.27 sector cap: correlated clusters stop together — 6 of 10
		// bots were semis/AI-correlated on 2026-08-21 with no cap anywhere;
		// one −5% semis day could fire 6-8 protective closes at once.
		if sector := sectorForSymbol(candidate.Symbol); sector != "" &&
			worker.sectorBotCount(ctx, settings.ID, sector) >= maxBotsPerSector {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("sector cap: в секторе %s уже %d ботов — коррелированный кластер, вход отложен", sector, maxBotsPerSector), nil)
			continue
		}
		// Entry gate (v2.0.12): anchor to the LIVE price. The scan price is
		// captured at scan start and ages through AI Kit calls, LLM audits
		// and backtest waits; deploying a grid centered minutes in the past
		// is how bots open already outside their range (ENSO class). A drift
		// beyond half an ATR means the candidate itself is stale — except in
		// the cascade-short window, where a price flying >0.5 ATR between
		// scan and deploy is the very move the cascade scan exists to short:
		// the DOM, funding-flush and macro gates are already cascade-exempt,
		// and this one would void every candidate precisely in that window.
		// The live price still re-anchors the candidate (fresh > 0); only an
		// UNREADABLE price (fresh zero) stays fail-closed even in cascade —
		// a grid centered on an unreadable tape is the stale-anchor bug
		// itself.
		if freshPrice, ok := worker.revalidateFreshPrice(ctx, &candidate, atrPct); ok || (cascadeShort && freshPrice.IsPositive()) {
			candidate.CurrentPrice = freshPrice
		} else {
			worker.logger.Info("entry gate: stale candidate price, skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"scan_price", candidate.CurrentPrice.String(), "live_price", freshPrice.String())
			worker.rejectCandidate(ctx, candidate,
				"entry gate: цена кандидата устарела (дрейф > 0.5 ATR с момента скана)", nil)
			continue
		}
		// Order-book (DOM) gate (v2.0.39): a one-sided book against the
		// entry direction vetoes the deploy — the tape's own inventory is
		// the cheapest real-time confirmation available. Fail-open on
		// transport errors (advisory signal); the reading is recorded in
		// the rejection/acceptance telemetry so the closed ledger can
		// validate the threshold like every other gate.
		if bids, asks, derr := worker.publicClient.GetDepth(ctx, candidate.Symbol, 50); derr == nil &&
			len(bids) > 0 && len(asks) > 0 && candidate.CurrentPrice.IsPositive() {
			imbalance := depthImbalance(bids, asks, candidate.CurrentPrice, 1.5)
			scannerTrend := strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))
			againstEntry := (scannerTrend != "short" && imbalance < 0.25) || (scannerTrend == "short" && imbalance > 0.75)
			if againstEntry && !cascadeShort {
				side := "аски доминируют (продавцы)"
				if scannerTrend == "short" {
					side = "биды доминируют (покупатели)"
				}
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("стакан: дисбаланс %.2f против направления — %s, вход отложен", imbalance, side),
					map[string]any{"depthGate": map[string]any{"imbalance": imbalance, "vetoed": true}})
				continue
			}
		}
		// Macro gate (v2.0.44, CoinGecko): beta-drift (BTC 24h ≤ −3%) and
		// alt-drain (dominance +0.35pp over 24h on flat BTC) veto non-short
		// entries. Pair-level ADX/Hurst cannot see market-wide rotation —
		// the 2026-08-30 night (BTC flat, alts −8..−37%) killed 8 NEUTRAL
		// bots for −$56.71 with every scanner-level gate green. Shorts and
		// cascade are exempt; fail-open until ~24h of snapshots exist.
		if veto, reason, macroTel := macroVeto(strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)), cascadeShort, macroCtx); veto {
			worker.rejectCandidate(ctx, candidate, reason, macroTel)
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
		// halves the damage of every un-timed entry. v2.0.27: PAPER sizes
		// the LEVEL COUNT against the FULL slot budget — the time-box
		// commits tranche 2 within 24h anyway, so steady state is the full
		// budget, and tranche-1-sized levels ($5 cap on $100) pinned
		// wide-span bots at 20 levels where the slot carries 40, halving
		// feasible crossings (fleet audit 2026-08-21). REAL keeps
		// tranche-sized geometry: its exchange create must satisfy
		// min-order at the actually-committed amount.
		investAmount := settings.BudgetUSDT
		trancheOn := settings.TrancheDeployEnabled
		if trancheOn {
			investAmount = settings.BudgetUSDT.Div(decimal.NewFromInt(2))
		}
		geometryBudget := settings.BudgetUSDT
		harGeo := worker.harGridGeometry(ctx, candidate.Symbol, decimalFloat(settings.FeeBps.Add(settings.SlippageBps)), geometryBudget.InexactFloat64())
		mesh := ComputeAdaptiveMesh(
			candidate.LowerPrice, candidate.UpperPrice, candidate.CurrentPrice,
			atrPct, regime, geometryBudget, settings.Leverage, 0.30,
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
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("entry gate: расширение волатильности (RV/базлайн %.2f ≥ 1.5) — вход в ускорение заблокирован", math.Round(ratio*100)/100), nil)
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
			// v2.0.45: the override re-reads the regime AFTER the scanner's
			// direction vetoes already ran — an override INTO neutral used to
			// skip every neutral-specific guard (2026-08-30: SPX entered
			// NEUTRAL via override at scan ADX 34.1 while the scanner itself
			// said short; it died −$16.32). Re-apply the scanner's neutral
			// trend-strength ceiling to overridden entries.
			if smartTrend == "neutral" && trend != "neutral" && trend != "no_trend" && trend != "" {
				if adxVal, ok := candidate.ModelAssumptions["adx"].(float64); ok && adxVal > 32.0 {
					worker.rejectCandidate(ctx, candidate,
						fmt.Sprintf("smart override → NEUTRAL при ADX сканера %.1f > 32 — тренд слишком силён для нейтральной сетки, вето сканера восстановлено", adxVal),
						map[string]any{"overrideNeutralHole": map[string]any{"scanAdx": adxVal, "scannerTrend": trend}})
					continue
				}
			}
			trend = smartTrend
		}
		if cascadeLong && trend != "short" {
			worker.rejectCandidate(ctx, candidate, fmt.Sprintf(
				"каскад ликвидаций лонгов $%.0fM/час — входы LONG/NEUTRAL на паузе (SHORT доступны)",
				cascadeUSD/1_000_000), nil)
			continue
		}
		// v2.0.21 cascade-short window: this out-of-turn scan exists to
		// deploy shorts into the forced unwind — everything else waits for
		// the regular scheduler.
		if cascadeShort && trend != "short" {
			worker.rejectCandidate(ctx, candidate,
				"каскад-триггер: внеочередной скан деплоит только SHORT-кандидаты", nil)
			continue
		}
		// v2.0.21 beta gate.
		if betaDown && trend != "short" {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("beta gate: BTC %s (ADX %.0f, slope %.2f%%) — NEUTRAL/LONG деплои на паузе, SHORT доступны",
					betaName, betaADX, betaSlope), nil)
			continue
		}
		if betaUp && trend == "short" {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("beta gate: BTC %s (ADX %.0f, slope +%.2f%%) — SHORT деплой против растущего рынка на паузе",
					betaName, betaADX, betaSlope), nil)
			continue
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
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("анти-хант: цена %s слишком близко к стопу %s (< 1.5 ATR запаса) — риск мгновенного STRUCT_INVALID",
						candidate.CurrentPrice.String(), antiHuntStop.String()), nil)
				continue
			}
		} else {
			if antiHuntStop.Sub(candidate.CurrentPrice).LessThan(atrPrice.Mul(decimal.NewFromFloat(1.5))) {
				worker.logger.Info("skip paper deploy: price too close to anti-hunt stop",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"price", candidate.CurrentPrice.String(), "stop", antiHuntStop.String())
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("анти-хант: цена %s слишком близко к стопу %s (< 1.5 ATR запаса) — риск мгновенного STRUCT_INVALID",
						candidate.CurrentPrice.String(), antiHuntStop.String()), nil)
				continue
			}
		}

		// Leverage precedence: Operator base leverage scaled adaptively by volatility (ATR)
		baseLev := settings.Leverage
		if baseLev <= 0 {
			baseLev = 3
		}
		botLev := baseLev
		levReason := fmt.Sprintf("Базовое (%dx)", baseLev)
		levMode := "BASE"
		if settings.AdaptiveLeverageEnabled {
			// v2.0.56 (F1): judge the span the bot actually trades. HAR's
			// applyToMesh has already rewritten the bounds by here, so the
			// candidate S/R span is stale: scanner-narrow candidates widened
			// to 16-25% meshes were de-geared like narrow grids (checkpoint
			// 2026-09-01: SKYAI/GIGGLE 2x on wide meshes) while targets below
			// already scale off the post-HAR span.
			spanPct := 0.0
			if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && candidate.CurrentPrice.IsPositive() {
				spanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
			}
			if spanPct <= 0 {
				spanPct = candidateSpanPct(candidate.LowerPrice, candidate.UpperPrice)
			}
			dyn := ComputeDynamicLeverage(atrPct, baseLev, spanPct)
			botLev = dyn.Leverage
			levReason = dyn.Reason
			levMode = "ADAPTIVE"
		} else if smartLev > 0 && smartLev < botLev {
			botLev = smartLev
			levReason = fmt.Sprintf("Smart Direction (%dx): %s", smartLev, smartReason)
			levMode = "SMART"
		} else if harGeo != nil && harGeo.geo.Leverage < botLev {
			botLev = harGeo.geo.Leverage
			levReason = fmt.Sprintf("HAR σ=%.0f%%/год R²=%.2f (%dx)",
				harGeo.forecastPct, harGeo.geo.Confidence, botLev)
			levMode = "HAR"
		}

		// v2.0.56 (F9): block directional flip-entries on a symbol that ran
		// another direction <12h ago. The cascade-short window is the
		// designed escape valve and stays exempt.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") &&
			worker.directionalFlipBlocked(ctx, candidate.Symbol, strings.ToUpper(trend), true) {
			worker.rejectCandidate(ctx, candidate,
				"флип направления: символ закрыл бота другого направления ≤12ч назад — направленный вход отложен", nil)
			continue
		}

		// v2.0.62 (R1): a DIRECTIONAL entry needs directional confirmation.
		// The 14d ledger: every big directional stop (SPX/SNXXX SHORT, WLD
		// LONG, JUP SHORT) carried confluence verdict=NEUTRAL — the engine
		// went directional while its own confluence stayed agnostic.
		// 1W/4L for directional entries, 69% of all losses. Cascade-shorts
		// remain the designed exemption.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") {
			verdict := candidateConfluenceVerdict(candidate.ModelAssumptions)
			want := "SUPPORT_SHORT"
			if trend == "long" {
				want = "SUPPORT_LONG"
			}
			if verdict != want {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("R1: направленный вход (%s) без направленного подтверждения — confluence verdict %s, требуется %s",
						strings.ToUpper(trend), verdict, want), nil)
				continue
			}
		}

		confluence := EvaluateConfluence(candidate, nil, nil)

		gridType := mesh.GridType
		meshSpanPct := 0.0
		if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && candidate.CurrentPrice.IsPositive() {
			meshSpanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
		}
		target, maxLoss := computeBotTargets(settings, candidate, botLev, meshSpanPct)
		// The envelope gate below reserves the candidate's FULL
		// (post-tranche-2) stop — the exact amount tranche2RiskGate later
		// re-doubles the stored half to — so it must be captured BEFORE the
		// tranche-1 halving. Reserving only the half admitted fleets that
		// converge to the envelope ceiling where every newborn's tranche-2 is
		// skipped until some bot dies (prod 2026-09-02: OP skip 15:02, ASTER
		// dead 15:13, OP tranche 15:14).
		candidateFullStop := decimal.Zero
		if maxLoss != nil {
			candidateFullStop = *maxLoss
		}
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

		// v2.0.27: PAPER runs the same durable risk exam as REAL — kill
		// switch, MaxLeverage, notional exposure caps and the daily-loss
		// breaker used to bypass paper entirely (prod CRWVX #366 deployed
		// at 4x with no check). Requested notional uses the FULL slot
		// budget: the 24h tranche time-box commits the second half anyway.
		if err := worker.risk.ValidateNewPaperGrid(ctx, candidate.Symbol, botLev, settings.BudgetUSDT); err != nil {
			worker.rejectCandidate(ctx, candidate, "risk engine: "+err.Error(), nil)
			worker.logger.Info("paper deploy blocked by risk engine",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "error", err)
			continue
		}

		// Fleet stop envelope: the RUNNING fleet's stored stops plus this
		// candidate's FULL (post-tranche-2) stop must stay under 0.8× the
		// daily-loss breaker. The reservation equals the amount tranche-2
		// will later double the stored half to, so a bot is never born into
		// a fleet that cannot fit its own doubling. A nil stop contributes
		// nothing to the envelope either way, so the gate only arms when
		// the candidate carries one.
		if maxLoss != nil {
			if reason := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateFullStop); reason != "" {
				worker.rejectCandidate(ctx, candidate, reason, nil)
				worker.logger.Info("paper deploy blocked by fleet stop envelope",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
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
					'entryFeatures', $25::JSONB,
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
			trancheFlag(trancheOn), settings.BudgetUSDT.String(), atrPct, entryFeaturesJSON(candidate)).Scan(&botID, &botNumber)
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
	// F10: capture the scan's top-scored rejections for the shadow
	// portfolio — after the deploy loop, off the hot path, fail-open.
	worker.captureShadowCandidates(ctx, settings, scanID)
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
	cascadeShort bool,
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
	cascadeLong, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000)
	if cascadeLong {
		worker.logger.Warn("liquidation cascade: LONG/NEUTRAL real deploys paused, SHORT stay live",
			"component", "autogrid_worker", "usd_1h", cascadeUSD)
		worker.noteDeployBlock(ctx, fmt.Sprintf("REAL: каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL деплои на паузе, SHORT доступны", cascadeUSD/1_000_000))
	}
	// When the LLM brain is enabled, an UNAUDITED candidate is not
	// deployable — regardless of why the audit is missing (beyond the
	// per-scan audit cap, transport failure, timeout). This is the hard
	// structural guarantee that closes the top-5 bypass.
	llmBrainEnabled := false
	if llmSettings, err := worker.llm.GetSettings(ctx); err == nil {
		llmBrainEnabled = llmSettings.Enabled && strings.TrimSpace(llmSettings.APIKey) != ""
	}
	// v2.0.58 (audit 2026-09-01): deployReal never ran the macro gate —
	// beta-drift/alt-drain vetoes protected paper only, the exact paths
	// REAL capital would ride. Same context load as the paper round.
	macroCtx := worker.loadMacroContext(ctx)
	for _, candidate := range candidates {
		// Non-ACCEPTED rows already carry their scanner-time rejection reason.
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		// Invariant: every deploy-loop skip must leave a reason on the
		// candidate row. A silent `continue` here kept the fleet "stuck" for
		// hours with every candidate ACCEPTED and no explanation (prod
		// 2026-09-03: 5/5 slots taken). Text mirrors the paper path (v2.0.66)
		// so analytics need not distinguish the source fleet.
		if activeCount >= settings.MaxActiveBots {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("портфель полон (%d/%d) — слот занят, вход отложен до освобождения", activeCount, settings.MaxActiveBots), nil)
			continue
		}
		// Macro gate (REAL mirror of the paper path): market-wide rotation
		// is invisible to pair-level ADX/Hurst — the 2026-08-30 night class.
		if veto, reason, macroTel := macroVeto(strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)), cascadeShort, macroCtx); veto {
			worker.rejectCandidate(ctx, candidate, reason, macroTel)
			continue
		}
		if !isEntryTimingFavorable(candidate) {
			worker.rejectCandidate(ctx, candidate,
				"вход-тайминг: текущая позиция в канале вне благоприятной зоны для этого направления", nil)
			continue
		}
		if llmBrainEnabled && candidate.ModelAssumptions["llmAuditId"] == nil {
			worker.logger.Warn("skip real deploy: no completed LLM audit for candidate",
				"component", "autogrid_worker", "symbol", candidate.Symbol)
			worker.rejectCandidate(ctx, candidate, "AI-аудит не завершён для этого кандидата (кап аудита/сбой LLM) — деплой заблокирован", nil)
			continue
		}
		// Entry gate (v2.0.12): funding extreme + falling OI = forced
		// deleveraging in progress (mirror of the paper path; v2.0.21
		// cascade-short exemption for scanner-shorts, same as paper).
		flushBlockedReal, flushWhyReal := worker.fundingFlushBlocked(ctx, candidate.Symbol, candidate.FundingRate)
		scannerShortReal := strings.EqualFold(strings.TrimSpace(candidate.RecommendedTrend), "short")
		if flushBlockedReal && !(cascadeShort && scannerShortReal) {
			worker.logger.Info("entry gate: funding flush in progress, skip real deploy",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", flushWhyReal)
			worker.rejectCandidate(ctx, candidate, "entry gate: "+flushWhyReal, nil)
			continue
		}
		if ok, reason := worker.revalidateCandidateTrend(ctx, &candidate, settings); !ok {
			worker.logger.Info("skip real deploy after fresh trend revalidation",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", reason)
			worker.rejectCandidate(ctx, candidate, "ре-валидация тренда: "+reason, nil)
			continue
		}

		// v2.0 Smart Direction: regime + funding + FNG → direction override.
		// v2.0.3: feeds the scanner's REAL Hurst (was hardcoded 0.5, which
		// dead-locked RANGE candidates into WAIT), reads the regime safely,
		// and the decision now actually overrides trend + leverage below.
		smartTrend, smartLev, smartReason := "", 0, ""
		fngReal, _ := worker.GetFearGreed(ctx)
		betaNameReal, betaADXReal, betaSlopeReal := worker.marketBetaRegime(ctx)
		betaDownReal, betaUpReal := betaGateTrend(betaNameReal, betaADXReal, betaSlopeReal)
		fundingKnownReal := false
		fundingInput := FundingContext{}
		if fundingCtx, fundErr := worker.GetFundingForSymbol(ctx, candidate.Symbol); fundErr == nil {
			fundingInput = FundingContext{
				AverageRate: fundingCtx.AverageRate,
				IsExtreme:   fundingCtx.IsExtreme,
			}
			fundingKnownReal = true
		}
		// v2.0.21 carry picture (REAL mirror).
		fundingInput.Avg48h, _, fundingInput.Stable48h = worker.fundingStats48h(ctx, candidate.Symbol)
		// v2.0.15: always run (zero funding context on gaps) — sentiment
		// gates must not depend on funding-data availability (mirror).
		smartDir := SelectDirection(
			RegimeContext{
				Regime:     candidateRegime(candidate.ModelAssumptions),
				Confidence: confluenceConfidence(candidate.ModelAssumptions),
				HurstValue: candidateHurst(candidate.ModelAssumptions),
			},
			fundingInput,
			EventContext{FearGreedExtreme: fngReal, LiquidationCascade: cascadeShort},
		)
		if smartDir.Direction == "WAIT" || smartDir.Direction == "CLOSE_ALL" {
			worker.rejectCandidate(ctx, candidate, "smart direction: "+smartDir.Reason, nil)
			worker.logger.Info("v2.0 smart direction: skip",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"reason", smartDir.Reason)
			continue
		}
		// v2.0.19 mirror: funding-blind directional picks don't override —
		// see the paper-path comment (prod: SKHY #328).
		if fundingKnownReal || smartDir.Direction == "NEUTRAL" {
			smartTrend = strings.ToLower(smartDir.Direction)
			smartLev = smartDir.Leverage
			smartReason = smartDir.Reason
		} else {
			worker.logger.Info("smart direction needs funding context; scanner trend governs",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"smart", smartDir.Direction)
		}
		// v2.0.15 demotion guard (mirror of the paper path).
		if (smartTrend == "long" || smartTrend == "short") &&
			(strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)) == "no_trend" ||
				strings.TrimSpace(candidate.RecommendedTrend) == "") {
			worker.logger.Info("smart direction contradicts demoted scanner trend: skip real deploy",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"smart", smartTrend, "scanner", candidate.RecommendedTrend)
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("smart direction (%s) против демотированного тренда сканера (no_trend после 24h ±3%% против направления)", smartTrend), nil)
			continue
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
		// Same invariant as the slot check: the skip must be visible. The
		// candidate row would otherwise stay ACCEPTED with no reason while a
		// RUNNING grid already harvests the symbol (paper mirror: «символ уже
		// в работе»).
		if exists {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("символ уже в работе: RUNNING-грид по %s активен, повторный деплой не нужен", candidate.Symbol), nil)
			continue
		}
		// Cooldown with escalation (v2.0.28, paper-parity): each additional
		// protective close in the trailing 24h doubles the re-entry window
		// (2h → 4h → 8h → 24h cap); profit takes and operator/exchange-driven
		// closes are exempt.
		var protectiveCloses int
		var lastProtectiveAt *time.Time
		if err := worker.db.QueryRow(ctx, `
			SELECT COUNT(*), MAX(COALESCE(closed_at, updated_at))
			FROM grid_bots
			WHERE account_id = $1 AND symbol = $2
			  AND status IN ('STOPPED', 'LIQUIDATED')
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '24 hours'
		`, *settings.AccountID, candidate.Symbol).Scan(&protectiveCloses, &lastProtectiveAt); err == nil &&
			protectiveCloses > 0 && lastProtectiveAt != nil {
			window := time.Duration(cooldownHours(protectiveCloses)) * time.Hour
			if time.Since(*lastProtectiveAt) < window {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("cooldown: %d защитных закрытий за 24ч, окно %s с последнего — повторный вход отложен",
						protectiveCloses, window), nil)
				continue
			}
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
		// Sizing the grid level count against the full slot budget ($200)
		// enables 30-40 levels (step ~0.55%) with $5/level order capacity,
		// doubling grid crossing captures vs halving levels to 20.
		investAmount := settings.BudgetUSDT
		if settings.TrancheDeployEnabled {
			investAmount = settings.BudgetUSDT.Div(decimal.NewFromInt(2))
		}
		geometryBudget := settings.BudgetUSDT
		mesh := ComputeAdaptiveMesh(
			candidate.LowerPrice, candidate.UpperPrice, candidate.CurrentPrice,
			atrPct, regime, geometryBudget, settings.Leverage, 0.30,
		)

		// v2.0 HAR-RV geometry — the same sizing the paper fleet validates:
		// forecast next-day volatility from daily candles, derive range width
		// / level count / vol-inverse leverage. Falls back to the ATR mesh
		// when history or fit quality is insufficient.
		harGeo := worker.harGridGeometry(ctx, candidate.Symbol, decimalFloat(settings.FeeBps.Add(settings.SlippageBps)), geometryBudget.InexactFloat64())
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
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("entry gate: расширение волатильности (RV/базлайн %.2f ≥ 1.5) — вход в ускорение заблокирован", math.Round(ratio*100)/100), nil)
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
		if cascadeLong && trend != "short" {
			worker.rejectCandidate(ctx, candidate, fmt.Sprintf(
				"каскад ликвидаций лонгов $%.0fM/час — входы LONG/NEUTRAL на паузе (SHORT доступны)",
				cascadeUSD/1_000_000), nil)
			continue
		}
		// v2.0.21 cascade-short window (REAL mirror).
		if cascadeShort && trend != "short" {
			worker.rejectCandidate(ctx, candidate,
				"каскад-триггер: внеочередной скан деплоит только SHORT-кандидаты", nil)
			continue
		}
		// v2.0.56 (F9, REAL mirror): block directional flip-entries on a
		// symbol that ran another direction <12h ago; cascade-shorts exempt.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") &&
			worker.directionalFlipBlocked(ctx, candidate.Symbol, strings.ToUpper(trend), false) {
			worker.rejectCandidate(ctx, candidate,
				"флип направления: символ закрыл бота другого направления ≤12ч назад — направленный вход отложен", nil)
			continue
		}

		// v2.0.62 (R1, REAL mirror): directional entry requires the matching
		// confluence verdict; cascade-shorts exempt.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") {
			verdict := candidateConfluenceVerdict(candidate.ModelAssumptions)
			want := "SUPPORT_SHORT"
			if trend == "long" {
				want = "SUPPORT_LONG"
			}
			if verdict != want {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("R1: направленный вход (%s) без направленного подтверждения — confluence verdict %s, требуется %s",
						strings.ToUpper(trend), verdict, want), nil)
				continue
			}
		}
		// v2.0.21 beta gate (REAL mirror).
		if betaDownReal && trend != "short" {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("beta gate: BTC %s (ADX %.0f, slope %.2f%%) — NEUTRAL/LONG деплои на паузе, SHORT доступны",
					betaNameReal, betaADXReal, betaSlopeReal), nil)
			continue
		}
		if betaUpReal && trend == "short" {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("beta gate: BTC %s (ADX %.0f, slope +%.2f%%) — SHORT деплой против растущего рынка на паузе",
					betaNameReal, betaADXReal, betaSlopeReal), nil)
			continue
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
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("анти-хант: цена %s слишком близко к стопу %s (< 1.5 ATR запаса)",
						candidate.CurrentPrice.String(), antiHuntStop.String()), nil)
				continue
			}
		} else {
			minDistance := atrPrice.Mul(decimal.NewFromFloat(1.5))
			if antiHuntStop.Sub(candidate.CurrentPrice).LessThan(minDistance) {
				deployErrors = append(deployErrors, fmt.Sprintf(
					"%s: price %s too close to anti-hunt stop %s (< 1.5 ATR room) — skipped",
					candidate.Symbol, candidate.CurrentPrice.String(), antiHuntStop.String()))
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("анти-хант: цена %s слишком близко к стопу %s (< 1.5 ATR запаса)",
						candidate.CurrentPrice.String(), antiHuntStop.String()), nil)
				continue
			}
		}

		// Leverage precedence: Operator base leverage scaled adaptively by volatility (ATR)
		baseLev := settings.Leverage
		if baseLev <= 0 {
			baseLev = 3
		}
		botLev := baseLev
		if settings.AdaptiveLeverageEnabled {
			// v2.0.56 (F1): mirror of the paper path — the de-gear span must
			// come from the final mesh (HAR applyToMesh ran above), not the
			// stale candidate S/R bounds.
			spanPct := 0.0
			if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && candidate.CurrentPrice.IsPositive() {
				spanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
			}
			if spanPct <= 0 {
				spanPct = candidateSpanPct(candidate.LowerPrice, candidate.UpperPrice)
			}
			dyn := ComputeDynamicLeverage(atrPct, baseLev, spanPct)
			botLev = dyn.Leverage
		} else if smartLev > 0 && smartLev < botLev {
			botLev = smartLev
		} else if harGeo != nil && harGeo.geo.Leverage < botLev {
			botLev = harGeo.geo.Leverage
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
		botTargetSpan := 0.0
		if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && candidate.CurrentPrice.IsPositive() {
			botTargetSpan, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
		}
		botTarget, botMaxLoss := computeBotTargets(settings, candidate, botLev, botTargetSpan)
		// v2.0.67 parity: the deploy envelope gate reserves the candidate's
		// FULL (post-tranche-2) stop — the amount the top-up later doubles
		// the stored half to. It must be captured BEFORE the tranche-1
		// halving below, mirroring the paper path (v2.0.66).
		candidateFullStop := decimal.Zero
		if botMaxLoss != nil {
			candidateFullStop = *botMaxLoss
		}
		if settings.TrancheDeployEnabled {
			// v2.0.15 (restored — the v2.0.13 patch was lost to a failed
			// batch): tranche 1 commits HALF the capital, so native
			// ProfitStop and the stored per-bot targets must be half too;
			// the invest_in top-up doubles them back.
			if botTarget != nil {
				half := botTarget.Div(decimal.NewFromInt(2))
				botTarget = &half
			}
			if botMaxLoss != nil {
				half := botMaxLoss.Div(decimal.NewFromInt(2))
				botMaxLoss = &half
			}
		}
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
		// v2.0.67 parity: the SAME fleet stop-envelope gate the paper path
		// runs (joint paper+REAL sum, full-stop reservation, 0.8× derived
		// breaker). REAL used to skip it entirely — a REAL deploy could
		// overflow the account's stop envelope while every paper deploy was
		// being refused for it.
		if botMaxLoss != nil {
			if reason := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateFullStop); reason != "" {
				deployErrors = append(deployErrors, fmt.Sprintf("%s: %s", candidate.Symbol, reason))
				worker.logger.Info("real deploy blocked by fleet stop envelope",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
			}
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
			// Exchange-side refusal (403 forbidden/maintenance) is a symbol
			// state, not a parameter problem — the create behind this check
			// would be refused identically, so reject the candidate BEFORE any
			// grid row is submitted instead of re-failing every scan window.
			if isSymbolOperationForbiddenError(checkErr) {
				worker.logger.Info("entry gate: exchange forbids the operation on symbol, real deploy deferred",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				worker.rejectCandidate(ctx, candidate,
					"биржа запрещает операцию по символу (forbidden/maintenance) — деплой отложен", nil)
				continue
			}
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
			// The lifecycle already persisted the FAILED grid row (the
			// authoritative audit of the refused attempt); the candidate row
			// must still leave the refusal reason — otherwise it stays
			// ACCEPTED and the next scan mints yet another FAILED row for the
			// whole maintenance window.
			if isSymbolOperationForbiddenError(createErr) {
				deployErrors = append(deployErrors, fmt.Sprintf("%s: create failed: %v", candidate.Symbol, createErr))
				worker.rejectCandidate(ctx, candidate,
					"биржа запрещает операцию по символу (forbidden/maintenance) — деплой отложен", nil)
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
	// v2.0.45: fleet stops now SETTLE paper bots at the live price (inventory
	// mark − taker+slippage exit fee) instead of freezing their last unrealized
	// mark — a real Pionex cancel settles exactly like any close, and the
	// full-history PnL card sums these rows as final.
	if err := worker.settleAndStopPaperBots(ctx, *settings, "STOPPED", "AUTOGRID_STOP"); err != nil {
		worker.logger.Warn("fleet stop: settle pass failed, falling back to bulk close",
			"component", "autogrid_worker", "error", err)
		if _, err := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET status = 'STOPPED', closed_reason = 'AUTOGRID_STOP', closed_at = NOW(), updated_at = NOW()
			WHERE settings_id = $1 AND status = 'RUNNING'
		`, settings.ID); err != nil {
			return fmt.Errorf("stop paper AutoGrid bots: %w", err)
		}
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
	if err := worker.settleAndStopPaperBots(ctx, *settings, "EMERGENCY_STOPPED", "EMERGENCY_STOP"); err != nil {
		_, _ = worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET status = 'EMERGENCY_STOPPED', closed_reason = 'EMERGENCY_STOP', closed_at = NOW(), updated_at = NOW()
			WHERE settings_id = $1 AND status = 'RUNNING'
		`, settings.ID)
	}
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

// pionexSymbolMaintenanceReason is Pionex's refusal code for grid operations
// on a symbol under exchange-side maintenance: HTTP 403 whose body does not
// parse as an envelope, so the code survives only inside the client's
// "invalid JSON response" snippet (or as the envelope code when it parses).
const pionexSymbolMaintenanceReason = "P_TRADING_BOT_OPERATION_IS_FORBIDDEN_SYMBOL_MAINTENANCE"

// pionexCredentialRefusalMarkers identify a 403 that is about the API
// credentials rather than the symbol: such a refusal is an account/config
// problem that touching another symbol will not dodge, and classifying it
// as a symbol state would silently mask a broken key as "deferred deploys".
var pionexCredentialRefusalMarkers = []string{
	"api_key", "api key", "apikey", "p_api",
	"signature", "unauthorized", "unauthorised",
	"authentication", "permission", "ip_whitelist",
}

// isSymbolOperationForbiddenError reports whether err is the exchange's
// symbol-scoped operation refusal: an HTTP 403 from the futuresGrid
// create/checkParams endpoints whose body names a forbidden operation — the
// maintenance reason code, "IS_FORBIDDEN", "Operation is forbidden" or any
// other "forbidden" fragment in the code/message snippet (Pionex also emits
// truncated non-JSON 403 bodies where the reason only survives inside the
// client's body snippet). Such a refusal is a symbol state that lifts on its
// own — it must defer the deploy, never count as a candidate defect or a
// pipeline failure. Credential refusals are explicitly NOT symbol states.
func isSymbolOperationForbiddenError(err error) bool {
	var apiErr *pionex.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(apiErr.Code + " " + apiErr.Message)
	for _, marker := range pionexCredentialRefusalMarkers {
		if strings.Contains(text, marker) {
			return false
		}
	}
	// Covers the maintenance code, any *_IS_FORBIDDEN_* code and the plain
	// "Operation is forbidden" message in one case-insensitive fragment.
	return strings.Contains(text, "forbidden")
}

// rejectCandidate records a late-stage rejection so the UI shows WHY a
// previously accepted candidate never deployed.
func (worker *Worker) rejectCandidate(
	ctx context.Context, candidate Candidate, reason string, assumptions map[string]any,
) {
	// A nil map marshals to the jsonb scalar `null`, and
	// `model_assumptions || 'null'::jsonb` turns the column into an ARRAY —
	// every reader (listCandidates, GetState) then fails on the whole scan.
	// Normalize to an empty object: `|| '{}'` is a no-op merge.
	if assumptions == nil {
		assumptions = map[string]any{}
	}
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

// dataHealthCheck (v2.0.58) watches the two feeds whose silent death costs
// the most: the economic calendar (the deploy gate goes blind with no
// future events in the table — the faireconomy feed 429'd unnoticed from
// 2026-08-30) and the liquidation stream (the Binance WS topic was
// misnamed for the system's entire history, zero rows ever, cascade gate
// inert). One alarm per feed per 24h; recovered feeds clear silently.
func (worker *Worker) dataHealthCheck(ctx context.Context) {
	alarm := func(key, message string) {
		if last, ok := worker.dataAlarmAt[key]; ok && time.Since(last) < 24*time.Hour {
			return
		}
		worker.dataAlarmAt[key] = time.Now().UTC()
		worker.logger.Warn("data feed stale", "component", "autogrid_worker", "feed", key, "detail", message)
		_ = QueueTelegramEvent(ctx, worker.db, "EMERGENCY", map[string]any{
			"message": message,
		})
	}
	var futureEvents int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM economic_events
		WHERE event_time > NOW() AND (country = 'USD' OR country IS NULL OR country = '')
	`).Scan(&futureEvents); err == nil && futureEvents == 0 {
		alarm("economic_events",
			"Календарь USD пуст: нет будущих событий — эконом-гейт деплоя слеп (фетч ForexFactory мёртв?)")
	}
	var lastLiq *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(captured_at) FROM liquidation_events
	`).Scan(&lastLiq); err == nil &&
		(lastLiq == nil || time.Since(*lastLiq) > 3*time.Hour) {
		alarm("liquidation_events",
			"Ликвидации не пишутся >3ч — каскад-гейт слеп (WS-источник мёртв; см. app_config.liquidation_source)")
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
	// F10: replay matured shadow rows in bounded batches (own due-anchor).
	worker.shadowSimIfDue(ctx, *settings)
	// v2.0.58: data-feed health — a silently dead collector must announce
	// itself instead of fail-opening every gate that leans on it.
	worker.dataHealthCheck(ctx)
	worker.maybeQueueCascadeShortScan(ctx, *settings)
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
		       pnl_target_usdt, max_loss_usdt, quote_investment, leverage,
		       anti_hunt_stop_price, COALESCE(bot_number, 0),
		       COALESCE(peak_pnl_usdt, 0), created_at,
		       COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       NULLIF(model_state->>'trancheBase',''),
		       NULLIF(model_state->>'trancheEntry',''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0),
		       NULLIF(model_state->>'trancheFailAt',''),
		       COALESCE(funding_paid_usdt, 0), last_funding_reconcile_at
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
		rowNum, adjustments, leverage                           int
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
		fundingPaid                                             decimal.Decimal
		lastFundingReconcileAt                                  *time.Time
	}
	bots := make([]managedBot, 0)
	for rows.Next() {
		var item managedBot
		if err := rows.Scan(
			&item.id, &item.accountID, &item.remoteID, &item.localStatus, &item.symbol,
			&item.direction, &item.lower, &item.upper, &item.rowNum,
			&item.adjustments, &item.pnlTarget, &item.maxLoss, &item.investment,
			&item.leverage,
			&item.antiHuntStop, &item.botNumber,
			&item.peak, &item.createdAt,
			&item.trancheDeployed, &item.trancheBase, &item.trancheEntry,
			&item.atrEntry, &item.trancheFailAt,
			&item.fundingPaid, &item.lastFundingReconcileAt,
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
				// The order-detail endpoint refuses finished grids, so the
				// final profit must come from the finished-bot list. Without
				// it the row would keep realized 0 and its last floating
				// mark forever (v2.0.74: 22 closures carried a stale −$0.35
				// unrealized sum the app showed as settled).
				closedReason := "ALREADY_CLOSED"
				finalRealized := decimal.Zero
				finalReported := false
				if finished := findFinishedGridRecord(ctx, client, bot.remoteID); finished != nil {
					finalRealized = finished.FinalProfit()
					finalReported = true
				}
				var finalRealizedArg any
				if finalReported {
					finalRealizedArg = finalRealized
				}
				if _, err := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET status = 'STOPPED', closed_reason = $2,
					    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
					    realized_pnl_usdt = COALESCE($3, realized_pnl_usdt),
					    unrealized_pnl_usdt = 0,
					    closed_at = NOW(), last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
					WHERE id = $1
				`, bot.id, closedReason, finalRealizedArg); err != nil {
					worker.logger.Error("persist already-closed grid state",
						"component", "autogrid_worker", "bot_id", bot.id, "error", err)
				}
				worker.logger.Info("Pionex grid not found or already closed on exchange, marked STOPPED",
					"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id,
					"final_pnl_persisted", finalReported)
				continue
			}
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET reconciliation_state = 'REMOTE_READ_FAILED',
				    last_error = $2, last_reconciled_at = NOW(), updated_at = NOW()
				WHERE id = $1
			`, bot.id, getErr.Error()); err != nil {
				worker.logger.Error("persist remote read failure",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			}
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
		// v2.0.74: realized must mirror the app's "Grid Profit" — the
		// exchange's accumulated realized grid profit (profitReduce), NOT
		// profitWithdrawn, which stays 0 while a futures grid compounds its
		// profit internally. unrealized stays the position's floating PnL at
		// the mark price, matching the app's "Floating PnL". total
		// (realized+unrealized) then reconciles with the app's "Total PnL".
		realized := remote.BUOrderData.GridProfit()
		unrealized := decimal.Zero
		if !remote.BUOrderData.Position.IsZero() && price.GreaterThan(decimal.Zero) {
			position := remote.BUOrderData.Position
			// Pionex may report the grid position as an unsigned magnitude
			// even for short grids. If a SHORT reports a positive position,
			// treat it as a magnitude and negate: profit for a short is
			// open−price, and an unsigned feed would otherwise invert every
			// short bot's PnL (closing winners, holding losers). A signed
			// feed (negative position) already encodes the side and passes
			// through unchanged.
			if bot.direction == "SHORT" && position.IsPositive() {
				position = position.Neg()
			}
			unrealized = position.Mul(price.Sub(remote.BUOrderData.PositionOpenPrice))
		}

		// REAL funding reconciliation: prefer the exchange's per-bot
		// fundingFeePayment (present on the grid record) over the symbol-wide
		// history fetch — the symbol fetch attributes every bot on a symbol
		// (and manual positions) to each bot. When the exchange field is
		// present, the durable column is resynced to it so telemetry and
		// realized never disagree; the history path stays only for records
		// that predate the field.
		if remote.BUOrderData.FundingFeePaymentReported() {
			exchangeFunding := remote.BUOrderData.FundingFeePayment()
			realized = realized.Add(exchangeFunding)
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET funding_paid_usdt = $2::NUMERIC,
				    last_funding_reconcile_at = NOW(),
				    updated_at = NOW()
				WHERE id = $1
			`, bot.id, exchangeFunding.Neg()); err != nil {
				// Persist failure must not corrupt the in-memory figure the
				// same pass persists as realized PnL, or the column and the
				// PnL would diverge until the next pass resyncs them.
				worker.logger.Warn("REAL funding per-bot resync persist failed",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			} else {
				bot.fundingPaid = exchangeFunding.Neg()
			}
		} else {
			// Legacy symbol-wide accrual, kept only for records that predate
			// the exchange field: Pionex settles perpetual funding in the
			// wallet, so no remote profit figure carries it.
			if bot.localStatus == "RUNNING" &&
				(bot.lastFundingReconcileAt == nil ||
					time.Since(*bot.lastFundingReconcileAt) >= realFundingReconcileInterval) {
				anchor := bot.createdAt
				if bot.lastFundingReconcileAt != nil && bot.lastFundingReconcileAt.After(anchor) {
					anchor = *bot.lastFundingReconcileAt
				}
				fundings, fundingErr := client.GetFundingFeeHistory(
					ctx, bot.symbol, anchor.UnixMilli(), time.Now().UnixMilli(), 200)
				if fundingErr != nil {
					// Fail-open, anchor untouched: advancing it on a failed fetch
					// would silently forfeit every fee inside the skipped window;
					// the next manage pass retries the same window instead.
					worker.logger.Warn("REAL funding reconcile fetch failed",
						"component", "autogrid_worker", "bot_id", bot.id,
						"symbol", bot.symbol, "error", fundingErr)
				} else {
					fundingSum := decimal.Zero
					for _, fee := range fundings {
						fundingSum = fundingSum.Add(fee.FundingFee)
					}
					if _, fundingErr := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET funding_paid_usdt = funding_paid_usdt + $2::NUMERIC,
						    last_funding_reconcile_at = NOW(),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id, fundingSum); fundingErr != nil {
						// Column and anchor move together in one UPDATE: a persist
						// failure must not count the sum in memory either, or the
						// same window would be subtracted twice on the retry.
						worker.logger.Warn("REAL funding reconcile persist failed",
							"component", "autogrid_worker", "bot_id", bot.id, "error", fundingErr)
					} else {
						bot.fundingPaid = bot.fundingPaid.Add(fundingSum)
					}
				}
			}
			// The column is cumulative and signed positive = paid; realized
			// re-derives from remote truth minus that column EVERY pass, so
			// the subtraction is idempotent across passes and survives anchor
			// failures. When the exchange reports fundingFeePayment the
			// funding is already inside realized above and bot.fundingPaid
			// was resynced to the same figure, so this branch is skipped.
			realized = realized.Sub(bot.fundingPaid)
		}

		// A durable stop intent (grid.stop / autogrid.stop / manual close)
		// must reach the exchange. Cancel-state machine values survive the
		// remote-truth persist below so failed cancels keep retrying.
		cancelStates := "('CANCEL_SUBMITTING','CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING','CANCEL_FAILED','CANCEL_OUTCOME_UNKNOWN')"

		if terminalRemoteGridStatus(remoteStatus) {
			status, closedReason := terminalOutcome(reasonBy)
			// v2.0.74: a finished grid settles at the exchange's own final
			// figure (profitExited nets grid profit, position-close PnL and
			// fees). The grid-profit `realized` above never includes the
			// position-close leg, so it must not survive as the final number.
			finalRealized := remote.BUOrderData.FinalProfit()
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET status = $2,
				    closed_reason = COALESCE(NULLIF(closed_reason, ''), $3),
				    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
				    last_remote_status = $4, realized_pnl_usdt = $5,
				    unrealized_pnl_usdt = 0, closed_at = NOW(),
				    last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
				WHERE id = $1
			`, bot.id, status, closedReason, remoteStatus, finalRealized); err != nil {
				worker.logger.Error("persist terminal grid state",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			}
			worker.logger.Info(
				"Pionex grid reached terminal state",
				"component", "autogrid_worker", "symbol", bot.symbol,
				"status", status, "reason", closedReason, "realized_pnl", finalRealized.String(),
			)
			continue
		}

		// Persist remote truth and PnL without reverting durable stop intents:
		// the local status is kept and in-flight cancel states are preserved.
		persistedReconciliation := "REMOTE_TERMINAL_PENDING"
		if bot.localStatus == "RUNNING" {
			persistedReconciliation = "REST_AUTHORITATIVE_OK"
		}
		if _, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = $2,
			    reconciliation_state = CASE
					WHEN reconciliation_state IN `+cancelStates+` THEN reconciliation_state
					ELSE $3
				END,
			    last_remote_status = $4, realized_pnl_usdt = $5,
			    unrealized_pnl_usdt = $6,
			    peak_pnl_usdt = GREATEST(COALESCE(peak_pnl_usdt, 0), $5::NUMERIC + $6::NUMERIC),
			    trough_pnl_usdt = LEAST(COALESCE(trough_pnl_usdt, 0), $5::NUMERIC + $6::NUMERIC),
			    last_reconciled_at = NOW(),
			    last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, bot.id, bot.localStatus, persistedReconciliation, remoteStatus, realized, unrealized); err != nil {
			// The PnL persist must never fail silently: v2.0.45 lost every
			// REAL mark for weeks exactly because this error was swallowed.
			worker.logger.Error("persist remote grid truth and PnL",
				"component", "autogrid_worker", "bot_id", bot.id, "error", err)
		}

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
					// v2.0.19: the excursion must be adverse FOR THE DIRECTION —
					// |price−entry| also fired on a directional bot's PROFIT
					// excursion (LONG rallying 0.75 ATR, two red candles →
					// top-up at the local top). The regime gate mirrors the
					// time-box: no second tranche into a confirmed trend.
					adverse := trancheAdversePct(bot.direction, price, entry)
					limit := bot.atrEntry * 2.0 * 0.75 / 100.0
					if adverse >= limit &&
						!worker.trancheTimeBoxTrending(ctx, bot.symbol) &&
						worker.trancheTurnConfirmed(ctx, bot.symbol, price, entry) {
						topUp = "подтверждённый adverse 0.75×ATR(1h)"
					}
				}
				if topUp != "" {
					// v2.0.56 (F2): the doubling below doubles max_loss with the
					// injected margin — same risk gate as the paper path. The
					// skip writes a 1h backoff marker so an armed 24h time-box
					// cannot re-log every manage pass.
					if skip := worker.tranche2RiskGate(ctx, *settings, bot.id, bot.leverage, botMaxLoss.Mul(decimal.NewFromInt(2))); skip != "" {
						tag, tErr2 := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET model_state = jsonb_set(model_state, '{tranche2SkipAt}',
							to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
						    updated_at = NOW()
						WHERE id = $1
						  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
						  AND COALESCE((model_state->>'tranche2SkipAt')::TIMESTAMPTZ, '1970-01-01') < NOW() - INTERVAL '1 hour'
					`, bot.id)
						if tErr2 == nil && tag.RowsAffected() == 1 {
							_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, "TRANCHE_2_SKIPPED", &price, nil, map[string]any{
								"reason": skip, "effective_max_loss": botMaxLoss.Mul(decimal.NewFromInt(2)).StringFixed(2),
							})
							worker.logger.Info("tranche 2 (REAL) skipped by risk gate",
								"component", "autogrid_worker", "bot_id", bot.id, "reason", skip)
						}
						topUp = ""
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
						if _, markErr := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET model_state = jsonb_set(model_state, '{trancheFailAt}', to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id); markErr != nil {
							worker.logger.Error("tranche 2 fail-marker write failed",
								"component", "autogrid_worker", "bot_id", bot.id, "error", markErr)
						}
					} else {
						// v2.0.19: peak_pnl_usdt must NOT double here. The
						// freshly injected margin starts at ~0 PnL, so doubling
						// the stored peak armed the trailing floor
						// max(0.8·2P, 0.5·2T) against total ≈ P on the very next
						// manage tick — an instant false TRAILING_TAKE_PROFIT on
						// exactly the most successful bots. Paper never doubled
						// it; this aligns REAL with the paper semantics.
						if _, err := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET pnl_target_usdt = pnl_target_usdt * 2,
						    max_loss_usdt = max_loss_usdt * 2,
						    model_state = jsonb_set(model_state, '{trancheDeployed}', '2'::jsonb),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id); err != nil {
							worker.logger.Error("tranche 2 target doubling failed (invest_in already committed)",
								"component", "autogrid_worker", "bot_id", bot.id, "error", err)
						} else {
							// Same-tick decision safety: decideBotAction below
							// reads the LOCAL copies taken before the tranche
							// block, not the struct fields — refresh BOTH, or
							// the top-up tick can fire a half-size TP/SL against
							// the doubled position.
							bot.investment = base
							if bot.pnlTarget != nil {
								doubled := bot.pnlTarget.Mul(decimal.NewFromInt(2))
								bot.pnlTarget = &doubled
								botTarget = doubled
							}
							if bot.maxLoss != nil {
								doubled := bot.maxLoss.Mul(decimal.NewFromInt(2))
								bot.maxLoss = &doubled
								botMaxLoss = doubled
							}
						}
						worker.logger.Info("tranche 2 deployed (REAL invest_in)",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", topUp)
						_ = QueueTelegramEvent(ctx, worker.db, "TRANCHE_2", map[string]any{
							"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": topUp,
						})
					}
				}
			}
		}

		// Behavior trace (v2.0.56 F7): REAL bots join the bot_telemetry
		// series so underwater/recovery analytics cover the real fleet too.
		// inventory_notional comes from the exchange-reported position;
		// grid_level stays 0 (the real loop tracks no paper ladder), while
		// funding_paid_usdt carries the reconciled cumulative column.
		// Best-effort insert, same contract as the paper path.
		if bot.localStatus == "RUNNING" {
			inventoryNotional := decimal.Zero
			if !remote.BUOrderData.Position.IsZero() && price.GreaterThan(decimal.Zero) {
				inventoryNotional = remote.BUOrderData.Position.Abs().Mul(price)
			}
			_, _ = worker.db.Exec(ctx, `
				INSERT INTO bot_telemetry
					(bot_id, bot_number, symbol, price, realized_pnl, unrealized_pnl,
					 total_pnl, grid_level, inventory_notional, adjustments_count, funding_paid_usdt)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			`, bot.id, bot.botNumber, bot.symbol, price, realized, unrealized,
				realized.Add(unrealized), 0, inventoryNotional, bot.adjustments, bot.fundingPaid)
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
				// v2.0.15: move the local anti-hunt stop with the range
				// (same distance-beyond-bound as at deploy) — the native
				// exchange stop cannot be updated via adjust_params, and a
				// stale local stop would stop gating anything.
				setStop := ""
				var stopArg []any
				if bot.antiHuntStop != nil && bot.antiHuntStop.GreaterThan(decimal.Zero) {
					newStop := decision.NewLower.Sub(bot.lower.Sub(*bot.antiHuntStop))
					if bot.direction == "SHORT" {
						newStop = decision.NewUpper.Add(bot.antiHuntStop.Sub(bot.upper))
					}
					setStop = ", anti_hunt_stop_price = $4"
					stopArg = append(stopArg, newStop)
				}
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET lower_price = $2, upper_price = $3,
					    adjustments_count = adjustments_count + 1`+setStop+`, updated_at = NOW()
					WHERE id = $1
				`, append([]any{bot.id, decision.NewLower, decision.NewUpper}, stopArg...)...)
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
			// v2.0.74: the detail endpoint refuses finished grids, so the
			// finished-bot list is the authoritative fallback for the final
			// profit. Either source must also zero any stale floating mark:
			// a terminal bot has no position left to float.
			var profit *decimal.Decimal
			if remote, remoteErr := historyClient.GetFuturesGridBot(ctx, item.remoteID); remoteErr == nil && remote != nil {
				value := remote.BUOrderData.FinalProfit()
				profit = &value
			} else if finished := findFinishedGridRecord(ctx, historyClient, item.remoteID); finished != nil {
				value := finished.FinalProfit()
				profit = &value
			}
			if profit != nil {
				if _, err := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET realized_pnl_usdt = $2,
					    unrealized_pnl_usdt = 0,
					    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
					    updated_at = NOW()
					WHERE id = $1
				`, item.id, *profit); err != nil {
					worker.logger.Error("backfill closed grid final PnL",
						"component", "autogrid_worker", "bot_id", item.id, "error", err)
				}
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

// findFinishedGridRecord pages the documented finished-bot list
// (GET /api/v1/bot/orders?status=finished) and returns the futures-grid
// entry with the given buOrderId, decoded into the typed detail payload.
// The order-detail endpoint refuses finished grids, so this list is the
// authoritative source of a closed grid's final profit figures. Returns nil
// when the record is absent or the listing fails; callers must treat nil as
// "unknown", never as "zero profit".
func findFinishedGridRecord(ctx context.Context, client *pionex.Client, buOrderID string) *pionex.BUOrderDataResponse {
	if strings.TrimSpace(buOrderID) == "" {
		return nil
	}
	token := ""
	for page := 0; page < 10; page++ {
		orders, next, listErr := client.ListBotOrders(ctx, "finished", token)
		if listErr != nil {
			return nil
		}
		for _, order := range orders {
			if order.BUOrderID == buOrderID {
				data, decodeErr := order.FuturesGridData()
				if decodeErr != nil {
					return nil
				}
				return data
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
	return nil
}

// fleetStopEnvelope returns Σ stored max_loss_usdt across the WHOLE risk
// account — paper_grid_bots AND grid_bots (REAL) — plus the reserve, minus
// one optionally excluded bot. PAPER and REAL stop exposure live on the same
// account against the same breaker, so every envelope gate must sum both
// tables jointly (v2.0.67 parity fix: the deploy gate used to count paper
// only, the tranche-2 gate counted only the candidate's own table).
// Package-level since v2.0.71: the manual DeployManualBot REAL path runs the
// same exam through the Service's own db/risk handles — one SQL, one verdict,
// no drift between the scan and the hand deploy.
func fleetStopEnvelope(
	ctx context.Context,
	db *pgxpool.Pool,
	settingsID string,
	excludeBotID *string,
	reserve decimal.Decimal,
) (decimal.Decimal, error) {
	var envelope decimal.Decimal
	err := db.QueryRow(ctx, `
		SELECT
		  (SELECT COALESCE(SUM(max_loss_usdt), 0) FROM paper_grid_bots
		     WHERE settings_id = $1 AND status = 'RUNNING'
		       AND ($2::UUID IS NULL OR id <> $2::UUID))
		  +
		  (SELECT COALESCE(SUM(max_loss_usdt), 0) FROM grid_bots
		     WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		       AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		       AND ($2::UUID IS NULL OR id <> $2::UUID))
		  + $3::NUMERIC
	`, settingsID, excludeBotID, reserve).Scan(&envelope)
	return envelope, err
}

// tranche2MaxLossCap derives the per-bot effective-stop ceiling after a
// tranche-2 top-up: the bot's DESIGN full stop (budget × its own leverage ×
// ADAPTIVE_ATR floor) × breakerHeadroom. The old static $12 (v2.0.56) sat
// below the 4x design stop ($16 on $200×4×2%) so wide 4x bots could never
// tranche (prod BEX $16 > $12 skipped forever); the derived cap restores the
// design: 2x→$10, 4x→$20 on the $200 budget while still refusing σ-scaled
// overshoots ($29.51 HEMI-style outliers).
func tranche2MaxLossCap(budgetUSDT decimal.Decimal, botLeverage int) decimal.Decimal {
	if botLeverage < 1 {
		botLeverage = 1
	}
	return budgetUSDT.
		Mul(decimal.NewFromInt(int64(botLeverage))).
		Mul(designStopFloorFrac()).
		Mul(decimal.NewFromFloat(breakerHeadroom))
}

// tranche2RiskGate (v2.0.56 F2, derived cap v2.0.67) guards the tranche-2
// top-up on BOTH fleets: the doubled stop must stay under the bot's derived
// per-bot cap AND the whole-account stop envelope (paper + REAL joint sum)
// under 0.8× the risk engine's daily-loss breaker (a synchronized stop wave
// must not outrun the breaker that is supposed to catch it). Returns "" when
// the top-up is allowed, otherwise a human-readable skip reason. Envelope
// query failure fails OPEN: deploy-time gates stay fail-closed, but starving
// a healthy bot of its second half over a read failure is the worse trade.
func (worker *Worker) tranche2RiskGate(
	ctx context.Context,
	settings Settings,
	botID string,
	botLeverage int,
	effMaxLoss decimal.Decimal,
) string {
	capUSDT := tranche2MaxLossCap(settings.BudgetUSDT, botLeverage)
	if effMaxLoss.GreaterThan(capUSDT) {
		return fmt.Sprintf("кап эффективного стопа: %s > %s USDT", effMaxLoss.StringFixed(2), capUSDT.StringFixed(2))
	}
	rs, err := worker.risk.LoadSettings(ctx)
	if err != nil || rs == nil || !rs.MaxDailyLossUSD.GreaterThan(decimal.Zero) {
		return ""
	}
	envelope, qErr := fleetStopEnvelope(ctx, worker.db, settings.ID, &botID, effMaxLoss)
	if qErr != nil {
		return ""
	}
	envelopeLimit := rs.MaxDailyLossUSD.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction))
	if envelope.GreaterThan(envelopeLimit) {
		return fmt.Sprintf("конверт стопов флота %s > 0.8× дневного брейкера %s",
			envelope.StringFixed(2), envelopeLimit.StringFixed(2))
	}
	return ""
}

// riskStopEnvelopeFraction is the fleet stop-envelope ceiling as a fraction
// of the risk engine's daily-loss breaker — one source shared by the
// tranche-2 top-up gate and the deploy gate so the two can never drift apart.
const riskStopEnvelopeFraction = 0.8

// stopEnvelopeExceeded is the shared envelope verdict. Strict inequality: a
// fleet sitting exactly at the ceiling still deploys (the live 10×$4 paper
// fleet against a $50 breaker must keep rotating).
func stopEnvelopeExceeded(envelope, breaker decimal.Decimal) bool {
	return envelope.GreaterThan(breaker.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction)))
}

// deployStopEnvelopeGate is the deploy path's mirror of the tranche-2
// envelope check (v2.0.67: applies to PAPER and REAL deploys alike — one
// risk account, one gate): the WHOLE account's RUNNING stop envelope (paper
// fleet + REAL fleet, joint sum) plus the candidate's FULL stop — the exact
// amount tranche2RiskGate re-doubles the stored tranche-1 half to on the
// top-up — must stay under the breaker, so a synchronized stop wave can
// never outrun the breaker that exists to catch it, and no bot is born
// already stranded (its tranche-2 fitting only after some other bot dies).
// Returns "" when the deploy may proceed, otherwise a human-readable
// rejection reason. Read failures fail OPEN with a logged warning (the same
// trade tranche2RiskGate makes); the durable risk exam above stays the
// fail-closed line. Package-level since v2.0.71 so the manual REAL deploy in
// Service.DeployManualBot runs the identical gate.
func deployStopEnvelopeGate(
	ctx context.Context,
	db *pgxpool.Pool,
	riskEngine *risk.Engine,
	logger *slog.Logger,
	settingsID string,
	candidateStop decimal.Decimal,
) string {
	rs, err := riskEngine.LoadSettings(ctx)
	if err != nil || rs == nil || !rs.MaxDailyLossUSD.GreaterThan(decimal.Zero) {
		return ""
	}
	envelope, err := fleetStopEnvelope(ctx, db, settingsID, nil, candidateStop)
	if err != nil {
		logger.Warn("deploy stop-envelope read failed; gate disarmed for this candidate",
			"component", "autogrid_worker", "error", err)
		return ""
	}
	if stopEnvelopeExceeded(envelope, rs.MaxDailyLossUSD) {
		return fmt.Sprintf("конверт стопов флота %s + полный стоп кандидата > 0.8× дневного брейкера %s",
			envelope.StringFixed(2),
			rs.MaxDailyLossUSD.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction)).StringFixed(2))
	}
	return ""
}

// directionalFlipBlocked (v2.0.56 F9): a DIRECTIONAL entry into a symbol
// that ran a bot of a different direction within the last 12 hours is a
// flip-trade on stale structure (2026-09-01: WLD ran NEUTRAL to a 03:34
// close, a LONG deployed 05:16 and donated −8.09 = 91% of the day's
// losses). NEUTRAL re-entries stay free. The 14d ledger shows directional
// entries at 1W/3L for 69% of all losses — the flip window is where they
// concentrate.
func (worker *Worker) directionalFlipBlocked(ctx context.Context, symbol, upperTrend string, paper bool) bool {
	var flipped bool
	if paper {
		if err := worker.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM paper_grid_bots
				WHERE symbol = $1 AND status = 'COMPLETED'
				  AND direction <> $2
				  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '12 hours'
			)
		`, symbol, upperTrend).Scan(&flipped); err != nil {
			return false
		}
		return flipped
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM grid_bots
			WHERE symbol = $1
			  AND status IN ('STOPPED', 'LIQUIDATED', 'COMPLETED', 'FAILED')
			  AND direction <> $2
			  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '12 hours'
		)
	`, symbol, upperTrend).Scan(&flipped); err != nil {
		return false
	}
	return flipped
}

// managePaperBots marks paper bots to market and closes them when the same
// PnL rules that govern real bots are hit, so PAPER mode exercises the whole
// lifecycle.
func (worker *Worker) managePaperBots(ctx context.Context, settings Settings) error {
	priceBySymbol, err := worker.priceMap(ctx)
	if err != nil {
		return err
	}
	// v2.0.17: every pass is OBSERVABLE. The 2026-08-20 incident — bots
	// unsupervised for hours while no error surfaced — was undiagnosable
	// because the pass itself logged nothing and the mark UPDATE swallowed
	// its errors.
	passMarked, passSkipped := 0, 0
	// Delist sweep (v2.0.17): a RUNNING bot whose updated_at is stale beyond
	// 45 minutes has had NO supervision — neither manage marks (which touch
	// updated_at) nor scan supervision marks. That means its price has been
	// unobtainable for 45+ minutes (delisting/renaming — the LAB case) or
	// the manage loop is wedged; either way it must not sit unsupervised
	// forever. Close it as an ops cleanup; exempt from the breaker.
	// v2.0.19 — two-tier, price-aware: the 45-minute tier additionally
	// requires the symbol to be MISSING from the live price map (the true
	// delist signature). A ticker-feed outage or manage-loop wedge ages
	// EVERY bot at once — that fleet-wide shape must not mass-close live
	// bots; only the 6-hour tier catches a genuinely wedged loop. An empty
	// price map means the feed itself failed — sweep is skipped entirely.
	if presentSymbols := mapKeys(priceBySymbol); len(presentSymbols) > 0 {
		sweep, sweepErr := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET status = 'COMPLETED', closed_reason = 'DELISTED_NO_PRICE',
			    closed_at = NOW(), updated_at = NOW()
			WHERE settings_id = $1 AND status = 'RUNNING'
			  AND (
			    (updated_at < NOW() - INTERVAL '45 minutes' AND NOT (symbol = ANY($2::text[])))
			    OR updated_at < NOW() - INTERVAL '6 hours'
			  )
		`, settings.ID, presentSymbols)
		if sweepErr == nil && sweep.RowsAffected() > 0 {
			worker.logger.Warn("delist sweep closed stale paper bots",
				"component", "autogrid_worker", "closed", sweep.RowsAffected())
			_ = QueueTelegramEvent(ctx, worker.db, "DELIST_SWEEP", map[string]any{
				"closed": sweep.RowsAffected(),
			})
		}
	}
	rows, err := worker.db.Query(ctx, `
		SELECT id, COALESCE(bot_number, 0), symbol, direction, entry_price, leverage, quote_investment,
		       lower_price, upper_price, pnl_target_usdt, max_loss_usdt,
		       grid_num, last_grid_level, realized_pnl_usdt, COALESCE(adjustments_count, 0),
		       anti_hunt_stop_price, opened_at, last_funding_at,
		       COALESCE(peak_pnl_usdt, 0),
		       COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       NULLIF(model_state->>'trancheBase',''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0),
		       candidate_id, COALESCE(pairs_completed, 0), COALESCE(funding_paid_usdt, 0)
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
		candidateID              *string
		pairsCompleted           int
		fundingPaid              decimal.Decimal
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
			&item.candidateID, &item.pairsCompleted, &item.fundingPaid,
		); err != nil {
			rows.Close()
			return err
		}
		bots = append(bots, item)
	}
	rows.Close()

	defer func() {
		worker.logger.Info("manage paper pass",
			"component", "autogrid_worker",
			"bots", len(bots), "marked", passMarked, "skipped_no_price", passSkipped)
	}()

	effectiveFeeBps := settings.FeeBps.Add(settings.SlippageBps)
	feeRate := effectiveFeeBps.Div(decimal.NewFromInt(10000))
	// Stop-radar (v2.0.47, SHADOW): collect per-bot state while the loop
	// already holds it, score once after the pass — the radar itself is
	// throttled per bot and never touches the exit ladder.
	radarInputs := make([]radarInput, 0, len(bots))
	// Completed grid pairs pay MAKER on both legs (Pionex futures grid quotes
	// passive limit orders: 0.02% maker vs 0.05% taker — pionex.com/en/fees);
	// the taker+slippage composite stays reserved for exits, which cross the
	// book. v2.0.23: pairs used to be booked at the taker composite, costing
	// paper ~10 bps per pair against reality (2026-08-20 external audit §2).
	pairFeeBps := decimal.NewFromFloat(pionexMakerFeeBps)

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
		// v2.0.58 (F6): the venue-native rate overlays the cross average —
		// it is what our perps actually settle, and the only source for
		// Pionex-exclusive listings (2026-09-01 audit: 30% of the fleet
		// accrued a flat 10bps while AAOIX's live rate was ~19bps).
		for symbol, fraction := range worker.nativeFundingRates(ctx) {
			fundingRateBySymbol[symbol] = fraction.Mul(decimal.NewFromInt(10000))
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
			passSkipped++
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
		pairsDelta := 0
		fundingPaid := bot.fundingPaid
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
			// per-level notional. Maker pair fees apply per completed pair;
			// taker+slippage only on exits (v2.0.23).
			currentLevel = gridLevelForPrice(bot.lower, bot.upper, bot.gridNum, price)
			previousLevel := currentLevel
			if bot.lastLevel != nil {
				previousLevel = *bot.lastLevel
			}
			var pairProfit, inventoryNotional decimal.Decimal
			pairProfit, unrealized, inventoryNotional = neutralGridPaperPNL(
				bot.lower, bot.upper, bot.gridNum, bot.investment, bot.leverage,
				previousLevel, currentLevel, price, pairFeeBps,
			)
			realized = realized.Add(pairProfit)
			fundingExposure = inventoryNotional
			fundingPays = price.LessThan(bot.lower.Add(bot.upper).Div(decimal.NewFromInt(2)))
			// Each level crossing completes one grid pair in the stateless
			// ladder — the activity counter the harvest-vs-bleed analytics
			// split needs (v2.0.54).
			if bot.lastLevel != nil && currentLevel != previousLevel {
				d := currentLevel - previousLevel
				if d < 0 {
					d = -d
				}
				pairsDelta = d
			}
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
				fundingPaid = fundingPaid.Add(*fundingDelta)
			} else {
				realized = realized.Add(*fundingDelta)
				fundingPaid = fundingPaid.Sub(*fundingDelta)
			}
			// Persist the accrual anchor separately: realized itself flows to
			// the mark/close/adjust UPDATE below, and a crash in between can
			// lose at most one 8h accrual — never double-count.
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots SET last_funding_at = $2,
				    funding_paid_usdt = funding_paid_usdt + $3,
				    updated_at = NOW()
				WHERE id = $1
			`, bot.id, *nextFundingAt, fundingDeltaSigned(fundingPays, *fundingDelta))
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
		radarInputs = append(radarInputs, radarInput{
			botID: bot.id, botNumber: bot.botNumber, botSource: "PAPER",
			symbol: bot.symbol, direction: bot.direction,
			price: price, antiHunt: bot.antiHuntStop, lower: bot.lower, upper: bot.upper,
			atrEntryPct: bot.atrEntry, total: total,
			inventorySide: inventorySideOf(bot.direction, price, bot.lower, bot.upper),
		})
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
				    peak_pnl_usdt = GREATEST(peak_pnl_usdt, $3),
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
			recordCandidateOutcome(ctx, worker.db, bot.candidateID, total, decision.Reason)

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
			// v2.0.15 re-anchors, applied as extra SET clauses after the
			// fixed $1..$6 parameters:
			//   1. directional grids re-anchor entry_price — their
			//      unrealized is re-derived from entry every tick, so
			//      without it each shift double-counts position PnL;
			//   2. the anti-hunt stop moves with the range, preserving its
			//      deploy-time distance-beyond-bound — a stale deploy stop
			//      drifts away from the shifted bounds and protects nothing.
			setClauses := ""
			var extraArgs []any
			if bot.direction != "NEUTRAL" {
				setClauses += fmt.Sprintf(", entry_price = $%d", 7+len(extraArgs))
				extraArgs = append(extraArgs, price)
			}
			if bot.antiHuntStop != nil && bot.antiHuntStop.GreaterThan(decimal.Zero) {
				newStop := decision.NewLower.Sub(bot.lower.Sub(*bot.antiHuntStop))
				if bot.direction == "SHORT" {
					newStop = decision.NewUpper.Add(bot.antiHuntStop.Sub(bot.upper))
				}
				setClauses += fmt.Sprintf(", anti_hunt_stop_price = $%d", 7+len(extraArgs))
				extraArgs = append(extraArgs, newStop)
			}
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots
				SET lower_price = $2, upper_price = $3,
				    adjustments_count = adjustments_count + 1,
				    mark_price = $4, unrealized_pnl_usdt = 0,
				    realized_pnl_usdt = $5, last_grid_level = $6`+setClauses+`
				WHERE id = $1
			`, append([]any{bot.id, decision.NewLower, decision.NewUpper, price, shiftRealized, newLevel}, extraArgs...)...)

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
		if _, mErr := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET mark_price = $2, unrealized_pnl_usdt = $3,
			    realized_pnl_usdt = $4, last_grid_level = $5,
			    pairs_completed = pairs_completed + $6,
			    funding_paid_usdt = $7,
			    peak_pnl_usdt = GREATEST(peak_pnl_usdt, $3::NUMERIC + $4::NUMERIC),
			    trough_pnl_usdt = LEAST(trough_pnl_usdt, $3::NUMERIC + $4::NUMERIC),
			    updated_at = NOW()
			WHERE id = $1
		`, bot.id, price, unrealized, realized, currentLevel, pairsDelta, fundingPaid); mErr != nil {
			worker.logger.Error("paper bot mark UPDATE failed",
				"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol, "error", mErr)
		} else {
			passMarked++
			// Behavior trace (v2.0.54): the underwater-duration and
			// recovery-shape analytics read this series, not the scalar
			// peak/trough columns.
			_, _ = worker.db.Exec(ctx, `
				INSERT INTO bot_telemetry
					(bot_id, bot_number, symbol, price, realized_pnl, unrealized_pnl,
					 total_pnl, grid_level, inventory_notional, adjustments_count, funding_paid_usdt)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			`, bot.id, bot.botNumber, bot.symbol, price, realized, unrealized,
				realized.Add(unrealized), currentLevel, fundingExposure,
				bot.adjustmentsCount, fundingPaid)
		}

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
					// v2.0.19: direction-signed adverse (profit excursions of
					// directional bots no longer count) + the same regime gate
					// as the time-box — no second tranche into a confirmed
					// trend (mirror of the REAL path).
					adverse := trancheAdversePct(bot.direction, price, bot.entry)
					// ATR(1h) ≈ 2 × ATR(15m) — the entry-time scanner figure.
					limit := bot.atrEntry * 2.0 * 0.75 / 100.0
					if adverse >= limit &&
						!worker.trancheTimeBoxTrending(ctx, bot.symbol) &&
						worker.trancheTurnConfirmed(ctx, bot.symbol, price, bot.entry) {
						trancheReason = "подтверждённый adverse 0.75×ATR(1h)"
					}
				}
				if trancheReason != "" {
					// v2.0.56 (F2): gate the doubling — per-bot effective stop
					// cap + fleet envelope ≤ 0.8× daily breaker. The event
					// payload now carries the effective target/stop so the
					// risk desk no longer shows "$8" over a $16 stop.
					effMaxLoss, effTarget := decimal.Zero, decimal.Zero
					if bot.maxLoss != nil {
						effMaxLoss = bot.maxLoss.Mul(decimal.NewFromInt(2))
					}
					if bot.pnlTarget != nil {
						effTarget = bot.pnlTarget.Mul(decimal.NewFromInt(2))
					}
					if skip := worker.tranche2RiskGate(ctx, settings, bot.id, bot.leverage, effMaxLoss); skip != "" {
						// Backoff marker: the 24h time-box keeps the trigger
						// armed forever, so a gated skip must not re-log on
						// every manage pass — one event per hour max.
						tag, tErr2 := worker.db.Exec(ctx, `
						UPDATE paper_grid_bots
						SET model_state = jsonb_set(model_state, '{tranche2SkipAt}',
							to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
						    updated_at = NOW()
						WHERE id = $1 AND status = 'RUNNING'
						  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
						  AND COALESCE((model_state->>'tranche2SkipAt')::TIMESTAMPTZ, '1970-01-01') < NOW() - INTERVAL '1 hour'
					`, bot.id)
						if tErr2 == nil && tag.RowsAffected() == 1 {
							_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "TRANCHE_2_SKIPPED", &price, nil, map[string]any{
								"reason": skip, "investment": base.String(),
								"effective_max_loss": effMaxLoss.StringFixed(2),
							})
							worker.logger.Info("tranche 2 skipped by risk gate",
								"component", "autogrid_worker", "symbol", bot.symbol, "reason", skip)
						}
					} else {
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
								"effective_target": effTarget.StringFixed(2), "effective_max_loss": effMaxLoss.StringFixed(2),
							})
							_ = QueueTelegramEvent(ctx, worker.db, "TRANCHE_2", map[string]any{
								"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": trancheReason,
								"effective_max_loss": effMaxLoss.StringFixed(2),
							})
						}
					}
				}
			}
		}
	}
	// v2.0.72: the REAL fleet joins the radar pass — without this append it
	// flew unscored and un-notified while paper was already protected. The
	// fetch is gated on the same mode switch radarPass itself checks so OFF
	// skips the extra query entirely.
	if settings.StopForecastMode != "OFF" {
		radarInputs = append(radarInputs, worker.realRadarInputs(ctx, settings, priceBySymbol)...)
	}
	worker.radarPass(ctx, settings, radarInputs)

	// Telemetry retention, batched like the radar snapshots.
	_, _ = worker.db.Exec(ctx, `DELETE FROM bot_telemetry WHERE captured_at < NOW() - INTERVAL '14 days' AND id % 1000 = 0`)
	return nil
}

func (worker *Worker) priceMap(ctx context.Context) (map[string]decimal.Decimal, error) {
	// v2.0.58 (F8): mark decisions on the exchange's own markPrice — the
	// official PnL reference per Pionex docs — fetched for the whole PERP
	// universe in one public call. Last-trade tickers stay as the fallback:
	// an indexes outage must not wedge supervision.
	prices := make(map[string]decimal.Decimal, 512)
	if indexes, err := worker.publicClient.GetIndexes(ctx, ""); err == nil {
		for _, idx := range indexes {
			if idx.MarkPrice.GreaterThan(decimal.Zero) {
				sym := strings.ToUpper(strings.TrimSpace(idx.Symbol))
				prices[sym] = idx.MarkPrice
				trimmed := strings.TrimSuffix(strings.TrimSuffix(sym, "_PERP"), ".PERP")
				prices[trimmed] = idx.MarkPrice
				prices[trimmed+"_PERP"] = idx.MarkPrice
				prices[trimmed+".PERP"] = idx.MarkPrice
			}
		}
		if len(prices) > 0 {
			return prices, nil
		}
	}
	tickers, err := worker.publicClient.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, err
	}
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

// mapKeys returns the key slice of a string-keyed map.
func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// trancheAdversePct returns the adverse excursion fraction SIGNED by the
// bot's direction: for LONG only price BELOW entry is adverse (a rally is
// profit — topping up after a rally plus a reversal buys the local top), for
// SHORT only price ABOVE entry, for NEUTRAL either side (inventory loads
// both ways). Pre-v2.0.19 the |price−entry| form fed directional bots'
// PROFIT-side excursions into the signal path.
func trancheAdversePct(direction string, price, entry decimal.Decimal) float64 {
	if !entry.IsPositive() || !price.IsPositive() {
		return 0
	}
	var adverse decimal.Decimal
	switch direction {
	case "LONG":
		if price.GreaterThanOrEqual(entry) {
			return 0
		}
		adverse = entry.Sub(price)
	case "SHORT":
		if price.LessThanOrEqual(entry) {
			return 0
		}
		adverse = price.Sub(entry)
	default:
		adverse = price.Sub(entry).Abs()
	}
	pct, _ := adverse.Div(entry).Float64()
	return pct
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

// betaGateTrend is the pure policy core of the v2.0.21 global beta gate:
// BTC's own tape decides whether altcoin NEUTRAL/LONG grids may open.
// Alts mean-revert locally while the market bleeds — every 2026-08-20 stop
// (IRENX/RIVER: local RANGE, market −3..−12%) was a neutral grid loading
// long inventory against BTC's trend. Activation is deliberately stricter
// than DetectRegime's ADX≥22: ADX≥25 plus a real slope.
func betaGateTrend(regime string, adx, emaSlopePct float64) (down, up bool) {
	if adx < 20 {
		return false, false
	}
	switch regime {
	case "TREND_DOWN":
		return emaSlopePct < -0.3, false
	case "TREND_UP":
		return false, emaSlopePct > 0.3
	}
	return false, false
}

// marketBetaRegime caches BTC's regime for 5 minutes; the deploy paths ask
// for it once per candidate loop.
func (worker *Worker) marketBetaRegime(ctx context.Context) (string, float64, float64) {
	if time.Since(worker.betaRegime.checkedAt) < 5*time.Minute {
		return worker.betaRegime.regime, worker.betaRegime.adx, worker.betaRegime.emaSlope
	}
	candles, err := worker.publicClient.GetKlines(ctx, "BTC_USDT_PERP", "60M", 60)
	if err != nil || len(candles) < 30 {
		// Fail-open: no BTC reading → no gate (the per-symbol gates still run).
		return "", 0, 0
	}
	result := marketdata.DetectRegime(candles)
	worker.betaRegime = betaRegimeCache{
		checkedAt: time.Now(),
		regime:    result.Regime,
		adx:       result.ADX,
		emaSlope:  result.EMASlopePct,
	}
	return result.Regime, result.ADX, result.EMASlopePct
}

// fundingStats48h returns the stable carry picture for a symbol: the 48h
// average per-8h rate qualifies as a carry setup when it is non-trivial,
// sampled densely enough, and not dominated by its own variance.
func (worker *Worker) fundingStats48h(ctx context.Context, symbol string) (avg, stddev float64, stable bool) {
	var samples int
	var spanHours float64
	// The sample count alone certifies ~4 minutes of data after a collector
	// gap (3 exchanges x 60s cadence); "stable for 48h" additionally needs
	// the samples to actually SPAN the window (review: HIGH).
	if err := worker.db.QueryRow(ctx, `
		SELECT AVG(funding_rate), COALESCE(STDDEV_POP(funding_rate), 0), COUNT(*),
		       EXTRACT(EPOCH FROM (MAX(captured_at) - MIN(captured_at))) / 3600.0
		FROM funding_snapshots
		WHERE symbol = $1 AND captured_at > NOW() - INTERVAL '48 hours'
	`, symbol).Scan(&avg, &stddev, &samples, &spanHours); err != nil || samples < 12 || spanHours < 6 {
		return 0, 0, false
	}
	magnitude := math.Abs(avg)
	if magnitude < fundingCarryThreshold {
		return avg, stddev, false
	}
	if magnitude >= 0.001 {
		return avg, stddev, false // extreme territory is a squeeze window, not carry
	}
	return avg, stddev, stddev < 0.6*magnitude
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

// maybeQueueCascadeShortScan (v2.0.21) turns the long-liquidation cascade
// detector from a passive freeze into the short side's entry trigger: while
// forced unwinding runs, an out-of-turn scan with cascadeShort semantics is
// queued at most once per 15 minutes. Latency from cascade start to a
// deployed SHORT grid drops from hours (waiting for the scheduled scan to
// see post-dump candidates that anti-FOMO then rejects as oversold) to
// ~10-15 minutes.
func (worker *Worker) maybeQueueCascadeShortScan(ctx context.Context, settings Settings) {
	if settings.Status != "RUNNING" {
		return
	}
	cascade, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000)
	if !cascade {
		return
	}
	var scanRecently bool
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM control_commands
			WHERE command_type = 'autogrid.scan'
			  AND status IN ('QUEUED', 'EXECUTING')
			  AND created_at > NOW() - INTERVAL '10 minutes'
		)
	`).Scan(&scanRecently); err == nil && scanRecently {
		return
	}
	bucket := time.Now().Unix() / (15 * 60)
	tag, err := worker.db.Exec(ctx, `
		INSERT INTO control_commands (
			actor_type, command_type, resource_type, resource_id,
			arguments, sanitized_arguments, idempotency_key, status
		) VALUES (
			'SYSTEM', 'autogrid.scan', 'autogrid', $1,
			'{"cascadeShort": true}'::jsonb, '{}'::jsonb, $2, 'QUEUED'
		)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, settings.ID, fmt.Sprintf("cascade-short-%s-%d", settings.ID, bucket))
	if err != nil {
		worker.logger.Error("queue cascade-short scan",
			"component", "autogrid_worker", "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		worker.logger.Warn("cascade-short scan queued: forced unwind window detected",
			"component", "autogrid_worker", "usd_1h", cascadeUSD)
		worker.noteDeployBlock(ctx, fmt.Sprintf(
			"каскад ликвидаций $%.0fM/час: внеочередной SHORT-скан поставлен (LONG/NEUTRAL на паузе)",
			cascadeUSD/1_000_000))
	}
}

// scheduledScanArguments builds the queued scheduled-scan command's
// arguments literal. Both variants are compile-time constants — the value
// never interpolates operator input, only which literal is selected.
func scheduledScanArguments(cascadeActive bool) string {
	if cascadeActive {
		return `'{"cascadeShort": true}'::jsonb`
	}
	return `'{}'::jsonb`
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
	// While the same long-liquidation cascade window that triggers the
	// out-of-turn scan is open, the SCHEDULED scan must carry cascadeShort
	// semantics too — otherwise its shorts are cut by R1/F9, the very gates
	// the cascade window is the designed exemption for. Same detector call
	// and threshold as maybeQueueCascadeShortScan.
	cascadeActive, _ := worker.CheckLiquidationCascade(ctx, 50_000_000)
	_, err = worker.db.Exec(ctx, `
		INSERT INTO control_commands (
			actor_type, command_type, resource_type, resource_id,
			arguments, sanitized_arguments, idempotency_key, status
		) VALUES (
			'SYSTEM', 'autogrid.scan', 'autogrid', $1,
			`+scheduledScanArguments(cascadeActive)+`, '{}'::jsonb, $2, 'QUEUED'
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
// buildLLMMarketContext assembles the free-source regime block for the
// candidate prompt: next USD high event / FOMC window, Fear&Greed, BTC 24h,
// intraday VIX/DXY from the v2.0.59 collectors. Best-effort per leg — a dead
// feed thins the block, never blocks the audit.
func (worker *Worker) buildLLMMarketContext(ctx context.Context) *llm.MarketContext {
	mc := &llm.MarketContext{}
	var evTitle string
	var evInMinutes int
	if err := worker.db.QueryRow(ctx, `
		SELECT title, FLOOR(EXTRACT(EPOCH FROM (event_time - NOW())) / 60)
		FROM economic_events
		WHERE impact = 'High' AND (country = 'USD' OR country IS NULL OR country = '')
		  AND event_time > NOW() AND event_time < NOW() + INTERVAL '24 hours'
		ORDER BY event_time LIMIT 1
	`).Scan(&evTitle, &evInMinutes); err == nil {
		mc.NextEventTitle = evTitle
		mc.NextEventInMin = evInMinutes
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT FLOOR(EXTRACT(EPOCH FROM (decision_at - NOW())) / 60)
		FROM fomc_meetings
		WHERE decision_at > NOW() AND decision_at < NOW() + INTERVAL '7 days'
		ORDER BY decision_at LIMIT 1
	`).Scan(&mc.FomcInMin); err != nil {
		mc.FomcInMin = 0
	}
	var fng int
	if err := worker.db.QueryRow(ctx, `
		SELECT value FROM sentiment_snapshots
		WHERE source = 'fng' AND captured_at > NOW() - INTERVAL '36 hours'
		ORDER BY captured_at DESC LIMIT 1
	`).Scan(&fng); err == nil {
		mc.FearGreed = &fng
	}
	if latest, _, err := marketdata.LatestCoinGeckoWindow(ctx, worker.db, time.Hour); err == nil && latest != nil {
		btc := latest.BTC24hPct
		mc.BTC24hPct = &btc
	}
	var vix, dxy float64
	if err := worker.db.QueryRow(ctx, `
		SELECT value::FLOAT8 FROM macro_snapshots
		WHERE metric = 'VIX' AND captured_at > NOW() - INTERVAL '3 hours'
		ORDER BY captured_at DESC LIMIT 1
	`).Scan(&vix); err == nil {
		mc.VIX = &vix
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT value::FLOAT8 FROM macro_snapshots
		WHERE metric = 'DXY' AND captured_at > NOW() - INTERVAL '3 hours'
		ORDER BY captured_at DESC LIMIT 1
	`).Scan(&dxy); err == nil {
		mc.DXY = &dxy
	}
	rows, err := worker.db.Query(ctx, `
		SELECT title FROM news_headlines
		WHERE captured_at > NOW() - INTERVAL '6 hours'
		ORDER BY captured_at DESC LIMIT 3
	`)
	if err == nil {
		for rows.Next() {
			var title string
			if rows.Scan(&title) == nil {
				mc.Headlines = append(mc.Headlines, title)
			}
		}
		rows.Close()
	}
	return mc
}

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
	// v2.0.59: one market-context build per scan — the auditor used to see
	// only the technical matrix and its own live search.
	marketCtx := worker.buildLLMMarketContext(ctx)
	auditedCount := 0
	// v2.0.19: the audit cap must never be the reason free slots stay empty.
	// deployReal hard-fails unaudited candidates, so with N free slots the
	// bottom (N−5) ACCEPTED candidates could NEVER deploy (prod: 7 ACCEPTED
	// / 7 free slots at cap 5). Raise the cap to cover the free slots.
	auditCap := 5
	var runningBots int
	countTable := "paper_grid_bots"
	if settings.ExecutionMode != "PAPER" {
		countTable = "grid_bots"
	}
	if err := worker.db.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE status = 'RUNNING'`, countTable,
	)).Scan(&runningBots); err == nil {
		if free := settings.MaxActiveBots - runningBots; free > auditCap {
			auditCap = free
		}
	}
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" || auditedCount >= auditCap {
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
			MarketContext:       marketCtx,
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
