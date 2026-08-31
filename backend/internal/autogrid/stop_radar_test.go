package autogrid

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

func d(v string) decimal.Decimal { return mustDecimal(v) }

func TestFirstPassageAnchors(t *testing.T) {
	// Zero-drift one-barrier anchors (reflexion principle doubles the
	// tail): distance a = σ√T → 2Φ(−1) ≈ 0.317; a = 0.5σ√T → 2Φ(−0.5) ≈ 0.617.
	p1 := firstPassageHit(0.01, 0.01, 1.0)
	if math.Abs(p1-0.3173) > 0.01 {
		t.Fatalf("1σ√T distance should hit ~0.317, got %.4f", p1)
	}
	pHalf := firstPassageHit(0.005, 0.01, 1.0)
	if math.Abs(pHalf-0.617) > 0.01 {
		t.Fatalf("0.5σ√T distance should hit ~0.617, got %.4f", pHalf)
	}
	// Far barrier: 3h-vol away over 6h horizon → moderate.
	p2 := firstPassageHit(0.03, 0.01, 6.0)
	if p2 < 0.05 || p2 > 0.5 {
		t.Fatalf("3h-vol over 6h should be moderate, got %.4f", p2)
	}
	if firstPassageHit(0, 0.01, 1) != 1 {
		t.Fatal("zero distance must be certain")
	}
	if firstPassageHit(0.01, 0, 1) != 0 {
		t.Fatal("zero vol must be impossible")
	}
}

func TestScoreBotCloseToStopScoresHigh(t *testing.T) {
	in := radarInput{
		botID: "x", botNumber: 1, symbol: "T_USDT_PERP", direction: "NEUTRAL",
		price: d("99.0"), antiHunt: &[]decimal.Decimal{d("98.5")}[0],
		lower: d("95"), upper: d("105"), atrEntryPct: 1.0, // σ_h ≈ 2%
		total: d("-3"), inventorySide: 1,
	}
	rs := scoreBot(in, 15, 10, 0.45, radarFleet{}, 0, 0)
	if rs.S1 < 0.5 {
		t.Fatalf("0.5%% stop distance at 2%% hourly vol must score high s1, got %.3f", rs.S1)
	}
	if rs.Band < 2 {
		t.Fatalf("close-to-stop bot must land in B2+, got band %d score %.3f", rs.Band, rs.Score)
	}
}

func TestScoreBotSafeBotStaysQuiet(t *testing.T) {
	in := radarInput{
		botID: "x", botNumber: 2, symbol: "T_USDT_PERP", direction: "NEUTRAL",
		price: d("100"), antiHunt: &[]decimal.Decimal{d("80")}[0],
		lower: d("90"), upper: d("110"), atrEntryPct: 0.7,
		total: d("1.5"), inventorySide: 0,
	}
	rs := scoreBot(in, 10, 10, 0.40, radarFleet{}, 0, 0)
	if rs.Band != 0 {
		t.Fatalf("safe bot must stay B0, got band %d score %.3f", rs.Band, rs.Score)
	}
}

// The 2026-08-30 night class: fleet underwater + dominance climbing while
// BTC stays flat — s3 must arm and lift every bot at least into B1.
func TestScoreBotAltDrainFleetSignal(t *testing.T) {
	in := radarInput{
		botID: "x", botNumber: 3, symbol: "T_USDT_PERP", direction: "NEUTRAL",
		price: d("100"), antiHunt: &[]decimal.Decimal{d("94")}[0],
		lower: d("95"), upper: d("105"), atrEntryPct: 0.8,
		total: d("-2"), inventorySide: 1,
	}
	fleet := radarFleet{rhoNeg: 0.9, domSlopeBps: 5.0, btc24h: 0.8, n: 10}
	rs := scoreBot(in, 10, 10, 0.45, fleet, 0, 0)
	if rs.S3 < 0.5 {
		t.Fatalf("alt-drain fleet must arm s3, got %.3f", rs.S3)
	}
	if rs.Band < 1 {
		t.Fatalf("alt-drain must lift bots into B1+, got band %d", rs.Band)
	}
}

func TestScoreBotHurstAdverseMultiplies(t *testing.T) {
	base := radarInput{
		botID: "x", botNumber: 4, symbol: "T_USDT_PERP", direction: "NEUTRAL",
		price: d("100"), antiHunt: &[]decimal.Decimal{d("95")}[0],
		lower: d("95"), upper: d("105"), atrEntryPct: 0.8,
		total: d("-1"), inventorySide: 1,
	}
	quiet := scoreBot(base, 10, 10, 0.45, radarFleet{}, 0, 0)
	trendy := scoreBot(base, 10, 10, 0.70, radarFleet{}, 0, 0)
	if trendy.M5 <= 1.0 || trendy.Score <= quiet.Score {
		t.Fatalf("adverse Hurst must multiply: m5 %.2f, scores %.3f vs %.3f", trendy.M5, trendy.Score, quiet.Score)
	}
}

func TestScoreBotCascadeNeedsTwoLegs(t *testing.T) {
	in := radarInput{
		botID: "x", botNumber: 5, symbol: "T_USDT_PERP", direction: "NEUTRAL",
		price: d("100"), antiHunt: &[]decimal.Decimal{d("97")}[0],
		lower: d("95"), upper: d("105"), atrEntryPct: 0.8, total: d("-1"), inventorySide: 1,
	}
	one := scoreBot(in, 10, 10, 0.45, radarFleet{}, 1, 0.9)
	two := scoreBot(in, 10, 10, 0.45, radarFleet{}, 2, 0.9)
	if one.S4 != 0 {
		t.Fatal("one cascade leg must not arm s4")
	}
	if two.S4 < 0.5 {
		t.Fatalf("two legs must arm s4, got %.3f", two.S4)
	}
}

func TestRadarBandBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		band  int
	}{{0.29, 0}, {0.30, 1}, {0.59, 1}, {0.60, 2}, {0.84, 2}, {0.85, 3}, {0.94, 3}, {0.95, 4}, {1.0, 4}}
	for _, c := range cases {
		if got := radarBand(c.score); got != c.band {
			t.Fatalf("radarBand(%.2f) = %d, want %d", c.score, got, c.band)
		}
	}
}

func TestInventorySideOf(t *testing.T) {
	if s := inventorySideOf("NEUTRAL", d("99"), d("95"), d("105")); s != 1 {
		t.Fatalf("below mid must be long inventory, got %.0f", s)
	}
	if s := inventorySideOf("NEUTRAL", d("101"), d("95"), d("105")); s != -1 {
		t.Fatalf("above mid must be short inventory, got %.0f", s)
	}
	if s := inventorySideOf("LONG", d("90"), d("95"), d("105")); s != 1 {
		t.Fatalf("LONG must be +1, got %.0f", s)
	}
	if s := inventorySideOf("SHORT", d("110"), d("95"), d("105")); s != -1 {
		t.Fatalf("SHORT must be -1, got %.0f", s)
	}
}

// v2.0.52 re-center geometry: same width, price at mid — the anti-hunt
// distance beyond the bound survives the shift.
func TestRecenterBounds(t *testing.T) {
	lower := decimal.NewFromFloat(100)
	upper := decimal.NewFromFloat(110)
	price := decimal.NewFromFloat(103.5)
	nl, nu := recenterBounds(lower, upper, price)
	if !nl.Equal(decimal.NewFromFloat(98.5)) || !nu.Equal(decimal.NewFromFloat(108.5)) {
		t.Fatalf("recenter must keep width centered on price, got [%s, %s]", nl, nu)
	}
	if !nu.Sub(nl).Equal(upper.Sub(lower)) {
		t.Fatalf("width must be preserved")
	}
}

func TestRecenterStopPreservesDistance(t *testing.T) {
	// long side: stop sat 2 below the old lower bound
	oldLower, oldUpper := decimal.NewFromFloat(100), decimal.NewFromFloat(110)
	oldStop := decimal.NewFromFloat(98)
	nl, nu := decimal.NewFromFloat(98.5), decimal.NewFromFloat(108.5)
	ns := recenterStop("NEUTRAL", oldLower, oldUpper, nl, nu, oldStop)
	if !ns.Equal(decimal.NewFromFloat(96.5)) {
		t.Fatalf("long-side stop must move to newLower-2, got %s", ns)
	}
	// short side: stop sat 2 above the old upper bound
	ss := recenterStop("SHORT", oldLower, oldUpper, nl, nu, decimal.NewFromFloat(112))
	if !ss.Equal(decimal.NewFromFloat(110.5)) {
		t.Fatalf("short-side stop must move to newUpper+2, got %s", ss)
	}
}

func TestCandidateSpanPct(t *testing.T) {
	if s := candidateSpanPct(decimal.NewFromFloat(100), decimal.NewFromFloat(102.8)); s < 2.79 || s > 2.81 {
		t.Fatalf("span pct = %f, want 2.8", s)
	}
	if s := candidateSpanPct(decimal.Zero, decimal.NewFromFloat(110)); s != 0 {
		t.Fatalf("zero lower must yield 0, got %f", s)
	}
}
