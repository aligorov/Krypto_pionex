package autogrid

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

func mustLevel(price, amount string) pionex.DepthLevel {
	return pionex.DepthLevel{Price: mustDecimal(price), Amount: mustDecimal(amount)}
}

func TestDepthImbalance(t *testing.T) {
	price := mustDecimal("100")
	// Balanced book → 0.5.
	balanced := depthImbalance(
		[]pionex.DepthLevel{mustLevel("99.5", "10")},
		[]pionex.DepthLevel{mustLevel("100.5", "10")},
		price, 1.5)
	if balanced < 0.49 || balanced > 0.51 {
		t.Fatalf("balanced book must read ~0.5, got %v", balanced)
	}
	// Asks 3x bids inside the band → ~0.25.
	askHeavy := depthImbalance(
		[]pionex.DepthLevel{mustLevel("99.5", "10")},
		[]pionex.DepthLevel{mustLevel("100.5", "30")},
		price, 1.5)
	if askHeavy > 0.26 {
		t.Fatalf("ask-heavy book must read ~0.25, got %v", askHeavy)
	}
	// Far-book depth must not count: huge bids 10% away → still balanced.
	farBook := depthImbalance(
		[]pionex.DepthLevel{mustLevel("90", "1000"), mustLevel("99.9", "10")},
		[]pionex.DepthLevel{mustLevel("100.1", "10")},
		price, 1.5)
	if farBook < 0.49 || farBook > 0.51 {
		t.Fatalf("far-book levels must not skew the reading, got %v", farBook)
	}
}
