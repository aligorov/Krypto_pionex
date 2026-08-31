package autogrid

import (
	"fmt"
	"math"
	"strings"

	"github.com/shopspring/decimal"
)

const (
	// BreakEvenFloorPct is the minimum profitable grid step (0.80%).
	// Sub-0.80% micro-steps generate pennies that are wiped out by slippage and fees.
	BreakEvenFloorPct = 0.80

	// MinOrderUsdtPerLevel is the Pionex minimum futures order size per grid level.
	MinOrderUsdtPerLevel = 2.00
)

type AdaptiveMeshResult struct {
	GridNum      int             `json:"gridNum"`
	GridStepPct  decimal.Decimal `json:"gridStepPct"`
	GridType     string          `json:"gridType"`
	StepPerLevel decimal.Decimal `json:"stepPerLevel"`
	LowerPrice   decimal.Decimal `json:"lowerPrice"`
	UpperPrice   decimal.Decimal `json:"upperPrice"`
}

// ComputeAdaptiveMesh dynamically calculates the optimal high-yield grid count and step size.
// It sizes grids with 6 to 14 solid levels ($20-$40/level on $200) and 1.0%-2.5% step sizes,
// ensuring each grid crossing generates substantial cash profit ($0.70 - $1.50 per fill).
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
			GridNum:     8,
			GridStepPct: decimal.NewFromFloat(minGridStepPct),
			GridType:    "GEOMETRIC",
			LowerPrice:  lower,
			UpperPrice:  upper,
		}
	}

	span := upper.Sub(lower)
	spanPct, _ := span.Div(currentPrice).Mul(decimal.NewFromInt(100)).Float64()
	if spanPct <= 0 {
		spanPct = 12.0
	}

	// Meaty Step Multiplier based on market regime:
	// - Range: 0.65 * ATR (giving 1.0% - 1.8% step for fat $0.70-$1.20 fills)
	// - Trend: 1.00 * ATR (giving 1.8% - 2.8% step for momentum continuation)
	// - Volatile / Breakout: 1.30 * ATR (giving 2.5% - 4.0% step)
	stepMult := 0.70
	switch regime {
	case "RANGE", "CONSOLIDATION":
		stepMult = 0.65
	case "TREND_UP", "TREND_DOWN":
		stepMult = 1.00
	case "VOLATILE", "BREAKOUT":
		stepMult = 1.30
	}

	targetStepPct := atrPct * stepMult
	if targetStepPct < minGridStepPct {
		targetStepPct = minGridStepPct
	}

	// Calculate ideal grid count (target 6 to 14 solid levels)
	idealGrids := int(math.Round(spanPct / targetStepPct))
	if idealGrids < 6 {
		idealGrids = 6
	}
	if idealGrids > 14 {
		idealGrids = 14
	}

	// Capital constraint: Total Buying Power / Min Order Value
	totalBuyingPower, _ := budgetUsdt.Mul(decimal.NewFromInt(int64(leverage))).Float64()
	maxGridsByCapital := int(totalBuyingPower / MinOrderUsdtPerLevel)
	if maxGridsByCapital < 6 {
		maxGridsByCapital = 6
	}
	if idealGrids > maxGridsByCapital {
		idealGrids = maxGridsByCapital
	}

	actualStepPct := spanPct / float64(idealGrids)
	if actualStepPct < minGridStepPct {
		actualStepPct = minGridStepPct
		idealGrids = int(math.Max(6, math.Round(spanPct/actualStepPct)))
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
	spanPct float64,
) DynamicLeverageResult {
	if baseLev < 1 {
		baseLev = 1
	}
	if baseLev > 10 {
		baseLev = 10
	}

	// v2.0.52 narrow-span de-gear: on a span <7% the maxLoss cap sits a
	// fixed ~2% from entry at 4x (audit 2026-08-31: 6/10 live bots,
	// sigma24h >= stop distance, P(stop/24h) 41-78%, -8.02 JUP class).
	// A 2x cap moves the stop outside the one-day noise band; the same
	// audit showed the wide AI-kit grids (11-17%) keep full leverage.
	if spanPct > 0 && spanPct < 7.0 && baseLev > 2 {
		return DynamicLeverageResult{
			Leverage:    2,
			Reason:      fmt.Sprintf("Узкий спан %.1f%% — де-гир 2x (стоп вне зоны шума)", spanPct),
			IsScaleDown: true,
		}
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

	switch strings.ToUpper(strings.TrimSpace(direction)) {
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
