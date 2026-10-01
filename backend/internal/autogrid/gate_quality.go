package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Gate quality aggregates (v2.0.184, decision-intelligence plan §6/§7
// screen 3). gate_value.go answers "what did the blocked entries do" from
// the legacy shadow portfolio (migration 0031); this module upgrades the
// question onto the decision-intelligence stack of migration 0062:
// per-gate × per-regime aggregates with observation COVERAGE and PROOF
// STRENGTH. The honest answer to "does this gate pay for itself" must say
// "not enough data" until coverage and completed outcomes say otherwise —
// proof_strength exists so nobody reads a 3-episode verdict as a conclusion.
//
// Grouping:
//   - the deciding gate (entry_decisions.code of outcome='REJECT' rows);
//   - the ALLOW control group gets its own synthetic rows,
//     gate='ALLOW_CHAIN' (episodes the whole chain admitted, §6);
//   - × the episode regime (entry_episodes.regime, UNKNOWN when unlinked —
//     a long week never answers for shorts, plan invariant 7).
//
// coverage = shadow_created / (shadow_created + skip_reasons) over
// shadow_coverage_log: an analytics failure is a LOST observation, never a
// zero PnL (plan invariant 2). The daily Telegram summary carries the lost
// counter from decision_intelligence.go.

const (
	// The two aggregate windows persisted into gate_quality_daily.window.
	gateQualityWindow24H = "24H"
	gateQualityWindow7D  = "7D"

	// Feature flag switch (migration 0062 inserts it enabled).
	gateQualityFlagKey = "decision_intelligence"

	// gateQualityAllowChain is the synthetic gate key for the ALLOW control
	// group rows (episode disposition ALLOWED).
	gateQualityAllowChain = "ALLOW_CHAIN"

	// gateQualityMinPairs is the calibration sample floor (§6): below it the
	// only honest calibration verdict is "insufficient".
	gateQualityMinPairs = 5

	// gateQualityDueInterval throttles the daily hook: the worker may call
	// every manage pass, the summary fires at most once per ~day. Slightly
	// under 24h so a fixed-time daily pass never skips a day.
	gateQualityDueInterval = 20 * time.Hour

	// gateQualityEvent rides the default Telegram lane (always-on, renders
	// vars["message"] verbatim — same pattern as SHADOW_CAPTURE_FAILED in
	// shadow_portfolio.go).
	gateQualityEvent = "GATE_QUALITY_DIGEST"

	// Durable "already summarized" marker (bot_execution_events) — survives
	// restarts, works even when Telegram is disabled or the window is empty.
	gateQualityMarkerBot   = "gate-quality"
	gateQualityMarkerEvent = "GATE_QUALITY_DIGEST_SENT"

	// gateQualityGateLen mirrors gate_quality_daily.gate VARCHAR(32): an
	// entry_decisions.code longer than the column would fail the upsert.
	gateQualityGateLen = 32
)

// ProofStrength is the pure evidence classifier shared by the upsert path
// and the summary: LOW while coverage < 0.5 OR fewer than 20 completed
// outcomes; MEDIUM while coverage < 0.8 OR fewer than 50; HIGH only when
// both bars are cleared. Boundaries are inclusive-clearing (0.5 counts as
// "not low", 0.8 / 50 as "not medium").
func ProofStrength(coverage float64, completedOutcomes int) string {
	if coverage < 0.5 || completedOutcomes < 20 {
		return "LOW"
	}
	if coverage < 0.8 || completedOutcomes < 50 {
		return "MEDIUM"
	}
	return "HIGH"
}

// gateQualityWindowStart anchors a window: 24H → start of the current UTC
// day; 7D → now−7d truncated to the UTC day. Truncation keeps every
// aggregate row day-aligned so ON CONFLICT keys stay stable across re-runs.
func gateQualityWindowStart(window string, now time.Time) (time.Time, error) {
	switch window {
	case gateQualityWindow24H:
		return now.UTC().Truncate(24 * time.Hour), nil
	case gateQualityWindow7D:
		return now.UTC().Add(-7 * 24 * time.Hour).Truncate(24 * time.Hour), nil
	default:
		return time.Time{}, fmt.Errorf(
			"gate quality: unknown window %q (want %s or %s)",
			window, gateQualityWindow24H, gateQualityWindow7D)
	}
}

// gateQualityKey groups aggregates by gate × regime.
type gateQualityKey struct {
	gate   string
	regime string
}

// gateQualityAgg accumulates one (gate, regime) group across the four
// source queries before the upsert.
type gateQualityAgg struct {
	gate   string
	regime string
	// Decision/episode counts (entry_decisions / entry_episodes).
	episodes  int
	decisions int
	// Coverage numerator/denominator parts (shadow_coverage_log).
	shadowCreated int
	skipped       int
	neither       int // shadow_created=FALSE with NO skip reason — an anomaly worth a note
	skipReasons   map[string]int
	// Shadow outcome state machine (shadow_candidates).
	completed    int // calc_state='DONE' + terminal pos_state
	openOutcomes int // pos_state='OPEN'
	// Model ±$ of blocked entries, decomposed (§6 honesty contract).
	blockedPnL     float64
	partsGrid      float64
	partsInventory float64
	partsFees      float64
	partsFunding   float64
	partsSlippage  float64
}

// gateQualityPair is one model-vs-actual calibration observation from the
// ALLOW control group (§6): the shadow engine's outcome for an admitted
// episode vs the deployed bot's realized PnL.
type gateQualityPair struct {
	direction string
	model     float64
	actual    float64
}

// ComputeGateQuality aggregates one window ("24H" | "7D") into
// gate_quality_daily. Every step errors loudly (the hook logs WARN); rows
// are upserted with ON CONFLICT (window, window_start, gate, regime) so
// re-runs and overlapping daily passes never double-count.
func ComputeGateQuality(ctx context.Context, worker *Worker, window string) error {
	if worker == nil || worker.db == nil {
		return errors.New("gate quality: nil worker or database pool")
	}
	windowStart, err := gateQualityWindowStart(window, time.Now())
	if err != nil {
		return err
	}

	aggs := make(map[gateQualityKey]*gateQualityAgg)
	group := func(gate, regime string) *gateQualityAgg {
		key := gateQualityKey{gate: gate, regime: regime}
		agg, ok := aggs[key]
		if !ok {
			agg = &gateQualityAgg{
				gate:        gateQualityTruncateGate(gate),
				regime:      regime,
				skipReasons: make(map[string]int),
			}
			aggs[key] = agg
		}
		return agg
	}

	// 1) Rejections: deciding gate × episode regime.
	rows, err := worker.db.Query(ctx, `
		SELECT d.code,
		       COALESCE(e.regime, 'UNKNOWN'),
		       COUNT(*),
		       COUNT(DISTINCT d.episode_id)
		FROM entry_decisions d
		LEFT JOIN entry_episodes e ON e.id = d.episode_id
		WHERE d.outcome = 'REJECT'
		  AND COALESCE(d.decision_at, d.created_at) >= $1
		GROUP BY d.code, COALESCE(e.regime, 'UNKNOWN')
	`, windowStart)
	if err != nil {
		return fmt.Errorf("gate quality %s: load rejections: %w", window, err)
	}
	for rows.Next() {
		var gate, regime string
		var decisions, episodes int
		if err := rows.Scan(&gate, &regime, &decisions, &episodes); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan rejections: %w", window, err)
		}
		agg := group(gate, regime)
		agg.decisions = decisions
		agg.episodes = episodes
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate rejections: %w", window, err)
	}
	rows.Close()

	// 2) ALLOW control group: one synthetic ALLOW_CHAIN row per regime of
	// episodes the chain admitted. Decision count keeps the window filter;
	// the episode itself counts once regardless of attempt count (§4.1).
	rows, err = worker.db.Query(ctx, `
		SELECT COALESCE(e.regime, 'UNKNOWN'),
		       COUNT(DISTINCT e.id),
		       COUNT(d.id) FILTER (
		           WHERE COALESCE(d.decision_at, d.created_at) >= $1
		       )
		FROM entry_episodes e
		LEFT JOIN entry_decisions d ON d.episode_id = e.id
		WHERE e.disposition = 'ALLOWED'
		  AND e.last_seen >= $1
		GROUP BY COALESCE(e.regime, 'UNKNOWN')
	`, windowStart)
	if err != nil {
		return fmt.Errorf("gate quality %s: load allowed episodes: %w", window, err)
	}
	for rows.Next() {
		var regime string
		var episodes, decisions int
		if err := rows.Scan(&regime, &episodes, &decisions); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan allowed episodes: %w", window, err)
		}
		agg := group(gateQualityAllowChain, regime)
		agg.episodes = episodes
		agg.decisions = decisions
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate allowed episodes: %w", window, err)
	}
	rows.Close()

	// 3a) Coverage totals per group. A coverage row of an eventually-allowed
	// episode belongs to the ALLOW_CHAIN control group, not the gate that
	// rejected an earlier attempt. The not-created mass is provisionally
	// "skipped"; pass 3b re-splits it — rows WITH a skip reason stay skipped
	// (the coverage denominator), the reasonless remainder becomes "neither"
	// (an anomaly worth a note, never silently dropped).
	rows, err = worker.db.Query(ctx, `
		WITH cov AS (
		    SELECT CASE WHEN e.disposition = 'ALLOWED' THEN $2 ELSE d.code END AS gate,
		           COALESCE(e.regime, 'UNKNOWN') AS regime,
		           scl.shadow_created
		    FROM shadow_coverage_log scl
		    JOIN entry_decisions d ON d.id = scl.decision_id
		    LEFT JOIN entry_episodes e ON e.id = COALESCE(scl.episode_id, d.episode_id)
		    WHERE scl.created_at >= $1
		      AND (d.outcome = 'REJECT' OR e.disposition = 'ALLOWED')
		)
		SELECT gate, regime,
		       COUNT(*) FILTER (WHERE shadow_created),
		       COUNT(*) FILTER (WHERE NOT shadow_created)
		FROM cov
		GROUP BY gate, regime
	`, windowStart, gateQualityAllowChain)
	if err != nil {
		return fmt.Errorf("gate quality %s: load coverage: %w", window, err)
	}
	for rows.Next() {
		var gate, regime string
		var created, notCreated int
		if err := rows.Scan(&gate, &regime, &created, &notCreated); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan coverage: %w", window, err)
		}
		agg := group(gate, regime)
		agg.shadowCreated = created
		agg.neither = notCreated
		agg.skipped = 0
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate coverage: %w", window, err)
	}
	rows.Close()

	// 3b) Skip-reason histogram per group (the coverage denominator makeup).
	rows, err = worker.db.Query(ctx, `
		SELECT CASE WHEN e.disposition = 'ALLOWED' THEN $2 ELSE d.code END AS gate,
		       COALESCE(e.regime, 'UNKNOWN') AS regime,
		       scl.skip_reason,
		       COUNT(*)
		FROM shadow_coverage_log scl
		JOIN entry_decisions d ON d.id = scl.decision_id
		LEFT JOIN entry_episodes e ON e.id = COALESCE(scl.episode_id, d.episode_id)
		WHERE scl.created_at >= $1
		  AND NOT scl.shadow_created
		  AND scl.skip_reason IS NOT NULL
		  AND (d.outcome = 'REJECT' OR e.disposition = 'ALLOWED')
		GROUP BY 1, 2, scl.skip_reason
	`, windowStart, gateQualityAllowChain)
	if err != nil {
		return fmt.Errorf("gate quality %s: load skip reasons: %w", window, err)
	}
	for rows.Next() {
		var gate, regime, reason string
		var count int
		if err := rows.Scan(&gate, &regime, &reason, &count); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan skip reasons: %w", window, err)
		}
		agg := group(gate, regime)
		agg.skipReasons[reason] += count
		agg.skipped += count
		agg.neither -= count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate skip reasons: %w", window, err)
	}
	rows.Close()
	for _, agg := range aggs {
		if agg.neither < 0 {
			agg.neither = 0
		}
	}

	// 4) Shadow outcomes of rejected episodes: completed/open counts, the
	// blocked model ±$ and its §6 decomposition. pos_state terminal +
	// calc_state='DONE' is a completed outcome; anything still OPEN is
	// open_outcomes; terminal-but-uncalculated rows count in neither (the
	// observation exists, the number does not — it is not a zero).
	rows, err = worker.db.Query(ctx, `
		SELECT d.code,
		       COALESCE(e.regime, 'UNKNOWN'),
		       COUNT(*) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ),
		       COUNT(*) FILTER (WHERE s.pos_state = 'OPEN'),
		       COALESCE(SUM(s.outcome_pnl_usdt) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0),
		       COALESCE(SUM(COALESCE(s.pnl_parts->>'grid', '0')::numeric) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0),
		       COALESCE(SUM(COALESCE(s.pnl_parts->>'inventory', '0')::numeric) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0),
		       COALESCE(SUM(COALESCE(s.pnl_parts->>'fees', '0')::numeric) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0),
		       COALESCE(SUM(COALESCE(s.pnl_parts->>'funding', '0')::numeric) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0),
		       COALESCE(SUM(COALESCE(s.pnl_parts->>'slippage', '0')::numeric) FILTER (
		           WHERE s.calc_state = 'DONE'
		             AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		       ), 0)
		FROM shadow_candidates s
		JOIN entry_decisions d ON d.id = s.decision_id
		LEFT JOIN entry_episodes e ON e.id = COALESCE(s.episode_id, d.episode_id)
		WHERE s.captured_at >= $1
		  AND d.outcome = 'REJECT'
		  AND COALESCE(e.disposition, 'REJECTED_STILL') <> 'ALLOWED'
		GROUP BY d.code, COALESCE(e.regime, 'UNKNOWN')
	`, windowStart)
	if err != nil {
		return fmt.Errorf("gate quality %s: load shadow outcomes: %w", window, err)
	}
	for rows.Next() {
		var gate, regime string
		var completed, openOutcomes int
		var pnl, grid, inventory, fees, funding, slippage float64
		if err := rows.Scan(
			&gate, &regime, &completed, &openOutcomes,
			&pnl, &grid, &inventory, &fees, &funding, &slippage,
		); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan shadow outcomes: %w", window, err)
		}
		agg := group(gate, regime)
		agg.completed = completed
		agg.openOutcomes = openOutcomes
		agg.blockedPnL = pnl
		agg.partsGrid = grid
		agg.partsInventory = inventory
		agg.partsFees = fees
		agg.partsFunding = funding
		agg.partsSlippage = slippage
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate shadow outcomes: %w", window, err)
	}
	rows.Close()

	// 5) Calibration pairs from the ALLOW control group: model outcome vs
	// the deployed bot's realized PnL (episode → decision.ref_id →
	// grid_bots.candidate_id). uuid::text comparison — ref_id is TEXT and
	// may hold non-UUID ids on non-scanner paths.
	rows, err = worker.db.Query(ctx, `
		SELECT COALESCE(NULLIF(e.direction, ''), NULLIF(s.direction, ''), 'UNKNOWN'),
		       s.outcome_pnl_usdt,
		       g.realized_pnl_usdt
		FROM shadow_candidates s
		JOIN entry_decisions d ON d.id = s.decision_id
		LEFT JOIN entry_episodes e ON e.id = COALESCE(s.episode_id, d.episode_id)
		JOIN grid_bots g ON d.ref_id IS NOT NULL AND g.candidate_id::TEXT = d.ref_id
		WHERE s.captured_at >= $1
		  AND d.outcome = 'ALLOW'
		  AND e.disposition = 'ALLOWED'
		  AND s.calc_state = 'DONE'
		  AND s.pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		  AND s.outcome_pnl_usdt IS NOT NULL
		  AND g.realized_pnl_usdt IS NOT NULL
	`, windowStart)
	if err != nil {
		return fmt.Errorf("gate quality %s: load calibration pairs: %w", window, err)
	}
	var pairs []gateQualityPair
	for rows.Next() {
		var pair gateQualityPair
		if err := rows.Scan(&pair.direction, &pair.model, &pair.actual); err != nil {
			rows.Close()
			return fmt.Errorf("gate quality %s: scan calibration pair: %w", window, err)
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("gate quality %s: iterate calibration pairs: %w", window, err)
	}
	rows.Close()

	calibration := buildGateCalibration(pairs)
	calibrationJSON, err := json.Marshal(calibration)
	if err != nil {
		return fmt.Errorf("gate quality %s: encode calibration: %w", window, err)
	}

	// Deterministic upsert order (gate, regime).
	keys := make([]gateQualityKey, 0, len(aggs))
	for key := range aggs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].gate != keys[j].gate {
			return keys[i].gate < keys[j].gate
		}
		return keys[i].regime < keys[j].regime
	})

	for _, key := range keys {
		agg := aggs[key]
		coverage := 0.0
		if denominator := agg.shadowCreated + agg.skipped; denominator > 0 {
			coverage = float64(agg.shadowCreated) / float64(denominator)
		}
		proof := ProofStrength(coverage, agg.completed)
		// No completed outcomes → the blocked ±$ is UNKNOWN, not zero.
		var blocked *float64
		if agg.completed > 0 {
			value := gateValueRound(agg.blockedPnL)
			blocked = &value
		}
		parts, err := json.Marshal(map[string]float64{
			"grid":      gateValueRound(agg.partsGrid),
			"inventory": gateValueRound(agg.partsInventory),
			"fees":      gateValueRound(agg.partsFees),
			"funding":   gateValueRound(agg.partsFunding),
			"slippage":  gateValueRound(agg.partsSlippage),
		})
		if err != nil {
			return fmt.Errorf("gate quality %s: encode pnl parts: %w", window, err)
		}
		skipReasons, err := json.Marshal(agg.skipReasons)
		if err != nil {
			return fmt.Errorf("gate quality %s: encode skip reasons: %w", window, err)
		}
		notes, err := json.Marshal(map[string]int{
			"shadow_created": agg.shadowCreated,
			"skipped":        agg.skipped,
			"skip_neither":   agg.neither,
		})
		if err != nil {
			return fmt.Errorf("gate quality %s: encode notes: %w", window, err)
		}
		if _, err := worker.db.Exec(ctx, `
			INSERT INTO gate_quality_daily (
				window_kind, window_start, gate, regime, episodes, decisions,
				coverage, skip_reasons, completed_outcomes, open_outcomes,
				blocked_model_pnl, blocked_model_pnl_parts, proof_strength,
				calibration, notes
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7::numeric, $8::jsonb, $9, $10,
				$11::numeric, $12::jsonb, $13,
				$14::jsonb, $15::jsonb
			)
			ON CONFLICT (window_kind, window_start, gate, regime) DO UPDATE SET
				computed_at = NOW(),
				episodes = EXCLUDED.episodes,
				decisions = EXCLUDED.decisions,
				coverage = EXCLUDED.coverage,
				skip_reasons = EXCLUDED.skip_reasons,
				completed_outcomes = EXCLUDED.completed_outcomes,
				open_outcomes = EXCLUDED.open_outcomes,
				blocked_model_pnl = EXCLUDED.blocked_model_pnl,
				blocked_model_pnl_parts = EXCLUDED.blocked_model_pnl_parts,
				proof_strength = EXCLUDED.proof_strength,
				calibration = EXCLUDED.calibration,
				notes = EXCLUDED.notes
		`,
			window, windowStart, agg.gate, agg.regime, agg.episodes, agg.decisions,
			gateValueRound(coverage), string(skipReasons), agg.completed, agg.openOutcomes,
			blocked, string(parts), proof,
			string(calibrationJSON), string(notes),
		); err != nil {
			return fmt.Errorf("gate quality %s: upsert %s/%s: %w",
				window, agg.gate, agg.regime, err)
		}
	}
	return nil
}

// RunDueGateQuality is the worker hook: once per ~day (after gate_value in
// the manage pass) it recomputes both windows and queues the Telegram
// summary. The throttle marker is durable (bot_execution_events), so it
// holds across restarts, empty windows and disabled Telegram. Best-effort:
// every failure logs WARN and returns — the next pass retries; nothing here
// may disturb the manage loop that calls it.
func RunDueGateQuality(ctx context.Context, worker *Worker) {
	if worker == nil || worker.db == nil {
		return
	}
	if !gateQualityEnabled(ctx, worker) {
		return
	}
	var sent bool
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bot_execution_events
			WHERE event_type = $1 AND bot_id = $2
			  AND created_at > NOW() - INTERVAL '20 hours'
		)
	`, gateQualityMarkerEvent, gateQualityMarkerBot).Scan(&sent); err != nil {
		logGateQualityWarn(worker, "gate quality: due check failed", err)
		return
	}
	if sent {
		return
	}

	// Both windows recompute on every attempt: the upsert is idempotent, so
	// a partial failure simply retries the whole pair on the next pass.
	if err := ComputeGateQuality(ctx, worker, gateQualityWindow24H); err != nil {
		logGateQualityWarn(worker, "gate quality: 24H compute failed", err)
		return
	}
	if err := ComputeGateQuality(ctx, worker, gateQualityWindow7D); err != nil {
		logGateQualityWarn(worker, "gate quality: 7D compute failed", err)
		return
	}

	start24, err := gateQualityWindowStart(gateQualityWindow24H, time.Now())
	if err != nil {
		logGateQualityWarn(worker, "gate quality: window anchor failed", err)
		return
	}
	start7, err := gateQualityWindowStart(gateQualityWindow7D, time.Now())
	if err != nil {
		logGateQualityWarn(worker, "gate quality: window anchor failed", err)
		return
	}
	rows, err := worker.db.Query(ctx, `
		SELECT window_kind, gate, regime, episodes, coverage,
		       completed_outcomes, blocked_model_pnl, proof_strength
		FROM gate_quality_daily
		WHERE (window_kind = $1 AND window_start = $2)
		   OR (window_kind = $3 AND window_start = $4)
	`, gateQualityWindow24H, start24, gateQualityWindow7D, start7)
	if err != nil {
		logGateQualityWarn(worker, "gate quality: summary load failed", err)
		return
	}
	var qualityRows []gateQualityRow
	for rows.Next() {
		var row gateQualityRow
		if err := rows.Scan(
			&row.windowKind, &row.gate, &row.regime, &row.episodes,
			&row.coverage, &row.completed, &row.blocked, &row.proof,
		); err != nil {
			rows.Close()
			logGateQualityWarn(worker, "gate quality: summary scan failed", err)
			return
		}
		qualityRows = append(qualityRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		logGateQualityWarn(worker, "gate quality: summary iterate failed", err)
		return
	}
	rows.Close()

	// Lost-observation counter lives in decision_intelligence.go (monotonic
	// process-lifetime count of observation writes that never reached the
	// database) — the summary must surface it (plan invariant 2: an
	// analytics failure is a lost observation, never silence).
	lost := LostObservations()

	text := buildGateQualitySummary(qualityRows, int(lost))
	if err := QueueTelegramEvent(ctx, worker.db, gateQualityEvent, map[string]any{
		"message": text,
	}); err != nil {
		logGateQualityWarn(worker, "gate quality: summary queue failed", err)
		return
	}
	if err := LogBotEvent(ctx, worker.db, gateQualityMarkerBot, 0, "SYSTEM", "",
		gateQualityMarkerEvent, nil, nil, map[string]any{
			"rows":              len(qualityRows),
			"lost_observations": lost,
			"windows":           []string{gateQualityWindow24H, gateQualityWindow7D},
		}); err != nil {
		// Marker missing → the next pass re-sends the summary; visible in
		// logs rather than silently skipped.
		logGateQualityWarn(worker, "gate quality: marker write failed", err)
	}
	if worker.logger != nil {
		worker.logger.Info("gate quality summary sent",
			"component", "autogrid_worker",
			"rows", len(qualityRows),
			"lost_observations", lost)
	}
}

// gateQualityRow is the read-back projection of gate_quality_daily the
// summary renders.
type gateQualityRow struct {
	windowKind string
	gate       string
	regime     string
	episodes   int
	coverage   float64
	completed  int
	blocked    *float64
	proof      string
}

// buildGateQualitySummary renders the daily Telegram text: per window the
// top-3 gates by |blocked_model_pnl| with regime/episodes/coverage/proof, a
// "not enough data" line for LOW-proof gates with zero completed outcomes,
// and the lost-observation counter. Pure for unit tests.
func buildGateQualitySummary(rows []gateQualityRow, lostObservations int) string {
	var builder strings.Builder
	for windowIndex, window := range []string{gateQualityWindow24H, gateQualityWindow7D} {
		if windowIndex > 0 {
			builder.WriteString("\n")
		}
		fmt.Fprintf(&builder, "🧪 Качество гейтов (%s):\n", window)
		var windowRows []gateQualityRow
		for _, row := range rows {
			if row.windowKind == window {
				windowRows = append(windowRows, row)
			}
		}
		if len(windowRows) == 0 {
			builder.WriteString("— нет наблюдений\n")
			continue
		}
		top := append([]gateQualityRow(nil), windowRows...)
		sort.SliceStable(top, func(i, j int) bool {
			left, right := gateQualityAbsPnL(top[i].blocked), gateQualityAbsPnL(top[j].blocked)
			if left != right {
				return left > right
			}
			if top[i].gate != top[j].gate {
				return top[i].gate < top[j].gate
			}
			return top[i].regime < top[j].regime
		})
		if len(top) > 3 {
			top = top[:3]
		}
		for rank, row := range top {
			fmt.Fprintf(&builder, "%d. %s [%s] — эп. %d, cov %d%%, model %s, proof %s\n",
				rank+1, row.gate, row.regime, row.episodes,
				int(math.Round(row.coverage*100)),
				gateQualityPnLText(row.blocked), row.proof)
		}
		var insufficient []string
		ordered := append([]gateQualityRow(nil), windowRows...)
		sort.Slice(ordered, func(i, j int) bool {
			if ordered[i].gate != ordered[j].gate {
				return ordered[i].gate < ordered[j].gate
			}
			return ordered[i].regime < ordered[j].regime
		})
		for _, row := range ordered {
			if row.proof == "LOW" && row.completed == 0 {
				insufficient = append(insufficient, fmt.Sprintf("%s [%s]", row.gate, row.regime))
			}
		}
		if len(insufficient) > 0 {
			if len(insufficient) > 6 {
				insufficient = append(insufficient[:6], fmt.Sprintf("+%d ещё", len(insufficient)-6))
			}
			fmt.Fprintf(&builder, "мало данных (LOW, 0 исходов): %s\n",
				strings.Join(insufficient, ", "))
		}
	}
	fmt.Fprintf(&builder, "потерянные наблюдения: %d", lostObservations)
	return builder.String()
}

// gateQualityAbsPnL is |blocked ±$| for ranking; NULL (unknown) ranks as 0.
func gateQualityAbsPnL(blocked *float64) float64 {
	if blocked == nil {
		return 0
	}
	return math.Abs(*blocked)
}

// gateQualityPnLText formats the blocked ±$ for the summary line.
func gateQualityPnLText(blocked *float64) string {
	if blocked == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f", *blocked)
}

// buildGateCalibration turns the ALLOW control-group pairs into the §6
// calibration verdict: MAE and sign-match share overall and per direction.
// Below gateQualityMinPairs pairs the only honest answer is "insufficient".
func buildGateCalibration(pairs []gateQualityPair) map[string]any {
	if len(pairs) < gateQualityMinPairs {
		return map[string]any{"pairs": len(pairs), "status": "insufficient"}
	}
	byDirection := make(map[string][]gateQualityPair)
	for _, pair := range pairs {
		byDirection[pair.direction] = append(byDirection[pair.direction], pair)
	}
	directionStats := make(map[string]any, len(byDirection))
	for direction, group := range byDirection {
		directionStats[direction] = gateQualityCalibrationStats(group)
	}
	overall := gateQualityCalibrationStats(pairs)
	return map[string]any{
		"pairs":        len(pairs),
		"status":       "ok",
		"mae":          overall["mae"], // map[string]float64: value, not assertion
		"sign_match":   overall["sign_match"],
		"by_direction": directionStats,
	}
}

// gateQualityCalibrationStats computes {pairs, mae, sign_match} over one
// pair set; sign(0)=0 — a flat model vs a flat actual still "matches".
func gateQualityCalibrationStats(pairs []gateQualityPair) map[string]float64 {
	sumAbsError, signMatches := 0.0, 0
	for _, pair := range pairs {
		sumAbsError += math.Abs(pair.model - pair.actual)
		if gateQualitySign(pair.model) == gateQualitySign(pair.actual) {
			signMatches++
		}
	}
	count := float64(len(pairs))
	if count == 0 {
		return map[string]float64{"pairs": 0, "mae": 0, "sign_match": 0}
	}
	return map[string]float64{
		"pairs":      count,
		"mae":        gateValueRound(sumAbsError / count),
		"sign_match": gateValueRound(float64(signMatches) / count),
	}
}

// gateQualitySign is the three-valued sign used by the sign-match share.
func gateQualitySign(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	default:
		return 0
	}
}

// gateQualityTruncateGate keeps entry_decisions.code (VARCHAR(64)) inside
// gate_quality_daily.gate (VARCHAR(32)) without failing the upsert.
func gateQualityTruncateGate(gate string) string {
	runes := []rune(gate)
	if len(runes) > gateQualityGateLen {
		return string(runes[:gateQualityGateLen])
	}
	return gate
}

// gateQualityEnabled mirrors shadowPortfolioEnabled: default true, and a
// broken flag read must not silently kill the daily summary.
func gateQualityEnabled(ctx context.Context, worker *Worker) bool {
	enabled := true
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = $1), true)
	`, gateQualityFlagKey).Scan(&enabled); err != nil {
		return true
	}
	return enabled
}

// logGateQualityWarn never panics on bare unit workers (nil logger).
func logGateQualityWarn(worker *Worker, message string, cause error) {
	if worker == nil || worker.logger == nil {
		return
	}
	worker.logger.Warn(message, "component", "autogrid_worker", "error", cause)
}
