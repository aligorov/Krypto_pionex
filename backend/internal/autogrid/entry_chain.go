package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
)

// Entry admission chain (v2.0.138 package B foundation). Five bypass holes
// came from the same shape: every path that can OPEN or ADD risk carried its
// own hand-copied subset of the fleet-wide market gates, and the subsets
// drifted — the manual REAL deploy skipped all of them, invest_in skipped the
// breaker, paper tranche-2 skipped the stress moratorium, and the DGT
// re-deploy kept a third divergent breaker SQL. This file is the ONE composer
// for those gates plus the entry_decisions journal (migration 0052) that
// makes every admission evaluation auditable.
//
// The composer COMPOSES — it does not rewrite working gates. Every leg is
// the same SQL/reading the inline call sites ran, in the same plan order
// (storm → circuit breaker → economic events → liquidation cascade + feed
// health → macro veto), returning the exact user-facing Russian texts the
// previous call sites emitted so telegram/logs stay stable.

// EntryPath names the admission lane (entry_decisions.path). One row per
// evaluation per path; analytics group by this column to compare, e.g., what
// SCANNER_REAL refused for the same market MANUAL_DEPLOY allowed through.
type EntryPath string

const (
	EntryPathScannerPaper EntryPath = "SCANNER_PAPER"
	EntryPathScannerReal  EntryPath = "SCANNER_REAL"
	EntryPathDGTReal      EntryPath = "DGT_REAL"
	EntryPathDGTPaper     EntryPath = "DGT_PAPER"
	EntryPathManualDeploy EntryPath = "MANUAL_DEPLOY"
	EntryPathInvestIn     EntryPath = "INVEST_IN"
	EntryPathTranche2     EntryPath = "TRANCHE2"
)

// Machine codes of the deciding gate (entry_decisions.code). Empty string
// means "no blocker" — the composer's clear verdict.
const (
	entryBlockedStorm = "STORM"
	// entryWaitGateUnreadable (v2.0.167, week-audit P1-1): an ADVISORY leg
	// (breaker / economic events / cascade) whose SQL read failed even after
	// one immediate retry. The pass defers the candidate WITHOUT writing a
	// market REJECT — an infrastructure error must not poison the market
	// analytics (and must not silently ALLOW either: the audit's WAIT, with a
	// TTL of one scan pass). Capital-class gates stay fail-closed and the
	// feed-health leg stays fail-closed exactly as before.
	entryWaitGateUnreadable    = "GATE_UNREADABLE"
	entryBlockedCircuitBreaker = "CIRCUIT_BREAKER"
	entryBlockedEconomicEvent  = "ECONOMIC_EVENT"
	entryBlockedCascade        = "CASCADE"
	entryBlockedFeedHealth     = "FEED_HEALTH"
	entryBlockedMacro          = "MACRO"
)

// EntryChainInput is everything the composer needs about one admission
// evaluation. Direction drives the direction-aware legs: a SHORT entry is
// exempt from the liquidation cascade and the feed-health freeze (the long
// unwind window is precisely when shorts harvest — v2.0.19/v2.0.119).
type EntryChainInput struct {
	Path      EntryPath
	Settings  Settings
	Symbol    string
	Direction string // NEUTRAL | LONG | SHORT ("" → NEUTRAL)
	Fleet     string // PAPER | REAL
	RefID     string // candidate/bot/intent id
	// ScannerTrend carries the SCANNER's own recommended trend (lowercase
	// "long"/"short"/"no_trend") for the macro leg on scanner paths, where
	// the macro veto historically judged the scanner trend — NOT the final
	// smart-override direction — so an overridden SHORT stays exempt.
	// Empty → the macro leg judges Direction.
	ScannerTrend string
	// CascadeShort marks the out-of-turn cascade-short scan window: the
	// macro leg is exempt there (the flush IS the short entry).
	CascadeShort bool
}

// entryDirectionFromTrend normalizes any stored/scanner trend vocabulary
// ("short", "SHORT", "no_trend", "neutral", ""…) onto the composer's
// Direction enum.
func entryDirectionFromTrend(trend string) string {
	switch strings.ToLower(strings.TrimSpace(trend)) {
	case "long":
		return "LONG"
	case "short":
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}

// marketBlockerCache (v2.0.142, audit P2c) memoizes the fleet-constant
// composer legs for ONE deploy pass. The circuit-breaker and economic-event
// legs read fleet-wide tables — identical for every candidate of a pass —
// but the composer ran them per candidate (~41×/pass: two SQL round trips
// each on the hot scan path). A cache is created at the top of
// deployPaper/deployReal, filled by the pass-level probe and reused by every
// per-candidate cut. The direction-aware legs (cascade, feed health) and the
// macro veto stay per candidate — they judge the candidate's own
// direction/scanner trend. Storm stays uncached too: it is an in-memory read
// that may arm MID-pass, and freezing it at pass start would change
// semantics. Failure semantics are unchanged, only memoized: an SQL error
// keeps the leg's fail-open behavior for the whole pass, exactly as ~41
// uncached fail-opens would have. A nil cache reads through uncached, so
// the single-shot callers (service variant, DGT re-deploy) are unaffected.
type marketBlockerCache struct {
	breakerDone  bool
	breakerCount int
	breakerErr   error
	econDone     bool
	econBlocked  bool
	econTitle    string
	econErr      error
}

// breakerCloses is the memoized jointProtectiveClosesLastHour reading.
func (c *marketBlockerCache) breakerCloses(ctx context.Context, db *pgxpool.Pool, settingsID string) (int, error) {
	if c == nil {
		return jointProtectiveClosesLastHour(ctx, db, settingsID)
	}
	if !c.breakerDone {
		c.breakerCount, c.breakerErr = jointProtectiveClosesLastHour(ctx, db, settingsID)
		c.breakerDone = true
	}
	return c.breakerCount, c.breakerErr
}

// economicWindow is the memoized economicEventsAhead reading (the composer's
// fixed 2h-ahead window). v2.0.167: carries the SQL error so the composer
// can defer as WAIT instead of fail-opening on a partial DB degradation.
func (c *marketBlockerCache) economicWindow(ctx context.Context, db *pgxpool.Pool) (bool, string, error) {
	if c == nil {
		return economicEventsAheadErr(ctx, db, 2)
	}
	if !c.econDone {
		c.econBlocked, c.econTitle, c.econErr = economicEventsAheadErr(ctx, db, 2)
		c.econDone = true
	}
	return c.econBlocked, c.econTitle, c.econErr
}

// evaluateSharedMarketBlockers runs the fleet-wide market gates in plan order
// for the worker's paths (scanner deploys, DGT re-deploys, tranche-2 pours).
// Returns ("", "") when clear, else the machine code of the FIRST blocker
// and its human reason in the existing phrasing.
func (worker *Worker) evaluateSharedMarketBlockers(ctx context.Context, in EntryChainInput) (string, string) {
	code, reason, _, _ := worker.probeSharedMarketBlockers(ctx, in)
	return code, reason
}

// probeSharedMarketBlockers is the composer's full read: besides code/reason
// it returns the noteDeployBlock phrasing (the cascade/feed legs word the
// pass-level note slightly differently than the per-candidate rejection) and
// a small feature map of the deciding gate's reading (breaker count, event
// title, cascade USD…) so call sites can log exactly what they logged before
// and the journal can carry it in features.
func (worker *Worker) probeSharedMarketBlockers(ctx context.Context, in EntryChainInput) (code, reason, note string, features map[string]any) {
	return worker.probeSharedMarketBlockersCached(ctx, in, nil)
}

// probeSharedMarketBlockersCached (v2.0.142, audit P2c) is the pass-scoped
// cut of the composer: identical legs and order, with the fleet-constant
// legs (breaker, economic events) served from the pass's marketBlockerCache.
// deployPaper/deployReal create one cache per pass and thread it through
// both the pass-level probe and every per-candidate cut — the two SQL legs
// run once per pass instead of once per candidate. The cache pointer may be
// nil (single-shot callers get the plain uncached behavior).
func (worker *Worker) probeSharedMarketBlockersCached(ctx context.Context, in EntryChainInput, cache *marketBlockerCache) (code, reason, note string, features map[string]any) {
	if worker == nil || worker.db == nil {
		return "", "", "", nil
	}
	return evaluateSharedMarketBlockersDB(ctx, worker.db, worker.stormActive, in, cache)
}

// evaluateSharedMarketBlockersSvc is the service-layer variant for paths
// without a Worker (manual deploy, invest_in). Storm mode is worker-owned
// in-memory state; the Service.StormActive hook (wired by the worker's Run —
// same process) exposes it, and a nil hook reads as "not stormy". That is
// the one accepted limitation: a service-only binary cannot see storms.
// Uncached by design: its call sites evaluate once, not per candidate.
func (s *Service) evaluateSharedMarketBlockersSvc(ctx context.Context, in EntryChainInput) (string, string) {
	if s == nil || s.db == nil {
		return "", ""
	}
	var stormHook func() bool
	if s.StormActive != nil {
		stormHook = s.StormActive
	}
	code, reason, _, _ := evaluateSharedMarketBlockersDB(ctx, s.db, stormHook, in, nil)
	return code, reason
}

// evaluateSharedMarketBlockersDB is THE single implementation of the gate
// sequence. Worker and service variants are thin adapters over it.
//
// Leg order (plan order — the relative order the inline call sites already
// ran): storm → circuit breaker → economic events → liquidation cascade +
// feed health (both direction-aware, SHORT exempt) → macro veto (scanner
// trend + cascade-short exemption aware). Every leg fail-opens on SQL errors
// exactly like the inline code it replaces did. cache memoizes the
// fleet-constant legs (breaker, economic events) when the caller is a
// per-candidate deploy loop; nil reads through uncached.
func evaluateSharedMarketBlockersDB(
	ctx context.Context,
	db *pgxpool.Pool,
	stormActive func() bool,
	in EntryChainInput,
	cache *marketBlockerCache,
) (code, reason, note string, features map[string]any) {
	// 1. Storm (v2.0.111): opening fresh grids into a fleet-wide acceleration
	//    buys the worst entries of the day. Entries defer; closes, stops and
	//    the radar keep working untouched — so it applies to EVERY entry path.
	if stormActive != nil && stormActive() {
		r := stormBlockReason()
		return entryBlockedStorm, r, r, nil
	}
	// 2. Portfolio circuit breaker: >= 3 joint paper+REAL protective closes
	//    in the last hour pauses new entries. The joint count is deliberate —
	//    a paper fleet under stress proves a pipeline REAL would ride.
	//    v2.0.167: an unreadable reading (SQL error even after one direct
	//    retry bypassing the pass cache) defers the pass as WAIT instead of
	//    fail-opening.
	breakerCloses, breakerErr := cache.breakerCloses(ctx, db, in.Settings.ID)
	if breakerErr != nil {
		breakerCloses, breakerErr = jointProtectiveClosesLastHour(ctx, db, in.Settings.ID)
	}
	if breakerErr != nil {
		return entryWaitGateUnreadable,
			"защитный гейт недоступен (breaker SQL) — пасс отложен, повтор на следующем скане",
			"breaker unreadable after retry", map[string]any{"leg": "breaker"}
	}
	if breakerCloses >= 3 {
		r := circuitBreakerReason(in.Path, breakerCloses)
		return entryBlockedCircuitBreaker, r, r, map[string]any{"closes": breakerCloses}
	}
	// 3. Economic events: the T−2h…T+1h window around high-impact USD prints.
	//    v2.0.167: unreadable calendar (SQL error after retry) = WAIT, not
	//    "no events".
	econBlocked, econTitle, econErr := cache.economicWindow(ctx, db)
	if econErr != nil {
		econBlocked, econTitle, econErr = economicEventsAheadErr(ctx, db, 2)
	}
	if econErr != nil {
		return entryWaitGateUnreadable,
			"защитный гейт недоступен (эконом-календарь SQL) — пасс отложен, повтор на следующем скане",
			"economic calendar unreadable after retry", map[string]any{"leg": "economic_events"}
	}
	if econBlocked {
		r := economicEventReason(in.Path, econTitle)
		return entryBlockedEconomicEvent, r, r, map[string]any{"title": econTitle}
	}
	direction := entryDirectionFromTrend(in.Direction)
	if direction != "SHORT" {
		// 4a. Liquidation cascade: forced long unwinding blocks LONG/NEUTRAL
		//     entries (v2.0.14/v2.0.19). SHORT participation stays live — the
		//     unwind window is when shorts are paid.
		cascade, totalUSD, cascadeErr := liquidationCascadeActive(ctx, db, 50_000_000)
		if cascadeErr != nil {
			// v2.0.167: unreadable cascade reading = WAIT (one retry inside
			// the query helper is not added here — the feed-health leg below
			// independently fail-closes a dead feed, so this only covers a
			// transient SQL blip on a live feed).
			cascade, totalUSD, cascadeErr = liquidationCascadeActive(ctx, db, 50_000_000)
		}
		if cascadeErr != nil {
			return entryWaitGateUnreadable,
				"защитный гейт недоступен (каскад-ликвидаций SQL) — пасс отложен, повтор на следующем скане",
				"cascade unreadable after retry", map[string]any{"leg": "liquidation_cascade"}
		}
		if cascade {
			r, n := cascadeReasons(in.Path, totalUSD)
			return entryBlockedCascade, r, n, map[string]any{"usd_1h": totalUSD}
		}
		// 4b. Feed health (v2.0.119 fail-closed cascade): a SILENT liquidation
		//     source must read as blocked, not as "no cascade" — the gate's
		//     table dies quietly exactly for the crash it exists for.
		if healthy, lastEvent := liquidationSourceHealthyDB(ctx, db); !healthy {
			r, n := feedHealthReasons(in.Path, lastEvent)
			return entryBlockedFeedHealth, r, n, map[string]any{"last_event": lastEvent.Format(time.RFC3339)}
		}
	}
	// 5. Macro veto (v2.0.44, CoinGecko): beta-drift / alt-drain kill
	//    non-short entries; shorts and the cascade-short window are exempt;
	//    fail-open until ~24h of snapshot history exists.
	trend := strings.ToLower(strings.TrimSpace(in.ScannerTrend))
	if trend == "" {
		trend = "neutral"
		if direction == "SHORT" {
			trend = "short"
		} else if direction == "LONG" {
			trend = "long"
		}
	}
	if veto, vetoReason, telemetry := macroVeto(trend, in.CascadeShort, loadMacroContextDB(ctx, db)); veto {
		return entryBlockedMacro, vetoReason, vetoReason, telemetry
	}
	return "", "", "", nil
}

// jointProtectiveClosesLastHour is THE single circuit-breaker reading: the
// joint paper (settings-scoped) + REAL (account-wide) protective-close count
// over the trailing hour, using the protectiveCloseExemptReasons exemption
// list. The three divergent copies this replaces (paper joint, REAL-only,
// DGT joint) all agreed on ≥3 within 1h — only their fleet scoping drifted.
func jointProtectiveClosesLastHour(ctx context.Context, db *pgxpool.Pool, settingsID string) (int, error) {
	var count int
	err := db.QueryRow(ctx, `
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
	`, settingsID).Scan(&count)
	return count, err
}

// economicEventsAhead mirrors Worker.CheckEconomicEvents (gates.go) — same
// SQL, same window — as a package function the worker-free service variant
// can call. gates.go keeps its method for its existing callers; this copy
// exists because gates.go is not part of the entry-chain refactor surface.
// economicEventsAhead keeps its historical fail-open signature for any
// legacy caller; the composer uses the error-carrying variant below.
func economicEventsAhead(ctx context.Context, db *pgxpool.Pool, hoursAhead int) (bool, string) {
	blocked, title, _ := economicEventsAheadErr(ctx, db, hoursAhead)
	return blocked, title
}

// economicEventsAheadErr is the v2.0.167 error-carrying core of the
// economic-events leg.
func economicEventsAheadErr(ctx context.Context, db *pgxpool.Pool, hoursAhead int) (bool, string, error) {
	const whereClause = `
        WHERE impact = 'High'
          AND (country = 'USD' OR country IS NULL OR country = '')
          AND event_time BETWEEN NOW() - INTERVAL '1 hour' AND NOW() + ($1 || ' hours')::interval
    `
	var count int
	err := db.QueryRow(ctx, `
        SELECT COUNT(*) FROM economic_events`+whereClause,
		fmt.Sprintf("%d", hoursAhead)).Scan(&count)
	if err != nil {
		return false, "", err
	}
	if count > 0 {
		var title string
		_ = db.QueryRow(ctx, `
        SELECT title FROM economic_events`+whereClause+`
        ORDER BY ABS(EXTRACT(EPOCH FROM (event_time - NOW()))) LIMIT 1`,
			fmt.Sprintf("%d", hoursAhead)).Scan(&title)
		return true, title, nil
	}
	var fomcCount int
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM fomc_meetings
		WHERE decision_at BETWEEN NOW() - INTERVAL '30 minutes' AND NOW() + INTERVAL '3 hours'
	`).Scan(&fomcCount); err == nil && fomcCount > 0 {
		return true, "FOMC decision window", nil
	}
	return false, "", nil
}

// liquidationCascadeActive mirrors Worker.CheckLiquidationCascade (gates.go):
// long-side liquidation USD over the trailing hour against the threshold.
func liquidationCascadeActive(ctx context.Context, db *pgxpool.Pool, thresholdUSD float64) (bool, float64, error) {
	var totalUSD float64
	err := db.QueryRow(ctx, `
        SELECT COALESCE(SUM(value_usd), 0) FROM liquidation_events
        WHERE captured_at > NOW() - INTERVAL '1 hour'
          AND side = 'long'
    `).Scan(&totalUSD)
	if err != nil {
		return false, 0, err
	}
	return totalUSD > thresholdUSD, totalUSD, nil
}

// liquidationSourceHealthyDB mirrors Worker.LiquidationSourceHealthy
// (gates.go): transport evidence for the configured source; quiet markets
// can be healthy, missing health or SQL errors cannot.
func liquidationSourceHealthyDB(ctx context.Context, db *pgxpool.Pool) (healthy bool, lastEvent time.Time) {
	var last *time.Time
	var connected bool
	if err := db.QueryRow(ctx, `
        SELECT connected, last_message_at FROM liquidation_feed_health
        WHERE source = COALESCE(
            (SELECT NULLIF(value#>>'{}', '') FROM app_config WHERE key = 'liquidation_source'),
            'bybit')
    `).Scan(&connected, &last); err != nil || last == nil {
		return false, time.Time{}
	}
	age := time.Since(*last)
	return connected && age >= 0 && age <= liquidationSourceStaleness, *last
}

// loadMacroContextDB is the worker-free twin of Worker.loadMacroContext
// (macro_gate.go): the same marketdata.LatestCoinGeckoWindow reading and the
// same derived struct, so the service variant judges the identical context.
func loadMacroContextDB(ctx context.Context, db *pgxpool.Pool) macroContext {
	latest, aged, err := marketdata.LatestCoinGeckoWindow(ctx, db, macroHistoryAge)
	if err != nil || latest == nil {
		return macroContext{loaded: false}
	}
	mc := macroContext{loaded: true}
	btc := latest.BTC24hPct
	mc.btc24h = &btc
	if time.Since(latest.CapturedAt) > time.Hour {
		mc.stale = true
	}
	if aged != nil && latest.BTCDominancePct > 0 && aged.BTCDominancePct > 0 {
		d := latest.BTCDominancePct - aged.BTCDominancePct
		mc.domDelta = &d
	}
	return mc
}

// ── reason families ────────────────────────────────────────────────────────
//
// One text per (gate, path) — the exact strings the inline call sites
// emitted, so telegram/log texts stay byte-stable. The note variant is the
// pass-level noteDeployBlock phrasing; the reason variant is what a rejected
// candidate/bot carries. Paths that had no inline text before (manual
// deploy, invest_in, tranche-2) get the same sentence shapes.

func stormBlockReason() string {
	return "шторм-режим флота: ≥3 символов в ускорении за 5м — новые входы отложены до конца шторм-окна (стопы и радар работают)"
}

func circuitBreakerReason(path EntryPath, closes int) string {
	switch path {
	case EntryPathScannerReal:
		return fmt.Sprintf("REAL circuit breaker: %d защитных закрытий за последний час — деплои на паузе", closes)
	case EntryPathDGTReal, EntryPathDGTPaper:
		return fmt.Sprintf("circuit breaker: %d защитных закрытий за последний час — редеплой на паузе", closes)
	case EntryPathInvestIn, EntryPathTranche2:
		return fmt.Sprintf("circuit breaker: %d защитных закрытий за последний час — доливка на паузе", closes)
	default:
		return fmt.Sprintf("circuit breaker: %d защитных закрытий за последний час — новые деплои на паузе", closes)
	}
}

func economicEventReason(path EntryPath, title string) string {
	switch path {
	case EntryPathScannerReal:
		return "REAL деплой заблокирован: макро-событие USD «" + title + "» (окно T−2ч…T+1ч)"
	case EntryPathDGTReal, EntryPathDGTPaper:
		return "макро-событие USD «" + title + "» — редеплой отложен"
	case EntryPathInvestIn, EntryPathTranche2:
		return "макро-событие USD «" + title + "» — доливка отложена"
	default:
		return "деплой заблокирован: макро-событие USD «" + title + "» (окно T−2ч…T+1ч)"
	}
}

// cascadeReasons returns (per-entry rejection, pass-level note). The scanner
// paths worded the two consumers slightly differently; both are preserved.
func cascadeReasons(path EntryPath, usd1h float64) (reason, note string) {
	million := usd1h / 1_000_000
	switch path {
	case EntryPathDGTReal, EntryPathDGTPaper:
		r := fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL редеплой на паузе", million)
		return r, r
	case EntryPathManualDeploy:
		r := fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL деплой на паузе (SHORT доступен)", million)
		return r, r
	case EntryPathInvestIn, EntryPathTranche2:
		r := fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL доливка на паузе", million)
		return r, r
	case EntryPathScannerReal:
		return fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — входы LONG/NEUTRAL на паузе (SHORT доступны)", million),
			fmt.Sprintf("REAL: каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL деплои на паузе, SHORT доступны", million)
	default:
		return fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — входы LONG/NEUTRAL на паузе (SHORT доступны)", million),
			fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL деплои на паузе, SHORT доступны", million)
	}
}

func feedHealthReasons(path EntryPath, lastEvent time.Time) (reason, note string) {
	switch path {
	case EntryPathDGTReal, EntryPathDGTPaper:
		r := fmt.Sprintf("источник ликвидаций нестабилен (тишина >15м, последнее %s) — LONG/NEUTRAL редеплой на паузе до восстановления",
			lastEvent.Format(time.RFC3339))
		return r, r
	case EntryPathManualDeploy:
		r := "источник ликвидаций нестабилен (тишина >15м) — деплой отложен до восстановления"
		return r, r
	case EntryPathInvestIn, EntryPathTranche2:
		r := "источник ликвидаций нестабилен (тишина >15м) — доливка отложена до восстановления"
		return r, r
	case EntryPathScannerReal:
		return "источник ликвидаций нестабилен (тишина >15м) — входы LONG/NEUTRAL на паузе до восстановления",
			"REAL: источник ликвидаций нестабилен (тишина >15м) — LONG/NEUTRAL деплои на паузе до восстановления"
	default:
		return "источник ликвидаций нестабилен (тишина >15м) — входы LONG/NEUTRAL на паузе до восстановления",
			"источник ликвидаций нестабилен (тишина >15м) — LONG/NEUTRAL деплои на паузе до восстановления"
	}
}

// ── entry-decision journal (migration 0052) ────────────────────────────────

// Entry decision outcomes (entry_decisions.outcome).
const (
	entryOutcomeAllow = "ALLOW"
	// entryOutcomeWait is written by the v2.0.140 DIRECTION_FLIP handoff
	// (grid_lifecycle_policy.go): the slot does not deploy here — it is
	// handed to the scanner's cascade-short lane, so the journal records the
	// deferral instead of an ALLOW/REJECT verdict.
	entryOutcomeWait   = "WAIT"
	entryOutcomeReject = "REJECT"
)

// journalEntryDecision persists one admission evaluation into entry_decisions
// (write-only, failure-tolerant — a journal failure must never change, delay
// or block the trading decision it observes). Written on ALLOW after a
// successful create/deploy (one row per created bot) and on REJECT at every
// market-blocker rejection; WAIT marks the DIRECTION_FLIP handoff deferral.
func (worker *Worker) journalEntryDecision(ctx context.Context, in EntryChainInput, outcome, code, reason string, features map[string]any) {
	if worker == nil || worker.db == nil {
		return
	}
	if err := insertEntryDecisionRow(ctx, worker.db, in, outcome, code, reason, features); err != nil && worker.logger != nil {
		worker.logger.Warn("entry decision journal write failed",
			"component", "autogrid_worker", "path", in.Path, "symbol", in.Symbol, "error", err)
	}
}

// journalEntryDecisionSvc is the service-layer variant (manual deploy,
// invest_in) — same row, same failure-tolerance, no Worker required.
func journalEntryDecisionSvc(ctx context.Context, db *pgxpool.Pool, in EntryChainInput, outcome, code, reason string, features map[string]any) {
	if db == nil {
		return
	}
	_ = insertEntryDecisionRow(ctx, db, in, outcome, code, reason, features)
}

func insertEntryDecisionRow(ctx context.Context, db *pgxpool.Pool, in EntryChainInput, outcome, code, reason string, features map[string]any) error {
	if features == nil {
		features = map[string]any{}
	}
	featuresJSON, err := json.Marshal(features)
	if err != nil {
		featuresJSON = []byte("{}")
	}
	// v2.0.184 decision intelligence: every journaled evaluation gets its
	// episode linkage and a single-element gate trace (the deciding gate with
	// the numeric inputs it saw). Best-effort alongside the row itself — an
	// episode-resolution failure downgrades to the plain pre-184 insert.
	var episodeID *uuid.UUID
	attemptNo := 1
	dir := in.Direction
	if dir == "" {
		dir = "NEUTRAL"
	}
	regime, _ := features["regime"].(string)
	if eID, att, eErr := ResolveEpisode(ctx, db, in.Symbol, dir, regime); eErr == nil {
		episodeID = &eID
		attemptNo = att
	}
	stage, _ := features["stage"].(string)
	if stage == "" {
		stage = "INTERMEDIATE"
	}
	dataQuality, _ := features["dataQuality"].(string)
	if dataQuality == "" {
		dataQuality = "OK"
	}
	traceJSON, _ := json.Marshal([]GateTraceEntry{{
		Gate: code, Verdict: outcome, Inputs: features, Quality: dataQuality,
	}})
	_, err = db.Exec(ctx, `
		INSERT INTO entry_decisions (
			path, fleet, symbol, outcome, code, reason, features, config_version, ref_id,
			episode_id, attempt_no, decision_at, data_quality, gate_trace, stage
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9,
			$10, $11, NOW(), $12, $13::jsonb, $14)
	`,
		string(in.Path), in.Fleet, in.Symbol, outcome, code, reason,
		string(featuresJSON), ConfigVersion(ctx, db, in.Settings.ID), in.RefID,
		episodeID, attemptNo, dataQuality, string(traceJSON), stage)
	return err
}
