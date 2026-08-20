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
	// Avg48h/Stable48h carry the 48h funding-carry picture (v2.0.21): a
	// stable non-trivial rate is a paid-to-hold edge that deserves a
	// directional grid even inside RANGE (short collects carry at a stable
	// positive rate, long at a stable negative one).
	Avg48h     float64
	Stable48h  bool
}

// fundingCarryThreshold is the minimum stable 48h average rate (per 8h)
// that qualifies as a carry setup: 0.0003 = 0.03%/8h ≈ 0.09%/day on
// notional — meaningful against the measured 0.1-0.3%/day grid harvest.
const fundingCarryThreshold = 0.0003

// RegimeContext carries the market regime from the confluence/scanner.
type RegimeContext struct {
	Regime     string  // RANGE, TREND_UP, TREND_DOWN
	Confidence float64 // 0-1
	HurstValue float64 // <0.45 mean-rev, >0.55 trending
}

// EventContext carries blocking event information.
type EventContext struct {
	HighImpactEvent24h bool // FOMC/CPI within 24h
	// LiquidationCascade (long-side, >$50M/1h) means forced selling into a
	// knife: LONG/NEUTRAL grids load falling inventory, but a SHORT grid is
	// the participation window (v2.0.21 — it used to freeze everything,
	// killing the best short entries of the cycle exactly when they set up).
	LiquidationCascade bool
	FearGreedExtreme   int // >85 extreme greed, <15 extreme fear
}

// SelectDirection chooses the grid direction based on regime + funding + events.
// This replaces the old "always NEUTRAL" approach that loses money in trends.
func SelectDirection(regime RegimeContext, funding FundingContext, events EventContext) DirectionDecision {
	// 1. BLOCKING GATES (highest priority)
	if events.HighImpactEvent24h {
		return DirectionDecision{Direction: "WAIT", Reason: "high-impact economic event within 24h"}
	}
	// Sentiment paralysis zones (wired in v2.0.8; scoped in v2.0.14).
	// Euphoria (>=85) blocks everything — for a grid, overpaying at the
	// crowd's top is the worst entry window in any shape. Extreme panic
	// (1..15) blocks DIRECTIONAL entries unless an active liquidation
	// cascade overrides for the SHORT side: forced unwind is continuation
	// evidence, and freezing the only short window of the cycle to "wait
	// for calm" surrenders it (v2.0.21).
	fng := events.FearGreedExtreme
	panicZone := fng >= 1 && fng <= 15
	panicFreezesShorts := panicZone && !events.LiquidationCascade

	// 2. REGIME-BASED DIRECTION
	if fng >= 85 {
		return DirectionDecision{Direction: "WAIT", Reason: fmt.Sprintf("Fear&Greed euphoria %d — paralysis zone (all shapes)", fng)}
	}
	switch regime.Regime {
	case "TREND_DOWN":
		if panicFreezesShorts {
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
		// A long-side liquidation cascade (the only kind the detector
		// arms on) is the opposite of a LONG entry window.
		if events.LiquidationCascade {
			return DirectionDecision{Direction: "WAIT", Reason: "liquidation cascade — only SHORT entries in this window"}
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
		// A knife cascade does freeze it though: a neutral grid loading
		// long inventory into forced unwind is the PEPE failure class.
		if events.LiquidationCascade {
			return DirectionDecision{Direction: "WAIT", Reason: "liquidation cascade — NEUTRAL grid would load the knife"}
		}
		// The 0.60 boundary is aligned with the confluence engine's
		// HurstHardVetoNeutral.
		if regime.HurstValue < 0.60 {
			// v2.0.21 funding-carry: a stable, non-trivial 48h rate pays
			// the grid to hold a direction it would otherwise not take —
			// short collects at a stable positive rate, long at negative.
			// Extreme spot rates still veto (squeeze/flush risk windows).
			if funding.Stable48h && !funding.IsExtreme {
				if funding.Avg48h >= fundingCarryThreshold {
					return DirectionDecision{
						Direction: "SHORT",
						Leverage:  2,
						Reason:    fmt.Sprintf("RANGE + carry: фандинг +%.3f%%/8ч стабилен 48ч", funding.Avg48h*100),
					}
				}
				if funding.Avg48h <= -fundingCarryThreshold {
					return DirectionDecision{
						Direction: "LONG",
						Leverage:  2,
						Reason:    fmt.Sprintf("RANGE + carry: фандинг %.3f%%/8ч стабилен 48ч (long получает)", funding.Avg48h*100),
					}
				}
			}
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
