package marketdata

// ML regime classifier infrastructure for the Smart Grid Engine.
//
// The pipeline: FeatureBuilder assembles a market-context feature vector from
// (a) the exchange-agnostic FeatureSource (funding / open interest / sentiment
// / macro events collected into PostgreSQL) and (b) the technical regime
// already computed by DetectRegime plus candle-derived inputs (HAR volatility
// forecast, Hurst exponent, price changes). A RegimeClassifier (ONNX seam) may
// turn that vector into a prediction; until a model artifact exists the
// rule-based fallback is the primary path, so the builder never hard-fails on
// a missing model or missing collector tables.

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Regime classes predicted by the classifier. Indices match the Python
// trainer (training/train_regime.py REGIME_CLASSES).
const (
	RegimeMLRange     = "RANGE"
	RegimeMLTrendUp   = "TREND_UP"
	RegimeMLTrendDown = "TREND_DOWN"
	RegimeMLCrash     = "CRASH"
)

// Prediction sources.
const (
	PredictionSourceML        = "ml"
	PredictionSourceRuleBased = "rule_based"
)

// Feature-source lookback windows.
const (
	fundingLookback   = 24 * time.Hour
	oiCompareLookback = 24 * time.Hour
	eventHorizon      = 24 * time.Hour
)

// Thresholds mirroring the Python feature definitions.
const (
	defaultFearGreed = 50.0 // neutral sentiment when no snapshot exists
	defaultHurst     = 0.5  // random-walk prior
)

// mlFeatureNames is the canonical ONNX input order. It MUST stay in sync with
// FEATURE_NAMES in training/train_regime.py and with MLFeatures.FeatureVector.
var mlFeatureNames = []string{
	"funding_avg",
	"funding_extreme",
	"oi_change_24h",
	"oi_rising",
	"realized_vol_daily",
	"har_forecast",
	"hurst_exponent",
	"adx14",
	"choppiness14",
	"bbw_percentile",
	"price_change_24h",
	"price_change_7d",
	"fear_greed",
	"high_impact_event",
}

// FeatureNames returns the canonical feature-vector order shared by the
// Python trainer and the (future) ONNX loader.
func FeatureNames() []string {
	out := make([]string, len(mlFeatureNames))
	copy(out, mlFeatureNames)
	return out
}

// MLFeatures is the input vector for the ONNX model.
type MLFeatures struct {
	FundingAvg       float64 `json:"fundingAvg"`
	FundingExtreme   float64 `json:"fundingExtreme"`   // 1.0 if |avg| > 0.001, else 0
	OIChange24h      float64 `json:"oiChange24h"`      // percent
	OIRising         float64 `json:"oiRising"`         // 1.0 / 0.0
	RealizedVolDaily float64 `json:"realizedVolDaily"` // ATR% proxy, percent
	HARForecast      float64 `json:"harForecast"`      // Corsi HAR daily vol forecast, percent
	HurstExponent    float64 `json:"hurstExponent"`    // DFA estimate, 0.1..1.0
	ADX14            float64 `json:"adx14"`
	Choppiness14     float64 `json:"choppiness14"`
	BBWPercentile    float64 `json:"bbwPercentile"`
	PriceChange24h   float64 `json:"priceChange24h"`  // percent
	PriceChange7d    float64 `json:"priceChange7d"`   // percent
	FearGreed        float64 `json:"fearGreed"`       // 0..100
	HighImpactEvent  float64 `json:"highImpactEvent"` // 1.0 if event within 24h
}

// FeatureVector returns the features in the canonical ONNX input order.
func (f *MLFeatures) FeatureVector() []float64 {
	if f == nil {
		return make([]float64, len(mlFeatureNames))
	}
	return []float64{
		f.FundingAvg,
		f.FundingExtreme,
		f.OIChange24h,
		f.OIRising,
		f.RealizedVolDaily,
		f.HARForecast,
		f.HurstExponent,
		f.ADX14,
		f.Choppiness14,
		f.BBWPercentile,
		f.PriceChange24h,
		f.PriceChange7d,
		f.FearGreed,
		f.HighImpactEvent,
	}
}

// RegimePrediction is the output from the classifier.
type RegimePrediction struct {
	Regime     string  `json:"regime"`     // RANGE, TREND_UP, TREND_DOWN, CRASH
	Confidence float64 `json:"confidence"` // 0-1
	Source     string  `json:"source"`     // "ml" or "rule_based"
}

// RegimeClassifier is the inference seam for the trained ONNX model. The
// production implementation will load the artifact from PostgreSQL (bytea,
// per the zero-ENV runtime policy), run it against MLFeatures.FeatureVector
// and map argmax to the regime classes. It is intentionally an interface:
// until the model exists every builder runs the rule-based fallback, and
// tests can stub a ready classifier without any ONNX runtime dependency.
type RegimeClassifier interface {
	// Ready reports whether a valid model artifact is loaded and fresh.
	Ready() bool
	// Classify maps the feature vector to a regime prediction.
	Classify(features *MLFeatures) (*RegimePrediction, error)
}

// FeatureSource abstracts the market-context storage behind the ML feature
// vector so the builder has no hard database dependency: production wires
// NewDBFeatureSource over pgxpool, tests use in-memory fakes, and per-field
// failures degrade to documented defaults instead of erroring the scan.
type FeatureSource interface {
	// FundingAverage returns the mean funding rate across exchanges over the
	// lookback window (per-8h rate, e.g. 0.0001).
	FundingAverage(ctx context.Context, symbol string, lookback time.Duration) (float64, error)
	// OpenInterestChange returns the latest open interest (USD) and the value
	// as of `lookback` ago; the caller derives the 24h change and rising flag.
	OpenInterestChange(ctx context.Context, symbol string, lookback time.Duration) (current, previous float64, err error)
	// FearGreedIndex returns the latest Fear & Greed value (0..100).
	FearGreedIndex(ctx context.Context) (float64, error)
	// HighImpactEvent reports whether a HIGH-impact macro event occurred
	// recently or is scheduled within the given horizon.
	HighImpactEvent(ctx context.Context, horizon time.Duration) (bool, error)
}

// FeatureBuilder constructs the feature vector for the regime classifier and
// produces predictions. Falls back to rule-based classification whenever the
// model is absent, stale or errors — the rule path is the primary path until
// a model artifact has been trained and validated.
type FeatureBuilder struct {
	source     FeatureSource
	classifier RegimeClassifier
}

// NewFeatureBuilder wires a feature source and an optional classifier.
// A nil classifier (or one whose Ready() is false) keeps the builder on the
// rule-based path; a nil source makes BuildFeatures fail loudly.
func NewFeatureBuilder(source FeatureSource, classifier RegimeClassifier) *FeatureBuilder {
	return &FeatureBuilder{source: source, classifier: classifier}
}

// BuildFeatures assembles the ML feature vector from feature-source data plus
// technical indicators. Missing collector data degrades per-field to sane
// defaults (documented on each field); an error is returned only when the
// builder itself is unusable (nil source).
func (fb *FeatureBuilder) BuildFeatures(
	ctx context.Context,
	symbol string,
	regime RegimeResult,
	candles []pionex.KlineCandle,
) (*MLFeatures, error) {
	if fb == nil || fb.source == nil {
		return nil, errors.New("marketdata: FeatureBuilder has no feature source")
	}

	features := &MLFeatures{
		FearGreed:     defaultFearGreed,
		HurstExponent: defaultHurst,
	}

	// 1. Funding: average across exchanges; extreme flag at |avg| > 0.001
	// (ExtremeFundingThreshold in queries.go, shared with the rule engine).
	if avg, err := fb.source.FundingAverage(ctx, symbol, fundingLookback); err == nil {
		features.FundingAvg = avg
		features.FundingExtreme = boolAsFloat(FundingIsExtreme(avg))
	}

	// 2. Open interest: 24h change in percent plus the rising flag.
	if current, previous, err := fb.source.OpenInterestChange(ctx, symbol, oiCompareLookback); err == nil {
		if previous > 0 {
			features.OIChange24h = (current - previous) / previous * 100
		}
		features.OIRising = boolAsFloat(current > previous)
	}

	// 3. Fear & Greed: latest snapshot, neutral 50 when absent.
	if fng, err := fb.source.FearGreedIndex(ctx); err == nil && fng >= 0 {
		features.FearGreed = fng
	}

	// 4. Macro calendar: HIGH-impact event within the horizon.
	if hit, err := fb.source.HighImpactEvent(ctx, eventHorizon); err == nil && hit {
		features.HighImpactEvent = 1
	}

	// 5. Technical block: DetectRegime outputs plus candle-derived inputs.
	features.RealizedVolDaily = regime.ATRPct
	features.ADX14 = regime.ADX
	features.Choppiness14 = regime.Choppiness
	features.BBWPercentile = regime.BBWPercentile

	if len(candles) >= 2 {
		interval := inferCandleSeconds(candles)
		features.PriceChange24h = priceChangeOver(candles, 24*time.Hour, interval)
		features.PriceChange7d = priceChangeOver(candles, 7*24*time.Hour, interval)
		features.HARForecast = mlHarForecast(candles, interval)
		if series := ExtractSeries(candles); series.Len() >= 64 {
			if hurst, ok := HurstDFA(series.Close); ok {
				features.HurstExponent = hurst
			}
		}
	}
	return features, nil
}

// PredictRegime uses the ONNX-backed classifier when one is loaded and ready,
// otherwise falls back to rule-based classification using the existing
// regime/confluence engine. The rule path swallows feature failures by
// design: a scan must never die because a collector table is empty.
func (fb *FeatureBuilder) PredictRegime(
	ctx context.Context,
	symbol string,
	regime RegimeResult,
	candles []pionex.KlineCandle,
) (*RegimePrediction, error) {
	if fb != nil && fb.classifier != nil && fb.classifier.Ready() {
		features, err := fb.BuildFeatures(ctx, symbol, regime, candles)
		if err == nil {
			if prediction, classifyErr := fb.classifier.Classify(features); classifyErr == nil && prediction != nil {
				if prediction.Source == "" {
					prediction.Source = PredictionSourceML
				}
				return prediction, nil
			}
		}
	}
	return fb.ruleBasedFallback(regime), nil
}

// ruleBasedFallback uses the existing confluence engine when the ML model is
// not available or has expired. Confidence heuristics: compressed bands
// (BBW below the 25th percentile) keep the RANGE read, a strong ADX keeps the
// trend read; a TREND_DOWN pinned to window lows with a steep EMA slope and
// strong ADX is escalated to CRASH, matching the trainer's crash label
// shape (24h return beyond -7% / drawdown beyond 10%).
func (fb *FeatureBuilder) ruleBasedFallback(regime RegimeResult) *RegimePrediction {
	confidence := 0.5
	if regime.BBWPercentile > 0 && regime.BBWPercentile < 25 {
		confidence = 0.7 // compressed = more likely to stay ranging
	}
	if regime.ADX > 30 {
		confidence = math.Max(confidence, 0.7) // strong trend detected
	}

	result := RegimePrediction{
		Regime:     regime.Regime, // RANGE, TREND_UP, TREND_DOWN
		Confidence: confidence,
		Source:     PredictionSourceRuleBased,
	}

	// Crash escalation: one-directional collapse — price at window lows,
	// steeply negative EMA slope and trend strength confirming.
	if regime.Regime == RegimeMLTrendDown &&
		regime.RangePositionPct <= 5 &&
		regime.EMASlopePct <= -3 &&
		regime.ADX > 30 {
		result.Regime = RegimeMLCrash
		result.Confidence = 0.8
	}
	return &result
}

// DBFeatureSource is the PostgreSQL-backed FeatureSource over the smart-data
// collector tables (migrations/0023_smart_data.sql: funding_snapshots,
// oi_history, sentiment_snapshots, economic_events). Query semantics mirror
// the Service layer in queries.go (exchange-fresh funding average, summed
// per-exchange OI) so training and inference see the same market context.
type DBFeatureSource struct {
	db *pgxpool.Pool
}

// NewDBFeatureSource creates the PostgreSQL feature source.
func NewDBFeatureSource(db *pgxpool.Pool) *DBFeatureSource {
	return &DBFeatureSource{db: db}
}

// FundingAverage implements FeatureSource: mean funding rate across the
// exchanges whose latest snapshot inside the lookback is still fresh.
func (s *DBFeatureSource) FundingAverage(ctx context.Context, symbol string, lookback time.Duration) (float64, error) {
	var avg float64
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(AVG(funding_rate), 0)
		FROM (
			SELECT DISTINCT ON (exchange) exchange, funding_rate
			FROM funding_snapshots
			WHERE symbol = $1 AND captured_at >= $2
			ORDER BY exchange, captured_at DESC
		) latest
	`, symbol, time.Now().UTC().Add(-lookback)).Scan(&avg)
	return avg, err
}

// OpenInterestChange implements FeatureSource: per-exchange latest and oldest
// OI inside the lookback, summed across exchanges (total tracked OI change).
func (s *DBFeatureSource) OpenInterestChange(ctx context.Context, symbol string, lookback time.Duration) (float64, float64, error) {
	var current, previous float64
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(latest_usd), 0), COALESCE(SUM(oldest_usd), 0)
		FROM (
			SELECT exchange,
			       (array_agg(oi_usd ORDER BY captured_at DESC))[1] AS latest_usd,
			       (array_agg(oi_usd ORDER BY captured_at ASC))[1]  AS oldest_usd
			FROM oi_history
			WHERE symbol = $1 AND captured_at >= $2 AND oi_usd IS NOT NULL
			GROUP BY exchange
		) per_exchange
	`, symbol, time.Now().UTC().Add(-lookback)).Scan(&current, &previous)
	return current, previous, err
}

// FearGreedIndex implements FeatureSource: latest Fear & Greed snapshot.
func (s *DBFeatureSource) FearGreedIndex(ctx context.Context) (float64, error) {
	var value *float64
	if err := s.db.QueryRow(ctx, `
		SELECT value FROM sentiment_snapshots
		WHERE source = 'fng'
		ORDER BY captured_at DESC LIMIT 1
	`).Scan(&value); err != nil {
		return 0, err
	}
	if value == nil {
		return 0, errors.New("marketdata: latest fear & greed value is NULL")
	}
	return *value, nil
}

// HighImpactEvent implements FeatureSource: a High-impact event that started
// within the last hour or is scheduled inside the horizon.
func (s *DBFeatureSource) HighImpactEvent(ctx context.Context, horizon time.Duration) (bool, error) {
	now := time.Now().UTC()
	var hit bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM economic_events
			WHERE impact = 'High'
			  AND event_time >= $1
			  AND event_time <= $2
		)
	`, now.Add(-time.Hour), now.Add(horizon)).Scan(&hit)
	return hit, err
}

// inferCandleSeconds returns the typical spacing between candles in seconds.
// Pionex kline timestamps are seconds, but the median-diff heuristic also
// normalizes millisecond feeds so lookback math stays correct either way.
func inferCandleSeconds(candles []pionex.KlineCandle) float64 {
	if len(candles) < 2 {
		return 0
	}
	diffs := make([]float64, 0, len(candles)-1)
	for i := 1; i < len(candles); i++ {
		d := float64(candles[i].Time - candles[i-1].Time)
		if d > 0 {
			diffs = append(diffs, d)
		}
	}
	if len(diffs) == 0 {
		return 0
	}
	sort.Float64s(diffs)
	median := diffs[len(diffs)/2]
	if median >= 10000 {
		// Millisecond feed: a seconds-based 15m candle differs by 900000.
		return median / 1000
	}
	return median
}

// barsPerDay converts an inferred candle interval into candles per day,
// floored at 1 so short windows never divide by zero.
func barsPerDay(intervalSeconds float64) int {
	if intervalSeconds <= 0 {
		return 96 // 15m default used across the engine
	}
	bars := int(math.Round(86400 / intervalSeconds))
	if bars < 1 {
		return 1
	}
	return bars
}

// priceChangeOver returns the percent change between the newest close and
// the newest close at least `target` old. When the candle window is shorter
// than the target the oldest available close is the reference.
func priceChangeOver(candles []pionex.KlineCandle, target time.Duration, intervalSeconds float64) float64 {
	if len(candles) < 2 {
		return 0
	}
	last := candles[len(candles)-1]
	lastClose, _ := last.Close.Float64()
	if lastClose <= 0 {
		return 0
	}
	reference := candles[0]
	if intervalSeconds > 0 {
		threshold := last.Time - int64(target.Seconds())
		for _, candle := range candles {
			if candle.Time <= threshold {
				reference = candle
			} else {
				break
			}
		}
	}
	refClose, _ := reference.Close.Float64()
	if refClose <= 0 {
		return 0
	}
	return (lastClose - refClose) / refClose * 100
}

// mlHarForecast produces the har_forecast feature in daily volatility
// percent (the unit the Python trainer uses). The primary path is the
// engine's OLS HAR model (ForecastVolatilityFromCandles, annualized %)
// rescaled to daily via /sqrt(365); windows too short or degenerate for OLS
// (< 31 daily buckets, constant volatility) fall back to the fixed-weight
// Corsi combination below.
func mlHarForecast(candles []pionex.KlineCandle, intervalSeconds float64) float64 {
	if _, annualizedPct, err := ForecastVolatilityFromCandles(candles); err == nil &&
		annualizedPct > 0 && !math.IsNaN(annualizedPct) && !math.IsInf(annualizedPct, 0) {
		return annualizedPct / math.Sqrt(365)
	}
	return harForecastFallback(candles, barsPerDay(intervalSeconds))
}

// harForecastFallback implements a Corsi HAR-RV style daily volatility
// forecast as a model feature: realized variance is aggregated over daily /
// weekly / monthly windows and blended with the literature weights
// 0.5/0.3/0.2 over the components that have data. The result is expressed
// as a daily volatility percent so it is scale-comparable with
// RealizedVolDaily (ATR%).
func harForecastFallback(candles []pionex.KlineCandle, perDay int) float64 {
	n := len(candles)
	if n < 2 || perDay < 1 {
		return 0
	}
	closes := make([]float64, 0, n)
	for _, candle := range candles {
		if v, _ := candle.Close.Float64(); v > 0 {
			closes = append(closes, v)
		}
	}
	if len(closes) < 2 {
		return 0
	}
	returns := make([]float64, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		returns[i-1] = math.Log(closes[i] / closes[i-1])
	}
	rv := func(days int) (float64, bool) {
		take := days * perDay
		if take > len(returns) {
			return 0, false
		}
		sumSq := 0.0
		for _, r := range returns[len(returns)-take:] {
			sumSq += r * r
		}
		return sumSq / float64(days), true
	}
	weights := []struct {
		days  int
		share float64
	}{{1, 0.5}, {5, 0.3}, {22, 0.2}}

	totalWeight, weighted := 0.0, 0.0
	for _, component := range weights {
		if value, ok := rv(component.days); ok {
			weighted += component.share * value
			totalWeight += component.share
		}
	}
	if totalWeight <= 0 {
		return 0
	}
	return math.Sqrt(weighted/totalWeight) * 100
}

// boolAsFloat encodes boolean flags as the 0/1 floats the model expects.
func boolAsFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
