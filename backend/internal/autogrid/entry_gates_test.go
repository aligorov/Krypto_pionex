package autogrid

import (
	"math"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// Known-input RV check: constant +0.5% log-return per 15m candle over the
// 8-candle window → σ_15m = 0.005, annualized = 0.005·√35040 ≈ 0.9365 →
// ≈ 93.65%.
func TestRealizedVolPct15m(t *testing.T) {
	candles := make([]pionex.KlineCandle, 0, 12)
	price := 100.0
	for i := 0; i < 12; i++ {
		candles = append(candles, pionex.KlineCandle{
			Time:   int64(i) * 900_000,
			Close:  decimal.NewFromFloat(price),
			Open:   decimal.NewFromFloat(price / 1.005),
			High:   decimal.NewFromFloat(price * 1.006),
			Low:    decimal.NewFromFloat(price * 0.994),
			Volume: decimal.NewFromFloat(100),
		})
		price *= 1.005
	}
	got := RealizedVolPct15m(candles, 8)
	want := math.Log(1.005) * math.Sqrt(returnsPerYear15m) * 100
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("RV = %.4f%%, want %.4f%%", got, want)
	}
}

// Pionex klines arrive newest-first — the RV window must read the same tail
// regardless of feed order (v2.011 candle-order lesson applied at birth).
func TestRealizedVolPct15mOrderInvariant(t *testing.T) {
	ascending := make([]pionex.KlineCandle, 0, 12)
	price := 100.0
	for i := 0; i < 12; i++ {
		ascending = append(ascending, pionex.KlineCandle{
			Time:  int64(i) * 900_000,
			Close: decimal.NewFromFloat(price),
		})
		if i%2 == 0 {
			price *= 1.008 // uneven returns so ordering matters
		} else {
			price *= 0.997
		}
	}
	descending := make([]pionex.KlineCandle, 0, len(ascending))
	for i := len(ascending) - 1; i >= 0; i-- {
		descending = append(descending, ascending[i])
	}
	a := RealizedVolPct15m(ascending, 8)
	d := RealizedVolPct15m(descending, 8)
	if a == 0 || math.Abs(a-d) > 1e-9 {
		t.Fatalf("RV must be order-invariant: ascending %.6f vs newest-first %.6f", a, d)
	}
}

// Insufficient history must return 0 (gate disarms), never a bogus value.
func TestRealizedVolPct15mTooShort(t *testing.T) {
	if got := RealizedVolPct15m(nil, 8); got != 0 {
		t.Fatalf("nil candles: got %.4f, want 0", got)
	}
	candles := make([]pionex.KlineCandle, 5)
	for i := range candles {
		candles[i] = pionex.KlineCandle{Time: int64(i) * 900_000, Close: decimal.NewFromFloat(100 + float64(i))}
	}
	if got := RealizedVolPct15m(candles, 8); got != 0 {
		t.Fatalf("short history: got %.4f, want 0", got)
	}
}

func TestTrancheAdversePctDirectionSigned(t *testing.T) {
	entry := decimal.NewFromFloat(100)
	// LONG: only price BELOW entry is adverse; a rally is profit and must
	// not arm the signal tranche (v2.0.19 — the |price−entry| form topped
	// up directional bots at their own local extremes).
	if got := trancheAdversePct("LONG", decimal.NewFromFloat(99), entry); got < 0.0099 || got > 0.0101 {
		t.Fatalf("LONG below entry must report 1%% adverse, got %v", got)
	}
	if got := trancheAdversePct("LONG", decimal.NewFromFloat(105), entry); got != 0 {
		t.Fatalf("LONG profit excursion must not count as adverse, got %v", got)
	}
	// SHORT mirrors: only price ABOVE entry is adverse.
	if got := trancheAdversePct("SHORT", decimal.NewFromFloat(101.5), entry); got < 0.0149 || got > 0.0151 {
		t.Fatalf("SHORT above entry must report 1.5%% adverse, got %v", got)
	}
	if got := trancheAdversePct("SHORT", decimal.NewFromFloat(95), entry); got != 0 {
		t.Fatalf("SHORT profit excursion must not count as adverse, got %v", got)
	}
	// NEUTRAL: inventory loads either way — both sides count.
	if got := trancheAdversePct("NEUTRAL", decimal.NewFromFloat(103), entry); got < 0.0299 || got > 0.0301 {
		t.Fatalf("NEUTRAL must count either-side excursion, got %v", got)
	}
	if got := trancheAdversePct("LONG", decimal.NewFromFloat(0), entry); got != 0 {
		t.Fatalf("degenerate price must return 0, got %v", got)
	}
}
