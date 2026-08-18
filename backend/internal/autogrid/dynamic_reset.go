package autogrid

import (
	"github.com/shopspring/decimal"
)

// DGT (Dynamic Grid Trading) reset algorithm from arXiv 2506.11921.
// When price breaks out of the grid range, instead of closing the bot
// and realizing losses, the grid is REBUILT around the new price.
// This converts "range break = loss" into "range break = reposition".

type ResetPlan struct {
	Action   string // "RESET_UP", "RESET_DOWN"
	NewLower decimal.Decimal
	NewUpper decimal.Decimal
	Reason   string
}

// ShouldResetGrid checks if the bot's price has broken out beyond the
// grid range and returns a reset plan.
func ShouldResetGrid(
	direction string,
	lower, upper decimal.Decimal,
	currentPrice decimal.Decimal,
	rangeBreakBuffer decimal.Decimal, // as percent (e.g., 1.0 = 1%)
	adjustmentsLeft int,
) *ResetPlan {
	if adjustmentsLeft <= 0 {
		return nil // no more resets allowed
	}

	buffer := rangeBreakBuffer.Div(decimal.NewFromInt(100))
	breakUp := upper.Mul(decimal.NewFromInt(1).Add(buffer))
	breakDown := lower.Mul(decimal.NewFromInt(1).Sub(buffer))

	width := upper.Sub(lower)
	halfWidth := width.Div(decimal.NewFromInt(2))

	// Breakout UP: price above upper + buffer
	if currentPrice.GreaterThan(breakUp) {
		return &ResetPlan{
			Action:   "RESET_UP",
			NewLower: currentPrice.Sub(halfWidth),
			NewUpper: currentPrice.Add(halfWidth),
			Reason:   "DGT breakout UP — rebuild grid at new price level",
		}
	}

	// Breakdown DOWN: price below lower - buffer
	if currentPrice.LessThan(breakDown) {
		return &ResetPlan{
			Action:   "RESET_DOWN",
			NewLower: currentPrice.Sub(halfWidth),
			NewUpper: currentPrice.Add(halfWidth),
			Reason:   "DGT breakout DOWN — rebuild grid at new price level",
		}
	}

	return nil // price still in range, hold
}
