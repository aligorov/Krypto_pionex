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
	// hurstHardVeto 0.55 (was 0.60, v2.0.39): closed-ledger audit of
	// 2026-08-23..30 — NEUTRAL entries with Hurst ≥0.55 lost −$37.5 vs
	// +$10.1 won below (XLM 0.59 −12.69, EDEN 0.58 −6.89, BICO 0.58 −5.20,
	// DOS 0.59 −4.08). The 0.55-0.60 band was already dying.
	hurstHardVeto     = 0.55

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
// regime context. Weights: flow 0.20, momentum turn 0.15, squeeze release
// 0.15, fair-price stretch 0.10, Fibonacci pocket 0.15, MACD cross 0.15,
// Stochastic RSI extreme 0.10 for directional support; range support
// weights volatility phase, mean-reverting regime, and structural S/R bounds.
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

	// --- Volume flow & Divergence (0.20): sees accumulation/distribution before price moves.
	// Never fires without a Hurst gate: divergences against a strong trend are the documented trap.
	if result.HurstGate != HurstGateTrendDanger {
		if bundle.OBVDiv.Direction > 0 {
			divWeight := 0.20 * clamp(bundle.OBVDiv.Strength+0.4, 0, 1)
			if bundle.RSIDiv.Direction > 0 {
				divWeight = clamp(divWeight*1.25, 0, 0.20) // Dual OBV+RSI bullish divergence confirmation
				vote("dual_bullish_divergence", 1, divWeight, true, "price LL with OBV HL & RSI HL")
			} else {
				vote("obv_bullish_divergence", 1, divWeight, true, "price LL, OBV HL")
			}
			result.LongScore += divWeight
		} else if bundle.OBVDiv.Direction < 0 {
			divWeight := 0.20 * clamp(bundle.OBVDiv.Strength+0.4, 0, 1)
			if bundle.RSIDiv.Direction < 0 {
				divWeight = clamp(divWeight*1.25, 0, 0.20) // Dual OBV+RSI bearish divergence confirmation
				vote("dual_bearish_divergence", -1, divWeight, true, "price HH with OBV LH & RSI LH")
			} else {
				vote("obv_bearish_divergence", -1, divWeight, true, "price HH, OBV LH")
			}
			result.ShortScore += divWeight
		} else if bundle.RSIDiv.Direction > 0 {
			rsiWeight := 0.15 * bundle.RSIDiv.Strength
			vote("rsi_bullish_divergence", 1, rsiWeight, true, "price LL, RSI HL")
			result.LongScore += rsiWeight
		} else if bundle.RSIDiv.Direction < 0 {
			rsiWeight := 0.15 * bundle.RSIDiv.Strength
			vote("rsi_bearish_divergence", -1, rsiWeight, true, "price HH, RSI LH")
			result.ShortScore += rsiWeight
		}
	}

	// --- Early Momentum Turn (0.15): IFT-RSI crosses 1-2 bars before classic RSI.
	if bundle.IFT.CrossedUp {
		vote("ift_rsi_turn_up", 1, 0.15, true, "crossed -0.5 upward")
		result.LongScore += 0.15
	} else if bundle.IFT.CrossedDown {
		vote("ift_rsi_turn_down", -1, 0.15, true, "crossed +0.5 downward")
		result.ShortScore += 0.15
	}

	// --- Volatility Phase (0.15 directional / 0.35 range): squeeze release marks start of expansion.
	if bundle.Keltner.JustReleased {
		dir := bundle.Keltner.ReleaseDir
		if dir > 0 {
			vote("squeeze_release_up", 1, 0.15, true, "BB left Keltner upward")
			result.LongScore += 0.15
		} else if dir < 0 {
			vote("squeeze_release_down", -1, 0.15, true, "BB left Keltner downward")
			result.ShortScore += 0.15
		}
	}

	// --- Fair-Price Stretch (0.10): price stretched under/over the volume-weighted anchor.
	if bundle.AVWAP.ZScore <= -1.5 {
		vote("avwap_stretched_below", 1, 0.10, true, "z<=-1.5 vs anchored fair price")
		result.LongScore += 0.10
	} else if bundle.AVWAP.ZScore >= 1.5 {
		vote("avwap_stretched_above", -1, 0.10, true, "z>=+1.5 vs anchored fair price")
		result.ShortScore += 0.10
	}

	// --- Fibonacci Retracement & Golden Pocket (0.15): institutional pullback zone.
	if bundle.Fib.InGoldenPocket {
		if bundle.Fib.TrendDir == 1 {
			vote("fib_golden_pocket_long", 1, 0.15, true, "price in 0.618-0.786 pullback golden pocket")
			result.LongScore += 0.15
		} else if bundle.Fib.TrendDir == -1 {
			vote("fib_golden_pocket_short", -1, 0.15, true, "price in 0.618-0.786 relief golden pocket")
			result.ShortScore += 0.15
		}
	} else if bundle.Fib.DistancePct <= 0.6 {
		if bundle.Fib.TrendDir == 1 && bundle.Fib.NearRatio >= 0.382 {
			vote("fib_support_shelf", 1, 0.08, true, "price resting on key Fib support")
			result.LongScore += 0.08
		} else if bundle.Fib.TrendDir == -1 && bundle.Fib.NearRatio >= 0.382 {
			vote("fib_resist_shelf", -1, 0.08, true, "price testing key Fib resistance")
			result.ShortScore += 0.08
		}
	}

	// --- MACD Momentum & Crossover (0.15): trend confirmation and inflection detector.
	if bundle.MACD.CrossedUp {
		vote("macd_bullish_cross", 1, 0.15, true, "MACD line crossed above signal line")
		result.LongScore += 0.15
	} else if bundle.MACD.CrossedDown {
		vote("macd_bearish_cross", -1, 0.15, true, "MACD line crossed below signal line")
		result.ShortScore += 0.15
	} else if bundle.MACD.HistTurning {
		if bundle.MACD.Histogram < 0 && bundle.MACD.Histogram > bundle.MACD.PrevHist {
			vote("macd_hist_bullish_turn", 1, 0.08, true, "MACD histogram contracting upward")
			result.LongScore += 0.08
		} else if bundle.MACD.Histogram > 0 && bundle.MACD.Histogram < bundle.MACD.PrevHist {
			vote("macd_hist_bearish_turn", -1, 0.08, true, "MACD histogram contracting downward")
			result.ShortScore += 0.08
		}
	}

	// --- Stochastic RSI Extreme Reversal (0.10): early oversold/overbought hook.
	if bundle.StochRSI.CrossedUp {
		vote("stoch_rsi_oversold_cross", 1, 0.10, true, "%K hooked above %D from oversold")
		result.LongScore += 0.10
	} else if bundle.StochRSI.CrossedDown {
		vote("stoch_rsi_overbought_cross", -1, 0.10, true, "%K hooked below %D from overbought")
		result.ShortScore += 0.10
	}

	// --- Range Support: compression + mean-reverting memory + position inside the value area.
	rangeSupport := 0.0
	if bundle.Keltner.InSqueeze {
		vote("keltner_squeeze_on", 0, 0.35, true, "compressed volatility")
		rangeSupport += 0.35
	}
	if result.HurstGate == HurstGateGridFriendly {
		vote("hurst_mean_reverting", 0, 0.25, true, "H<0.45")
		rangeSupport += 0.25
	}
	if regime.BBWPercentile > 0 && regime.BBWPercentile < 25 {
		vote("bbw_low_percentile", 0, 0.25, true, "bands tighter than 75% of window")
		rangeSupport += 0.25
	}
	if bundle.SR.SupportStrength > 0.6 && bundle.SR.ResistStrength > 0.6 {
		vote("sr_bracket_confirmed", 0, 0.15, true, "price bounded by dual multi-touch S/R shelves")
		rangeSupport += 0.15
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
