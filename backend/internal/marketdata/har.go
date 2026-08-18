package marketdata

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// HAR (Heterogeneous Autoregressive) realized-volatility model, Corsi 2009.
//
// The model exploits the well-documented volatility persistence of crypto
// markets: tomorrow's realized volatility is explained by yesterday's (daily
// trader horizon), last week's and last month's:
//
//	RV_t = c + β_D·RV_{t-1} + β_W·mean(RV_{t-2..t-6}) + β_M·mean(RV_{t-7..t-22})
//
// RV (Realized Volatility) is the root of the sum of squared log returns over
// the period. Daily volatility persistence gives R² of 0.3-0.5 versus ~0 for
// returns themselves — which is exactly why grids are sized off volatility,
// not direction.
//
// Unit convention for every public helper here: per-day realized volatility,
// annualized and expressed in percent (45.2 = 45.2% annualized).

const (
	// harMinPoints is the minimum series length accepted by TrainHAR: 22
	// points are consumed by the monthly lag, the remainder must still leave
	// enough regression observations for a stable OLS fit.
	harMinPoints = 30
	// tradingDaysPerYear annualizes daily volatility for 24/7 crypto markets
	// (variance scales linearly with time: σ_annual = σ_daily·√365).
	tradingDaysPerYear = 365.0
)

// HARCoefficients are the fitted OLS coefficients of the HAR regression.
type HARCoefficients struct {
	Intercept   float64 // c
	BetaDaily   float64 // β_D — weight of yesterday's volatility
	BetaWeekly  float64 // β_W — weight of the trailing 5-day average
	BetaMonthly float64 // β_M — weight of the trailing 16-day average
}

// HARForecast is a fitted HAR model ready to produce one-step forecasts.
type HARForecast struct {
	Coefficients HARCoefficients
	RSquared     float64 // in-sample fit quality, usable as forecast confidence
	TrainedAt    time.Time
}

// RealizedVolatility computes realized volatility from per-period log
// returns and annualizes it for daily data (365 periods per year — crypto
// trades around the clock):
//
//	RV = sqrt(Σ r_i² / n) · sqrt(365) · 100
//
// The result is annualized volatility in percent (e.g. 45.2 = 45.2% a year).
// Returns 0 for fewer than 2 returns.
func RealizedVolatility(returns []float64) float64 {
	return RealizedVolatilityAnnualized(returns, tradingDaysPerYear)
}

// RealizedVolatilityAnnualized is the general form of RealizedVolatility:
// periodsPerYear is the number of return observations per year for sub-daily
// data (e.g. 365·24 for hourly candles).
//
// Annualization is invariant to how a day is chopped up:
// sqrt(Σ r²/n)·sqrt(365·k) = sqrt(Σ r²)·sqrt(365), so one day of intraday
// returns fed with periodsPerYear = 365·rows_per_day reproduces the daily
// figure exactly.
func RealizedVolatilityAnnualized(returns []float64, periodsPerYear float64) float64 {
	if len(returns) < 2 || periodsPerYear <= 0 {
		return 0
	}
	sumSq := 0.0
	for _, r := range returns {
		sumSq += r * r
	}
	rv := math.Sqrt(sumSq / float64(len(returns)))
	return rv * math.Sqrt(periodsPerYear) * 100
}

// harDailyRV converts one day's summed squared returns into an annualized
// daily volatility percentage. It uses the sum (not the mean) form so a day
// holding a single close-to-close return still yields |r|·sqrt(365)·100.
func harDailyRV(sumSq float64) float64 {
	return math.Sqrt(sumSq) * math.Sqrt(tradingDaysPerYear) * 100
}

// harRegressors builds the HAR predictor triple for target index t:
//
//	daily   = RV[t-1]              (yesterday)
//	weekly  = mean(RV[t-6..t-2])   (trailing 5 days, skipping yesterday)
//	monthly = mean(RV[t-22..t-7])  (trailing 16 days, skipping the week)
//
// t may equal len(vol) to build the regressors that forecast the first
// unseen period after the series ends.
func harRegressors(vol []float64, t int) (daily, weekly, monthly float64, ok bool) {
	if t < 22 || t > len(vol) {
		return 0, 0, 0, false
	}
	// vol[t-6:t-1] covers indices t-6..t-2 (5 values);
	// vol[t-22:t-6] covers indices t-22..t-7 (16 values).
	return vol[t-1], mean(vol[t-6 : t-1]), mean(vol[t-22 : t-6]), true
}

// LatestHARRegressors extracts the predictor triple forecasting the period
// immediately after the end of the series.
func LatestHARRegressors(volSeries []float64) (daily, weekly, monthly float64, ok bool) {
	return harRegressors(volSeries, len(volSeries))
}

// TrainHAR fits the HAR model by ordinary least squares:
//
//	Y_t = RV[t],  X_t = [1, RV[t-1], mean(RV[t-2..t-6]), mean(RV[t-7..t-22])]
//
// The normal equations (X'X)·β = X'Y are solved with Gauss-Jordan elimination
// (see solveLinearSystem) — no external linear-algebra dependency. R² is
// computed as 1 - SS_res/SS_tot.
//
// Returns nil when there is insufficient data (< 30 points), too few
// regression rows, or the normal equations are singular (e.g. a constant
// volatility series).
func TrainHAR(volSeries []float64) *HARForecast {
	if len(volSeries) < harMinPoints {
		return nil
	}

	// Assemble one regression row per target t >= 22.
	rows := make([][]float64, 0, len(volSeries)-22)
	targets := make([]float64, 0, len(volSeries)-22)
	for t := 22; t < len(volSeries); t++ {
		daily, weekly, monthly, ok := harRegressors(volSeries, t)
		if !ok {
			continue
		}
		rows = append(rows, []float64{1, daily, weekly, monthly})
		targets = append(targets, volSeries[t])
	}
	// OLS needs strictly more observations than the 4 estimated parameters.
	if len(targets) < 5 {
		return nil
	}

	// Normal equations: A = X'X (4×4 symmetric), b = X'Y.
	const k = 4
	a := make([][]float64, k)
	for i := range a {
		a[i] = make([]float64, k)
	}
	b := make([]float64, k)
	for obs, x := range rows {
		y := targets[obs]
		for i := 0; i < k; i++ {
			b[i] += x[i] * y
			for j := 0; j < k; j++ {
				a[i][j] += x[i] * x[j]
			}
		}
	}

	beta, ok := solveLinearSystem(a, b)
	if !ok {
		return nil
	}

	// R² = 1 - SS_res/SS_tot: the share of RV variance the heterogeneous
	// lags explain. A constant series (SS_tot = 0) carries no information,
	// so R² is reported as 0 rather than NaN.
	yBar := mean(targets)
	ssRes, ssTot := 0.0, 0.0
	for obs, x := range rows {
		pred := beta[0]*x[0] + beta[1]*x[1] + beta[2]*x[2] + beta[3]*x[3]
		residual := targets[obs] - pred
		deviation := targets[obs] - yBar
		ssRes += residual * residual
		ssTot += deviation * deviation
	}
	r2 := 0.0
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	for _, v := range append(beta, r2) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	}

	return &HARForecast{
		Coefficients: HARCoefficients{
			Intercept:   beta[0],
			BetaDaily:   beta[1],
			BetaWeekly:  beta[2],
			BetaMonthly: beta[3],
		},
		RSquared:  r2,
		TrainedAt: time.Now(),
	}
}

// PredictNextVol produces the one-step-ahead forecast from explicit regressor
// values. Feed it the same units the model was trained on (annualized
// percent for the helpers in this file).
func (h *HARForecast) PredictNextVol(recentDaily, recentWeekly, recentMonthly float64) float64 {
	return h.Coefficients.Intercept +
		h.Coefficients.BetaDaily*recentDaily +
		h.Coefficients.BetaWeekly*recentWeekly +
		h.Coefficients.BetaMonthly*recentMonthly
}

// solveLinearSystem solves A·x = b via Gauss-Jordan elimination with partial
// pivoting. A and b are modified in place; after the reduction A is the
// identity and b holds the solution. Choosing the largest-magnitude pivot per
// column keeps the elimination numerically stable. Returns ok=false when the
// system is singular — for OLS that means collinear regressors or a
// degenerate series.
func solveLinearSystem(a [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	if n == 0 || len(a) != n {
		return nil, false
	}

	// Singularity threshold scaled by the largest matrix entry so the test
	// is unit-agnostic (volatilities arrive in percent, not in [0,1]).
	scale := 0.0
	for i := 0; i < n; i++ {
		if len(a[i]) != n {
			return nil, false
		}
		for j := 0; j < n; j++ {
			if v := math.Abs(a[i][j]); v > scale {
				scale = v
			}
		}
	}
	if scale == 0 {
		return nil, false
	}
	const eps = 1e-12

	for col := 0; col < n; col++ {
		// Partial pivoting: bring the largest remaining entry of the column
		// onto the diagonal so we never divide by a near-zero pivot.
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(a[pivot][col]) < eps*scale {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]

		// Normalize the pivot row, then eliminate the column from every
		// other row (full Jordan reduction).
		diag := a[col][col]
		for j := col; j < n; j++ {
			a[col][j] /= diag
		}
		b[col] /= diag
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			factor := a[r][col]
			if factor == 0 {
				continue
			}
			for j := col; j < n; j++ {
				a[r][j] -= factor * a[col][j]
			}
			b[r] -= factor * b[col]
		}
	}
	return b, true
}

// candleDayKey buckets a candle timestamp into a UTC day index. Pionex
// klines report unix seconds; the millisecond branch is defensive so an
// ms-based feed still buckets by calendar day.
func candleDayKey(ts int64) int64 {
	if ts > 1_000_000_000_000 {
		return ts / 86_400_000
	}
	return ts / 86_400
}

// ForecastVolatilityFromCandles is the end-to-end convenience pipeline:
//
//  1. log returns from consecutive closes,
//  2. per-day realized variance — squared returns bucketed by the UTC day of
//     the closing candle (intraday candles aggregate inside their day; daily
//     candles contribute one close-to-close return per day),
//  3. HAR training on the resulting daily RV series (annualized %),
//  4. one-step-ahead forecast from the latest regressors.
//
// Returns the fitted model, the next-day annualized volatility forecast in
// percent, and an error for insufficient or degenerate input.
func ForecastVolatilityFromCandles(candles []pionex.KlineCandle) (*HARForecast, float64, error) {
	series := ExtractSeries(candles)
	// 31 closes yield 30 daily returns — exactly the HAR minimum.
	if series.Len() < 31 {
		return nil, 0, fmt.Errorf("har: need at least 31 candles, got %d", series.Len())
	}

	// Bucket squared log returns by day, preserving chronological order via
	// a first-seen index; a final sort guards against unsorted feeds.
	type dayVariance struct {
		day   int64
		sumSq float64
	}
	var days []dayVariance
	dayIndex := make(map[int64]int)
	for i := 1; i < series.Len(); i++ {
		r := math.Log(series.Close[i] / series.Close[i-1])
		day := candleDayKey(series.Time[i])
		idx, seen := dayIndex[day]
		if !seen {
			idx = len(days)
			dayIndex[day] = idx
			days = append(days, dayVariance{day: day})
		}
		days[idx].sumSq += r * r
	}
	sort.Slice(days, func(i, j int) bool { return days[i].day < days[j].day })

	dailyVols := make([]float64, len(days))
	for i := range days {
		dailyVols[i] = harDailyRV(days[i].sumSq)
	}

	model := TrainHAR(dailyVols)
	if model == nil {
		return nil, 0, fmt.Errorf("har: training failed on %d daily observations", len(dailyVols))
	}

	daily, weekly, monthly, ok := LatestHARRegressors(dailyVols)
	if !ok {
		return nil, 0, fmt.Errorf("har: insufficient history for latest regressors")
	}
	forecast := model.PredictNextVol(daily, weekly, monthly)
	if math.IsNaN(forecast) || math.IsInf(forecast, 0) {
		return nil, 0, fmt.Errorf("har: forecast is not finite (%.4f)", forecast)
	}
	return model, forecast, nil
}
