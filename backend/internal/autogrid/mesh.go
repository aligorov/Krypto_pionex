package autogrid

import (
	"fmt"
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

// DynamicLeverageResult holds the determined leverage and human-readable explanation.
type DynamicLeverageResult struct {
	Leverage    int    `json:"leverage"`
	Reason      string `json:"reason"`
	IsScaleDown bool   `json:"isScaleDown"`
}

// ComputeDynamicLeverage computes safe volatility-adjusted leverage based on ATR and base leverage.
func ComputeDynamicLeverage(
	atrPct float64,
	baseLev int,
) DynamicLeverageResult {
	if baseLev < 1 {
		baseLev = 1
	}
	if baseLev > 10 {
		baseLev = 10
	}

	// Normal Volatility (ATR <= 4.0%): Keep full base leverage
	if atrPct <= 4.0 {
		return DynamicLeverageResult{
			Leverage:    baseLev,
			Reason:      fmt.Sprintf("Базовое (ATR %.1f%%)", atrPct),
			IsScaleDown: false,
		}
	}

	// Elevated Volatility (4.0% < ATR <= 7.0%): Step down by 1x (min 2x)
	if atrPct <= 7.0 {
		scaled := baseLev - 1
		if scaled < 2 {
			scaled = 2
		}
		if scaled >= baseLev {
			return DynamicLeverageResult{
				Leverage:    baseLev,
				Reason:      fmt.Sprintf("Базовое (ATR %.1f%%)", atrPct),
				IsScaleDown: false,
			}
		}
		return DynamicLeverageResult{
			Leverage:    scaled,
			Reason:      fmt.Sprintf("ATR %.1f%% — снижено с %dx для защиты", atrPct, baseLev),
			IsScaleDown: true,
		}
	}

	// Extreme Volatility (ATR > 7.0%): Step down by 2x (min 2x)
	scaled := baseLev - 2
	if scaled < 2 {
		scaled = 2
	}
	if scaled >= baseLev {
		return DynamicLeverageResult{
			Leverage:    baseLev,
			Reason:      fmt.Sprintf("Базовое (ATR %.1f%%)", atrPct),
			IsScaleDown: false,
		}
	}
	return DynamicLeverageResult{
		Leverage:    scaled,
		Reason:      fmt.Sprintf("ATR %.1f%% (риск сквиза) — защита %dx", atrPct, scaled),
		IsScaleDown: true,
	}
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
