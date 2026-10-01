package autogrid

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The decision-intelligence unit contract (DB-free): trace serialization
// round-trip, data-quality semantics, and the lost-observation counter.
// The DB-backed paths (ResolveEpisode bump-or-create, RecordDecision's
// full 0062 insert, LogShadowCoverage) are covered by the separate
// integration suite on a disposable postgres.

// TestGateTraceJSONRoundTrip pins the gate_trace JSON contract: exact tag
// names from migration 0062, omitempty on the optional legs, and a
// lossless marshal→unmarshal round-trip of the populated fields.
func TestGateTraceJSONRoundTrip(t *testing.T) {
	observed := time.Now().UTC().Truncate(time.Second)
	trace := []GateTraceEntry{
		{
			Gate:       "RV",
			Verdict:    "PASS",
			Threshold:  1.5,
			Inputs:     map[string]any{"ratio": 1.62, "candles": 96.0},
			ObservedAt: &observed,
			Quality:    dataQualityOK,
		},
		{
			Gate:      "ANTI_FOMO",
			Verdict:   "EXEMPT",
			Exemption: "confirmed_directional_trend",
		},
		{Gate: "BACKTEST", Verdict: "NOT_EVALUATED"},
	}

	raw := marshalGateTrace(trace)
	if string(raw) == "[]" {
		t.Fatalf("trace serialized to empty array: %s", raw)
	}
	if !json.Valid(raw) {
		t.Fatalf("trace is not valid JSON: %s", raw)
	}

	var back []GateTraceEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip unmarshal failed: %v (raw=%s)", err, raw)
	}
	if len(back) != len(trace) {
		t.Fatalf("round-trip lost entries: got %d want %d", len(back), len(trace))
	}
	if back[0].Gate != "RV" || back[0].Verdict != "PASS" || back[0].Threshold != 1.5 {
		t.Fatalf("entry 0 corrupted: %+v", back[0])
	}
	if back[0].ObservedAt == nil || !back[0].ObservedAt.Equal(observed) {
		t.Fatalf("observed_at did not round-trip: got %v want %v", back[0].ObservedAt, observed)
	}
	if !reflect.DeepEqual(back[0].Inputs, map[string]any{"ratio": 1.62, "candles": 96.0}) {
		t.Fatalf("inputs did not round-trip: %+v", back[0].Inputs)
	}
	if back[1].Exemption != "confirmed_directional_trend" {
		t.Fatalf("exemption did not round-trip: %+v", back[1])
	}

	// Tag names are the 0062 contract (snake_case), and the optional legs
	// of an unevaluated gate stay absent instead of serializing zero values.
	var first map[string]any
	if err := json.Unmarshal(marshalTraceEntry(trace[2]), &first); err != nil {
		t.Fatalf("entry unmarshal failed: %v", err)
	}
	for _, key := range []string{"gate", "verdict"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("entry missing key %q: %v", key, first)
		}
	}
	for _, absent := range []string{"threshold", "exemption", "inputs", "observed_at", "quality"} {
		if _, ok := first[absent]; ok {
			t.Fatalf("unevaluated entry serialized optional key %q: %v", absent, first)
		}
	}

	if got := string(marshalGateTrace(nil)); got != "[]" {
		t.Fatalf("nil trace must serialize to [], got %s", got)
	}
}

// TestMarshalGateTraceUnserializableInputs: an Inputs map carrying a value
// JSON cannot encode (NaN — the realistic residue of a failed indicator
// computation) must cost only that map, never the entry or the trace: the
// entry survives with Quality=ERROR so the replay engine sees the defect
// instead of a silently missing observation.
func TestMarshalGateTraceUnserializableInputs(t *testing.T) {
	trace := []GateTraceEntry{
		{Gate: "STRESS", Verdict: "REJECT", Threshold: 3.0, Inputs: map[string]any{"ratio": math.NaN()}},
		{Gate: "KNIFE", Verdict: "NOT_EVALUATED"},
	}
	raw := marshalGateTrace(trace)
	if !json.Valid(raw) {
		t.Fatalf("NaN inputs produced invalid JSON: %s", raw)
	}
	var back []GateTraceEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal failed: %v (raw=%s)", err, raw)
	}
	if len(back) != 2 {
		t.Fatalf("trace lost entries: got %d want 2 (%s)", len(back), raw)
	}
	if back[0].Gate != "STRESS" || back[0].Verdict != "REJECT" {
		t.Fatalf("entry corrupted by input sanitization: %+v", back[0])
	}
	if back[0].Quality != dataQualityError {
		t.Fatalf("entry with dropped inputs must carry Quality=ERROR, got %q", back[0].Quality)
	}
	if back[0].Inputs != nil {
		t.Fatalf("unserializable inputs must be dropped, got %+v", back[0].Inputs)
	}
}

// TestNormalizeTraceVerdict: an empty verdict defaults to UNKNOWN; a
// recorded verdict passes through verbatim — the trace is jsonb, an
// out-of-vocabulary verdict stays visible instead of being rewritten.
func TestNormalizeTraceVerdict(t *testing.T) {
	if got := normalizeTraceVerdict(""); got != traceVerdictUnknown {
		t.Fatalf("empty verdict: got %q want %q", got, traceVerdictUnknown)
	}
	if got := normalizeTraceVerdict("   "); got != traceVerdictUnknown {
		t.Fatalf("blank verdict: got %q want %q", got, traceVerdictUnknown)
	}
	if got := normalizeTraceVerdict("REJECT"); got != "REJECT" {
		t.Fatalf("recorded verdict rewritten: %q", got)
	}
	if got := normalizeTraceVerdict("ALLOWED_DEGRADED"); got != "ALLOWED_DEGRADED" {
		t.Fatalf("out-of-vocabulary verdict must stay visible, got %q", got)
	}
}

// TestNormalizeDataQualitySemantics pins the row-level passport rollup:
// explicit valid values win, the trace rolls up worst-first with the
// precedence ERROR > DESYNC > STALE > MISSING > OK, unknown explicit values
// degrade to the derived value (never into the column), and an empty trace
// with no explicit claim reads OK.
func TestNormalizeDataQualitySemantics(t *testing.T) {
	trace := func(qualities ...string) []GateTraceEntry {
		entries := make([]GateTraceEntry, len(qualities))
		for i, q := range qualities {
			entries[i] = GateTraceEntry{Gate: "G", Verdict: "PASS", Quality: q}
		}
		return entries
	}
	cases := []struct {
		name     string
		explicit string
		trace    []GateTraceEntry
		want     string
	}{
		{"explicit valid wins over trace", dataQualityStale, trace(dataQualityOK), dataQualityStale},
		{"empty explicit derives worst of trace", "", trace(dataQualityOK, dataQualityStale), dataQualityStale},
		{"error outranks everything", "", trace(dataQualityDesync, dataQualityError, dataQualityStale), dataQualityError},
		{"desync outranks stale and missing", "", trace(dataQualityMissing, dataQualityStale, dataQualityDesync), dataQualityDesync},
		{"stale outranks missing", "", trace(dataQualityMissing, dataQualityStale), dataQualityStale},
		{"missing beats nothing", "", trace(dataQualityOK, dataQualityMissing), dataQualityMissing},
		{"all ok stays ok", "", trace(dataQualityOK, dataQualityOK), dataQualityOK},
		{"empty trace empty explicit is ok", "", nil, dataQualityOK},
		{"garbage explicit falls back to trace", "BOGUS", trace(dataQualityError), dataQualityError},
		{"garbage explicit empty trace degrades to ok", "BOGUS", nil, dataQualityOK},
		{"unknown trace quality is ignored", "", trace("WEIRD"), dataQualityOK},
		{"nil trace with explicit", dataQualityDesync, nil, dataQualityDesync},
	}
	for _, tc := range cases {
		if got := normalizeDataQuality(tc.explicit, tc.trace); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestNormalizeDecisionStage: the stage vocabulary is whitelisted and the
// default for anything unrecognized is SCANNER — the earliest stage, the
// one making the fewest geometry claims.
func TestNormalizeDecisionStage(t *testing.T) {
	for _, valid := range []string{decisionStageScanner, decisionStageIntermediate, decisionStageFinal} {
		if got := normalizeDecisionStage(valid); got != valid {
			t.Errorf("valid stage %q rewritten to %q", valid, got)
		}
	}
	for _, invalid := range []string{"", "scanner", "FINAL-ish", "unknown"} {
		if got := normalizeDecisionStage(invalid); got != decisionStageScanner {
			t.Errorf("invalid stage %q must degrade to SCANNER, got %q", invalid, got)
		}
	}
}

// TestJSONSafeMapAndMarshalJSONMap: a NaN inside the feature vector must
// not void the features column, and the drop must be visible (the
// "_unserializable" key list); nil maps marshal to {} — the jsonb `null`
// scalar is the prod merge-breaker this guards against.
func TestJSONSafeMapAndMarshalJSONMap(t *testing.T) {
	raw := map[string]any{"score": 1.25, "ratio": math.Inf(1), "ok": "yes"}
	safe := jsonSafeMap(raw)
	if _, ok := safe["ratio"]; ok {
		t.Fatalf("unserializable value survived: %+v", safe)
	}
	dropped, _ := safe["_unserializable"].([]string)
	if len(dropped) != 1 || dropped[0] != "ratio" {
		t.Fatalf("dropped keys not recorded: %+v", safe)
	}
	buf := marshalJSONMap(raw)
	if string(buf) != `{"_unserializable":["ratio"],"ok":"yes","score":1.25}` {
		t.Fatalf("unexpected serialization: %s", buf)
	}
	if got := string(marshalJSONMap(nil)); got != "{}" {
		t.Fatalf("nil map must marshal to {}, got %s", got)
	}
}

// TestLostObservationsCounter: a RecordDecision with no pool (shutdown
// race / wiring bug) is a lost observation by construction — counted and
// swallowed, never returned to the trading path. LostObservations is the
// report-facing read.
func TestLostObservationsCounter(t *testing.T) {
	before := LostObservations()
	RecordDecision(context.Background(), nil, DecisionRecord{
		Path: "SCANNER_PAPER", Fleet: "PAPER", Symbol: "BTC_USDT_PERP",
		Outcome: "REJECT", Code: "STRESS", Reason: "test",
		PriceAtDecision: testDecimalPtr(decimal.NewFromInt(42)),
	})
	RecordDecision(context.Background(), nil, DecisionRecord{Path: "SCANNER_REAL", Symbol: "ETH_USDT_PERP", Outcome: "ALLOW"})
	if after := LostObservations(); after < before+2 {
		t.Fatalf("lost counter did not advance: before=%d after=%d", before, after)
	}
	LogShadowCoverage(context.Background(), nil, mustUUID(), mustUUID(), "BTC_USDT_PERP", "", false, "CAP_REACHED")
	if after := LostObservations(); after < before+3 {
		t.Fatalf("coverage ledger loss not counted: before=%d after=%d", before, after)
	}
}

// testDecimalPtr is package-local: presets.go already owns the name
// decimalPtr with a string-argument signature.
func testDecimalPtr(d decimal.Decimal) *decimal.Decimal { return &d }

func mustUUID() uuid.UUID { return uuid.New() }
