package autogrid

import (
	"fmt"
)

// DirectionDecision tells deployReal what direction to use for a candidate.
type DirectionDecision struct {
	Direction string // "LONG", "SHORT", "NEUTRAL", "WAIT", "CLOSE_ALL"
	Leverage  int
	Reason    string
}

// FundingContext carries the cross-exchange funding data for direction selection.
type FundingContext struct {
	AverageRate float64 // average funding rate across exchanges
	IsExtreme   bool    // |avg| > 0.001 (0.1%)
}

// RegimeContext carries the market regime from the confluence/scanner.
type RegimeContext struct {
	Regime     string  // RANGE, TREND_UP, TREND_DOWN
	Confidence float64 // 0-1
	HurstValue float64 // <0.45 mean-rev, >0.55 trending
}

// EventContext carries blocking event information.
type EventContext struct {
	HighImpactEvent24h bool // FOMC/CPI within 24h
	LiquidationCascade bool // >$50M liquidations in last hour
	FearGreedExtreme   int  // >85 extreme greed, <15 extreme fear
}

// SelectDirection chooses the grid direction based on regime + funding + events.
// This replaces the old "always NEUTRAL" approach that loses money in trends.
func SelectDirection(regime RegimeContext, funding FundingContext, events EventContext) DirectionDecision {
	// 1. BLOCKING GATES (highest priority)
	if events.HighImpactEvent24h {
		return DirectionDecision{Direction: "WAIT", Reason: "high-impact economic event within 24h"}
	}
	if events.LiquidationCascade {
		return DirectionDecision{Direction: "WAIT", Reason: "liquidation cascade in progress"}
	}
	// Sentiment paralysis zones (wired in v2.0.8; scoped in v2.0.14).
	// Euphoria (>=85) blocks everything — for a grid, overpaying at the
	// crowd's top is the worst entry window in any shape. Extreme panic
	// (1..15) now blocks only DIRECTIONAL entries: capitulation is the
	// richest mean-reversion harvest window (Nagel 2012) and freezing
	// NEUTRAL grids through it surrendered the best windows of the cycle.
	// 0 is the zero-value/no-signal case.
	fng := events.FearGreedExtreme
	panicZone := fng >= 1 && fng <= 15

	// 2. REGIME-BASED DIRECTION
	if fng >= 85 {
		return DirectionDecision{Direction: "WAIT", Reason: fmt.Sprintf("Fear&Greed euphoria %d — paralysis zone (all shapes)", fng)}
	}
	switch regime.Regime {
	case "TREND_DOWN":
		if panicZone {
			return DirectionDecision{Direction: "WAIT", Reason: fmt.Sprintf("Fear&Greed panic %d — directional freeze", fng)}
		}
		// v2.0.14 symmetric relaxation: SHORT when shorts EARN carry
		// (classic), or when funding is merely not extreme — in dumps
		// funding typically flips negative (shorts pay), which used to
		// veto the best short setups outright. Extreme funding still
		// vetoes: crowded shorts squeeze.
		if funding.AverageRate > 0.0001 || !funding.IsExtreme {
			return DirectionDecision{
				Direction: "SHORT",
				Leverage:  2,
				Reason:    fmt.Sprintf("TREND_DOWN + funding %.4f%% (не экстремальный)", funding.AverageRate*100),
			}
		}
		return DirectionDecision{Direction: "WAIT", Reason: "TREND_DOWN but funding extreme — crowded shorts risk"}

	case "TREND_UP":
		if panicZone {
			return DirectionDecision{Direction: "WAIT", Reason: fmt.Sprintf("Fear&Greed panic %d — directional freeze", fng)}
		}
		// v2.0.14 symmetric relaxation: LONG when longs EARN carry, or when
		// funding is merely not extreme — rallies carry positive funding
		// (longs pay), which previously made LONG unreachable in every
		// up-market (2026-08-20 audit: deployable rally universe ~0-2%).
		// Extreme funding still vetoes: crowded longs are squeeze fuel.
		if funding.AverageRate < -0.0001 || !funding.IsExtreme {
			return DirectionDecision{
				Direction: "LONG",
				Leverage:  2,
				Reason:    fmt.Sprintf("TREND_UP + funding %.4f%% (не экстремальный)", funding.AverageRate*100),
			}
		}
		return DirectionDecision{Direction: "WAIT", Reason: "TREND_UP but funding extreme — crowded longs risk"}

	case "RANGE":
		// panicZone deliberately does NOT freeze NEUTRAL: capitulation is
		// the richest harvest window for a mean-reversion grid.
		// NEUTRAL while the pair still mean-reverts hard enough for a grid.
		// The 0.60 boundary is aligned with the confluence engine's
		// HurstHardVetoNeutral — the original `HurstValue < 0.50` combined
		// with the (pre-v2.0.3) hardcoded Hurst=0.5 input dead-locked every
		// RANGE candidate into WAIT.
		if regime.HurstValue < 0.60 {
			return DirectionDecision{
				Direction: "NEUTRAL",
				Leverage:  2, // NOT 4x like before
				Reason:    fmt.Sprintf("RANGE confidence %.2f, Hurst %.2f", regime.Confidence, regime.HurstValue),
			}
		}
		return DirectionDecision{Direction: "WAIT", Reason: fmt.Sprintf("RANGE but trending Hurst %.2f >= 0.60", regime.HurstValue)}
	}

	return DirectionDecision{Direction: "WAIT", Reason: "unknown regime"}
}
