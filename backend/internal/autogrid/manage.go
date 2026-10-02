package autogrid

import (
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Management actions for a running native grid bot.
// Management actions for a running native grid bot.
const (
	ActionHold               = "HOLD"
	ActionCloseTakeProfit    = "CLOSE_TAKE_PROFIT"
	ActionCloseStopLoss      = "CLOSE_STOP_LOSS"
	ActionCloseRangeBreak    = "CLOSE_RANGE_BREAK"
	ActionCloseStructInvalid = "CLOSE_STRUCT_INVALID"
	ActionCloseSmartHarvest  = "CLOSE_SMART_HARVEST"
	ActionCloseOURotation    = "CLOSE_OU_ROTATION"
	ActionUpdateTrailingSL   = "UPDATE_TRAILING_SL"
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
	AntiHuntStop      *decimal.Decimal
	TargetPrice       *decimal.Decimal
	StopLossPrice     *decimal.Decimal
	StopLossHigh      *decimal.Decimal
	TrailingSLPrice   *decimal.Decimal
	OFIRegime         *string
	MicroPriceBiasBps *float64
	AgeHours          float64
	OUHalfLifeHours   float64
	SmartExitEnabled  bool
	OFIHarvestEnabled bool
	OURotationEnabled bool
	// PricePrecision is the symbol's quote precision (deploy-time scanner
	// reading, model_state.carried): the trailing-SL candidate rounds to it —
	// a fixed Round(4) zeroed sub-cent perps and missed quote precision on
	// BTC-class symbols (v2.0.155). 0 = fall back to 4.
	PricePrecision int
	// StormActive defers the OU rotation inside a fleet-wide acceleration
	// (v2.0.111 doctrine, worker-parity; nil = storm unknown, rotate).
	StormActive *bool
}

type manageDecision struct {
	Action          string
	Reason          string
	NewLower        decimal.Decimal
	NewUpper        decimal.Decimal
	TrailingSLPrice *decimal.Decimal
}

// decideBotAction is the pure supervision policy for a running bot:
//  0. check price-based take-profit and stop-loss;
//  1. take the money when the per-bot PnL target is reached;
//  2. harvest tops on OFI exhaustion (SmartProfitHarvest);
//  3. lock in profit on trailing pullback once peak profit arms at >= 50% of target;
//  4. advance native trailing SL to protect gains on the exchange card;
//  5. rotate capital out of stagnant bots via Ornstein-Uhlenbeck half-life;
//  6. cut the loss at the configured maximum or structural invalidation;
//  7. when price escapes the grid range, follow the break with a native range shift.
func decideBotAction(input botActionInput) manageDecision {
	total := input.RealizedPNL.Add(input.UnrealizedPNL)

	// 0. Price-based Take-Profit: if target price is set and reached natively
	if input.TargetPrice != nil && input.TargetPrice.GreaterThan(decimal.Zero) && input.CurrentPrice.GreaterThan(decimal.Zero) {
		tpReached := false
		switch input.Direction {
		case "SHORT":
			tpReached = input.CurrentPrice.LessThanOrEqual(*input.TargetPrice)
		default: // LONG, NEUTRAL
			tpReached = input.CurrentPrice.GreaterThanOrEqual(*input.TargetPrice)
		}
		if tpReached && total.IsPositive() {
			return manageDecision{Action: ActionCloseTakeProfit, Reason: "TAKE_PROFIT_PRICE_HIT"}
		}
	}

	// 0.5 Price-based Stop-Loss: if stop loss price is set and breached
	if input.StopLossPrice != nil && input.StopLossPrice.GreaterThan(decimal.Zero) && input.CurrentPrice.GreaterThan(decimal.Zero) {
		slBreached := false
		switch input.Direction {
		case "SHORT":
			slBreached = input.CurrentPrice.GreaterThanOrEqual(*input.StopLossPrice)
		default: // LONG, NEUTRAL
			slBreached = input.CurrentPrice.LessThanOrEqual(*input.StopLossPrice)
		}
		if slBreached {
			return manageDecision{Action: ActionCloseStopLoss, Reason: "STOP_LOSS_PRICE_BREACH"}
		}
	}

	// Neutral Grid Upper Stop-Loss Breach
	if input.StopLossHigh != nil && input.StopLossHigh.GreaterThan(decimal.Zero) && input.CurrentPrice.GreaterThan(decimal.Zero) {
		if input.CurrentPrice.GreaterThanOrEqual(*input.StopLossHigh) {
			return manageDecision{Action: ActionCloseStopLoss, Reason: "STOP_LOSS_HIGH_BREACH"}
		}
	}

	// 1. Direct Take-Profit
	if input.PnLTarget.GreaterThan(decimal.Zero) && total.GreaterThanOrEqual(input.PnLTarget) {
		return manageDecision{Action: ActionCloseTakeProfit, Reason: "TAKE_PROFIT"}
	}

	// 1.5 Smart Profit Harvest: Exit at the peak before pullback on OFI/Microstructure exhaustion
	if input.SmartExitEnabled && input.OFIHarvestEnabled && total.GreaterThan(input.Budget.Mul(decimal.NewFromFloat(0.015))) {
		isExhausted := false
		if input.Direction == "LONG" {
			if input.MicroPriceBiasBps != nil && *input.MicroPriceBiasBps < -2.5 {
				if input.OFIRegime != nil && (*input.OFIRegime == "CONFIRMED_DUMP" || *input.OFIRegime == "DUMP_PRESSURE") {
					isExhausted = true
				}
			}
		} else if input.Direction == "SHORT" {
			if input.MicroPriceBiasBps != nil && *input.MicroPriceBiasBps > 2.5 {
				if input.OFIRegime != nil && (*input.OFIRegime == "CONFIRMED_PUMP" || *input.OFIRegime == "PUMP_PRESSURE") {
					isExhausted = true
				}
			}
		}
		if isExhausted {
			return manageDecision{Action: ActionCloseSmartHarvest, Reason: "SMART_PROFIT_HARVEST_OFI"}
		}
	}

	// 1.8 Trailing Stop Loss Advance: if bot is in significant profit (>= 50% of target), advance native SL.
	// v2.0.155: only a bot that CARRIES a native card stop (StopLossPrice
	// set — deployed under ADAPTIVE_ATR) may have it trailed; a NONE-mode bot
	// must not gain a card stop through the update endpoint. The candidate
	// rounds to the symbol's price precision — a fixed Round(4) zeroed
	// sub-cent perps (the PEPE failure class) and could miss quote precision
	// on BTC-class symbols.
	if input.PnLTarget.GreaterThan(decimal.Zero) && total.GreaterThanOrEqual(input.PnLTarget.Mul(decimal.NewFromFloat(0.50))) && input.CurrentPrice.GreaterThan(decimal.Zero) &&
		input.StopLossPrice != nil {
		trailPrec := int32(4)
		if input.PricePrecision > 0 {
			trailPrec = int32(input.PricePrecision)
		}
		var candidateSL decimal.Decimal
		if input.Direction == "LONG" {
			candidateSL = input.CurrentPrice.Mul(decimal.NewFromFloat(0.985)).Round(trailPrec)
			if (input.TrailingSLPrice == nil || candidateSL.GreaterThan(*input.TrailingSLPrice)) &&
				(input.StopLossPrice == nil || candidateSL.GreaterThan(*input.StopLossPrice)) {
				return manageDecision{
					Action:          ActionUpdateTrailingSL,
					Reason:          "TRAILING_SL_ADVANCE",
					TrailingSLPrice: &candidateSL,
				}
			}
		} else if input.Direction == "SHORT" {
			candidateSL = input.CurrentPrice.Mul(decimal.NewFromFloat(1.015)).Round(trailPrec)
			if (input.TrailingSLPrice == nil || candidateSL.LessThan(*input.TrailingSLPrice)) &&
				(input.StopLossPrice == nil || candidateSL.LessThan(*input.StopLossPrice)) {
				return manageDecision{
					Action:          ActionUpdateTrailingSL,
					Reason:          "TRAILING_SL_ADVANCE",
					TrailingSLPrice: &candidateSL,
				}
			}
		}
	}

	// 1.9 Ornstein-Uhlenbeck Half-Life Rotation: prevents capital lock-in during prolonged flat consolidation.
	// v2.0.155 parity with the worker's GRID_AGED_HALF_LIFE: the v2.0.89
	// 4-hour floor (no rotation before it, whatever the fast OU fit says) and
	// the v2.0.111 storm deferral — rotating INTO a fleet-wide acceleration
	// crystallizes the bottom tick (ICP #1381 −$4.34).
	if input.OURotationEnabled && input.OUHalfLifeHours > 0 {
		maxAgeHours := 2.0 * input.OUHalfLifeHours
		if maxAgeHours < 4.0 {
			maxAgeHours = 4.0
		}
		stormDefer := input.StormActive != nil && *input.StormActive
		if input.AgeHours > maxAgeHours && !stormDefer {
			if total.Abs().LessThan(input.Budget.Mul(decimal.NewFromFloat(0.01))) {
				return manageDecision{Action: ActionCloseOURotation, Reason: "OU_HALFLIFE_ROTATION"}
			}
		}
	}

	// 2. Trailing Take-Profit & Early Profit Lock — DIRECTIONAL ONLY.
	// v2.0.164 (prod 29-30.09 postmortem + debate evidence): a NEUTRAL grid
	// is a mean-reversion harvester whose profit is capped by construction —
	// the exchange profit_amount card IS its take-profit. A local PnL trail
	// on a $1 card target armed at 50% ($0.50) and closed on a 20% peak
	// pullback, converting every $1 harvest into $0.40-0.80 micro-exits
	// while always beating the card. Industry anchors arm at the TP itself
	// (3Commas) with a step-anchored callback; for MR grids the evidence
	// reads "fixed TP, no trail". So NEUTRAL bots keep only the card (and
	// the stop ladder); LONG/SHORT keep the trail — trend profit needs it —
	// with the arm raised 50%→75% of target (v2.0.56 history below).
	if input.PnLTarget.GreaterThan(decimal.Zero) && input.Direction != "NEUTRAL" {
		// Early Profit Locking: arm trailing once peak profit reaches the
		// arm fraction of target. v2.0.56: the arm is a pure fraction of the
		// target with NO dollar cap — the retired min(0.35×target, $3.50)
		// form cut all 5/5 trailing exits of the 24h checkpoint at
		// 0.68–0.79×peak. v2.0.164: 0.5→0.75 — a half-armed trail surrenders
		// the upper half of trend runs; 75% matches the 3Commas-style
		// "arm near the target" doctrine.
		targetArmThreshold := input.PnLTarget.Mul(decimal.NewFromFloat(0.75))

		if input.PeakPNL.GreaterThanOrEqual(targetArmThreshold) {
			// Breakeven Lock: if an armed peak decays back near zero, lock profit (+0.2% budget)
			breakevenFloor := input.Budget.Mul(decimal.NewFromFloat(0.002))
			if total.LessThanOrEqual(breakevenFloor) {
				return manageDecision{Action: ActionCloseTakeProfit, Reason: "BREAKEVEN_LOCK"}
			}

			// Trailing Floor: allow a 20% pullback from peak, guaranteed at
			// 30% of target. v2.0.45: the guarantee is CAPPED at 80% of the
			// arm level — since v2.0.19 made targets leverage-consistent
			// ($36 on 4x), 0.30×target ($10.80) exceeded every plausible
			// peak, inverting the "guarantee" into an instant exit on the
			// arming tick. v2.0.52: cap raised to 85% of arm. v2.0.56: with
			// the arm at 50% of target the 0.85×arm cap (0.425×target) sat
			// above the 0.30×target guarantee, so the plain 20% trail
			// governed; the cap stays as a guard against arm retuning
			// reintroducing the arming-tick inversion. v2.0.164 arm=0.75:
			// 0.85×arm = 0.64×target — again above the 0.30 guarantee, the
			// plain trail governs and the guard stands.
			pullbackTolerance := input.PeakPNL.Mul(decimal.NewFromFloat(0.20))
			trailingFloor := input.PeakPNL.Sub(pullbackTolerance)
			guaranteedFloorCap := targetArmThreshold.Mul(decimal.NewFromFloat(0.85))
			minGuaranteedFloor := input.PnLTarget.Mul(decimal.NewFromFloat(0.30))
			if minGuaranteedFloor.GreaterThan(guaranteedFloorCap) {
				minGuaranteedFloor = guaranteedFloorCap
			}
			if minGuaranteedFloor.GreaterThan(trailingFloor) {
				trailingFloor = minGuaranteedFloor
			}
			if total.LessThan(trailingFloor) {
				return manageDecision{Action: ActionCloseTakeProfit, Reason: "TRAILING_TAKE_PROFIT"}
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

	// 4.6 Pre-break regime exit (v2.0.186, prod ZAMA #1537): the scanner
	// confirmed TREND_UP a full 1.5h BEFORE the physical border cross
	// (position 76%→89%, EMA slope accelerating) while the NEUTRAL sat on
	// growing short inventory and only RANGE_BREAK_UP at full cost. An
	// UNDERWATER neutral parked at the edge of its range in a confirmed
	// adverse regime now exits before the break pays full price. The 88%
	// edge reuses the scanner's anti-FOMO strong-trend extreme band (same
	// threshold class, not a new constant); the profit side is left to the
	// normal TP — this is a stop-class save, it never cuts a working grid.
	if input.Direction == "NEUTRAL" &&
		(input.Regime == "TREND_UP" || input.Regime == "TREND_DOWN") &&
		total.LessThan(decimal.Zero) &&
		input.CurrentPrice.LessThan(breakUp) && input.CurrentPrice.GreaterThan(breakDown) { // strictly PRE-break
		span := input.Upper.Sub(input.Lower)
		if span.GreaterThan(decimal.Zero) {
			posPct := input.CurrentPrice.Sub(input.Lower).Div(span).Mul(decimal.NewFromInt(100))
			edgeUp := input.Regime == "TREND_UP" && posPct.GreaterThanOrEqual(decimal.NewFromFloat(88.0))
			edgeDown := input.Regime == "TREND_DOWN" && posPct.LessThanOrEqual(decimal.NewFromFloat(12.0))
			if edgeUp || edgeDown {
				reason := "RANGE_BREAK_UP_EARLY"
				if edgeDown {
					reason = "RANGE_BREAK_DOWN_EARLY"
				}
				return manageDecision{Action: ActionCloseRangeBreak, Reason: reason}
			}
		}
	}

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
			// Note (v2.0.14 audit): the down side needs no loss-bounded
			// trend stop — adverseDown (TREND_DOWN or unknown) already
			// closes here at ANY loss. The deliberate asymmetry vs the
			// up-side stop: a down-break in a downtrend is a continuing
			// knife (exit now), while an up-break in an uptrend may be
			// profitable rally participation (exit only at -MaxLoss/2).
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
			// When a NEUTRAL grid breaks above upper in an uptrend (TREND_UP):
			// all inventory has been sold into the rally. Shifting up means opening
			// a new short inventory against a running train (PEPE failure class).
			// If the bot has positive PnL, lock in profit and exit.
			// If underwater in a confirmed TREND_UP, cut loss immediately instead of shifting.
			if input.Regime == "TREND_UP" {
				if total.GreaterThan(decimal.Zero) {
					return manageDecision{Action: ActionCloseTakeProfit, Reason: "RANGE_BREAK_UP_PROFIT_TAKE"}
				}
				return manageDecision{Action: ActionCloseRangeBreak, Reason: "RANGE_BREAK_UP_TREND_STOP"}
			}
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

// pionexMakerFeeBps is the documented Pionex futures maker fee (0.02%,
// https://www.pionex.com/en/fees): the futures grid engine quotes passive
// limit orders, so a COMPLETED pair pays maker on both legs. Protective
// closes are the exception — they cross the book and pay taker — which is
// why the exit-fee honesty mark keeps the taker fee+slippage composite.
// Booking pairs at the taker composite overstated friction by ~10 bps per
// pair and made paper systematically underreport the harvest REAL grids
// keep (2026-08-20 external audit §2, verified against the official fee page).
const pionexMakerFeeBps = 2.0

// Paper-engine friction (v2.0.89). The calibrated rates live in the ONE
// block in finalprofit.go (paperFillBufferPct / paperStopTakerBps /
// paperStopSlippageBps / paperStopCostComposite):
//
//	calibrated vs REAL epoch 2026-09-03..05: wallet −31 vs uncalibrated
//	paper +70/day — the uncalibrated simulator counted boundary-touch fills,
// charged nothing at entry and closed stops at the mark. The three helpers
// below are where those rates enter the engine.

// paperCloseFeeRate is the per-dollar close cost a protective close pays on
// the open inventory: taker 0.05% + slippage 0.05% = 10 bps.
func paperCloseFeeRate() decimal.Decimal {
	return paperStopCostComposite.Div(decimal.NewFromInt(10000))
}

// paperEntryFee prices the entry fee a fresh paper grid pays at deploy on
// its INITIAL inventory notional — booked into realized at deploy exactly
// like the exchange debits the wallet. Rate follows the ACTUAL order type
// (v2.0.97 exchange-truth fix): a neutral grid builds its ladder with
// PASSIVE limit orders → maker 2 bps (the 2026-08-20 audit's own finding —
// the old taker charge here contradicted pionexMakerFeeBps used on pair
// legs); a directional grid opens its full notional at market → taker 5 bps.
// Directional grids open the full leveraged notional crossing the book; a
// neutral grid starts with the uniform-ladder inventory at the deploy price
// (|levels from mid| × per-level notional — the same stateless ladder
// neutralGridPaperPNL marks).
func paperEntryFee(
	direction string,
	lower, upper decimal.Decimal,
	gridNum int,
	investment decimal.Decimal,
	leverage int,
	deployPrice decimal.Decimal,
) decimal.Decimal {
	return paperInitialInventoryNotional(direction, lower, upper, gridNum, investment, leverage, deployPrice).
		Mul(paperEntryRateBps(direction)).Div(decimal.NewFromInt(10000))
}

// paperEntryRateBps is the entry fee rate by direction: maker for the
// passive-limit neutral ladder, taker for market-entered directional grids.
func paperEntryRateBps(direction string) decimal.Decimal {
	if strings.EqualFold(strings.TrimSpace(direction), "NEUTRAL") {
		return decimal.NewFromFloat(pionexMakerFeeBps)
	}
	return paperStopTakerBps
}

// paperPourEntryFee prices the entry fee an invest_in pour pays on the
// notional it adds to a LIVE grid (same rate split as paperEntryFee — the
// pour's neutral margin goes into new passive limits, directional margin
// crosses the book). Directional pours add the full leveraged margin; a
// neutral grid holds roughly half its ladder as inventory at any time, so
// the pour's added inventory is priced at the half-notional convention
// (deterministic — the paper row carries no live inventory column; the
// manage-loop tranche path uses the actual mark instead).
func paperPourEntryFee(direction string, investment decimal.Decimal, leverage int) decimal.Decimal {
	if !investment.IsPositive() || leverage < 1 {
		return decimal.Zero
	}
	notional := investment.Mul(decimal.NewFromInt(int64(leverage)))
	if strings.ToUpper(strings.TrimSpace(direction)) != "NEUTRAL" {
		return notional.Mul(paperStopTakerBps).Div(decimal.NewFromInt(10000))
	}
	return notional.Div(decimal.NewFromInt(2)).Mul(paperEntryRateBps("NEUTRAL")).Div(decimal.NewFromInt(10000))
}

// paperInitialInventoryNotional is the notional a fresh grid puts on at
// entry (see paperEntryFee).
func paperInitialInventoryNotional(
	direction string,
	lower, upper decimal.Decimal,
	gridNum int,
	investment decimal.Decimal,
	leverage int,
	deployPrice decimal.Decimal,
) decimal.Decimal {
	if !investment.IsPositive() || leverage < 1 {
		return decimal.Zero
	}
	notional := investment.Mul(decimal.NewFromInt(int64(leverage)))
	if strings.ToUpper(direction) != "NEUTRAL" {
		return notional
	}
	if !upper.GreaterThan(lower) || gridNum < 2 || !deployPrice.GreaterThan(decimal.Zero) {
		// No usable geometry: mid-deploy convention — half the notional, the
		// below-mid half a neutral ladder starts holding.
		return notional.Div(decimal.NewFromInt(2))
	}
	levelWidth := upper.Sub(lower).Div(decimal.NewFromInt(int64(gridNum)))
	mid := upper.Add(lower).Div(decimal.NewFromInt(2))
	halfLevels := decimal.NewFromInt(int64(gridNum)).Div(decimal.NewFromInt(2))
	levelsFromMid := mid.Sub(deployPrice).Div(levelWidth)
	if levelsFromMid.GreaterThan(halfLevels) {
		levelsFromMid = halfLevels
	}
	if levelsFromMid.LessThan(halfLevels.Neg()) {
		levelsFromMid = halfLevels.Neg()
	}
	perLevel := notional.Div(decimal.NewFromInt(int64(gridNum)))
	return levelsFromMid.Abs().Mul(perLevel)
}

// bufferedGridLevel maps a price to its grid level with the fill buffer
// (v2.0.89 friction calibration): a level counts as crossed only when the
// price traded paperFillBufferPct (0.03%) BEYOND its boundary — a touch is
// not a fill. The buffer is applied against the direction of movement since
// the last observation (the ladder baseline), so a shallow boundary-kiss
// oscillation books zero pairs while a genuine traverse still completes them.
// Multi-level traverses keep working: the buffered price simply demands the
// extra depth on the final, partially-traversed level.
func bufferedGridLevel(
	lower, upper decimal.Decimal,
	gridNum int,
	price decimal.Decimal,
	lastLevel int,
) int {
	raw := gridLevelForPrice(lower, upper, gridNum, price)
	if raw == lastLevel || !price.GreaterThan(decimal.Zero) {
		return raw
	}
	buffer := paperFillBufferPct.Div(decimal.NewFromInt(100))
	if raw < lastLevel {
		// Downward move: pretend the price is buffer% higher — the dip must
		// exceed the buffer before the lower level counts.
		return gridLevelForPrice(lower, upper, gridNum, price.Mul(decimal.NewFromInt(1).Add(buffer)))
	}
	// Upward move: pretend the price is buffer% lower.
	return gridLevelForPrice(lower, upper, gridNum, price.Mul(decimal.NewFromInt(1).Sub(buffer)))
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
//
// The feeBps argument is the PER-LEG fee pair fills pay — pass the maker fee
// (pionexMakerFeeBps), not the taker+slippage composite.
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
//
// Since v2.0.6 rateBps is SIGNED and usually the real cross-exchange rate
// from funding_snapshots: positive = longs pay shorts (the classic drag on
// dip inventories), negative = longs RECEIVE (short squeezes paying carry
// to the grid). The caller's pay/receive flip turns the signed magnitude
// into the correct cash flow either way.
func fundingAccrual(
	exposure decimal.Decimal,
	rateBps decimal.Decimal,
	openedAt time.Time,
	lastFundingAt *time.Time,
	now time.Time,
) (*decimal.Decimal, *time.Time) {
	if !exposure.IsPositive() || rateBps.IsZero() {
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

// paperTrailPrecision derives the trailing-SL rounding scale for the paper
// arm from the grid's own lower bound (v2.0.155 review I-2): paper rows
// carry no deploy-time precision marker, and the exchange-accepted lower
// price is the best witness of the symbol's tick scale. Capped at 8 like
// the deploy heuristic.
func paperTrailPrecision(lower decimal.Decimal) int {
	if exp := lower.Exponent(); exp < 0 {
		if prec := int(-exp); prec > 0 && prec <= 8 {
			return prec
		}
	}
	return 4
}
