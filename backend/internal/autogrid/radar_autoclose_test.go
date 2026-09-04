package autogrid

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// v2.0.84 radar auto-close integration pins. The matrix under test:
//
//	OFF    — nothing is ever closed, whatever the band;
//	BAND3  — band>=3 + floating<0 + dwell>=3 + age>30m + 1h cooldown →
//	         RADAR_AUTOCLOSE event BEFORE the close intent, then
//	         Service.RequestBotClose (REAL → STOP_REQUESTED, PAPER → settle);
//	STRICT — the BAND3 gates plus a KNOWN dist_to_stop < 0.5 ATR (S1<=0 =
//	         no anchored stop → dist is a default zero, never a measurement).
//
// Never-close-green is invariant across every mode (the 2026-09-04 backtest
// counter-examples #651/#670/#675 were all flagged in profit and survived).

// autocloseScores is a band-3 signal with a measured, near barrier distance.
func autocloseScores() radarScores {
	return radarScores{Band: 3, Score: 0.80, S1: 0.95, DistToStopATR: 0.30}
}

func autocloseAction(symbol, botID string) radarInput {
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	return b
}

// (a) OFF — the shipping default — must close nothing even with every other
// gate satisfied.
func TestRadarAutocloseOffClosesNothing(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, botID, symbol)
	// Under water on the floating leg (realized profit is irrelevant).
	if _, err := h.pool.Exec(ctx,
		`UPDATE grid_bots SET unrealized_pnl_usdt = -0.75, realized_pnl_usdt = 0.5 WHERE id = $1`, botID); err != nil {
		t.Fatalf("sink bot: %v", err)
	}

	for _, mode := range []string{"OFF", ""} {
		h.settings.StopForecastMode = "ACTIVE"
		h.settings.RadarAutoCloseMode = mode
		h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, botID), autocloseScores())
	}
	assertNotClosedAndNoEvent(t, h, ctx, botID, "OFF must close nothing")
}

// (b) BAND3 closes an under-water dwelt bot: event first, then the close
// intent. A bot in the green (floating >= 0) is never closed, even dwell-sated
// band 3 — the shift arm owns green bots.
func TestRadarAutocloseBand3(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Green twin: floating +0.5 — must survive untouched.
	greenID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, greenID, symbol)
	if _, err := h.pool.Exec(ctx,
		`UPDATE grid_bots SET unrealized_pnl_usdt = 0.5, realized_pnl_usdt = 0.5 WHERE id = $1`, greenID); err != nil {
		t.Fatalf("green bot: %v", err)
	}
	h.settings.StopForecastMode = "ACTIVE"
	h.settings.RadarAutoCloseMode = "BAND3"
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, greenID), autocloseScores())
	assertNotClosedAndNoEvent(t, h, ctx, greenID, "a floating-green bot must never be auto-closed")

	// Under-water bot: floating -0.75 (realized +0.5 — the gate is the
	// floating leg, exactly the exchange preflight's figure).
	redID := h.seedRealBot(t, symbol, 0)
	h.seedDwell(t, redID, symbol)
	if _, err := h.pool.Exec(ctx,
		`UPDATE grid_bots SET unrealized_pnl_usdt = -0.75, realized_pnl_usdt = 0.5 WHERE id = $1`, redID); err != nil {
		t.Fatalf("sink bot: %v", err)
	}
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, redID), autocloseScores())

	var status, reason string
	if err := h.pool.QueryRow(ctx,
		`SELECT status, COALESCE(closed_reason,'') FROM grid_bots WHERE id = $1`, redID).Scan(&status, &reason); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if status != "STOP_REQUESTED" || reason != "RADAR_AUTOCLOSE_BAND3" {
		t.Fatalf("under-water dwell bot must be STOP_REQUESTED/RADAR_AUTOCLOSE_BAND3, got %s/%s", status, reason)
	}
	var evMode string
	var evBand int
	var evTotal decimal.Decimal
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'mode', (details->>'band')::INT,
		       COALESCE(pnl_usdt, 0)
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_AUTOCLOSE'
		ORDER BY created_at DESC LIMIT 1
	`, redID).Scan(&evMode, &evBand, &evTotal); err != nil {
		t.Fatalf("RADAR_AUTOCLOSE event must be logged before the close: %v", err)
	}
	if evMode != "BAND3" || evBand != 3 || !evTotal.Equal(decimal.NewFromFloat(-0.25)) {
		t.Fatalf("event must carry mode=BAND3 band=3 total=-0.25 (0.5-0.75), got %s/%d/%s", evMode, evBand, evTotal)
	}

	// Dwell unsatisfied (no trailing snapshots): a fresh under-water bot must
	// NOT be closed by a single spike.
	noDwellID := h.seedRealBot(t, symbol, 0)
	if _, err := h.pool.Exec(ctx,
		`UPDATE grid_bots SET unrealized_pnl_usdt = -0.75 WHERE id = $1`, noDwellID); err != nil {
		t.Fatalf("sink bot: %v", err)
	}
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, noDwellID), autocloseScores())
	assertNotClosedAndNoEvent(t, h, ctx, noDwellID, "a single band-3 spike without dwell must not close")
}

// (c) STRICT fires only with a KNOWN near distance: far from the barrier →
// no close; dist unknown (S1 = 0, no anchored stop) → never, even though
// dist_to_stop_atr reads as a default zero.
func TestRadarAutocloseStrictDistanceGate(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	h.settings.StopForecastMode = "ACTIVE"
	h.settings.RadarAutoCloseMode = "STRICT"

	seed := func() string {
		id := h.seedRealBot(t, symbol, 0)
		h.seedDwell(t, id, symbol)
		if _, err := h.pool.Exec(ctx,
			`UPDATE grid_bots SET unrealized_pnl_usdt = -0.75 WHERE id = $1`, id); err != nil {
			t.Fatalf("sink bot: %v", err)
		}
		return id
	}

	// Far from the barrier (2.0 ATR): BAND3 would fire, STRICT must not.
	farID := seed()
	rs := autocloseScores()
	rs.DistToStopATR = 2.0
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, farID), rs)
	assertNotClosedAndNoEvent(t, h, ctx, farID, "STRICT at 2.0 ATR must hold")

	// No anchored stop: S1 = 0, dist_to_stop_atr a default zero — NOT a
	// measured "at the barrier". Must never fire.
	noStopID := seed()
	rs = radarScores{Band: 3, Score: 0.80, S1: 0, DistToStopATR: 0}
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, noStopID), rs)
	assertNotClosedAndNoEvent(t, h, ctx, noStopID, "STRICT without a known dist must never fire")

	// Near and known (0.30 ATR): fires.
	nearID := seed()
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, nearID), autocloseScores())
	var status, reason string
	if err := h.pool.QueryRow(ctx,
		`SELECT status, COALESCE(closed_reason,'') FROM grid_bots WHERE id = $1`, nearID).Scan(&status, &reason); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if status != "STOP_REQUESTED" || reason != "RADAR_AUTOCLOSE_STRICT" {
		t.Fatalf("STRICT at 0.30 ATR must close, got %s/%s", status, reason)
	}
}

// (d) The durable 1h per-bot cooldown: a RADAR_AUTOCLOSE event 30 minutes old
// blocks a second close; two hours old it does not (the 0035 pattern, with
// the event itself as the source — restart-proof).
func TestRadarAutocloseCooldownOneHour(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	h.settings.StopForecastMode = "ACTIVE"
	h.settings.RadarAutoCloseMode = "BAND3"

	seed := func(ageMinutes int) string {
		id := h.seedRealBot(t, symbol, 0)
		h.seedDwell(t, id, symbol)
		if _, err := h.pool.Exec(ctx,
			`UPDATE grid_bots SET unrealized_pnl_usdt = -0.75 WHERE id = $1`, id); err != nil {
			t.Fatalf("sink bot: %v", err)
		}
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO bot_execution_events (
				bot_id, bot_number, bot_source, symbol,
				event_type, details, created_at
			) VALUES ($1, 500, 'REAL', $2, 'RADAR_AUTOCLOSE',
				'{"mode":"BAND3","reason":"RADAR_AUTOCLOSE_BAND3"}'::jsonb,
				NOW() - make_interval(mins => $3))
		`, id, symbol, ageMinutes); err != nil {
			t.Fatalf("seed autoclose event: %v", err)
		}
		return id
	}

	recent := seed(30)
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, recent), autocloseScores())
	// The seed event exists by construction — only the close itself must be
	// blocked while the hour runs.
	var status string
	if err := h.pool.QueryRow(ctx,
		`SELECT status FROM grid_bots WHERE id = $1`, recent).Scan(&status); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if status != "RUNNING" {
		t.Fatalf("a 30m-old RADAR_AUTOCLOSE must block the close for the hour, got %s", status)
	}
	var events int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_AUTOCLOSE'
	`, recent).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("the blocked pass must not log a second RADAR_AUTOCLOSE, got %d", events)
	}

	stale := seed(120)
	h.worker.radarMaybeAutoclose(ctx, *h.settings, autocloseAction(symbol, stale), autocloseScores())
	if err := h.pool.QueryRow(ctx,
		`SELECT status FROM grid_bots WHERE id = $1`, stale).Scan(&status); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if status != "STOP_REQUESTED" {
		t.Fatalf("a 2h-old RADAR_AUTOCLOSE must no longer block, got %s", status)
	}
}

// (e) Parity: a PAPER bot flows through the identical gates and the identical
// Service.RequestBotClose path — settled COMPLETED with the RADAR_AUTOCLOSE
// reason and a bot_source=PAPER event.
func TestRadarAutoclosePaperParity(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_PAPER_USDT_PERP"
	var botID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt,
			realized_pnl_usdt, unrealized_pnl_usdt, opened_at
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999,
			0.25, -0.75, NOW() - INTERVAL '2 hours'
		)
		RETURNING id
	`, h.settings.ID, symbol).Scan(&botID); err != nil {
		t.Fatalf("seed paper bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = $1`, botID)
		_, _ = h.pool.Exec(ctx, `DELETE FROM bot_risk_snapshots WHERE bot_id = $1`, botID)
		_, _ = h.pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID)
	})
	h.seedDwell(t, botID, symbol) // REAL-stamped dwell rows count for paper too: gate is per-bot, not per-source

	h.settings.StopForecastMode = "ACTIVE"
	h.settings.RadarAutoCloseMode = "BAND3"
	b := radarInput{
		botID: botID, botNumber: 500, botSource: "PAPER", symbol: symbol,
		direction: "NEUTRAL", price: decimal.NewFromInt(100),
		lower: decimal.NewFromInt(90), upper: decimal.NewFromInt(110),
		total: decimal.NewFromFloat(-0.5), inventorySide: 1,
	}
	h.worker.radarMaybeAutoclose(ctx, *h.settings, b, autocloseScores())

	var status, reason string
	if err := h.pool.QueryRow(ctx,
		`SELECT status, COALESCE(closed_reason,'') FROM paper_grid_bots WHERE id = $1`, botID).Scan(&status, &reason); err != nil {
		t.Fatalf("load paper bot: %v", err)
	}
	if status != "COMPLETED" || reason != "RADAR_AUTOCLOSE_BAND3" {
		t.Fatalf("paper bot must settle COMPLETED/RADAR_AUTOCLOSE_BAND3, got %s/%s", status, reason)
	}
	var source string
	if err := h.pool.QueryRow(ctx, `
		SELECT bot_source FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_AUTOCLOSE'
		ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&source); err != nil {
		t.Fatalf("paper RADAR_AUTOCLOSE event must be logged: %v", err)
	}
	if source != "PAPER" {
		t.Fatalf("paper autoclose event must carry bot_source=PAPER, got %s", source)
	}
}

// assertNotClosedAndNoEvent pins the negative branch: bot still RUNNING, no
// RADAR_AUTOCLOSE event, nothing queued.
func assertNotClosedAndNoEvent(t *testing.T, h *radarRealHarness, ctx context.Context, botID, msg string) {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(ctx,
		`SELECT status FROM grid_bots WHERE id = $1`, botID).Scan(&status); err != nil {
		t.Fatalf("%s: load bot: %v", msg, err)
	}
	if status != "RUNNING" {
		t.Fatalf("%s: bot must stay RUNNING, got %s", msg, status)
	}
	var events int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_AUTOCLOSE'
	`, botID).Scan(&events); err != nil {
		t.Fatalf("%s: count events: %v", msg, err)
	}
	if events != 0 {
		t.Fatalf("%s: no RADAR_AUTOCLOSE event expected, got %d", msg, events)
	}
}
