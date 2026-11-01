package marketdata

import (
	"math"
	"testing"
)

func TestCornishFisherVaR_ShortSeries(t *testing.T) {
	var99, skew, kurt := CornishFisherVaR([]float64{100, 101, 102}, 96)
	if var99 != 0 || skew != 0 || kurt != 0 {
		t.Fatalf("expected 0 for series < 20 candles")
	}
}

func TestCornishFisherVaR_NormalSeries(t *testing.T) {
	// 50 candles with modest 0.2% random variation
	closes := make([]float64, 50)
	closes[0] = 100.0
	for i := 1; i < 50; i++ {
		step := 0.002 * math.Sin(float64(i))
		closes[i] = closes[i-1] * (1.0 + step)
	}

	var99, _, _ := CornishFisherVaR(closes, 96)
	if var99 <= 0 || var99 > 20.0 {
		t.Fatalf("expected reasonable 1-day VaR99 for calm series, got %.2f%%", var99)
	}
}

func TestCornishFisherVaR_FatTailRejection(t *testing.T) {
	// Construct a severe flash-crash series with fat negative tail:
	// 40 candles quiet, then 3 violent drops (-8% each)
	closes := make([]float64, 50)
	closes[0] = 100.0
	for i := 1; i < 40; i++ {
		closes[i] = closes[i-1] * (1.0 + 0.001*math.Sin(float64(i)))
	}
	for i := 40; i < 44; i++ {
		closes[i] = closes[i-1] * 0.90 // -10% candles
	}
	for i := 44; i < 50; i++ {
		closes[i] = closes[i-1]
	}

	var99, skew, kurt := CornishFisherVaR(closes, 96)
	if var99 < 25.0 {
		t.Errorf("expected extreme VaR99 > 25%% for flash crash series, got %.2f%% (skew=%.2f, kurt=%.2f)", var99, skew, kurt)
	}
	if kurt < 0 {
		t.Errorf("expected positive excess kurtosis for heavy tails, got %.2f", kurt)
	}
}
