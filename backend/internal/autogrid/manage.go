package autogrid

import (
	"github.com/shopspring/decimal"
)

// Management actions for a running native grid bot.
const (
	ActionHold           = "HOLD"
	ActionCloseTakeProfit = "CLOSE_TAKE_PROFIT"
	ActionCloseStopLoss  = "CLOSE_STOP_LOSS"
	ActionCloseRangeBreak = "CLOSE_RANGE_BREAK"
	ActionAdjustUp       = "ADJUST_UP"
	ActionAdjustDown     = "ADJUST_DOWN"
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
}

type manageDecision struct {
	Action            string
	Reason            string
	NewLower          decimal.Decimal
	NewUpper          decimal.Decimal
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
//   LONG   down: TREND_DOWN or unknown -> close; RANGE/TREND_UP -> shift down
//   LONG   up:   follow with shift up (inventory was sold into strength)
//   SHORT  up:   TREND_UP or unknown -> close; RANGE/TREND_DOWN -> shift up
//   SHORT  down: follow with shift down (shorts harvest the fall)
//   NEUTRAL: adverse break closes unless the regime still supports a shift;
//   the profitable side of a neutral grid keeps shifting with the market.
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
	if !input.CurrentPrice.GreaterThan(decimal.Zero) || !input.Upper.GreaterThan(input.Lower) {
		return manageDecision{Action: ActionHold}
	}
	buffer := input.RangeBreakBuffer.Div(decimal.NewFromInt(100))
	breakDown := input.Lower.Mul(decimal.NewFromInt(1).Sub(buffer))
	breakUp := input.Upper.Mul(decimal.NewFromInt(1).Add(buffer))
	adverseDown := input.Regime == "TREND_DOWN" || input.Regime == ""
	adverseUp := input.Regime == "TREND_UP" || input.Regime == ""

	if input.CurrentPrice.LessThan(breakDown) {
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
// observation points: every pair of level crossings is one filled
// buy-low/sell-high round trip worth (per-level capital × relative step),
// while the inventory accumulated below the range midpoint carries mark PnL.
// Fees apply per round trip from the configured fee basis points.
func neutralGridPaperPNL(
	lower, upper decimal.Decimal,
	gridNum int,
	investment decimal.Decimal,
	lastLevel, currentLevel int,
	price decimal.Decimal,
	feeBps decimal.Decimal,
) (crossingProfit, unrealized decimal.Decimal) {
	if !upper.GreaterThan(lower) || gridNum < 2 || !price.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero
	}
	width := upper.Sub(lower)
	mid := upper.Add(lower).Div(decimal.NewFromInt(2))
	levelWidth := width.Div(decimal.NewFromInt(int64(gridNum)))
	perLevelCapital := investment.Div(decimal.NewFromInt(int64(gridNum)))
	stepPct := levelWidth.Div(mid)
	feePct := feeBps.Mul(decimal.NewFromInt(2)).Div(decimal.NewFromInt(10000))

	crossings := currentLevel - lastLevel
	if crossings < 0 {
		crossings = -crossings
	}
	crossingProfit = perLevelCapital.Mul(stepPct.Sub(feePct)).Mul(decimal.NewFromInt(int64(crossings))).Div(decimal.NewFromInt(2))

	// Inventory model:
	// Below the midpoint, the grid accumulates long inventory bought on dips.
	// Above the midpoint, the grid accumulates short inventory sold on rallies.
	position := price.Sub(mid).Div(width.Div(decimal.NewFromInt(2)))
	if position.GreaterThan(decimal.NewFromInt(1)) {
		position = decimal.NewFromInt(1)
	}
	if position.LessThan(decimal.NewFromInt(-1)) {
		position = decimal.NewFromInt(-1)
	}
	if position.IsNegative() {
		inventory := investment.Mul(position.Neg())
		entry := mid.Add(position.Mul(width.Div(decimal.NewFromInt(4))))
		if entry.GreaterThan(decimal.Zero) {
			unrealized = inventory.Mul(price.Sub(entry).Div(entry))
		}
	} else if position.IsPositive() {
		inventory := investment.Mul(position)
		entry := mid.Add(position.Mul(width.Div(decimal.NewFromInt(4))))
		if entry.GreaterThan(decimal.Zero) {
			unrealized = inventory.Mul(entry.Sub(price).Div(entry))
		}
	}
	return crossingProfit, unrealized
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
