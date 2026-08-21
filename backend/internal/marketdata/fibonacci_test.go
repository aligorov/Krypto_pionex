package marketdata

import (
	"math"
	"testing"
)

func TestFibonacciUpswing(t *testing.T) {
	// Upswing: 100 to 200 over 40 bars, then pullback to 135 (in 0.618-0.786 golden pocket)
	n := 50
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)

	// Low at bar 0
	for i := 0; i < 35; i++ {
		p := 100.0 + float64(i)*2.857 // reaches 200 at i=35
		closes[i] = p
		highs[i] = p * 1.002
		lows[i] = p * 0.998
	}
	// Pullback from bar 35 to 49
	for i := 35; i < n; i++ {
		p := 200.0 - float64(i-35)*4.5 // drops to ~137 at i=49
		closes[i] = p
		highs[i] = p * 1.002
		lows[i] = p * 0.998
	}

	s := &Series{
		Time:   make([]int64, n),
		Open:   closes,
		High:   highs,
		Low:    lows,
		Close:  closes,
		Volume: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		s.Time[i] = int64(i) * 900
		s.Volume[i] = 100
	}

	fib := ComputeFibonacciRetracement(s, 50)
	if fib.TrendDir != 1 {
		t.Fatalf("expected upswing (trendDir=1), got %d", fib.TrendDir)
	}
	if math.Abs(fib.SwingHigh-200.0*1.002) > 1.0 {
		t.Fatalf("expected swingHigh ~200.4, got %.2f", fib.SwingHigh)
	}
	if math.Abs(fib.SwingLow-100.0*0.998) > 1.0 {
		t.Fatalf("expected swingLow ~99.8, got %.2f", fib.SwingLow)
	}
	if !fib.InGoldenPocket {
		t.Fatalf("expected price %.2f to be in Golden Pocket (0.618-0.786), levels=[%.2f, %.2f]",
			fib.CurrentPrice, fib.Levels[3], fib.Levels[4])
	}
	if fib.SupportLevel <= 0 || fib.ResistanceLevel <= 0 {
		t.Fatalf("expected positive S/R levels, got sup=%.2f res=%.2f", fib.SupportLevel, fib.ResistanceLevel)
	}
}

func TestFibonacciDownswing(t *testing.T) {
	// Downswing: 200 down to 100 over 35 bars, then relief bounce to 170 (in golden pocket)
	n := 50
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)

	// High at bar 0
	for i := 0; i < 35; i++ {
		p := 200.0 - float64(i)*2.857 // drops to 100 at i=35
		closes[i] = p
		highs[i] = p * 1.002
		lows[i] = p * 0.998
	}
	// Relief bounce from bar 35 to 49
	for i := 35; i < n; i++ {
		p := 100.0 + float64(i-35)*5.0 // rises to ~170 at i=49
		closes[i] = p
		highs[i] = p * 1.002
		lows[i] = p * 0.998
	}

	s := &Series{
		Time:   make([]int64, n),
		Open:   closes,
		High:   highs,
		Low:    lows,
		Close:  closes,
		Volume: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		s.Time[i] = int64(i) * 900
		s.Volume[i] = 100
	}

	fib := ComputeFibonacciRetracement(s, 50)
	if fib.TrendDir != -1 {
		t.Fatalf("expected downswing (trendDir=-1), got %d", fib.TrendDir)
	}
	if !fib.InGoldenPocket {
		t.Fatalf("expected price %.2f to be in Golden Pocket (0.618-0.786), levels=[%.2f, %.2f]",
			fib.CurrentPrice, fib.Levels[3], fib.Levels[4])
	}
}

func TestFibonacciShortSeries(t *testing.T) {
	s := seriesFromCloses([]float64{100, 101, 102})
	fib := ComputeFibonacciRetracement(s, 10)
	if fib.TrendDir != 0 || fib.SwingHigh != 0 {
		t.Fatalf("expected empty/zero struct for short series")
	}
}
