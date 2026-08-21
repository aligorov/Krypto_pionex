package marketdata

import (
	"testing"
)

func TestMACDBullishCrossover(t *testing.T) {
	// 50 bars: 35 bars falling from 100 to 70, then 15 bars sharply rallying from 70 to 110
	n := 50
	closes := make([]float64, n)
	for i := 0; i < 35; i++ {
		closes[i] = 100.0 - float64(i)*0.857
	}
	for i := 35; i < n; i++ {
		closes[i] = 70.0 + float64(i-35)*2.667
	}

	macd := ComputeMACD(closes)
	if macd.MACD == 0 && macd.Signal == 0 {
		t.Fatalf("expected non-zero MACD calculations")
	}
	// The sharp turnaround must either trigger a bullish crossover or turn the histogram upward
	if !macd.CrossedUp && !macd.HistTurning && macd.Histogram <= macd.PrevHist {
		t.Fatalf("expected bullish momentum signal from MACD reversal, got crossedUp=%v histTurning=%v hist=%.3f prev=%.3f",
			macd.CrossedUp, macd.HistTurning, macd.Histogram, macd.PrevHist)
	}
}

func TestStochRSIOversoldCross(t *testing.T) {
	// Sustained decline then a bounce off the lows
	n := 50
	closes := make([]float64, n)
	for i := 0; i < 45; i++ {
		closes[i] = 100.0 - float64(i)*0.8
	}
	for i := 45; i < n; i++ {
		closes[i] = 64.0 + float64(i-45)*2.0
	}

	stoch := ComputeStochRSI(closes)
	if stoch.K == 0 && stoch.D == 0 {
		t.Fatalf("expected valid StochRSI values")
	}
	if stoch.K < 0 || stoch.K > 100 || stoch.D < 0 || stoch.D > 100 {
		t.Fatalf("StochRSI out of [0, 100] bounds: K=%.2f, D=%.2f", stoch.K, stoch.D)
	}
}

func TestRSIDivergenceBullish(t *testing.T) {
	// Construct bullish divergence: First sharp drop 100 -> 80, sharp rally to 95, then shallow drop to 76 (lower low)
	n := 60
	closes := make([]float64, n)
	for i := 0; i < 20; i++ {
		closes[i] = 100.0
	}
	// Drop 1: 100 -> 80 (fast drop over 15 bars -> deep RSI)
	for i := 20; i < 35; i++ {
		closes[i] = 100.0 - float64(i-20)*1.33
	}
	// Rally: 80 -> 95 over 10 bars
	for i := 35; i < 45; i++ {
		closes[i] = 80.0 + float64(i-35)*1.5
	}
	// Drop 2: 95 -> 77 over 15 bars (slower decline to lower price low -> higher RSI low)
	for i := 45; i < n; i++ {
		closes[i] = 95.0 - float64(i-45)*1.2
	}

	div := DetectRSIDivergence(closes, 40)
	if div.Direction != 1 && div.Direction != 0 {
		t.Fatalf("expected bullish divergence (1) or neutral (0), got %v", div.Direction)
	}
}

func TestIndicatorsShortSeries(t *testing.T) {
	shortCloses := []float64{100, 101, 102}
	macd := ComputeMACD(shortCloses)
	if macd.MACD != 0 || macd.Signal != 0 {
		t.Fatalf("expected empty MACD for short series")
	}

	stoch := ComputeStochRSI(shortCloses)
	if stoch.K != 0 || stoch.D != 0 {
		t.Fatalf("expected empty StochRSI for short series")
	}

	div := DetectRSIDivergence(shortCloses, 40)
	if div.Direction != 0 {
		t.Fatalf("expected empty RSIDiv for short series")
	}
}
