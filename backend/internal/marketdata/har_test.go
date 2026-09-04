package marketdata

import (
	"math"
	"math/rand"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// harCandle builds a synthetic candle where only the close and timestamp
// matter: the HAR pipeline consumes close-to-close log returns.
func harCandle(ts int64, close float64) pionex.KlineCandle {
	return pionex.KlineCandle{
		Time:   ts,
		Open:   decimal.NewFromFloat(close),
		Close:  decimal.NewFromFloat(close),
		High:   decimal.NewFromFloat(close),
		Low:    decimal.NewFromFloat(close),
		Volume: decimal.NewFromFloat(10),
	}
}

// harVolCandles generates len(volPct)+1 daily candles whose log returns are
// normal with the per-day annualized volatilities given in volPct (in %).
func harVolCandles(seed int64, volPct []float64) []pionex.KlineCandle {
	rng := rand.New(rand.NewSource(seed))
	price := 100.0
	start := int64(1_700_000_000)
	candles := make([]pionex.KlineCandle, 0, len(volPct)+1)
	candles = append(candles, harCandle(start, price))
	for i, annualPct := range volPct {
		// annualized % → daily return fraction: σ_d = σ_a / √365.
		sigma := annualPct / 100 / math.Sqrt(365)
		price *= math.Exp(rng.NormFloat64() * sigma)
		candles = append(candles, harCandle(start+int64(i+1)*86_400, price))
	}
	return candles
}

// Alternating ±1% returns have a known RMS of 1% per day; annualization for
// 24/7 crypto markets must scale it by √365 and express the result in %.
func TestRealizedVolatility(t *testing.T) {
	returns := make([]float64, 100)
	for i := range returns {
		if i%2 == 0 {
			returns[i] = 0.01
		} else {
			returns[i] = -0.01
		}
	}
	want := 0.01 * math.Sqrt(365) * 100 // ≈ 19.105% annualized
	if got := RealizedVolatility(returns); math.Abs(got-want) > 1e-9 {
		t.Fatalf("RealizedVolatility = %.6f, want %.6f", got, want)
	}

	// General annualization: hourly-scale returns passed with 365·24
	// periods per year.
	hourly := make([]float64, 100)
	for i := range hourly {
		if i%2 == 0 {
			hourly[i] = 0.002
		} else {
			hourly[i] = -0.002
		}
	}
	wantHourly := 0.002 * math.Sqrt(365*24) * 100
	if got := RealizedVolatilityAnnualized(hourly, 365*24); math.Abs(got-wantHourly) > 1e-9 {
		t.Fatalf("RealizedVolatilityAnnualized = %.6f, want %.6f", got, wantHourly)
	}

	// Degenerate inputs must yield 0, never NaN.
	if got := RealizedVolatility([]float64{0.01}); got != 0 {
		t.Fatalf("single return must yield 0, got %.6f", got)
	}
	if got := RealizedVolatility(nil); got != 0 {
		t.Fatalf("nil returns must yield 0, got %.6f", got)
	}
}

// Persistent volatility must be learnable (R² > 0.5); iid volatility must
// not (R² < 0.1); short series must be rejected outright.
func TestHARTrain(t *testing.T) {
	// AR(1) volatility around 30%: v[t] = 30 + 0.9·(v[t-1]-30) + ε.
	// Theoretical one-lag R² = 0.9² = 0.81, comfortably above the bar.
	rng := rand.New(rand.NewSource(7))
	persistent := make([]float64, 400)
	v := 30.0
	for i := range persistent {
		v = 30 + 0.9*(v-30) + rng.NormFloat64()*4.0
		if v < 1 {
			v = 1
		}
		persistent[i] = v
	}
	model := TrainHAR(persistent)
	if model == nil {
		t.Fatal("TrainHAR must fit a 400-point persistent series")
	}
	if model.RSquared < 0.5 {
		t.Fatalf("persistent series R² = %.3f, want > 0.5", model.RSquared)
	}
	// Volatility is persistent, so the betas must sum near the AR(1)
	// persistence coefficient rather than scattering around zero.
	betaSum := model.Coefficients.BetaDaily +
		model.Coefficients.BetaWeekly +
		model.Coefficients.BetaMonthly
	if betaSum < 0.5 || betaSum > 1.5 {
		t.Fatalf("beta sum = %.3f, want persistence in (0.5, 1.5)", betaSum)
	}
	if model.TrainedAt.IsZero() {
		t.Fatal("TrainedAt must be stamped")
	}

	// iid series: regressors carry no information, R² ≈ k/n ≈ 0.01.
	rng2 := rand.New(rand.NewSource(11))
	random := make([]float64, 400)
	for i := range random {
		random[i] = 30 + rng2.NormFloat64()*10
	}
	modelRandom := TrainHAR(random)
	if modelRandom == nil {
		t.Fatal("TrainHAR must fit a 400-point random series")
	}
	if modelRandom.RSquared >= 0.1 {
		t.Fatalf("random series R² = %.3f, want < 0.1", modelRandom.RSquared)
	}

	// Insufficient data (< 30 points) and degenerate series must return nil.
	for _, n := range []int{0, 5, 29} {
		if m := TrainHAR(make([]float64, n)); m != nil {
			t.Fatalf("TrainHAR(%d points) must return nil", n)
		}
	}
	constant := make([]float64, 100) // all-zero RV → singular normal equations
	if m := TrainHAR(constant); m != nil {
		t.Fatal("constant series must be rejected as singular")
	}

	// PredictNextVol is the plain linear combination of the coefficients.
	manual := &HARForecast{Coefficients: HARCoefficients{
		Intercept: 1, BetaDaily: 0.5, BetaWeekly: 0.3, BetaMonthly: 0.2,
	}}
	if got := manual.PredictNextVol(10, 20, 30); math.Abs(got-18) > 1e-9 {
		t.Fatalf("PredictNextVol = %.6f, want 18", got)
	}
}

// 200 daily candles with a known alternating volatility regime must produce
// a sane model and a next-day forecast inside a reasonable band, and a
// high-vol history must forecast above a low-vol history.
func TestForecastVolatilityFromCandles(t *testing.T) {
	// Regime switching every 40 days between 25% and 80% annualized —
	// exactly the pattern the monthly HAR lag is built to capture.
	vols := make([]float64, 200)
	for i := range vols {
		if (i/40)%2 == 0 {
			vols[i] = 25
		} else {
			vols[i] = 80
		}
	}
	model, forecast, err := ForecastVolatilityFromCandles(harVolCandles(3, vols))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model == nil {
		t.Fatal("model must be returned")
	}
	if forecast < 5 || forecast > 200 {
		t.Fatalf("forecast %.2f%% annualized is outside the sane (5, 200) band", forecast)
	}
	if model.RSquared <= 0.05 {
		t.Fatalf("regime-switching series must show signal, R² = %.3f", model.RSquared)
	}

	// Constant regimes: the high-vol forecast must dominate the low-vol one.
	high := make([]float64, 200)
	low := make([]float64, 200)
	for i := range high {
		high[i] = 80
		low[i] = 15
	}
	_, forecastHigh, err := ForecastVolatilityFromCandles(harVolCandles(5, high))
	if err != nil {
		t.Fatalf("high-vol candles: %v", err)
	}
	_, forecastLow, err := ForecastVolatilityFromCandles(harVolCandles(6, low))
	if err != nil {
		t.Fatalf("low-vol candles: %v", err)
	}
	if forecastHigh <= forecastLow {
		t.Fatalf("high-vol forecast %.2f must exceed low-vol forecast %.2f", forecastHigh, forecastLow)
	}

	// Too few candles for even the 30-point HAR minimum must error.
	if _, _, err := ForecastVolatilityFromCandles(harVolCandles(1, make([]float64, 20))); err == nil {
		t.Fatal("20 daily candles must produce an error")
	}
	if _, _, err := ForecastVolatilityFromCandles(nil); err == nil {
		t.Fatal("nil candles must produce an error")
	}
}

// Low volatility: daily vol < 1% → the range floor applies and max leverage
// is allowed; the step must still cover 3× the fee (v2.0.75: and never dip
// under the shared 0.25% density floor).
func TestGridGeometryLowVol(t *testing.T) {
	const feeBps = 5.0
	g := ComputeGridGeometry(10, 0.42, feeBps, 0) // 10% annualized ≈ 0.52% daily
	if g.RangePct != 3.0 {
		t.Fatalf("range must clamp to the 3%% floor, got %.2f", g.RangePct)
	}
	if g.Leverage != 4 {
		t.Fatalf("sub-3%% daily vol allows 4x leverage (v2.0.38 ladder), got %d", g.Leverage)
	}
	if g.GridCount != 12 { // 3% / 0.25% shared density floor
		t.Fatalf("grid count = %d, want 12", g.GridCount)
	}
	if math.Abs(g.StepPct-0.25) > 1e-9 {
		t.Fatalf("step = %.4f%%, want 0.25%%", g.StepPct)
	}
	if g.StepPct < 3*feeBps/100-1e-9 {
		t.Fatalf("step %.4f%% must cover 3x fee (0.15%%)", g.StepPct)
	}
	if g.StopPct != 1.5 {
		t.Fatalf("stop = %.2f, want 1.5 (half the range)", g.StopPct)
	}
	if g.Confidence != 0.42 {
		t.Fatalf("confidence = %.2f, want the HAR R² verbatim", g.Confidence)
	}
}

// High volatility: 100% annualized ≈ 5.23% daily → wide range, no leverage,
// stop at half the range.
func TestGridGeometryHighVol(t *testing.T) {
	g := ComputeGridGeometry(100, 0.3, 5, 0)
	wantRange := 100 / math.Sqrt(365) * 2.5 // ≈ 13.09%, inside [3, 25]
	if math.Abs(g.RangePct-wantRange) > 1e-9 {
		t.Fatalf("range = %.4f, want %.4f (2.5 × daily vol)", g.RangePct, wantRange)
	}
	if g.Leverage != 3 {
		t.Fatalf("5.2%% daily vol must be 3x (v2.0.38 ladder), got %d", g.Leverage)
	}
	if math.Abs(g.StopPct-wantRange*0.5) > 1e-9 {
		t.Fatalf("stop = %.4f, want %.4f", g.StopPct, wantRange*0.5)
	}
	if g.RangePct <= ComputeGridGeometry(10, 0.3, 5, 0).RangePct {
		t.Fatal("high-vol range must be wider than the clamped low-vol range")
	}
	if g.GridCount < 6 || g.GridCount > 500 {
		t.Fatalf("grid count %d outside [6, 500]", g.GridCount)
	}
}

// Leverage bands (v2.0.38 ladder): 4x below 3% daily, 3x in between,
// 2x above 10% daily.
func TestGridGeometryLeverageBands(t *testing.T) {
	cases := []struct {
		annualVolPct float64
		wantLeverage int
	}{
		{10, 4},  // 0.52% daily
		{35, 4},  // 1.83% daily
		{100, 3}, // 5.23% daily
		{214, 2}, // 11.2% daily
	}
	for _, tc := range cases {
		g := ComputeGridGeometry(tc.annualVolPct, 0.3, 5, 0)
		if g.Leverage != tc.wantLeverage {
			t.Fatalf("%.0f%% annualized (%.2f%% daily): leverage = %d, want %d",
				tc.annualVolPct, tc.annualVolPct/math.Sqrt(365), g.Leverage, tc.wantLeverage)
		}
	}
}

// Across fees and volatility regimes every geometry must keep its step
// above 3× the fee (and above the shared 0.25% density floor) and its
// count/range inside the clamps.
func TestGridGeometryStepCoversFees(t *testing.T) {
	for _, feeBps := range []float64{2, 5, 10} {
		minStep := 3 * feeBps / 100
		if minStep < GridStepFloorPct {
			minStep = GridStepFloorPct
		}
		for _, annualVolPct := range []float64{5, 20, 45, 80, 150, 400} {
			g := ComputeGridGeometry(annualVolPct, 0.3, feeBps, 0)
			if g.StepPct+1e-9 < minStep {
				t.Fatalf("%.0f%% vol, %.0f bps fee: step %.4f%% below minimum %.4f%%",
					annualVolPct, feeBps, g.StepPct, minStep)
			}
			if g.GridCount < 6 || g.GridCount > 500 {
				t.Fatalf("%.0f%% vol, %.0f bps fee: grid count %d outside [6, 500]",
					annualVolPct, feeBps, g.GridCount)
			}
			if g.RangePct < 3-1e-9 || g.RangePct > 25+1e-9 {
				t.Fatalf("%.0f%% vol, %.0f bps fee: range %.4f outside [3, 25]",
					annualVolPct, feeBps, g.RangePct)
			}
		}
	}
}

func TestComputeGridGeometryBudgetCap(t *testing.T) {
	// BANK-кейс (prod #304): σ=214%/год → daily 11.2% → 2x under the
	// v2.0.38 leverage ladder. С бюджетом $200 и этажом $8/уровень
	// (v2.0.75, был $5): maxByBudget = 200×2/8 = 50 — каждый уровень
	// остаётся бирже-исполнимым.
	g := ComputeGridGeometry(214, 0.12, 7, 200)
	if g.Leverage != 2 {
		t.Fatalf("σ=214%% must be 2x (v2.0.38 ladder), got %d", g.Leverage)
	}
	if g.GridCount != 50 {
		t.Fatalf("budget cap must limit to 50 levels (200×2/8), got %d", g.GridCount)
	}
	if g.StepPct < 0.3 {
		t.Fatalf("step must stay wide enough, got %.3f", g.StepPct)
	}

	// Средняя вола: 56%/год → daily 2.93% → 4x, range 7.32% → 29 уровней
	// при шаге 0.25% — бюджетный кап 200×4/8 = 100 не связывающий.
	g = ComputeGridGeometry(56, 0.11, 7, 200)
	if g.Leverage != 4 {
		t.Fatalf("σ=56%% must be 4x (v2.0.38 ladder), got %d", g.Leverage)
	}
	if g.GridCount != 29 {
		t.Fatalf("mid vol must follow the step count 29, got %d", g.GridCount)
	}

	// Высокая вола + большой бюджет: 25% диапазон / 0.25% шаг = 100 уровней
	// (бюджетный кап 1000×2/8 = 250 его не касается).
	g = ComputeGridGeometry(214, 0.12, 7, 1000)
	if g.GridCount != 100 {
		t.Fatalf("high vol with big budget stays at 100 levels, got %d", g.GridCount)
	}

	// Крошечный бюджет не должен ронять уровни ниже структурного пола 6
	// (20×2/8 = 5 → клэмп вверх).
	g = ComputeGridGeometry(214, 0.12, 7, 20)
	if g.GridCount < 6 {
		t.Fatalf("structural floor of 6 levels must hold, got %d", g.GridCount)
	}
}
