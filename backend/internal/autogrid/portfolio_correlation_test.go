package autogrid

import (
	"math"
	"testing"
)

func TestPearsonCorrelation_Collinear(t *testing.T) {
	a := []float64{0.01, 0.02, -0.01, 0.03, -0.02, 0.015, -0.005, 0.025, -0.015, 0.01}
	b := make([]float64, len(a))
	for i, v := range a {
		b[i] = v * 2.0 // perfectly collinear
	}

	rho := PearsonCorrelation(a, b)
	if math.Abs(rho-1.0) > 1e-6 {
		t.Fatalf("expected rho=1.0 for collinear series, got %f", rho)
	}
}

func TestPearsonCorrelation_Inverse(t *testing.T) {
	a := []float64{0.01, 0.02, -0.01, 0.03, -0.02, 0.015, -0.005, 0.025, -0.015, 0.01}
	b := make([]float64, len(a))
	for i, v := range a {
		b[i] = -v * 1.5 // perfectly inversely correlated
	}

	rho := PearsonCorrelation(a, b)
	if math.Abs(rho-(-1.0)) > 1e-6 {
		t.Fatalf("expected rho=-1.0 for inverse series, got %f", rho)
	}
}

func TestPearsonCorrelation_ShortSeries(t *testing.T) {
	a := []float64{0.01, 0.02, 0.03}
	b := []float64{0.01, 0.02, 0.03}
	rho := PearsonCorrelation(a, b)
	if rho != 0 {
		t.Fatalf("expected rho=0 for n < 10, got %f", rho)
	}
}
