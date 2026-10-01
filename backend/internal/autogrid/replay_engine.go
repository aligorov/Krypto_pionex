package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Replay experiments over RECORDED decisions (v2.0.184, plan §6/§7 screen 2/
// §8 stage 3, migration 0062). The engine re-judges stored entry_decisions
// gate traces against a hypothetical override — "what the chain would have
// said with RV at 1.8" — and measures the verdict-level effect against the
// completed shadow outcomes of the same episodes. It is a pure observer:
// no exchange call, no settings mutation, no order, no bot. A run is an
// immutable row; a new experiment never overwrites an old one.
//
// Re-playability is honest by construction: a gate whose recorded trace
// lacks the numeric replay triple (value + threshold + direction — today
// only RV writes it: inputs {"ratio":1.62,"threshold":1.5,"direction":"gte"})
// answers NOT_REPLAYABLE(threshold_not_stored) instead of being guessed.
// STRESS/FEE_GATE/ORDERBOOK/KNIFE/CHECK_PARAMS traces carry the numbers
// they decided with but no direction contract, and the shared-market gates
// plus BACKTEST journal a single {gate, verdict, inputs} element — none of
// them is threshold-replayable, and the engine says so instead of pretending.
//
// Zero-override runs are the model validation: they MUST reproduce the
// stored verdicts (plan §8 stage 3 DoD); a recorded triple that contradicts
// its own stored verdict marks the decision NOT_REPLAYABLE(inconsistent_trace).

const (
	// replayFlagKey gates the worker loop (on by default — the flag exists
	// for emergency mute, exactly like shadow_portfolio/backtest_gate).
	replayFlagKey = "replay_experiments"
	// replayBatchSize is the decisions-per-batch contract from the plan
	// (§4.5: heavy computation goes through the worker in batches).
	replayBatchSize = 500
	// replayLease is the worker's claim on a RUNNING run; a crashed worker's
	// lease expires and the next pass resumes from the last committed batch
	// (items are skipped via NOT EXISTS — no duplicates, no double counts).
	replayLease = 10 * time.Minute
	// replayMaxRunsPerPass bounds one invocation of RunDueReplayExperiments
	// so a large backlog cannot wedge the manage pass; the rest waits for
	// the next tick.
	replayMaxRunsPerPass = 16
)

// replay_run_items.verdict vocabulary (migration 0062).
const (
	replayItemReproduced    = "REPRODUCED"
	replayItemChanged       = "CHANGED"
	replayItemNotReplayable = "NOT_REPLAYABLE"
)

// NOT_REPLAYABLE causes (replay_run_items.not_replayable_cause).
const (
	replayCauseGateNotInTrace     = "gate_not_in_trace"
	replayCauseInputsMissing      = "inputs_missing"
	replayCauseThresholdNotStored = "threshold_not_stored"
	replayCauseChainNotRecorded   = "chain_not_recorded"
	replayCauseInconsistentTrace  = "inconsistent_trace"
)

// Gate trace verdict vocabulary used by the re-player. decision_intelligence
// keeps UNKNOWN; the classes the replay branches on live here.
const (
	traceVerdictPass         = "PASS"
	traceVerdictReject       = "REJECT"
	traceVerdictWait         = "WAIT"
	traceVerdictExempt       = "EXEMPT"
	traceVerdictNotEvaluated = "NOT_EVALUATED"
)

// replayExperimentsEnabled reads the feature flag (default on when absent).
func (worker *Worker) replayExperimentsEnabled(ctx context.Context) bool {
	enabled := true
	_ = worker.db.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = $1), true)
	`, replayFlagKey).Scan(&enabled)
	return enabled
}

// ── enqueue ─────────────────────────────────────────────────────────────────

// EnqueueReplayRun creates a QUEUED experiment over the [from, to) decision
// window. baseline is informational (the recorded traces ARE the baseline);
// overrides is {"gate":"RV","params":{"threshold":1.8}} — empty/nil overrides
// mean a VALIDATION run, where the zero override must reproduce the stored
// verdicts or honestly report NOT_REPLAYABLE. The row is immutable once
// written: results land on it, nothing ever rewrites a finished experiment.
func EnqueueReplayRun(ctx context.Context, db *pgxpool.Pool, from, to time.Time, baseline, overrides map[string]any, codeVersion string) (uuid.UUID, error) {
	if !from.Before(to) {
		return uuid.Nil, fmt.Errorf("replay: period_from must be before period_to")
	}
	if db == nil {
		return uuid.Nil, fmt.Errorf("replay: nil pool")
	}
	if baseline == nil {
		baseline = map[string]any{}
	}
	if overrides == nil {
		overrides = map[string]any{}
	}
	baselineJSON, err := json.Marshal(baseline)
	if err != nil {
		return uuid.Nil, fmt.Errorf("replay: marshal baseline: %w", err)
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return uuid.Nil, fmt.Errorf("replay: marshal overrides: %w", err)
	}
	var id string
	if err := db.QueryRow(ctx, `
		INSERT INTO replay_runs (period_from, period_to, baseline, overrides, code_version)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5)
		RETURNING id::TEXT
	`, from, to, string(baselineJSON), string(overridesJSON), codeVersion).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("replay: enqueue run: %w", err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("replay: parse run id %q: %w", id, err)
	}
	return parsed, nil
}

// ── pure re-play core ───────────────────────────────────────────────────────

// ReplayTraceAgainstOverride is the unit-testable heart of the engine: it
// re-judges one recorded gate trace against one override. See replayTraceCore
// for the exact rules; this wrapper is the contract surface.
func ReplayTraceAgainstOverride(trace []GateTraceEntry, baseOutcome string, overrides map[string]any) (replayable bool, newChainOutcome string, chainFollowed bool, notReplayableCause string) {
	replayable, newChainOutcome, chainFollowed, notReplayableCause, _ = replayTraceCore(trace, baseOutcome, overrides)
	return replayable, newChainOutcome, chainFollowed, notReplayableCause
}

// replayTraceCore additionally reports gateVerdictChanged (the OVERRIDDEN
// gate's own verdict flipped) so the run can distinguish "chain outcome
// unchanged because the gate did not flip" from "gate flipped but a later
// recorded REJECT kept the chain at REJECT" — both end at newChainOutcome ==
// baseOutcome, only the first is REPRODUCED.
//
// Rules (contract):
//  0. empty overrides → validation: every entry carrying a full replay
//     triple is recomputed with its OWN stored threshold; a disagreement
//     with the stored verdict is inconsistent_trace. Entries without the
//     triple are not claims and are not verified. A completely empty trace
//     has no chain to validate → chain_not_recorded.
//  1. override gate absent from the trace → NOT_REPLAYABLE(gate_not_in_trace).
//  2. target gate without the replay triple → NOT_REPLAYABLE(inputs_missing /
//     threshold_not_stored). EXEMPT answers threshold_not_stored (the
//     exemption decided, not the threshold); NOT_EVALUATED answers
//     inputs_missing (the gate never measured anything).
//  3. the gate verdict is recomputed as: direction "gte" → value >= threshold
//     is the REJECT class, "lte" → value <= threshold is; anything else in
//     the recorded direction is inputs_missing (incomplete replay contract).
//  4. verdict unchanged → newChainOutcome = baseOutcome, chainFollowed = true
//     (the recorded chain already ran to its end).
//  5. REJECT→PASS → walk the REMAINDER of the trace: no remainder at all →
//     NOT_REPLAYABLE(chain_not_recorded); any NOT_EVALUATED after the gate →
//     chainFollowed = false and the new outcome is unknown (""); a recorded
//     REJECT later in the remainder keeps the chain REJECT; otherwise the
//     chain outcome is ALLOW, or WAIT when a subsequent gate deferred.
//  6. PASS→REJECT → the chain is REJECT the moment any gate rejects:
//     newChainOutcome = REJECT, chainFollowed = true.
func replayTraceCore(trace []GateTraceEntry, baseOutcome string, overrides map[string]any) (replayable bool, newChainOutcome string, chainFollowed bool, notReplayableCause string, gateVerdictChanged bool) {
	gateName := ""
	if overrides != nil {
		gateName, _ = overrides["gate"].(string)
		gateName = strings.ToUpper(strings.TrimSpace(gateName))
	}

	// Rule 0 — validation mode.
	if gateName == "" {
		if len(trace) == 0 {
			return false, "", false, replayCauseChainNotRecorded, false
		}
		for _, entry := range trace {
			value, threshold, direction, _, ok := replayGateFacts(entry)
			if !ok {
				continue // triple incomplete: verification impossible, not contradicted
			}
			verdict := strings.ToUpper(strings.TrimSpace(entry.Verdict))
			if verdict != traceVerdictPass && verdict != traceVerdictReject {
				continue // EXEMPT/WAIT/NOT_EVALUATED were not threshold decisions
			}
			if replayThresholdRejects(value, threshold, direction) != (verdict == traceVerdictReject) {
				return false, "", false, replayCauseInconsistentTrace, false
			}
		}
		return true, baseOutcome, true, "", false
	}

	// Rule 1 — the overridden gate must be in the recorded trace.
	idx := -1
	for i, entry := range trace {
		if strings.EqualFold(strings.TrimSpace(entry.Gate), gateName) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, "", false, replayCauseGateNotInTrace, false
	}
	target := trace[idx]
	targetVerdict := strings.ToUpper(strings.TrimSpace(target.Verdict))

	// Rule 2 — gates the threshold did not decide cannot be threshold-overridden.
	if targetVerdict == traceVerdictExempt {
		return false, "", false, replayCauseThresholdNotStored, false
	}
	if targetVerdict == traceVerdictNotEvaluated {
		return false, "", false, replayCauseInputsMissing, false
	}
	value, storedThreshold, direction, cause, ok := replayGateFacts(target)
	if !ok {
		return false, "", false, cause, false
	}

	newThreshold, hasOverride := replayOverrideThreshold(overrides)
	if !hasOverride {
		// An override without a numeric threshold has nothing to apply —
		// the same honest "no threshold" answer as a trace without one.
		return false, "", false, replayCauseThresholdNotStored, false
	}
	_ = storedThreshold // validation uses it; here the override replaces it

	// Rule 3 — recompute the gate verdict under the override.
	rejectsNow := replayThresholdRejects(value, newThreshold, direction)
	storedRejected := targetVerdict == traceVerdictReject

	// Rule 4 — unchanged gate verdict: the recorded chain already decided.
	if rejectsNow == storedRejected {
		return true, baseOutcome, true, "", false
	}

	// Rule 6 — PASS→REJECT: one rejecting gate is enough.
	if rejectsNow {
		return true, entryOutcomeReject, true, "", true
	}

	// Rule 5 — REJECT→PASS: the recorded remainder is the only admissible
	// evidence about the rest of the chain (plan invariant: no simulation).
	remainder := trace[idx+1:]
	if len(remainder) == 0 {
		return false, "", false, replayCauseChainNotRecorded, true
	}
	outcome := entryOutcomeAllow
	followed := true
	for _, entry := range remainder {
		verdict := strings.ToUpper(strings.TrimSpace(entry.Verdict))
		switch verdict {
		case traceVerdictReject:
			// A recorded later REJECT keeps the chain REJECT regardless of
			// anything recorded before it (first reject wins in a chain).
			return true, entryOutcomeReject, true, "", true
		case traceVerdictWait:
			outcome = entryOutcomeWait
		case traceVerdictNotEvaluated:
			followed = false
		}
	}
	if !followed {
		return true, "", false, "", true
	}
	return true, outcome, true, "", true
}

// replayGateFacts extracts the threshold-replay triple from one trace
// entry: the compared VALUE, the THRESHOLD and the comparison DIRECTION.
// The recorder puts everything a threshold gate decided with inside Inputs
// (RV: {"ratio":1.62,"threshold":1.5,"direction":"gte"}); the entry-level
// Threshold field is honored when set. VALUE is read from inputs["value"]
// (the generic contract key) with inputs["ratio"] as the recorded RV key.
// A zero threshold means "absent" — no gate of this fleet uses 0 as a
// working threshold.
func replayGateFacts(entry GateTraceEntry) (value, threshold float64, direction string, cause string, ok bool) {
	if len(entry.Inputs) == 0 {
		return 0, 0, "", replayCauseInputsMissing, false
	}
	threshold = entry.Threshold
	if threshold == 0 {
		if t, alright := replayFloat(entry.Inputs["threshold"]); alright {
			threshold = t
		}
	}
	if threshold == 0 {
		return 0, 0, "", replayCauseThresholdNotStored, false
	}
	value, alright := replayFloat(entry.Inputs["value"])
	if !alright {
		value, alright = replayFloat(entry.Inputs["ratio"])
	}
	if !alright {
		return 0, 0, "", replayCauseInputsMissing, false
	}
	rawDirection, _ := entry.Inputs["direction"].(string)
	switch strings.ToLower(strings.TrimSpace(rawDirection)) {
	case "gte":
		direction = "gte"
	case "lte":
		direction = "lte"
	default:
		// A threshold without a recorded comparison direction is not a
		// replayable contract — which side rejects was never pinned.
		return 0, 0, "", replayCauseInputsMissing, false
	}
	return value, threshold, direction, "", true
}

// replayThresholdRejects applies the recorded comparison semantics:
// "gte" → value >= threshold is the REJECT class, "lte" → value <= threshold.
func replayThresholdRejects(value, threshold float64, direction string) bool {
	if direction == "gte" {
		return value >= threshold
	}
	return value <= threshold
}

// replayOverrideThreshold reads the override's new threshold from
// overrides["params"]["threshold"] (the contract shape), falling back to a
// top-level "threshold" key for operator convenience.
func replayOverrideThreshold(overrides map[string]any) (float64, bool) {
	if overrides == nil {
		return 0, false
	}
	if params, isMap := overrides["params"].(map[string]any); isMap {
		if f, alright := replayFloat(params["threshold"]); alright {
			return f, true
		}
	}
	f, alright := replayFloat(overrides["threshold"])
	return f, alright
}

// replayFloat coerces a stored trace/override number into a float64.
// Traces round-trip through jsonb (→ float64), but in-memory callers may
// hand ints or json.Number; parseable numeric strings are accepted, NaN/Inf
// are not (a failed indicator computation is a data-quality defect, not a
// comparable value).
func replayFloat(raw any) (float64, bool) {
	var f float64
	switch x := raw.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	case json.Number:
		parsed, err := x.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	case string:
		parsed, err := parseReplayFloatString(x)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// parseReplayFloatString parses the numeric-string form ("1.8", " 1.8 ").
func parseReplayFloatString(s string) (float64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("replay: empty numeric string")
	}
	return strconv.ParseFloat(trimmed, 64)
}

// ── worker: claim → batch-process → finalize ───────────────────────────────

// replayClaimedRun is one leased experiment waiting to be processed.
type replayClaimedRun struct {
	id         string
	periodFrom time.Time
	periodTo   time.Time
	overrides  map[string]any
}

// RunDueReplayExperiments is the worker hook: claim QUEUED runs (or RUNNING
// runs whose lease expired — crash recovery, plan §9 "restart mid-processing
// resumes via lease") and process each in batches of replayBatchSize
// decisions. Idempotent by construction: a re-taken run skips decisions that
// already have an item row (NOT EXISTS), so resumed batches neither
// duplicate items nor double-count the effect. Replay never calls the
// exchange, never touches settings, never trades — it only reads recorded
// decisions and writes run rows.
func RunDueReplayExperiments(ctx context.Context, worker *Worker) {
	if worker == nil || worker.db == nil {
		return
	}
	if !worker.replayExperimentsEnabled(ctx) {
		return
	}
	for processed := 0; processed < replayMaxRunsPerPass; processed++ {
		if ctx.Err() != nil {
			return
		}
		run, err := worker.claimReplayRun(ctx)
		if err != nil {
			worker.replayWarn("replay: claim run failed", err)
			return
		}
		if run == nil {
			return // queue drained (for this pass)
		}
		if err := worker.processReplayRun(ctx, run); err != nil {
			// A cancelled context is a shutdown, not a failure: leave the run
			// RUNNING — the lease expires and the next pass resumes from the
			// last committed batch. Anything else fails the run loudly.
			if ctx.Err() != nil {
				return
			}
			worker.replayWarn("replay: run failed", fmt.Errorf("run %s: %w", run.id, err))
			worker.finishReplayRun(ctx, run.id, "FAILED", err.Error(), nil, nil)
		}
	}
}

// claimReplayRun leases the oldest due experiment with
// FOR UPDATE SKIP LOCKED, so concurrent workers never take the same run.
// Returns (nil, nil) when nothing is due.
func (worker *Worker) claimReplayRun(ctx context.Context) (*replayClaimedRun, error) {
	var run replayClaimedRun
	var overridesRaw []byte
	err := worker.db.QueryRow(ctx, `
		UPDATE replay_runs
		SET status = 'RUNNING', lease_until = NOW() + $1::interval, status_reason = ''
		WHERE id = (
			SELECT id FROM replay_runs
			WHERE status = 'QUEUED'
			   OR (status = 'RUNNING' AND lease_until < NOW())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id::TEXT, period_from, period_to, overrides
	`, fmt.Sprintf("%d seconds", int(replayLease.Seconds()))).Scan(
		&run.id, &run.periodFrom, &run.periodTo, &overridesRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	if len(overridesRaw) > 0 {
		if err := json.Unmarshal(overridesRaw, &run.overrides); err != nil {
			// The row is already leased to us — fail it NOW: returning the
			// error alone would leave it RUNNING until the lease expires,
			// gets reclaimed, and fails the same way again, forever.
			worker.finishReplayRun(ctx, run.id, "FAILED", "overrides unreadable: "+err.Error(), nil, nil)
			return nil, nil // run handled; the loop moves to the next one
		}
	}
	if run.overrides == nil {
		run.overrides = map[string]any{}
	}
	return &run, nil
}

// replayDecisionRow is one batch row of the decision window.
type replayDecisionRow struct {
	id         string
	symbol     string
	outcome    string
	episodeID  *string
	traceBytes []byte
}

// replayPendingItem is one computed item waiting for its batch insert.
type replayPendingItem struct {
	decisionID    string
	symbol        string
	verdict       string
	cause         string
	baseOutcome   string
	newOutcome    string
	chainFollowed bool
	episodePnL    *float64
	admissionDiff int // (new ALLOW?) − (base ALLOW?): only ≠0 items carry episodes
	episodeID     *string
}

// processReplayRun walks the window in batches. Every batch first refreshes
// the lease (a run cancelled or stolen mid-processing stops silently), then
// selects decisions NOT yet having an item in this run, computes the
// per-decision verdicts purely in memory, attaches completed episode
// outcomes where the admission actually differs, and inserts the batch.
func (worker *Worker) processReplayRun(ctx context.Context, run *replayClaimedRun) error {
	leaseSeconds := fmt.Sprintf("%d seconds", int(replayLease.Seconds()))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tag, err := worker.db.Exec(ctx, `
			UPDATE replay_runs
			SET lease_until = NOW() + $2::interval
			WHERE id = $1::uuid AND status = 'RUNNING'
		`, run.id, leaseSeconds)
		if err != nil {
			return fmt.Errorf("lease refresh: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// CANCELLED or reclaimed — not ours anymore; stop without
			// touching the status another worker now owns.
			return nil
		}

		rows, err := worker.db.Query(ctx, `
			SELECT d.id::TEXT, d.symbol, d.outcome, d.episode_id::TEXT, d.gate_trace
			FROM entry_decisions d
			WHERE COALESCE(d.decision_at, d.created_at) >= $1
			  AND COALESCE(d.decision_at, d.created_at) < $2
			  AND NOT EXISTS (
			      SELECT 1 FROM replay_run_items i
			      WHERE i.run_id = $3::uuid AND i.decision_id = d.id
			  )
			ORDER BY COALESCE(d.decision_at, d.created_at), d.id
			LIMIT $4
		`, run.periodFrom, run.periodTo, run.id, replayBatchSize)
		if err != nil {
			return fmt.Errorf("batch select: %w", err)
		}
		batch := make([]replayDecisionRow, 0, replayBatchSize)
		for rows.Next() {
			var r replayDecisionRow
			if err := rows.Scan(&r.id, &r.symbol, &r.outcome, &r.episodeID, &r.traceBytes); err != nil {
				rows.Close()
				return fmt.Errorf("batch scan: %w", err)
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("batch rows: %w", err)
		}
		if len(batch) == 0 {
			break // window fully processed
		}

		items := make([]replayPendingItem, 0, len(batch))
		for _, r := range batch {
			items = append(items, worker.computeReplayItem(run, r))
		}
		if err := worker.attachEpisodeOutcomes(ctx, items); err != nil {
			return err
		}
		if err := worker.insertReplayItems(ctx, run.id, items); err != nil {
			return err
		}
		if len(batch) < replayBatchSize {
			break // final partial batch — avoid one extra empty select
		}
	}

	stats, effect, err := worker.finalizeReplayRunStats(ctx, run.id)
	if err != nil {
		return err
	}
	worker.finishReplayRun(ctx, run.id, "DONE", "", stats, effect)
	if worker.logger != nil {
		worker.logger.Info(fmt.Sprintf("replay run DONE: %s", replayStatsLine(stats)),
			"component", "autogrid_worker", "run_id", run.id)
	}
	return nil
}

// computeReplayItem is the pure per-decision step: parse the trace, re-judge
// it against the run's override, derive the item verdict. For validation
// runs (empty overrides) CHANGED is impossible by construction — the
// belt-and-braces guard maps any such artifact to NOT_REPLAYABLE
// (inconsistent_trace) instead of letting a bug pose as a result.
func (worker *Worker) computeReplayItem(run *replayClaimedRun, r replayDecisionRow) replayPendingItem {
	item := replayPendingItem{
		decisionID:  r.id,
		symbol:      r.symbol,
		baseOutcome: r.outcome,
	}
	var trace []GateTraceEntry
	if err := json.Unmarshal(r.traceBytes, &trace); err != nil {
		// A trace that cannot even be parsed has no replayable inputs.
		item.verdict = replayItemNotReplayable
		item.cause = replayCauseInputsMissing
		return item
	}
	replayable, newOutcome, chainFollowed, cause, gateChanged := replayTraceCore(trace, r.outcome, run.overrides)
	item.newOutcome = newOutcome
	item.chainFollowed = chainFollowed
	validation := strings.TrimSpace(replayOverrideGate(run.overrides)) == ""
	switch {
	case !replayable:
		item.verdict = replayItemNotReplayable
		item.cause = cause
	case validation && gateChanged:
		item.verdict = replayItemNotReplayable
		item.cause = replayCauseInconsistentTrace
	case gateChanged:
		item.verdict = replayItemChanged
	default:
		item.verdict = replayItemReproduced
	}
	// Episode evidence only matters where the whole-chain admission differs
	// (a = 1 for ALLOW, plan §6: ΔPnL_proxy = Σ (a_new − a_base) × PnL̂) —
	// and only a REPLAYABLE verdict has a defined a_new: a NOT_REPLAYABLE
	// item must never contribute to the effect (its "" outcome is "unknown",
	// not "blocked"). Validation runs reproduce the base outcome by
	// construction, so their diff is always 0 and they stay effect-free.
	if item.verdict == replayItemReproduced || item.verdict == replayItemChanged {
		base := r.outcome == entryOutcomeAllow
		next := newOutcome == entryOutcomeAllow
		if base != next {
			item.admissionDiff = -1
			if next {
				item.admissionDiff = 1
			}
			item.episodeID = r.episodeID
		}
	}
	return item
}

// replayOverrideGate extracts the override's gate name ("" = validation).
func replayOverrideGate(overrides map[string]any) string {
	if overrides == nil {
		return ""
	}
	gate, _ := overrides["gate"].(string)
	return gate
}

// attachEpisodeOutcomes fetches the completed model outcomes of the episodes
// behind the admission-differring items of one batch: shadow_candidates with
// calc_state='DONE' and a terminal pos_state (CLOSED_TP/CLOSED_SL/
// HORIZON_END), latest per episode. Only already-computed evidence — the
// re-player never simulates a trajectory (plan invariant §2.4).
func (worker *Worker) attachEpisodeOutcomes(ctx context.Context, items []replayPendingItem) error {
	episodeIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.admissionDiff != 0 && item.episodeID != nil && *item.episodeID != "" {
			episodeIDs = append(episodeIDs, *item.episodeID)
		}
	}
	if len(episodeIDs) == 0 {
		return nil
	}
	rows, err := worker.db.Query(ctx, `
		SELECT DISTINCT ON (episode_id) episode_id::TEXT, outcome_pnl_usdt
		FROM shadow_candidates
		WHERE episode_id = ANY($1::uuid[])
		  AND calc_state = 'DONE'
		  AND pos_state IN ('CLOSED_TP', 'CLOSED_SL', 'HORIZON_END')
		ORDER BY episode_id, captured_at DESC
	`, episodeIDs)
	if err != nil {
		return fmt.Errorf("episode outcomes select: %w", err)
	}
	defer rows.Close()
	outcomes := make(map[string]*float64, len(episodeIDs))
	for rows.Next() {
		var episodeID string
		var pnl *float64
		if err := rows.Scan(&episodeID, &pnl); err != nil {
			return fmt.Errorf("episode outcomes scan: %w", err)
		}
		outcomes[episodeID] = pnl
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("episode outcomes rows: %w", err)
	}
	for i := range items {
		if items[i].admissionDiff != 0 && items[i].episodeID != nil {
			if pnl, found := outcomes[*items[i].episodeID]; found {
				items[i].episodePnL = pnl
			}
		}
	}
	return nil
}

// insertReplayItems writes one batch of item rows in a single pgx.Batch —
// per-statement continuous $N, explicit casts, no swallowed errors.
func (worker *Worker) insertReplayItems(ctx context.Context, runID string, items []replayPendingItem) error {
	batch := &pgx.Batch{}
	for _, item := range items {
		batch.Queue(`
			INSERT INTO replay_run_items (
				run_id, decision_id, symbol, verdict, not_replayable_cause,
				base_outcome, new_outcome, chain_followed, episode_outcome_pnl
			) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::numeric)
		`, runID, item.decisionID, item.symbol, item.verdict, item.cause,
			item.baseOutcome, item.newOutcome, item.chainFollowed, item.episodePnL)
	}
	if err := worker.db.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("items insert: %w", err)
	}
	return nil
}

// finalizeReplayRunStats computes the run's stats and effect from the
// durable items (the source of truth — a resumed run finalizes identically
// to an uninterrupted one). Episodes are deduplicated once per
// COALESCE(episode_id, decision id): ten attempts of one opportunity must
// not inflate the counterfactual tenfold (plan §4.1).
func (worker *Worker) finalizeReplayRunStats(ctx context.Context, runID string) ([]byte, []byte, error) {
	const diffExpr = `((CASE WHEN new_outcome = 'ALLOW' THEN 1 ELSE 0 END)
	                 - (CASE WHEN base_outcome = 'ALLOW' THEN 1 ELSE 0 END))`
	const eligibleExpr = `rn = 1 AND episode_outcome_pnl IS NOT NULL AND ` + diffExpr + ` <> 0`
	var checked, changed, fullChain, outcomesAvailable int64
	var delta, missed, prevented float64
	var causesRaw []byte
	err := worker.db.QueryRow(ctx, `
		WITH ranked AS (
			SELECT i.verdict, i.not_replayable_cause, i.chain_followed,
			       i.base_outcome, i.new_outcome, i.episode_outcome_pnl,
			       ROW_NUMBER() OVER (
			           PARTITION BY COALESCE(ed.episode_id::TEXT, 'dec:' || i.decision_id::TEXT)
			           ORDER BY i.id
			       ) AS rn
			FROM replay_run_items i
			LEFT JOIN entry_decisions ed ON ed.id = i.decision_id
			WHERE i.run_id = $1::uuid
		)
		SELECT
			(SELECT COUNT(*) FROM ranked)::BIGINT,
			(SELECT COUNT(*) FROM ranked WHERE verdict = 'CHANGED')::BIGINT,
			(SELECT COUNT(*) FROM ranked WHERE verdict = 'CHANGED' AND chain_followed)::BIGINT,
			(SELECT COUNT(*) FROM ranked WHERE `+eligibleExpr+`)::BIGINT,
			(SELECT COALESCE(SUM(`+diffExpr+` * episode_outcome_pnl), 0) FROM ranked WHERE `+eligibleExpr+`)::FLOAT8,
			(SELECT COALESCE(SUM(`+diffExpr+` * GREATEST(episode_outcome_pnl, 0)), 0) FROM ranked WHERE `+eligibleExpr+`)::FLOAT8,
			(SELECT COALESCE(SUM(`+diffExpr+` * LEAST(episode_outcome_pnl, 0)), 0) FROM ranked WHERE `+eligibleExpr+`)::FLOAT8,
			COALESCE((
				SELECT jsonb_object_agg(cause, cnt) FROM (
					SELECT not_replayable_cause AS cause, COUNT(*)::BIGINT AS cnt
					FROM ranked
					WHERE verdict = 'NOT_REPLAYABLE'
					  AND COALESCE(not_replayable_cause, '') <> ''
					GROUP BY not_replayable_cause
				) causes
			), '{}'::jsonb)
	`, runID).Scan(&checked, &changed, &fullChain, &outcomesAvailable,
		&delta, &missed, &prevented, &causesRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize stats: %w", err)
	}
	causes := map[string]int64{}
	if len(causesRaw) > 0 {
		if err := json.Unmarshal(causesRaw, &causes); err != nil {
			return nil, nil, fmt.Errorf("finalize stats: not_replayable causes: %w", err)
		}
	}
	stats := map[string]any{
		"checked":              checked,
		"verdicts_changed":     changed,
		"full_chain_available": fullChain,
		"outcomes_available":   outcomesAvailable,
		"not_replayable":       causes,
	}
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize stats: marshal: %w", err)
	}
	// No completed outcomes → the effect is honestly EMPTY (plan §6: "данных
	// недостаточ" is a valid first result, not a failure).
	effectJSON := []byte("{}")
	if outcomesAvailable > 0 {
		effect := map[string]any{
			"delta_pnl_proxy":  replayRound6(delta),
			"prevented_losses": replayRound6(prevented),
			"missed_profits":   replayRound6(missed),
		}
		effectJSON, err = json.Marshal(effect)
		if err != nil {
			return nil, nil, fmt.Errorf("finalize stats: marshal effect: %w", err)
		}
	}
	return statsJSON, effectJSON, nil
}

// finishReplayRun terminates the run: DONE with stats+effect, or FAILED with
// the reason (stats stay empty — partial numbers would pose as results).
func (worker *Worker) finishReplayRun(ctx context.Context, runID, status, reason string, statsJSON, effectJSON []byte) {
	if statsJSON == nil {
		statsJSON = []byte("{}")
	}
	if effectJSON == nil {
		effectJSON = []byte("{}")
	}
	if _, err := worker.db.Exec(ctx, `
		UPDATE replay_runs
		SET status = $2, status_reason = $3, stats = $4::jsonb, effect = $5::jsonb,
		    finished_at = NOW(), lease_until = NULL
		WHERE id = $1::uuid
	`, runID, status, reason, string(statsJSON), string(effectJSON)); err != nil {
		worker.replayWarn("replay: finish run write failed", fmt.Errorf("run %s status %s: %w", runID, status, err))
	}
}

// replayStatsLine renders the N/K/M/L contract for the log line.
func replayStatsLine(statsJSON []byte) string {
	var stats struct {
		Checked            int64            `json:"checked"`
		VerdictsChanged    int64            `json:"verdicts_changed"`
		FullChainAvailable int64            `json:"full_chain_available"`
		OutcomesAvailable  int64            `json:"outcomes_available"`
		NotReplayable      map[string]int64 `json:"not_replayable"`
	}
	if err := json.Unmarshal(statsJSON, &stats); err != nil {
		return "stats unreadable"
	}
	return fmt.Sprintf("checked=%d changed=%d full_chain=%d outcomes=%d not_replayable=%v",
		stats.Checked, stats.VerdictsChanged, stats.FullChainAvailable,
		stats.OutcomesAvailable, stats.NotReplayable)
}

// replayRound6 rounds a proxy figure to 6 decimals (jsonb-friendly, no
// float-noise tails) and normalizes -0 to 0.
func replayRound6(x float64) float64 {
	r := math.Round(x*1e6) / 1e6
	if r == 0 {
		return 0
	}
	return r
}

// replayWarn logs through the worker's logger when present (unit tests run
// with a bare Worker).
func (worker *Worker) replayWarn(msg string, err error) {
	if worker.logger != nil {
		worker.logger.Warn(msg, "component", "autogrid_worker", "error", err)
	}
}
