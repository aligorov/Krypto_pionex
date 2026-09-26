package marketdata

import (
	"math"

	"github.com/shopspring/decimal"
)

// GridGeometry is the translation of a HAR volatility forecast into futures
// grid parameters. Higher forecast volatility widens the range and de-gears
// leverage; the step floor keeps every grid round trip profitable after
// fees. Confidence carries the HAR R² so downstream sizing can discount a
// weak fit.
type GridGeometry struct {
	RangePct   float64 // total grid range as % of price
	GridCount  int     // number of grid levels
	StepPct    float64 // step between adjacent levels in %
	Leverage   int     // recommended leverage (1-3)
	StopPct    float64 // stop-loss distance below the range in %
	Confidence float64 // HAR model R² (quality of the forecast)
}

// minPerLevelNotionalUsdt caps the grid level count so each level's order
// notional stays above a Pionex-executable size. At ~$2/level a real grid's
// orders drop below exchange minimums on many futures pairs — the pre-flight
// checkParams would reject the config in REAL mode, so the geometry must not
// produce it in the first place. v2.0.75: raised 5 → 8 to match the shared
// margin-density doctrine (MinGridLevelNotionalUSDT) so every level-count
// path — scanner, mesh, HAR, manual — agrees on the same economics.
const minPerLevelNotionalUsdt = 8.0

// ComputeGridGeometry converts a HAR volatility forecast into grid parameters.
//
//	forecastVolPct: annualized daily volatility in % (e.g. 45 = 45% a year)
//	harR2:          in-sample R² of the HAR fit, stored verbatim as Confidence
//	feeBps:         one-way fee+slippage in basis points (e.g. 7 = 0.07%)
//	budgetUsdt:     quote investment; <= 0 disables the per-level notional cap
//
// Mapping rationale:
//   - range  = 2.5 × daily volatility (covers roughly ±1.25σ of the next
//     day's move, so the grid is neither instantly escaped nor idle),
//   - step   ≥ 3 × fee+slippage (a round trip pays the feed twice; the third
//     multiple is the safety buffer),
//   - leverage falls as volatility rises: 3x only in calm regimes, 1x once
//     daily moves exceed 5%,
//   - level count is additionally capped so each level's order notional
//     (budget × leverage / levels) stays ≥ minPerLevelNotionalUsdt.
func ComputeGridGeometry(forecastVolPct float64, harR2 float64, feeBps float64, budgetUsdt float64) GridGeometry {
	// Annualized % → daily %: σ_daily = σ_annual / √365 (crypto trades 24/7).
	dailyVolPct := forecastVolPct / math.Sqrt(365)

	// Grid range = 2.5 × daily volatility.
	rangePct := dailyVolPct * 2.5
	if rangePct < 3.0 {
		rangePct = 3.0 // structural floor: below 3% a range grid barely spreads
	}
	if rangePct > 25.0 {
		rangePct = 25.0 // ceiling: beyond this a grid stops being a grid
	}

	// Step must clear fees: 2× for the round trip + 1× buffer.
	// feeBps/100 converts basis points to percent. The absolute minimum is
	// the fee-gate density floor derived from the SAME feeBps the caller
	// passes (v2.0.93 FIX-A parameterization: 2× the round trip at this
	// one-way fee+slippage pair) — cheaper fees must not produce sub-floor
	// steps the rest of the fleet no longer makes, and pricier fees must
	// raise the floor in lockstep with the deploy gate. Degenerate feeBps
	// (≤ 0) degrades to the fleet-default floor inside FeeGateStepFloorPct.
	minStepPct := feeBps * 3.0 / 100.0
	if floorPct := FeeGateStepFloorPct(feeBps, 0); minStepPct < floorPct {
		minStepPct = floorPct
	}

	// Leverage inversely proportional to volatility.
	leverage := 3
	if dailyVolPct > 10.0 {
		leverage = 2 // extreme volatility → 2x floor
	}
	if dailyVolPct < 3.0 {
		leverage = 4 // low volatility → 4x
	}

	// Number of fee-viable steps that fit in the range. floor(range/step)
	// guarantees the realized step (= range/count) never drops below
	// minStepPct unless a floor below kicks in.
	gridCount := int(rangePct / minStepPct)
	if budgetUsdt > 0 {
		// Per-level notional cap: budget × leverage / levels ≥ floor.
		maxByBudget := int(budgetUsdt * float64(leverage) / minPerLevelNotionalUsdt)
		if gridCount > maxByBudget {
			gridCount = maxByBudget
		}
	}
	// v2.0.75: the 8..100 window joins the shared 6..500 clamp of the
	// margin-density doctrine — a thin-notional HAR forecast may legally
	// land below 8 levels, and 100 was an arbitrary ceiling below the
	// exchange's own 500.
	if gridCount < GridLevelsMin {
		gridCount = GridLevelsMin
	}
	if gridCount > GridLevelsMax {
		gridCount = GridLevelsMax
	}

	// Stop at half the range below the lower bound: outside normal grid
	// oscillation, close enough to cut a regime break early.
	stopPct := rangePct * 0.5

	return GridGeometry{
		RangePct:   rangePct,
		GridCount:  gridCount,
		StepPct:    rangePct / float64(gridCount),
		Leverage:   leverage,
		StopPct:    stopPct,
		Confidence: harR2,
	}
}

// ComputeGaussianGridLevels computes grid levels clustered according to a Gaussian/volatility-adaptive
// density distribution around the center price. Orders are denser near center where ~68% of price oscillations
// take place, and expand toward the boundary tails to reduce idle capital and maximize round-trip frequency.
func ComputeGaussianGridLevels(lower, upper, center decimal.Decimal, gridNum int, precision int32) []decimal.Decimal {
	if gridNum < 2 {
		return []decimal.Decimal{lower.Round(precision), upper.Round(precision)}
	}
	if center.LessThanOrEqual(lower) || center.GreaterThanOrEqual(upper) {
		center = lower.Add(upper).Div(decimal.NewFromInt(2))
	}

	levels := make([]decimal.Decimal, gridNum)
	centerF, _ := center.Float64()
	lowerF, _ := lower.Float64()
	upperF, _ := upper.Float64()

	// Power exponent: gamma > 1 compresses levels near center.
	// gamma = 1.4 provides optimal balance of central density without starving outer wings.
	gamma := 1.4

	for i := 0; i < gridNum; i++ {
		// Normalized position u from -1 (at i=0) to +1 (at i=gridNum-1)
		u := -1.0 + 2.0*float64(i)/float64(gridNum-1)
		sign := 1.0
		if u < 0 {
			sign = -1.0
		}
		warped := sign * math.Pow(math.Abs(u), gamma)

		var priceF float64
		if warped < 0 {
			priceF = centerF + warped*(centerF-lowerF)
		} else {
			priceF = centerF + warped*(upperF-centerF)
		}

		levels[i] = decimal.NewFromFloat(priceF).Round(precision)
	}

	// Guarantee exact boundary anchoring
	levels[0] = lower.Round(precision)
	levels[gridNum-1] = upper.Round(precision)
	return levels
}
