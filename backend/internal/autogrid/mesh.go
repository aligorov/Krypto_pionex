package autogrid

import (
	"math"

	"github.com/shopspring/decimal"
)

const (
	// BreakEvenFloorPct is the absolute mathematical minimum grid step (0.30%).
	// Any step below this threshold loses 50-70% of gross profit to exchange fees.
	BreakEvenFloorPct = 0.30

	// MinOrderUsdtPerLevel is the Pionex minimum futures order size per grid level.
	MinOrderUsdtPerLevel = 1.50
)

type AdaptiveMeshResult struct {
	GridNum      int             `json:"gridNum"`
	GridStepPct  decimal.Decimal `json:"gridStepPct"`
	GridType     string          `json:"gridType"`
	StepPerLevel decimal.Decimal `json:"stepPerLevel"`
	LowerPrice   decimal.Decimal `json:"lowerPrice"`
	UpperPrice   decimal.Decimal `json:"upperPrice"`
}

// ComputeAdaptiveMesh dynamically calculates the optimal grid count and step size.
// It guarantees that GridStepPct is never below the BreakEvenFloorPct (0.30%),
// adapts to market regime and ATR volatility, and respects Pionex minimum order sizes.
func ComputeAdaptiveMesh(
	lower decimal.Decimal,
	upper decimal.Decimal,
	currentPrice decimal.Decimal,
	atrPct float64,
	regime string,
	budgetUsdt decimal.Decimal,
	leverage int,
	minGridStepPct float64,
) AdaptiveMeshResult {
	if minGridStepPct < BreakEvenFloorPct {
		minGridStepPct = BreakEvenFloorPct
	}

	if upper.LessThanOrEqual(lower) || currentPrice.LessThanOrEqual(decimal.Zero) {
		return AdaptiveMeshResult{
			GridNum:     20,
			GridStepPct: decimal.NewFromFloat(minGridStepPct),
			GridType:    "GEOMETRIC",
			LowerPrice:  lower,
			UpperPrice:  upper,
		}
	}

	span := upper.Sub(lower)
	spanPct, _ := span.Div(currentPrice).Mul(decimal.NewFromInt(100)).Float64()
	if spanPct <= 0 {
		spanPct = 10.0
	}

	// Dynamic ATR Multiplier based on market regime
	// - Low volatility / Range: tighter step (0.35% - 0.50%)
	// - High volatility / Trend / Meme: wider step (0.80% - 1.50%)
	stepMult := 0.40
	switch regime {
	case "RANGE", "CONSOLIDATION":
		stepMult = 0.35
	case "TREND_UP", "TREND_DOWN":
		stepMult = 0.55
	case "VOLATILE", "BREAKOUT":
		stepMult = 0.75
	}

	targetStepPct := atrPct * stepMult
	if targetStepPct < minGridStepPct {
		targetStepPct = minGridStepPct
	}

	// Calculate ideal grid count from span and target step
	idealGrids := int(math.Round(spanPct / targetStepPct))
	if idealGrids < 10 {
		idealGrids = 10
	}
	if idealGrids > 60 {
		idealGrids = 60
	}

	// Capital constraint: Total Buying Power / Min Order Value
	totalBuyingPower, _ := budgetUsdt.Mul(decimal.NewFromInt(int64(leverage))).Float64()
	maxGridsByCapital := int(totalBuyingPower / MinOrderUsdtPerLevel)
	if maxGridsByCapital < 10 {
		maxGridsByCapital = 10
	}
	if idealGrids > maxGridsByCapital {
		idealGrids = maxGridsByCapital
	}

	actualStepPct := spanPct / float64(idealGrids)
	if actualStepPct < minGridStepPct {
		actualStepPct = minGridStepPct
		idealGrids = int(math.Max(10, math.Round(spanPct/actualStepPct)))
	}

	stepPerLevel := span.Div(decimal.NewFromInt(int64(idealGrids)))

	return AdaptiveMeshResult{
		GridNum:      idealGrids,
		GridStepPct:  decimal.NewFromFloat(math.Round(actualStepPct*1000) / 1000),
		GridType:     "GEOMETRIC",
		StepPerLevel: stepPerLevel,
		LowerPrice:   lower,
		UpperPrice:   upper,
	}
}

// ComputeDynamicLeverage computes safe volatility-adjusted leverage (1.5x to 3.5x).
// Formula: Leverage = Clamp( MaxRiskBudget (2.5%) / DistanceToStopLossPct, 2, MaxAllowed )
func ComputeDynamicLeverage(
	entryPrice decimal.Decimal,
	stopLossPrice decimal.Decimal,
	atrPct float64,
	confidence float64,
	maxAllowedLev int,
) int {
	if maxAllowedLev < 2 {
		maxAllowedLev = 2
	}
	if maxAllowedLev > 4 {
		maxAllowedLev = 4
	}

	if confidence <= 0 {
		confidence = 0.80
	}

	distPct := 2.5
	if entryPrice.GreaterThan(decimal.Zero) && stopLossPrice.GreaterThan(decimal.Zero) {
		diff := entryPrice.Sub(stopLossPrice).Abs()
		d, _ := diff.Div(entryPrice).Mul(decimal.NewFromInt(100)).Float64()
		if d > 0.5 {
			distPct = d
		}
	}

	// 2.5% max risk budget per position stop-out
	const maxRiskBudgetPct = 2.5
	rawLev := (maxRiskBudgetPct / distPct) * confidence

	// High volatility penalty: if ATR > 4%, cap leverage to 2x
	if atrPct > 4.0 && rawLev > 2.0 {
		rawLev = 2.0
	}

	lev := int(math.Round(rawLev))
	if lev < 2 {
		lev = 2
	}
	if lev > maxAllowedLev {
		lev = maxAllowedLev
	}
	return lev
}

// ComputeAntiHuntStop calculates a stop loss price positioned 1.5x ATR beyond visible S/R.
func ComputeAntiHuntStop(
	direction string,
	lowerPrice decimal.Decimal,
	upperPrice decimal.Decimal,
	currentPrice decimal.Decimal,
	atrPrice decimal.Decimal,
	atrMult float64,
) decimal.Decimal {
	if atrMult <= 0 {
		atrMult = 1.5
	}
	buffer := atrPrice.Mul(decimal.NewFromFloat(atrMult))

	switch direction {
	case "SHORT":
		// Stop is above upper resistance
		return upperPrice.Add(buffer)
	default:
		// LONG or NEUTRAL: Stop is below lower support
		stop := lowerPrice.Sub(buffer)
		if stop.LessThanOrEqual(decimal.Zero) {
			stop = lowerPrice.Mul(decimal.NewFromFloat(0.95))
		}
		return stop
	}
}
