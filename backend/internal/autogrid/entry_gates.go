package autogrid

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// Entry-quality gates (v2.0.12, package A of the 2026-08-19 entry research).
//
// Three independent blockers address the two documented failure classes of a
// price-centered neutral grid (arXiv 2506.11921: the naive placement carries
// zero expected alpha — edge lives in WHEN and WHERE the grid starts):
//
//  1. volExpansionBlocked — a fixed-step grid keeps its per-pair edge (step −
//     2·fee) constant while inventory risk grows with σ² (Avellaneda–Stoikov
//     inventory penalty; Moreira–Muir: conditional Sharpe falls in high-vol
//     states). Deploying mid-expansion sizes the range to a transient vol
//     spike that breaks immediately. Block ONLY the expansion itself
//     (RV ≥ 1.5× HAR forecast), never "low vol" — post-flush high vol is the
//     richest harvesting window (Nagel 2012) and must stay enterable.
//  2. fundingFlushBlocked — funding extreme AND falling open interest is the
//     signature of forced deleveraging: the CAUSE of falling knives, not the
//     price symptom (RSI/ADX/Hurst all lag a fresh 2–6h dump; a 48h DFA
//     barely moves). Block while the flush is running; once OI stabilizes
//     the block lifts on its own — no latch.
//  3. revalidateFreshPrice — the scanner's price is captured at scan start
//     and can be minutes old after AI Kit + LLM + backtest waits; the grid,
//     entry and anti-hunt stop must anchor to the live price, and a drift
//     beyond half an ATR means the whole candidate is stale.

const (
	// volExpansionRatioThreshold blocks deployment while trailing realized
	// volatility runs at or above this multiple of the HAR forecast. 1.5 is
	// deliberately high: it fires only on genuine expansion so the post-flush
	// premium window (elevated but DECAYING vol) stays deployable.
	volExpansionRatioThreshold = 1.5
	// volExpansionWindow is how many 15m returns feed the trailing RV (2h).
	volExpansionWindow = 8
	// volExpansionBaseline is the self-referencing fallback window (24h) used
	// when no HAR forecast exists (new listings).
	volExpansionBaseline = 96
	// trancheTimeBox deploys the second tranche unconditionally after this
	// age — capping the insurance premium (deferred pair production on the
	// half-sized grid).
	trancheTimeBox = 24 * time.Hour
	// oiFlushChangePct — OI must be falling by at least this much over the
	// last hour for the funding+OI flush block to arm; smaller moves are
	// collector noise.
	oiFlushChangePct = -0.5
	// freshPriceDriftATR — a candidate whose price moved more than this
	// fraction of ATR(15m) between scan and deploy is stale, not a signal.
	freshPriceDriftATR = 0.5
)

// returnsPerYear15m annualizes 15m-return variance.
const returnsPerYear15m = 365.0 * 24.0 * 4.0

// RealizedVolPct15m computes annualized realized volatility in percent from
// 15m candles over the last `window` closed candles. Feed order is
// normalized (Pionex klines arrive newest-first — v2.0.11 lesson).
func RealizedVolPct15m(candles []pionex.KlineCandle, window int) float64 {
	if window < 2 || len(candles) < window+1 {
		return 0
	}
	sorted := append([]pionex.KlineCandle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })
	tail := sorted[len(sorted)-window-1:]
	sumSq := 0.0
	n := 0
	for i := 1; i < len(tail); i++ {
		prev, _ := tail[i-1].Close.Float64()
		cur, _ := tail[i].Close.Float64()
		if prev <= 0 || cur <= 0 {
			continue
		}
		r := math.Log(cur / prev)
		sumSq += r * r
		n++
	}
	if n < 2 {
		return 0
	}
	return math.Sqrt(sumSq/float64(n)) * math.Sqrt(returnsPerYear15m) * 100
}

// volExpansionBlocked reports whether the symbol is in an active volatility
// expansion: trailing 2h realized vol at or above volExpansionRatioThreshold
// times a reference level (both annualized, so the ratio is scale-free).
//
// With a HAR forecast the reference is the model's next-day estimate. WITHOUT
// one — new listings with <31 daily candles, the knife-prone cohort the
// 2026-08-19 audit showed was uncovered — the reference degrades to the
// symbol's own trailing 24h realized vol: block when the last 2h run at 1.5x
// the past day's baseline. Self-referencing removes the dependence on a
// daily-anchored model at the cost of missing slow vol ramps; both failure
// directions stay bounded by the range sizing itself.
func (worker *Worker) volExpansionBlocked(ctx context.Context, symbol string, forecastAnnPct float64) (bool, float64) {
	if forecastAnnPct > 0 {
		candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", volExpansionWindow+4)
		if err != nil || len(candles) < volExpansionWindow+1 {
			// No fresh candles → cannot prove expansion → do not block. The
			// research ranks a false block (missing the post-flush window)
			// above a false pass (range sizing still bounds the damage).
			return false, 0
		}
		ratio := RealizedVolPct15m(candles, volExpansionWindow) / forecastAnnPct
		return ratio >= volExpansionRatioThreshold, ratio
	}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", volExpansionBaseline+4)
	if err != nil || len(candles) < volExpansionBaseline+1 {
		return false, 0
	}
	baseline := RealizedVolPct15m(candles, volExpansionBaseline)
	if baseline <= 0 {
		return false, 0
	}
	ratio := RealizedVolPct15m(candles, volExpansionWindow) / baseline
	return ratio >= volExpansionRatioThreshold, ratio
}

// trancheTurnConfirmed checks the second tranche's direction gate: after an
// adverse excursion the price must have ticked back toward entry on the last
// CLOSED 15m candle (dz/dt > 0 discretized) before the remaining budget is
// committed. Blindly averaging down on a falling knife is the exact failure
// tranches exist to blunt.
func (worker *Worker) trancheTurnConfirmed(ctx context.Context, symbol string, price, entry decimal.Decimal) bool {
	// Two CONSECUTIVE confirming closes: a single up-tick is a coin flip
	// (audit: one close adds ~30-45 min delay and near-zero information);
	// two in a row filter most whipsaw false-turns.
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", 6)
	if err != nil || len(candles) < 4 {
		return false
	}
	sorted := append([]pionex.KlineCandle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })
	c1, _ := sorted[len(sorted)-2].Close.Float64() // last CLOSED candle
	c2, _ := sorted[len(sorted)-3].Close.Float64()
	c3, _ := sorted[len(sorted)-4].Close.Float64()
	if c1 <= 0 || c2 <= 0 || c3 <= 0 {
		return false
	}
	if price.LessThan(entry) {
		return c1 > c2 && c2 > c3 // adverse-down: two closes turning up
	}
	return c1 < c2 && c2 < c3 // adverse-up: two closes turning down
}

// fundingFlushBlocked reports whether the symbol is mid deleveraging-flush:
// cross-exchange funding at an extreme while aggregate open interest is
// falling — longs being force-unwound into a knife. Funding alone is
// deliberately insufficient (it persists for weeks in trends); missing OI
// coverage disarms the gate rather than blocking on one leg.
func (worker *Worker) fundingFlushBlocked(ctx context.Context, symbol string, fundingRate *decimal.Decimal) (bool, string) {
	if fundingRate == nil {
		return false, ""
	}
	rate, _ := fundingRate.Float64()
	if !marketdata.FundingIsExtreme(rate) {
		return false, ""
	}
	oi, err := worker.market.GetOIChange1h(ctx, symbol)
	if err != nil || oi == nil {
		return false, ""
	}
	if oi.ChangePct <= oiFlushChangePct {
		return true, "funding flush: фординг экстремален, OI падает — принудительный делеверидж"
	}
	return false, ""
}

// revalidateFreshPrice fetches the live ticker and either re-anchors the
// candidate to it (grid, entry, mark and anti-hunt all use CurrentPrice) or
// rejects the candidate as stale after a drift beyond half an ATR.
func (worker *Worker) revalidateFreshPrice(ctx context.Context, candidate *Candidate, atrPct float64) (decimal.Decimal, bool) {
	tickers, err := worker.publicClient.GetTickers(ctx, candidate.Symbol, "PERP")
	if err != nil || len(tickers) == 0 || !tickers[0].Close.IsPositive() {
		// Fail-closed on transport: a deploy decision made on a price we
		// cannot even read is exactly the stale-anchor bug being fixed.
		return decimal.Zero, false
	}
	fresh := tickers[0].Close
	scan := candidate.CurrentPrice
	if scan.IsPositive() {
		drift := fresh.Sub(scan).Abs().Div(scan)
		limit := decimal.NewFromFloat(atrPct / 100.0 * freshPriceDriftATR)
		if drift.GreaterThan(limit) {
			return fresh, false
		}
	}
	return fresh, true
}

// depthImbalance returns the bid share of total order-book notional within
// ±bandPct of the reference price (v2.0.39 DOM gate): 0.5 = balanced,
// <0.25 = asks dominate (sell pressure against LONG/NEUTRAL entries),
// >0.75 = bids dominate (buy pressure against SHORT entries). Levels
// outside the band do not count — far-book depth says nothing about the
// next move.
func depthImbalance(bids, asks []pionex.DepthLevel, price decimal.Decimal, bandPct float64) float64 {
	if !price.IsPositive() || len(bids) == 0 || len(asks) == 0 {
		return 0.5
	}
	ref, _ := price.Float64()
	band := bandPct / 100.0
	notional := func(levels []pionex.DepthLevel) float64 {
		sum := 0.0
		for _, level := range levels {
			levelPrice, _ := level.Price.Float64()
			amount, _ := level.Amount.Float64()
			if levelPrice <= 0 || math.Abs(levelPrice/ref-1) > band {
				continue
			}
			sum += levelPrice * amount
		}
		return sum
	}
	bid, ask := notional(bids), notional(asks)
	if bid+ask <= 0 {
		return 0.5
	}
	return bid / (bid + ask)
}
