package autogrid

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

// wickShieldReArmCooldown blocks a second arming for five minutes after any
// shield cleared. NEAR #1401 (2026-09-27): the shield armed at 05:46:54,
// cleared on a one-minute bounce ("price recovered"), re-armed at 05:49:47 on
// the same unclosed-wick pattern and deferred the max-loss stop a second
// 90s window — 4m25s total under a breached cap, −$14.90 settled on an $8
// cap. One grace per signal episode is the contractual maximum.
const wickShieldReArmCooldown = 5 * time.Minute

// evaluateWickShield checks whether a structural-invalidation or range-break
// close was triggered by an intraday liquidity sweep wick. v2.0.119 rules,
// each born from the NEAR #1401 post-mortem:
//
//   - ActionCloseStopLoss is OUT OF SCOPE entirely (enforced by the caller):
//     a breached dollar cap is not a wick question, and any relative
//     threshold loses to a 2%/30s spike between passes.
//   - The wick side follows the SIGNED POSITION, not bot.direction — a
//     NEUTRAL grid holding short inventory dies on the UPPER move, so the
//     lower-wick branch must never arm for it (the exact NEAR inversion).
//   - Arming inspects only CLOSED candles: the last closed bar is
//     candles[len-2], the same bar the invalidation check reads. Arming on
//     the unclosed candles[len-1] saw intrabar wicks that close away.
//   - A flat position gets no shield: with zero inventory there is nothing
//     a wick can damage, and the close itself is already risk-free.
//
// If a rejection wick is detected, the close is deferred for up to graceSec
// seconds, at most once per signal episode.
func (worker *Worker) evaluateWickShield(
	ctx context.Context,
	symbol string,
	signedPos decimal.Decimal,
	currentPrice decimal.Decimal,
	triggeredAt *string,
	extreme *decimal.Decimal,
	lastClearedAt *string,
	graceSec int,
) (shouldHold bool, newTriggeredAt *string, newExtreme *decimal.Decimal, reason string) {
	if graceSec <= 0 {
		graceSec = 90
	}
	shortSide := signedPos.IsNegative()
	if signedPos.IsZero() {
		return false, nil, nil, "flat position — wick shield not applicable"
	}

	// Case 1: Wick shield is already armed on this bot
	if triggeredAt != nil && strings.TrimSpace(*triggeredAt) != "" {
		stamped, err := time.Parse(time.RFC3339, strings.TrimSpace(*triggeredAt))
		if err != nil {
			return false, nil, nil, "invalid wick shield timestamp"
		}
		if extreme == nil || extreme.IsZero() {
			return false, nil, nil, "missing wick shield extreme price"
		}

		// Check if extreme was violated
		if shortSide {
			// For a short inventory, breach means price punched HIGHER than the wick high
			if currentPrice.GreaterThan(*extreme) {
				return false, nil, nil, fmt.Sprintf("wick shield breached: price %s > wick high %s", currentPrice.String(), extreme.String())
			}
		} else {
			// For a long inventory, breach means price punched LOWER than the wick low
			if currentPrice.LessThan(*extreme) {
				return false, nil, nil, fmt.Sprintf("wick shield breached: price %s < wick low %s", currentPrice.String(), extreme.String())
			}
		}

		// Check if a 5m candle has already closed beyond the extreme level (confirmed structural close)
		if worker.publicClient != nil {
			if candles, err := worker.publicClient.GetKlines(ctx, symbol, "5M", 2); err == nil && len(candles) >= 2 {
				closedCandle := candles[len(candles)-2]
				if shortSide {
					if closedCandle.Close.GreaterThan(*extreme) {
						return false, nil, nil, fmt.Sprintf("wick shield invalidated: 5M candle closed body (%s) above extreme (%s)", closedCandle.Close.String(), extreme.String())
					}
				} else {
					if closedCandle.Close.LessThan(*extreme) {
						return false, nil, nil, fmt.Sprintf("wick shield invalidated: 5M candle closed body (%s) below extreme (%s)", closedCandle.Close.String(), extreme.String())
					}
				}
			}
		}

		elapsed := time.Since(stamped)
		if elapsed < time.Duration(graceSec)*time.Second {
			remaining := graceSec - int(elapsed.Seconds())
			return true, triggeredAt, extreme, fmt.Sprintf("wick shield active: holding (extreme %s, %ds left in grace window)", extreme.String(), remaining)
		}

		// Grace window expired and price hasn't recovered
		return false, nil, nil, fmt.Sprintf("wick shield grace window expired (%ds passed)", int(elapsed.Seconds()))
	}

	// Case 2: Shield not yet armed. One grace per signal episode: a shield
	// cleared less than wickShieldReArmCooldown ago may not re-arm — the
	// NEAR double-deferral class.
	if lastClearedAt != nil && strings.TrimSpace(*lastClearedAt) != "" {
		if cleared, err := time.Parse(time.RFC3339, strings.TrimSpace(*lastClearedAt)); err == nil &&
			time.Since(cleared) < wickShieldReArmCooldown {
			return false, nil, nil, fmt.Sprintf("wick shield re-arm cooldown (%ds of %ds passed)",
				int(time.Since(cleared).Seconds()), int(wickShieldReArmCooldown.Seconds()))
		}
	}

	if worker.publicClient == nil {
		return false, nil, nil, "public client not initialized"
	}

	// Fetch 3 candles of 5M interval: len-1 is the live (unclosed) bar,
	// len-2 is the last CLOSED bar — the only bar allowed to arm the shield.
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "5M", 3)
	if err != nil || len(candles) < 2 {
		return false, nil, nil, "failed to fetch closed 5M candles for wick analysis"
	}

	closedCandle := candles[len(candles)-2]
	analysis := marketdata.AnalyzeCandle(closedCandle)

	if shortSide {
		// Bearish wick against a short inventory: long UPPER wick, price bouncing down
		if (analysis.UpperWickRatio >= 0.40 || analysis.IsPinBarBear) &&
			currentPrice.LessThan(closedCandle.High) {
			nowStr := time.Now().UTC().Format(time.RFC3339)
			high := closedCandle.High
			return true, &nowStr, &high, fmt.Sprintf("armed wick shield: closed 5m upper wick ratio %.2f (high %s)", analysis.UpperWickRatio, high.String())
		}
	} else {
		// Bullish wick against a long inventory: long LOWER wick, price bouncing up
		if (analysis.LowerWickRatio >= 0.40 || analysis.IsPinBarBull) &&
			currentPrice.GreaterThan(closedCandle.Low) {
			nowStr := time.Now().UTC().Format(time.RFC3339)
			low := closedCandle.Low
			return true, &nowStr, &low, fmt.Sprintf("armed wick shield: closed 5m lower wick ratio %.2f (low %s)", analysis.LowerWickRatio, low.String())
		}
	}

	return false, nil, nil, "no rejection wick formed"
}

// directionalConfirmedByPriceAction checks if recent candlestick price action
// (Pin Bar, Engulfing, or SFP liquidity sweep) confirms a directional entry thesis (LONG or SHORT) on 15M candles.
func (worker *Worker) directionalConfirmedByPriceAction(ctx context.Context, symbol string, trend string) (bool, string) {
	if worker.publicClient == nil {
		return false, "no public client"
	}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", 3)
	if err != nil || len(candles) < 2 {
		return false, "insufficient candle data"
	}
	prev := candles[len(candles)-2]
	curr := candles[len(candles)-1]
	currAnalysis := marketdata.AnalyzeCandle(curr)

	t := strings.ToLower(trend)
	if t == "long" {
		if currAnalysis.IsPinBarBull {
			return true, fmt.Sprintf("Bullish Pin Bar / Hammer (lower wick %.0f%%)", currAnalysis.LowerWickRatio*100)
		}
		if isEngulfing, side := marketdata.DetectEngulfing(prev, curr); isEngulfing && side == "BULLISH" {
			return true, "Bullish Engulfing candle"
		}
		lookbackLow := prev.Low
		if isSweep, wickRatio := marketdata.DetectSFP(lookbackLow, curr); isSweep {
			return true, fmt.Sprintf("Bullish SFP sweep (swept %s, wick %.0f%%, closed %s)", lookbackLow.String(), wickRatio*100, curr.Close.String())
		}
	} else if t == "short" {
		if currAnalysis.IsPinBarBear {
			return true, fmt.Sprintf("Bearish Pin Bar / Shooting Star (upper wick %.0f%%)", currAnalysis.UpperWickRatio*100)
		}
		if isEngulfing, side := marketdata.DetectEngulfing(prev, curr); isEngulfing && side == "BEARISH" {
			return true, "Bearish Engulfing candle"
		}
	}
	return false, "no confirming candlestick pattern"
}

// calculateFleetNetDelta sums the total directional delta (USDT notional) across
// all active running grid bots to prevent over-concentrated directional exposure.
//
// v2.0.140: the second return value is the NEUTRAL park — the sum of
// ½×invest×leverage across the fleet's RUNNING, UN-shifted NEUTRAL bots.
// The audit hole: an un-shifted NEUTRAL contributed exactly ZERO to net
// delta, so an 8-bot NEUTRAL alt fleet was an invisible shared short-vol
// bet — every one of those grids loads up to ~half its notional of adverse
// inventory in the SAME market-wide move. The park is direction-agnostic
// (ABS, no sign): the callers charge it against whichever side the
// candidate is about to expose. A SHIFTED neutral does not add park — its
// live inventory is already in the delta through shiftPosition×mark, and
// stacking the park on top would double-count that bot.
func (worker *Worker) calculateFleetNetDelta(ctx context.Context, settingsID string, paper bool) (decimal.Decimal, decimal.Decimal, error) {
	var query string
	if paper {
		query = `
			SELECT direction, quote_investment, leverage,
			       COALESCE(NULLIF(model_state->>'shiftPosition','')::NUMERIC, 0),
			       COALESCE(mark_price, entry_price, (lower_price + upper_price)/2, 0)
			FROM paper_grid_bots
			WHERE settings_id = $1 AND status = 'RUNNING'
		`
	} else {
		query = `
			SELECT direction, quote_investment, leverage,
			       COALESCE(NULLIF(model_state->>'shiftPosition','')::NUMERIC, 0),
			       COALESCE(NULLIF(struct_context->>'entryPrice','')::NUMERIC, (lower_price + upper_price)/2, 0)
			FROM grid_bots
			WHERE autogrid_settings_id = $1 AND status = 'RUNNING' AND bu_order_id IS NOT NULL
		`
	}

	rows, err := worker.db.Query(ctx, query, settingsID)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	defer rows.Close()

	totalDelta := decimal.Zero
	neutralParkHalfNotional := decimal.Zero
	for rows.Next() {
		var direction string
		var investment, shiftPos, markPrice decimal.Decimal
		var leverage int
		if err := rows.Scan(&direction, &investment, &leverage, &shiftPos, &markPrice); err != nil {
			continue
		}

		if leverage <= 0 {
			leverage = 1
		}

		var botDelta decimal.Decimal
		if !shiftPos.IsZero() && markPrice.IsPositive() {
			botDelta = shiftPos.Mul(markPrice)
		} else {
			dir := strings.ToUpper(direction)
			if dir == "LONG" {
				botDelta = investment.Mul(decimal.NewFromInt(int64(leverage)))
			} else if dir == "SHORT" {
				botDelta = investment.Mul(decimal.NewFromInt(int64(leverage))).Neg()
			} else if dir == "NEUTRAL" {
				// v2.0.140 park load: at boundary this grid holds ~half its
				// notional of one-sided inventory — worst-case adverse load
				// the cap must be able to see while the bot is still flat.
				neutralParkHalfNotional = neutralParkHalfNotional.Add(
					investment.Mul(decimal.NewFromInt(int64(leverage))).Div(decimal.NewFromInt(2)))
			}
		}
		totalDelta = totalDelta.Add(botDelta)
	}

	return totalDelta, neutralParkHalfNotional, nil
}

// checkOrderBookCushion queries the L2 depth and evaluates whether the order book
// provides adequate liquidity cushion to absorb orders without slippage cliffs.
//
// v2.0.119: REAL deploys pass failClosed=true — an unavailable book (429,
// transport error, missing client) REJECTS the candidate instead of waving it
// through. The old fail-open opened REAL bots blind exactly during scan
// bursts, when Pionex answers 429 and a 60s limiter cooldown blanks every
// depth call left in the pass. The paper arm keeps fail-open: it is a
// decision-parity sandbox, not a capital path. The same call now enforces the
// configured MaxSpreadPct (dead end-to-end since migration 0049 shipped the
// column with zero consumers): a book whose best bid/ask gap exceeds the
// limit cannot absorb a round trip without paying the gap twice.
func (worker *Worker) checkOrderBookCushion(
	ctx context.Context,
	symbol string,
	currentPrice decimal.Decimal,
	botNotional float64,
	minCushionRatio float64,
	failClosed bool,
	maxSpreadPct decimal.Decimal,
) (ok bool, profile marketdata.DepthProfile, reason string) {
	if worker.publicClient == nil {
		if failClosed {
			return false, marketdata.DepthProfile{}, "no public client (fail-closed)"
		}
		return true, marketdata.DepthProfile{}, "no public client"
	}
	bids, asks, err := worker.publicClient.GetDepth(ctx, symbol, 50)
	if err != nil || len(bids) == 0 || len(asks) == 0 {
		// An empty book is missing data, not a thin book: a live Pionex
		// symbol always carries levels on both sides, and the deploy-path
		// mocks of older tests returned a non-depth JSON that decoded to
		// zero levels — graded "thin" and wrongly rejecting candidates.
		if failClosed && err != nil {
			return false, marketdata.DepthProfile{}, "depth fetch failed (fail-closed): " + err.Error()
		}
		if failClosed {
			return false, marketdata.DepthProfile{}, "пустой стакан (нет уровней) — fail-closed"
		}
		return true, marketdata.DepthProfile{}, "depth unavailable (fail-open)"
	}
	profile = marketdata.ProfileOrderBook(bids, asks, currentPrice, botNotional, minCushionRatio)
	if maxSpreadPct.IsPositive() && len(bids) > 0 && len(asks) > 0 {
		bestBid, bestAsk := bids[0].Price, asks[0].Price
		if bestAsk.GreaterThan(bestBid) && bestBid.GreaterThan(decimal.Zero) {
			mid := bestAsk.Add(bestBid).Div(decimal.NewFromInt(2))
			// maxSpreadPct is stored as a FRACTION (migration 0049: 0.0020
			// = 0.20%) — compare fraction to fraction, print percent.
			spreadFrac := bestAsk.Sub(bestBid).Div(mid)
			if spreadFrac.GreaterThan(maxSpreadPct) {
				return false, profile, fmt.Sprintf("спред %.3f%% > лимита %.3f%% (bid %s / ask %s)",
					spreadFrac.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					maxSpreadPct.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					bestBid.StringFixed(6), bestAsk.StringFixed(6))
			}
		}
	}
	if profile.IsThinBook {
		return false, profile, fmt.Sprintf("глубина 2%% стакана: bid %.1fx / ask %.1fx от размера бота (нужно ≥%.0fx с обеих сторон)",
			profile.BidCushionRatio, profile.AskCushionRatio, minCushionRatio)
	}
	return true, profile, ""
}

// checkKnifePause checks recent market trades. If aggressive market taker dumping
// (≥75% sell volume) is occurring, LONG and NEUTRAL entries are paused to avoid catching a falling knife.
func (worker *Worker) checkKnifePause(
	ctx context.Context,
	symbol string,
	trend string,
) (paused bool, metrics marketdata.TakerFlowMetrics, reason string) {
	// 1. Fast-path check: Live WebSocket OFI and microstructure state.
	// If the real-time order flow engine is already tracking the symbol and detects
	// adverse flow (e.g. dump pressure or confirmed dump for LONG/NEUTRAL), veto immediately.
	// This guarantees that REST failures or delays cannot bypass an active, streaming OFI veto!
	// v2.0.164 (prod 30.09 postmortem): a DATA-HEALTH veto (book desync /
	// recovering / warming after a socket reset) is NO LONGER a hard entry
	// block — it logs DEFER_REST and falls through to the REST taker-flow
	// path below. A socket-wide 1008 rate-limit reset used to mark every
	// book DESYNC and freeze 100% of entries on "awaiting fresh snapshot"
	// while the REST trade tape was clean. Real flow vetoes (dump/pump
	// pressure — IsActionable) still hard-veto. Review P2: if BOTH lanes
	// are dead (desync soft-veto AND the REST fetch fails), the entry is
	// refused — knife protection must never silently drop to zero.
	desyncDeferred := false
	if worker.ofiEngine != nil {
		analysis := worker.ofiEngine.Analyze(symbol)
		if allowed, ofiReason := analysis.CanEnter(trend); !allowed {
			if analysis.IsActionable() {
				logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
					analysis, string(analysis.Readiness()), "VETO", ofiReason, "")
				return true, marketdata.TakerFlowMetrics{}, ofiReason
			}
			desyncDeferred = true
			logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
				analysis, string(analysis.Readiness()), "DEFER_REST", ofiReason,
				"desync-class veto falls through to the REST taker-flow check (v2.0.164)")
		} else if analysis.IsActionable() {
			// Allow with a live directional flow observed — the rare,
			// analysis-worthy accepts (plain NEUTRAL allows would flood the
			// journal with every scan).
			logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
				analysis, string(analysis.Readiness()), "ALLOW", "", "")
		}
	}

	if worker.publicClient == nil {
		if desyncDeferred {
			return true, marketdata.TakerFlowMetrics{}, "order book desync + no public client — обе ленты мертвы, вход заблокирован (v2.0.164)"
		}
		return false, marketdata.TakerFlowMetrics{}, "no public client"
	}
	trades, err := worker.publicClient.GetTrades(ctx, symbol, 60)
	if err != nil || len(trades) == 0 {
		if desyncDeferred {
			return true, marketdata.TakerFlowMetrics{}, "order book desync + REST trades недоступны — обе ленты мертвы, вход заблокирован (v2.0.164)"
		}
		return false, marketdata.TakerFlowMetrics{}, "trades fetch failed (fail-open)"
	}
	metrics = marketdata.AnalyzeTakerFlow(trades)

	// v2.0.123 dynamic microstructure: feed trades into OFI engine as a
	// chronologically sorted batch, roll micro-windows, and finalize for analysis.
	// v2.0.164: same soft/hard split as the fast path — a desync-class veto
	// after the batch falls through to the raw taker-flow checks below.
	if worker.ofiEngine != nil {
		worker.ofiEngine.IngestTradeBatch(symbol, trades)
		analysis := worker.ofiEngine.Analyze(symbol)
		if allowed, ofiReason := analysis.CanEnter(trend); !allowed {
			if analysis.IsActionable() {
				logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
					analysis, string(analysis.Readiness()), "VETO", ofiReason, "")
				return true, metrics, ofiReason
			}
			logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
				analysis, string(analysis.Readiness()), "DEFER_REST", ofiReason,
				"desync-class veto falls through to the raw taker-flow check (v2.0.164)")
		} else if analysis.IsActionable() {
			logOFIDecision(ctx, worker.db, defaultSettingsID(ctx, worker.db), symbol, ofiKindEntryVeto,
				analysis, string(analysis.Readiness()), "ALLOW", "", "")
		}
	}

	t := strings.ToLower(trend)
	if (t == "long" || t == "neutral" || t == "no_trend") && metrics.IsAggressiveDumping {
		return true, metrics, fmt.Sprintf("агрессивный сброс маркет-ордерами: %.0f%% taker sell volume ($%.0f) — нож падает",
			metrics.SellRatio*100, metrics.SellVolumeUSDT)
	}
	if t == "short" && metrics.IsAggressivePumping {
		return true, metrics, fmt.Sprintf("агрессивный памп маркет-ордерами: %.0f%% taker buy volume ($%.0f) — ракета вверх",
			metrics.BuyRatio*100, metrics.BuyVolumeUSDT)
	}
	return false, metrics, ""
}
