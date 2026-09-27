package marketdata

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func TestProfileOrderBook_NormalBook(t *testing.T) {
	currentPrice := decimal.NewFromFloat(100.0)
	botNotional := 1000.0 // $1,000 bot
	minCushion := 50.0    // wants $50,000 depth on BOTH sides (v2.0.119)

	bids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(99.5), Amount: decimal.NewFromFloat(200.0)}, // $19,900
		{Price: decimal.NewFromFloat(99.0), Amount: decimal.NewFromFloat(300.0)}, // $29,700
		{Price: decimal.NewFromFloat(98.5), Amount: decimal.NewFromFloat(200.0)}, // $19,700
		// Total bids in 2% = 19900 + 29700 + 19700 = $69,300
	}

	asks := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(100.5), Amount: decimal.NewFromFloat(200.0)}, // $20,100
		{Price: decimal.NewFromFloat(101.0), Amount: decimal.NewFromFloat(200.0)}, // $20,200
		{Price: decimal.NewFromFloat(101.5), Amount: decimal.NewFromFloat(270.0)}, // $27,405
		// Total asks in 2% = 20100 + 20200 + 27405 = $67,705 (67.7x, bid-heavy)
	}

	profile := ProfileOrderBook(bids, asks, currentPrice, botNotional, minCushion)

	if profile.IsThinBook {
		t.Fatalf("expected IsThinBook=false (bid $69.3k / ask $67.7k > $50k), got true")
	}
	if profile.BidCushionRatio < 60.0 {
		t.Errorf("expected BidCushionRatio >= 60, got %.2f", profile.BidCushionRatio)
	}
	if profile.AskCushionRatio < 60.0 {
		t.Errorf("expected AskCushionRatio >= 60 (v2.0.119 symmetric), got %.2f", profile.AskCushionRatio)
	}
	if profile.ImbalanceRatio <= 0.5 {
		t.Errorf("expected ImbalanceRatio > 0.5 (bid heavy), got %.2f", profile.ImbalanceRatio)
	}
}

// TestProfileOrderBook_AskThinBookIsThin pins the v2.0.119 fix: a book with
// deep BIDS and an empty ASK side must be thin. The old verdict read only
// bidCushion, so a NEUTRAL/SHORT close buying into the empty side passed
// the gate.
func TestProfileOrderBook_AskThinBookIsThin(t *testing.T) {
	currentPrice := decimal.NewFromFloat(100.0)
	botNotional := 1000.0
	minCushion := 50.0

	// Deep bids ($69,300 = 69x), asks carry only $8,050 (8x < 50x).
	bids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(99.5), Amount: decimal.NewFromFloat(200.0)},
		{Price: decimal.NewFromFloat(99.0), Amount: decimal.NewFromFloat(300.0)},
		{Price: decimal.NewFromFloat(98.5), Amount: decimal.NewFromFloat(200.0)},
	}
	asks := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(100.5), Amount: decimal.NewFromFloat(80.0)}, // $8,040
	}

	profile := ProfileOrderBook(bids, asks, currentPrice, botNotional, minCushion)
	if !profile.IsThinBook {
		t.Fatalf("expected IsThinBook=true for ask-thin book (8x < 50x), got false — the v2.0.114 asymmetry")
	}
}

func TestProfileOrderBook_ThinBook(t *testing.T) {
	currentPrice := decimal.NewFromFloat(100.0)
	botNotional := 1000.0
	minCushion := 50.0

	// Thin order book with only $5,000 bid depth
	bids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(99.5), Amount: decimal.NewFromFloat(25.0)}, // $2,487.5
		{Price: decimal.NewFromFloat(99.0), Amount: decimal.NewFromFloat(25.0)}, // $2,475
	}
	asks := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(100.5), Amount: decimal.NewFromFloat(200.0)},
	}

	profile := ProfileOrderBook(bids, asks, currentPrice, botNotional, minCushion)

	if !profile.IsThinBook {
		t.Fatalf("expected IsThinBook=true for $5k depth vs $50k required, got false")
	}
}

func TestFindMajorWall(t *testing.T) {
	currentPrice := decimal.NewFromFloat(100.0)

	levels := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(99.5), Amount: decimal.NewFromFloat(10.0)},  // $995 (normal)
		{Price: decimal.NewFromFloat(99.0), Amount: decimal.NewFromFloat(10.0)},  // $990 (normal)
		{Price: decimal.NewFromFloat(98.5), Amount: decimal.NewFromFloat(600.0)}, // $59,100 (MAJOR WALL)
		{Price: decimal.NewFromFloat(98.0), Amount: decimal.NewFromFloat(10.0)},  // $980 (normal)
	}

	wallPrice, wallNotional, found := FindMajorWall(levels, currentPrice, true, 0.05)
	if !found {
		t.Fatalf("expected major wall to be found, got false")
	}
	if !wallPrice.Equal(decimal.NewFromFloat(98.5)) {
		t.Errorf("expected wallPrice=98.5, got %s", wallPrice.String())
	}
	if wallNotional < 50000 {
		t.Errorf("expected wallNotional >= 50000, got %.0f", wallNotional)
	}
}
