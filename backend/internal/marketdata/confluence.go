package marketdata

// Confluence verdicts: the engine aggregates INDEPENDENT information classes
// (regime memory, volume flow, one momentum voice, fair-price stretch,
// volatility phase) into a bounded verdict. Release 1 uses it as a score
// multiplier plus a direction veto — never as a mandatory trigger; the hard
// event gate arrives only after paper-mode lead-time statistics (release 2).
const (
	ConfluenceSupportLong  = "SUPPORT_LONG"
	ConfluenceSupportShort = "SUPPORT_SHORT"
	ConfluenceSupportRange = "SUPPORT_RANGE"
	ConfluenceConflict     = "CONFLICT"
	ConfluenceNeutral      = "NEUTRAL"

	HurstGateGridFriendly = "GRID_FRIENDLY" // H < 0.45, mean reversion dominates
	HurstGateNeutral      = "HURST_NEUTRAL"
	HurstGateTrendDanger  = "TREND_DANGER" // H > 0.58, grids bleed inventory
)

// Hurst thresholds follow the quant research (DFA on 128-200 bars):
// crypto intraday H oscillates around 0.5; sustained readings above ~0.58
// mark trend regimes that load one-sided grid inventory, below ~0.45 mark
// mean-reverting regimes where neutral grids harvest oscillation.
const (
	hurstGridFriendly = 0.45
	hurstTrendDanger  = 0.58
	hurstHardVeto     = 0.60

	confluenceSupportThreshold = 0.50
	confluenceConflictFloor    = 0.45
)

type ConfluenceVote struct {
	Name      string  `json:"name"`
	Direction float64 `json:"direction"` // +1 long, -1 short, 0 range
	Weight    float64 `json:"weight"`
	Fired     bool    `json:"fired"`
	Note      string  `json:"note,omitempty"`
}

type ConfluenceResult struct {
	Verdict    string           `json:"verdict"`
	Strength   float64          `json:"strength"` // 0..1 weighted support for the verdict
	LongScore  float64          `json:"longScore"`
	ShortScore float64          `json:"shortScore"`
	RangeScore float64          `json:"rangeScore"`
	HurstGate  string           `json:"hurstGate"`
	Votes      []ConfluenceVote `json:"votes"`
}

// EvaluateConfluence aggregates the independent indicator classes with the
// regime context. Weights: flow 0.30, momentum turn 0.25, squeeze release
// 0.25, fair-price stretch 0.20 for directional support; range support
// weights volatility phase and mean-reverting regime.
func EvaluateConfluence(regime RegimeResult, bundle IndicatorBundle) ConfluenceResult {
	result := ConfluenceResult{Verdict: ConfluenceNeutral, HurstGate: HurstGateNeutral}
	switch {
	case !bundle.HurstOK:
		result.HurstGate = HurstGateNeutral
	case bundle.Hurst < hurstGridFriendly:
		result.HurstGate = HurstGateGridFriendly
	case bundle.Hurst > hurstTrendDanger:
		result.HurstGate = HurstGateTrendDanger
	}

	vote := func(name string, direction, weight float64, fired bool, note string) {
		result.Votes = append(result.Votes, ConfluenceVote{
			Name: name, Direction: direction, Weight: weight, Fired: fired, Note: note,
		})
	}

	// --- Volume flow (0.30): the only component that sees accumulation
	// before price moves. Never fires without a Hurst gate: divergences
	// against a strong trend are the documented trap.
	if bundle.OBVDiv.Direction > 0 && result.HurstGate != HurstGateTrendDanger {
		vote("obv_bullish_divergence", 1, 0.30*clamp(bundle.OBVDiv.Strength+0.4, 0, 1), true, "price LL, OBV HL")
		result.LongScore += 0.30 * clamp(bundle.OBVDiv.Strength+0.4, 0, 1)
	} else if bundle.OBVDiv.Direction < 0 && result.HurstGate != HurstGateTrendDanger {
		vote("obv_bearish_divergence", -1, 0.30*clamp(bundle.OBVDiv.Strength+0.4, 0, 1), true, "price HH, OBV LH")
		result.ShortScore += 0.30 * clamp(bundle.OBVDiv.Strength+0.4, 0, 1)
	}

	// --- Momentum turn (0.25): the single oscillator voice, kept because
	// IFT-RSI crosses 1-2 bars before classic RSI reads.
	if bundle.IFT.CrossedUp {
		vote("ift_rsi_turn_up", 1, 0.25, true, "crossed -0.5 upward")
		result.LongScore += 0.25
	} else if bundle.IFT.CrossedDown {
		vote("ift_rsi_turn_down", -1, 0.25, true, "crossed +0.5 downward")
		result.ShortScore += 0.25
	}

	// --- Volatility phase (0.25 directional / 0.30 range): squeeze release
	// marks the start of expansion; the still-squeezed state is the range
	// grid's ideal entry window.
	if bundle.Keltner.JustReleased {
		dir := bundle.Keltner.ReleaseDir
		if dir > 0 {
			vote("squeeze_release_up", 1, 0.25, true, "BB left Keltner upward")
			result.LongScore += 0.25
		} else if dir < 0 {
			vote("squeeze_release_down", -1, 0.25, true, "BB left Keltner downward")
			result.ShortScore += 0.25
		}
	}

	// --- Fair-price stretch (0.20): price stretched under/over the
	// volume-weighted anchor tends to mean-revert toward it.
	if bundle.AVWAP.ZScore <= -1.5 {
		vote("avwap_stretched_below", 1, 0.20, true, "z<=-1.5 vs anchored fair price")
		result.LongScore += 0.20
	} else if bundle.AVWAP.ZScore >= 1.5 {
		vote("avwap_stretched_above", -1, 0.20, true, "z>=+1.5 vs anchored fair price")
		result.ShortScore += 0.20
	}

	// --- Range support: compression + mean-reverting memory + position
	// inside the value area. These conditions describe the neutral grid's
	// native habitat, not a direction.
	rangeSupport := 0.0
	if bundle.Keltner.InSqueeze {
		vote("keltner_squeeze_on", 0, 0.40, true, "compressed volatility")
		rangeSupport += 0.40
	}
	if result.HurstGate == HurstGateGridFriendly {
		vote("hurst_mean_reverting", 0, 0.30, true, "H<0.45")
		rangeSupport += 0.30
	}
	if regime.BBWPercentile > 0 && regime.BBWPercentile < 25 {
		vote("bbw_low_percentile", 0, 0.30, true, "bands tighter than 75% of window")
		rangeSupport += 0.30
	}
	result.RangeScore = clamp(rangeSupport, 0, 1)

	result.LongScore = clamp(result.LongScore, 0, 1)
	result.ShortScore = clamp(result.ShortScore, 0, 1)

	// Directional conflict (both sides meaningfully supported) is itself
	// information: size down, never auto-pick a side.
	if result.LongScore >= confluenceConflictFloor && result.ShortScore >= confluenceConflictFloor {
		result.Verdict = ConfluenceConflict
		result.Strength = clamp(result.LongScore+result.ShortScore-1, 0, 1)
		return result
	}
	switch {
	case result.LongScore >= confluenceSupportThreshold && result.LongScore >= result.RangeScore:
		result.Verdict = ConfluenceSupportLong
		result.Strength = result.LongScore
	case result.ShortScore >= confluenceSupportThreshold && result.ShortScore >= result.RangeScore:
		result.Verdict = ConfluenceSupportShort
		result.Strength = result.ShortScore
	case result.RangeScore >= confluenceSupportThreshold:
		result.Verdict = ConfluenceSupportRange
		result.Strength = result.RangeScore
	default:
		result.Verdict = ConfluenceNeutral
		result.Strength = math_Max(result.LongScore, result.ShortScore, result.RangeScore) * 0.5
	}
	return result
}

func math_Max(values ...float64) float64 {
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best
}

// HurstHardVetoNeutral reports whether the regime memory hard-blocks a fresh
// NEUTRAL grid entry: a persistently trending series loads one-sided
// inventory, which is exactly the failure the daily-loss breaker cannot see
// until the damage is done.
func HurstHardVetoNeutral(bundle IndicatorBundle) bool {
	return bundle.HurstOK && bundle.Hurst > hurstHardVeto
}
