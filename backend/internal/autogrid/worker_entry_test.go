package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestIsEntryTimingFavorable(t *testing.T) {
	// Long in Golden Pocket
	longInPocket := Candidate{
		RecommendedTrend: "long",
		CurrentPrice:     decimal.NewFromFloat(100),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 50.0,
			"confluence": map[string]any{
				"fibInGoldenPocket": true,
			},
		},
	}
	if !isEntryTimingFavorable(longInPocket) {
		t.Fatalf("expected long in Golden Pocket to be favorable")
	}

	// Long at top of channel without momentum (rangePos 78%) -> must be rejected
	longAtTop := Candidate{
		RecommendedTrend: "long",
		CurrentPrice:     decimal.NewFromFloat(105.6),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 78.0,
			"confluence": map[string]any{
				"fibInGoldenPocket": false,
				"macdCrossedUp":     false,
			},
		},
	}
	if isEntryTimingFavorable(longAtTop) {
		t.Fatalf("expected long at 78%% without momentum to be blocked by Anti-FOMO")
	}

	// Long at 68% with MACD momentum -> allowed
	longMomentum := Candidate{
		RecommendedTrend: "long",
		CurrentPrice:     decimal.NewFromFloat(103.6),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 68.0,
			"confluence": map[string]any{
				"macdCrossedUp": true,
			},
		},
	}
	if !isEntryTimingFavorable(longMomentum) {
		t.Fatalf("expected long at 68%% with MACD cross to be favorable")
	}

	// Long directly under resistance wall (wall at 100.2, price at 100.0 -> 0.2% away < 0.4%) -> rejected
	longUnderWall := Candidate{
		RecommendedTrend: "long",
		CurrentPrice:     decimal.NewFromFloat(100.0),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 50.0,
			"confluence": map[string]any{
				"srNearestResist": 100.2,
			},
		},
	}
	if isEntryTimingFavorable(longUnderWall) {
		t.Fatalf("expected long directly under resistance wall to be blocked")
	}

	// Short in Golden Pocket -> allowed
	shortInPocket := Candidate{
		RecommendedTrend: "short",
		CurrentPrice:     decimal.NewFromFloat(100),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 50.0,
			"confluence": map[string]any{
				"fibInGoldenPocket": true,
			},
		},
	}
	if !isEntryTimingFavorable(shortInPocket) {
		t.Fatalf("expected short in Golden Pocket to be favorable")
	}

	// Short at bottom of channel without momentum (rangePos 22%) -> rejected
	shortAtBottom := Candidate{
		RecommendedTrend: "short",
		CurrentPrice:     decimal.NewFromFloat(94.4),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 22.0,
			"confluence": map[string]any{
				"fibInGoldenPocket": false,
				"macdCrossedDown":   false,
			},
		},
	}
	if isEntryTimingFavorable(shortAtBottom) {
		t.Fatalf("expected short at 22%% without momentum to be blocked by Anti-FOMO")
	}

	// Neutral at center (50%) -> allowed; at boundary (85%) -> rejected
	neutralCenter := Candidate{
		RecommendedTrend: "no_trend",
		CurrentPrice:     decimal.NewFromFloat(100),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 50.0,
		},
	}
	if !isEntryTimingFavorable(neutralCenter) {
		t.Fatalf("expected neutral at 50%% to be favorable")
	}

	neutralEdge := Candidate{
		RecommendedTrend: "no_trend",
		CurrentPrice:     decimal.NewFromFloat(107),
		LowerPrice:       decimal.NewFromFloat(90),
		UpperPrice:       decimal.NewFromFloat(110),
		ModelAssumptions: map[string]any{
			"rangePositionPct": 85.0,
		},
	}
	if isEntryTimingFavorable(neutralEdge) {
		t.Fatalf("expected neutral at 85%% to be rejected")
	}
}

// TestStopEnvelopeExceeded pins the envelope verdict's boundary: a fleet
// sitting exactly at 0.8× the breaker still deploys (the live 10×$4 paper
// fleet on a $50 breaker must keep rotating), a cent above it is refused.
// The second pair pins the reservation arithmetic the deploy gate uses
// since the full-stop fix: fleet 36 + the candidate's FULL 8 overflows the
// $40 ceiling, even though the tranche-1 half it would store (4) fits —
// the old half-stop reservation passed exactly here and newborn bots were
// stranded at half capital until a slot died.
func TestStopEnvelopeExceeded(t *testing.T) {
	if stopEnvelopeExceeded(decimal.NewFromInt(40), decimal.NewFromInt(50)) {
		t.Fatalf("envelope exactly at 0.8×breaker must pass")
	}
	if !stopEnvelopeExceeded(decimal.NewFromFloat(40.01), decimal.NewFromInt(50)) {
		t.Fatalf("envelope above 0.8×breaker must block")
	}
	if stopEnvelopeExceeded(decimal.NewFromInt(36).Add(decimal.NewFromInt(4)), decimal.NewFromInt(50)) {
		t.Fatalf("fleet 36 + tranche-1 half 4 sits at the ceiling and must pass")
	}
	if !stopEnvelopeExceeded(decimal.NewFromInt(36).Add(decimal.NewFromInt(8)), decimal.NewFromInt(50)) {
		t.Fatalf("fleet 36 + FULL candidate stop 8 must block (44 > 0.8×50)")
	}
}

// stopEnvelopeFleetBot is one RUNNING bot in the stationarity simulation:
// stored is its max_loss_usdt as persisted (the tranche-1 half until the
// top-up doubles it), full is its post-tranche-2 stop.
type stopEnvelopeFleetBot struct {
	stored decimal.Decimal
	full   decimal.Decimal
}

// stopEnvelopeFleet mirrors the two SQL envelope sums over paper_grid_bots:
// sum of stored stops, optionally excluding one bot, plus a reservation.
func stopEnvelopeFleet(bots []stopEnvelopeFleetBot, exclude int, reserve decimal.Decimal) decimal.Decimal {
	sum := decimal.Zero
	for i, b := range bots {
		if i != exclude {
			sum = sum.Add(b.stored)
		}
	}
	return sum.Add(reserve)
}

// runStopEnvelopeSeries walks deploys and tranche-2 top-ups through the
// same verdicts the production gates apply (stopEnvelopeExceeded over the
// same sums; the tranche-2 per-bot cap tranche2MaxLossCapUSDT), asserting
// the stationarity invariant after every step.
func runStopEnvelopeSeries(t *testing.T, fullStops []decimal.Decimal, breaker decimal.Decimal) []stopEnvelopeFleetBot {
	t.Helper()
	limit := breaker.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction))
	var fleet []stopEnvelopeFleetBot
	step := func() {
		t.Helper()
		for i, b := range fleet {
			if got := stopEnvelopeFleet(fleet, i, b.full); got.GreaterThan(limit) {
				t.Fatalf("stationarity violated: Σ stored excl bot %d (%s) + its full stop %s = %s > %s; fleet=%v",
					i, stopEnvelopeFleet(fleet, i, decimal.Zero), b.full, got, limit, fleet)
			}
		}
		if got := stopEnvelopeFleet(fleet, -1, decimal.Zero); got.GreaterThan(limit) {
			t.Fatalf("stored envelope %s must never exceed %s, fleet=%v", got, limit, fleet)
		}
	}
	for _, full := range fullStops {
		// Deploy gate: reserve the FULL stop — the amount tranche-2 doubles
		// the stored half to.
		if stopEnvelopeExceeded(stopEnvelopeFleet(fleet, -1, full), breaker) {
			continue // candidate refused: the envelope cannot fit its full stop
		}
		fleet = append(fleet, stopEnvelopeFleetBot{stored: full.Div(decimal.NewFromInt(2)), full: full})
		step()
		// Tranche-2 gate: per-bot cap first, then the fleet envelope over
		// the DOUBLED stop (the manage loop's effMaxLoss).
		eff := fleet[len(fleet)-1].full
		if eff.GreaterThan(decimal.NewFromFloat(tranche2MaxLossCapUSDT)) ||
			stopEnvelopeExceeded(stopEnvelopeFleet(fleet, len(fleet)-1, eff), breaker) {
			continue // top-up gated: the bot stays at its tranche-1 half
		}
		fleet[len(fleet)-1].stored = eff
		step()
	}
	return fleet
}

// TestStopEnvelopeStationaryInvariant pins the property the full-stop
// reservation buys: after any series of deploys and tranche-2 top-ups on a
// $50 breaker, the fleet can never sit in a state where the stored stops
// plus any live bot's full stop overflow the $40 ceiling — i.e., no live
// bot's doubling is born stranded (the OP/ASTER pattern: a tranche-2 that
// fits only after some other bot dies). Scenarios: five 2x bots ($8 full
// stop each, ending exactly at the ceiling), and a mix led by a 4x $16
// bot whose top-up the per-bot cap refuses.
func TestStopEnvelopeStationaryInvariant(t *testing.T) {
	breaker := decimal.NewFromInt(50)

	five := runStopEnvelopeSeries(t,
		[]decimal.Decimal{
			decimal.NewFromInt(8), decimal.NewFromInt(8), decimal.NewFromInt(8),
			decimal.NewFromInt(8), decimal.NewFromInt(8),
		}, breaker)
	if len(five) != 5 {
		t.Fatalf("5×$8 scenario must deploy all five bots, got %d", len(five))
	}
	// All five tranche-2s fit at equality: the fleet stores 5×$8 = $40.
	for _, b := range five {
		if !b.stored.Equal(decimal.NewFromInt(8)) {
			t.Fatalf("5×$8 scenario: every top-up must complete, stored=%s", b.stored)
		}
	}

	// The mix: the 4x $16 candidate lands first (its $16 top-up is refused
	// by the per-bot cap, so it keeps an $8 half on the books), then $8
	// candidates rotate in — three fit before a fourth deploy would push
	// the 4x bot's reservation (Σ stored excl + $16) past $40.
	mix := runStopEnvelopeSeries(t,
		[]decimal.Decimal{
			decimal.NewFromInt(16),
			decimal.NewFromInt(8), decimal.NewFromInt(8), decimal.NewFromInt(8),
		}, breaker)
	if len(mix) != 4 {
		t.Fatalf("mix scenario must deploy all four bots, got %d", len(mix))
	}
	if !mix[0].stored.Equal(decimal.NewFromInt(8)) {
		t.Fatalf("the 4x $16 top-up must be cap-refused, stored=%s", mix[0].stored)
	}
	// End state stores 8 + 3×8 = 32: the 4x bot's reservation lands exactly
	// at the ceiling (24 + 16 = 40), every $8 bot has doubled.
	if got := stopEnvelopeFleet(mix, -1, decimal.Zero); !got.Equal(decimal.NewFromInt(32)) {
		t.Fatalf("mix scenario end state must store $32, got %s", got)
	}

	// A $16 candidate against a fleet already storing 4×$8 = $32 must be
	// REFUSED at deploy (32 + 16 = 48 > 40); the old half-stop reservation
	// (32 + 8 = 40) admitted exactly this candidate and stranded it.
	pre := []decimal.Decimal{decimal.NewFromInt(8), decimal.NewFromInt(8), decimal.NewFromInt(8), decimal.NewFromInt(8)}
	var fleet []stopEnvelopeFleetBot
	for _, full := range pre {
		fleet = append(fleet, stopEnvelopeFleetBot{stored: full, full: full})
	}
	if !stopEnvelopeExceeded(stopEnvelopeFleet(fleet, -1, decimal.NewFromInt(16)), breaker) {
		t.Fatalf("the $16 deploy after 4×$8 must be refused: 32 + 16 = 48 > 40")
	}
	if stopEnvelopeExceeded(stopEnvelopeFleet(fleet, -1, decimal.NewFromInt(8)), breaker) {
		t.Fatalf("sanity: an $8 redeploy at 32 + 8 = 40 stays at the ceiling and passes")
	}
}

// TestScheduledScanArguments pins the scheduled scan's command arguments:
// quiet window queues a plain scan, an active cascade window queues
// cascadeShort semantics so the SCHEDULED scan's shorts keep the R1/F9
// exemption the out-of-turn cascade scan already has.
func TestScheduledScanArguments(t *testing.T) {
	if got := scheduledScanArguments(false); got != `'{}'::jsonb` {
		t.Fatalf("quiet window must queue a plain scan, got %s", got)
	}
	if got := scheduledScanArguments(true); got != `'{"cascadeShort": true}'::jsonb` {
		t.Fatalf("active cascade window must queue cascadeShort semantics, got %s", got)
	}
}
