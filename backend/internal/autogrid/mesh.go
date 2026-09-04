package autogrid

import (
	"fmt"
	"math"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

type AdaptiveMeshResult struct {
	GridNum      int             `json:"gridNum"`
	GridStepPct  decimal.Decimal `json:"gridStepPct"`
	GridType     string          `json:"gridType"`
	StepPerLevel decimal.Decimal `json:"stepPerLevel"`
	LowerPrice   decimal.Decimal `json:"lowerPrice"`
	UpperPrice   decimal.Decimal `json:"upperPrice"`
}

// ComputeAdaptiveMesh sizes the grid level count under the margin-density
// doctrine (v2.0.75): step = max(0.25%, the step at which every level still
// carries ≥ $8 of the budget×leverage notional), levels =
// clamp(round(span/step), 6, 500). The old economics — a 0.80% "break-even"
// step floor plus a 6..14 count clamp — starved every $200-notional 4%-span
// deploy to 6 levels of 0.64-0.75%: three such live bots booked ZERO grid
// profit in 8h while the dense deployments (0.22% step, 29-60 levels)
// harvested everything. ATR/regime stay accepted for signature stability but
// no longer drive density — only the margin does.
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
	_ = atrPct         // density no longer follows volatility (see doc comment)
	_ = regime         // and the regime step multiplier is retired with it
	_ = minGridStepPct // the 0.25% economic floor supersedes caller guesses

	if upper.LessThanOrEqual(lower) || currentPrice.LessThanOrEqual(decimal.Zero) {
		return AdaptiveMeshResult{
			GridNum:     8,
			GridStepPct: decimal.NewFromFloat(marketdata.GridStepFloorPct),
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

	lev := int64(1)
	if leverage > 1 {
		lev = int64(leverage)
	}
	notional, _ := budgetUsdt.Mul(decimal.NewFromInt(lev)).Float64()
	idealGrids := marketdata.GridLevelsForRange(spanPct, notional)

	actualStepPct := spanPct / float64(idealGrids)
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
