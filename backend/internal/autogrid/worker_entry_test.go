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
				"macdCrossedUp":    false,
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
				"macdCrossedDown":  false,
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
