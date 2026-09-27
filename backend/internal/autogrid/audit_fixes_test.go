package autogrid

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ── v2.0.142 audit fixes: unit half ─────────────────────────────────────────

// TestTranchePourGateRefused pins the P2d classifier: only the invest-lane
// GATE reason families (breaker / margin reserve / risk engine — the durable
// pre-flight gates that run BEFORE any native call) may clear the
// trancheIntentAt fence. A native exchange refusal is its own class (the
// caller's IsOutcomeUnknown branch), and a persist failure after a
// successful native pour matches nothing — the fence must stay armed there.
func TestTranchePourGateRefused(t *testing.T) {
	native := fmt.Errorf("%w: pionex refused", ErrNativeAdjustRefused)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"native refused", native, false},
		{
			"breaker gate",
			errors.New("circuit breaker: 3 защитных закрытий за последний час — доливка на паузе"),
			true,
		},
		{
			"margin reserve gate",
			errors.New("резерв маржи: projected $1000.00 при equity $1000.00 оставляет <30% свободных — вход отложен"),
			true,
		},
		{
			"margin reserve read failure (fail-closed variant)",
			errors.New("резерв маржи: чтение снапшота equity не удалось (fail-closed): connection refused"),
			true,
		},
		{
			"risk engine gate",
			errors.New("invest_in rejected by risk engine: kill switch enabled"),
			true,
		},
		{
			// Post-native persist failure: the pour LANDED — the fence is the
			// only thing standing between a retry and a second real pour.
			"persist after native success", errors.New("persist adjustment: connection refused"), false,
		},
		{
			// Pre-native infra failure, but not one of the durable gates —
			// conservative: the classifier must stay a whitelist.
			"ticker unavailable",
			errors.New("adjust requires a live market price (openPrice) but the ticker is unavailable"),
			false,
		},
	}
	for _, tc := range cases {
		if got := tranchePourGateRefused(tc.err); got != tc.want {
			t.Errorf("%s: tranchePourGateRefused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFleetCandidateDeltaCharges pins the P2a pre-charge on the vocabulary
// BOTH deploy gates feed it (paper keeps "no_trend"/"neutral", REAL keeps
// "no_trend" for a neutral scanner trend). The crux: a NEUTRAL candidate
// charges ½×budget×lev — paper used to charge ZERO and skipped the gate.
func TestFleetCandidateDeltaCharges(t *testing.T) {
	budget, lev := decimal.NewFromInt(100), 3
	cases := map[string]string{
		"long":     "300",
		"short":    "-300",
		"no_trend": "150", // paper/REAL neutral vocabulary
		"neutral":  "150", // smart-override neutral
		"":         "150",
	}
	for trend, want := range cases {
		got := fleetCandidateDelta(trend, budget, lev)
		if got.String() != want {
			t.Errorf("fleetCandidateDelta(%q) = %s, want %s", trend, got.String(), want)
		}
	}
}

// TestProjectedFleetDeltaParkFold pins the P2a/v2.0.140 park projection now
// shared by both gates: directional candidates charge the park against their
// own side, a NEUTRAL candidate adds the park on top of |D+dc|.
func TestProjectedFleetDeltaParkFold(t *testing.T) {
	fleet := decimal.NewFromInt(-100)
	park := decimal.NewFromInt(200)
	cases := []struct {
		trend  string
		charge string
		want   string
	}{
		{"long", "300", "400"},     // |-100+300+200|
		{"short", "-300", "600"},   // |-100-300-200|
		{"no_trend", "150", "250"}, // |-100+150|+200
	}
	for _, tc := range cases {
		charge, _ := decimal.NewFromString(tc.charge)
		want, _ := decimal.NewFromString(tc.want)
		if got := projectedFleetDelta(tc.trend, fleet, charge, park); !got.Equal(want) {
			t.Errorf("projectedFleetDelta(%q) = %s, want %s", tc.trend, got.String(), want.String())
		}
	}
}

// ── v2.0.142 audit fixes: integration half (disposable DB) ──────────────────

// TestPaperNeutralCandidateChargesParkAndHalf locks the P2a fix at the gate's
// own composition: a paper NEUTRAL candidate over a parked neutral fleet now
// projects |fleet + ½×budget×lev| + park — pre-fix the paper gate charged
// ZERO for NEUTRAL and never consulted the cap at all. Mirrors the
// TestCalculateFleetNetDeltaNeutralPark seeding (two RUNNING paper NEUTRALs,
// $100 at 2x each → park 200, delta 0).
func TestPaperNeutralCandidateChargesParkAndHalf(t *testing.T) {
	env := newPortfolioTestEnv(t)
	ctx := context.Background()

	env.seedPaperBot(t, "PFDP-NEUT-HALF-1_USDT_PERP", "NEUTRAL", "RUNNING", "{}",
		decimal.NewFromInt(100), 2)
	env.seedPaperBot(t, "PFDP-NEUT-HALF-2_USDT_PERP", "NEUTRAL", "RUNNING", "{}",
		decimal.NewFromInt(100), 2)

	fleetDelta, park, err := env.worker.calculateFleetNetDelta(ctx, env.settings.ID, true)
	if err != nil {
		t.Fatalf("calculateFleetNetDelta(paper): %v", err)
	}
	if !fleetDelta.Equal(decimal.Zero) {
		t.Fatalf("paper fleet delta = %s, want 0 (hermetic PFDP- fleet polluted?)", fleetDelta.String())
	}
	if !park.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("paper neutral park = %s, want 200", park.String())
	}

	// The exact gate composition deployPaper now runs (shared helpers, REAL
	// parity): a NEUTRAL candidate, budget 100 at 2x.
	candidateDelta := fleetCandidateDelta("no_trend", decimal.NewFromInt(100), 2)
	if !candidateDelta.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("paper NEUTRAL pre-charge = %s, want 100 (½×100×2)", candidateDelta.String())
	}
	projected := projectedFleetDelta("no_trend", fleetDelta, candidateDelta, park)
	if !projected.Equal(decimal.NewFromInt(300)) {
		t.Fatalf("projected = %s, want |0+100|+200 = 300", projected.String())
	}
	// A cap the pre-fix gate never even consulted for NEUTRAL now rejects:
	// 300 > 250. On the line (300) passes — the cap is a ceiling, not a moat.
	if !projected.GreaterThan(decimal.NewFromInt(250)) {
		t.Fatalf("projected %s must exceed the 250 cap", projected.String())
	}
	if projected.GreaterThan(decimal.NewFromInt(300)) {
		t.Fatalf("projected %s must not exceed itself", projected.String())
	}
}

// TestMarketBlockerCacheMemoizesFleetConstantLegs pins the P2c contract: the
// breaker and economic-event legs are read ONCE per pass — state that changes
// after the first probe must not leak into cached re-probes, while a fresh
// cache (the next pass) and a nil cache (single-shot callers) read live. This
// is the memoized twin of the fail-open semantics: whatever the first reading
// was (including an error's fail-open) stands for the whole pass.
func TestMarketBlockerCacheMemoizesFleetConstantLegs(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE symbol LIKE 'AUDIT-MEM-%'`)
		_, _ = f.pool.Exec(ctx, `DELETE FROM economic_events WHERE title = 'Producer Price Index'`)
	})
	in := EntryChainInput{Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "PAPER"}

	pass1 := &marketBlockerCache{}
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, pass1); code != "" {
		t.Fatalf("first probe on clean DB = %q, want clear", code)
	}

	// Arm BOTH fleet-constant legs after the pass's first probe.
	f.seedPaperClose(t, "AUDIT-MEM-A_USDT_PERP", "STOP_LOSS", 10*time.Minute)
	f.seedPaperClose(t, "AUDIT-MEM-B_USDT_PERP", "STRUCT_INVALID", 20*time.Minute)
	f.seedPaperClose(t, "AUDIT-MEM-C_USDT_PERP", "RANGE_BREAK_DOWN", 30*time.Minute)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Producer Price Index', NOW() + INTERVAL '90 minutes', 'High', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed economic event: %v", err)
	}

	// The SAME pass (same cache) must still read clear — neither leg's SQL
	// ran again. An uncached probe here would return the breaker.
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, pass1); code != "" {
		t.Fatalf("cached re-probe = %q, want clear (fleet-constant legs memoized per pass)", code)
	}
	// A fresh cache (the NEXT pass) sees the armed breaker.
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, &marketBlockerCache{}); code != entryBlockedCircuitBreaker {
		t.Fatalf("fresh cache = %q, want %q", code, entryBlockedCircuitBreaker)
	}
	// The econ leg memoizes independently: with the closes removed, pass1
	// still reads its FIRST breaker state… which was clear, and its FIRST
	// econ state — also clear; the econ event alone must not leak in.
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM paper_grid_bots WHERE symbol LIKE 'AUDIT-MEM-%'`); err != nil {
		t.Fatalf("clear closes: %v", err)
	}
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, pass1); code != "" {
		t.Fatalf("pass1 after closes removed = %q, want clear (both legs still memoized)", code)
	}
	// A fresh cache now names the economic event (breaker disarmed above).
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, &marketBlockerCache{}); code != entryBlockedEconomicEvent {
		t.Fatalf("fresh cache = %q, want %q", code, entryBlockedEconomicEvent)
	}
	// Nil cache reads through uncached: with the event gone everything is
	// clear again — single-shot callers (service variant, DGT) are unaffected.
	if _, err := f.pool.Exec(ctx, `DELETE FROM economic_events`); err != nil {
		t.Fatalf("clear econ events: %v", err)
	}
	if code, _, _, _ := f.worker.probeSharedMarketBlockersCached(ctx, in, nil); code != "" {
		t.Fatalf("nil cache (uncached) = %q, want clear", code)
	}
}

// TestFlipEventWithin24hRealOnly pins the P3-4 filter: only REAL
// DIRECTION_FLIP events satisfy the cascade flip-cooldown exemption —
// DIRECTION_FLIP is written solely by the REAL-only flip experiment, so a
// PAPER-source flip row must not arm an exemption its fleet never earned.
func TestFlipEventWithin24hRealOnly(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	const paperSym, realSym = "AUDIT-FLIP-P_USDT_PERP", "AUDIT-FLIP-R_USDT_PERP"
	t.Cleanup(func() {
		_, _ = f.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE symbol LIKE 'AUDIT-FLIP-%'`)
	})
	if err := LogBotEvent(ctx, f.pool, "itest-flip-paper", 990001, "PAPER", paperSym,
		"DIRECTION_FLIP", nil, nil, map[string]any{"from": "NEUTRAL", "to": "SHORT"}); err != nil {
		t.Fatalf("seed PAPER flip: %v", err)
	}
	if f.worker.flipEventWithin24h(ctx, paperSym) {
		t.Fatalf("PAPER-source flip must not satisfy the exemption")
	}
	if err := LogBotEvent(ctx, f.pool, "itest-flip-real", 990002, "REAL", realSym,
		"DIRECTION_FLIP", nil, nil, map[string]any{"from": "NEUTRAL", "to": "SHORT"}); err != nil {
		t.Fatalf("seed REAL flip: %v", err)
	}
	if !f.worker.flipEventWithin24h(ctx, realSym) {
		t.Fatalf("REAL flip within 24h must satisfy the exemption")
	}
	// A symbol with no flips at all stays unexempt.
	if f.worker.flipEventWithin24h(ctx, "AUDIT-FLIP-NONE_USDT_PERP") {
		t.Fatalf("no flip events must not satisfy the exemption")
	}
}
