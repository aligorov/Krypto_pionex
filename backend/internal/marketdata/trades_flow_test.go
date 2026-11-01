package marketdata

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func TestAnalyzeTakerFlow_Dumping(t *testing.T) {
	trades := []pionex.Trade{
		{Price: decimal.NewFromFloat(100.0), Size: decimal.NewFromFloat(100.0), Side: "SELL"}, // $10,000
		{Price: decimal.NewFromFloat(99.5), Size: decimal.NewFromFloat(100.0), Side: "SELL"},  // $9,950
		{Price: decimal.NewFromFloat(99.0), Size: decimal.NewFromFloat(20.0), Side: "BUY"},    // $1,980
	}

	metrics := AnalyzeTakerFlow(trades)
	if !metrics.IsAggressiveDumping {
		t.Fatalf("expected IsAggressiveDumping=true, got false")
	}
	if metrics.LargeSellCount != 2 {
		t.Errorf("expected 2 large sells, got %d", metrics.LargeSellCount)
	}
	if metrics.SellRatio < 0.85 {
		t.Errorf("expected SellRatio >= 0.85, got %.2f", metrics.SellRatio)
	}
}

func TestAnalyzeTakerFlow_Balanced(t *testing.T) {
	trades := []pionex.Trade{
		{Price: decimal.NewFromFloat(100.0), Size: decimal.NewFromFloat(50.0), Side: "SELL"}, // $5,000
		{Price: decimal.NewFromFloat(100.0), Size: decimal.NewFromFloat(50.0), Side: "BUY"},  // $5,000
	}

	metrics := AnalyzeTakerFlow(trades)
	if metrics.IsAggressiveDumping || metrics.IsAggressivePumping {
		t.Fatalf("expected balanced flow, got dumping=%v, pumping=%v", metrics.IsAggressiveDumping, metrics.IsAggressivePumping)
	}
	if metrics.BuyRatio != 0.5 {
		t.Errorf("expected BuyRatio=0.5, got %.2f", metrics.BuyRatio)
	}
}

func TestAnalyzeTakerFlow_WhaleMAD(t *testing.T) {
	// Simulated altcoin tape: 15 normal trades ~$30 (total ~$450),
	// followed by a sudden $2,500 sell print (whale dumping).
	// Under the old static thresholds ($5,000 and $15,000), this would be MISSED.
	// Under the MAD Modified Z-score, M_z > 3.5 flags WhaleSellCount=1, SellRatio > 80%,
	// triggering IsAggressiveDumping.
	trades := make([]pionex.Trade, 0, 16)
	for i := 0; i < 15; i++ {
		side := "BUY"
		if i%2 == 0 {
			side = "SELL"
		}
		trades = append(trades, pionex.Trade{
			Price: decimal.NewFromFloat(1.0),
			Size:  decimal.NewFromFloat(30.0),
			Side:  side,
		})
	}
	// Add whale sell
	trades = append(trades, pionex.Trade{
		Price: decimal.NewFromFloat(1.0),
		Size:  decimal.NewFromFloat(2500.0),
		Side:  "SELL",
	})

	metrics := AnalyzeTakerFlow(trades)
	if !metrics.IsAggressiveDumping {
		t.Fatalf("expected IsAggressiveDumping=true via Whale MAD, got false")
	}
	if metrics.WhaleSellCount != 1 {
		t.Errorf("expected WhaleSellCount=1, got %d", metrics.WhaleSellCount)
	}
	if metrics.MaxModifiedZScore < 3.5 {
		t.Errorf("expected MaxModifiedZScore >= 3.5, got %.2f", metrics.MaxModifiedZScore)
	}
}

func TestAnalyzeTakerFlow_ZeroMADProtection(t *testing.T) {
	// Tape where all trades are identical $20: MAD would mathematically be 0.
	// Safety floor must prevent division by zero or panic.
	trades := make([]pionex.Trade, 0, 10)
	for i := 0; i < 10; i++ {
		trades = append(trades, pionex.Trade{
			Price: decimal.NewFromFloat(2.0),
			Size:  decimal.NewFromFloat(10.0),
			Side:  "BUY",
		})
	}
	metrics := AnalyzeTakerFlow(trades)
	if metrics.MADNotionalUSDT <= 0 {
		t.Errorf("expected MADNotionalUSDT > 0 with floor, got %.4f", metrics.MADNotionalUSDT)
	}
	if metrics.MaxModifiedZScore != 0 {
		t.Errorf("expected MaxModifiedZScore = 0 for identical trades, got %.2f", metrics.MaxModifiedZScore)
	}
}
