package autogrid

import (
	"math"
	"strings"
)

// v2.0.161 directional-trend exemption (разлочка направленных сеток).
//
// Prod 2026-09-29: the scanner accepted six clean directional candidates
// (AERO/VVV/HYPE/USOX short, CRWVX/ALABX long) and the deployer skipped
// every one at the RV gate ("volatility expansion", ratio 1.5–4.0) — the
// fleet stayed 100% NEUTRAL in a trending tape while the survivor
// consensus says futures neutral grids are the first thing to avoid.
//
// v2.0.148 already proved the shape on the beta-down cohort: for a pair
// with its own CONFIRMED downtrend, the RV expansion IS the move the short
// is paid to ride (counterfactual 4.3:1), so the RV gate stands down and
// entry-timing lifts (a confirmed downtrend sits at the channel bottom by
// construction — the 40–90% short zone would reject exactly the trades
// the trend offers). Every later gate stays armed.
//
// v2.0.161 widens the same stand-down to ANY confirmed own-direction
// candidate, symmetric:
//   - SHORT: own strong downtrend (ADX > 22 or |EMA slope| > 0.5, slope < 0)
//     → exempt from RV + entry-timing, cohort marker dirTrendShort.
//   - LONG: own strong uptrend AND BTC not in TREND_DOWN (the beta-gate
//     would pause it anyway — no exemption may bypass a market-wide veto)
//     → exempt, cohort marker dirTrendLong.
//
// betaDownExempt (BTC-confirmed, 4.3:1-proven) keeps its own marker so the
// 14-day cohort analytics can partition the wider class from the proven
// one and roll back on evidence.
const (
	dirTrendMinADX      = 22.0
	dirTrendMinSlopePct = 0.5
)

type dirTrendVerdict struct {
	Exempt bool
	Cohort string // "betaDownExempt" | "dirTrendShort" | "dirTrendLong"
}

// directionalTrendExempt is the single stand-down oracle for the RV and
// entry-timing gates. betaDownShortExempt keeps priority (its cohort is
// prod-proven); the wider own-trend class follows the same thresholds.
func directionalTrendExempt(candidate Candidate, btcTrendDown bool) dirTrendVerdict {
	if betaDownShortExempt(candidate, btcTrendDown) {
		return dirTrendVerdict{Exempt: true, Cohort: "betaDownExempt"}
	}
	adx, _ := candidate.ModelAssumptions["adx"].(float64)
	slope, _ := candidate.ModelAssumptions["emaSlopePct"].(float64)
	strongTrend := adx > dirTrendMinADX || math.Abs(slope) > dirTrendMinSlopePct
	switch strings.ToLower(strings.TrimSpace(candidate.RecommendedTrend)) {
	case "short":
		if strongTrend && slope < 0 {
			return dirTrendVerdict{Exempt: true, Cohort: "dirTrendShort"}
		}
	case "long":
		if btcTrendDown {
			return dirTrendVerdict{}
		}
		if !strongTrend || slope <= 0 {
			return dirTrendVerdict{}
		}
		// v2.0.162 (operator override, unlock plan): the exempt long cohort
		// may enter on breakouts up to 90% of the channel — impulse entries
		// live in the upper channel by construction (the 161-review 75% cap
		// would have re-blocked exactly the breakout class the operator is
		// unlocking). The RSI ≤ 70 overheating cap STAYS — that is the actual
		// FOMO trap; channel height is not.
		rsi, _ := candidate.ModelAssumptions["rsi"].(float64)
		if rsi > 70.0 {
			return dirTrendVerdict{}
		}
		if pos, ok := candidate.ModelAssumptions["rangePositionPct"].(float64); ok && pos > 90.0 {
			return dirTrendVerdict{}
		}
		return dirTrendVerdict{Exempt: true, Cohort: "dirTrendLong"}
	}
	return dirTrendVerdict{}
}
