package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
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

// trancheMarkerFresh reports whether an RFC3339 model_state marker (written
// with the to_char UTC pattern) is younger than maxAge. Fresh invest intents
// fence fail-closed paths that cannot verify the remote outcome.
func trancheMarkerFresh(marker *string, maxAge time.Duration) bool {
	if marker == nil {
		return false
	}
	stamped, err := time.Parse(time.RFC3339, strings.TrimSpace(*marker))
	return err == nil && time.Since(stamped) < maxAge
}

// tranchePourGateRefused (v2.0.142, audit P2d) classifies an AdjustBot
// invest_in error as a GATE refusal: one of the durable pre-flight gates the
// service invest lane runs BEFORE any native exchange call — the joint
// circuit breaker, the v2.0.140 margin reserve, the risk engine. Such an
// error proves the pour never reached Pionex, so the caller may clear the
// trancheIntentAt fence and let the 1h trancheFailAt backoff govern the
// retry. The prefixes are the exact reason families service.go's invest
// lane emits (circuitBreakerReason / marginReserveBlocker / the risk wrap);
// a native exchange refusal (ErrNativeAdjustRefused) is NOT gate-class —
// its outcome handling stays with the exchange classifier — and a persist
// failure after a successful native call ("persist adjustment: …") matches
// none of these, keeping the fence armed exactly as v2.0.78 CRIT-2 requires.
func tranchePourGateRefused(err error) bool {
	if err == nil || errors.Is(err, ErrNativeAdjustRefused) {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "circuit breaker:") ||
		strings.HasPrefix(msg, "резерв маржи:") ||
		strings.HasPrefix(msg, "invest_in rejected by risk engine:")
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
// v2.0.89 part B: GRID_AGED_HALF_LIFE joins the exempts — the OU half-life
// rotation is a PLANNED thesis-expiry exit, not a death: it must neither
// lock the symbol out of the scanner's next entry nor feed the portfolio
// breaker (a synchronized age wave would otherwise freeze deploys for an
// hour with zero protective meaning).
const protectiveCloseExemptReasons = `'TAKE_PROFIT', 'TAKE_PROFIT_NATIVE', 'TAKE_PROFIT_PRICE_HIT', 'TRAILING_TAKE_PROFIT', 'BREAKEVEN_LOCK',
	'SMART_PROFIT_HARVEST_OFI', 'RANGE_BREAK_UP_PROFIT_TAKE', 'GRID_AGED_HALF_LIFE', 'OU_HALFLIFE_ROTATION',
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
	// priceFeedBlindSince / priceFeedBlindLastAlarm track the v2.0.149
	// price-feed blindness episode (exit audit F1): non-nil Since while any
	// supervised bot has no price, EMERGENCY page after 10 minutes repeated
	// at most hourly (same single-goroutine argument as trancheTBRegime —
	// the manage loop is the only writer). priceFeedBlindLastPass carries
	// "the previous pass ended blind" across passes so a partial outage
	// (healthy map, missing symbol) cannot close the episode early (review
	// P2-1: episode churn kept the 10-minute page forever unreachable).
	priceFeedBlindSince     *time.Time
	priceFeedBlindLastAlarm time.Time
	priceFeedBlindLastPass  bool
	// radarPriceTrail is the in-memory symbol→(ts,price) pair behind the
	// v2.0.85 band-2 velocity trigger: two consecutive radar passes measure
	// the speed toward the adverse edge (same single-goroutine argument as
	// trancheTBRegime — radarPass runs on the manage loop). Deliberately not
	// durable: it is a measurement, not a cooldown.
	radarPriceTrail map[string]radarPricePoint
	// ouReadings caches the per-symbol OU half-life + fresh ATR reading the
	// grid lifecycle policy shares between the half-life age rotation and
	// the DGT re-deploy geometry (v2.0.89 part B). TTL 30m; same
	// single-goroutine argument as trancheTBRegime — the manage loop owns it.
	// wsLane is the v2.0.98 advisory public WebSocket (INDEX markPrice) —
	// nil until startWSLane, all consumers nil-check.
	ouReadings map[string]ouSymbolReading
	wsLane     *pionex.PublicStream
	ofiEngine  *marketdata.OFIEngine
	// ofiStatsAt throttles the periodic OFI ingest-stats log (single manage
	// goroutine → plain field); v2.0.137 lane observability.
	ofiStatsAt time.Time
	// ofiStatsPrev holds the previous ingest-stats snapshot so the periodic
	// log can emit per-interval _5m rates instead of only cumulative totals
	// (same single manage goroutine → plain field); v2.0.137 lane observability.
	ofiStatsPrev marketdata.OFIEngineStats
	// gateValueAt throttles the daily per-gate shadow counterfactual report
	// (package D, v2.0.139) to one per 24h (single manage goroutine → plain
	// field). The report builder itself lives in gate_value.go.
	gateValueAt time.Time
	// terminalRecheckAt throttles the v2.0.99 finished-record re-check sweep
	// (single manage goroutine → plain field).
	terminalRecheckAt time.Time
	// terminalReopenDone gates the one-time v2.0.99 upgrade heal that reopens
	// estimate-class finals the old binary froze as confirmed.
	terminalReopenDone bool
	// terminalIdentityHealDone gates rechecking explicitly identified legacy
	// calculations without discarding their previous financial result.
	terminalIdentityHealDone bool
	// shiftOffsetHealDone gates the one-time v2.0.107 heal that seeds the
	// inventory cost-basis offset for bots that shifted range under earlier
	// versions (AAVE #1288).
	shiftOffsetHealDone bool
	// v127ScreenParityHealDone gates the one-time v2.0.127 heal that restores
	// screen parity with Pionex for shifted bots (OP #1427, AR #1422, EDGE #1416).
	v127ScreenParityHealDone bool
	// terminalRawLogged dedups the v2.0.100 raw-payload witness to one line
	// per pending row (single manage goroutine → plain map).
	terminalRawLogged map[string]bool
	// v2.0.111 storm mode: ≥3 fleet symbols tripping the realtime trigger
	// within 5 minutes defer rotations/deploys for a rolling 30 minutes.
	// Written from the WS callback goroutine, read from the manage loop —
	// guarded by stormMu.
	stormMu       sync.RWMutex
	stormTriggers map[string]time.Time
	stormUntil    time.Time
	stormLoggedAt time.Time
	// gateUnreadableStreak counts consecutive entry passes whose advisory
	// gates were SQL-unreadable (v2.0.167): three in a row (~15m of scan
	// cadence) pages the operator; lastGateUnreadableAlarmAt dedups hourly.
	gateUnreadableStreak      int
	lastGateUnreadableAlarmAt time.Time
	// lastCapitalAlarmAt throttles the v2.0.145 capital-deficiency telegram
	// to one per hour (single manage goroutine → plain field).
	lastCapitalAlarmAt time.Time
	// fleetStormSet pins the v2.0.111 storm sensor to the RUNNING fleet
	// (v2.0.144: pre-warmed candidate symbols must not arm market-wide
	// storms). Managed under stormMu; empty set = legacy count-everything.
	fleetStormSet map[string]struct{}
	// runningRawLogged dedups the v2.0.101 raw-payload witness for RUNNING
	// grids to one line per (bot, adjustments) pair.
	runningRawLogged map[string]bool
	// v2.0.102 event-driven supervision: the WS lane signals a sharp move on
	// a fleet symbol and the main loop runs an out-of-band manage pass.
	realtimeSignal chan string
	realtimeMu     sync.RWMutex
	realtimeWatch  map[string]realtimePoint
	lastEventPass  time.Time
	// orphanSweepAt throttles the v2.0.104 exchange→DB orphan adoption
	// sweep (single manage goroutine → plain field).
	orphanSweepAt time.Time

	emergencyExitMu         sync.Mutex
	emergencyExits          map[string]time.Time
	emergencyShadowDebounce map[string]time.Time
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
	w := &Worker{
		db: db, service: service, accounts: accountService, risk: riskEngine,
		scanner: marketdata.NewScanner(publicClient), publicClient: publicClient,
		market:                  marketdata.NewService(db),
		llm:                     llmService,
		logger:                  logger,
		owner:                   fmt.Sprintf("autogrid-%d", time.Now().UnixNano()),
		trancheTBRegime:         make(map[string]trancheTBTrend),
		dataAlarmAt:             make(map[string]time.Time),
		radarPriceTrail:         make(map[string]radarPricePoint),
		ouReadings:              make(map[string]ouSymbolReading),
		realtimeSignal:          make(chan string, 1),
		realtimeWatch:           make(map[string]realtimePoint),
		ofiEngine:               marketdata.NewOFIEngine(marketdata.DefaultOFIEngineConfig()),
		emergencyExits:          make(map[string]time.Time),
		emergencyShadowDebounce: make(map[string]time.Time),
	}
	service.SetLivePriceResolver(w.LiveMarkPrice)
	return w
}

func (worker *Worker) isEmergencyExitDebounced(botID string) bool {
	worker.emergencyExitMu.Lock()
	defer worker.emergencyExitMu.Unlock()
	if worker.emergencyExits == nil {
		return false
	}
	until, ok := worker.emergencyExits[botID]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(worker.emergencyExits, botID)
	return false
}

func (worker *Worker) armEmergencyExitDebounce(botID string, duration time.Duration) {
	worker.emergencyExitMu.Lock()
	defer worker.emergencyExitMu.Unlock()
	if worker.emergencyExits == nil {
		worker.emergencyExits = make(map[string]time.Time)
	}
	worker.emergencyExits[botID] = time.Now().Add(duration)
}

func (worker *Worker) isEmergencyShadowDebounced(botID string) bool {
	worker.emergencyExitMu.Lock()
	defer worker.emergencyExitMu.Unlock()
	if worker.emergencyShadowDebounce == nil {
		return false
	}
	until, ok := worker.emergencyShadowDebounce[botID]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(worker.emergencyShadowDebounce, botID)
	return false
}

func (worker *Worker) armEmergencyShadowDebounce(botID string, duration time.Duration) {
	worker.emergencyExitMu.Lock()
	defer worker.emergencyExitMu.Unlock()
	if worker.emergencyShadowDebounce == nil {
		worker.emergencyShadowDebounce = make(map[string]time.Time)
	}
	worker.emergencyShadowDebounce[botID] = time.Now().Add(duration)
}

// LiveMarkPrice returns the freshest known WebSocket mark price for a symbol.
func (worker *Worker) LiveMarkPrice(symbol string) (decimal.Decimal, bool) {
	if worker.wsLane != nil {
		if update, ok := worker.wsLane.Mark(symbol); ok && update.Fresh(45*time.Second) && update.MarkPrice.IsPositive() {
			return update.MarkPrice, true
		}
		trimmed := trimPERPAlias(symbol)
		if update, ok := worker.wsLane.Mark(trimmed); ok && update.Fresh(45*time.Second) && update.MarkPrice.IsPositive() {
			return update.MarkPrice, true
		}
		if update, ok := worker.wsLane.Mark(trimmed + "_PERP"); ok && update.Fresh(45*time.Second) && update.MarkPrice.IsPositive() {
			return update.MarkPrice, true
		}
	}
	return decimal.Zero, false
}

func (worker *Worker) Run(ctx context.Context) {
	commandTicker := time.NewTicker(time.Second)
	scheduleTicker := time.NewTicker(10 * time.Second)
	reconcileTicker := time.NewTicker(30 * time.Second)
	defer commandTicker.Stop()
	defer scheduleTicker.Stop()
	defer reconcileTicker.Stop()
	worker.startWSLane(ctx)
	// v2.0.138 entry chain: expose the worker-owned storm window to the
	// service layer (manual deploy / invest_in run the shared market-blocker
	// composer without a Worker). Same process, set once before the loop;
	// a service built without a worker keeps the documented nil default.
	if worker.service != nil {
		worker.service.StormActive = worker.stormActive
	}
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
		// v2.0.83 bot-aggregate equity: the first snapshot of the process
		// must land at startup, not one manage interval later — the running
		// PnL columns were persisted by the previous process's passes.
		worker.runGuarded("equity_snapshot", func() {
			worker.captureBotAggregateEquity(ctx, *settings)
		})
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
		case symbol := <-worker.realtimeSignal:
			worker.handleRealtimeSignal(symbol, reconcileTicker)
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
		   OR (status = 'EXECUTING' AND lease_owner IS DISTINCT FROM $1)
	`, worker.owner)
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
	// v2.0.147: while the beta gate reads BTC TREND_DOWN, the scanner lifts
	// the SHORT anti-FOMO floors for pair-confirmed downtrends (counterfactual
	// 2026-09-28: those cuts fell 2.4:1 in the short's favor while NEUTRAL
	// bled). The loop re-reads the same cached regime at deploy time — the
	// fresher read wins and any disagreement resolves conservatively (a
	// scan-time down / deploy-time up split re-cuts the candidate at the
	// beta block; the reverse only admits floor-passing shorts).
	if _, betaDownScan := betaGateTrend(worker.marketBetaRegime(ctx)); betaDownScan {
		scanConfig.BetaDownShortMode = true
	}
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
	// v2.0.143 (audit-2): no updated_at bump — last_error is a UI note, not
	// a settings-semantic change, and ConfigVersion hashes updated_at (a
	// deploy-block note must not mint a new cv for unchanged parameters).
	// Matches the deployErrors writers in manageDeployments, which never
	// bumped it either.
	_, _ = worker.db.Exec(ctx, `
		UPDATE autogrid_settings SET last_error = $1
	`, reason)
}

// losingSymbolStats is one row of the cumulative symbol-cooldown preload.
type losingSymbolStats struct {
	Closes  int
	NetUSDT float64
}

// loadLosingSymbolCooldowns (v2.0.94) returns the repeat-loser cohort: symbols
// whose paper ledger over the trailing 7 days shows 3+ completed closes with
// a negative cumulative net, or 2 closes already −$2+ deep. Weekly mining of
// the 155-outcome week: ENA (−$4.06/2), ZEC (−$2.75/3), XMR (−$2.68/4) and
// SNXXX (−$2.38/5) drained −$11.9 of the −$32.5 gross loss purely by
// re-entering after every half-life rotation — each individual close was a
// well-behaved exit, so the per-close dist-aware cooldown never engaged.
// The window rolls, so the block self-heals as old losses age out; both
// deploy modes read the SAME paper ledger (the strategy's evidence base),
// keeping paper statistics transferable to REAL. A preload failure disarms
// the gate for this round (logs loudly) instead of wedging the fleet.
func (worker *Worker) loadLosingSymbolCooldowns(ctx context.Context, settingsID string) map[string]losingSymbolStats {
	cooled := map[string]losingSymbolStats{}
	rows, err := worker.db.Query(ctx, `
		SELECT symbol, COUNT(*)::int, COALESCE(SUM(realized_pnl_usdt), 0)::float8
		FROM paper_grid_bots
		WHERE settings_id = $1
		  AND status = 'COMPLETED'
		  AND closed_at > NOW() - INTERVAL '7 days'
		GROUP BY symbol
		HAVING (COUNT(*) >= 3 AND SUM(realized_pnl_usdt) < 0)
		    OR (COUNT(*) >= 2 AND SUM(realized_pnl_usdt) <= -2.0)
	`, settingsID)
	if err != nil {
		worker.logger.Warn("symbol cooldown preload failed — gate disarmed for this round",
			"component", "autogrid_worker", "error", err)
		worker.noteDeployBlock(ctx, "кулдаун проигрывающих символов не загружен (сбой БД) — гейт разоружён на этот раунд")
		return cooled
	}
	defer rows.Close()
	for rows.Next() {
		var symbol string
		var stats losingSymbolStats
		if err := rows.Scan(&symbol, &stats.Closes, &stats.NetUSDT); err == nil {
			cooled[symbol] = stats
		}
	}
	return cooled
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
		// The AI Kit count is advisory geometry, but it must respect the
		// fee-gate floor like every other density source: an unclamped AI
		// count (spot grids of 100-500 levels over a narrow span) produced
		// 0.08-0.13% steps that only died at the deploy gate — the whole
		// fleet starved (prod 2026-09-05, zero deploys). v2.0.93: the ceiling
		// is the FULL margin-density doctrine count — GridLevelsForRange over
		// the candidate span at the settings fees (FIX-A: the floor follows
		// the operator's feeBps/slippageBps, not the pinned default) and the
		// $8/level notional cap — so an adopted grid is born viable AND
		// margin-dense (FIX-K: adoption floor is the doctrine's 6 levels, not
		// the old 2).
		spanPct := candidate.UpperPrice.Sub(candidate.LowerPrice).
			Div(candidate.LowerPrice).InexactFloat64() * 100
		notional := settings.BudgetUSDT.
			Mul(decimal.NewFromInt(int64(settings.Leverage))).InexactFloat64()
		adopted := clampAIGridCount(spanPct, notional,
			decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps), strategy.GridCount)
		boundary := "AI_GRID_COUNT_ADOPTED_WITH_CLAMP_2_500_RANGE_STAYS_SCANNER_SR"
		if strategy.GridCount != adopted {
			boundary = fmt.Sprintf(
				"AI_GRID_COUNT_CLAMPED_TO_FEE_GATE_%d_OF_%d_RANGE_STAYS_SCANNER_SR",
				adopted, strategy.GridCount)
		}
		if _, err := worker.db.Exec(ctx, `
			UPDATE autogrid_candidates
			SET model_assumptions = model_assumptions || $3::jsonb,
			    grid_num = $4::INT
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
				"boundary":        boundary,
			},
		}, adopted); err != nil {
			return fmt.Errorf("persist AI Kit advisory for %s: %w", candidate.Symbol, err)
		}
		enriched++
	}
	return nil
}

// clampAIGridCount folds an adopted native AI Kit level count into the
// margin-density doctrine (v2.0.93 FIX-K): the ceiling is the doctrine count
// for the candidate span — the fee-gate floor at the ACTUAL settings fees
// (FIX-A) plus the $8-per-level notional cap — and the floor is the
// doctrine's own 6 levels (a grid stays a grid), not the old 2. An adopted
// grid is therefore born viable AND margin-dense.
func clampAIGridCount(spanPct, notionalUSDT, feeBps, slippageBps float64, aiCount int) int {
	ceiling := marketdata.GridLevelsForRange(spanPct, notionalUSDT, feeBps, slippageBps)
	adopted := aiCount
	if adopted > ceiling {
		adopted = ceiling
	}
	if adopted < marketdata.GridLevelsMin {
		adopted = marketdata.GridLevelsMin
	}
	return adopted
}

// computeBotTargets derives the per-bot PnL target and stop-out (see
// computeBotTargetsWithStress). Thin v2.0.139 compatibility wrapper for the
// callers with no deployed geometry in hand (DGT re-center stubs, shadow
// portfolio): a zero stressGeometry disables the stress floor, so the
// numbers are exactly the pre-v2.0.139 derivation.
func computeBotTargets(settings Settings, candidate Candidate, leverage int, rangeSpanPct float64) (*decimal.Decimal, *decimal.Decimal) {
	target, loss, _ := computeBotTargetsWithStress(settings, candidate, leverage, rangeSpanPct, stressGeometry{})
	return target, loss
}

// computeBotTargetsWithStress derives the per-bot PnL target and stop-out. In
// DYNAMIC mode the numbers come from the pair's own readings (native AI Kit
// estimate when enriched, otherwise the scanner sigma/ATR blend and model
// drawdown), scaled to the budget and the FINAL leverage (v2.0.19: the PnL
// model marks directional positions on budget×leverage notional — unscaled
// stops died in noise, prod SKHY #328); FIXED mode returns the operator's
// amounts verbatim. rangeSpanPct is the deployed mesh span in % — it floors
// the stop-out above a full normal traverse (v2.0.24); 0 skips the floor.
//
// v2.0.139 stress-inventory floor (package C1): the dynamic cap can legally
// land BELOW the mark-to-market of a full adverse traverse to the anti-hunt
// stop (NEAR #1401 settled −$14.90 on a ~$10 cap) — geo carries the deployed
// geometry, and when the simulated traverse loss exceeds the derived max-loss
// the floor lifts maxLoss to it (stress.floored marks the telemetry). The
// stress loss itself is returned for BOTH modes so the deploy gates can
// reject geometries whose traverse risk overflows the budget ceiling.
func computeBotTargetsWithStress(settings Settings, candidate Candidate, leverage int, rangeSpanPct float64, geo stressGeometry) (*decimal.Decimal, *decimal.Decimal, stressResult) {
	stress := stressResult{loss: stressInventoryLossUSDT(
		geo.direction, geo.entry, geo.lower, geo.upper, geo.stop,
		geo.gridNum, geo.invest, leverage)}
	if settings.PnLTargetMode != "DYNAMIC" {
		if settings.PnLTargetUSDT.IsZero() || settings.MaxLossUSDT.IsZero() {
			return nil, nil, stress
		}
		target, loss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		return &target, &loss, stress
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
	// v2.0.139: the floor, not the ceiling, is the missing bound — the stop
	// must at least cover what the grid's own geometry loses on a full
	// adverse traverse to the anti-hunt stop.
	if stress.loss.GreaterThan(loss) {
		loss = stress.loss.Round(2)
		stress.floored = true
	}
	return &target, &loss, stress
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
	var activeCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID).Scan(&activeCount); err != nil {
		return fmt.Errorf("count active paper grids: %w", err)
	}
	// v2.0.138 entry chain: the fleet-wide market gates — storm → portfolio
	// circuit breaker → economic events → liquidation cascade + feed health —
	// now run through ONE shared composer (evaluateSharedMarketBlockers,
	// entry_chain.go): the same sequence every entry path clears, closing the
	// "each lane hand-copied its own subset" drift class. Storm, the breaker
	// and economic events block the WHOLE pass (every candidate defers);
	// the cascade/feed legs stay direction-aware and only NOTE here — the
	// per-candidate cut inside the loop still lets SHORT candidates through.
	// The macro veto judges each candidate's scanner trend and therefore runs
	// per candidate, with the final direction known.
	// v2.0.142 (audit P2c): the fleet-constant legs (breaker, economic
	// events) are memoized per pass — the pass-level probe below fills the
	// cache and every per-candidate cut reuses it.
	passBlockers := &marketBlockerCache{}
	passCode, _, passNote, passFeatures := worker.probeSharedMarketBlockersCached(ctx, EntryChainInput{
		Path: EntryPathScannerPaper, Settings: settings, Fleet: "PAPER", Direction: "NEUTRAL",
	}, passBlockers)
	switch passCode {
	case "":
		// clear
	case entryBlockedStorm:
		// v2.0.111 storm deferral (paper parity): opening fresh grids into a
		// fleet-wide acceleration buys the worst entries of the day; the scan
		// retries in four minutes and the storm window is rolling.
		worker.logger.Info("paper deploy deferred by storm mode", "component", "autogrid_worker")
		return nil
	case entryWaitGateUnreadable:
		worker.logger.Warn("paper pass deferred: advisory gates SQL-unreadable (WAIT)",
			"component", "autogrid_worker", "note", passNote)
		return nil
	case entryBlockedCircuitBreaker:
		worker.logger.Warn("Portfolio circuit breaker: recent stop-losses holding new deployments",
			"component", "autogrid_worker", "recentStopLossCount", passFeatures["closes"])
		worker.noteDeployBlock(ctx, passNote)
		return nil
	case entryBlockedEconomicEvent:
		worker.logger.Warn("paper deploy blocked by economic event",
			"component", "autogrid_worker", "reason", passFeatures["title"])
		worker.noteDeployBlock(ctx, passNote)
		return nil
	case entryBlockedMacro:
		// Per-candidate leg: the macro veto judges each candidate's SCANNER
		// trend (shorts exempt) inside the loop — nothing to defer here.
	default:
		// CASCADE / FEED_HEALTH: LONG/NEUTRAL defer, SHORT stay live — note
		// only, the per-candidate cut below decides per direction.
		worker.logger.Warn("liquidation gate: LONG/NEUTRAL paper deploys paused, SHORT stay live",
			"component", "autogrid_worker", "code", passCode, "usd_1h", passFeatures["usd_1h"],
			"block", passNote)
		worker.noteDeployBlock(ctx, passNote)
	}
	fng, _ := worker.GetFearGreed(ctx)
	// v2.0.21 global beta gate: BTC's tape gates altcoin NEUTRAL/LONG
	// entries — the 2026-08-20 stops were local-RANGE alts loading long
	// inventory into a market-wide bleed.
	betaName, betaADX, betaSlope := worker.marketBetaRegime(ctx)
	betaDown, betaUp := betaGateTrend(betaName, betaADX, betaSlope)
	backtestGateOn := worker.backtestGateEnabled(ctx)
	// v2.0.93 FIX-F (paper/REAL parity): the LLM-audit gate the REAL path
	// runs since the "top-5 bypass" fix. Paper used to deploy unaudited
	// candidates while REAL refused them — paper statistics then proved a
	// pipeline REAL would have blocked, the exact failure the backtest-gate
	// parity comment above describes. Fail-open when the LLM brain is not
	// wired (worker.llm == nil — embedded/test builds): an absent reviewer
	// must not wedge the whole paper fleet.
	llmBrainEnabledPaper := false
	if worker.llm != nil {
		if llmSettings, err := worker.llm.GetSettings(ctx); err == nil {
			llmBrainEnabledPaper = llmSettings.Enabled && strings.TrimSpace(llmSettings.APIKey) != ""
		}
	}
	// v2.0.94 cumulative symbol cooldown: one preload, membership check per
	// candidate — same gate, same text in the REAL loop below.
	losingSymbols := worker.loadLosingSymbolCooldowns(ctx, settings.ID)
	for _, candidate := range candidates {
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		if st, cooled := losingSymbols[candidate.Symbol]; cooled {
			worker.rejectCandidate(ctx, candidate, fmt.Sprintf(
				"символьный кулдаун: %d закрытий, нетто −$%.2f за 7д — повторные входы заморожены до выхода окна из минуса",
				st.Closes, -st.NetUSDT), nil)
			continue
		}
		if !isEntryTimingFavorable(candidate) {
			v := directionalTrendExempt(candidate, betaDown, betaUp)
			if !v.Exempt {
				worker.rejectCandidate(ctx, candidate,
					"вход-тайминг: текущая позиция в канале вне благоприятной зоны для этого направления", nil)
				continue
			}
			// v2.0.147/v2.0.161 attribution marker: this directional entry
			// happened ONLY because the confirmed-trend exemption lifted
			// entry-timing — persisted into model_assumptions so the 14-day
			// follow-up can partition realized outcomes by entry cohort
			// (betaDownExempt = prod-proven; dirTrend* = v2.0.161 wider class).
			candidate.ModelAssumptions[v.Cohort] = true
		}
		// LLM audit gate (FIX-F): same rule, same text as the REAL branch.
		if llmBrainEnabledPaper && candidate.ModelAssumptions["llmAuditId"] == nil {
			worker.logger.Warn("skip paper deploy: no completed LLM audit for candidate",
				"component", "autogrid_worker", "symbol", candidate.Symbol)
			worker.rejectCandidate(ctx, candidate, "AI-аудит не завершён для этого кандидата (кап аудита/сбой LLM) — деплой заблокирован", nil)
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
			if time.Since(*lastProtectiveAt) < window &&
				// v2.0.140 (review P1-1): the cascade-short lane is exempt
				// when the cooldown is armed SOLELY by the flip family — the
				// SHORT is the designed harvest of the exact tape that
				// closed the NEUTRAL, and the flip budget (1/24h) is the
				// binding anti-saw guard. Without this the flip experiment
				// can never re-enter its own symbol (its triggers arm a 2h+
				// cooldown the cascade lane did not bypass).
				!(cascadeShort && strings.EqualFold(strings.TrimSpace(candidate.RecommendedTrend), "short") &&
					worker.cascadeFlipCooldownExemptPaper(ctx, settings.ID, candidate.Symbol)) {
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
		// itself. v2.0.148: a beta-down pair-confirmed short gets the same
		// treatment — a downward drift since scan is the short's payload;
		// the fresh price re-anchors geometry instead of voiding the entry.
		if freshPrice, ok := worker.revalidateFreshPrice(ctx, &candidate, atrPct); ok || ((cascadeShort || betaDownShortExempt(candidate, betaDown)) && freshPrice.IsPositive()) {
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
			atrPct, regime, geometryBudget, settings.Leverage,
			decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps),
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
			if v := directionalTrendExempt(candidate, betaDown, betaUp); v.Exempt {
				// v2.0.148/v2.0.161: the RV gate is direction-blind by design,
				// but for a pair with its own CONFIRMED trend the "expansion"
				// IS the move the directional grid is paid to ride
				// (counterfactual 2026-09-28: the cut short cohort fell a
				// median −1.09%/2h, 4.3:1). Stamp the cohort; every later
				// gate stays armed.
				candidate.ModelAssumptions[v.Cohort] = true
			} else {
				worker.logger.Info("entry gate: volatility expansion, skip",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"rv_ref_ratio", math.Round(ratio*100)/100)
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("entry gate: расширение волатильности (RV/базлайн %.2f ≥ 1.5) — вход в ускорение заблокирован", math.Round(ratio*100)/100), nil)
				continue
			}
		}
		if harGeo != nil {
			harGeo.applyToMesh(candidate.CurrentPrice, &mesh)
		}
		// v2.0.163 wide-grid doctrine (paper mirror): ≥8% span after every
		// geometry source; levels keep, steps widen with the span.
		mesh.LowerPrice, mesh.UpperPrice = EnsureDeploySpan(mesh.LowerPrice, mesh.UpperPrice, candidate.CurrentPrice)

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
		// v2.0.138 entry chain: the shared market-blocker composer now owns
		// the per-candidate fleet gates — liquidation cascade + feed health
		// (direction-aware against the FINAL trend: SHORT stays live, the
		// smart override included) and the macro veto (judging the SCANNER
		// trend with the cascade-short exemption, exactly the inputs each
		// leg ran before). Storm, the portfolio breaker and economic events
		// were cleared once per pass above. Plan order puts the cascade
		// ahead of the macro veto, so a candidate hitting both now names the
		// cascade — both statements were always true.
		entryIn := EntryChainInput{
			Path: EntryPathScannerPaper, Settings: settings, Symbol: candidate.Symbol,
			Direction: strings.ToUpper(trend), Fleet: "PAPER", RefID: candidate.ID,
			ScannerTrend: strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)),
			CascadeShort: cascadeShort,
		}
		if code, reason, _, features := worker.probeSharedMarketBlockersCached(ctx, entryIn, passBlockers); code != "" {
			if code == entryWaitGateUnreadable {
				// v2.0.167: infrastructure unreadable — WAIT this pass
				// WITHOUT a market REJECT (analytics must not count it).
				worker.journalEntryDecision(ctx, entryIn, entryOutcomeWait, code, reason, features)
				continue
			}
			worker.rejectCandidate(ctx, candidate, reason, features)
			worker.journalEntryDecision(ctx, entryIn, entryOutcomeReject, code, reason, features)
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
		// v2.0.93 FIX-I (paper/REAL parity): the same ±2% boundary clamp the
		// REAL deploy applies after ComputeAntiHuntStop. A degenerate ATR
		// (near-zero reading) parks the raw stop INSIDE the range — a stop a
		// grid immediately crosses — while REAL pushed it back out; paper
		// then stored a stop its own distance check below had to reject.
		antiHuntStop = ClampAntiHuntStopIntoBounds(trend, mesh.LowerPrice, mesh.UpperPrice, antiHuntStop)

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
		// designed escape valve and stays exempt. Quant Vision v3.0 allows
		// flip entry if a fresh confirmed price action pattern is formed.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") &&
			worker.directionalFlipBlocked(ctx, candidate.Symbol, strings.ToUpper(trend), true) {
			paConfirmed, paReason := worker.directionalConfirmedByPriceAction(ctx, candidate.Symbol, trend)
			if !paConfirmed {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("флип направления: символ закрыл бота другого направления ≤12ч назад и нет свечного паттерна (%s)", paReason), nil)
				continue
			}
			worker.logger.Info("paper directional flip lock bypassed by confirmed price action",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "pattern", paReason)
		}

		// v2.0.62 (R1) + Quant Vision v3.0: directional entry requires
		// either the matching confluence verdict OR confirmed candlestick price action
		// (Pin Bar, Engulfing, or SFP liquidity sweep). Cascade-shorts exempt.
		// v2.0.162 (unlock plan step 3): a confirmed own-trend impulse is
		// exempt too — a decisive trend prints reversal candles rarely (a
		// green/red series has no hammer), so the pattern requirement
		// converted every clean impulse entry into a NEUTRAL fallback.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") {
			verdict := candidateConfluenceVerdict(candidate.ModelAssumptions)
			want := "SUPPORT_SHORT"
			if trend == "long" {
				want = "SUPPORT_LONG"
			}
			if verdict != want {
				if v := directionalTrendExempt(candidate, betaDown, betaUp); v.Exempt {
					// v2.0.162 review P1: stamp the cohort — an R1-only
					// exemption must stay visible in the 14-day outcome
					// partition or the promised evidence-based rollback
					// cannot see this entry class.
					candidate.ModelAssumptions[v.Cohort] = true
					worker.logger.Info("paper directional entry approved via confirmed trend (R1 exempt)",
						"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "cohort", v.Cohort)
				} else {
					paConfirmed, paReason := worker.directionalConfirmedByPriceAction(ctx, candidate.Symbol, trend)
					if !paConfirmed {
						worker.rejectCandidate(ctx, candidate,
							fmt.Sprintf("R1 + Vision: направленный вход (%s) без подтверждения — confluence %s (нужно %s) и нет свечного паттерна (%s)",
								strings.ToUpper(trend), verdict, want, paReason), nil)
						continue
					}
					worker.logger.Info("paper directional entry approved via Price Action",
						"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "pattern", paReason)
				}
			}
		}

		// Fleet Net Delta Cap (Quant & Vision v3.0). v2.0.142 (audit P2a):
		// paper now charges NEUTRAL candidates exactly like the REAL gate —
		// the shared fleetCandidateDelta/projectedFleetDelta helpers keep the
		// two fleets byte-identical (paper/REAL parity holds in this lane;
		// the v2.0.93 FIX-G drift class is closed: the sandbox proves the
		// delta math REAL rides, neutral park and ½-notional included).
		candidateDelta := fleetCandidateDelta(trend, settings.BudgetUSDT, botLev)
		if settings.FleetMaxNetDeltaUSDT.IsPositive() && !candidateDelta.IsZero() {
			fleetDelta, neutralPark, err := worker.calculateFleetNetDelta(ctx, settings.ID, true)
			if err != nil {
				// v2.0.140 fail-closed parity: paper used to wave the
				// candidate through on a delta read error while REAL rejected
				// it — the sandbox drifted from the capital path exactly when
				// the DB was flakiest.
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("флот-дельта недоступна (fail-closed): %v — деплой отложен для защиты портфеля", err), nil)
				continue
			}
			projectedDelta := projectedFleetDelta(trend, fleetDelta, candidateDelta, neutralPark)
			if projectedDelta.GreaterThan(settings.FleetMaxNetDeltaUSDT) {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Fleet net delta cap: текущая дельта $%s + парк нейтральных $%s + кандидат $%s превысит лимит $%s — пауза входа для защиты портфеля",
						fleetDelta.StringFixed(2), neutralPark.StringFixed(2), candidateDelta.StringFixed(2), settings.FleetMaxNetDeltaUSDT.StringFixed(2)), nil)
				continue
			}
		}

		// Order Book Cushion Check (Quant & Vision v3.0) — feeds L2 depth into OFIEngine
		if settings.OrderbookProfilerEnabled {
			botNotional := settings.BudgetUSDT.Mul(decimal.NewFromInt(int64(botLev))).InexactFloat64()
			minCushion := settings.MinDepthCushionRatio.InexactFloat64()
			ok, profile, depthReason := worker.checkOrderBookCushion(ctx, candidate.Symbol, candidate.CurrentPrice, botNotional, minCushion, false, settings.MaxSpreadPct)
			if !ok {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Стакан: %s — отказ по фильтру тонкой ликвидности", depthReason), nil)
				continue
			}
			if trend != "short" && profile.HasBidWall && profile.BidWallPrice.GreaterThan(mesh.LowerPrice) && profile.BidWallPrice.LessThan(candidate.CurrentPrice) {
				worker.logger.Info("paper anchoring grid lowerPrice above order book bid wall",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"old_lower", mesh.LowerPrice.String(), "bid_wall", profile.BidWallPrice.String())
				mesh.LowerPrice = profile.BidWallPrice
			}
			// v2.0.163 review P1-1 (paper mirror): re-apply the 8% floor
			// after the anchor and gate the FINAL step. v2.0.164: round the
			// re-widened bounds to price precision (REAL-parity — the
			// unrounded twin fed the parity checkParams probe wrong shapes).
			paperPrec := 6
			if p, ok := candidate.ModelAssumptions["pricePrecision"].(float64); ok && p > 0 {
				paperPrec = int(p)
			} else if pInt, ok := candidate.ModelAssumptions["pricePrecision"].(int); ok && pInt > 0 {
				paperPrec = pInt
			} else if candidate.CurrentPrice.GreaterThan(decimal.Zero) {
				if exp := candidate.CurrentPrice.Exponent(); exp < 0 {
					paperPrec = int(-exp)
				}
			}
			if paperPrec > 8 {
				paperPrec = 8
			}
			if nl, nu := EnsureDeploySpan(mesh.LowerPrice, mesh.UpperPrice, candidate.CurrentPrice); !nl.Equal(mesh.LowerPrice) || !nu.Equal(mesh.UpperPrice) {
				nl = nl.Round(int32(paperPrec))
				nu = nu.Round(int32(paperPrec))
				if nu.GreaterThan(nl) {
					mesh.LowerPrice, mesh.UpperPrice = nl, nu
				}
			}
			if spanPct := mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).InexactFloat64(); mesh.GridNum > 0 {
				if stepPct := spanPct / float64(mesh.GridNum); stepPct > 0 {
					if reason, violated := marketdata.FeeGateRejection(stepPct, decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps)); violated {
						worker.rejectCandidate(ctx, candidate, "fee-gate (финальная геометрия после анкера): "+reason, nil)
						continue
					}
				}
			}
		}

		// Knife Pause & OFI Check (Quant & Vision v3.0)
		if settings.KnifePauseEnabled {
			paused, _, knifeReason := worker.checkKnifePause(ctx, candidate.Symbol, trend)
			if paused {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Knife Pause: %s — деплой отложен для защиты от падающего ножа", knifeReason), nil)
				continue
			}
		}

		confluence := EvaluateConfluence(candidate, nil, nil)

		gridType := mesh.GridType
		meshSpanPct := 0.0
		if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && candidate.CurrentPrice.IsPositive() {
			meshSpanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
		}
		target, maxLoss, stress := computeBotTargetsWithStress(settings, candidate, botLev, meshSpanPct, stressGeometry{
			direction: trend, entry: candidate.CurrentPrice,
			lower: mesh.LowerPrice, upper: mesh.UpperPrice, stop: antiHuntStop,
			gridNum: mesh.GridNum,
			// Full-slot budget, not the tranche-halved investAmount: the
			// stress model is linear in invest, so this is the SAME number
			// for the tranche-1 bot, while the post-top-up stop (the floor
			// doubled back by tranche-2) then covers the doubled inventory
			// too — sizing the floor on the half would re-open the exact
			// NEAR #1401 gap one top-up later.
			invest: settings.BudgetUSDT,
		})
		// v2.0.139 stress-inventory gate: the floor keeps the STORED stop
		// honest, but a geometry whose full-traverse loss overflows the
		// tranche-2 effective-stop ceiling (DynamicLossMaxPct ×
		// breakerHeadroom — the SAME budget every cap in the fleet derives
		// from) is not budget-sized at all: a bot admitted above it deploys
		// and then has its top-up structurally refused forever. FIXED-mode
		// targets are exempt — the operator set the stop deliberately.
		if settings.PnLTargetMode != "FIXED" {
			stressCeiling := tranche2MaxLossCap(settings.BudgetUSDT, botLev)
			if stress.loss.GreaterThan(stressCeiling) {
				worker.logger.Info("skip paper deploy: stress inventory exceeds loss ceiling",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"stress_loss", stress.loss.StringFixed(2), "ceiling", stressCeiling.StringFixed(2))
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("стресс-инвентарь: полный проход сетки до стопа $%s превышает допустимый убыток $%s — геометрия концентрирует риск больше бюджета",
						stress.loss.StringFixed(2), stressCeiling.StringFixed(2)),
					map[string]any{"stressLossFloor": map[string]any{
						"stressLossUsdt": stress.loss.StringFixed(2),
						"ceilingUsdt":    stressCeiling.StringFixed(2),
					}})
				continue
			}
		}
		// v2.0.139 telemetry: the marker rides the bot row's entryFeatures
		// (model_state on paper) exactly when the floor lifted the cap.
		if stress.floored {
			if candidate.ModelAssumptions == nil {
				candidate.ModelAssumptions = map[string]any{}
			}
			candidate.ModelAssumptions["stressLossFloor"] = true
		}

		atrVal := 0.0
		if aVal, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && aVal > 0 {
			atrVal = aVal * candidate.CurrentPrice.InexactFloat64() / 100.0
		}
		var obProfile *marketdata.DepthProfile
		if dp, ok := candidate.ModelAssumptions["orderBookDepth"].(*marketdata.DepthProfile); ok {
			obProfile = dp
		}
		var srRes *marketdata.SRAnalysisResult
		if sr, ok := candidate.ModelAssumptions["srAnalysis"].(*marketdata.SRAnalysisResult); ok {
			srRes = sr
		}

		minRR := 1.8
		if settings.MinRiskReward.IsPositive() {
			minRR = settings.MinRiskReward.InexactFloat64()
		}

		adaptiveRes := marketdata.ComputeIndividualTargetPrices(marketdata.AdaptiveBotTargetInput{
			Symbol:         candidate.Symbol,
			Direction:      trend,
			CurrentPrice:   candidate.CurrentPrice,
			LowerPrice:     mesh.LowerPrice,
			UpperPrice:     mesh.UpperPrice,
			GridNum:        mesh.GridNum,
			Budget:         settings.BudgetUSDT.InexactFloat64(),
			Leverage:       botLev,
			ATR:            atrVal,
			OrderBookDepth: obProfile,
			SRAnalysis:     srRes,
			MinRiskReward:  minRR,
			TakerFeeBps:    settings.FeeBps.InexactFloat64(),
			SlippageBps:    settings.SlippageBps.InexactFloat64(),
		})

		normTrend := strings.ToLower(strings.TrimSpace(trend))
		isNeutralPaper := normTrend == "no_trend" || normTrend == "neutral" || normTrend == ""
		var targetPriceDec *decimal.Decimal
		if !isNeutralPaper {
			tp := adaptiveRes.TargetPrice
			targetPriceDec = &tp
		}
		var stopLossPriceDec decimal.Decimal
		var stopLossHighDec *decimal.Decimal
		if settings.StopLossMode == "ADAPTIVE_ATR" {
			stopLossPriceDec = adaptiveRes.StopLossPrice
			stopLossHighDec = adaptiveRes.StopLossHigh
		}
		riskRewardDec := decimal.NewFromFloat(adaptiveRes.RiskRewardRatio)
		adaptiveStrat := adaptiveRes.AdaptiveStrategy

		if settings.PnLTargetMode == "DYNAMIC" {
			botTargetVal := decimal.NewFromFloat(adaptiveRes.TargetUSDT).Round(2)
			target = &botTargetVal
			botMaxLossVal := decimal.NewFromFloat(adaptiveRes.MaxLossUSDT).Round(2)
			maxLoss = &botMaxLossVal
			if stress.loss.GreaterThan(*maxLoss) {
				botMaxLossVal = stress.loss.Round(2)
				maxLoss = &botMaxLossVal
			}
		}
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

		// v2.0.96 exchange-parity pre-flight («всё поведение paper — как на
		// бирже»): PAPER used to open bots without ever asking Pionex, so the
		// paper fleet could include grids the exchange would refuse — per-
		// symbol minimum investment, grid-row limits, symbols under
		// maintenance — and paper statistics then proved a fleet REAL can
		// never run. The REAL path has run native checkParams since v2.0.67;
		// paper now runs the SAME call with the SAME verdicts. Fail-open by
		// design: a missing/unreachable account or a transient estimation
		// failure is not a rejection and must not wedge the paper fleet —
		// only the exchange's OWN refusals (min investment, forbidden symbol)
		// reject the candidate, with the REAL path's texts.
		if settings.AccountID != nil {
			if paperClient, clientErr := worker.service.PrivateClient(ctx, worker.accounts, *settings.AccountID); clientErr == nil {
				if base, quote, splitErr := SplitPionexPerp(candidate.Symbol); splitErr == nil {
					// REAL maps the trend onto the exchange enum before its
					// BUOrderData (neutral → no_trend); the paper gate must
					// send the SAME value or the estimation judges a
					// different bot than the one REAL would create.
					checkTrend := trend
					if checkTrend == "neutral" {
						checkTrend = "no_trend"
					}
					futuresBase := base
					if !strings.HasSuffix(futuresBase, ".PERP") && !strings.HasSuffix(futuresBase, "_PERP") {
						futuresBase = fmt.Sprintf("%s.PERP", base)
					}
					paperParams := pionex.NativeFuturesGridCreateParams{
						Base: futuresBase, Quote: quote,
						BUOrderData: pionex.BUOrderData{
							Top: mesh.UpperPrice, Bottom: mesh.LowerPrice,
							Row: mesh.GridNum, GridType: mapGridType(settings.DensityGridEnabled),
							Trend:           checkTrend,
							Leverage:        botLev,
							QuoteInvestment: investAmount.Round(2),
						},
					}
					check, checkErr := paperClient.CheckFuturesGridParams(ctx, paperParams)
					if checkErr != nil {
						// Same symbol-state semantics as the REAL path: the
						// create behind this check would be refused identically.
						if isSymbolOperationForbiddenError(checkErr) {
							worker.rejectCandidate(ctx, candidate,
								"биржа запрещает операцию по символу (forbidden/maintenance) — деплой отложен", nil)
							continue
						}
						// v2.0.168: paper parity defers with the candidate when the
						// exchange pre-flight is unreadable — same fail-closed class
						// as the REAL lane (the sandbox must exercise the REAL gate
						// order, not a bypassed one).
						worker.logger.Warn("paper parity checkParams unavailable — candidate deferred (fail-closed)",
							"component", "autogrid_worker", "symbol", candidate.Symbol, "error", checkErr)
						continue
					} else if check != nil && check.GetMinInvestment().GreaterThan(decimal.Zero) &&
						investAmount.LessThan(check.GetMinInvestment()) {
						worker.rejectCandidate(ctx, candidate, fmt.Sprintf(
							"checkParams биржи: бюджет %s ниже минимальной инвестиции %s — paper-бот открылся бы, REAL нет (паритет)",
							investAmount.Round(2), check.GetMinInvestment().StringFixed(2)), nil)
						continue
					}
				}
			} else {
				// v2.0.168: no client at all = defer too (parity with REAL).
				worker.logger.Warn("paper parity checkParams client unavailable — candidate deferred (fail-closed)",
					"component", "autogrid_worker", "symbol", candidate.Symbol, "error", clientErr)
				continue
			}
		}

		var botID string
		var botNumber int
		// v2.0.89 entry friction: the taker fee on the initial inventory
		// notional is booked into realized at deploy — the exchange debits
		// the wallet at fill time, paper must too. Also lands in
		// fees_paid_usdt (the fee ledger) and model_state (breakdown).
		paperEntryFeePaid := paperEntryFee(databaseTrend(trend), mesh.LowerPrice, mesh.UpperPrice,
			mesh.GridNum, investAmount, botLev, candidate.CurrentPrice)
		err = worker.db.QueryRow(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, candidate_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, model_state,
				pnl_target_usdt, max_loss_usdt,
				grid_step_pct, confluence_score, anti_hunt_stop_price,
				realized_pnl_usdt, fees_paid_usdt,
				target_price, stop_loss_price, stop_loss_high, trailing_sl_price,
				adaptive_strategy, risk_reward_ratio
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
					'entryFeeUsdt', $26::TEXT,
					'warning', 'paper PnL is not a native Pionex grid backtest'
				),
				$13, $14,
				$16, $17, $18,
				-$26::NUMERIC, $26::NUMERIC,
				$27, $28, $29, $30, $31, $32
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
			trancheFlag(trancheOn), settings.BudgetUSDT.String(), atrPct, entryFeaturesJSON(candidate),
			paperEntryFeePaid.Round(8).String(),
			targetPriceDec, stopLossPriceDec, stopLossHighDec, stopLossPriceDec,
			adaptiveStrat, riskRewardDec).Scan(&botID, &botNumber)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return fmt.Errorf("deploy paper grid %s: %w", candidate.Symbol, err)
		}
		activeCount++

		// v2.0.138 entry chain: the ALLOW row — one per created bot, with the
		// config version that admitted it.
		worker.journalEntryDecision(ctx, EntryChainInput{
			Path: EntryPathScannerPaper, Settings: settings, Symbol: candidate.Symbol,
			Direction: strings.ToUpper(trend), Fleet: "PAPER", RefID: botID,
		}, entryOutcomeAllow, "CLEAR", "", nil)

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
		// v2.0.143 (audit-2): no updated_at bump — clearing the stale note
		// is bookkeeping, not a settings change; ConfigVersion hashes
		// updated_at and must not churn on it.
		_, _ = worker.db.Exec(ctx, `
			UPDATE autogrid_settings SET last_error = NULL
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
// cascadeShort (v2.0.93 FIX-H, paper-parity): inside the out-of-turn
// cascade-short window a price flying >0.5 ATR between scan and deploy is
// the very move the cascade scan exists to short — the drift gate would void
// every candidate precisely in that window. The live price still re-anchors
// the candidate; only an UNREADABLE price stays fail-closed even in cascade.
func (worker *Worker) revalidateCandidateTrend(
	ctx context.Context,
	candidate *Candidate,
	settings Settings,
	cascadeShort bool,
	exemptDownShort bool,
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
		// beyond half an ATR voids the candidate itself — except in the
		// cascade-short window (FIX-H, mirror of the paper path's exemption)
		// and for a v2.0.148 beta-down confirmed short, where a downward
		// drift is the entry's own thesis (fresh price still re-anchors).
		if fresh, ok := worker.revalidateFreshPrice(ctx, candidate, atrPct); ok || ((cascadeShort || exemptDownShort) && fresh.IsPositive()) {
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

// capitalEffectiveBudget uses only fresh Spot-funded bot equity. Missing,
// stale or unreadable funding must never restore the full configured budget.
func (worker *Worker) capitalEffectiveBudget(ctx context.Context, accountID string, settingsBudget decimal.Decimal, trancheOn bool, reservePct decimal.Decimal) (decimal.Decimal, bool) {
	equity, committed, spendable, err := loadBotFundingReserve(ctx, worker.db, accountID)
	if err != nil {
		// v2.0.172b (audit P1-2): classify the error — stale data is NOT a
		// capital deficit; the old path silently returned zero and the
		// caller alarmed "capital deficiency" while the wallet had $1000+.
		errStr := err.Error()
		if strings.Contains(errStr, "stale") || strings.Contains(errStr, "invalid timestamp") {
			worker.logger.Warn("capital snapshot STALE — entries deferred (audit P1-2 v2: zero budget, not full)",
				"component", "autogrid_worker", "error", err)
			_ = QueueTelegramEvent(ctx, worker.db, "FUNDING_STALE", map[string]any{
				"message": "⚠️ Снимок капитала устарел (>10 мин) — входы отложены до обновления баланса, это НЕ дефицит средств.",
			})
			// v2.0.172c (audit correction): return ZERO, not the full budget —
			// returning the budget bypassed the capital check entirely (the
			// auto path doesn't call marginReserveBlocker). Zero means the
			// scan defers; the telegram tells the operator it's a data issue.
			return decimal.Zero, true
		}
		worker.logger.Warn("bot funding unavailable", "component", "autogrid_worker", "error", err)
		return decimal.Zero, true
	}
	slot, _ := fitBotFundingBudget(equity, committed, decimal.Min(settingsBudget, spendable.Floor()), trancheOn, reservePct)
	return slot, !slot.Equal(settingsBudget)
}

// capitalDeficiencyAlarm announces a hard capital shortfall at most once an
// hour (telegram + log) — the v2.0.140 freeze was 52 SILENT rejections
// long before anyone noticed. Single manage goroutine → plain field.
func (worker *Worker) capitalDeficiencyAlarm(ctx context.Context, settings Settings, slotBudget decimal.Decimal) {
	if time.Since(worker.lastCapitalAlarmAt) < time.Hour {
		return
	}
	worker.lastCapitalAlarmAt = time.Now()
	worker.logger.Warn("capital deficiency: no slot fits the margin reserve",
		"component", "autogrid_worker", "settings_budget", settings.BudgetUSDT.StringFixed(2))
	_ = QueueTelegramEvent(ctx, worker.db, "CAPITAL_DEFICIENCY", map[string]any{
		"message": fmt.Sprintf("⚠️ <b>Дефицит капитала:</b> даже минимальный слот не влезает в %s%%-резерв маржи — новые боты не открываются. Проверьте доступные USDT на Spot для финансирования ботов и свежесть снимка капитала.", settings.MarginReservePct.StringFixed(0)),
	})
}

// marginReserveBlocker protects fixed manual commitments with the same
// fresh Spot-funded equity and planned bot commitments used by auto sizing.
// Missing, stale, legacy or unreadable snapshots defer new risk.
func marginReserveBlocker(
	ctx context.Context,
	db *pgxpool.Pool,
	accountID string,
	addCommitment decimal.Decimal,
	trancheOn bool,
	reservePct decimal.Decimal,
	topUpBotID ...string,
) (code, reason string) {
	equity, activeCommitted, spendable, err := loadBotFundingReserve(ctx, db, accountID)
	if err != nil {
		// v2.0.172 (audit §5): a stale/unreadable snapshot is NOT a deficit —
		// the sequential scan+manage cycle can leave the snapshot >10 min old
		// while the wallet actually has funds. Separate the code so the
		// analytics don't count "data freshness" as "no capital".
		if strings.Contains(err.Error(), "stale") || strings.Contains(err.Error(), "invalid timestamp") {
			return "FUNDING_STALE", fmt.Sprintf("снимок капитала устарел (не дефицит!): %v — обновление баланса отложено из-за долгого скан-цикла", err)
		}
		return "MARGIN_RESERVE", fmt.Sprintf("резерв маржи: капитал ботов недоступен: %v", err)
	}

	// A top-up consumes its already reserved tranche, not a second copy.
	if len(topUpBotID) > 0 {
		var reserved decimal.Decimal
		err := db.QueryRow(ctx, `
			SELECT CASE WHEN model_state->>'trancheDeployed' = '1' THEN
			 GREATEST(0, COALESCE(CASE WHEN model_state->>'trancheBase' ~ '^[0-9]+(\.[0-9]+)?$'
			     THEN (model_state->>'trancheBase')::NUMERIC END, quote_investment*2) - quote_investment)
			 ELSE 0 END FROM grid_bots WHERE id=$1 AND account_id=$2
		`, topUpBotID[0], accountID).Scan(&reserved)
		if err != nil {
			return "MARGIN_RESERVE", fmt.Sprintf("read reserved tranche: %v", err)
		}
		addCommitment = decimal.Max(decimal.Zero, addCommitment.Sub(reserved))
	}

	// The slot commits its FULL budget: with tranches on, tranche-2's top-up
	// is contractually scheduled (24h time-box), so the doubling is reserved
	// up front — mirroring the stop envelope's full-stop reservation.
	slots := decimal.NewFromInt(1)
	if trancheOn {
		slots = decimal.NewFromInt(2)
	}
	projected := activeCommitted.Add(addCommitment.Mul(slots))
	if reservePct.IsNegative() {
		reservePct = decimal.Zero
	}
	if reservePct.GreaterThan(decimal.NewFromInt(95)) {
		reservePct = decimal.NewFromInt(95)
	}
	factor := decimal.NewFromInt(1).Sub(reservePct.Div(decimal.NewFromInt(100)))
	ceiling := equity.Mul(factor)
	if projected.GreaterThan(ceiling) || addCommitment.Mul(slots).GreaterThan(spendable) {
		return "MARGIN_RESERVE", fmt.Sprintf(
			"резерв маржи: projected $%s при equity $%s оставляет <%s%% свободных — вход отложен",
			projected.StringFixed(2), equity.StringFixed(2), reservePct.StringFixed(0))
	}
	return "", ""
}

// fleetCandidateDelta (v2.0.142, audit P2a) is the ONE candidate pre-charge
// for the fleet net-delta cap: LONG charges +budget×lev, SHORT −budget×lev,
// and a NEUTRAL candidate pre-charges ½×budget×lev of adverse inventory —
// at a boundary a neutral grid already holds ~50% notional long exposure.
// One helper, both deploy sites (paper above, REAL below) — the paper arm
// used to charge ZERO for a NEUTRAL candidate while REAL pre-charged the
// half, so the sandbox's delta gate never saw the neutral inventory REAL
// was already refusing. That closed the v2.0.93 FIX-G paper/REAL drift
// class in this lane: paper/REAL parity now holds by construction.
func fleetCandidateDelta(trend string, budget decimal.Decimal, botLev int) decimal.Decimal {
	notional := budget.Mul(decimal.NewFromInt(int64(botLev)))
	switch trend {
	case "long":
		return notional
	case "short":
		return notional.Neg()
	default:
		return notional.Div(decimal.NewFromInt(2))
	}
}

// projectedFleetDelta (v2.0.142, audit P2a) folds the fleet reading, the
// candidate pre-charge and the v2.0.140 NEUTRAL park into the cap's |·|
// projection: the fleet's un-shifted NEUTRAL grids carry zero net delta but
// ½×invest×lev of POTENTIAL adverse inventory each — a market-wide move
// loads them all onto the SAME side the candidate is about to expose.
// Directional candidates charge the park against their own side (+ for LONG,
// − for SHORT); a NEUTRAL candidate keeps its ½-notional pre-charge and adds
// the fleet park on top of |D| — the park is adverse on EITHER side. Shared
// by the paper and REAL deploy gates so the projection cannot drift again.
func projectedFleetDelta(trend string, fleetDelta, candidateDelta, neutralPark decimal.Decimal) decimal.Decimal {
	projected := fleetDelta.Add(candidateDelta)
	switch trend {
	case "short":
		projected = projected.Sub(neutralPark)
	case "long":
		projected = projected.Add(neutralPark)
	default:
		projected = projected.Abs().Add(neutralPark)
	}
	return projected.Abs()
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
	// v2.0.111 storm deferral moved into the shared entry chain below (the
	// composer's first leg) — same deferral, now journaled and shared with
	// every other entry path.
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
	// v2.0.138 entry chain: the fleet-wide market gates — storm → portfolio
	// circuit breaker → economic events → liquidation cascade + feed health —
	// now run through ONE shared composer (evaluateSharedMarketBlockers,
	// entry_chain.go). The breaker leg is deliberately the JOINT paper+REAL
	// protective-close count: the old REAL-only copy armed on real losses
	// alone while a paper fleet under stress proved the exact pipeline REAL
	// was about to ride. Storm/breaker/economic block the WHOLE pass; the
	// cascade/feed legs stay direction-aware and only NOTE here — the
	// per-candidate cut inside the loop still lets SHORT candidates through.
	// v2.0.142 (audit P2c): the fleet-constant legs (breaker, economic
	// events) are memoized per pass — the pass-level probe below fills the
	// cache and every per-candidate cut reuses it.
	passBlockers := &marketBlockerCache{}
	passCode, _, passNote, passFeatures := worker.probeSharedMarketBlockersCached(ctx, EntryChainInput{
		Path: EntryPathScannerReal, Settings: settings, Fleet: "REAL", Direction: "NEUTRAL",
	}, passBlockers)
	switch passCode {
	case "":
		// clear
	case entryBlockedStorm:
		// v2.0.111 storm deferral: opening fresh grids into a fleet-wide
		// acceleration buys the worst entries of the day; the scan retries in
		// four minutes and the storm window is rolling.
		worker.logger.Info("deploy deferred by storm mode",
			"component", "autogrid_worker", "scan_id", scanID)
		return nil
	case entryWaitGateUnreadable:
		worker.logger.Warn("real pass deferred: advisory gates SQL-unreadable (WAIT)",
			"component", "autogrid_worker", "note", passNote)
		return nil
	case entryBlockedCircuitBreaker:
		worker.logger.Warn("Portfolio circuit breaker: recent real stop-losses holding new deployments",
			"component", "autogrid_worker", "recentStopLossCountReal", passFeatures["closes"])
		worker.noteDeployBlock(ctx, passNote)
		return nil
	case entryBlockedEconomicEvent:
		worker.logger.Warn("deploy blocked by economic event",
			"component", "autogrid_worker", "reason", passFeatures["title"])
		worker.noteDeployBlock(ctx, passNote)
		return nil
	case entryBlockedMacro:
		// Per-candidate leg: the macro veto judges each candidate's SCANNER
		// trend (shorts exempt) inside the loop — nothing to defer here.
	default:
		// CASCADE / FEED_HEALTH: LONG/NEUTRAL defer, SHORT stay live — note
		// only, the per-candidate cut below decides per direction.
		worker.logger.Warn("liquidation gate: LONG/NEUTRAL real deploys paused, SHORT stay live",
			"component", "autogrid_worker", "code", passCode, "usd_1h", passFeatures["usd_1h"],
			"block", passNote)
		worker.noteDeployBlock(ctx, passNote)
	}
	deployErrors := make([]string, 0)
	backtestGateOn := worker.backtestGateEnabled(ctx)
	// v2.0.147 beta-down SHORT exemption: one pass-level read of the SAME
	// 5-min cached regime the beta block below enforces — entry-timing must
	// not cut a pair-confirmed downtrend short just because the channel
	// position reads "unfavorable" (in a trend regime the extreme IS the
	// continuation; counterfactual 2026-09-28: those cuts fell 4.3:1).
	betaDownEntry, betaUpEntry := betaGateTrend(worker.marketBetaRegime(ctx))
	// When the LLM brain is enabled, an UNAUDITED candidate is not
	// deployable — regardless of why the audit is missing (beyond the
	// per-scan audit cap, transport failure, timeout). This is the hard
	// structural guarantee that closes the top-5 bypass.
	llmBrainEnabled := false
	if llmSettings, err := worker.llm.GetSettings(ctx); err == nil {
		llmBrainEnabled = llmSettings.Enabled && strings.TrimSpace(llmSettings.APIKey) != ""
	}
	// v2.0.94 cumulative symbol cooldown — REAL mirror of the paper gate:
	// the repeat-loser cohort is strategy-level evidence, not fleet-level.
	losingSymbols := worker.loadLosingSymbolCooldowns(ctx, settings.ID)
	for _, candidate := range candidates {
		// Non-ACCEPTED rows already carry their scanner-time rejection reason.
		if candidate.Decision != "ACCEPTED" {
			continue
		}
		if st, cooled := losingSymbols[candidate.Symbol]; cooled {
			worker.rejectCandidate(ctx, candidate, fmt.Sprintf(
				"символьный кулдаун: %d закрытий, нетто −$%.2f за 7д — повторные входы заморожены до выхода окна из минуса",
				st.Closes, -st.NetUSDT), nil)
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
		if !isEntryTimingFavorable(candidate) {
			v := directionalTrendExempt(candidate, betaDownEntry, betaUpEntry)
			if !v.Exempt {
				worker.rejectCandidate(ctx, candidate,
					"вход-тайминг: текущая позиция в канале вне благоприятной зоны для этого направления", nil)
				continue
			}
			// v2.0.147/v2.0.161 attribution marker — paper-mirror comment:
			// entry-timing lifted by the confirmed-trend exemption;
			// partitions the 14-day follow-up by entry cohort.
			candidate.ModelAssumptions[v.Cohort] = true
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
		if ok, reason := worker.revalidateCandidateTrend(ctx, &candidate, settings, cascadeShort, directionalTrendExempt(candidate, betaDownEntry, betaUpEntry).Exempt); !ok {
			worker.logger.Info("skip real deploy after fresh trend revalidation",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", reason)
			worker.rejectCandidate(ctx, candidate, "ре-валидация тренда: "+reason, nil)
			continue
		}
		// Order-book (DOM) gate (v2.0.93 FIX-G, paper-parity): a one-sided
		// book against the entry direction vetoes the deploy — the REAL path
		// is the one that pays real money for the tape's inventory and it ran
		// WITHOUT the gate the paper fleet has had since v2.0.39. Fail-open on
		// transport errors (advisory signal); cascade-short window exempt,
		// exactly like the paper arm.
		if bids, asks, derr := worker.publicClient.GetDepth(ctx, candidate.Symbol, 50); derr == nil &&
			len(bids) > 0 && len(asks) > 0 && candidate.CurrentPrice.IsPositive() {
			imbalance := depthImbalance(bids, asks, candidate.CurrentPrice, 1.5)
			scannerTrendReal := strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))
			againstEntry := (scannerTrendReal != "short" && imbalance < 0.25) || (scannerTrendReal == "short" && imbalance > 0.75)
			if againstEntry && !cascadeShort {
				side := "аски доминируют (продавцы)"
				if scannerTrendReal == "short" {
					side = "биды доминируют (покупатели)"
				}
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("стакан: дисбаланс %.2f против направления — %s, вход отложен", imbalance, side),
					map[string]any{"depthGate": map[string]any{"imbalance": imbalance, "vetoed": true}})
				continue
			}
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
			if time.Since(*lastProtectiveAt) < window &&
				// v2.0.140 (review P1-1): cascade-short flip exemption, REAL
				// mirror of the paper gate — see the paper-site comment.
				!(cascadeShort && strings.EqualFold(strings.TrimSpace(candidate.RecommendedTrend), "short") &&
					worker.cascadeFlipCooldownExemptReal(ctx, *settings.AccountID, candidate.Symbol)) {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("cooldown: %d защитных закрытий за 24ч, окно %s с последнего — повторный вход отложен",
						protectiveCloses, window), nil)
				continue
			}
		}
		// v2.0.27 sector cap (v2.0.93 FIX-G, REAL mirror): correlated clusters
		// stop together — the paper fleet has been capped since the 2026-08-21
		// audit while REAL stacked unbounded same-sector grids.
		if sector := sectorForSymbol(candidate.Symbol); sector != "" &&
			worker.sectorRealBotCount(ctx, *settings.AccountID, sector) >= maxBotsPerSector {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("sector cap: в секторе %s уже %d ботов — коррелированный кластер, вход отложен", sector, maxBotsPerSector), nil)
			continue
		}
		atrPct := 2.0
		if val, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && val > 0 {
			atrPct = val
		}
		// v2.0.75 exchange-rejection cooldown: a FAILED create (403 forbidden/
		// maintenance at the create stage, BOT_INTERNAL_ERROR) is a symbol
		// state that persists across scans — the lifecycle already persists
		// the authoritative FAILED grid row, so the durable marker IS the
		// grid_bots ledger itself. Without this gate PUMP hammered 8 refused
		// creates in a row (one per scan window, ~2.5m apart).
		var rejectedCreateAt *time.Time
		if err := worker.db.QueryRow(ctx, `
			SELECT MAX(updated_at)
			FROM grid_bots
			WHERE account_id = $1 AND symbol = $2
			  AND status = 'FAILED'
			  AND reconciliation_state = 'FAILED_AUTHORITATIVE'
			  AND updated_at > NOW() - INTERVAL '1 hour'
		`, *settings.AccountID, candidate.Symbol).Scan(&rejectedCreateAt); err == nil && rejectedCreateAt != nil {
			worker.rejectCandidate(ctx, candidate,
				fmt.Sprintf("биржа отклонила создание (%s в %sZ) — кулдаун 1ч",
					candidate.Symbol, rejectedCreateAt.UTC().Format("15:04")), nil)
			continue
		}
		regime := "RANGE"
		if val, ok := candidate.ModelAssumptions["regime"].(string); ok && val != "" {
			regime = val
		}

		// v2.0.145 capital-adaptive slot: the fleet must place bots
		// AUTONOMOUSLY at whatever capital the account actually holds —
		// a hard MARGIN_RESERVE reject froze every deploy for hours on a
		// $253 account (52 silent rejections, prod 2026-09-28) because a
		// $100 slot with its tranche doubling ($200) overflowed the 30%
		// reserve line ($177). Instead of refusing, the slot SCALES DOWN to
		// what fits under the reserve; only a slot below the exchange
		// minimum rejects. Capital deficiency now ANNOUNCES itself (hourly
		// telegram) instead of starving the fleet quietly.
		slotBudget, capitalScaled := worker.capitalEffectiveBudget(ctx, *settings.AccountID, settings.BudgetUSDT, settings.TrancheDeployEnabled, settings.MarginReservePct)
		if !slotBudget.IsPositive() {
			worker.capitalDeficiencyAlarm(ctx, settings, slotBudget)
			reason := fmt.Sprintf("резерв маржи: свободно под новый слот $0 — капитал Spot для ботов недоступен или минимальный слот не влезает в %s%%-резерв", settings.MarginReservePct.StringFixed(0))
			deployErrors = append(deployErrors, fmt.Sprintf("%s: %s", candidate.Symbol, reason))
			worker.rejectCandidate(ctx, candidate, reason, nil)
			worker.journalEntryDecision(ctx, EntryChainInput{
				Path: EntryPathScannerReal, Settings: settings, Symbol: candidate.Symbol,
				Direction: entryDirectionFromTrend(strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend))), Fleet: "REAL", RefID: candidate.ID,
			}, entryOutcomeReject, "MARGIN_RESERVE", reason, nil)
			continue
		}
		if capitalScaled {
			worker.logger.Warn("capital-adaptive slot scaled down for margin reserve",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"settings_budget", settings.BudgetUSDT.StringFixed(2), "effective_budget", slotBudget.StringFixed(2))
		}
		slotSettings := settings
		slotSettings.BudgetUSDT = slotBudget

		// v2.0.13 tranches: REAL commits HALF the budget at create; the
		// manage loop tops up via the native invest_in endpoint after a
		// confirmed adverse excursion or the 24h time-box (paper mirror).
		// Sizing the grid level count against the full slot budget ($200)
		// enables 30-40 levels (step ~0.55%) with $5/level order capacity,
		// doubling grid crossing captures vs halving levels to 20.
		investAmount := slotBudget
		if settings.TrancheDeployEnabled {
			investAmount = slotBudget.Div(decimal.NewFromInt(2))
		}
		geometryBudget := slotBudget
		mesh := ComputeAdaptiveMesh(
			candidate.LowerPrice, candidate.UpperPrice, candidate.CurrentPrice,
			atrPct, regime, geometryBudget, settings.Leverage,
			decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps),
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
			if v := directionalTrendExempt(candidate, betaDownEntry, betaUpEntry); v.Exempt {
				// v2.0.148 (REAL mirror)/v2.0.161: expansion is the confirmed
				// trend's payload, not its hazard — the RV gate stands down
				// for the confirmed own-direction cohort (short always; long
				// unless BTC itself is falling), every later gate stays armed.
				candidate.ModelAssumptions[v.Cohort] = true
			} else {
				worker.logger.Info("entry gate: volatility expansion, skip real deploy",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"rv_ref_ratio", math.Round(ratio*100)/100)
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("entry gate: расширение волатильности (RV/базлайн %.2f ≥ 1.5) — вход в ускорение заблокирован", math.Round(ratio*100)/100), nil)
				continue
			}
		}
		if harGeo != nil {
			harGeo.applyToMesh(candidate.CurrentPrice, &mesh)
			worker.logger.Info("v2.0 har geometry applied",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"forecast_vol_pct", harGeo.forecastPct, "r2", harGeo.geo.Confidence,
				"range_pct", harGeo.geo.RangePct, "grid_num", mesh.GridNum,
				"leverage_cap", harGeo.geo.Leverage)
		}

		// v2.0.163 wide-grid doctrine: after every geometry source has
		// spoken, the span must still cover a normal move — ≥8% of price.
		// The level count keeps: wider bounds with the same levels widen
		// every step (bigger harvest per crossing, same fee share).
		if nl, nu := EnsureDeploySpan(mesh.LowerPrice, mesh.UpperPrice, candidate.CurrentPrice); !nl.Equal(mesh.LowerPrice) || !nu.Equal(mesh.UpperPrice) {
			worker.logger.Info("v2.0.163 wide-grid: span widened to 8% floor",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"was_lower", mesh.LowerPrice.StringFixed(6), "was_upper", mesh.UpperPrice.StringFixed(6),
				"new_lower", nl.StringFixed(6), "new_upper", nu.StringFixed(6), "grid_num", mesh.GridNum)
			mesh.LowerPrice, mesh.UpperPrice = nl, nu
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
		// v2.0.138 entry chain: the shared market-blocker composer now owns
		// the per-candidate fleet gates — liquidation cascade + feed health
		// (direction-aware against the FINAL trend: SHORT stays live, smart
		// override included) and the macro veto (judging the SCANNER trend
		// with the cascade-short exemption — v2.0.58 closed the paper-only
		// hole; the composer keeps those exact inputs). Storm, the joint
		// portfolio breaker and economic events were cleared once per pass
		// above. Plan order puts the cascade ahead of the macro veto, so a
		// candidate hitting both now names the cascade.
		entryIn := EntryChainInput{
			Path: EntryPathScannerReal, Settings: settings, Symbol: candidate.Symbol,
			Direction: entryDirectionFromTrend(trend), Fleet: "REAL", RefID: candidate.ID,
			ScannerTrend: strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)),
			CascadeShort: cascadeShort,
		}
		if code, reason, _, features := worker.probeSharedMarketBlockersCached(ctx, entryIn, passBlockers); code != "" {
			if code == entryWaitGateUnreadable {
				// v2.0.167: infrastructure unreadable — WAIT this pass
				// WITHOUT a market REJECT (analytics must not count it).
				worker.journalEntryDecision(ctx, entryIn, entryOutcomeWait, code, reason, features)
				continue
			}
			worker.rejectCandidate(ctx, candidate, reason, features)
			worker.journalEntryDecision(ctx, entryIn, entryOutcomeReject, code, reason, features)
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
		// Quant Vision v3.0 allows flip entry if a fresh confirmed price action pattern is formed.
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") &&
			worker.directionalFlipBlocked(ctx, candidate.Symbol, strings.ToUpper(trend), false) {
			paConfirmed, paReason := worker.directionalConfirmedByPriceAction(ctx, candidate.Symbol, trend)
			if !paConfirmed {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("флип направления: символ закрыл бота другого направления ≤12ч назад и нет свечного паттерна (%s)", paReason), nil)
				continue
			}
			worker.logger.Info("real directional flip lock bypassed by confirmed price action",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "pattern", paReason)
		}

		// v2.0.62 (R1, REAL mirror) + Quant Vision v3.0: directional entry requires
		// either the matching confluence verdict OR confirmed candlestick price action
		// (Pin Bar, Engulfing, or SFP liquidity sweep). Cascade-shorts exempt.
		// v2.0.162 (unlock plan step 3, REAL mirror): confirmed own-trend
		// impulses are exempt from the pattern requirement (see paper twin).
		if (trend == "long" || trend == "short") && !(cascadeShort && trend == "short") {
			verdict := candidateConfluenceVerdict(candidate.ModelAssumptions)
			want := "SUPPORT_SHORT"
			if trend == "long" {
				want = "SUPPORT_LONG"
			}
			if verdict != want {
				if v := directionalTrendExempt(candidate, betaDownEntry, betaUpEntry); v.Exempt {
					// v2.0.162 review P1 (REAL mirror): stamp the cohort for
					// the 14-day outcome partition.
					candidate.ModelAssumptions[v.Cohort] = true
					worker.logger.Info("real directional entry approved via confirmed trend (R1 exempt)",
						"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "cohort", v.Cohort)
				} else {
					paConfirmed, paReason := worker.directionalConfirmedByPriceAction(ctx, candidate.Symbol, trend)
					if !paConfirmed {
						worker.rejectCandidate(ctx, candidate,
							fmt.Sprintf("R1 + Vision: направленный вход (%s) без подтверждения — confluence %s (нужно %s) и нет свечного паттерна (%s)",
								strings.ToUpper(trend), verdict, want, paReason), nil)
						continue
					}
					worker.logger.Info("real directional entry approved via Price Action",
						"component", "autogrid_worker", "symbol", candidate.Symbol, "trend", trend, "pattern", paReason)
				}
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

		// Safety check on StopLoss positioning (v2.0.93: the shared
		// ClampAntiHuntStopIntoBounds helper — paper deploy and the DGT
		// re-center arms run the identical rule).
		antiHuntStop = ClampAntiHuntStopIntoBounds(trend, lowerPrice, upperPrice, antiHuntStop).
			Round(int32(pricePrecision))

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

		// Fleet Net Delta Cap (Quant & Vision v3.0). v2.0.142 (audit P2a):
		// the charge and the park projection moved into the shared
		// fleetCandidateDelta/projectedFleetDelta helpers — the paper gate
		// above runs the identical math, so paper/REAL parity holds in this
		// lane by construction.
		candidateDelta := fleetCandidateDelta(trend, slotBudget, botLev)
		if settings.FleetMaxNetDeltaUSDT.IsPositive() && !candidateDelta.IsZero() {
			fleetDelta, neutralPark, err := worker.calculateFleetNetDelta(ctx, settings.ID, false)
			if err != nil {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Fleet net delta error: %v — деплой отложен для защиты портфеля (fail-closed)", err), nil)
				continue
			}
			projectedDelta := projectedFleetDelta(trend, fleetDelta, candidateDelta, neutralPark)
			if projectedDelta.GreaterThan(settings.FleetMaxNetDeltaUSDT) {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Fleet net delta cap: текущая дельта $%s + парк нейтральных $%s + кандидат $%s превысит лимит $%s — пауза входа для защиты портфеля",
						fleetDelta.StringFixed(2), neutralPark.StringFixed(2), candidateDelta.StringFixed(2), settings.FleetMaxNetDeltaUSDT.StringFixed(2)), nil)
				continue
			}
		}

		// Order Book Cushion Check (Quant & Vision v3.0) — feeds L2 depth into OFIEngine
		if settings.OrderbookProfilerEnabled {
			botNotional := slotBudget.Mul(decimal.NewFromInt(int64(botLev))).InexactFloat64()
			minCushion := settings.MinDepthCushionRatio.InexactFloat64()
			ok, profile, depthReason := worker.checkOrderBookCushion(ctx, candidate.Symbol, candidate.CurrentPrice, botNotional, minCushion, true, settings.MaxSpreadPct)
			if !ok {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Стакан: %s — отказ по фильтру тонкой ликвидности", depthReason), nil)
				continue
			}
			if trend != "short" && profile.HasBidWall && profile.BidWallPrice.GreaterThan(lowerPrice) && profile.BidWallPrice.LessThan(candidate.CurrentPrice) {
				worker.logger.Info("real anchoring grid lowerPrice above order book bid wall",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"old_lower", lowerPrice.String(), "bid_wall", profile.BidWallPrice.String())
				lowerPrice = profile.BidWallPrice.Round(int32(pricePrecision))
			}
			// v2.0.163 review P1-1: the wall anchor narrows the span AFTER
			// the doctrine widening — re-apply the 8% floor and gate the
			// FINAL geometry so no deploy ships a sub-floor step.
			// v2.0.164 (prod 29-30.09 postmortem): the re-widened bounds
			// MUST be rounded to the symbol's price precision — this was
			// the one unrounded mutation reaching CreateFuturesGridBot and
			// the exchange refused five accepted candidates with
			// "top not match quote precision" (XLM/ALABX/ARB/SOXLX/UNI).
			if nl, nu := EnsureDeploySpan(lowerPrice, upperPrice, candidate.CurrentPrice); !nl.Equal(lowerPrice) || !nu.Equal(upperPrice) {
				nl = nl.Round(int32(pricePrecision))
				nu = nu.Round(int32(pricePrecision))
				if nu.GreaterThan(nl) {
					worker.logger.Info("v2.0.163 wide-grid: re-widened after bid-wall anchor",
						"component", "autogrid_worker", "symbol", candidate.Symbol,
						"new_lower", nl.StringFixed(6), "new_upper", nu.StringFixed(6))
					lowerPrice, upperPrice = nl, nu
				} else {
					worker.logger.Warn("v2.0.164 wide-grid: re-widen collapsed after rounding — keeping anchored bounds",
						"component", "autogrid_worker", "symbol", candidate.Symbol,
						"lower", lowerPrice.StringFixed(6), "upper", upperPrice.StringFixed(6))
				}
			}
			if spanPct := upperPrice.Sub(lowerPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).InexactFloat64(); mesh.GridNum > 0 {
				if stepPct := spanPct / float64(mesh.GridNum); stepPct > 0 {
					if reason, violated := marketdata.FeeGateRejection(stepPct, decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps)); violated {
						worker.rejectCandidate(ctx, candidate, "fee-gate (финальная геометрия после анкера): "+reason, nil)
						continue
					}
				}
			}
		}

		// Knife Pause & OFI Check (Quant & Vision v3.0)
		if settings.KnifePauseEnabled {
			paused, _, knifeReason := worker.checkKnifePause(ctx, candidate.Symbol, trend)
			if paused {
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("Knife Pause: %s — деплой отложен для защиты от падающего ножа", knifeReason), nil)
				continue
			}
		}

		if err := worker.risk.ValidateNewGrid(
			ctx, *settings.AccountID, candidate.Symbol,
			botLev, slotBudget,
		); err != nil {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: risk gate: %v", candidate.Symbol, err))
			// v2.0.138 (audit): the refusal must reach the candidate row too —
			// deployErrors only feeds the log/last_error, so the candidate
			// stayed ACCEPTED with no reason and the next scan re-ran the whole
			// pipeline for it (paper mirror: "risk engine: …", v2.0.27).
			worker.rejectCandidate(ctx, candidate, "risk engine: "+err.Error(), nil)
			worker.journalEntryDecision(ctx, EntryChainInput{
				Path: EntryPathScannerReal, Settings: settings, Symbol: candidate.Symbol,
				Direction: entryDirectionFromTrend(trend), Fleet: "REAL", RefID: candidate.ID,
			}, entryOutcomeReject, "RISK_ENGINE", "risk engine: "+err.Error(), nil)
			continue
		}
		// v2.0.145: the v2.0.140 margin-reserve reject moved UP into the
		// capital-adaptive slotBudget at the top of this iteration — the
		// reserve now SCALES the slot instead of refusing it (manual deploy
		// and invest_in keep the hard blocker: their commitments are fixed
		// by the operator).
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
		botTarget, botMaxLoss, stress := computeBotTargetsWithStress(slotSettings, candidate, botLev, botTargetSpan, stressGeometry{
			direction: trend, entry: candidate.CurrentPrice,
			lower: lowerPrice, upper: upperPrice, stop: antiHuntStop,
			gridNum: mesh.GridNum,
			// Full-slot budget, not the tranche-halved investAmount (paper
			// twin's comment): tranche-1 stress is the same number, and the
			// post-top-up stop then covers the doubled inventory too.
			invest: slotBudget,
		})
		// v2.0.149 (exit audit F3): FIXED mode with a zero target/loss
		// resolves to nil,nil — such a bot carries NO stop anywhere (the
		// local exits read 0 = off and no exchange LossStop is sent at
		// create). The settings plane already rejects the combination
		// (validateSettings), but a raw SQL settings edit must not be
		// able to buy an unprotected grid: refuse the deploy outright.
		if botTarget == nil || botMaxLoss == nil {
			deployErrors = append(deployErrors, fmt.Sprintf(
				"%s: no target/loss resolved (FIXED без стопа?) — deploy refused",
				candidate.Symbol))
			worker.rejectCandidate(ctx, candidate,
				"деплой без таргета/стопа отказан: FIXED-режим с нулевым MaxLossUSDT/PnLTargetUSDT оставляет бота вообще без защиты (аудит выхода F3)",
				map[string]any{"stopUnarmedRefusal": true})
			continue
		}
		// v2.0.139 stress-inventory gate (paper parity): the floor keeps the
		// STORED stop honest, but a geometry whose full-traverse loss
		// overflows the tranche-2 effective-stop ceiling (DynamicLossMaxPct ×
		// breakerHeadroom — the same budget every fleet cap derives from)
		// is not budget-sized: refuse BEFORE a grid row or a create fee is
		// ever submitted. FIXED-mode targets are exempt (operator's stop).
		if settings.PnLTargetMode != "FIXED" {
			stressCeiling := tranche2MaxLossCap(slotBudget, botLev)
			if stress.loss.GreaterThan(stressCeiling) {
				deployErrors = append(deployErrors, fmt.Sprintf(
					"%s: stress inventory $%s exceeds loss ceiling $%s",
					candidate.Symbol, stress.loss.StringFixed(2), stressCeiling.StringFixed(2)))
				worker.rejectCandidate(ctx, candidate,
					fmt.Sprintf("стресс-инвентарь: полный проход сетки до стопа $%s превышает допустимый убыток $%s — геометрия концентрирует риск больше бюджета",
						stress.loss.StringFixed(2), stressCeiling.StringFixed(2)),
					map[string]any{"stressLossFloor": map[string]any{
						"stressLossUsdt": stress.loss.StringFixed(2),
						"ceilingUsdt":    stressCeiling.StringFixed(2),
					}})
				continue
			}
		}
		// v2.0.67 parity: the deploy envelope gate reserves the candidate's
		// FULL (post-tranche-2) stop — the amount the top-up later doubles
		// the stored half to. It must be captured BEFORE the tranche-1
		// halving below, mirroring the paper path (v2.0.66).
		candidateFullStop := decimal.Zero
		if botMaxLoss != nil {
			candidateFullStop = *botMaxLoss
		}
		if botMaxLoss != nil {
			if reason := deployStopEnvelopeGate(ctx, worker.db, worker.risk, worker.logger, settings.ID, candidateFullStop); reason != "" {
				deployErrors = append(deployErrors, fmt.Sprintf("%s: %s", candidate.Symbol, reason))
				worker.logger.Info("real deploy blocked by fleet stop envelope",
					"component", "autogrid_worker", "symbol", candidate.Symbol)
				continue
			}
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
		atrVal := 0.0
		if aVal, ok := candidate.ModelAssumptions["atrPct"].(float64); ok && aVal > 0 {
			atrVal = aVal * candidate.CurrentPrice.InexactFloat64() / 100.0
		}
		var obProfile *marketdata.DepthProfile
		if dp, ok := candidate.ModelAssumptions["orderBookDepth"].(*marketdata.DepthProfile); ok {
			obProfile = dp
		}
		var srRes *marketdata.SRAnalysisResult
		if sr, ok := candidate.ModelAssumptions["srAnalysis"].(*marketdata.SRAnalysisResult); ok {
			srRes = sr
		}

		minRR := 1.8
		if settings.MinRiskReward.IsPositive() {
			minRR = settings.MinRiskReward.InexactFloat64()
		}

		adaptiveRes := marketdata.ComputeIndividualTargetPrices(marketdata.AdaptiveBotTargetInput{
			Symbol:         candidate.Symbol,
			Direction:      trend,
			CurrentPrice:   candidate.CurrentPrice,
			LowerPrice:     lowerPrice,
			UpperPrice:     upperPrice,
			GridNum:        mesh.GridNum,
			Budget:         slotBudget.InexactFloat64(),
			Leverage:       botLev,
			ATR:            atrVal,
			OrderBookDepth: obProfile,
			SRAnalysis:     srRes,
			MinRiskReward:  minRR,
			TakerFeeBps:    settings.FeeBps.InexactFloat64(),
			SlippageBps:    settings.SlippageBps.InexactFloat64(),
			PricePrecision: pricePrecision,
		})

		// Sized net true USDT targets: respect PnLTargetMode (DYNAMIC vs FIXED) and TrancheDeployEnabled
		if settings.PnLTargetMode == "DYNAMIC" {
			botTargetVal := decimal.NewFromFloat(adaptiveRes.TargetUSDT).Round(2)
			botMaxLossVal := decimal.NewFromFloat(adaptiveRes.MaxLossUSDT).Round(2)
			if stress.loss.GreaterThan(botMaxLossVal) {
				botMaxLossVal = stress.loss.Round(2)
			}
			if settings.TrancheDeployEnabled {
				botTargetVal = botTargetVal.Div(decimal.NewFromInt(2)).Round(2)
				botMaxLossVal = botMaxLossVal.Div(decimal.NewFromInt(2)).Round(2)
			}
			botTarget = &botTargetVal
			botMaxLoss = &botMaxLossVal
		}

		normTrendReal := strings.ToLower(strings.TrimSpace(trend))
		isNeutralReal := normTrendReal == "no_trend" || normTrendReal == "neutral" || normTrendReal == ""

		// v2.0.160 harvest doctrine: a neutral card TP is a HARVEST amount —
		// cap the dynamic (tranche-halved) target at 2% of the committed
		// investment so the exchange card actually fires instead of waiting
		// for a full-range traversal that never happens. The stored
		// pnl_target_usdt below inherits the same capped value.
		if isNeutralReal {
			botTarget = applyNeutralHarvestTP(settings.PnLTargetMode, investAmount, botTarget)
		}

		// Configure native Pionex take-profit directly on the bot card:
		// Neutral futures grids hold bidirectional inventory (short above, long below).
		// Setting a price-based profitStop causes an instant premature trigger as soon as short
		// orders fill because market_price <= upper_price is immediately true. Neutral grids MUST
		// use profit_amount take-profit (USDT target).
		// Directional grids (LONG / SHORT) hold one-way directional inventory, where price-based
		// profitStop works natively on the Pionex card.
		var targetPriceDec *decimal.Decimal
		if isNeutralReal {
			if botTarget != nil && botTarget.GreaterThan(decimal.Zero) {
				data.ProfitStopType = "profit_amount"
				data.ProfitStop = botTarget
				data.ProfitStopDelay = nil
			} else {
				data.ProfitStopType = ""
				data.ProfitStop = nil
				data.ProfitStopDelay = nil
			}
			targetPriceDec = nil
		} else {
			targetPrice := adaptiveRes.TargetPrice.Round(int32(pricePrecision))
			data.ProfitStopType = "price"
			data.ProfitStop = &targetPrice
			profitDelay := 15
			data.ProfitStopDelay = &profitDelay
			targetPriceDec = &targetPrice
		}

		// Configure native Pionex price-based stop-loss: strictly respect settings.StopLossMode
		var slPrice decimal.Decimal
		var slHighPrice *decimal.Decimal
		if settings.StopLossMode == "ADAPTIVE_ATR" {
			slPrice = ClampAntiHuntStopIntoBounds(trend, lowerPrice, upperPrice, adaptiveRes.StopLossPrice).Round(int32(pricePrecision))
			data.LossStopType = "price"
			data.LossStop = &slPrice
			if adaptiveRes.StopLossHigh != nil {
				h := adaptiveRes.StopLossHigh.Round(int32(pricePrecision))
				slHighPrice = &h
				data.LossStopHigh = slHighPrice
			}
			lossDelay := 15
			data.LossStopDelay = &lossDelay
		} else {
			data.LossStopType = ""
			data.LossStop = nil
			data.LossStopHigh = nil
			data.LossStopDelay = nil
		}

		// Walk-forward backtest gate: the traded TF must pass empirical walk-forward
		// backtesting with the exact parameters (range, levels, leverage, investment,
		// stop loss, direction) that will be deployed to Pionex.
		if backtestGateOn {
			stopLossPct := 8.0
			if candidate.CurrentPrice.IsPositive() && antiHuntStop.IsPositive() {
				if trend == "short" {
					if antiHuntStop.GreaterThan(candidate.CurrentPrice) {
						pct, _ := antiHuntStop.Sub(candidate.CurrentPrice).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
						if pct > 0 {
							stopLossPct = pct
						}
					}
				} else {
					if candidate.CurrentPrice.GreaterThan(antiHuntStop) {
						pct, _ := candidate.CurrentPrice.Sub(antiHuntStop).Div(candidate.CurrentPrice).Mul(decimal.NewFromInt(100)).Float64()
						if pct > 0 {
							stopLossPct = pct
						}
					}
				}
			}
			deployParams := BacktestDeployParams{
				Symbol:      candidate.Symbol,
				Interval:    settings.CandleInterval,
				LowerPrice:  lowerPrice,
				UpperPrice:  upperPrice,
				GridNum:     mesh.GridNum,
				Leverage:    botLev,
				Investment:  investAmount,
				Direction:   trend,
				StopLossPct: stopLossPct,
				FeeBps:      decimalFloat(settings.FeeBps),
				SlippageBps: decimalFloat(settings.SlippageBps),
			}
			verdict := worker.backtestGateWithParams(ctx, settings, candidate.Symbol, &deployParams)
			if verdict.Pending {
				worker.logger.Info("backtest gate: awaiting walk-forward results for exact deploy params",
					"component", "autogrid_worker", "symbol", candidate.Symbol,
					"lower", deployParams.LowerPrice.String(), "upper", deployParams.UpperPrice.String(),
					"levels", deployParams.GridNum, "leverage", deployParams.Leverage)
				continue
			}
			if !verdict.Allowed {
				worker.rejectCandidate(ctx, candidate, verdict.Reason, map[string]any{
					"backtestGate": map[string]any{
						"allowed": verdict.Allowed, "reason": verdict.Reason,
						"traded": verdict.Traded, "neighbors": verdict.Neighbors,
						"params": deployParams,
					},
				})
				worker.journalEntryDecision(ctx, EntryChainInput{
					Path: EntryPathScannerReal, Settings: settings, Symbol: candidate.Symbol,
					Direction: entryDirectionFromTrend(trend), Fleet: "REAL", RefID: candidate.ID,
				}, entryOutcomeReject, "BACKTEST_GATE", verdict.Reason, map[string]any{
					"traded": verdict.Traded,
				})
				worker.logger.Info("backtest gate rejected candidate",
					"component", "autogrid_worker", "symbol", candidate.Symbol, "reason", verdict.Reason)
				continue
			}
			worker.logger.Info("backtest gate passed with exact deploy params",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"reason", verdict.Reason, "potential_pct", verdict.PotentialPct)
			if candidate.ModelAssumptions == nil {
				candidate.ModelAssumptions = make(map[string]any)
			}
			candidate.ModelAssumptions["walkForwardProof"] = map[string]any{
				"passed":           true,
				"netEV":            verdict.Traded.NetEV,
				"ci95Lower":        verdict.Traded.CI95Lower,
				"ci95Upper":        verdict.Traded.CI95Upper,
				"oosSharpe":        verdict.Traded.OOSSharpe,
				"oosSortino":       verdict.Traded.OOSSortino,
				"winRate":          verdict.Traded.WinRate,
				"profitFactor":     verdict.Traded.ProfitFactor,
				"maxDrawdown":      verdict.Traded.MaxDD,
				"regimesTested":    verdict.Traded.RegimesTested,
				"worstPeriod":      verdict.Traded.WorstPeriod,
				"sampleSufficient": verdict.Traded.SampleSufficient,
			}
		}

		params := pionex.NativeFuturesGridCreateParams{
			Base: base, Quote: quote, BUOrderData: data,
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
			// v2.0.168 fail-closed: a failed pre-flight check no longer falls
			// through to direct creation. The pre-168 shape mismatch (camelCase
			// create-struct against a snake_case endpoint) made most checks fail
			// silently — "attempting direct creation" was the norm, the min-
			// investment and liquidation validations were decorative. Unknown
			// validation state = no deploy.
			worker.logger.Warn("entry gate: checkParams failed — real deploy refused (fail-closed)",
				"component", "autogrid_worker", "symbol", candidate.Symbol, "error", checkErr)
			deployErrors = append(deployErrors, fmt.Sprintf("%s: checkParams: %v", candidate.Symbol, checkErr))
			worker.rejectCandidate(ctx, candidate,
				"preflight checkParams недоступен — деплой отказан (fail-closed, v2.0.168): "+checkErr.Error(),
				map[string]any{"checkParamsRefused": true})
			continue
		} else if check != nil {
			// v2.0.172 (audit): log the exchange's ACTUAL boundaries and
			// estimate presence for diagnostics — the estimate-clearing bug
			// (investment outside [min, max] range) was invisible because
			// we only checked the floor.
			worker.logger.Info("checkParams diagnostics",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"min_investment", check.GetMinInvestment().StringFixed(2),
				"max_investment", check.GetMaxInvestment().StringFixed(2),
				"our_investment", investAmount.StringFixed(2),
				"liq_up", check.EstimateLiquidationUp.StringFixed(4),
				"liq_down", check.EstimateLiquidationDown.StringFixed(4))
			if check.GetMinInvestment().GreaterThan(decimal.Zero) &&
				investAmount.LessThan(check.GetMinInvestment()) {
				deployErrors = append(deployErrors, fmt.Sprintf(
					"%s: budget %s below Pionex minimum investment %s",
					candidate.Symbol, settings.BudgetUSDT, check.GetMinInvestment(),
				))
				continue
			}
			// v2.0.172: check the ceiling too — the exchange clears
			// liquidation estimates when the investment is outside the
			// allowed range, making the both-empty refuse fire with no
			// diagnostic context.
			if check.GetMaxInvestment().GreaterThan(decimal.Zero) &&
				investAmount.GreaterThan(check.GetMaxInvestment()) {
				maxReason := fmt.Sprintf(
					"инвестиция %s выше максимума биржи %s — биржа очищает оценки ликвидации вне диапазона",
					investAmount.StringFixed(2), check.GetMaxInvestment().StringFixed(2))
				deployErrors = append(deployErrors, fmt.Sprintf("%s: %s", candidate.Symbol, maxReason))
				// v2.0.172c (audit P2-4): the refusal must reach the
				// candidate card AND the entry-decision journal.
				worker.rejectCandidate(ctx, candidate, maxReason,
					map[string]any{"maxInvestmentExceeded": true,
						"max_investment": check.GetMaxInvestment().StringFixed(2)})
				worker.journalEntryDecision(ctx, entryIn, entryOutcomeReject, "EXCHANGE_MAX_INVESTMENT",
					maxReason, map[string]any{
						"max_investment": check.GetMaxInvestment().StringFixed(2),
						"our_investment": investAmount.StringFixed(2),
					})
				continue
			}
		}
		// v2.0.161 liquidation guard: the exchange's own estimates must sit
		// at least liqGuardMinPct beyond the stored stops — a stop closer to
		// the estimated liquidation has no room for wicks/funding drift
		// before the exchange force-closes (prod: ORDI −$10.45, NEAR −$10.41
		// were exactly this geometry).
		// v2.0.168: BOTH estimates empty = the exchange told us nothing about
		// the wall — unknown risk is not acceptable risk on a leveraged grid.
		// (Prod postmortem: the pre-168 request-shape bug made every check
		// come back estimate-less and all six running bots carried NULL liq
		// columns with the guard inert.)
		var liqEstUp, liqEstDown *decimal.Decimal
		if check != nil {
			if check.EstimateLiquidationUp.GreaterThan(decimal.Zero) {
				v := check.EstimateLiquidationUp
				liqEstUp = &v
			}
			if check.EstimateLiquidationDown.GreaterThan(decimal.Zero) {
				v := check.EstimateLiquidationDown
				liqEstDown = &v
			}
		}
		if check != nil && liqEstUp == nil && liqEstDown == nil {
			deployErrors = append(deployErrors,
				fmt.Sprintf("%s: checkParams вернул без оценок ликвидации", candidate.Symbol))
			worker.rejectCandidate(ctx, candidate,
				"ликвидационный гейт: биржа не вернула оценок ликвидации — неизвестный риск не является допустимым риском, деплой отказан (v2.0.168)",
				map[string]any{"liqEstimatesMissing": true})
			continue
		}
		if reason := liquidationGuardReason(trend, candidate.CurrentPrice,
			derefZero(liqEstDown), derefZero(liqEstUp), slPrice, slHighPrice, botLev); reason != "" {
			deployErrors = append(deployErrors, fmt.Sprintf("%s: %s", candidate.Symbol, reason))
			worker.logger.Info("entry gate: liquidation guard refused deploy",
				"component", "autogrid_worker", "symbol", candidate.Symbol,
				"leverage", botLev, "liq_up", derefZero(liqEstUp).StringFixed(4),
				"liq_down", derefZero(liqEstDown).StringFixed(4))
			worker.rejectCandidate(ctx, candidate, reason, map[string]any{"liqGuardBlocked": true})
			continue
		}
		trancheMarkers := map[string]any{
			"trancheDeployed": trancheFlag(settings.TrancheDeployEnabled),
			"trancheBase":     slotBudget.String(),
			"trancheEntry":    candidate.CurrentPrice.String(),
			"atrPctEntry":     atrPct,
			// v2.0.155: the symbol's deploy-time precision rides model_state
			// so the manage loop rounds the trailing-SL candidate to it.
			"pricePrecision": pricePrecision,
		}
		// v2.0.139 telemetry: the stress-floor marker rides model_state
		// exactly when the floor lifted the cap (paper twin: the bot row's
		// entryFeatures field).
		if stress.floored {
			trancheMarkers["stressLossFloor"] = true
		}
		// v2.0.161 review P2: the exemption cohort markers must persist on
		// the REAL fleet (model_state via TrancheState) or the 14-day
		// outcome partition for validate-or-rollback exists on paper only.
		for _, cohort := range []string{"betaDownExempt", "dirTrendShort", "dirTrendLong"} {
			if on, _ := candidate.ModelAssumptions[cohort].(bool); on {
				trancheMarkers[cohort] = true
			}
		}
		var storedSL *decimal.Decimal
		var storedSLHigh *decimal.Decimal
		if settings.StopLossMode == "ADAPTIVE_ATR" {
			storedSL = &slPrice
			storedSLHigh = slHighPrice
		}
		riskRewardDec := decimal.NewFromFloat(adaptiveRes.RiskRewardRatio)
		adaptiveStrat := adaptiveRes.AdaptiveStrategy

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
			// v2.0.13: per-bot tranche markers (audit F1/F6) — deriving the
			// pending tranche from live settings would auto-inject real margin
			// into every old bot on any budget raise. v2.0.78: they ride the
			// lifecycle INSERT — the old follow-up UPDATE was best-effort and
			// its failure silently stripped the bot of its tranche contract.
			// Markers match the paper model_state contract.
			TrancheState:     trancheMarkers,
			TargetPrice:      targetPriceDec,
			StopLossPrice:    storedSL,
			StopLossHigh:     storedSLHigh,
			TrailingSLPrice:  storedSL,
			AdaptiveStrategy: &adaptiveStrat,
			RiskRewardRatio:  &riskRewardDec,
			LiqPriceUp:       liqEstUp,
			LiqPriceDown:     liqEstDown,
			// v2.0.165: the candidate link the outcome cohorts need.
			CandidateID: &candidate.ID,
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

		// v2.0.138 entry chain: the ALLOW row — one per created bot, with the
		// config version that admitted it.
		worker.journalEntryDecision(ctx, EntryChainInput{
			Path: EntryPathScannerReal, Settings: settings, Symbol: candidate.Symbol,
			Direction: entryDirectionFromTrend(trend), Fleet: "REAL", RefID: botID,
		}, entryOutcomeAllow, "CLEAR", "", nil)

		// v2.0.89 round-trip fee ledger: the taker entry fee the wallet paid
		// at deploy is booked durably — the epoch formula subtracts Σ fees.
		// Basis matches pionex.EntryFeeUSDT (taker 0.05% × investment ×
		// leverage), the same model the invest_in pours book.
		if feeErr := recordRealEntryFee(ctx, worker.db, botID,
			pionex.EntryFeeUSDT(investAmount, botLev), "entryFeeUsdt"); feeErr != nil {
			worker.logger.Error("entry fee booking failed — ledger fee leg incomplete for this deploy",
				"component", "autogrid_worker", "bot_id", botID, "error", feeErr)
		}

		var botNum int
		_ = worker.db.QueryRow(ctx, `SELECT COALESCE(bot_number, 0) FROM grid_bots WHERE id = $1`, botID).Scan(&botNum)

		_ = LogBotEvent(ctx, worker.db, botID, botNum, "REAL", candidate.Symbol, "CREATED", &candidate.CurrentPrice, nil, map[string]any{
			"leverage": botLev, "gridNum": mesh.GridNum, "lowerPrice": lowerPrice, "upperPrice": upperPrice, "budget": slotBudget, "capitalScaled": capitalScaled,
		})
		_ = QueueTelegramEvent(ctx, worker.db, "BOT_CREATED", map[string]any{
			"bot_number": botNum, "symbol": candidate.Symbol, "direction": strings.ToUpper(trend),
			"leverage": botLev, "lower_price": lowerPrice, "upper_price": upperPrice,
			"grid_num": mesh.GridNum, "quote_investment": slotBudget, "source": "REAL",
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
		// Only a pass that actually ran deploys owns last_error: an idle
		// clean pass must not wipe freeze notes set in THIS same pass by
		// noteDeployBlock (cascade/feed-health defer with zero ACCEPTED
		// candidates and per-candidate cuts never touch deployErrors).
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
	// v2.0.78 CRIT-3: rows without a buOrderId are never touched — a stop
	// request on an unadopted submission makes the row a STOP_REQUESTED zombie
	// (the manage loop only supervises rows WITH a remote id) that blocks the
	// symbol and the slot forever. Adoption owns NULL-bu rows instead.
	if _, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED', closed_reason = COALESCE(closed_reason, 'AUTOGRID_STOP'), updated_at = NOW()
		WHERE autogrid_settings_id = $1
		  AND bu_order_id IS NOT NULL
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
				    reconciliation_state = $2,
				    closed_at = NOW(), last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
				WHERE id = $1
			`, botID, TerminalFinalPendingExchange)
			worker.logger.Info("Pionex grid already closed remotely, marked STOPPED pending exchange final",
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
	if _, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED',
		    reconciliation_state = 'CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING',
		    last_error = NULL, updated_at = NOW()
		WHERE id = $1
	`, botID); err != nil {
		// The marker arms the reconcile loop's cancel verification; losing it
		// to a swallowed error left STOP_REQUESTED dead code (pre-0039 the
		// 38-char value also overflowed VARCHAR(32) silently).
		worker.logger.Warn("mark cancel accepted failed",
			"component", "autogrid_worker", "bot_id", botID, "error", err)
	}
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
//
// v2.0.86: the economic calendar has two writers — the FRED releases
// calendar (primary, 6h refresh of a ±14d window) and ForexFactory
// (fallback, 429-backoff kept). The calendar counts as ALIVE when either
// source has an event within the next 7 days, so a dead FF feed alone no
// longer pages the operator while FRED keeps the gate armed. The alarm now
// fires only when BOTH sources are stale/empty on the +7d window.
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
	var freshEvents int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM economic_events
		WHERE event_time > NOW() AND event_time < NOW() + INTERVAL '7 days'
		  AND (country = 'USD' OR country IS NULL OR country = '')
		  AND source IN ('FRED', 'forexfactory', 'BLS_SCHED')
	`).Scan(&freshEvents); err == nil && freshEvents == 0 {
		alarm("economic_events",
			"Календарь USD пуст: нет событий (FRED/ForexFactory) в окне +7д — эконом-гейт деплоя слеп (оба источника мертвы?)")
	}
	var lastLiq *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(captured_at) FROM liquidation_events
	`).Scan(&lastLiq); err == nil {
		since := time.Duration(1<<62 - 1)
		if lastLiq != nil {
			since = time.Since(*lastLiq)
		}
		// v2.0.167 (week-audit P1-2): the cascade gate FAIL-CLOSES LONG/NEUTRAL
		// entries at 15m of silence — the operator must learn about the
		// freeze in its first minutes, not three hours later. 20m threshold
		// (past the gate block, inside the first reconnect cycles), hourly
		// dedup via the alarm() cadence.
		if since > 20*time.Minute && since <= 3*time.Hour {
			alarm("liquidation_events",
				"Ликвидации не пишутся ~"+since.Truncate(time.Minute).String()+
					" — каскад-гейт уже БЛОКИРУЕТ входы LONG/NEUTRAL (порог 15м). Проверьте WS-источник (app_config.liquidation_source); авто-фэйловер на запасной источник через 15м тишины (v2.0.167)")
		}
		if lastLiq == nil || since > 3*time.Hour {
			alarm("liquidation_events",
				"Ликвидации не пишутся >3ч — каскад-гейт слеп (WS-источник мёртв; см. app_config.liquidation_source)")
		}
	}
}

// terminalSettleResult is the v2.0.89 terminal-final decision: the figure to
// persist (nil = NULL final, "no telemetry → no final"), the
// model_state.finalProfitSource marker, and the close cost riding next to an
// estimate. The v2.0.75–88 honesty gate (refuse grid_funding_residual on
// loss-class closes) is superseded: the residual/flat legs are no longer
// produced by the chain at all (client.SettledProfit v2.0.89), so every
// terminal either settles at the exchange's own netted total or at the
// telemetry-net-close estimate.
type terminalSettleResult struct {
	final     any             // *decimal.Decimal or nil (SQL NULL)
	marker    string          // finalProfitSource value
	closeCost decimal.Decimal // 0 unless final is a telemetry_net_close estimate
}

// settleTerminalFinal resolves the terminal final for one REAL bot from the
// exchange chain result:
//
//   - profitExited / total-alias: the exchange's netted total — accepted for
//     every close class (the position-close leg is inside by construction)
//     except the guarded anomaly of a POSITIVE total on a loss-class close
//     (decided from BOTH the stored closed_reason and the exchange reasonBy —
//     our manage stop comes back as "user cancel", so the stored reason is
//     the only loss witness); a gated figure falls through to the estimate;
//   - otherwise: estimate b — telemetry_last_total − (taker 0.05% + slippage
//     0.05%) × last inventory notional, floored at −max_loss − close cost for
//     EXECUTED stops (manage STOP_LOSS and native STOP_LOSS_NATIVE alike:
//     max_loss carries the deployed stop's implied loss, and the epoch proved
//     the mark-to-execution gap — ICP/SUI fired native stops while their last
//     telemetry marks still read positive);
//   - no telemetry: NULL final, marker 'none' — never a guessed figure.
//
// Errors from the telemetry probe are surfaced as a Warn + NULL settle (the
// exactly-once marker still lands, so a transient DB failure cannot loop the
// backfill forever) — the failure is logged, never swallowed silently.
func (worker *Worker) settleTerminalFinal(
	ctx context.Context,
	botID string,
	closedAtAnchor time.Time,
	maxLoss *decimal.Decimal,
	storedReason, exchangeReason string,
	settled decimal.Decimal,
	source pionex.FinalProfitSource,
) terminalSettleResult {
	if gated := gateSettledProfit(settled, source, storedReason, exchangeReason); gated != nil {
		return terminalSettleResult{final: *gated, marker: string(source)}
	}
	if source == pionex.FinalProfitExited || source == pionex.FinalProfitTotalAlias {
		worker.logger.Warn("exchange total refused: positive total on a loss-class close — pricing the close from telemetry instead",
			"component", "autogrid_worker", "bot_id", botID,
			"stored_reason", storedReason, "reason_by", exchangeReason,
			"exchange_total", settled.StringFixed(4))
	}
	stop := decimal.Zero
	if maxLoss != nil {
		stop = *maxLoss
	}
	// The stop floor consults the STORED reason first (our vocabulary, the
	// manage/native stop witness) and the exchange-mapped terminal outcome
	// second — the exchange's own reasonBy words ("loss_stop") never match
	// our dictionary directly.
	floorReason := storedReason
	if !exchangeStopClass(floorReason) && exchangeReason != "" {
		floorReason = exchangeReason
	}
	estimate, closeCost, err := estimateTerminalFinal(
		ctx, worker.db, botID, &closedAtAnchor, stop, floorReason)
	if err != nil {
		worker.logger.Warn("terminal estimate probe failed — final settled NULL",
			"component", "autogrid_worker", "bot_id", botID, "error", err)
		return terminalSettleResult{final: nil, marker: string(pionex.FinalProfitNone)}
	}
	if estimate == nil {
		worker.logger.Warn("terminal settle has no exchange total and no telemetry — final left NULL",
			"component", "autogrid_worker", "bot_id", botID, "reason", storedReason)
		return terminalSettleResult{final: nil, marker: string(pionex.FinalProfitNone)}
	}
	return terminalSettleResult{
		final:     *estimate,
		marker:    string(pionex.FinalProfitTelemetryNetClose),
		closeCost: closeCost,
	}
}

type managedBot struct {
	id, accountID, remoteID, localStatus, symbol, direction   string
	lower, upper                                              decimal.Decimal
	rowNum, adjustments, leverage                             int
	pnlTarget, maxLoss                                        *decimal.Decimal
	antiHuntStop                                              *decimal.Decimal
	investment                                                decimal.Decimal
	botNumber                                                 int
	peak                                                      decimal.Decimal
	createdAt                                                 time.Time
	closedReason                                              string
	trancheDeployed                                           int
	trancheBase                                               *string
	trancheEntry                                              *string
	liqPriceUp                                                *decimal.Decimal
	liqPriceDown                                              *decimal.Decimal
	modelStateMap                                             map[string]any
	atrEntry                                                  float64
	trancheFailAt                                             *string
	trancheIntentAt                                           *string
	fundingPaid                                               decimal.Decimal
	lastFundingReconcileAt                                    *time.Time
	shiftFloatingOffset                                       decimal.Decimal
	shiftPosition                                             decimal.Decimal
	supervisionFloor                                          decimal.Decimal
	rebasePool                                                decimal.Decimal
	lastEntryMark                                             *decimal.Decimal
	lastSignedPos                                             *decimal.Decimal
	wickShieldTriggeredAt                                     *string
	wickShieldExtreme                                         *decimal.Decimal
	wickShieldLastClearedAt                                   *string
	rebasePos                                                 decimal.Decimal
	peakFloor                                                 *decimal.Decimal
	shiftRealizedBase                                         decimal.Decimal
	lastRemoteGridProfit                                      decimal.Decimal
	lastAdjustmentsCount                                      int
	targetPrice, stopLossPrice, stopLossHigh, trailingSLPrice *decimal.Decimal
	adaptiveStrategy                                          string
	riskRewardRatio                                           *decimal.Decimal
	// pricePrecision carries the deploy-time symbol precision (model_state)
	// for the trailing-SL rounding; trailingSLFailAt is the 1h backoff
	// marker for a failed native SL update (v2.0.155).
	pricePrecision   int
	trailingSLFailAt *string
}

// managedBot is the per-pass supervision row for REAL bots (v2.0.149:
// hoisted to package level so the blind-stop helper can share it).

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
	// v2.0.98: keep the real-time lane's INDEX subscriptions aligned with the
	// RUNNING fleet before the pass samples prices.
	worker.syncWSSubscriptions(ctx, *settings)
	// v2.0.137: lane observability — ORDERBOOK/TRADE delivery volume and
	// drop reasons for BOTH lanes, every 5 minutes, so a silently dead or
	// skewed feed is visible in the logs (not inferred from bot regimes).
	// _5m fields are interval deltas against the previous snapshot (rates,
	// not cumulative totals); ob_clamp_* come from the WS lane's server-ts
	// clamp. Silence alarms fire only when a real interval was measured —
	// the first pass right after boot has no baseline yet.
	if worker.ofiEngine != nil && time.Since(worker.ofiStatsAt) > 5*time.Minute {
		intervalStart := worker.ofiStatsAt
		worker.ofiStatsAt = time.Now()
		stats := worker.ofiEngine.Stats()
		prev := worker.ofiStatsPrev
		worker.ofiStatsPrev = stats
		// Counters only grow; a shrink means a fresh engine (restart), so
		// clamp the delta at zero instead of logging a wrapped uint64.
		drop := func(cur, was uint64) uint64 {
			if cur < was {
				return 0
			}
			return cur - was
		}
		obDelta := drop(stats.OrderbookFrames, prev.OrderbookFrames)
		tradesDelta := drop(stats.Trades, prev.Trades)
		args := []any{
			"component", "autogrid_worker",
			"symbols", stats.Symbols,
			"orderbook_frames", stats.OrderbookFrames,
			"trades", stats.Trades,
			"trades_dropped_dedup", stats.TradesDroppedDedup,
			"trades_dropped_too_old", stats.TradesDroppedTooOld,
			"trades_dropped_isolation", stats.TradesDroppedIsolation,
			"orderbook_dropped_sequence", stats.OrderbookDroppedSequence,
			"orderbook_dropped_crossed", stats.OrderbookDroppedCrossed,
			"orderbook_dropped_unsynced", stats.OrderbookDroppedUnsynced,
			"orderbook_frames_5m", obDelta,
			"trades_5m", tradesDelta,
			"trades_dropped_dedup_5m", drop(stats.TradesDroppedDedup, prev.TradesDroppedDedup),
			"trades_dropped_too_old_5m", drop(stats.TradesDroppedTooOld, prev.TradesDroppedTooOld),
			"trades_dropped_isolation_5m", drop(stats.TradesDroppedIsolation, prev.TradesDroppedIsolation),
			"orderbook_dropped_sequence_5m", drop(stats.OrderbookDroppedSequence, prev.OrderbookDroppedSequence),
			"orderbook_dropped_crossed_5m", drop(stats.OrderbookDroppedCrossed, prev.OrderbookDroppedCrossed),
			"orderbook_dropped_unsynced_5m", drop(stats.OrderbookDroppedUnsynced, prev.OrderbookDroppedUnsynced),
		}
		if !intervalStart.IsZero() {
			args = append(args, "interval_seconds", int64(time.Since(intervalStart).Seconds()))
		}
		if worker.wsLane != nil {
			// WS-side clamp view: server-ts trust rate and worst accepted
			// skew — a high fallback share or big max skew indicts the time
			// domain, not the lanes themselves.
			used, fallback, maxSkewMS := worker.wsLane.OrderbookClampStats()
			args = append(args, "ob_clamp_used", used, "ob_clamp_fallback", fallback, "ob_max_skew_ms", maxSkewMS)
		}
		worker.logger.Info("ofi engine ingest stats", args...)
		if !intervalStart.IsZero() {
			// Aggregate simplification: orderbook_frames_5m > 0 already
			// implies at least one subscribed symbol (the engine only
			// tracks subscribed symbols) received frames over the interval.
			if obDelta > 0 && tradesDelta == 0 {
				worker.logger.Warn("ofi TRADE lane silent while ORDERBOOK flows",
					"component", "autogrid_worker", "orderbook_frames_5m", obDelta)
			} else if obDelta == 0 {
				worker.logger.Warn("ofi ORDERBOOK lane silent",
					"component", "autogrid_worker", "symbols", stats.Symbols)
			}
		}
	}
	// v2.0.139: daily per-gate shadow counterfactual report (package D).
	if time.Since(worker.gateValueAt) > 24*time.Hour {
		worker.gateValueAt = time.Now()
		worker.buildGateValueReport(ctx)
	}
	// v2.0.111: surface storm arm/extend transitions once per arm.
	worker.maybeLogStormState(ctx)
	// v2.0.99: overwrite estimate-class terminal finals with the exchange's
	// netted total once the finished record surfaces (throttled internally).
	worker.recheckPendingExchangeFinals(ctx, *settings)
	// v2.0.104: adopt exchange-side running grids our DB doesn't actively
	// track (throttled internally).
	worker.reconcileOrphanExchangeBots(ctx, *settings)
	var count int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID).Scan(&count); err != nil || count == 0 {
		// v2.0.83: even with zero running bots the epoch still owns its
		// closed terminals — the bot-aggregate ledger keeps flowing.
		if err == nil {
			worker.captureBotAggregateEquity(ctx, *settings)
		}
		return clampInterval(settings.ManageIntervalSeconds), err
	}
	priceBySymbol, priceErr := worker.priceMap(ctx)
	if priceErr != nil {
		worker.logger.Warn("fetch tickers for management", "component", "autogrid_worker", "error", priceErr)
	}
	// v2.0.149 (exit audit F1): a failed or empty price map is a blindness
	// episode, not a WARN — the PnL stops degrade to the exchange-total
	// basis and the price exits disarm until the feed recovers.
	worker.notePriceFeedMapHealth(priceErr != nil || len(priceBySymbol) == 0)
	rows, err := worker.db.Query(ctx, `
		SELECT id, account_id, bu_order_id, status, symbol, direction,
		       lower_price, upper_price, grid_num, adjustments_count,
		       pnl_target_usdt, max_loss_usdt, quote_investment, leverage,
		       anti_hunt_stop_price, COALESCE(bot_number, 0),
		       COALESCE(peak_pnl_usdt, 0), created_at, COALESCE(closed_reason, ''),
		       COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0),
		       NULLIF(model_state->>'trancheBase',''),
		       NULLIF(model_state->>'trancheEntry',''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0),
		       NULLIF(model_state->>'trancheFailAt',''),
		       NULLIF(model_state->>'trancheIntentAt',''),
		       COALESCE(funding_paid_usdt, 0), last_funding_reconcile_at,
		       COALESCE(NULLIF(model_state->>'shiftFloatingOffset','')::NUMERIC, 0),
		       COALESCE(NULLIF(model_state->>'shiftPosition','')::NUMERIC, 0),
		       COALESCE(supervision_floor_pnl_usdt, unrealized_pnl_usdt, 0),
		       COALESCE(NULLIF(model_state->>'rebasePool','')::NUMERIC, 0),
		       NULLIF(model_state->>'payloadEntryMark','')::NUMERIC,
		       NULLIF(model_state->>'payloadSignedPos','')::NUMERIC,
		       NULLIF(model_state->>'wickShieldTriggeredAt',''),
		       NULLIF(model_state->>'wickShieldExtreme','')::NUMERIC,
		       NULLIF(model_state->>'wickShieldLastClearedAt',''),
		       COALESCE(NULLIF(model_state->>'rebasePos','')::NUMERIC, 0),
		       NULLIF(model_state->>'peakFloorUsdt','')::NUMERIC,
		       COALESCE(NULLIF(model_state->>'shiftRealizedBase','')::NUMERIC, 0),
		       COALESCE(NULLIF(model_state->>'lastRemoteGridProfit','')::NUMERIC, 0),
		       COALESCE(NULLIF(model_state->>'lastAdjustmentsCount','')::INT, 0),
		       target_price, stop_loss_price, stop_loss_high, trailing_sl_price,
		       COALESCE(adaptive_strategy, ''), risk_reward_ratio,
		       COALESCE(NULLIF(model_state->>'pricePrecision','')::INT, 0),
		       NULLIF(model_state->>'trailingSLFailAt',''),
		       liq_price_up, liq_price_down, model_state
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		ORDER BY updated_at
	`, settings.ID)
	if err != nil {
		return clampInterval(settings.ManageIntervalSeconds), err
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
			&item.peak, &item.createdAt, &item.closedReason,
			&item.trancheDeployed, &item.trancheBase, &item.trancheEntry,
			&item.atrEntry, &item.trancheFailAt, &item.trancheIntentAt,
			&item.fundingPaid, &item.lastFundingReconcileAt,
			&item.shiftFloatingOffset, &item.shiftPosition,
			&item.supervisionFloor, &item.rebasePool, &item.lastEntryMark, &item.lastSignedPos,
			&item.wickShieldTriggeredAt, &item.wickShieldExtreme,
			&item.wickShieldLastClearedAt, &item.rebasePos, &item.peakFloor,
			&item.shiftRealizedBase, &item.lastRemoteGridProfit, &item.lastAdjustmentsCount,
			&item.targetPrice, &item.stopLossPrice, &item.stopLossHigh, &item.trailingSLPrice,
			&item.adaptiveStrategy, &item.riskRewardRatio,
			&item.pricePrecision, &item.trailingSLFailAt,
			&item.liqPriceUp, &item.liqPriceDown, &item.modelStateMap,
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
				// v2.0.89 terminal final: the exchange's netted total
				// (profitExited/alias) or the telemetry-net-close estimate —
				// the residual grid-fallback is no longer a final anywhere.
				// The estimate runs even when the finished-list record is gone
				// (list horizon expired): it needs no exchange payload, only
				// our own telemetry chain.
				settled, source := decimal.Zero, pionex.FinalProfitNone
				exchangeReason := ""
				if finished := findFinishedGridRecord(ctx, client, bot.remoteID); finished != nil {
					settled, source = finished.SettledProfit()
					exchangeReason = strings.TrimSpace(finished.ReasonBy)
				}
				decision := worker.settleTerminalFinal(ctx, bot.id, time.Now(),
					bot.maxLoss, bot.closedReason, exchangeReason, settled, source)
				finalRealizedArg := decision.final
				// v2.0.99: a telemetry-estimate final is NOT the exchange's
				// truth — the finished-list record can be missing at cancel
				// time (finalization lag, paging depth, list error) and the
				// estimate then diverges from the app's netted total (ARB
				// #1286: ours +2.23 vs exchange +1.66). Such rows land in
				// TERMINAL_FINAL_PENDING_EXCHANGE and a bounded re-check
				// sweep overwrites the estimate with the exchange total when
				// the record surfaces. Only a real exchange total confirms
				// terminally.
				reconState := pendingOrConfirmedRecon(decision.marker)
				if _, err := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET status = 'STOPPED', closed_reason = COALESCE(NULLIF(closed_reason, ''), $2),
					    reconciliation_state = $7,
					    realized_pnl_usdt = CASE WHEN $4::BOOLEAN THEN NULL ELSE COALESCE($3, realized_pnl_usdt) END,
					    unrealized_pnl_usdt = 0,
					    supervision_floor_pnl_usdt = 0,
					    fees_paid_usdt = fees_paid_usdt + $6::NUMERIC,
					    model_state = jsonb_set(
					        CASE WHEN $6::NUMERIC = 0 THEN COALESCE(model_state, '{}'::jsonb)
					             ELSE jsonb_set(COALESCE(model_state, '{}'::jsonb), '{closeCostUsdt}', to_jsonb($6::NUMERIC)) END,
					        '{finalProfitSource}', to_jsonb($5::TEXT)),
					    closed_at = NOW(), last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
					WHERE id = $1
				`, bot.id, closedReason, finalRealizedArg, finalRealizedArg == nil,
					decision.marker, decision.closeCost, reconState); err != nil {
					worker.logger.Error("persist already-closed grid state",
						"component", "autogrid_worker", "bot_id", bot.id, "error", err)
				}
				// v2.0.165: outcome cohort row for the already-closed settle path.
				outcomeTotal := decimal.Zero
				if d, ok := finalRealizedArg.(decimal.Decimal); ok {
					outcomeTotal = d
				}
				recordRealBotOutcome(ctx, worker.db, worker.logger, bot.id, outcomeTotal, closedReason)
				worker.logger.Info("Pionex grid not found or already closed on exchange, marked STOPPED",
					"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id,
					"final_pnl_persisted", finalRealizedArg != nil,
					"final_profit_source", decision.marker, "close_cost", decision.closeCost,
					"reconciliation_state", reconState)
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
		// v2.0.101 running-payload witness: after a range shift the API
		// re-bases positionOpenPrice (AAVE #1288: payload float −0.47 while
		// the app nets −1.52 from the true inventory cost ~158.6 vs payload
		// ~154.7) — supervision and the operator see an understated loss
		// until the true-cost field is pinned. Log the RAW running payload
		// once per bot after each adjustment so the next parser change is
		// driven by a live witness, not doc guesses.
		if bot.adjustments > 0 {
			witnessKey := fmt.Sprintf("%s:%d", bot.id, bot.adjustments)
			if worker.runningRawLogged == nil {
				worker.runningRawLogged = make(map[string]bool)
			}
			if !worker.runningRawLogged[witnessKey] {
				worker.runningRawLogged[witnessKey] = true
				if rawBytes, rawErr := json.Marshal(remote.BUOrderData); rawErr == nil {
					worker.logger.Warn("adjusted running grid raw payload witness",
						"component", "autogrid_worker", "bot_number", bot.botNumber,
						"symbol", bot.symbol, "adjustments", bot.adjustments,
						"raw", truncateForLog(string(rawBytes)))
				}
			}
		}
		// v2.0.74: realized must mirror the app's "Grid Profit" — the
		// exchange's accumulated realized grid profit (profitReduce), NOT
		// profitWithdrawn, which stays 0 while a futures grid compounds its
		// profit internally.
		//
		// v2.0.113 two-circuit PnL architecture:
		// 1. Display/TG circuit (unrealized): raw exchange floating PnL using the
		//    exchange payload's PositionOpenPrice. Restores 1:1 screen parity for VIRTUAL
		//    and future shifts without phantom offsets.
		// 2. Risk/Supervision circuit (supervisionFloor): conservative floor = min(raw, raw + rebasePool)
		//    detects jumps in positionOpenPrice (>1% across passes from shifts or tranche-2 invest_in),
		//    absorbing lost historical basis into rebasePool so stops and radar never suffer from
		//    false optimism on underwater positions (the PENGU #1386 class).
		remoteGrid := remote.BUOrderData.GridProfit()
		// v2.0.127: Detect if Pionex reset gridProfit after a range shift (OP class):
		if !bot.lastRemoteGridProfit.IsZero() && remoteGrid.LessThan(bot.lastRemoteGridProfit) {
			bot.shiftRealizedBase = bot.shiftRealizedBase.Add(bot.lastRemoteGridProfit)
			worker.logger.Info("absorbed prior range grid profit into shiftRealizedBase",
				"component", "autogrid_worker",
				"bot_number", bot.botNumber, "symbol", bot.symbol,
				"prior_grid_profit", bot.lastRemoteGridProfit.String(),
				"new_grid_profit", remoteGrid.String(),
				"total_shift_realized_base", bot.shiftRealizedBase.String())
		}
		bot.lastRemoteGridProfit = remoteGrid
		realized := bot.shiftRealizedBase.Add(remoteGrid)

		unrealized := decimal.Zero
		supervisionFloor := decimal.Zero
		currentEntry := remote.BUOrderData.PositionOpenPrice
		// v2.0.149 (exit audit F1): set when the floating leg had to be
		// taken from the exchange total because the local price is dead —
		// the marker lands in model_state and is stripped on the first
		// healthy pass.
		blindFloatingFromExchange := false

		isZeroPos := remote.BUOrderData.Position.IsZero()
		var signedPos decimal.Decimal
		if !isZeroPos {
			signedPos = remote.BUOrderData.Position
			// Pionex may report the grid position as an unsigned magnitude
			// even for short grids. If a SHORT reports a positive position,
			// treat it as a magnitude and negate: profit for a short is
			// open−price, and an unsigned feed would otherwise invert every
			// short bot's PnL (closing winners, holding losers). A signed
			// feed (negative position) already encodes the side and passes
			// through unchanged.
			if bot.direction == "SHORT" && signedPos.IsPositive() {
				signedPos = signedPos.Neg()
			}
		}

		// Point Д.4: Zero position or sign flip clears pool, mark, and legacy offsets.
		isFlip := false
		if !signedPos.IsZero() {
			if bot.lastSignedPos != nil && !bot.lastSignedPos.IsZero() && signedPos.Mul(*bot.lastSignedPos).IsNegative() {
				isFlip = true
			} else if !bot.shiftPosition.IsZero() && signedPos.Mul(bot.shiftPosition).IsNegative() {
				isFlip = true
			}
		}

		if isZeroPos || isFlip {
			// Inventory closed into realized grid profit (or flipped side).
			// Strip legacy offsets, rebasePool, entry mark, and signed pos.
			bot.shiftFloatingOffset = decimal.Zero
			bot.shiftPosition = decimal.Zero
			bot.rebasePool = decimal.Zero
			bot.rebasePos = decimal.Zero
			bot.lastEntryMark = nil
			bot.lastSignedPos = nil
		}

		if !isZeroPos {
			// Raw unrealized matches the exchange app's Floating PnL using the payload's PositionOpenPrice
			if currentEntry.GreaterThan(decimal.Zero) && price.GreaterThan(decimal.Zero) {
				unrealized = signedPos.Mul(price.Sub(currentEntry))
			} else if !price.GreaterThan(decimal.Zero) {
				// v2.0.149 (exit audit F1): a dead price must not zero-mask
				// the floating leg. TotalProfit is the exchange's own
				// grid+floating total and needs no local price; it is
				// accepted only as a LOSS signal (see blindFloatingEstimate)
				// so the books are never flattered nor invented against.
				if est := blindFloatingEstimate(remote.BUOrderData.TotalProfit, realized); est.IsNegative() {
					// v2.0.155 (review SEC-001): a floating loss beyond the
					// bot's whole notional (investment × leverage) is not a
					// market move — it is a glitch or a poisoned response.
					// Reject the signal with an Error page rather than close
					// the fleet on it; the exchange's own LossStop keeps
					// guarding the position either way.
					if !blindEstimatePlausible(est, bot.investment, bot.leverage) {
						worker.logger.Error("blind floating estimate beyond notional — signal rejected",
							"component", "autogrid_worker", "bot_number", bot.botNumber,
							"symbol", bot.symbol, "estimate", est.StringFixed(2))
					} else {
						unrealized = est
						blindFloatingFromExchange = true
					}
				}
			}

			// Point Д.1 & Д.5: Rebase detection across passes.
			// v2.0.127: Trigger if bot has adjusted or jump > 0.1% on non-flip
			hasAdjusted := bot.adjustments > bot.lastAdjustmentsCount
			if !isFlip && bot.lastEntryMark != nil && bot.lastEntryMark.IsPositive() && currentEntry.IsPositive() {
				jump := currentEntry.Sub(*bot.lastEntryMark).Div(*bot.lastEntryMark).Abs()
				if hasAdjusted || jump.GreaterThan(decimal.NewFromFloat(0.001)) {
					// The exchange re-based positionOpenPrice. The lost floating PnL is signedPos * (newEntry - oldEntry)
					rebaseDelta := signedPos.Mul(currentEntry.Sub(*bot.lastEntryMark))
					if rebaseDelta.IsNegative() {
						bot.rebasePool = bot.rebasePool.Add(rebaseDelta)
						// v2.0.119 decay anchor: the discard belongs to the
						// inventory alive at absorb time. As the grid sells
						// that inventory down, the exchange realizes the
						// discarded loss into its books — an unscaled pool
						// would double-count it (the v2.0.113 review's P1).
						bot.rebasePos = signedPos
						worker.logger.Info("absorbed positionOpenPrice rebase into supervision pool",
							"component", "autogrid_worker",
							"bot_number", bot.botNumber, "symbol", bot.symbol,
							"old_entry", bot.lastEntryMark.String(), "new_entry", currentEntry.String(),
							"rebase_delta", rebaseDelta.StringFixed(4), "rebase_pool", bot.rebasePool.StringFixed(4),
							"rebase_pos", signedPos.String())
					}
				}
			}
			bot.lastAdjustmentsCount = bot.adjustments

			// Point Д.1: payloadEntryMark and payloadSignedPos are tracked EVERY pass
			bot.lastEntryMark = &currentEntry
			bot.lastSignedPos = &signedPos

			// Legacy v2.0.107 shift offset for rows that predate rebasePool (e.g. AAVE #1288)
			legacyShiftOffset := decimal.Zero
			if !bot.shiftFloatingOffset.IsZero() && !bot.shiftPosition.IsZero() {
				ratio := signedPos.Div(bot.shiftPosition)
				if ratio.IsPositive() {
					if ratio.GreaterThan(decimal.NewFromInt(1)) {
						ratio = decimal.NewFromInt(1)
					}
					legacyShiftOffset = bot.shiftFloatingOffset.Mul(ratio)
				}
			}

			// v2.0.119: the rebasePool decays with the anchored inventory —
			// pos/anchor clamped to [0,1] — exactly like the legacy shift
			// offset, so realized-away loss is never double-counted while
			// the grid heals. Growth beyond the anchor keeps the full pool
			// (clamp at 1): the added inventory carried its own honest entry.
			poolEff := bot.rebasePool
			if !bot.rebasePool.IsZero() && !bot.rebasePos.IsZero() {
				poolRatio := signedPos.Div(bot.rebasePos)
				if poolRatio.IsPositive() && poolRatio.LessThan(decimal.NewFromInt(1)) {
					poolEff = bot.rebasePool.Mul(poolRatio)
				}
			}

			// Point Б: Conservative floor = min(raw, raw + decayed rebasePool + legacyShiftOffset)
			floorCandidate := unrealized.Add(poolEff).Add(legacyShiftOffset)
			if floorCandidate.LessThan(unrealized) {
				supervisionFloor = floorCandidate
			} else {
				supervisionFloor = unrealized
			}
			// v2.0.127: Display circuit uses supervisionFloor so that the UI column
			// "ВСЕГО" (realized + unrealized) strictly matches Pionex Web's Profit.
			unrealized = supervisionFloor
		} else {
			supervisionFloor = decimal.Zero
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
			// v2.0.89 terminal final: the exchange's netted total
			// (profitExited/total-alias) is accepted for every close class
			// except the guarded positive-on-loss anomaly; anything else
			// settles at the telemetry-net-close estimate — last telemetry
			// total minus taker+slippage close cost, floored at the manage
			// stop level for STOP_LOSS closes. The gate consults BOTH reason
			// sources: the stored closed_reason is the only witness of our own
			// manage stop (the exchange answers "user cancel" for every stop we
			// submit natively — prod FARTCOIN +2.349 on ANTI_HUNT).
			settled, source := remote.BUOrderData.SettledProfit()
			decision := worker.settleTerminalFinal(ctx, bot.id, time.Now(),
				bot.maxLoss, bot.closedReason, closedReason, settled, source)
			// v2.0.99: estimate-class finals stay re-checkable until the
			// exchange's finished record confirms the netted total.
			terminalRecon := pendingOrConfirmedRecon(decision.marker)
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET status = $2,
				    closed_reason = COALESCE(NULLIF(closed_reason, ''), $3),
				    reconciliation_state = $8,
				    last_remote_status = $4, realized_pnl_usdt = $5,
				    unrealized_pnl_usdt = 0, supervision_floor_pnl_usdt = 0, closed_at = NOW(),
				    fees_paid_usdt = fees_paid_usdt + $7::NUMERIC,
				    model_state = jsonb_set(
				        CASE WHEN $7::NUMERIC = 0 THEN COALESCE(model_state, '{}'::jsonb)
				             ELSE jsonb_set(COALESCE(model_state, '{}'::jsonb), '{closeCostUsdt}', to_jsonb($7::NUMERIC)) END,
				        '{finalProfitSource}', to_jsonb($6::TEXT)),
				    last_reconciled_at = NOW(), last_error = NULL, updated_at = NOW()
				WHERE id = $1
			`, bot.id, status, closedReason, remoteStatus, decision.final,
				decision.marker, decision.closeCost, terminalRecon); err != nil {
				worker.logger.Error("persist terminal grid state",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			}
			worker.logger.Info(
				"Pionex grid reached terminal state",
				"component", "autogrid_worker", "symbol", bot.symbol,
				"status", status, "reason", closedReason, "realized_pnl", decision.final, "profit_source", decision.marker,
			)
			continue
		}

		// Persist remote truth and PnL without reverting durable stop intents:
		// the local status is kept and in-flight cancel states are preserved.
		// v2.0.78: the status guard keeps stop intents recorded by a concurrent
		// command goroutine (worker.stop / CloseAll / grid.stop) — writing back
		// the stale RUNNING snapshot reverts the intent and the bot never stops.
		persistedReconciliation := "REMOTE_TERMINAL_PENDING"
		if bot.localStatus == "RUNNING" {
			persistedReconciliation = "REST_AUTHORITATIVE_OK"
		}
		var entryMarkParam *decimal.Decimal
		if bot.lastEntryMark != nil && bot.lastEntryMark.IsPositive() {
			entryMarkParam = bot.lastEntryMark
		}
		var rebasePoolParam *decimal.Decimal
		if !bot.rebasePool.IsZero() {
			rebasePoolParam = &bot.rebasePool
		}
		var rebasePosParam *decimal.Decimal
		if !bot.rebasePos.IsZero() {
			rebasePosParam = &bot.rebasePos
		}
		var signedPosParam *decimal.Decimal
		if bot.lastSignedPos != nil && !bot.lastSignedPos.IsZero() {
			signedPosParam = bot.lastSignedPos
		}
		// v2.0.155: the floor-basis peak ratchets BEFORE the persist ships it.
		// Since v2.0.119 the persist wrote the SCANNED value while the
		// per-pass max lived only in the loop copy — peakFloorUsdt in the DB
		// never grew, and TRAILING_TAKE_PROFIT/BREAKEVEN_LOCK compared the
		// current total against a stale peak, keeping both exits dead
		// cross-pass for every REAL bot.
		peakFloorNow := realized.Add(supervisionFloor)
		if bot.peakFloor != nil && bot.peakFloor.GreaterThan(peakFloorNow) {
			peakFloorNow = *bot.peakFloor
		}
		bot.peakFloor = &peakFloorNow
		var peakFloorParam *decimal.Decimal
		if peakFloorNow.IsPositive() {
			peakFloorParam = &peakFloorNow
		}
		var shiftRealizedBaseParam *decimal.Decimal
		if !bot.shiftRealizedBase.IsZero() {
			shiftRealizedBaseParam = &bot.shiftRealizedBase
		}
		var lastRemoteGridProfitParam *decimal.Decimal
		if !bot.lastRemoteGridProfit.IsZero() {
			lastRemoteGridProfitParam = &bot.lastRemoteGridProfit
		}
		var lastAdjustmentsCountParam *int
		if bot.lastAdjustmentsCount > 0 {
			lastAdjustmentsCountParam = &bot.lastAdjustmentsCount
		}
		var ofiRegimeParam *string
		var ofiScoreParam *decimal.Decimal
		var microBiasParam *decimal.Decimal
		if worker.ofiEngine != nil {
			analysis := worker.ofiEngine.Analyze(bot.symbol)
			if analysis.Regime != "" {
				regimeStr := string(analysis.Regime)
				ofiRegimeParam = &regimeStr
			}
			if analysis.CurrentOFI != 0 {
				d := decimal.NewFromFloat(analysis.CurrentOFI).Round(2)
				ofiScoreParam = &d
			}
			if analysis.MicroPriceBiasBps != 0 {
				d := decimal.NewFromFloat(analysis.MicroPriceBiasBps).Round(2)
				microBiasParam = &d
			}
		}
		clearKeys := isZeroPos || isFlip

		if _, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = CASE
					WHEN status IN ('STOP_REQUESTED', 'STOPPING') THEN status
					ELSE $2
				END,
			    reconciliation_state = CASE
					WHEN reconciliation_state IN `+cancelStates+` THEN reconciliation_state
					ELSE $3
				END,
			    last_remote_status = $4, realized_pnl_usdt = $5,
			    unrealized_pnl_usdt = $6,
			    supervision_floor_pnl_usdt = $7,
			    peak_pnl_usdt = GREATEST(COALESCE(peak_pnl_usdt, 0), $5::NUMERIC + $6::NUMERIC),
			    trough_pnl_usdt = LEAST(COALESCE(trough_pnl_usdt, 0), $5::NUMERIC + $6::NUMERIC),
			    model_state = CASE
					WHEN $8::BOOLEAN THEN
						COALESCE(model_state, '{}'::jsonb) - 'shiftFloatingOffset' - 'shiftPosition' - 'rebasePool' - 'rebasePos' - 'payloadEntryMark' - 'payloadSignedPos'
					ELSE
						(COALESCE(model_state, '{}'::jsonb) - 'rebasePool' - 'rebasePos' - 'payloadEntryMark' - 'payloadSignedPos' - 'peakFloorUsdt' - 'shiftRealizedBase' - 'lastRemoteGridProfit' - 'lastAdjustmentsCount' - 'ofiRegime' - 'ofiScore' - 'microPriceBiasBps' - 'priceFeedBlindFloating')
						|| jsonb_strip_nulls(jsonb_build_object(
							'payloadEntryMark', $9::NUMERIC,
							'rebasePool', $10::NUMERIC,
							'payloadSignedPos', $11::NUMERIC,
							'rebasePos', $12::NUMERIC,
							'peakFloorUsdt', $13::NUMERIC,
							'shiftRealizedBase', $14::NUMERIC,
							'lastRemoteGridProfit', $15::NUMERIC,
							'lastAdjustmentsCount', $16::INT,
							'ofiRegime', $17::TEXT,
							'ofiScore', $18::NUMERIC,
							'microPriceBiasBps', $19::NUMERIC
						))
				END,
			    last_reconciled_at = NOW(),
			    last_error = NULL, updated_at = NOW()
			WHERE id = $1
		`, bot.id, bot.localStatus, persistedReconciliation, remoteStatus, realized, unrealized, supervisionFloor, clearKeys, entryMarkParam, rebasePoolParam, signedPosParam, rebasePosParam, peakFloorParam, shiftRealizedBaseParam, lastRemoteGridProfitParam, lastAdjustmentsCountParam, ofiRegimeParam, ofiScoreParam, microBiasParam); err != nil {
			// The PnL persist must never fail silently: v2.0.45 lost every
			// REAL mark for weeks exactly because this error was swallowed.
			worker.logger.Error("persist remote grid truth and PnL",
				"component", "autogrid_worker", "bot_id", bot.id, "error", err)
		}
		if blindFloatingFromExchange {
			// v2.0.149 (exit audit F1): post-mortem marker — this pass's
			// unrealized came from the exchange total while the local price
			// was dead; the healthy strip above clears it on recovery.
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object('priceFeedBlindFloating', true),
				    updated_at = NOW()
				WHERE id = $1
			`, bot.id); err != nil {
				worker.logger.Warn("persist priceFeedBlindFloating marker",
					"component", "autogrid_worker", "bot_id", bot.id, "error", err)
			}
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
			// v2.0.149 (exit audit F1): a dead price disarms only the
			// price-dependent exits. The PnL exits — max-loss, take-profit,
			// trailing — keep firing on the floor basis (unrealized from the
			// exchange total where it honestly signals a loss): the
			// 2026-09-25..27 overshoot class (SUI/DOT/ORDI/NEAR, 4/4 stops
			// past the cap) grew exactly from this branch being a silent
			// continue.
			worker.notePriceFeedBotBlind(bot.symbol, bot.botNumber)
			worker.blindStopsEvaluate(ctx, client, settings, bot, realized, supervisionFloor)
			continue
		}
		// v2.0.89 part B — OU half-life age rotation (REAL arm, paper-parity):
		// a bot older than clamp(2×HL, 4h, 48h) has outlived its fitted
		// range. Close by market (STOP_REQUESTED + native cancel, the exact
		// decision-close machinery below); the slot returns to the scanner,
		// never to a DGT re-center. Placed before the break matrix so a stale
		// range cannot hide behind a HOLD.
		{
			botAge := time.Since(bot.createdAt)
			ageVerdict := gridAgeVerdictFor(worker.ouReadingForSymbol(ctx, bot.symbol, *settings), botAge)
			if ageVerdict.rotate && worker.stormActive() {
				// v2.0.111 storm deferral: rotating INTO a fleet-wide
				// acceleration crystallizes the bottom tick (ICP #1381
				// −$4.34). The verdict re-arms next pass; the deferral is
				// bounded by the storm window itself.
				worker.logger.Info("half-life rotation deferred by storm mode",
					"component", "autogrid_worker", "bot_number", bot.botNumber,
					"symbol", bot.symbol, "age_hours", botAge.Hours(),
					"max_age_hours", ageVerdict.maxAgeHours)
			} else if ageVerdict.rotate {
				totalPnL := realized.Add(unrealized)
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET status = 'STOP_REQUESTED', closed_reason = $2,
					    model_state = jsonb_set(jsonb_set(COALESCE(model_state, '{}'::jsonb),
					        '{halfLifeHours}', to_jsonb($3::FLOAT8)),
					        '{maxAgeHours}', to_jsonb($4::FLOAT8)),
					    updated_at = NOW()
					WHERE id = $1 AND status = 'RUNNING'
				`, bot.id, gridAgedHalfLifeReason, ageVerdict.halfLifeHours, ageVerdict.maxAgeHours)
				if err := worker.cancelRealBot(ctx, client, bot.id, bot.remoteID, "autogrid "+gridAgedHalfLifeReason); err != nil {
					worker.logger.Error("close REAL bot by half-life rotation",
						"component", "autogrid_worker", "bot_id", bot.id, "error", err)
				} else {
					worker.logger.Info("REAL bot rotated by OU half-life",
						"component", "autogrid_worker", "symbol", bot.symbol,
						"age_hours", botAge.Hours(), "max_age_hours", ageVerdict.maxAgeHours)
					pnlPct := decimal.Zero
					if !bot.investment.IsZero() {
						pnlPct = totalPnL.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
					}
					_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol,
						gridAgedHalfLifeReason, &price, &totalPnL, map[string]any{
							"reason": gridAgedHalfLifeReason, "pnlPct": pnlPct,
							"age_hours":       botAge.Hours(),
							"max_age_hours":   ageVerdict.maxAgeHours,
							"half_life_hours": ageVerdict.halfLifeHours,
						})
					queueGridAgedTelegram(ctx, worker, "REAL", bot.botNumber, bot.symbol, ageVerdict, botAge)
				}
				continue
			}
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
		// v2.0.78 CRIT-2 self-heal: invest_in is at-least-once at the exchange
		// but exactly-once locally, and native pour + local persist + target
		// doubling were three separate writes. A crash between them either
		// double-poured real margin on retry or left a doubled margin with
		// half-size stops forever. Exchange truth wins every pass.
		remoteInvestment, remoteInvestmentReported := remote.BUOrderData.Investment()
		trancheActive := false
		trancheBaseParsed := decimal.Zero
		if bot.trancheBase != nil {
			if base, bErr := decimal.NewFromString(*bot.trancheBase); bErr == nil && base.GreaterThan(decimal.Zero) {
				trancheBaseParsed = base
				trancheActive = bot.trancheDeployed >= 1
			}
		}
		if bot.localStatus == "RUNNING" && trancheActive {
			// (a) Resync the local investment column to the exchange-reported
			// figure when they diverge: the previous pour may have landed
			// without its persist (or the persist may have landed twice).
			if remoteInvestmentReported &&
				remoteInvestment.Sub(bot.investment).Abs().GreaterThan(decimal.NewFromInt(1)) {
				localBefore := bot.investment
				if _, err := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET quote_investment = $2::NUMERIC, updated_at = NOW()
					WHERE id = $1
				`, bot.id, remoteInvestment); err != nil {
					worker.logger.Error("tranche resync: persist exchange investment failed",
						"component", "autogrid_worker", "bot_id", bot.id, "error", err)
				} else {
					bot.investment = remoteInvestment
					worker.logger.Warn("tranche resync: local investment diverged from exchange truth — resynced",
						"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
						"local", localBefore.StringFixed(2), "remote", remoteInvestment.StringFixed(2))
					tag, markErr := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{trancheResyncAt}',
							to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
						    updated_at = NOW()
						WHERE id = $1
						  AND COALESCE((model_state->>'trancheResyncAt')::TIMESTAMPTZ, '1970-01-01') < NOW() - INTERVAL '1 hour'
					`, bot.id)
					if markErr == nil && tag.RowsAffected() == 1 {
						_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol,
							"TRANCHE_RESYNC", &price, nil, map[string]any{
								"action":            "investment_resynced",
								"local_investment":  localBefore.String(),
								"remote_investment": remoteInvestment.String(),
							})
					}
				}
			}
			// (b) The exchange already holds the full tranche while the
			// targets/stop are still single-sized (pour landed, doubling
			// persist fell): complete the doubling idempotently — the
			// trancheDeployed=1 guard makes it exactly-once.
			if bot.trancheDeployed == 1 && !bot.investment.LessThan(trancheBaseParsed) {
				tag, dErr := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET pnl_target_usdt = pnl_target_usdt * 2,
					    max_loss_usdt = max_loss_usdt * 2,
					    model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{trancheDeployed}', '2'::jsonb),
					    updated_at = NOW()
					WHERE id = $1
					  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
				`, bot.id)
				if dErr != nil {
					worker.logger.Error("tranche self-heal: target doubling persist failed",
						"component", "autogrid_worker", "bot_id", bot.id, "error", dErr)
				} else if tag.RowsAffected() == 1 {
					bot.trancheDeployed = 2
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
					worker.logger.Warn("tranche self-heal: full tranche found with single-sized targets — doubling completed",
						"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol)
					_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol,
						"TRANCHE_RESYNC", &price, nil, map[string]any{
							"action":             "targets_doubled",
							"investment":         bot.investment.String(),
							"effective_target":   botTarget.StringFixed(2),
							"effective_max_loss": botMaxLoss.StringFixed(2),
						})
				}
			}
		}

		// Self-heal / active repair for neutral bots' exchange take-profit card.
		// v2.0.158: a price-based profitStop triggers instantly on a neutral
		// grid (market_price <= upper_price is immediately true) — replace it
		// with profit_amount.
		// v2.0.160: also shrink an oversized profit_amount card (pre-160
		// dynamic targets: $15–44 on $50–100 slots) down to the harvest
		// ceiling and mirror the capped amount into pnl_target_usdt so the
		// local exit ladder and the exchange card agree. FIXED mode keeps the
		// v2.0.158 behavior only (the operator's explicit amount is law).
		if bot.localStatus == "RUNNING" && client != nil {
			isNeutralBot := strings.EqualFold(bot.direction, "NEUTRAL") || strings.EqualFold(remote.BUOrderData.Trend, "no_trend")
			if isNeutralBot {
				cardType := strings.ToLower(strings.TrimSpace(remote.BUOrderData.ProfitStopType))
				harvestCap := NeutralHarvestTPCap(bot.investment)
				fixedMode := settings.PnLTargetMode == "FIXED"
				needsRepair := false
				repairReason := ""
				switch cardType {
				case "price":
					needsRepair = true
					repairReason = "price_legacy"
				case "profit_amount":
					// Skip when the exchange already holds a larger (post-top-up)
					// commitment than the local column: the cap would be computed
					// from a stale half-investment and the only-lower mirror would
					// then pin it there forever. The resync above persists first;
					// the next pass caps from the true figure.
					staleInvestment := remoteInvestmentReported &&
						remoteInvestment.GreaterThan(bot.investment.Add(decimal.NewFromInt(1)))
					if !fixedMode && !staleInvestment && remote.BUOrderData.ProfitStop.GreaterThan(harvestCap) {
						needsRepair = true
						repairReason = "harvest_cap"
					}
				}
				if needsRepair {
					cardWas := remote.BUOrderData.ProfitStop
					effective := harvestCap
					// FIXED mode keeps the operator's stored amount verbatim
					// (fallback to the cap only when nothing is stored);
					// DYNAMIC keeps a stored amount below the cap untouched.
					if botTarget.GreaterThan(decimal.Zero) && (fixedMode || botTarget.LessThan(harvestCap)) {
						effective = botTarget
					}
					profitValStr := effective.StringFixed(2)
					newTakeProfit := effective

					worker.logger.Warn("repairing neutral bot take-profit on Pionex card",
						"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
						"remote_id", bot.remoteID, "target_amount", profitValStr,
						"reason", repairReason, "card_was", cardWas.StringFixed(2),
						"harvest_cap", harvestCap.StringFixed(2))

					req := pionex.FuturesGridUpdateTriggerProfitLossRequest{
						BUOrderID: bot.remoteID,
						List: []pionex.FuturesGridTriggerProfitLossItem{
							{
								Type:                "stop_profit",
								StopType:            "profit_amount",
								Value:               profitValStr,
								ProfitStopSellModel: "TO_USDT",
							},
						},
					}
					_, uErr := client.UpdateFuturesGridTriggerProfitLoss(ctx, req)
					if uErr != nil {
						worker.logger.Error("failed to repair neutral bot take-profit on Pionex",
							"component", "autogrid_worker", "bot_id", bot.id, "remote_id", bot.remoteID, "error", uErr)
					} else {
						worker.logger.Info("successfully repaired neutral bot take-profit on Pionex card",
							"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
							"remote_id", bot.remoteID, "target_amount", profitValStr, "reason", repairReason)
						remote.BUOrderData.ProfitStopType = "profit_amount"
						remote.BUOrderData.ProfitStop = effective
						_, _ = worker.db.Exec(ctx, `
							UPDATE grid_bots
							SET take_profit = $1, target_price = NULL, updated_at = NOW()
							WHERE id = $2
						`, newTakeProfit, bot.id)
						// Mirror the ceiling into the stored dynamic target so the
						// local exit ladder cannot wait on the pre-160 fantasy
						// amount the card just abandoned. Only ever lowered;
						// FIXED mode keeps the operator's number.
						if !fixedMode {
							_, _ = worker.db.Exec(ctx, `
								UPDATE grid_bots
								SET pnl_target_usdt = $2::NUMERIC, updated_at = NOW()
								WHERE id = $1
								  AND pnl_target_usdt IS NOT NULL
								  AND pnl_target_usdt > $2::NUMERIC
							`, bot.id, effective)
							// Same-tick decision safety (the tranche-2 precedent):
							// decideBotAction below reads the local copies, so
							// refresh them or this tick can wait one pass on the
							// abandoned target.
							bot.pnlTarget = &effective
							botTarget = effective
						}
						_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol,
							"REPAIR_TAKE_PROFIT", &price, nil, map[string]any{
								"action":      "switched_to_profit_amount",
								"target_usdt": profitValStr,
								"reason":      repairReason,
								"card_was":    cardWas.StringFixed(2),
								"harvest_cap": harvestCap.StringFixed(2),
							})
					}
				} else if bot.targetPrice != nil {
					// Clean up DB target_price artifact for neutral bots so UI displays pnlTargetUsdt cleanly
					_, _ = worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET target_price = NULL, updated_at = NOW()
						WHERE id = $1
					`, bot.id)
					bot.targetPrice = nil
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
		// v2.0.119: the trailing/breakeven engine compares like with like —
		// the peak rides the supervisionFloor basis (the raw peak_pnl_usdt
		// column stays untouched for display continuity). v2.0.155: the
		// ratchet now happens before the persist above; this is the same
		// value, kept for the decision input below.
		peakFloorNow = *bot.peakFloor
		closeDecidedAt := time.Now()
		var ofiRegime *string
		var microBias *float64
		if worker.ofiEngine != nil {
			micro := worker.ofiEngine.Analyze(bot.symbol)
			rStr := string(micro.Regime)
			ofiRegime = &rStr
			microBias = &micro.MicroPriceBiasBps
		}
		ouReading := worker.ouReadingForSymbol(ctx, bot.symbol, *settings)
		ouHalfLife := 0.0
		if ouReading.ok && ouReading.halfLifeHours > 0 {
			ouHalfLife = ouReading.halfLifeHours
		}
		// v2.0.155: storm state (OU rotation deferral) and the trailing-SL
		// precision — the grid's own accepted precision backs up a missing
		// deploy-time marker.
		stormNow := worker.stormActive()
		trailPrecision := bot.pricePrecision
		if trailPrecision <= 0 {
			trailPrecision = paperTrailPrecision(bot.lower)
		}

		decision := decideBotAction(botActionInput{
			Direction:         bot.direction,
			Lower:             bot.lower,
			Upper:             bot.upper,
			CurrentPrice:      price,
			RealizedPNL:       realized,
			UnrealizedPNL:     supervisionFloor,
			PeakPNL:           peakFloorNow,
			Budget:            bot.investment,
			PnLTarget:         botTarget,
			MaxLoss:           botMaxLoss,
			RangeBreakBuffer:  settings.RangeBreakBufferPct,
			AdjustmentsLeft:   settings.MaxAdjustmentsPerBot - bot.adjustments,
			Regime:            regime,
			AntiHuntStop:      bot.antiHuntStop,
			TargetPrice:       bot.targetPrice,
			StopLossPrice:     bot.stopLossPrice,
			StopLossHigh:      bot.stopLossHigh,
			TrailingSLPrice:   bot.trailingSLPrice,
			OFIRegime:         ofiRegime,
			MicroPriceBiasBps: microBias,
			AgeHours:          time.Since(bot.createdAt).Hours(),
			OUHalfLifeHours:   ouHalfLife,
			SmartExitEnabled:  settings.SmartExitEnabled,
			OFIHarvestEnabled: settings.OFIHarvestEnabled,
			OURotationEnabled: settings.OURotationEnabled,
			PricePrecision:    trailPrecision,
			StormActive:       &stormNow,
		})
		// v2.0.168 liquidation-estimate backfill: the pre-168 checkParams
		// request-shape bug left EVERY running bot with NULL liq columns —
		// the proximity guard below never armed. For a bot whose stored
		// estimates are all empty, re-ask the (now fixed) checkParams with
		// the bot's own geometry once per hour and persist what comes back;
		// if the estimate lands closer to the stop than the deploy-time
		// doctrine allows, page the operator (supervision continues via the
		// existing ladders — this is instrumentation, not a new close path).
		if bot.localStatus == "RUNNING" && client != nil &&
			(bot.liqPriceUp == nil && bot.liqPriceDown == nil) &&
			remote.BUOrderData.Bottom.GreaterThan(decimal.Zero) && remote.BUOrderData.Top.GreaterThan(remote.BUOrderData.Bottom) {
			backfillDue := true
			if last := bot.modelStateMap["liqEstimateBackfillAt"]; last != nil {
				if ts, pErr := time.Parse(time.RFC3339, fmt.Sprintf("%v", last)); pErr == nil && time.Since(ts) < time.Hour {
					backfillDue = false
				}
			}
			if backfillDue {
				checkBase, _, splitErr := SplitPionexPerp(bot.symbol)
				if splitErr != nil {
					checkBase = ""
				}
				if checkBase != "" && !strings.HasSuffix(checkBase, ".PERP") {
					checkBase = checkBase + ".PERP"
				}
				var checkRes *pionex.FuturesGridCheckParamsResult
				var cErr error
				if checkBase != "" {
					checkRes, cErr = client.CheckFuturesGridParams(ctx, pionex.NativeFuturesGridCreateParams{
						Base:  checkBase,
						Quote: "USDT",
						BUOrderData: pionex.BUOrderData{
							Top: remote.BUOrderData.Top, Bottom: remote.BUOrderData.Bottom,
							Row: remote.BUOrderData.Row, GridType: remote.BUOrderData.GridType,
							Trend: remote.BUOrderData.Trend, Leverage: bot.leverage,
							QuoteInvestment: bot.investment,
						},
					})
				}
				nowStamp := time.Now().UTC().Format(time.RFC3339)
				if cErr == nil && checkRes != nil {
					_, _ = worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET liq_price_up = COALESCE($2::NUMERIC, liq_price_up),
						    liq_price_down = COALESCE($3::NUMERIC, liq_price_down),
						    model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object('liqEstimateBackfillAt', $4::TEXT),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id, zeroToNil(checkRes.EstimateLiquidationUp), zeroToNil(checkRes.EstimateLiquidationDown), nowStamp)
					guardSL := derefZero(bot.stopLossPrice)
					if reason := liquidationGuardReason(remote.BUOrderData.Trend, price,
						checkRes.EstimateLiquidationDown, checkRes.EstimateLiquidationUp,
						guardSL, bot.stopLossHigh, bot.leverage); reason != "" {
						worker.logger.Warn("liq backfill: running bot violates the liquidation guard doctrine",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", reason)
						_ = QueueTelegramEvent(ctx, worker.db, "LIQ_GUARD_BACKFILL", map[string]any{
							"message": "⚠️ Бот #" + fmt.Sprintf("%d", bot.botNumber) + " (" + bot.symbol + "): " + reason +
								" — бот работает со старой геометрией, проверьте вручную",
						})
					}
				} else {
					_, _ = worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object('liqEstimateBackfillAt', $2::TEXT),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id, nowStamp)
				}
			}
		}
		// v2.0.161 liquidation proximity guard: the running liquidation price
		// is the hard wall behind every stop — when price is within
		// liqProximityClosePct of it, no stop ladder can be trusted to
		// execute first. Persist the live reading and close through the
		// standard stop path (arms cooldown deliberately). Only preempts a
		// HOLD: an already-firing TP/stop decision keeps priority.
		if runningLiq := remote.BUOrderData.LiquidationPrice; runningLiq.GreaterThan(decimal.Zero) {
			_, _ = worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET liq_price = $2::NUMERIC, updated_at = NOW()
				WHERE id = $1 AND (liq_price IS NULL OR liq_price <> $2::NUMERIC)
			`, bot.id, runningLiq)
			if decision.Action == ActionHold && liquidationProximityBreached(price, runningLiq) {
				decision = manageDecision{Action: ActionCloseStopLoss, Reason: "LIQ_PROXIMITY"}
				worker.logger.Warn("liquidation proximity guard: closing bot",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"price", price.StringFixed(6), "liq_price", runningLiq.StringFixed(6))
			}
		}
		// v2.0.93 FIX-E (paper-canonical order): the tranche-2 pour runs AFTER
		// the stop decision, not before it. The old order poured on the very
		// tick that then closed or shifted the bot — real margin into a stop
		// (wasted pour, doubled stop charged at exit) or into a range shift.
		// The paper arm always had the decision-first order (its close/adjust
		// branches `continue` past the tranche block); that order is canonical:
		// on a conflicting tick the stop wins and the pour waits for a calm
		// (HOLD) tick. The resync/self-heal reconciliation above stays BEFORE
		// the decision — a doubled position must be judged against doubled
		// stops, which is exactly what the refreshed locals provide.
		if decision.Action == ActionHold && !trancheBackoff && bot.trancheDeployed == 1 && bot.trancheBase != nil && bot.localStatus == "RUNNING" && price.IsPositive() {
			// v2.0.111 stress moratorium (B): no second tranche while the
			// bot's radar band is ≥2 or a fleet storm is active. v2.0.138
			// (audit): the block now leaves a durable trace — one journal row
			// per hour max, fenced by the same tranche2SkipAt backoff marker
			// the risk-gate skip uses, so a stressed bot cannot flood the
			// journal at manage cadence.
			if !worker.trancheStressAllowed(ctx, bot.id) {
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
					worker.journalEntryDecision(ctx, EntryChainInput{
						Path: EntryPathTranche2, Settings: *settings, Symbol: bot.symbol,
						Direction: bot.direction, Fleet: "REAL", RefID: bot.id,
					}, entryOutcomeReject, "TRANCHE_STRESS",
						"мораторий стресса: шторм-режим или радар-полоса ≥2 — вторая доля не вливается в спасаемую позицию", nil)
				}
			} else if base, bErr := decimal.NewFromString(*bot.trancheBase); bErr == nil && base.GreaterThan(bot.investment) {
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
					investmentBefore := bot.investment
					pending := base.Sub(bot.investment)
					// v2.0.78 CRIT-2(c): the pour is fenced by durable intent
					// and remote truth. A marker written before the native call
					// plus the exchange-reported investment make a retry decide
					// "already landed" instead of pouring a second margin.
					pour := true
					if remoteInvestmentReported && !remoteInvestment.LessThan(base.Sub(decimal.NewFromInt(1))) {
						pour = false
						worker.logger.Warn("tranche 2 pour skipped: exchange already holds the full tranche — resyncing instead of re-pouring",
							"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
							"remote_investment", remoteInvestment.StringFixed(2))
					} else if !remoteInvestmentReported && trancheMarkerFresh(bot.trancheIntentAt, 24*time.Hour) {
						// No remote truth to consult while a previous attempt's
						// outcome is unconfirmed: fail closed — starving one
						// top-up window is cheaper than a second real pour.
						pour = false
						worker.logger.Warn("tranche 2 pour deferred: fresh invest intent with no exchange investment to verify against",
							"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol)
					}
					if pour {
						intentTag, intentErr := worker.db.Exec(ctx, `
						UPDATE grid_bots
						SET model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{trancheIntentAt}',
							to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id)
						if intentErr != nil || intentTag.RowsAffected() != 1 {
							// Without the durable intent the pour cannot be
							// verified on retry — refuse to move real margin.
							worker.logger.Error("tranche 2 intent-marker write failed — pour refused",
								"component", "autogrid_worker", "bot_id", bot.id, "error", intentErr)
							pour = false
						}
					}
					if pour {
						if _, err := worker.service.AdjustBot(ctx, worker.accounts, settings.ID, bot.id, AdjustBotInput{
							Mode:            "invest_in",
							QuoteInvestment: pending.Round(2),
						}); err != nil {
							worker.logger.Error("tranche 2 invest_in failed",
								"component", "autogrid_worker", "bot_id", bot.id, "error", err)
							if (errors.Is(err, ErrNativeAdjustRefused) && !pionex.IsOutcomeUnknown(err)) ||
								tranchePourGateRefused(err) {
								// The exchange itself refused the call, OR a durable
								// gate refused it before any native call left the
								// building (v2.0.142 audit P2d: breaker/margin-reserve/
								// risk-engine refusals are not ErrNativeAdjustRefused,
								// so the pour intent used to stay armed and fence the
								// next attempt for 24h instead of the intended 1h
								// backoff): the pour provably never landed — clear the
								// intent so the backoff retry is not fenced by a
								// stale marker.
								_, _ = worker.db.Exec(ctx, `
								UPDATE grid_bots
								SET model_state = model_state - 'trancheIntentAt', updated_at = NOW()
								WHERE id = $1
							`, bot.id)
							}
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
							// v2.0.78: the trancheDeployed=1 guard makes a retry of
							// this statement exactly-once (CRIT-2 self-heal branch b
							// uses the same guard).
							if _, err := worker.db.Exec(ctx, `
							UPDATE grid_bots
							SET pnl_target_usdt = pnl_target_usdt * 2,
							    max_loss_usdt = max_loss_usdt * 2,
							    model_state = jsonb_set(model_state, '{trancheDeployed}', '2'::jsonb),
							    updated_at = NOW()
							WHERE id = $1
							  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
						`, bot.id); err != nil {
								worker.logger.Error("tranche 2 target doubling failed (invest_in already committed — self-heal will complete it)",
									"component", "autogrid_worker", "bot_id", bot.id, "error", err)
							} else {
								// Same-tick decision safety: decideBotAction below
								// reads the LOCAL copies taken before the tranche
								// block, not the struct fields — refresh BOTH, or
								// the top-up tick can fire a half-size TP/SL against
								// the doubled position.
								bot.investment = base
								bot.trancheDeployed = 2
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
							// v2.0.142 (audit P3-3): journal the successful pour
							// as the TRANCHE2 path's ALLOW row — the journal's
							// "every path" contract needs the landing side, not
							// only the refusals (the worker owns the tranche-2
							// success point; the MANUAL invest_in success point
							// lives in service.go's AdjustBot return, outside
							// this file).
							worker.journalEntryDecision(ctx, EntryChainInput{
								Path: EntryPathTranche2, Settings: *settings, Symbol: bot.symbol,
								Direction: bot.direction, Fleet: "REAL", RefID: bot.id,
							}, entryOutcomeAllow, "POUR",
								"tranche 2 poured: "+topUp,
								map[string]any{
									"poured_usdt":       pending.StringFixed(2),
									"investment_before": investmentBefore.String(),
									"investment_after":  base.String(),
								})
							// v2.0.89 round-trip fee ledger: the pour's taker
							// entry fee is booked INSIDE AdjustBot's invest_in
							// persist (the single chokepoint every REAL pour —
							// tranche-2 and manual top-ups alike — passes
							// through); booking here too would double-count it.
							// v2.0.75 parity: the REAL top-up used to be telegram-only
							// — the ledger never saw the margin doubling (paper logs
							// TRANCHE_2 since v2.0.13). Same payload shape, REAL source.
							// botTarget/botMaxLoss were refreshed to the doubled
							// figures by the block above — those ARE the effective
							// post-top-up target/stop.
							effTarget, effStop := decimal.Zero, decimal.Zero
							if bot.pnlTarget != nil {
								effTarget = *bot.pnlTarget
							}
							if bot.maxLoss != nil {
								effStop = *bot.maxLoss
							}
							_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, "TRANCHE_2", &price, nil, map[string]any{
								"reason":            topUp,
								"investment_before": investmentBefore.String(), "investment_after": base.String(),
								"effective_target": effTarget.StringFixed(2), "effective_max_loss": effStop.StringFixed(2),
							})
							// Vars cover every placeholder of template_range_adjust
							// (bot_number/symbol/lower_price/upper_price/
							// adjustments_count) so the operator message renders the
							// real template instead of the compose-fallback line.
							_ = QueueTelegramEvent(ctx, worker.db, "TRANCHE_2", map[string]any{
								"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": topUp,
								"effective_max_loss": effStop.StringFixed(2),
								"lower_price":        bot.lower.StringFixed(6), "upper_price": bot.upper.StringFixed(6),
								"adjustments_count": bot.adjustments + 1,
							})
						}
					}
				}
			}
		}

		if decision.Action == ActionHold || decision.Action == ActionAdjustUp || decision.Action == ActionAdjustDown {
			if bot.wickShieldTriggeredAt != nil {
				worker.logger.Info("wick shield saved bot from close - price recovered",
					"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id, "price", price.String())
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
					    || jsonb_build_object('wickShieldLastClearedAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
					    updated_at = NOW()
					WHERE id = $1
				`, bot.id)
				_ = QueueTelegramEvent(ctx, worker.db, "WICK_SHIELD_SAVED", map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol, "price": price.StringFixed(6),
				})
			}
		}

		// v2.0.119: ActionCloseStopLoss is OUT of the wick shield's reach.
		// NEAR #1401 (2026-09-27): a breached dollar cap is not a wick
		// question — the shield deferred the max-loss stop twice (05:46:54
		// and 05:49:47, both past the $8 cap, judged by a LOWER wick while
		// the short inventory died on the upper move) and the bot settled
		// −$14.90. The shield may still defer structural/range-break closes
		// when explicitly enabled, with the wick side taken from the SIGNED
		// position and one grace per signal episode.
		wickShieldClear := func() {
			if bot.wickShieldTriggeredAt != nil {
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
					    || jsonb_build_object('wickShieldLastClearedAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
					    updated_at = NOW()
					WHERE id = $1
				`, bot.id)
			}
		}
		switch decision.Action {
		case ActionCloseStructInvalid, ActionCloseRangeBreak:
			if settings.WickShieldEnabled {
				shouldHold, newTriggeredAt, newExtreme, shieldReason := worker.evaluateWickShield(
					ctx, bot.symbol, signedPos, price, bot.wickShieldTriggeredAt, bot.wickShieldExtreme, bot.wickShieldLastClearedAt, settings.WickGraceSec,
				)
				if shouldHold {
					if bot.wickShieldTriggeredAt == nil && newTriggeredAt != nil && newExtreme != nil {
						_, _ = worker.db.Exec(ctx, `
							UPDATE grid_bots
							SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
							    || jsonb_build_object('wickShieldTriggeredAt', $2::TEXT, 'wickShieldExtreme', $3::NUMERIC),
							    updated_at = NOW()
							WHERE id = $1
						`, bot.id, *newTriggeredAt, newExtreme.String())
						worker.logger.Info("wick shield armed: deferring structural close",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", decision.Reason, "shield_reason", shieldReason)
						_ = QueueTelegramEvent(ctx, worker.db, "WICK_SHIELD_ARMED", map[string]any{
							"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": decision.Reason,
							"wick_extreme": newExtreme.StringFixed(6), "grace_sec": settings.WickGraceSec,
						})
					} else {
						worker.logger.Debug("wick shield holding: close deferred",
							"component", "autogrid_worker", "symbol", bot.symbol, "shield_reason", shieldReason)
					}
					continue
				}
				wickShieldClear()
			}
			fallthrough
		case ActionCloseStopLoss, ActionCloseTakeProfit, ActionCloseSmartHarvest, ActionCloseOURotation:
			// v2.0.120 (review agent): the stop DECISION runs on the floor
			// basis (UnrealizedPNL = supervisionFloor), so the intent marker
			// must carry the same basis — a raw-basis marker on a pooled bot
			// systematically books the known discard into "execution
			// slippage" and poisons the EXIT_SLIPPAGE calibration.
			totalPnL := realized.Add(supervisionFloor)
			intentAt := time.Now()
			active, intentErr := worker.recordCloseIntent(ctx, bot.id, decision.Reason, totalPnL)
			if intentErr != nil {
				worker.logger.Error("close intent persist failed", "bot_id", bot.id, "error", intentErr)
			} else if !active {
				// Another writer already made the bot terminal. Do not turn
				// it back into STOPPING via cancelRealBot.
				continue
			}
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

					// v2.0.171 fast-follow re-deploy (consensus-2.0): a TP
					// close in a bot whose symbol still trends is the BEST
					// moment to re-enter — the strategy just proved itself on
					// this pair. Queue an out-of-turn scan (cascade-style)
					// so the scanner re-evaluates within ~4 min instead of
					// waiting for the next scheduled slot.
					if totalPnL.IsPositive() {
						worker.queueFastFollowScan(ctx, *settings, bot.symbol, bot.botNumber)
					}
				} else if decision.Action == ActionCloseSmartHarvest {
					eventType = "SMART_HARVEST"
				} else if decision.Action == ActionCloseOURotation {
					eventType = "OU_ROTATION"
				}
				pnlPct := decimal.Zero
				if !bot.investment.IsZero() {
					pnlPct = totalPnL.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
				}
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, eventType, &price, &totalPnL, map[string]any{
					"reason": decision.Reason, "pnlPct": pnlPct,
					// v2.0.119 SLA telemetry stage 1: detect→intent latency.
					// decideBotAction ran this same pass; the delta is the
					// intent-persist cost the risk engine controls.
					"intent_latency_ms": time.Since(closeDecidedAt).Milliseconds(),
					"intent_to_send_ms": time.Since(intentAt).Milliseconds(),
					"stop_intent_total": totalPnL.StringFixed(4),
				})
				_ = QueueTelegramEvent(ctx, worker.db, eventType, map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol,
					"pnl_usdt": totalPnL.StringFixed(4), "pnl_pct": pnlPct.StringFixed(2),
					"reason": decision.Reason,
				})
				// v2.0.89 part B — DGT break re-deploy (REAL arm, paper-parity):
				// the native cancel was accepted; a RANGE_BREAK_* close QUEUES
				// the re-deploy intent on the row. It executes from
				// processDgtRealRedeployIntents the moment the parent settles
				// terminal — centering the fresh grid on the break price with
				// the same slot capital, through the same native lifecycle a
				// deploy uses. When blocked or disabled the old scanner-owned
				// behavior is exactly what remains.
				if settings.DgtRedeployEnabled && dgtBreakRedeployReason(decision.Reason) {
					worker.dgtQueueRealRedeploy(ctx, *settings, dgtRedeploySpec{
						symbol:       bot.symbol,
						direction:    bot.direction,
						breakPrice:   price,
						slotBudget:   slotCapital(bot.trancheBase, bot.investment),
						oldBotID:     bot.id,
						oldBotNumber: bot.botNumber,
						atrFallback:  bot.atrEntry,
						accountID:    bot.accountID,
					})
				}
			}
		case ActionAdjustUp, ActionAdjustDown:
			if !price.GreaterThan(decimal.Zero) {
				worker.logger.Error("adjust native grid range skipped: no live price",
					"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol)
				break
			}
			// v2.0.128 OFI Protection for Running Bots:
			// Freeze range shift if order book and taker flow exhibit persistent toxic pressure or confirmed breakout
			// v2.0.139 — PRESSURE-tier widening: adjust-down freezes under dump
			// flow (DUMP_PRESSURE or CONFIRMED_DUMP), adjust-up under pump flow
			// — "don't chase the flow" (audit C gap). Deliberately one-sided:
			// a pump must NOT freeze an adjust-down (that would strand the grid
			// under a rocket) — the freeze keys on the flow it chases only.
			if worker.ofiEngine != nil {
				micro := worker.ofiEngine.Analyze(bot.symbol)
				if decision.Action == ActionAdjustDown {
					if micro.Regime == marketdata.RegimeDumpPressure || micro.Regime == marketdata.RegimeConfirmedDump {
						worker.logger.Warn("adjust down frozen by OFI microstructure veto (falling knife protection)",
							"component", "autogrid_worker", "bot_number", bot.botNumber, "symbol", bot.symbol,
							"regime", string(micro.Regime), "reason", micro.Reason,
							"ofi", micro.CurrentOFI, "micro_bias_bps", micro.MicroPriceBiasBps)
						// v2.0.138 journal: the freeze's feature vector, so the
						// veto is auditable/replayable (engine state is memory-only).
						logOFIDecision(ctx, worker.db, settings.ID, bot.symbol, ofiKindAdjustFreeze,
							micro, string(micro.Readiness()), "FREEZE", micro.Reason,
							fmt.Sprintf("#%d", bot.botNumber))
						break
					}
				} else if decision.Action == ActionAdjustUp {
					if micro.Regime == marketdata.RegimePumpPressure || micro.Regime == marketdata.RegimeConfirmedPump {
						worker.logger.Warn("adjust up frozen by OFI microstructure veto (rocket runaway protection)",
							"component", "autogrid_worker", "bot_number", bot.botNumber, "symbol", bot.symbol,
							"regime", string(micro.Regime), "reason", micro.Reason,
							"ofi", micro.CurrentOFI, "micro_bias_bps", micro.MicroPriceBiasBps)
						logOFIDecision(ctx, worker.db, settings.ID, bot.symbol, ofiKindAdjustFreeze,
							micro, string(micro.Readiness()), "FREEZE", micro.Reason,
							fmt.Sprintf("#%d", bot.botNumber))
						break
					}
				}
			}
			// v2.0.85 "shift always" (same mode preflight as the radar): the
			// live remote FLOATING PnL selects HOW the break shift ships —
			// green → normal re-base; under water → keepInvestment rescue
			// transfer (documented flag: pure range move, the exchange skips
			// its PROFIT_LESS_THAN_ZERO gate but still validates the price
			// range), dry-run adjustParamsCheck FIRST so a refused check never
			// costs the live call. The break keeps its existing owners
			// regardless — RANGE_BREAK_* closes on adverse regime,
			// _NO_ADJUSTMENTS_LEFT on budget exhaustion, plus the anti-hunt
			// structural stop and the max-loss stop keep running.
			shiftMode := adjustShiftMode(supervisionFloor)
			adjustReq := pionex.AdjustFuturesGridParams{
				BUOrderID: bot.remoteID, Type: "adjust_params",
				ExtraMargin: false, OpenPrice: &price,
				Bottom: &decision.NewLower, Top: &decision.NewUpper, Row: bot.rowNum,
			}
			if shiftMode == shiftModeKeepInvestment {
				keepFlag := true
				adjustReq.KeepInvestment = &keepFlag
				if _, checkErr := client.CheckAdjustFuturesGridBot(ctx, adjustReq); checkErr != nil {
					// The exchange already refused this exact payload: the
					// live call is a guaranteed rejection, nothing is sent
					// and the reason is durable in the logs (the break's
					// exit ladder keeps owning the bot).
					worker.logger.Error("manage range shift refused by adjustParamsCheck — live call skipped",
						"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
						"mode", shiftMode, "reason", decision.Reason, "error", checkErr)
					break
				}
			}
			if err := client.AdjustFuturesGridBot(ctx, adjustReq); err != nil {
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
				// v2.0.78: the range persist is CHECKED — a swallowed failure
				// here would silently desync the local bounds and re-arm the
				// same shift on the next pass. The ADJUST_RANGE event below
				// still fires on exchange success regardless: it is what arms
				// the durable radar cooldown, and the exchange has already
				// moved the range.
				//
				// v2.0.113: Range shifts no longer write shiftFloatingOffset to model_state.
				// Any positionOpenPrice re-basing by Pionex is detected dynamically across passes
				// by the reconcile loop and absorbed into rebasePool for the supervision circuit,
				// preserving raw 1:1 screen parity on the display circuit.
				if tag, persistErr := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET lower_price = $2, upper_price = $3,
					    adjustments_count = adjustments_count + 1`+setStop+`, updated_at = NOW()
					WHERE id = $1
				`, append([]any{bot.id, decision.NewLower, decision.NewUpper}, stopArg...)...); persistErr != nil || tag.RowsAffected() != 1 {
					worker.logger.Error("adjusted native grid range, but local range persist failed — next reconcile resyncs from remote truth",
						"component", "autogrid_worker", "bot_id", bot.id,
						"rows_affected", tag.RowsAffected(), "error", persistErr)
				} else {
					bot.adjustments++
				}
				worker.logger.Info("adjusted native grid range",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"lower", decision.NewLower.String(), "upper", decision.NewUpper.String())

				totalPnL := realized.Add(unrealized)
				if err := LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, "ADJUST_RANGE", &price, &totalPnL, map[string]any{
					"reason": decision.Reason, "mode": shiftMode,
					"new_lower": decision.NewLower.String(), "new_upper": decision.NewUpper.String(),
				}); err != nil {
					worker.logger.Warn("adjust-range event insert failed — durable cooldown not armed",
						"component", "autogrid_worker", "bot_id", bot.id, "error", err)
				}
				_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol,
					"lower_price": decision.NewLower.StringFixed(6), "upper_price": decision.NewUpper.StringFixed(6),
					"reason": decision.Reason, "mode": shiftMode, "adjustments_count": bot.adjustments + 1,
				})
			}
		case ActionUpdateTrailingSL:
			if decision.TrailingSLPrice == nil || !decision.TrailingSLPrice.IsPositive() {
				break
			}
			// v2.0.155: a failing updateTriggerProfitLoss must not machine-gun
			// the exchange every manage pass — one retry per hour, the same
			// backoff shape as the tranche-2 pour marker.
			if bot.trailingSLFailAt != nil {
				if failedAt, pErr := time.Parse(time.RFC3339, strings.TrimSpace(*bot.trailingSLFailAt)); pErr == nil &&
					time.Since(failedAt) < time.Hour {
					break
				}
			}
			newSLStr := decision.TrailingSLPrice.String()
			worker.logger.Info("advancing trailing stop-loss natively on Pionex bot card",
				"component", "autogrid_worker", "bot_id", bot.id, "symbol", bot.symbol,
				"remote_id", bot.remoteID, "new_sl", newSLStr)

			req := pionex.FuturesGridUpdateTriggerProfitLossRequest{
				BUOrderID: bot.remoteID,
				List: []pionex.FuturesGridTriggerProfitLossItem{
					{
						Type:     "stop_loss",
						StopType: "price",
						Value:    newSLStr,
					},
				},
			}
			_, updateErr := client.UpdateFuturesGridTriggerProfitLoss(ctx, req)
			if updateErr != nil {
				worker.logger.Error("failed to update native trailing stop-loss on Pionex",
					"component", "autogrid_worker", "bot_id", bot.id, "remote_id", bot.remoteID,
					"error", updateErr)
				if _, mErr := worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET model_state = jsonb_set(COALESCE(model_state, '{}'::jsonb), '{trailingSLFailAt}',
						to_jsonb(to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))),
					    updated_at = NOW()
					WHERE id = $1
				`, bot.id); mErr != nil {
					worker.logger.Warn("persist trailingSLFailAt backoff marker",
						"component", "autogrid_worker", "bot_id", bot.id, "error", mErr)
				}
			} else {
				_, _ = worker.db.Exec(ctx, `
					UPDATE grid_bots
					SET trailing_sl_price = $1, stop_loss_price = $1,
					    model_state = COALESCE(model_state, '{}'::jsonb) - 'trailingSLFailAt',
					    updated_at = NOW()
					WHERE id = $2
				`, decision.TrailingSLPrice, bot.id)
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "REAL", bot.symbol, "TRAILING_SL_ADVANCED", &price, nil, map[string]any{
					"new_sl": newSLStr, "reason": decision.Reason,
				})
				_ = QueueTelegramEvent(ctx, worker.db, "TRAILING_SL_ADVANCED", map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol, "new_sl": newSLStr, "reason": decision.Reason,
				})
			}
		}
	}

	// Synchronize closed bots with exchange history to fill accurate realized PnL.
	// v2.0.74 admitted the zero-PnL REMOTE_TERMINAL_CONFIRMED population the
	// old query excluded. v2.0.75 settled every terminal row in the 48h window
	// EXACTLY ONCE via the model_state.finalProfitSource marker. v2.0.89: the
	// window widens to 30 days (migration 0045 re-opened the whole REAL epoch's
	// residual finals) and the settle itself is exchange-total →
	// telemetry-net-close estimate → NULL; rows whose exchange record carries
	// no netted total settle from LOCAL telemetry, so no exchange call is
	// spent on them.
	closedRows, err := worker.db.Query(ctx, `
		SELECT id, bu_order_id, account_id, COALESCE(max_loss_usdt, 0),
		       COALESCE(closed_reason, ''), COALESCE(closed_at, updated_at)
		FROM grid_bots
		WHERE bu_order_id IS NOT NULL
		  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
		  AND model_state->>'finalProfitSource' IS NULL
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '30 days'
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err == nil {
		type closedBotItem struct {
			id, remoteID, accountID string
			maxLoss                 decimal.Decimal
			closedReason            string
			closedAt                time.Time
		}
		var unSynced []closedBotItem
		for closedRows.Next() {
			var item closedBotItem
			if err := closedRows.Scan(&item.id, &item.remoteID, &item.accountID,
				&item.maxLoss, &item.closedReason, &item.closedAt); err == nil {
				unSynced = append(unSynced, item)
			}
		}
		closedRows.Close()
		for _, item := range unSynced {
			historyClient, clientErr := clientFor(item.accountID)
			if clientErr != nil {
				continue
			}
			// v2.0.89: exchange-total first (either endpoint), estimate second.
			// A record WITHOUT a netted total no longer settles here — the
			// chain returns FinalProfitNone and the telemetry estimate takes
			// over, so a missing netted carrier can never leave the row on a
			// stale running accrual. The finished-bot list is still probed
			// BEFORE giving up on the exchange: it is the documented carrier
			// of netted totals for grids the detail endpoint refuses. Only
			// when the list has no record either does the detail endpoint's
			// not_found-class refusal COUNT as an answer (the grid is
			// finished; no total will ever come — estimate from local
			// telemetry). A genuine transport failure leaves the row
			// marker-less for the next pass — an estimate written on a
			// transport blip could shadow a profitExited still owed to us.
			var settled decimal.Decimal
			var source pionex.FinalProfitSource
			var exchangeReason string
			probeAnswered := false
			if remote, remoteErr := historyClient.GetFuturesGridBot(ctx, item.remoteID); remoteErr == nil && remote != nil {
				settled, source = remote.BUOrderData.SettledProfit()
				exchangeReason = remote.ReasonBy
				probeAnswered = true
			} else if finished := findFinishedGridRecord(ctx, historyClient, item.remoteID); finished != nil {
				settled, source = finished.SettledProfit()
				exchangeReason = finished.ReasonBy
				probeAnswered = true
			} else if remoteErr != nil && terminalRefusalError(remoteErr) {
				probeAnswered = true
			}
			if !probeAnswered {
				continue
			}
			// The gate consults BOTH reason sources: the stored closed_reason
			// is the only witness of our own manage stop (the exchange answers
			// "user cancel" for every stop we submit natively — prod FARTCOIN
			// +2.349 on ANTI_HUNT).
			decision := worker.settleTerminalFinal(ctx, item.id, item.closedAt,
				&item.maxLoss, item.closedReason, exchangeReason, settled, source)
			// The finalProfitSource marker rides IN the realized UPDATE
			// (v2.0.78): a standalone marker write that outlived a failed
			// realized persist would arm the exactly-once key against a row
			// whose figure never landed.
			if _, err := worker.db.Exec(ctx, `
				UPDATE grid_bots
				SET realized_pnl_usdt = CASE WHEN $3::BOOLEAN THEN NULL ELSE $2::NUMERIC END,
				    unrealized_pnl_usdt = 0,
				    supervision_floor_pnl_usdt = 0,
				    reconciliation_state = $6,
				    fees_paid_usdt = fees_paid_usdt + $5::NUMERIC,
				    model_state = jsonb_set(
				        CASE WHEN $5::NUMERIC = 0 THEN COALESCE(model_state, '{}'::jsonb)
				             ELSE jsonb_set(COALESCE(model_state, '{}'::jsonb), '{closeCostUsdt}', to_jsonb($5::NUMERIC)) END,
				        '{finalProfitSource}', to_jsonb($4::TEXT)),
				    updated_at = NOW()
				WHERE id = $1
			`, item.id, decision.final, decision.final == nil, decision.marker, decision.closeCost,
				pendingOrConfirmedRecon(decision.marker)); err != nil {
				worker.logger.Error("backfill closed grid final PnL",
					"component", "autogrid_worker", "bot_id", item.id, "error", err)
			}
			// v2.0.165: outcome cohort row for the 30d back-sweep settle.
			sweepTotal := decimal.Zero
			if d, ok := decision.final.(decimal.Decimal); ok {
				sweepTotal = d
			}
			recordRealBotOutcome(ctx, worker.db, worker.logger, item.id, sweepTotal, item.closedReason)
		}
	}

	// Unknown submissions are adopted per account: each pending row carries
	// the account its client must authenticate as. v2.0.78 CRIT-3: STOP_REQUESTED
	// rows with a NULL bu_order_id (stop requests that raced a create, or the
	// pre-fix zombies) join the adoption pool — the stop intent itself is
	// preserved on adopt, the manage loop then submits the native cancel.
	unknownAccountRows, err := worker.db.Query(ctx, `
		SELECT DISTINCT account_id FROM grid_bots
		WHERE bu_order_id IS NULL
		  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION', 'STOP_REQUESTED')
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

	// v2.0.83 bot-aggregate equity: capture runs AFTER the bot loop — the
	// realized/unrealized columns it sums were just refreshed from remote
	// truth by the passes above (running persist, terminal settle, 48h
	// backfill). Self-throttled to 5 minutes by the snapshot table.
	worker.captureBotAggregateEquity(ctx, *settings)

	// v2.0.89 part B: execute queued DGT re-deploys whose parent row has
	// settled terminal in this pass (or a previous one) — the REAL arm of
	// the break re-start, AFTER every settle path had its chance to write
	// the terminal state the intent gate keys on.
	worker.processDgtRealRedeployIntents(ctx, *settings)

	// v2.0.102: refresh the event-driven trigger baselines from THIS pass's
	// marks and the fleet's deploy-time ATRs, so the next sharp WS move is
	// measured against what supervision last saw.
	atrBySymbol := make(map[string]float64, len(bots))
	fleetSymbols := make([]string, 0, len(bots))
	for _, bot := range bots {
		if bot.atrEntry > 0 && bot.atrEntry > atrBySymbol[bot.symbol] {
			atrBySymbol[bot.symbol] = bot.atrEntry
		}
		fleetSymbols = append(fleetSymbols, bot.symbol)
	}
	// v2.0.144: pin the storm sensor to the fleet BEFORE the baselines
	// refresh — see setFleetStormSymbols for the pre-warm regression.
	worker.setFleetStormSymbols(fleetSymbols)
	if priceErr == nil && len(priceBySymbol) > 0 {
		worker.rememberRealtimeBaselines(priceBySymbol, atrBySymbol)
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
// v2.0.78 CRIT-3: STOP_REQUESTED rows with a NULL bu_order_id join the pool —
// they are the stop-race zombies; adoption preserves their stop intent.
func (worker *Worker) reconcileUnknownSubmissions(ctx context.Context, client *pionex.Client) {
	rows, err := worker.db.Query(ctx, `
		SELECT id, symbol, status, quote_investment, EXTRACT(EPOCH FROM created_at) * 1000
		FROM grid_bots
		WHERE bu_order_id IS NULL
		  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION', 'STOP_REQUESTED')
		  AND created_at > NOW() - INTERVAL '48 hours'
		  AND created_at < NOW() - INTERVAL '90 seconds'
		ORDER BY created_at
		LIMIT 10
	`)
	if err != nil {
		return
	}
	type unknownBot struct {
		id, symbol, status string
		investment         decimal.Decimal
		createdMS          float64
	}
	pending := make([]unknownBot, 0, 10)
	for rows.Next() {
		var item unknownBot
		if err := rows.Scan(&item.id, &item.symbol, &item.status, &item.investment, &item.createdMS); err == nil {
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
		maxPages := 10
		if listStatus == "finished" {
			maxPages = 3 // Recent finished grids are on the first pages; older history is irrelevant for adoption
		}
		for page := 0; page < maxPages; page++ {
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
			if page == maxPages-1 && listStatus == "running" {
				// Page budget exhausted on RUNNING: the listing is NOT complete — a live bot may sit on the next page.
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
				SET bu_order_id = $2,
				    status = CASE WHEN status = 'STOP_REQUESTED' THEN status ELSE 'RUNNING' END,
				    reconciliation_state = 'REMOTE_ID_PERSISTED',
				    last_remote_status = 'ADOPTED_AFTER_UNKNOWN_SUBMISSION',
				    last_error = NULL, updated_at = NOW()
				WHERE id = $1 AND bu_order_id IS NULL
				  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION', 'STOP_REQUESTED')
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
		// No running or finished order matches. Clear the row when:
		// 1) Both lists paginated to the end without errors, AND
		// 2) Either the operator explicitly requested stop (STOP_REQUESTED),
		//    or the 5-minute grace period has elapsed.
		graceElapsed := time.Since(time.UnixMilli(int64(bot.createdMS))) >= 5*time.Minute
		operatorRequestedStop := bot.status == "STOP_REQUESTED"
		if !listsComplete || (!graceElapsed && !operatorRequestedStop) {
			continue
		}
		tag, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'FAILED', closed_reason = 'NOT_CREATED_ON_EXCHANGE',
			    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
			    last_error = NULL, closed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND bu_order_id IS NULL
			  AND status IN ('SUBMISSION_UNKNOWN', 'PENDING_SUBMISSION', 'STOP_REQUESTED')
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
	data, _ := findFinishedGridRaw(ctx, client, buOrderID)
	return data
}

// findFinishedGridRaw also returns the record's raw buOrderData payload so
// the re-check sweep can log a live witness of the finished-record field
// shapes (the docs' FuturesGridOrderData has already drifted from the live
// API once — the raw line pins the truth for the next parser change).
func findFinishedGridRaw(ctx context.Context, client *pionex.Client, buOrderID string) (*pionex.BUOrderDataResponse, []byte) {
	if strings.TrimSpace(buOrderID) == "" {
		return nil, nil
	}
	token := ""
	// v2.0.99: the finished history only grows (epoch-2 + epoch-4 closures),
	// and a record the paging never reaches silently downgrades the terminal
	// final to a telemetry estimate. 30 pages with per-error visibility
	// instead of the silent nil the old loop returned.
	for page := 0; page < 30; page++ {
		orders, next, listErr := client.ListBotOrders(ctx, "finished", token)
		if listErr != nil {
			slog.Warn("finished-grid list probe failed — terminal final may fall back to estimate",
				"component", "autogrid_worker", "page", page, "error", listErr)
			return nil, nil
		}
		for _, order := range orders {
			if order.BUOrderID == buOrderID {
				data, decodeErr := order.FuturesGridData()
				if decodeErr != nil {
					slog.Warn("finished-grid record decode failed",
						"component", "autogrid_worker", "bu_order_id", buOrderID, "error", decodeErr)
					return nil, nil
				}
				return data, order.BUOrderData
			}
		}
		if next == "" {
			return nil, nil
		}
		token = next
	}
	slog.Warn("finished-grid record not found within paging depth",
		"component", "autogrid_worker", "bu_order_id", buOrderID, "pages_scanned", 30)
	return nil, nil
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
// tranche-2 top-up: the bot's DESIGN full stop range (budget × its own
// leverage × the ADAPTIVE_ATR stop CEILING) × breakerHeadroom. v2.0.67 sized
// the cap against the stop FLOOR (2%), which refused exactly the wide-stop
// bots the top-up exists to save: σ-scaled stops legally land anywhere in
// [DynamicLossMinPct, DynamicLossMaxPct], and a 6x/$100 bot with a $21.57
// stop was skipped ×3 against the floor-based $15 (prod SKYAI 2026-09-04).
// The ceiling-based cap admits the whole legal stop range (6x/$100 → $37.50)
// while still refusing genuine overshoots (a $40 stop on 6x/$100 exceeds
// even the ceiling by headroom).
func tranche2MaxLossCap(budgetUSDT decimal.Decimal, botLeverage int) decimal.Decimal {
	if botLeverage < 1 {
		botLeverage = 1
	}
	return budgetUSDT.
		Mul(decimal.NewFromInt(int64(botLeverage))).
		Mul(designStopCeilFrac()).
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
		       candidate_id, COALESCE(pairs_completed, 0), COALESCE(funding_paid_usdt, 0),
		       NULLIF(model_state->>'wickShieldTriggeredAt',''),
		       NULLIF(model_state->>'wickShieldExtreme','')::NUMERIC,
		       NULLIF(model_state->>'wickShieldLastClearedAt',''),
		       target_price, stop_loss_price, stop_loss_high, trailing_sl_price,
		       COALESCE(adaptive_strategy, ''), risk_reward_ratio
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
		wickShieldTriggeredAt    *string
		wickShieldExtreme        *decimal.Decimal
		wickShieldLastClearedAt  *string
		targetPrice              *decimal.Decimal
		stopLossPrice            *decimal.Decimal
		stopLossHigh             *decimal.Decimal
		trailingSLPrice          *decimal.Decimal
		adaptiveStrategy         string
		riskRewardRatio          *decimal.Decimal
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
			&item.wickShieldTriggeredAt, &item.wickShieldExtreme, &item.wickShieldLastClearedAt,
			&item.targetPrice, &item.stopLossPrice, &item.stopLossHigh, &item.trailingSLPrice,
			&item.adaptiveStrategy, &item.riskRewardRatio,
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

	// v2.0.89: the settings fee composite no longer prices anything here —
	// entry fees are booked once at deploy (paperEntryFee), pair legs pay the
	// documented maker rate and closes pay the calibrated taker+slippage
	// composite (paperCloseFeeRate). The calibrated rates live in ONE block
	// in finalprofit.go (vs REAL epoch 2026-09-03..05: wallet −31 vs
	// uncalibrated paper +70/day).
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

	// v2.0.155 REAL-parity: the storm deferral rides the paper OU rotation too.
	stormNow := worker.stormActive()
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
			// taker+slippage only on exits (v2.0.23). v2.0.89 fill buffer: a
			// level counts as crossed only 0.03% beyond its boundary — the
			// boundary-kiss fills were half of paper's fictional edge.
			levelBaseline := gridLevelForPrice(bot.lower, bot.upper, bot.gridNum, price)
			previousLevel := levelBaseline
			if bot.lastLevel != nil {
				levelBaseline = *bot.lastLevel
				previousLevel = *bot.lastLevel
			}
			currentLevel = bufferedGridLevel(bot.lower, bot.upper, bot.gridNum, price, levelBaseline)
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
			// Directional grid. v2.0.89: the entry taker fee is booked into
			// REALIZED once at deploy (paperEntryFee) — amortizing it into
			// every unrealized mark would re-charge it on every pass. The
			// floating mark is the pure position PnL; the exit fee lands in
			// the exit-fee block below.
			fundingExposure = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage)))
			fundingPays = bot.direction == "LONG"
			exitNotional = fundingExposure
			switch bot.direction {
			case "LONG":
				unrealized = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(price.Div(bot.entry).Sub(decimal.NewFromInt(1)))
			case "SHORT":
				unrealized = bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage))).Mul(decimal.NewFromInt(1).Sub(price.Div(bot.entry)))
			}
		}
		// Exit-fee honesty (v2.0.89 calibrated): the mark a close could
		// crystallize is net of taker 0.05% + slippage 0.05% on the open
		// inventory — the settings-composite (7 bps) understated what a
		// protective close pays in the book. Applied before decisions so
		// stop/target thresholds see the true close value.
		if exitNotional.IsPositive() {
			unrealized = unrealized.Sub(exitNotional.Mul(paperCloseFeeRate()))
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

		// v2.0.89 part B — OU half-life age rotation (paper arm): a bot
		// older than clamp(2×HL, 4h, 48h) has outlived the range its mesh
		// was fitted to. Soft rotation at the current mark; the slot returns
		// to the SCANNER (plain recycle — never a DGT re-center here, the
		// aged thesis says nothing about where to re-enter). Runs before the
		// break matrix so a stale range cannot hide behind a HOLD.
		{
			ageVerdict := gridAgeVerdictFor(worker.ouReadingForSymbol(ctx, bot.symbol, settings), time.Since(bot.openedAt))
			// v2.0.155 (review I-1): the paper pre-block rotation defers inside a
			// fleet storm exactly like the REAL one — rotating INTO the
			// acceleration crystallizes the bottom tick (v2.0.111 doctrine).
			if ageVerdict.rotate && !stormNow {
				_, err := worker.db.Exec(ctx, `
					UPDATE paper_grid_bots
					SET status = 'COMPLETED', closed_reason = $2,
					    realized_pnl_usdt = $3, unrealized_pnl_usdt = 0,
					    peak_pnl_usdt = GREATEST(peak_pnl_usdt, $3),
					    mark_price = $4, last_grid_level = $5,
					    model_state = jsonb_set(jsonb_set(COALESCE(model_state, '{}'::jsonb),
					        '{halfLifeHours}', to_jsonb($6::FLOAT8)),
					        '{maxAgeHours}', to_jsonb($7::FLOAT8)),
					    closed_at = NOW(), updated_at = NOW()
					WHERE id = $1 AND status = 'RUNNING'
				`, bot.id, gridAgedHalfLifeReason, total, price, currentLevel,
					ageVerdict.halfLifeHours, ageVerdict.maxAgeHours)
				if err != nil {
					return fmt.Errorf("close paper bot %s: %w", bot.symbol, err)
				}
				worker.logger.Info("paper bot rotated by OU half-life",
					"component", "autogrid_worker", "symbol", bot.symbol,
					"age_hours", time.Since(bot.openedAt).Hours(),
					"max_age_hours", ageVerdict.maxAgeHours, "pnl", total.String())
				recordCandidateOutcome(ctx, worker.db, bot.candidateID, total, gridAgedHalfLifeReason)
				pnlPct := decimal.Zero
				if !bot.investment.IsZero() {
					pnlPct = total.Div(bot.investment).Mul(decimal.NewFromInt(100)).Round(2)
				}
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol,
					gridAgedHalfLifeReason, &price, &total, map[string]any{
						"reason": gridAgedHalfLifeReason, "pnlPct": pnlPct,
						"age_hours":       time.Since(bot.openedAt).Hours(),
						"max_age_hours":   ageVerdict.maxAgeHours,
						"half_life_hours": ageVerdict.halfLifeHours,
					})
				queueGridAgedTelegram(ctx, worker, "PAPER", bot.botNumber, bot.symbol,
					ageVerdict, time.Since(bot.openedAt))
				continue
			}
		}

		// Lazily detect regime only when price escapes the range
		regime := ""
		buffer := settings.RangeBreakBufferPct.Div(decimal.NewFromInt(100))
		if price.LessThan(bot.lower.Mul(decimal.NewFromInt(1).Sub(buffer))) ||
			price.GreaterThan(bot.upper.Mul(decimal.NewFromInt(1).Add(buffer))) {
			regime = worker.regimeForSymbol(ctx, bot.symbol)
		}

		var ofiRegime *string
		var microBias *float64
		if worker.ofiEngine != nil {
			micro := worker.ofiEngine.Analyze(bot.symbol)
			rStr := string(micro.Regime)
			ofiRegime = &rStr
			microBias = &micro.MicroPriceBiasBps
		}
		ouReading := worker.ouReadingForSymbol(ctx, bot.symbol, settings)
		ouHalfLife := 0.0
		if ouReading.ok && ouReading.halfLifeHours > 0 {
			ouHalfLife = ouReading.halfLifeHours
		}

		decision := decideBotAction(botActionInput{
			Direction:         bot.direction,
			Lower:             bot.lower,
			Upper:             bot.upper,
			CurrentPrice:      price,
			RealizedPNL:       realized,
			UnrealizedPNL:     unrealized,
			PeakPNL:           peakPnL,
			Budget:            bot.investment,
			PnLTarget:         botTarget,
			MaxLoss:           botMaxLoss,
			RangeBreakBuffer:  settings.RangeBreakBufferPct,
			AdjustmentsLeft:   settings.MaxAdjustmentsPerBot - bot.adjustmentsCount,
			Regime:            regime,
			AntiHuntStop:      bot.antiHuntStop,
			TargetPrice:       bot.targetPrice,
			StopLossPrice:     bot.stopLossPrice,
			StopLossHigh:      bot.stopLossHigh,
			TrailingSLPrice:   bot.trailingSLPrice,
			OFIRegime:         ofiRegime,
			MicroPriceBiasBps: microBias,
			AgeHours:          time.Since(bot.openedAt).Hours(),
			OUHalfLifeHours:   ouHalfLife,
			SmartExitEnabled:  settings.SmartExitEnabled,
			OFIHarvestEnabled: settings.OFIHarvestEnabled,
			OURotationEnabled: settings.OURotationEnabled,
			// v2.0.155 REAL-parity: the storm deferral and the trailing
			// precision ride the paper arm too (the OU rotation and the
			// trailing candidate must not diverge between the fleets).
			PricePrecision: paperTrailPrecision(bot.lower),
			StormActive:    &stormNow,
		})

		if decision.Action == ActionHold || decision.Action == ActionAdjustUp || decision.Action == ActionAdjustDown {
			if bot.wickShieldTriggeredAt != nil {
				worker.logger.Info("paper wick shield saved bot from close - price recovered",
					"component", "autogrid_worker", "symbol", bot.symbol, "bot_id", bot.id, "price", price.String())
				_, _ = worker.db.Exec(ctx, `
					UPDATE paper_grid_bots
					SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
					    || jsonb_build_object('wickShieldLastClearedAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
					    updated_at = NOW()
					WHERE id = $1
				`, bot.id)
				_ = QueueTelegramEvent(ctx, worker.db, "WICK_SHIELD_SAVED", map[string]any{
					"bot_number": bot.botNumber, "symbol": bot.symbol, "price": price.StringFixed(6), "mode": "PAPER",
				})
			}
		}

		// v2.0.119 paper arm mirrors the REAL rules: ActionCloseStopLoss is
		// never deferred, the wick side follows the signed position. Paper
		// tracks no per-level inventory for NEUTRAL grids, so the side is
		// synthesized from the declared direction and a NEUTRAL paper bot —
		// whose inventory side is unknowable here — gets no shield at all.
		if decision.Action == ActionCloseStructInvalid || decision.Action == ActionCloseRangeBreak {
			if settings.WickShieldEnabled {
				paperSignedPos := decimal.Zero
				switch strings.ToUpper(bot.direction) {
				case "LONG":
					paperSignedPos = decimal.NewFromInt(1)
				case "SHORT":
					paperSignedPos = decimal.NewFromInt(-1)
				}
				shouldHold, newTriggeredAt, newExtreme, shieldReason := worker.evaluateWickShield(
					ctx, bot.symbol, paperSignedPos, price, bot.wickShieldTriggeredAt, bot.wickShieldExtreme, bot.wickShieldLastClearedAt, settings.WickGraceSec,
				)
				if shouldHold {
					if bot.wickShieldTriggeredAt == nil && newTriggeredAt != nil && newExtreme != nil {
						_, _ = worker.db.Exec(ctx, `
							UPDATE paper_grid_bots
							SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
							    || jsonb_build_object('wickShieldTriggeredAt', $2::TEXT, 'wickShieldExtreme', $3::NUMERIC),
							    updated_at = NOW()
							WHERE id = $1
						`, bot.id, *newTriggeredAt, newExtreme.String())
						worker.logger.Info("paper wick shield armed: deferring structural close",
							"component", "autogrid_worker", "symbol", bot.symbol, "reason", decision.Reason, "shield_reason", shieldReason)
						_ = QueueTelegramEvent(ctx, worker.db, "WICK_SHIELD_ARMED", map[string]any{
							"bot_number": bot.botNumber, "symbol": bot.symbol, "reason": decision.Reason,
							"wick_extreme": newExtreme.StringFixed(6), "grace_sec": settings.WickGraceSec, "mode": "PAPER",
						})
					} else {
						worker.logger.Debug("paper wick shield holding: close deferred",
							"component", "autogrid_worker", "symbol", bot.symbol, "shield_reason", shieldReason)
					}
					continue
				}
				if bot.wickShieldTriggeredAt != nil {
					_, _ = worker.db.Exec(ctx, `
						UPDATE paper_grid_bots
						SET model_state = (COALESCE(model_state, '{}'::jsonb) - 'wickShieldTriggeredAt' - 'wickShieldExtreme')
						    || jsonb_build_object('wickShieldLastClearedAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
						    updated_at = NOW()
						WHERE id = $1
					`, bot.id)
				}
			}
		}

		if decision.Action == ActionCloseTakeProfit || decision.Action == ActionCloseStopLoss ||
			decision.Action == ActionCloseRangeBreak || decision.Action == ActionCloseStructInvalid ||
			decision.Action == ActionCloseSmartHarvest || decision.Action == ActionCloseOURotation {
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
			} else if decision.Action == ActionCloseSmartHarvest {
				eventType = "SMART_HARVEST"
			} else if decision.Action == ActionCloseOURotation {
				eventType = "OU_ROTATION"
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
			// v2.0.89 part B — DGT break re-deploy (paper arm): the settle
			// above is done, the slot is free; a RANGE_BREAK_* close now
			// re-opens the symbol IMMEDIATELY, centered on the break price
			// with the same slot capital (arXiv 2506.11921). The re-deploy
			// carries its own gate set (macro/econ/portfolio/risk/ladder) —
			// see grid_lifecycle_policy.go; when it is blocked or disabled
			// the old scanner-owned behavior is exactly what remains.
			if settings.DgtRedeployEnabled && dgtBreakRedeployReason(decision.Reason) {
				worker.dgtRedeployPaper(ctx, settings, dgtRedeploySpec{
					symbol:       bot.symbol,
					direction:    bot.direction,
					breakPrice:   price,
					slotBudget:   slotCapital(bot.trancheBase, bot.investment),
					oldBotID:     bot.id,
					oldBotNumber: bot.botNumber,
					candidateID:  bot.candidateID,
					atrFallback:  bot.atrEntry,
				})
			}
			continue
		}

		if decision.Action == ActionUpdateTrailingSL {
			if decision.TrailingSLPrice != nil && decision.TrailingSLPrice.IsPositive() {
				_, _ = worker.db.Exec(ctx, `
					UPDATE paper_grid_bots
					SET trailing_sl_price = $1, stop_loss_price = $1, updated_at = NOW()
					WHERE id = $2
				`, decision.TrailingSLPrice, bot.id)
				_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "TRAILING_SL_ADVANCED", &price, nil, map[string]any{
					"new_sl": decision.TrailingSLPrice.String(), "reason": decision.Reason,
				})
			}
			continue
		}

		if decision.Action == ActionAdjustUp || decision.Action == ActionAdjustDown {
			// v2.0.128 OFI Protection for Running Bots (paper parity):
			// v2.0.139 — PRESSURE-tier widening (audit C gap, REAL mirror
			// above): adjust-down freezes under dump flow (DUMP_PRESSURE or
			// CONFIRMED_DUMP), adjust-up under pump flow — "don't chase the
			// flow"; a counter-flow regime must not freeze the shift.
			if worker.ofiEngine != nil {
				micro := worker.ofiEngine.Analyze(bot.symbol)
				if decision.Action == ActionAdjustDown {
					if micro.Regime == marketdata.RegimeDumpPressure || micro.Regime == marketdata.RegimeConfirmedDump {
						worker.logger.Warn("manage paper range shift frozen by OFI dump pressure",
							"component", "autogrid_worker", "bot_number", bot.botNumber, "symbol", bot.symbol,
							"regime", string(micro.Regime), "reason", micro.Reason)
						// v2.0.138 journal: the freeze's feature vector (REAL
						// mirror above) — paper freezes count the same.
						logOFIDecision(ctx, worker.db, settings.ID, bot.symbol, ofiKindAdjustFreeze,
							micro, string(micro.Readiness()), "FREEZE", micro.Reason,
							fmt.Sprintf("#%d", bot.botNumber))
						continue
					}
				} else if decision.Action == ActionAdjustUp {
					if micro.Regime == marketdata.RegimePumpPressure || micro.Regime == marketdata.RegimeConfirmedPump {
						worker.logger.Warn("manage paper range shift frozen by OFI pump pressure",
							"component", "autogrid_worker", "bot_number", bot.botNumber, "symbol", bot.symbol,
							"regime", string(micro.Regime), "reason", micro.Reason)
						logOFIDecision(ctx, worker.db, settings.ID, bot.symbol, ofiKindAdjustFreeze,
							micro, string(micro.Readiness()), "FREEZE", micro.Reason,
							fmt.Sprintf("#%d", bot.botNumber))
						continue
					}
				}
			}
			// Reset the pair-counting baseline under the NEW geometry. The old
			// code persisted currentLevel (computed against the OLD bounds),
			// so the next manage tick saw a phantom half-grid traverse and
			// booked gridNum/2 fictional completed pairs. Recomputing the
			// level against the shifted bounds makes the first tick after the
			// shift a no-op baseline (price sits at the new mid).
			newLevel := gridLevelForPrice(decision.NewLower, decision.NewUpper, bot.gridNum, price)
			// v2.0.93 FIX-J (REAL parity, v2.0.85 "shift always"): HOW the
			// shift ships follows the same floating-PnL preflight the REAL
			// manage path uses (adjustShiftMode). Green (unrealized ≥ +$0.10)
			// → normal re-base: the inventory mark crystallizes into realized.
			// Under water → keep_investment semantics: a pure range move, the
			// position is TRANSFERRED not exited — the floating loss carries
			// forward as unrealized against the new bounds instead of being
			// crystallized, mirroring the exchange's keepInvestment=true
			// rescue transfer the REAL path ships. The paper model used to
			// crystallize every shift, so an under-water paper bot banked its
			// full floating loss at the moment of the rescue while its REAL
			// twin kept the position alive.
			shiftMode := adjustShiftMode(unrealized)
			// The exit-fee component charged into `unrealized` for close
			// decisions is added BACK either way — no position is exited on a
			// shift, and repeated shifts must not stack phantom exit fees
			// (v2.0.13 audit fix).
			exitFeeBack := decimal.Zero
			if exitNotional.IsPositive() {
				exitFeeBack = exitNotional.Mul(paperCloseFeeRate())
			}
			shiftRealized := realized
			carryUnrealized := decimal.Zero
			if shiftMode == shiftModeNormal {
				shiftRealized = realized.Add(unrealized).Add(exitFeeBack)
			} else {
				carryUnrealized = unrealized.Add(exitFeeBack)
			}
			// v2.0.15 re-anchors, applied as extra SET clauses after the
			// fixed $1..$7 parameters:
			//   1. directional grids re-anchor entry_price on a NORMAL
			//      re-base — their unrealized is re-derived from entry every
			//      tick, so without it each shift double-counts position PnL.
			//      In keep_investment mode the entry stays: the transferred
			//      position keeps its original basis (the REAL exchange
			//      transfer moves bounds, not entries);
			//   2. the anti-hunt stop moves with the range (both modes),
			//      preserving its deploy-time distance-beyond-bound — a stale
			//      deploy stop drifts away from the shifted bounds and
			//      protects nothing.
			setClauses := ""
			var extraArgs []any
			if bot.direction != "NEUTRAL" && shiftMode == shiftModeNormal {
				setClauses += fmt.Sprintf(", entry_price = $%d", 8+len(extraArgs))
				extraArgs = append(extraArgs, price)
			}
			if bot.antiHuntStop != nil && bot.antiHuntStop.GreaterThan(decimal.Zero) {
				newStop := decision.NewLower.Sub(bot.lower.Sub(*bot.antiHuntStop))
				if bot.direction == "SHORT" {
					newStop = decision.NewUpper.Add(bot.antiHuntStop.Sub(bot.upper))
				}
				setClauses += fmt.Sprintf(", anti_hunt_stop_price = $%d", 8+len(extraArgs))
				extraArgs = append(extraArgs, newStop)
			}
			_, _ = worker.db.Exec(ctx, `
				UPDATE paper_grid_bots
				SET lower_price = $2, upper_price = $3,
				    adjustments_count = adjustments_count + 1,
				    mark_price = $4, unrealized_pnl_usdt = $5,
				    realized_pnl_usdt = $6, last_grid_level = $7`+setClauses+`
				WHERE id = $1
			`, append([]any{bot.id, decision.NewLower, decision.NewUpper, price,
				carryUnrealized, shiftRealized, newLevel}, extraArgs...)...)

			worker.logger.Info("adjusted paper grid range on the fly",
				"component", "autogrid_worker", "symbol", bot.symbol, "mode", shiftMode,
				"lower", decision.NewLower.String(), "upper", decision.NewUpper.String())

			_ = LogBotEvent(ctx, worker.db, bot.id, bot.botNumber, "PAPER", bot.symbol, "ADJUST_RANGE", &price, &total, map[string]any{
				"reason": decision.Reason, "mode": shiftMode,
				"new_lower": decision.NewLower.String(), "new_upper": decision.NewUpper.String(),
			})
			_ = QueueTelegramEvent(ctx, worker.db, "ADJUST_RANGE", map[string]any{
				"bot_number": bot.botNumber, "symbol": bot.symbol,
				"lower_price": decision.NewLower.StringFixed(6), "upper_price": decision.NewUpper.StringFixed(6),
				"reason": decision.Reason, "mode": shiftMode, "adjustments_count": bot.adjustmentsCount + 1,
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
					// risk desk no longer shows "$8" over a "$16" stop.
					effMaxLoss, effTarget := decimal.Zero, decimal.Zero
					if bot.maxLoss != nil {
						effMaxLoss = bot.maxLoss.Mul(decimal.NewFromInt(2))
					}
					if bot.pnlTarget != nil {
						effTarget = bot.pnlTarget.Mul(decimal.NewFromInt(2))
					}
					// v2.0.138 (audit, paper parity): the REAL lane's stress
					// moratorium — no second tranche while the bot's radar
					// band is ≥2 or a fleet storm is active; adding margin
					// into stress doubles exactly the position the radar is
					// trying to save (prod ORDI #1396). One journal row per
					// hour max via the tranche2SkipAt backoff marker.
					if !worker.trancheStressAllowed(ctx, bot.id) {
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
							worker.journalEntryDecision(ctx, EntryChainInput{
								Path: EntryPathTranche2, Settings: settings, Symbol: bot.symbol,
								Direction: bot.direction, Fleet: "PAPER", RefID: bot.id,
							}, entryOutcomeReject, "TRANCHE_STRESS",
								"мораторий стресса: шторм-режим или радар-полоса ≥2 — вторая доля не вливается в спасаемую позицию", nil)
						}
					} else if skip := worker.tranche2RiskGate(ctx, settings, bot.id, bot.leverage, effMaxLoss); skip != "" {
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
						// v2.0.89 tranche friction: the pour is a taker entry
						// too — taker 0.05% on the inventory notional the pour
						// adds (a neutral pour doubles the ladder, so the
						// added inventory IS the current inventory; a
						// directional pour adds pending × leverage).
						pourAddedNotional := base.Sub(bot.investment).
							Mul(decimal.NewFromInt(int64(bot.leverage)))
						if bot.direction == "NEUTRAL" {
							pourAddedNotional = fundingExposure
						}
						pourFee := pourAddedNotional.Mul(paperStopTakerBps).
							Div(decimal.NewFromInt(10000))
						tag, tErr := worker.db.Exec(ctx, `
						UPDATE paper_grid_bots
						SET quote_investment = $2,
						    pnl_target_usdt = pnl_target_usdt * 2,
						    max_loss_usdt = max_loss_usdt * 2,
						    realized_pnl_usdt = realized_pnl_usdt - $3::NUMERIC,
						    fees_paid_usdt = fees_paid_usdt + $3::NUMERIC,
						    model_state = jsonb_set(model_state, '{trancheDeployed}', '2'::jsonb),
						    updated_at = NOW()
						WHERE id = $1 AND status = 'RUNNING'
						  AND COALESCE(NULLIF(model_state->>'trancheDeployed','')::INT, 0) = 1
					`, bot.id, base, pourFee.Round(8).String())
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
	// v2.0.141 (audit P1): the journal tables' ids are UUIDs — Postgres has
	// no uuid % integer, so the v2.0.138 modulo batching failed on EVERY
	// pass and the discarded error made retention dead code (unbounded
	// growth). The tables are small (<100k rows steady state): a plain
	// created_at delete needs no batching.
	_, _ = worker.db.Exec(ctx, `DELETE FROM ofi_decision_snapshots WHERE created_at < NOW() - INTERVAL '14 days'`)
	_, _ = worker.db.Exec(ctx, `DELETE FROM entry_decisions WHERE created_at < NOW() - INTERVAL '14 days'`)
	_, _ = worker.db.Exec(ctx, `DELETE FROM gate_value_snapshots WHERE created_at < NOW() - INTERVAL '90 days'`)
	return nil
}

func (worker *Worker) priceMap(ctx context.Context) (map[string]decimal.Decimal, error) {
	// v2.0.58 (F8): mark decisions on the exchange's own markPrice — the
	// official PnL reference per Pionex docs — fetched for the whole PERP
	// universe in one public call. Last-trade tickers stay as the fallback:
	// an indexes outage must not wedge supervision.
	//
	// v2.0.98: the real-time lane overlays fresh WebSocket marks on top of
	// whichever REST snapshot landed — subscribed symbols get a mark that is
	// seconds old instead of seconds-plus-interval, and a REST outage falls
	// back to lane marks alone before the tickers fallback.
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
	} else {
		worker.logger.Debug("indexes fetch failed, trying tickers fallback",
			"component", "autogrid_worker", "error", err)
	}
	worker.overlayWSMarks(prices)
	if len(prices) > 0 {
		return prices, nil
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
// the tranche-2 time-box and adverse-turn gates, fetching the regime at most once
// per 5 minutes per symbol (the check would otherwise run every manage tick for every 24h+
// pending bot).
func (worker *Worker) trancheTimeBoxTrending(ctx context.Context, symbol string) bool {
	if cached, ok := worker.trancheTBRegime[symbol]; ok && time.Since(cached.checkedAt) < 5*time.Minute {
		return cached.trending
	}
	res, ok := worker.regimeResultForSymbol(ctx, symbol)
	trending := false
	if ok {
		// Guard: regime TREND_UP/TREND_DOWN or ADX > 25.0
		// Pouring tranche 2 into an active trend (ADX > 25) averages down into
		// a runaway breakout, turning normal stops into doubled stop losses.
		trending = res.Regime == "TREND_UP" || res.Regime == "TREND_DOWN" || res.ADX > 25.0
	}
	worker.trancheTBRegime[symbol] = trancheTBTrend{checkedAt: time.Now(), trending: trending}
	return trending
}

func (worker *Worker) regimeResultForSymbol(ctx context.Context, symbol string) (marketdata.RegimeResult, bool) {
	if worker.publicClient == nil {
		return marketdata.RegimeResult{}, false
	}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "60M", 60)
	if err != nil || len(candles) < 30 {
		return marketdata.RegimeResult{}, false
	}
	return marketdata.DetectRegime(candles), true
}

// regimeForSymbol lazily recomputes the market regime for a managed symbol.
func (worker *Worker) regimeForSymbol(ctx context.Context, symbol string) string {
	res, ok := worker.regimeResultForSymbol(ctx, symbol)
	if !ok {
		return ""
	}
	return res.Regime
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

// betaDownShortExempt (v2.0.147): while the beta gate reads BTC TREND_DOWN,
// a SHORT candidate whose own tape confirms the downtrend passes the
// entry-timing gate even outside the "favorable" channel zone. The mirror
// of the scanner's antiFomoShortFloorsLifted: strong trend (>22 ADX or
// >0.5 slope, the band the anti-FOMO floors already widen on) AND a
// falling EMA — slope sign is the direction proof, a rising pair is a
// divergence and keeps the gate armed. NEUTRAL/LONG are never exempt;
// R1+Vision and every later gate stay armed for the exempted short too.
func betaDownShortExempt(candidate Candidate, betaDown bool) bool {
	if !betaDown || candidate.RecommendedTrend != "short" {
		return false
	}
	adx, _ := candidate.ModelAssumptions["adx"].(float64)
	slope, _ := candidate.ModelAssumptions["emaSlopePct"].(float64)
	strongTrend := adx > 22.0 || math.Abs(slope) > 0.5
	return strongTrend && slope < 0
}

// marketBetaRegime caches BTC's regime for 2 minutes; the deploy paths ask
// for it once per candidate loop. Checks both 15M for fast intraday dump detection
// and 60M for macro market trend.
func (worker *Worker) marketBetaRegime(ctx context.Context) (string, float64, float64) {
	if time.Since(worker.betaRegime.checkedAt) < 2*time.Minute {
		return worker.betaRegime.regime, worker.betaRegime.adx, worker.betaRegime.emaSlope
	}
	// Fast 15M check: if BTC is actively flushing or pumping on 15M, trigger early beta regime
	if candles15M, err15 := worker.publicClient.GetKlines(ctx, "BTC_USDT_PERP", "15M", 30); err15 == nil && len(candles15M) >= 30 {
		res15 := marketdata.DetectRegime(candles15M)
		if res15.Regime == "TREND_DOWN" && res15.ADX >= 22.0 && res15.EMASlopePct < -0.4 {
			worker.betaRegime = betaRegimeCache{
				checkedAt: time.Now(),
				regime:    res15.Regime,
				adx:       res15.ADX,
				emaSlope:  res15.EMASlopePct,
			}
			return res15.Regime, res15.ADX, res15.EMASlopePct
		}
		if res15.Regime == "TREND_UP" && res15.ADX >= 22.0 && res15.EMASlopePct > 0.4 {
			worker.betaRegime = betaRegimeCache{
				checkedAt: time.Now(),
				regime:    res15.Regime,
				adx:       res15.ADX,
				emaSlope:  res15.EMASlopePct,
			}
			return res15.Regime, res15.ADX, res15.EMASlopePct
		}
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

// queueFastFollowScan (v2.0.171, consensus-2.0): after a profitable TP close,
// queue an out-of-turn scan so the scanner can immediately re-deploy the
// symbol if the trend is still confirmed. At most once per symbol per
// 15 minutes; the scan itself re-evaluates all gates — this is a priority
// nudge, not a bypass.
func (worker *Worker) queueFastFollowScan(ctx context.Context, settings Settings, symbol string, botNumber int) {
	if settings.Status != "RUNNING" {
		return
	}
	bucket := time.Now().Unix() / (15 * 60)
	idemKey := fmt.Sprintf("fast-follow-%s-%s-%d", settings.ID, symbol, bucket)
	tag, err := worker.db.Exec(ctx, `
		INSERT INTO control_commands (
			actor_type, command_type, resource_type, resource_id,
			arguments, sanitized_arguments, idempotency_key, status
		) VALUES (
			'SYSTEM', 'autogrid.scan', 'autogrid', $1,
			$2::jsonb, '{}'::jsonb, $3, 'QUEUED'
		)
		ON CONFLICT (idempotency_key) DO NOTHING
	`, settings.ID,
		fmt.Sprintf(`{"fastFollowSymbol": "%s", "fastFollowBotNumber": %d}`, symbol, botNumber),
		idemKey)
	if err != nil {
		return // best-effort
	}
	if tag.RowsAffected() > 0 {
		worker.logger.Info("fast-follow scan queued: profitable TP close on trending symbol",
			"component", "autogrid_worker", "symbol", symbol, "bot_number", botNumber)
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
