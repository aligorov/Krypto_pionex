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
