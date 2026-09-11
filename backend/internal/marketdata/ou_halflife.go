package marketdata

import (
	"math"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// CandleCloses extracts the positive close series from klines, order
// preserved — the input OUHalfLifeSteps expects.
func CandleCloses(candles []pionex.KlineCandle) []float64 {
	closes := make([]float64, 0, len(candles))
	for _, c := range candles {
		if v := c.Close.InexactFloat64(); v > 0 {
			closes = append(closes, v)
		}
	}
	return closes
}

// OU half-life helpers shared by the scanner entry gate (v2.0.94) and the
// autogrid lifecycle timer (v2.0.89). Extracted here so both read ONE
// implementation — autogrid cannot be imported from marketdata (cycle), so
// the direction is: marketdata owns the math, autogrid delegates.

// OUHalfLifeSteps fits the AR(1) regression Δp_t = a + b·p_{t-1} on the
// close series and returns the Ornstein-Uhlenbeck half-life in STEP units:
//
//	b  = cov(p_{t-1}, Δp) / var(p_{t-1})
//	HL = -ln(2) / ln(1+b)
//
// (the exact discrete-OU form; -ln(2)/b is the small-b approximation).
// ok=false when the fit is impossible or the tape is NOT mean-reverting:
// b ≥ 0 (trending) or b ≤ -1 (oscillating/divergent — not an OU process).
func OUHalfLifeSteps(prices []float64) (float64, bool) {
	const minCandles = 30
	if len(prices) < minCandles {
		return 0, false
	}
	n := float64(len(prices) - 1)
	var sumLag, sumDelta, sumLagDelta, sumLagSq float64
	for i := 1; i < len(prices); i++ {
		lag := prices[i-1]
		delta := prices[i] - lag
		sumLag += lag
		sumDelta += delta
		sumLagDelta += lag * delta
		sumLagSq += lag * lag
	}
	cov := sumLagDelta - sumLag*sumDelta/n
	varLag := sumLagSq - sumLag*sumLag/n
	if varLag <= 0 {
		return 0, false
	}
	b := cov / varLag
	if b >= -1e-9 || b <= -1+1e-9 {
		return 0, false
	}
	hl := -math.Ln2 / math.Log(1+b)
	if math.IsNaN(hl) || math.IsInf(hl, 0) || hl <= 0 {
		return 0, false
	}
	return hl, true
}

// CandleIntervalHours converts a settings candle interval ("15M", "60M",
// "4H", "1D"…) to its step length in hours; the default matches the 15m
// scanner cadence when the string is unparseable.
func CandleIntervalHours(interval string) float64 {
	s := strings.ToUpper(strings.TrimSpace(interval))
	num, start := 0, 0
	for start < len(s) && s[start] >= '0' && s[start] <= '9' {
		num = num*10 + int(s[start]-'0')
		start++
	}
	if num == 0 {
		num = 1
	}
	switch {
	case strings.HasSuffix(s, "M") && !strings.HasSuffix(s, "MO"):
		return float64(num) / 60.0
	case strings.HasSuffix(s, "H"):
		return float64(num)
	case strings.HasSuffix(s, "D"):
		return float64(num) * 24.0
	case strings.HasSuffix(s, "W"):
		return float64(num) * 168.0
	default:
		return 0.25
	}
}

// MinOUHalfLifeHours is the v2.0.94 entry gate: a candidate whose measured
// regime half-life is below this bar is rejected at the scanner. Weekly
// mining of 155 paper outcomes (2026-09-04→11): half-life-closed bots with
// HL < 2h averaged −$0.07 (net −$1.59 on 23) while HL 2–4h averaged +$0.46,
// 4–6h +$1.22, ≥6h +$1.37 — monotone. A regime that does not persist at
// least ~2h cannot amortize the round-trip friction before the OU timer
// rotates the grid out. 0 disables the gate.
const MinOUHalfLifeHours = 2.0
