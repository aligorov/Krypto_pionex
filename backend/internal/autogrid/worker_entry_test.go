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
func TestStopEnvelopeExceeded(t *testing.T) {
	if stopEnvelopeExceeded(decimal.NewFromInt(40), decimal.NewFromInt(50)) {
		t.Fatalf("envelope exactly at 0.8×breaker must pass")
	}
	if !stopEnvelopeExceeded(decimal.NewFromFloat(40.01), decimal.NewFromInt(50)) {
		t.Fatalf("envelope above 0.8×breaker must block")
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
