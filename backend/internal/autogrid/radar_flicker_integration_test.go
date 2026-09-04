package autogrid

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// seedBandSnapshots plants a chronological band sequence (oldest first) for a
// bot so the dwell window reads exactly the flicker under test.
func (h *radarRealHarness) seedBandSnapshots(t *testing.T, botID, symbol string, scores []float64) {
	t.Helper()
	for i, score := range scores {
		ageMin := (len(scores) - i) * 3 // 3-minute radar cadence, newest last
		if _, err := h.pool.Exec(context.Background(), `
			INSERT INTO bot_risk_snapshots
				(bot_id, bot_number, bot_source, symbol, mode, score, band, captured_at)
			VALUES ($1, 500, 'REAL', $2, 'ACTIVE', $3, $4, NOW() - make_interval(mins => $5))
		`, botID, symbol, score, radarBand(score), ageMin); err != nil {
			t.Fatalf("seed band snapshot: %v", err)
		}
	}
}

// (a) The operator's flicker scenario: the band bounced 2↔3↔4 at the
// boundary. With the strict-consecutive dwell a single sub-B3 snapshot
// reset the count to 1 exactly when the escape mattered (prod AXTIX #668
// class). The v2.0.75 window dwell tolerates ONE flicker inside the trailing
// window: «2-3-4-4-4» and «4-4-2-4» both fire, «4-2-2-4» still blocks.
func TestRadarDwellFlickerTolerance(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Literal operator scenario «band мигает 2-3-4-4-4» (chronological): the
	// escape must fire — this sequence also passed the old rule, it is pinned
	// so the window semantics can never regress below it.
	literal := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, literal, symbol, []float64{0.65, 0.80, 0.93, 0.93, 0.93})
	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = literal
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 4, Score: 0.93, S1: 1.0, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("scenario 2-3-4-4-4 must escape (1 adjust), got %d", got)
	}

	// The flicker that used to be deaf: a B2 snapshot INSIDE the trailing
	// window (chronological 4,4,2,4 — the current pass re-entered B4).
	// Old rule: dwell=1 → blocked forever while the band oscillated. New
	// rule: 3 of the last 4 over B3 → escape.
	flicker := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, flicker, symbol, []float64{0.93, 0.93, 0.65, 0.93})
	b.botID = flicker
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 4, Score: 0.93, S1: 1.0, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 2 {
		t.Fatalf("a single B2 flicker inside the window must not block the escape, got %d adjusts", got-1)
	}

	// Two misses inside the window still block: recurrence without
	// persistence is noise, not a signal.
	noisy := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, noisy, symbol, []float64{0.93, 0.65, 0.65, 0.93})
	b.botID = noisy
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 4, Score: 0.93, S1: 1.0, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 2 {
		t.Fatalf("two sub-B3 snapshots inside the window must keep the dwell blocked, got %d adjusts", got-1)
	}
}

// (b) The deaf branch made loud: when the exchange rejects the escape
// adjustParams, the failure must leave a durable RADAR_RECENTER_FAILED event
// (rate-limited to one per hour) instead of the Warn-only swallow that kept
// prod AXTIX #668 silent through 8 hours of band 3/4.
func TestRadarRealRecenterFailureIsDurable(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, botID, symbol)
	h.mock.failAdjust.Store(true)

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{
		Band: 4, Score: 0.93, S1: 1.0, DistToStopATR: 2.0,
	})

	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("the exchange must have been asked once, got %d", got)
	}
	var events int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_RECENTER_FAILED'
	`, botID).Scan(&events); err != nil {
		t.Fatalf("count failure events: %v", err)
	}
	if events != 1 {
		t.Fatalf("a rejected escape must leave exactly one RADAR_RECENTER_FAILED event, got %d", events)
	}
	var marker string
	if err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(model_state->>'radarFailAlertAt','') FROM grid_bots WHERE id = $1
	`, botID).Scan(&marker); err != nil || marker == "" {
		t.Fatalf("the 1h dedup marker must be armed, got %q (%v)", marker, err)
	}
	var adjustments int
	if err := h.pool.QueryRow(ctx, `SELECT adjustments_count FROM grid_bots WHERE id = $1`, botID).Scan(&adjustments); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if adjustments != 0 {
		t.Fatalf("failed adjust must not spend the budget, got %d", adjustments)
	}

	// The hour-long dedup: a second rejected attempt stays at one event.
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{
		Band: 4, Score: 0.93, S1: 1.0, DistToStopATR: 2.0,
	})
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_RECENTER_FAILED'
	`, botID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("failure events must dedup to 1/h, got %d (%v)", events, err)
	}
	// And the radar's own cooldown must NOT arm on the failure — the next
	// pass after the marker window may retry the escape.
	lastAt, ok := h.worker.radarLastActionAt(ctx, botID)
	if ok && time.Since(lastAt) < time.Hour {
		t.Fatal("a failed recenter must not arm the durable radar cooldown")
	}
}
