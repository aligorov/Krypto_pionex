package autogrid

import (
	"time"

	"github.com/shopspring/decimal"
)

// Management actions for a running native grid bot.
const (
	ActionHold               = "HOLD"
	ActionCloseTakeProfit    = "CLOSE_TAKE_PROFIT"
	ActionCloseStopLoss      = "CLOSE_STOP_LOSS"
	ActionCloseRangeBreak    = "CLOSE_RANGE_BREAK"
	ActionCloseStructInvalid = "CLOSE_STRUCT_INVALID"
	ActionAdjustUp           = "ADJUST_UP"
	ActionAdjustDown         = "ADJUST_DOWN"
)

type botActionInput struct {
	Direction        string          // LONG, SHORT, NEUTRAL
	Lower            decimal.Decimal // current grid bottom
	Upper            decimal.Decimal // current grid top
	CurrentPrice     decimal.Decimal
	RealizedPNL      decimal.Decimal
	UnrealizedPNL    decimal.Decimal
	PeakPNL          decimal.Decimal // highest realized+unrealized PnL seen so far
	Budget           decimal.Decimal // bot investment
	PnLTarget        decimal.Decimal // per-bot take-profit in USDT (0 = off)
	MaxLoss          decimal.Decimal // per-bot max loss in USDT (0 = off)
	RangeBreakBuffer decimal.Decimal // percent beyond the range before acting
	AdjustmentsLeft  int
	Regime           string // RANGE, TREND_UP, TREND_DOWN ("" = unknown)
	// AntiHuntStop is the deploy-time invalidation level: price beyond it
	// against the bot's direction means the thesis the grid was opened
	// under is dead — close before the exchange stop gets swept.
	AntiHuntStop *decimal.Decimal
}

type manageDecision struct {
	Action   string
	Reason   string
	NewLower decimal.Decimal
	NewUpper decimal.Decimal
}

// decideBotAction is the pure supervision policy for a running bot:
//  1. take the money when the per-bot PnL target is reached;
//  2. lock in profit on trailing pullback when peak profit reached >= 70% of target;
//  3. protect breakeven when peak profit reached >= 50% of target;
//  4. cut the loss at the configured maximum;
//  5. when price escapes the grid range, follow the break with a native range
//     shift when the regime allows, otherwise close to avoid trend damage.
//
// Break matrix (down / up):
//
//	LONG   down: TREND_DOWN or unknown -> close; RANGE/TREND_UP -> shift down
//	LONG   up:   follow with shift up (inventory was sold into strength)
//	SHORT  up:   TREND_UP or unknown -> close; RANGE/TREND_DOWN -> shift up
//	SHORT  down: follow with shift down (shorts harvest the fall)
//	NEUTRAL: adverse break closes unless the regime still supports a shift;
//	the profitable side of a neutral grid keeps shifting with the market.
func decideBotAction(input botActionInput) manageDecision {
	total := input.RealizedPNL.Add(input.UnrealizedPNL)

	// 1. Direct Take-Profit
	if input.PnLTarget.GreaterThan(decimal.Zero) && total.GreaterThanOrEqual(input.PnLTarget) {
		return manageDecision{Action: ActionCloseTakeProfit, Reason: "TAKE_PROFIT"}
	}

	// 2. Trailing Take-Profit & Breakeven Lock
	if input.PnLTarget.GreaterThan(decimal.Zero) {
		target70Pct := input.PnLTarget.Mul(decimal.NewFromFloat(0.70))
		if input.PeakPNL.GreaterThanOrEqual(target70Pct) {
			// Pullback tolerance: 20% of peak profit
			pullbackTolerance := input.PeakPNL.Mul(decimal.NewFromFloat(0.20))
			trailingFloor := input.PeakPNL.Sub(pullbackTolerance)
			minFloor := input.PnLTarget.Mul(decimal.NewFromFloat(0.50))
			if minFloor.GreaterThan(trailingFloor) {
				trailingFloor = minFloor
			}
			if total.LessThan(trailingFloor) {
				return manageDecision{Action: ActionCloseTakeProfit, Reason: "TRAILING_TAKE_PROFIT"}
			}
		}

		// 3. Breakeven Lock (if reached 50% target, don't allow profit to turn into loss)
		target50Pct := input.PnLTarget.Mul(decimal.NewFromFloat(0.50))
		if input.PeakPNL.GreaterThanOrEqual(target50Pct) {
			breakevenFloor := input.Budget.Mul(decimal.NewFromFloat(0.002)) // 0.2% of budget to cover fees
			if total.LessThanOrEqual(breakevenFloor) {
				return manageDecision{Action: ActionCloseTakeProfit, Reason: "BREAKEVEN_LOCK"}
			}
		}
	}

	// 4. Stop-Loss
	if input.MaxLoss.GreaterThan(decimal.Zero) && total.LessThanOrEqual(input.MaxLoss.Neg()) {
		return manageDecision{Action: ActionCloseStopLoss, Reason: "STOP_LOSS"}
	}

	// 4.5 Structural invalidation: the deploy-time anti-hunt level marks
	// where the opening thesis is dead. Unlike the range break it acts
	// regardless of regime — a sweep beyond it against the direction is
	// the documented exit-before-the-crowd point, before inventory loads.
	if input.AntiHuntStop != nil && input.AntiHuntStop.GreaterThan(decimal.Zero) &&
		input.CurrentPrice.GreaterThan(decimal.Zero) {
		stopBroken := false
		switch input.Direction {
		case "SHORT":
			stopBroken = input.CurrentPrice.GreaterThan(*input.AntiHuntStop)
		default: // LONG and NEUTRAL both hold long-side inventory on a break down
			stopBroken = input.CurrentPrice.LessThan(*input.AntiHuntStop)
		}
		if stopBroken {
			return manageDecision{Action: ActionCloseStructInvalid, Reason: "STRUCT_INVALID_ANTI_HUNT"}
		}
	}

	if !input.CurrentPrice.GreaterThan(decimal.Zero) || !input.Upper.GreaterThan(input.Lower) {
		return manageDecision{Action: ActionHold}
	}
	buffer := input.RangeBreakBuffer.Div(decimal.NewFromInt(100))
	breakDown := input.Lower.Mul(decimal.NewFromInt(1).Sub(buffer))
	breakUp := input.Upper.Mul(decimal.NewFromInt(1).Add(buffer))
	adverseDown := input.Regime == "TREND_DOWN" || input.Regime == ""
	adverseUp := input.Regime == "TREND_UP" || input.Regime == ""

	if input.CurrentPrice.LessThan(breakDown) {
		// v2.0 DGT reset: instead of closing on range break, try rebuilding
		// the grid around the new price. Falls back to close when no
		// adjustments remain or when the regime is adverse.
		// DGT reset: only when the regime doesn't contradict the direction
		if reset := ShouldResetGrid(input.Direction, input.Lower, input.Upper,
			input.CurrentPrice, input.RangeBreakBuffer, input.AdjustmentsLeft); reset != nil && !adverseDown {
			return manageDecision{
				Action:   ActionAdjustDown,
				Reason:   reset.Reason,
				NewLower: reset.NewLower,
				NewUpper: reset.NewUpper,
			}
		}
		switch input.Direction {
		case "SHORT":
			return adjustDecision(input, ActionAdjustDown, "RANGE_SHIFT_DOWN")
		case "LONG":
			if adverseDown {
				return manageDecision{Action: ActionCloseRangeBreak, Reason: "RANGE_BREAK_DOWN"}
			}
			return adjustDecision(input, ActionAdjustDown, "RANGE_SHIFT_DOWN")
		default: // NEUTRAL holds inventory on a downside break
			if adverseDown {
				return manageDecision{Action: ActionCloseRangeBreak, Reason: "RANGE_BREAK_DOWN"}
			}
			return adjustDecision(input, ActionAdjustDown, "RANGE_SHIFT_DOWN")
		}
	}
	if input.CurrentPrice.GreaterThan(breakUp) {
		if reset := ShouldResetGrid(input.Direction, input.Lower, input.Upper,
			input.CurrentPrice, input.RangeBreakBuffer, input.AdjustmentsLeft); reset != nil && !adverseUp {
			return manageDecision{
				Action:   ActionAdjustUp,
				Reason:   reset.Reason,
				NewLower: reset.NewLower,
				NewUpper: reset.NewUpper,
			}
		}
		switch input.Direction {
		case "LONG":
			return adjustDecision(input, ActionAdjustUp, "RANGE_SHIFT_UP")
		case "SHORT":
			if adverseUp {
				return manageDecision{Action: ActionCloseRangeBreak, Reason: "RANGE_BREAK_UP"}
			}
			return adjustDecision(input, ActionAdjustUp, "RANGE_SHIFT_UP")
		default: // NEUTRAL sits on unsold inventory on an upside break
			return adjustDecision(input, ActionAdjustUp, "RANGE_SHIFT_UP")
		}
	}
	return manageDecision{Action: ActionHold}
}

func adjustDecision(input botActionInput, action, reason string) manageDecision {
	if input.AdjustmentsLeft <= 0 {
		return manageDecision{Action: ActionCloseRangeBreak, Reason: reason + "_NO_ADJUSTMENTS_LEFT"}
	}
	width := input.Upper.Sub(input.Lower)
	half := width.Div(decimal.NewFromInt(2))
	return manageDecision{
		Action:   action,
		Reason:   reason,
		NewLower: input.CurrentPrice.Sub(half),
		NewUpper: input.CurrentPrice.Add(half),
	}
}

// neutralGridPaperPNL simulates what a native neutral grid earns between two
// observation points using leveraged ladder economics (v1.3.22):
//
// Realized profit follows the exchange's own attribution — Pionex books grid
// profit only on COMPLETED buy/sell pairs, so a crossing only earns when it
// moves back through levels that already accumulated inventory. A one-way
// traverse out of the range books zero profit and just loads inventory.
//
// Inventory is marked as the exact uniform ladder: levels-from-mid ×
// per-level notional (investment × leverage / gridNum), with the average
// entry at the midpoint of the filled half. Funding is NOT modeled here; the
// supervision loop accrues it separately per 8h boundary on inventoryNotional.
func neutralGridPaperPNL(
	lower, upper decimal.Decimal,
	gridNum int,
	investment decimal.Decimal,
	leverage int,
	lastLevel, currentLevel int,
	price decimal.Decimal,
	feeBps decimal.Decimal,
) (pairProfit, unrealized, inventoryNotional decimal.Decimal) {
	if !upper.GreaterThan(lower) || gridNum < 2 || !price.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero, decimal.Zero
	}
	width := upper.Sub(lower)
	mid := upper.Add(lower).Div(decimal.NewFromInt(2))
	levelWidth := width.Div(decimal.NewFromInt(int64(gridNum)))
	if leverage < 1 {
		leverage = 1
	}
	perLevelNotional := investment.Mul(decimal.NewFromInt(int64(leverage))).Div(decimal.NewFromInt(int64(gridNum)))
	stepPct := levelWidth.Div(mid)
	feePct := feeBps.Mul(decimal.NewFromInt(2)).Div(decimal.NewFromInt(10000))

	// Pair completion: inventory is long (midLevel − lastLevel) levels when the
	// bot sits below the midpoint. A crossing in the direction OF that
	// inventory closes min(|delta|, |inventory|) round trips; a crossing away
	// from it only extends the inventory and earns nothing.
	delta := currentLevel - lastLevel
	invPrev := gridNum/2 - lastLevel
	pairs := 0
	if delta != 0 && ((delta > 0) == (invPrev > 0)) && invPrev != 0 {
		absDelta, absInv := delta, invPrev
		if absDelta < 0 {
			absDelta = -absDelta
		}
		if absInv < 0 {
			absInv = -absInv
		}
		if absDelta < absInv {
			pairs = absDelta
		} else {
			pairs = absInv
		}
	}
	pairProfit = perLevelNotional.Mul(stepPct.Sub(feePct)).Mul(decimal.NewFromInt(int64(pairs)))

	// Inventory mark (stateless, consistent with the pair model under
	// monotone movement between observations): levelsFromMid is positive when
	// price is below the midpoint (long inventory bought on dips) and negative
	// above it (short inventory sold on rallies).
	halfLevels := decimal.NewFromInt(int64(gridNum)).Div(decimal.NewFromInt(2))
	levelsFromMid := mid.Sub(price).Div(levelWidth)
	if levelsFromMid.GreaterThan(halfLevels) {
		levelsFromMid = halfLevels
	}
	if levelsFromMid.LessThan(halfLevels.Neg()) {
		levelsFromMid = halfLevels.Neg()
	}
	inventoryNotional = levelsFromMid.Abs().Mul(perLevelNotional)
	if inventoryNotional.IsPositive() {
		entry := mid.Sub(levelsFromMid.Mul(levelWidth).Div(decimal.NewFromInt(2)))
		if entry.GreaterThan(decimal.Zero) {
			if levelsFromMid.IsPositive() {
				unrealized = inventoryNotional.Mul(price.Sub(entry).Div(entry))
			} else {
				unrealized = inventoryNotional.Mul(entry.Sub(price).Div(entry))
			}
		}
	}
	return pairProfit, unrealized, inventoryNotional
}

// gridLevelForPrice maps a price to its grid level, clamped into the range.
func gridLevelForPrice(lower, upper decimal.Decimal, gridNum int, price decimal.Decimal) int {
	if !upper.GreaterThan(lower) || gridNum < 2 || !price.GreaterThan(decimal.Zero) {
		return 0
	}
	levelWidth := upper.Sub(lower).Div(decimal.NewFromInt(int64(gridNum)))
	level := price.Sub(lower).Div(levelWidth).IntPart()
	if level < 0 {
		return 0
	}
	if level >= int64(gridNum) {
		return gridNum - 1
	}
	return int(level)
}

// fundingAccrual returns the funding cash flow (absolute magnitude; the
// caller applies the pay/receive sign) accrued since the last settled 8h
// boundary, together with the next settlement anchor. Nil when no full
// boundary has been crossed yet. Pionex settles perpetual funding every 8
// hours and reflects it in the position's floating PnL; the paper simulator
// books it into realized so stop/target decisions see it immediately.
func fundingAccrual(
	exposure decimal.Decimal,
	rateBps decimal.Decimal,
	openedAt time.Time,
	lastFundingAt *time.Time,
	now time.Time,
) (*decimal.Decimal, *time.Time) {
	if !exposure.IsPositive() || rateBps.IsNegative() || !rateBps.IsPositive() {
		return nil, nil
	}
	anchor := openedAt
	if lastFundingAt != nil && lastFundingAt.After(anchor) {
		anchor = *lastFundingAt
	}
	const fundingInterval = 8 * time.Hour
	elapsed := now.Sub(anchor)
	if elapsed < fundingInterval {
		return nil, nil
	}
	boundaries := int64(elapsed / fundingInterval)
	delta := exposure.Mul(rateBps).Div(decimal.NewFromInt(10000)).Mul(decimal.NewFromInt(boundaries))
	if delta.IsZero() {
		return nil, nil
	}
	next := anchor.Add(time.Duration(boundaries) * fundingInterval)
	return &delta, &next
}
