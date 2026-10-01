package autogrid

import (
	"context"
	"testing"
	"time"
)

// rvRejectTrace is the canonical recorded RV rejection (worker.go
// rejectCandidateDI): everything a threshold gate decided with lives in
// Inputs — {"ratio":1.62,"threshold":1.5,"direction":"gte"}.
func rvRejectTrace(ratio, threshold float64) []GateTraceEntry {
	return []GateTraceEntry{{
		Gate:    "RV",
		Verdict: "REJECT",
		Inputs: map[string]any{
			"ratio":     ratio,
			"threshold": threshold,
			"direction": "gte",
		},
		Quality: "OK",
	}}
}

func rvOverride(threshold float64) map[string]any {
	return map[string]any{
		"gate":   "RV",
		"params": map[string]any{"threshold": threshold},
	}
}

func TestReplayValidationRunReproducesStoredVerdict(t *testing.T) {
	trace := rvRejectTrace(1.62, 1.5)
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, map[string]any{})
	if !replayable || cause != "" {
		t.Fatalf("validation: expected replayable, got replayable=%v cause=%q", replayable, cause)
	}
	if newOutcome != entryOutcomeReject {
		t.Fatalf("validation: newOutcome = %q, want REJECT (reproduced)", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("validation: chainFollowed must be true for a reproduced verdict")
	}
}

func TestReplayValidationRunFlagsInconsistentTrace(t *testing.T) {
	// Stored verdict PASS while the recorded triple says 1.62 >= 1.5 → REJECT:
	// the zero override contradicts the stored verdict — a recording bug.
	trace := []GateTraceEntry{{
		Gate:    "RV",
		Verdict: "PASS",
		Inputs:  map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"},
	}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeAllow, map[string]any{})
	if replayable {
		t.Fatal("inconsistent trace must not be replayable")
	}
	if cause != replayCauseInconsistentTrace {
		t.Fatalf("cause = %q, want inconsistent_trace", cause)
	}
}

func TestReplayValidationRunEmptyTraceHasNoChain(t *testing.T) {
	replayable, _, _, cause := ReplayTraceAgainstOverride(nil, entryOutcomeReject, map[string]any{})
	if replayable {
		t.Fatal("empty trace in validation mode must not claim REPRODUCED")
	}
	if cause != replayCauseChainNotRecorded {
		t.Fatalf("cause = %q, want chain_not_recorded", cause)
	}
}

func TestReplayValidationRunSkipsNonThresholdEntries(t *testing.T) {
	// Shared-market gates journal a single {gate, verdict, inputs} element
	// with a flat feature map — no replay triple, no verification, no blame.
	trace := []GateTraceEntry{
		{Gate: "MACRO", Verdict: "REJECT", Inputs: map[string]any{"dom_delta": 1.2}},
		{Gate: "BACKTEST", Verdict: "PASS", Inputs: map[string]any{
			"traded": map[string]any{"net_ev": 0.4, "oos_pct": 3.1, "round_trips": 21},
		}},
	}
	replayable, newOutcome, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, map[string]any{})
	if !replayable || cause != "" {
		t.Fatalf("expected replayable validation over non-numeric trace, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeReject {
		t.Fatalf("newOutcome = %q, want REJECT", newOutcome)
	}
}

func TestReplayGateNotInTrace(t *testing.T) {
	trace := rvRejectTrace(1.62, 1.5)
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "STRESS", "params": map[string]any{"threshold": 9.0}})
	if replayable {
		t.Fatal("override of a gate absent from the trace must not be replayable")
	}
	if cause != replayCauseGateNotInTrace {
		t.Fatalf("cause = %q, want gate_not_in_trace", cause)
	}
}

func TestReplayThresholdNotStoredForNonDirectionGates(t *testing.T) {
	// STRESS records {stress_loss, ceiling} — numbers, but no threshold key
	// and no comparison direction: not threshold-replayable by design.
	trace := []GateTraceEntry{{
		Gate:    "STRESS",
		Verdict: "REJECT",
		Inputs:  map[string]any{"stress_loss": 6.4, "ceiling": 5.0},
	}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "STRESS", "params": map[string]any{"threshold": 7.0}})
	if replayable {
		t.Fatal("STRESS must not be threshold-replayable")
	}
	if cause != replayCauseThresholdNotStored {
		t.Fatalf("cause = %q, want threshold_not_stored", cause)
	}
}

func TestReplayThresholdNotStoredForExemptGate(t *testing.T) {
	trace := []GateTraceEntry{{
		Gate:      "ANTI_FOMO",
		Verdict:   "EXEMPT",
		Exemption: "confirmed_directional_trend",
		Inputs:    map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"},
	}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "ANTI_FOMO", "params": map[string]any{"threshold": 1.8}})
	if replayable {
		t.Fatal("an exempted gate was not decided by its threshold")
	}
	if cause != replayCauseThresholdNotStored {
		t.Fatalf("cause = %q, want threshold_not_stored", cause)
	}
}

func TestReplayInputsMissingWithoutValue(t *testing.T) {
	trace := []GateTraceEntry{{
		Gate:    "RV",
		Verdict: "REJECT",
		Inputs:  map[string]any{"threshold": 1.5, "direction": "gte"},
	}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if replayable {
		t.Fatal("missing compared value must not be replayable")
	}
	if cause != replayCauseInputsMissing {
		t.Fatalf("cause = %q, want inputs_missing", cause)
	}
}

func TestReplayInputsMissingWithoutDirection(t *testing.T) {
	trace := []GateTraceEntry{{
		Gate:    "RV",
		Verdict: "REJECT",
		Inputs:  map[string]any{"ratio": 1.62, "threshold": 1.5},
	}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if replayable {
		t.Fatal("missing comparison direction must not be replayable")
	}
	if cause != replayCauseInputsMissing {
		t.Fatalf("cause = %q, want inputs_missing", cause)
	}
}

func TestReplayInputsMissingForNotEvaluatedGate(t *testing.T) {
	trace := []GateTraceEntry{{Gate: "KNIFE", Verdict: "NOT_EVALUATED"}}
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "KNIFE", "params": map[string]any{"threshold": 2.0}})
	if replayable {
		t.Fatal("a gate that never ran cannot be overridden")
	}
	if cause != replayCauseInputsMissing {
		t.Fatalf("cause = %q, want inputs_missing", cause)
	}
}

func TestReplayOverrideWithoutThresholdIsHonestNo(t *testing.T) {
	trace := rvRejectTrace(1.62, 1.5)
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "RV"})
	if replayable {
		t.Fatal("an override carrying no numeric threshold must not apply")
	}
	if cause != replayCauseThresholdNotStored {
		t.Fatalf("cause = %q, want threshold_not_stored", cause)
	}
}

func TestReplayUnchangedVerdictKeepsBaseOutcome(t *testing.T) {
	// 1.62 >= 1.4 stays REJECT: the gate verdict does not flip, the recorded
	// chain already ran to its end — rule 4.
	trace := rvRejectTrace(1.62, 1.5)
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.4))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable unchanged verdict, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeReject {
		t.Fatalf("newOutcome = %q, want REJECT (base)", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("unchanged verdict implies the recorded chain decided")
	}
}

func TestReplayRejectToPassWithNotEvaluatedRemainder(t *testing.T) {
	// The common shape of a short-circuited rejection: later gates are
	// NOT_EVALUATED stubs. Flipping the deciding gate leaves the rest of the
	// chain unknown — honest answer: chainFollowed=false, outcome "".
	trace := []GateTraceEntry{
		{Gate: "RV", Verdict: "REJECT", Inputs: map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"}},
		{Gate: "KNIFE", Verdict: "NOT_EVALUATED"},
		{Gate: "BACKTEST", Verdict: "NOT_EVALUATED"},
	}
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable, got %v/%q", replayable, cause)
	}
	if chainFollowed {
		t.Fatal("NOT_EVALUATED after the flipped gate must break chainFollowed")
	}
	if newOutcome != "" {
		t.Fatalf("newOutcome = %q, want \"\" (unknown chain)", newOutcome)
	}
}

func TestReplayRejectToPassWithoutAnyRemainder(t *testing.T) {
	// Single-element trace (the shared-gate / RV journal shape): flipping the
	// only gate leaves NOTHING recorded about the chain → chain_not_recorded.
	trace := rvRejectTrace(1.62, 1.5)
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if replayable {
		t.Fatal("no remainder means the chain was never recorded")
	}
	if cause != replayCauseChainNotRecorded {
		t.Fatalf("cause = %q, want chain_not_recorded", cause)
	}
}

func TestReplayRejectToPassFullChainAllows(t *testing.T) {
	trace := []GateTraceEntry{
		{Gate: "RV", Verdict: "REJECT", Inputs: map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"}},
		{Gate: "KNIFE", Verdict: "PASS"},
		{Gate: "BACKTEST", Verdict: "PASS"},
	}
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeAllow {
		t.Fatalf("newOutcome = %q, want ALLOW", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("fully evaluated remainder must keep chainFollowed")
	}
}

func TestReplayRejectToPassLaterRejectKeepsChainReject(t *testing.T) {
	trace := []GateTraceEntry{
		{Gate: "RV", Verdict: "REJECT", Inputs: map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"}},
		{Gate: "KNIFE", Verdict: "PASS"},
		{Gate: "STRESS", Verdict: "REJECT"},
	}
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeReject {
		t.Fatalf("newOutcome = %q, want REJECT (later gate still rejects)", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("a recorded later REJECT was evaluated — chainFollowed must hold")
	}
}

func TestReplayRejectToPassLaterWaitDefers(t *testing.T) {
	trace := []GateTraceEntry{
		{Gate: "RV", Verdict: "REJECT", Inputs: map[string]any{"ratio": 1.62, "threshold": 1.5, "direction": "gte"}},
		{Gate: "GATE_UNREADABLE", Verdict: "WAIT"},
	}
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject, rvOverride(1.8))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeWait {
		t.Fatalf("newOutcome = %q, want WAIT (subsequent deferral)", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("a recorded WAIT was evaluated — chainFollowed must hold")
	}
}

func TestReplayPassToRejectClosesChain(t *testing.T) {
	// RV PASSED at 1.3 against 1.5; the override drops the threshold to 1.2 →
	// 1.3 >= 1.2 now rejects: the chain is REJECT the moment any gate does.
	trace := []GateTraceEntry{
		{Gate: "RV", Verdict: "PASS", Inputs: map[string]any{"ratio": 1.3, "threshold": 1.5, "direction": "gte"}},
		{Gate: "KNIFE", Verdict: "PASS"},
		{Gate: "BACKTEST", Verdict: "PASS"},
	}
	replayable, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(trace, entryOutcomeAllow, rvOverride(1.2))
	if !replayable || cause != "" {
		t.Fatalf("expected replayable, got %v/%q", replayable, cause)
	}
	if newOutcome != entryOutcomeReject {
		t.Fatalf("newOutcome = %q, want REJECT", newOutcome)
	}
	if !chainFollowed {
		t.Fatal("PASS→REJECT needs no remainder — chainFollowed must hold")
	}
}

func TestReplayLteDirectionSemantics(t *testing.T) {
	// "lte": value <= threshold is the REJECT class. 0.9 <= 1.0 rejects at
	// 1.0; raising the threshold to 0.8 flips the gate to PASS.
	trace := []GateTraceEntry{{
		Gate:    "FLOOR",
		Verdict: "REJECT",
		Inputs:  map[string]any{"value": 0.9, "threshold": 1.0, "direction": "lte"},
	}}
	// Single-gate trace flipped REJECT→PASS with NO remainder recorded: the
	// chain after the gate was never recorded — chain_not_recorded.
	replayable, _, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "FLOOR", "params": map[string]any{"threshold": 0.8}})
	if replayable || cause != replayCauseChainNotRecorded {
		t.Fatalf("single-gate flip: replayable=%v cause=%q, want false/chain_not_recorded", replayable, cause)
	}
	// The same override on a trace WITH a recorded remainder allows.
	trace = append(trace, GateTraceEntry{Gate: "KNIFE", Verdict: "PASS"})
	_, newOutcome, chainFollowed, _ := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": "FLOOR", "params": map[string]any{"threshold": 0.8}})
	if newOutcome != entryOutcomeAllow || !chainFollowed {
		t.Fatalf("lte flip with PASS remainder: outcome=%q followed=%v, want ALLOW/true", newOutcome, chainFollowed)
	}
}

func TestReplayUsesEntryLevelThresholdField(t *testing.T) {
	trace := []GateTraceEntry{{
		Gate:      "RV",
		Verdict:   "REJECT",
		Threshold: 1.5,
		Inputs:    map[string]any{"ratio": 1.62, "direction": "gte"},
	}}
	_, newOutcome, chainFollowed, cause := ReplayTraceAgainstOverride(
		append(trace, GateTraceEntry{Gate: "KNIFE", Verdict: "PASS"}), entryOutcomeReject, rvOverride(1.8))
	if cause != "" {
		t.Fatalf("entry-level threshold must satisfy the replay contract, cause=%q", cause)
	}
	if newOutcome != entryOutcomeAllow || !chainFollowed {
		t.Fatalf("outcome=%q followed=%v, want ALLOW/true", newOutcome, chainFollowed)
	}
}

func TestReplayGateMatchIsCaseInsensitive(t *testing.T) {
	trace := rvRejectTrace(1.62, 1.5)
	trace = append(trace, GateTraceEntry{Gate: "KNIFE", Verdict: "PASS"})
	_, newOutcome, _, cause := ReplayTraceAgainstOverride(trace, entryOutcomeReject,
		map[string]any{"gate": " rv ", "params": map[string]any{"threshold": 1.8}})
	if cause != "" {
		t.Fatalf("gate match must ignore case/whitespace, cause=%q", cause)
	}
	if newOutcome != entryOutcomeAllow {
		t.Fatalf("newOutcome = %q, want ALLOW", newOutcome)
	}
}

func TestReplayFloatCoercion(t *testing.T) {
	cases := []struct {
		raw    any
		want   float64
		wantOK bool
	}{
		{raw: float64(1.5), want: 1.5, wantOK: true},
		{raw: 2, want: 2, wantOK: true},
		{raw: int64(3), want: 3, wantOK: true},
		{raw: "1.8", want: 1.8, wantOK: true},
		{raw: " 1.8 ", want: 1.8, wantOK: true},
		{raw: "nope", wantOK: false},
		{raw: "", wantOK: false},
		{raw: nil, wantOK: false},
		{raw: []any{1.0}, wantOK: false},
		{raw: map[string]any{}, wantOK: false},
	}
	for _, tc := range cases {
		got, ok := replayFloat(tc.raw)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("replayFloat(%#v) = (%v, %v), want (%v, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestEnqueueReplayRunArgumentValidation(t *testing.T) {
	from := time.Now().Add(-24 * time.Hour)
	if _, err := EnqueueReplayRun(context.Background(), nil, time.Now(), from, nil, nil, "test"); err == nil {
		t.Fatal("inverted window must error, not panic")
	}
	if _, err := EnqueueReplayRun(context.Background(), nil, from, time.Now(), nil, nil, "test"); err == nil {
		t.Fatal("nil pool must error, not panic")
	}
}

func TestComputeReplayItemEffectEligibility(t *testing.T) {
	worker := &Worker{} // computeReplayItem touches no db/logger
	episodeID := "11111111-1111-1111-1111-111111111111"
	// RV passed at 1.3/1.5, rest of the chain recorded PASS — an ALLOW decision.
	allowTrace := []byte(`[
		{"gate":"RV","verdict":"PASS","inputs":{"ratio":1.3,"threshold":1.5,"direction":"gte"}},
		{"gate":"KNIFE","verdict":"PASS"}
	]`)

	// PASS→REJECT override on an ALLOW decision: effect-eligible, diff −1.
	item := worker.computeReplayItem(
		&replayClaimedRun{overrides: map[string]any{"gate": "RV", "params": map[string]any{"threshold": 1.2}}},
		replayDecisionRow{id: "d1", symbol: "BTC_USDT_PERP", outcome: entryOutcomeAllow, episodeID: &episodeID, traceBytes: allowTrace})
	if item.verdict != replayItemChanged {
		t.Fatalf("verdict = %q, want CHANGED", item.verdict)
	}
	if item.admissionDiff != -1 || item.episodeID == nil || *item.episodeID != episodeID {
		t.Fatalf("ALLOW→REJECT must carry diff −1 and its episode, got diff=%d episode=%v", item.admissionDiff, item.episodeID)
	}

	// NOT_REPLAYABLE on an ALLOW decision: no a_new exists — the item must
	// stay out of the effect even though "" != ALLOW.
	item = worker.computeReplayItem(
		&replayClaimedRun{overrides: map[string]any{"gate": "STRESS", "params": map[string]any{"threshold": 7.0}}},
		replayDecisionRow{id: "d2", symbol: "BTC_USDT_PERP", outcome: entryOutcomeAllow, episodeID: &episodeID, traceBytes: allowTrace})
	if item.verdict != replayItemNotReplayable || item.cause != replayCauseGateNotInTrace {
		t.Fatalf("verdict=%q cause=%q, want NOT_REPLAYABLE/gate_not_in_trace", item.verdict, item.cause)
	}
	if item.admissionDiff != 0 || item.episodeID != nil {
		t.Fatal("NOT_REPLAYABLE items must never be effect-eligible")
	}

	// Validation run over the same ALLOW decision: REPRODUCED, effect-free.
	item = worker.computeReplayItem(
		&replayClaimedRun{overrides: map[string]any{}},
		replayDecisionRow{id: "d3", symbol: "BTC_USDT_PERP", outcome: entryOutcomeAllow, episodeID: &episodeID, traceBytes: allowTrace})
	if item.verdict != replayItemReproduced {
		t.Fatalf("verdict = %q, want REPRODUCED", item.verdict)
	}
	if item.admissionDiff != 0 {
		t.Fatal("validation runs reproduce the base admission — no effect")
	}
}

func TestComputeReplayItemUnparsableTrace(t *testing.T) {
	worker := &Worker{}
	item := worker.computeReplayItem(
		&replayClaimedRun{overrides: map[string]any{"gate": "RV", "params": map[string]any{"threshold": 1.8}}},
		replayDecisionRow{id: "d4", symbol: "X", outcome: entryOutcomeReject, traceBytes: []byte(`{not json`)})
	if item.verdict != replayItemNotReplayable || item.cause != replayCauseInputsMissing {
		t.Fatalf("verdict=%q cause=%q, want NOT_REPLAYABLE/inputs_missing", item.verdict, item.cause)
	}
}
