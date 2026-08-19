package autogrid

import (
	"context"
	"math"
	"sort"

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
// times the HAR next-day forecast (both annualized, so the ratio is
// scale-free). Only evaluated when a HAR forecast exists — the ATR-mesh
// fallback path already resizes geometry for high-vol symbols.
func (worker *Worker) volExpansionBlocked(ctx context.Context, symbol string, forecastAnnPct float64) (bool, float64) {
	if forecastAnnPct <= 0 {
		return false, 0
	}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "15M", volExpansionWindow+4)
	if err != nil || len(candles) < volExpansionWindow+1 {
		// No fresh candles → cannot prove expansion → do not block. The
		// research ranks a false block (missing the post-flush window) above
		// a false pass (HAR range sizing still bounds the damage).
		return false, 0
	}
	ratio := RealizedVolPct15m(candles, volExpansionWindow) / forecastAnnPct
	return ratio >= volExpansionRatioThreshold, ratio
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
