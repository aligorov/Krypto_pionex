package marketdata

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// Dynamic targets replace fixed USDT amounts: each bot's take-profit and
// stop-out are derived from what the pair's own volatility actually offers,
// scaled to the invested budget.
//
// take-profit % of budget = clamp(0.6 × effective daily volatility, 1.8..6)
// stop-out    % of budget = clamp(max(0.5 × drawdown, 0.15 × grid span), 1.0..4)
//
// The effective volatility prefers the native Pionex AI Kit reading (live
// per-pair estimate from the exchange) and falls back to the scanner's
// sigma/ATR blend; drawdown prefers the AI Kit maxDrawDown and falls back to
// the scanner model drawdown.
const (
	dynamicTargetVolFraction = 0.85
	dynamicLossDDFraction    = 0.50
	// 4.5 - 10.0%: 5% base target on $200 budget ($10.00 profit per bot cycle)
	// scaling up to 10% ($20.00) on high-momentum trends.
	dynamicTargetMinPct = 4.5
	dynamicTargetMaxPct = 10.0
	// DynamicLossMinPct is the ADAPTIVE_ATR stop-out floor (% of budget×
	// leverage notional). Exported because it is ALSO the fleet-design stop
	// floor the risk derivation (breaker, tranche-2 cap) budgets against —
	// a designed bot can never store a smaller stop than this.
	DynamicLossMinPct = 2.0
	// DynamicLossMaxPct is the stop-out ceiling of the same formula. The
	// tranche-2 per-bot cap must budget against THIS bound, not the floor:
	// σ-scaled stops legally land anywhere in [Min, Max], and a floor-based
	// cap refuses exactly the wide-stop bots the top-up exists to save
	// (prod 2026-09-04: SKYAI 6x skipped ×3 with "$21.57 > $15").
	DynamicLossMaxPct      = 5.0
	dynamicLossRangeFloorK = 0.15
	minRiskRewardRatio     = 1.50
)

type DynamicTargetsInput struct {
	Budget float64
	// Leverage scales the USDT amounts to the exposure the PnL model
	// actually measures: mark-to-market computes directional PnL on
	// budget×leverage notional, so an unscaled 1%-of-budget stop fires on a
	// ~0.5% price move at 2x and ~0.25% at 4x — pure noise (prod: SHORT
	// #328 STOP_LOSS 45 minutes after deploy). Scaling both amounts keeps
	// the stop distance in PRICE terms constant across leverage while
	// preserving the RR ratio. 0 or 1 = unscaled (1x).
	Leverage int
	// AIVolatilityPct / AIDrawdownPct come from the native Pionex AI Kit
	// when an account is configured; zero means "not available".
	AIVolatilityPct float64
	AIDrawdownPct   float64
	// ScannerVolatilityPct is the annualized-to-daily sigma estimate and
	// ScannerATRPct the ATR(14)/price reading from the regime detector.
	ScannerVolatilityPct float64
	ScannerATRPct        float64
	ScannerDrawdownPct   float64
	// RangeSpanPct is the DEPLOYED grid span (upper−lower)/mid in % — the
	// quantity the PnL model actually marks against. It couples the loss
	// floor to a full normal traverse (see dynamicLossRangeFloorK) so the
	// $-stop sits structurally outside the grid's own oscillation. 0 =
	// unknown (manual deploys) — the floor then contributes nothing.
	RangeSpanPct float64
}

type DynamicTargets struct {
	TargetUSDT     float64
	MaxLossUSDT    float64
	TargetPct      float64
	LossPct        float64
	VolSource      string
	DrawdownSource string
}

// ComputeDynamicTargets turns market readings into a per-bot PnL target and
// stop-out in USDT. Wide, wild ranges earn bigger targets; quiet ranges get
// modest ones — the numbers follow the market with a strictly positive Risk-Reward Ratio (>= 1.50:1).
func ComputeDynamicTargets(input DynamicTargetsInput) DynamicTargets {
	vol, volSource := input.AIVolatilityPct, "pionex_ai_kit"
	if vol <= 0 {
		// Blend the scanner sigma with ATR: sigma captures overall dispersion,
		// ATR the recent intraday travel a grid actually harvests.
		vol = 0.5*input.ScannerVolatilityPct + 0.5*input.ScannerATRPct
		volSource = "scanner_sigma_atr_blend"
	}
	drawdown, ddSource := input.AIDrawdownPct, "pionex_ai_kit"
	if drawdown <= 0 {
		drawdown = input.ScannerDrawdownPct
		ddSource = "scanner_model"
	}

	lossFloor := dynamicLossDDFraction * drawdown
	if input.RangeSpanPct > 0 {
		if rangeFloor := dynamicLossRangeFloorK * input.RangeSpanPct; rangeFloor > lossFloor {
			lossFloor = rangeFloor
		}
	}
	lossPct := clamp(lossFloor, DynamicLossMinPct, DynamicLossMaxPct)
	rawTargetPct := math.Max(dynamicTargetVolFraction*vol, lossPct*minRiskRewardRatio)
	targetPct := clamp(rawTargetPct, dynamicTargetMinPct, dynamicTargetMaxPct)
	budget := math.Max(input.Budget, 0)
	lev := 1.0
	if input.Leverage > 1 {
		lev = float64(input.Leverage)
	}
	return DynamicTargets{
		TargetUSDT:     budget * targetPct / 100 * lev,
		MaxLossUSDT:    budget * lossPct / 100 * lev,
		TargetPct:      targetPct,
		LossPct:        lossPct,
		VolSource:      volSource,
		DrawdownSource: ddSource,
	}
}

// AdaptiveBotTargetInput carries market structure, volatility, and order book
// characteristics needed to calculate native price-based TP/SL and strategy.
type AdaptiveBotTargetInput struct {
	Symbol         string
	Direction      string // "LONG", "SHORT", "NEUTRAL"
	CurrentPrice   decimal.Decimal
	LowerPrice     decimal.Decimal
	UpperPrice     decimal.Decimal
	GridNum        int
	Budget         float64
	Leverage       int
	ATR            float64
	OrderBookDepth *DepthProfile
	SRAnalysis     *SRAnalysisResult
	MinRiskReward  float64
	TakerFeeBps    float64
	SlippageBps    float64
	FundingFeeBps  float64
}

// AdaptiveBotTargetsResult returns individualized price targets, stops,
// and net true USDT profit/loss projections for the bot.
type AdaptiveBotTargetsResult struct {
	TargetPrice      decimal.Decimal  `json:"targetPrice"`
	StopLossPrice    decimal.Decimal  `json:"stopLossPrice"`
	StopLossHigh     *decimal.Decimal `json:"stopLossHigh,omitempty"`
	TargetUSDT       float64          `json:"targetUsdt"`
	MaxLossUSDT      float64          `json:"maxLossUsdt"`
	RiskRewardRatio  float64          `json:"riskRewardRatio"`
	AdaptiveStrategy string           `json:"adaptiveStrategy"`
	Reason           string           `json:"reason"`
}

// Strategy taxonomy for individual bots
const (
	StrategyOFIWallDefense      = "OFI_WALL_DEFENSE"
	StrategySRMomentumRunner    = "SR_MOMENTUM_RUNNER"
	StrategyMeanReversionOU     = "MEAN_REVERSION_OU"
	StrategyVolatilityExpansion = "VOLATILITY_EXPANSION"
)

// ComputeIndividualTargetPrices computes individualized, structural TP and SL
// price levels for a bot using L2 order book wall front-running, S/R swing shelves,
// and ATR buffers. Replaces rigid USDT clamps with true quantitative market levels,
// enforcing R:R >= 1.80 and calculating net profit after taker fees, slippage, and funding.
func ComputeIndividualTargetPrices(input AdaptiveBotTargetInput) AdaptiveBotTargetsResult {
	currPriceF, _ := input.CurrentPrice.Float64()
	if currPriceF <= 0 {
		return AdaptiveBotTargetsResult{AdaptiveStrategy: StrategyVolatilityExpansion}
	}
	lowerF, _ := input.LowerPrice.Float64()
	upperF, _ := input.UpperPrice.Float64()

	minRR := input.MinRiskReward
	if minRR <= 0 {
		minRR = 1.8 // Default quantitative minimum R:R
	}
	atr := input.ATR
	if atr <= 0 {
		atr = 0.02 * currPriceF
	}
	takerBps := input.TakerFeeBps
	if takerBps <= 0 {
		takerBps = 5.0
	}
	slipBps := input.SlippageBps
	if slipBps <= 0 {
		slipBps = 2.0
	}
	fundBps := input.FundingFeeBps
	if fundBps <= 0 {
		fundBps = 1.5
	}
	budget := input.Budget
	if budget <= 0 {
		budget = 100.0
	}
	lev := input.Leverage
	if lev < 1 {
		lev = 1
	}

	strategy := StrategyVolatilityExpansion
	direction := input.Direction
	if direction == "" {
		direction = "LONG"
	}

	// Strategy selection based on market microstructure & order flow
	if input.OrderBookDepth != nil && (input.OrderBookDepth.HasBidWall || input.OrderBookDepth.HasAskWall) {
		if input.OrderBookDepth.ImbalanceRatio > 0.58 || input.OrderBookDepth.ImbalanceRatio < 0.42 {
			strategy = StrategyOFIWallDefense
		}
	} else if input.SRAnalysis != nil && (input.SRAnalysis.ResistStrength > 0.6 || input.SRAnalysis.SupportStrength > 0.6) {
		strategy = StrategySRMomentumRunner
	} else if direction == "NEUTRAL" {
		strategy = StrategyMeanReversionOU
	}

	var tp, sl decimal.Decimal
	var slHigh *decimal.Decimal

	switch direction {
	case "SHORT":
		// Target: front-run nearest bid wall or support shelf
		if input.OrderBookDepth != nil && input.OrderBookDepth.HasBidWall && input.OrderBookDepth.BidWallPrice.GreaterThan(decimal.Zero) && input.OrderBookDepth.BidWallPrice.LessThan(input.CurrentPrice) {
			tp = input.OrderBookDepth.BidWallPrice.Mul(decimal.NewFromFloat(1.0015))
		} else if input.SRAnalysis != nil && input.SRAnalysis.NearestSupport > 0 && input.SRAnalysis.NearestSupport < currPriceF {
			tp = decimal.NewFromFloat(input.SRAnalysis.NearestSupport * 1.0010)
		} else {
			tp = input.LowerPrice
			if tp.GreaterThanOrEqual(input.CurrentPrice) {
				tp = input.CurrentPrice.Sub(decimal.NewFromFloat(2.0 * atr))
			}
		}

		// Stop loss: placed above upper price and resistance with ATR buffer
		rVal := upperF
		if rVal <= currPriceF {
			rVal = currPriceF + 1.5*atr
		}
		if input.OrderBookDepth != nil && input.OrderBookDepth.HasAskWall && input.OrderBookDepth.AskWallPrice.GreaterThan(input.UpperPrice) {
			awF, _ := input.OrderBookDepth.AskWallPrice.Float64()
			if awF > rVal {
				rVal = awF
			}
		} else if input.SRAnalysis != nil && input.SRAnalysis.NearestResist > rVal {
			rVal = input.SRAnalysis.NearestResist
		}
		sl = decimal.NewFromFloat(rVal + 0.4*atr)

	case "NEUTRAL":
		strategy = StrategyMeanReversionOU
		// Stop loss below lower grid and above upper grid
		slLowVal := lowerF - 0.4*atr
		if slLowVal <= 0 {
			slLowVal = lowerF * 0.95
		}
		sl = decimal.NewFromFloat(slLowVal)
		slHighVal := upperF + 0.4*atr
		slHighDec := decimal.NewFromFloat(slHighVal)
		slHigh = &slHighDec
		// Take profit front-running upper range
		tp = input.UpperPrice.Mul(decimal.NewFromFloat(0.9985))

	default: // "LONG"
		// Target: front-run nearest ask wall or resistance shelf
		if input.OrderBookDepth != nil && input.OrderBookDepth.HasAskWall && input.OrderBookDepth.AskWallPrice.GreaterThan(input.CurrentPrice) {
			tp = input.OrderBookDepth.AskWallPrice.Mul(decimal.NewFromFloat(0.9985))
		} else if input.SRAnalysis != nil && input.SRAnalysis.NearestResist > currPriceF {
			tp = decimal.NewFromFloat(input.SRAnalysis.NearestResist * 0.9990)
		} else {
			tp = input.UpperPrice
			if tp.LessThanOrEqual(input.CurrentPrice) {
				tp = input.CurrentPrice.Add(decimal.NewFromFloat(2.0 * atr))
			}
		}

		// Stop loss: placed below lower price and support with ATR buffer
		sVal := lowerF
		if sVal >= currPriceF || sVal <= 0 {
			sVal = currPriceF - 1.5*atr
		}
		if input.OrderBookDepth != nil && input.OrderBookDepth.HasBidWall && input.OrderBookDepth.BidWallPrice.LessThan(input.LowerPrice) && input.OrderBookDepth.BidWallPrice.GreaterThan(decimal.Zero) {
			bwF, _ := input.OrderBookDepth.BidWallPrice.Float64()
			if bwF < sVal && bwF > 0 {
				sVal = bwF
			}
		} else if input.SRAnalysis != nil && input.SRAnalysis.NearestSupport > 0 && input.SRAnalysis.NearestSupport < sVal {
			sVal = input.SRAnalysis.NearestSupport
		}
		slVal := sVal - 0.4*atr
		if slVal <= 0 {
			slVal = sVal * 0.95
		}
		sl = decimal.NewFromFloat(slVal)
	}

	// Calculate distances and enforce R:R >= minRR
	rewardDist := math.Abs(tp.Sub(input.CurrentPrice).InexactFloat64())
	riskDist := math.Abs(input.CurrentPrice.Sub(sl).InexactFloat64())
	if riskDist <= 0 {
		riskDist = 0.01 * currPriceF
	}

	rr := rewardDist / riskDist
	if rr < minRR {
		// Extend target so that reward / risk matches minRR
		requiredReward := minRR * riskDist
		if direction == "SHORT" {
			tp = input.CurrentPrice.Sub(decimal.NewFromFloat(requiredReward))
			if tp.LessThanOrEqual(decimal.Zero) {
				tp = input.CurrentPrice.Mul(decimal.NewFromFloat(0.5))
			}
		} else { // LONG or NEUTRAL
			tp = input.CurrentPrice.Add(decimal.NewFromFloat(requiredReward))
		}
		rewardDist = math.Abs(tp.Sub(input.CurrentPrice).InexactFloat64())
		rr = rewardDist / riskDist
	}

	// Calculate net true profit and max loss accounting for taker fees, slippage, and funding
	notional := budget * float64(lev)
	roundTripFeeFrac := (2.0*(takerBps+slipBps) + fundBps) / 10000.0
	frictionUSDT := notional * roundTripFeeFrac

	grossTargetUSDT := notional * (rewardDist / currPriceF)
	netTargetUSDT := grossTargetUSDT - frictionUSDT
	if netTargetUSDT < 1.0 {
		netTargetUSDT = 1.0
	}

	grossLossUSDT := notional * (riskDist / currPriceF)
	netLossUSDT := grossLossUSDT + frictionUSDT

	reason := fmt.Sprintf("strategy=%s direction=%s TP=%s SL=%s RR=%.2f net_target=$%.2f max_loss=$%.2f",
		strategy, direction, tp.StringFixed(4), sl.StringFixed(4), rr, netTargetUSDT, netLossUSDT)

	return AdaptiveBotTargetsResult{
		TargetPrice:      tp,
		StopLossPrice:    sl,
		StopLossHigh:     slHigh,
		TargetUSDT:       netTargetUSDT,
		MaxLossUSDT:      netLossUSDT,
		RiskRewardRatio:  rr,
		AdaptiveStrategy: strategy,
		Reason:           reason,
	}
}

// Grid density doctrine (v2.0.75): DENSITY SCALES WITH THE MARGIN, not with
// a fee-floor guess. The old economics floored the step at ~0.80% and clamped
// the count at 6..14, so every $200-notional 4%-span deploy collapsed to 6
// levels of 0.64-0.75% — three such bots produced ZERO grid profit in 8h
// while the dense deployments (0.22% step, 29-60 levels) harvested
// everything. The only two economic constraints that matter:
//
//   - the step floor: 0.25% — dense enough that a normal oscillation
//     crosses levels, wide enough that maker fees (2×~2-5bps round trip)
//     stay a small fraction of the captured step;
//   - the per-level notional floor: notional/levels ≥ $8 — below it the
//     per-fill harvest is dust and the order sits near exchange minimums.
//
// levels = clamp(round(span / max(0.25%, $8-step)), 6, 500).
// $100×2x on a 4% span → 16 levels × $12.5; $50 on the same span → the
// min-order step widens to 0.64% → 6 levels × $8.33.
const (
	// GridStepFloorPct is the economic density floor for the grid step.
	GridStepFloorPct = 0.25
	// StepFloorRoundTripMultiple is how many round trips a level step must
	// clear: the floor = multiple × RoundTripCostPct(fee, slip). v2.0.94
	// raised it 2.0 → 2.5 (0.35% at the 5/2 bps fleet default) on the weekly
	// mining of 155 paper outcomes: every one of the 8 stop-outs of the week
	// (STRUCT_INVALID_ANTI_HUNT/RANGE_BREAK, −$14.87) sat in the ≤0.31% band
	// this floor admits, avg +$0.24/bot — while the 0.31–0.45% band (the new
	// floor's center) averaged +$0.93 with 1 loser and 0 stops. External
	// consensus (Bitsgap/QuickNode/Quantpedia) is step ≥ 2–3× round-trip; the
	// old 2× floor is a survival minimum, not a profitable one.
	StepFloorRoundTripMultiple = 2.5
	// MinGridLevelNotionalUSDT is the smallest acceptable per-level order
	// notional (budget×leverage/levels).
	MinGridLevelNotionalUSDT = 8.0
	// GridLevelsMin/Max clamp the count: 6 keeps a grid a grid, 500 is the
	// Pionex futures contract row ceiling. Exported (v2.0.93): the AI Kit
	// adoption clamp and any other density source must adopt the same floor
	// the doctrine clamps to.
	GridLevelsMin = 6
	GridLevelsMax = 500
)

// GridLevelsForRange derives the grid level count for a span (%) under the
// margin-density doctrine: the step is max(feeGateFloor, the step at which
// every level still carries ≥ $8 of the budget×leverage notional), where
// feeGateFloor = 2×RoundTripCostPct at the ACTUAL fee/slippage pair the fleet
// runs (v2.0.93 parameterization: the floor used to be pinned to the
// fleet-default 5/2 bps, so an operator raising feeBps starved the fleet at a
// gate the floor no longer matched — the gate and the density source MUST
// read the same numbers). notional ≤ 0 (unknown) falls back to the pure step
// floor; fee/slippage ≤ 0 (unknown) falls back to the fleet-default floor
// (DefaultGridStepFloorPct). floor() — not round() — is load-bearing:
// rounding UP past notional/$8 would silently shrink the per-level notional
// below the floor the whole formula exists to protect.
func GridLevelsForRange(rangePct, notionalUSDT, feeBps, slippageBps float64) int {
	if rangePct <= 0 {
		return 8
	}
	stepPct := FeeGateStepFloorPct(feeBps, slippageBps)
	if notionalUSDT > 0 {
		if minOrderStep := MinGridLevelNotionalUSDT * rangePct / notionalUSDT; minOrderStep > stepPct {
			stepPct = minOrderStep
		}
	}
	levels := math.Floor(rangePct / stepPct)
	return int(clamp(levels, GridLevelsMin, GridLevelsMax))
}

// FeeGateStepFloorPct is the density step floor as a function of the ACTUAL
// round-trip costs: StepFloorRoundTripMultiple × (fee + slippage) on both
// legs, in percent. It is the same invariant ValidateMinGridStep and
// FeeGateRejection enforce — the density floor and the deploy-time fee-gate
// are one number, derived from one input. Unknown costs (fee+slippage ≤ 0)
// degrade to the fleet-default floor.
func FeeGateStepFloorPct(feeBps, slippageBps float64) float64 {
	if feeBps <= 0 && slippageBps <= 0 {
		return DefaultGridStepFloorPct()
	}
	return StepFloorRoundTripMultiple * RoundTripCostPct(feeBps, slippageBps)
}

// DefaultGridStepFloorPct is the DOCUMENTED FALLBACK of FeeGateStepFloorPct:
// 2.5× the round-trip cost at the fleet-default 5/2 bps = 0.35%. It stays as
// the contract for pure paths that genuinely have no live settings (and as
// the degenerate-input floor inside FeeGateStepFloorPct); every real path
// (scanner, mesh, AI Kit clamp, manual deploy, DGT re-center) passes its own
// feeBps/slippageBps through so the density floor and the fee-gate can never
// disagree again.
func DefaultGridStepFloorPct() float64 {
	return StepFloorRoundTripMultiple * RoundTripCostPct(5, 2) // 0.35% at fleet defaults
}

// RoundTripCostPct returns the friction of ONE grid level round trip in
// percent of price: two legs (buy + sell), each paying feeBps + slippageBps.
// At the fleet defaults (5 bps fee / 2 bps slippage) that is
// 2 × 7 / 100 = 0.14% — the level step must clear
// StepFloorRoundTripMultiple × THAT (2.5× = 0.35% since v2.0.94).
func RoundTripCostPct(feeBps, slippageBps float64) float64 {
	return 2.0 * (feeBps + slippageBps) / 100.0 // bps → %, × 2 legs
}

// stepFloorEpsilon absorbs the float representation of the boundary: at the
// fleet defaults the bar is 2.5×0.14 = 0.35000000000000003 in double, so a
// grid whose realized step is EXACTLY 0.35% must pass, not die on the last
// bit (prod 2026-09-12: ACE/APT top-score candidates rejected with the
// displayed "0.35% < 0.14%×2.5" — born-viable geometry starved on rounding).
const stepFloorEpsilon = 1e-9

// ValidateMinGridStep checks the v2.0.89 fee-gate invariant (floor raised to
// 2.5× round-trip in v2.0.94): the per-level step must be at least
// StepFloorRoundTripMultiple × the round-trip cost (fee + slippage on both
// legs). A grid whose step is below that bar has too little cost buffer for
// realistic execution friction; it is rejected as economically fragile,
// while the gate itself is not a claim that every such cycle loses money.
func ValidateMinGridStep(stepPct, feeBps, slippageBps float64) bool {
	return stepPct >= StepFloorRoundTripMultiple*RoundTripCostPct(feeBps, slippageBps)-stepFloorEpsilon
}

// FeeGateRejection is the shared fee-gate verdict (v2.0.89-A research fix,
// P1; floor raised to 2.5× round-trip in v2.0.94). stepPct is the FINAL
// realized step of the grid — span_pct / grid_num computed on the geometry
// that will actually be persisted/deployed, AFTER every level-count clamp
// (density doctrine, AI Kit row, manual row). It returns the operator-facing
// rejection reason when the step-floor invariant is violated, or ("", false)
// when the step clears the bar.
func FeeGateRejection(stepPct, feeBps, slippageBps float64) (string, bool) {
	roundTripPct := RoundTripCostPct(feeBps, slippageBps)
	if stepPct >= StepFloorRoundTripMultiple*roundTripPct-stepFloorEpsilon {
		return "", false
	}
	return fmt.Sprintf(
		"шаг уровня %.2f%% < %.1f× round-trip издержек %.2f%% — шаг ниже целевого буфера издержек (fee-gate)",
		stepPct, StepFloorRoundTripMultiple, roundTripPct), true
}

// GridStepPctForSpan is the realized per-level step of a grid: the span in
// percent of midline divided by the FINAL level count. gridNum < 1 yields 0
// (callers treat 0 as "no valid geometry" and the gate falls back).
func GridStepPctForSpan(spanPct float64, gridNum int) float64 {
	if gridNum < 1 || spanPct <= 0 {
		return 0
	}
	return spanPct / float64(gridNum)
}
