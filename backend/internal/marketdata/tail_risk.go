package marketdata

import "math"

// CornishFisherVaR computes the 99% parametric Value-at-Risk adjusted for skewness
// and excess kurtosis (Boudt, Peterson & Croux, 2008 / Cornish-Fisher expansion).
// Returns (VaR99Pct, skewness, excessKurtosis).
// Horizon is 1 day (scaled by sqrt(periodsPerDay)).
func CornishFisherVaR(closes []float64, periodsPerDay float64) (float64, float64, float64) {
	n := len(closes)
	if n < 20 {
		return 0, 0, 0
	}
	returns := make([]float64, 0, n-1)
	sum := 0.0
	for i := 1; i < n; i++ {
		prev := closes[i-1]
		curr := closes[i]
		if prev <= 0 || curr <= 0 {
			continue
		}
		r := math.Log(curr / prev)
		returns = append(returns, r)
		sum += r
	}
	nR := len(returns)
	if nR < 19 {
		return 0, 0, 0
	}
	mean := sum / float64(nR)

	m2, m3, m4 := 0.0, 0.0, 0.0
	for _, r := range returns {
		diff := r - mean
		d2 := diff * diff
		m2 += d2
		m3 += d2 * diff
		m4 += d2 * d2
	}
	variance := m2 / float64(nR)
	if variance <= 0 {
		return 0, 0, 0
	}
	stdev := math.Sqrt(variance)

	skew := (m3 / float64(nR)) / (variance * stdev)
	kurtosis := (m4/float64(nR))/(variance*variance) - 3.0 // Excess kurtosis

	// Standard 99% 1-tailed quantile for normal distribution
	z := 2.32635

	// Cornish-Fisher expansion:
	// z_cf = z + (z^2 - 1)/6 * S + (z^3 - 3z)/24 * K - (2z^3 - 5z)/36 * S^2
	zCF := z + (z*z-1.0)/6.0*skew + (z*z*z-3.0*z)/24.0*kurtosis - (2.0*z*z*z-5.0*z)/36.0*skew*skew

	// Clamp expansion quantile to sane bounds [1.5, 6.0] to prevent divergence on noisy crypto micro-samples
	zCF = clamp(zCF, 1.5, 6.0)

	// Scale to 1-day horizon
	horizonFactor := math.Sqrt(math.Max(periodsPerDay, 1.0))
	dailyVaR := -(mean - zCF*stdev) * horizonFactor * 100.0

	return dailyVaR, skew, kurtosis
}
