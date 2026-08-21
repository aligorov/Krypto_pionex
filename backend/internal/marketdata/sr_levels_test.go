package marketdata

import (
	"math"
	"testing"
)

func TestSRLevelsDetection(t *testing.T) {
	// Generate a series that oscillates between 90 and 110 multiple times, ending at 100
	n := 80
	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)

	for i := 0; i < n; i++ {
		// Sine wave oscillating between 90 and 110
		mid := 100.0
		val := mid + 10.0*math.Sin(float64(i)*math.Pi/10.0)
		closes[i] = val
		highs[i] = val + 1.0
		lows[i] = val - 1.0
	}
	closes[n-1] = 100.0 // end at mid

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

	sr := DetectSRLevels(s, 80, 2.0)
	if len(sr.Supports) == 0 {
		t.Fatalf("expected at least 1 support shelf, got 0")
	}
	if len(sr.Resistances) == 0 {
		t.Fatalf("expected at least 1 resistance shelf, got 0")
	}

	if sr.NearestSupport <= 0 || sr.NearestSupport > 95.0 {
		t.Fatalf("expected nearest support ~89-92, got %.2f", sr.NearestSupport)
	}
	if sr.NearestResist <= 0 || sr.NearestResist < 105.0 {
		t.Fatalf("expected nearest resist ~108-111, got %.2f", sr.NearestResist)
	}
	if sr.SupportStrength < 0.5 {
		t.Fatalf("expected strong support (multi-touch), got strength=%.2f", sr.SupportStrength)
	}
	if sr.ResistStrength < 0.5 {
		t.Fatalf("expected strong resistance (multi-touch), got strength=%.2f", sr.ResistStrength)
	}
}

func TestSRLevelsShortSeries(t *testing.T) {
	s := seriesFromCloses([]float64{100, 101, 102})
	sr := DetectSRLevels(s, 5, 1.0)
	if len(sr.Supports) != 0 || len(sr.Resistances) != 0 {
		t.Fatalf("expected empty S/R for short series")
	}
}
