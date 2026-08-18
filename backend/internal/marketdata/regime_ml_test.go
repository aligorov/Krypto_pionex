package marketdata

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// fakeFeatureSource is an in-memory FeatureSource so the builder and its
// tests never need a database (per the DB-agnostic interface requirement).
type fakeFeatureSource struct {
	fundingAvg float64
	fundingErr error

	oiCurrent  float64
	oiPrevious float64
	oiErr      error

	fng    float64
	fngErr error

	event    bool
	eventErr error
}

func (f *fakeFeatureSource) FundingAverage(_ context.Context, _ string, _ time.Duration) (float64, error) {
	return f.fundingAvg, f.fundingErr
}

func (f *fakeFeatureSource) OpenInterestChange(_ context.Context, _ string, _ time.Duration) (float64, float64, error) {
	return f.oiCurrent, f.oiPrevious, f.oiErr
}

func (f *fakeFeatureSource) FearGreedIndex(_ context.Context) (float64, error) {
	return f.fng, f.fngErr
}

func (f *fakeFeatureSource) HighImpactEvent(_ context.Context, _ time.Duration) (bool, error) {
	return f.event, f.eventErr
}

// failingSource errors on every domain.
func failingSource() *fakeFeatureSource {
	err := errors.New("collector table missing")
	return &fakeFeatureSource{
		fundingErr: err,
		oiErr:      err,
		fngErr:     err,
		eventErr:   err,
	}
}

// stubClassifier is a RegimeClassifier test double.
type stubClassifier struct {
	ready      bool
	prediction *RegimePrediction
	err        error
}

func (s *stubClassifier) Ready() bool { return s.ready }

func (s *stubClassifier) Classify(_ *MLFeatures) (*RegimePrediction, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.prediction, nil
}

// mlCandle builds a candle at an explicit unix-seconds timestamp.
func mlCandle(ts int64, close float64) pionex.KlineCandle {
	return pionex.KlineCandle{
		Time:   ts,
		Open:   decimal.NewFromFloat(close),
		Close:  decimal.NewFromFloat(close),
		High:   decimal.NewFromFloat(close * 1.005),
		Low:    decimal.NewFromFloat(close * 0.995),
		Volume: decimal.NewFromFloat(100),
	}
}

// mlCandles builds count 15-minute candles following the pattern.
func mlCandles(pattern func(i int) float64, count int) []pionex.KlineCandle {
	const spacing = 900 // seconds
	out := make([]pionex.KlineCandle, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, mlCandle(int64(i)*spacing, pattern(i)))
	}
	return out
}

func TestRuleBasedFallback(t *testing.T) {
	builder := NewFeatureBuilder(failingSource(), nil)

	cases := []struct {
		name           string
		regime         RegimeResult
		wantRegime     string
		wantConfidence float64
	}{
		{
			name:           "range with compressed bands keeps range at high confidence",
			regime:         RegimeResult{Regime: "RANGE", BBWPercentile: 10, ADX: 15},
			wantRegime:     RegimeMLRange,
			wantConfidence: 0.7,
		},
		{
			name:           "trend up with strong adx keeps trend at high confidence",
			regime:         RegimeResult{Regime: "TREND_UP", ADX: 35, BBWPercentile: 60},
			wantRegime:     RegimeMLTrendUp,
			wantConfidence: 0.7,
		},
		{
			name:           "trend down pinned to window lows escalates to crash",
			regime:         RegimeResult{Regime: "TREND_DOWN", ADX: 35, RangePositionPct: 2, EMASlopePct: -5},
			wantRegime:     RegimeMLCrash,
			wantConfidence: 0.8,
		},
		{
			name:           "undecided range stays at base confidence",
			regime:         RegimeResult{Regime: "RANGE", BBWPercentile: 50, ADX: 15},
			wantRegime:     RegimeMLRange,
			wantConfidence: 0.5,
		},
		{
			name:           "trend down without floor-pin stays trend down",
			regime:         RegimeResult{Regime: "TREND_DOWN", ADX: 35, RangePositionPct: 40, EMASlopePct: -5},
			wantRegime:     RegimeMLTrendDown,
			wantConfidence: 0.7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prediction := builder.ruleBasedFallback(tc.regime)
			if prediction.Regime != tc.wantRegime {
				t.Fatalf("regime: want %s, got %s", tc.wantRegime, prediction.Regime)
			}
			if prediction.Confidence != tc.wantConfidence {
				t.Fatalf("confidence: want %.2f, got %.2f", tc.wantConfidence, prediction.Confidence)
			}
			if prediction.Source != PredictionSourceRuleBased {
				t.Fatalf("source: want %s, got %s", PredictionSourceRuleBased, prediction.Source)
			}
		})
	}
}

func TestBuildFeatures(t *testing.T) {
	ctx := context.Background()
	source := &fakeFeatureSource{
		fundingAvg: 0.0005, // below the 0.001 extreme threshold
		oiCurrent:  120,
		oiPrevious: 100,
		fng:        25,
		event:      true,
	}
	builder := NewFeatureBuilder(source, nil)
	regime := RegimeResult{
		Regime:        "RANGE",
		ATRPct:        2.5,
		ADX:           18,
		Choppiness:    60,
		BBWPercentile: 30,
	}
	// 8 days of 15m candles on a linear ramp.
	candles := mlCandles(func(i int) float64 { return 100 + float64(i)*0.13 }, 768)

	features, err := builder.BuildFeatures(ctx, "BTC_USDT_PERP", regime, candles)
	if err != nil {
		t.Fatalf("BuildFeatures: %v", err)
	}
	if features.FundingAvg != 0.0005 {
		t.Fatalf("FundingAvg: want 0.0005, got %v", features.FundingAvg)
	}
	if features.FundingExtreme != 0 {
		t.Fatalf("FundingExtreme: want 0, got %v", features.FundingExtreme)
	}
	if diff := math.Abs(features.OIChange24h - 20.0); diff > 1e-9 {
		t.Fatalf("OIChange24h: want 20, got %v", features.OIChange24h)
	}
	if features.OIRising != 1 {
		t.Fatalf("OIRising: want 1, got %v", features.OIRising)
	}
	if features.RealizedVolDaily != 2.5 {
		t.Fatalf("RealizedVolDaily: want 2.5, got %v", features.RealizedVolDaily)
	}
	if features.ADX14 != 18 || features.Choppiness14 != 60 || features.BBWPercentile != 30 {
		t.Fatalf("regime mapping wrong: %+v", features)
	}
	if features.FearGreed != 25 {
		t.Fatalf("FearGreed: want 25, got %v", features.FearGreed)
	}
	if features.HighImpactEvent != 1 {
		t.Fatalf("HighImpactEvent: want 1, got %v", features.HighImpactEvent)
	}
	// 8-day linear ramp: every 24h slice gains ~6.6%.
	if features.PriceChange24h < 5 || features.PriceChange7d < 5 {
		t.Fatalf("price changes too small on ramp: 24h=%v 7d=%v", features.PriceChange24h, features.PriceChange7d)
	}
	if features.HARForecast <= 0 {
		t.Fatalf("HARForecast: want > 0 on a moving series, got %v", features.HARForecast)
	}
	if features.HurstExponent <= 0.1 || features.HurstExponent > 1 {
		t.Fatalf("HurstExponent out of range: %v", features.HurstExponent)
	}
}

func TestBuildFeaturesFundingExtreme(t *testing.T) {
	source := &fakeFeatureSource{fundingAvg: -0.002}
	builder := NewFeatureBuilder(source, nil)
	features, err := builder.BuildFeatures(context.Background(), "ETH_USDT_PERP", RegimeResult{}, nil)
	if err != nil {
		t.Fatalf("BuildFeatures: %v", err)
	}
	if features.FundingExtreme != 1 {
		t.Fatalf("FundingExtreme: want 1 for |avg|=0.002, got %v", features.FundingExtreme)
	}
}

func TestBuildFeaturesMissingData(t *testing.T) {
	builder := NewFeatureBuilder(failingSource(), nil)

	features, err := builder.BuildFeatures(context.Background(), "BTC_USDT_PERP", RegimeResult{}, nil)
	if err != nil {
		t.Fatalf("missing collector data must degrade to defaults, got error: %v", err)
	}
	if features.FundingAvg != 0 || features.FundingExtreme != 0 {
		t.Fatalf("funding defaults violated: %+v", features)
	}
	if features.OIChange24h != 0 || features.OIRising != 0 {
		t.Fatalf("OI defaults violated: %+v", features)
	}
	if features.FearGreed != defaultFearGreed {
		t.Fatalf("FearGreed default: want %.0f, got %v", defaultFearGreed, features.FearGreed)
	}
	if features.HighImpactEvent != 0 {
		t.Fatalf("HighImpactEvent default violated: %+v", features)
	}
	if features.HurstExponent != defaultHurst {
		t.Fatalf("Hurst default: want %.1f, got %v", defaultHurst, features.HurstExponent)
	}
	if features.HARForecast != 0 {
		t.Fatalf("HARForecast without candles: want 0, got %v", features.HARForecast)
	}
}

func TestBuildFeaturesNilSource(t *testing.T) {
	builder := NewFeatureBuilder(nil, nil)
	if _, err := builder.BuildFeatures(context.Background(), "BTC_USDT_PERP", RegimeResult{}, nil); err == nil {
		t.Fatal("nil source must fail loudly")
	}
}

func TestPredictRegimeFallbackWithoutModel(t *testing.T) {
	ctx := context.Background()
	regime := RegimeResult{Regime: "RANGE", BBWPercentile: 10}

	// No classifier wired: pure rule-based path.
	builder := NewFeatureBuilder(failingSource(), nil)
	prediction, err := builder.PredictRegime(ctx, "BTC_USDT_PERP", regime, nil)
	if err != nil {
		t.Fatalf("PredictRegime: %v", err)
	}
	if prediction.Source != PredictionSourceRuleBased {
		t.Fatalf("source: want rule_based, got %s", prediction.Source)
	}
	if prediction.Regime != RegimeMLRange || prediction.Confidence != 0.7 {
		t.Fatalf("unexpected prediction: %+v", prediction)
	}

	// Classifier present but not ready: still rule-based.
	notReady := NewFeatureBuilder(failingSource(), &stubClassifier{ready: false})
	prediction, err = notReady.PredictRegime(ctx, "BTC_USDT_PERP", regime, nil)
	if err != nil {
		t.Fatalf("PredictRegime: %v", err)
	}
	if prediction.Source != PredictionSourceRuleBased {
		t.Fatalf("not-ready classifier must not be used, got source %s", prediction.Source)
	}

	// Ready classifier that fails: falls back, never errors the scan.
	broken := &stubClassifier{ready: true, err: errors.New("onnx session closed")}
	fallback := NewFeatureBuilder(failingSource(), broken)
	prediction, err = fallback.PredictRegime(ctx, "BTC_USDT_PERP", regime, nil)
	if err != nil {
		t.Fatalf("classifier failure must fall back, got error: %v", err)
	}
	if prediction.Source != PredictionSourceRuleBased {
		t.Fatalf("classifier failure must use rule_based, got %s", prediction.Source)
	}
}

func TestPredictRegimeUsesReadyClassifier(t *testing.T) {
	source := &fakeFeatureSource{fundingAvg: 0.0001, oiCurrent: 100, oiPrevious: 100, fng: 60}
	classifier := &stubClassifier{
		ready:      true,
		prediction: &RegimePrediction{Regime: RegimeMLCrash, Confidence: 0.93},
	}
	builder := NewFeatureBuilder(source, classifier)
	prediction, err := builder.PredictRegime(
		context.Background(), "BTC_USDT_PERP",
		RegimeResult{Regime: "RANGE"}, mlCandles(func(i int) float64 { return 100 }, 100),
	)
	if err != nil {
		t.Fatalf("PredictRegime: %v", err)
	}
	if prediction.Source != PredictionSourceML {
		t.Fatalf("source: want ml, got %s", prediction.Source)
	}
	if prediction.Regime != RegimeMLCrash || prediction.Confidence != 0.93 {
		t.Fatalf("classifier output not propagated: %+v", prediction)
	}
}

func TestFeatureVectorMatchesFeatureNames(t *testing.T) {
	names := FeatureNames()
	if len(names) != 14 {
		t.Fatalf("FeatureNames: want 14, got %d", len(names))
	}
	features := &MLFeatures{
		FundingAvg: 0.0002, FundingExtreme: 0, OIChange24h: 5, OIRising: 1,
		RealizedVolDaily: 3, HARForecast: 2.8, HurstExponent: 0.47, ADX14: 25,
		Choppiness14: 55, BBWPercentile: 40, PriceChange24h: -1, PriceChange7d: 4,
		FearGreed: 72, HighImpactEvent: 1,
	}
	vector := features.FeatureVector()
	if len(vector) != len(names) {
		t.Fatalf("vector length %d != names length %d", len(vector), len(names))
	}
	for i, name := range names {
		if math.IsNaN(vector[i]) {
			t.Fatalf("feature %s at index %d is NaN", name, i)
		}
	}
	// FundingAvg must be first (matches the ONNX input order contract).
	if vector[0] != 0.0002 || vector[13] != 1 {
		t.Fatalf("vector order mismatch: %+v", vector)
	}

	// FeatureNames returns a copy: mutating it must not leak into the canonical order.
	names[0] = "tampered"
	if FeatureNames()[0] != "funding_avg" {
		t.Fatal("FeatureNames must return a defensive copy")
	}
}

func TestInferCandleSeconds(t *testing.T) {
	seconds := inferCandleSeconds(mlCandles(func(i int) float64 { return 100 }, 10))
	if seconds != 900 {
		t.Fatalf("seconds feed: want 900, got %v", seconds)
	}
	millis := []pionex.KlineCandle{
		mlCandle(0, 100), mlCandle(900000, 100), mlCandle(1800000, 100),
	}
	if got := inferCandleSeconds(millis); got != 900 {
		t.Fatalf("millis feed: want 900, got %v", got)
	}
	if got := inferCandleSeconds(nil); got != 0 {
		t.Fatalf("empty feed: want 0, got %v", got)
	}
}

func TestPriceChangeOver(t *testing.T) {
	candles := []pionex.KlineCandle{
		mlCandle(0, 100),
		mlCandle(900, 110),
		mlCandle(1800, 121),
	}
	// 900s lookback: threshold t=900 includes the t=900 candle (110) -> +10%.
	if got := priceChangeOver(candles, 900*time.Second, 900); math.Abs(got-10) > 1e-9 {
		t.Fatalf("boundary lookback: want 10, got %v", got)
	}
	// 901s lookback: threshold t=899 drops to the t=0 candle (100) -> +21%.
	if got := priceChangeOver(candles, 901*time.Second, 900); math.Abs(got-21) > 1e-9 {
		t.Fatalf("just past boundary: want 21, got %v", got)
	}
	// Lookback beyond the window: oldest candle is the reference -> +21%.
	if got := priceChangeOver(candles, 24*time.Hour, 900); math.Abs(got-21) > 1e-9 {
		t.Fatalf("window overflow: want 21, got %v", got)
	}
	flat := mlCandles(func(i int) float64 { return 100 }, 96)
	if got := priceChangeOver(flat, 24*time.Hour, 900); math.Abs(got) > 1e-9 {
		t.Fatalf("flat series: want 0, got %v", got)
	}
}

func TestHarForecastFallback(t *testing.T) {
	if got := harForecastFallback(mlCandles(func(i int) float64 { return 100 }, 96), 96); got != 0 {
		t.Fatalf("constant series: want 0 variance forecast, got %v", got)
	}
	volatile := mlCandles(func(i int) float64 {
		if i%2 == 0 {
			return 100 * 1.01
		}
		return 100 * 0.99
	}, 96*10)
	if got := harForecastFallback(volatile, 96); got <= 0 {
		t.Fatalf("alternating series: want positive forecast, got %v", got)
	}
	if got := harForecastFallback(nil, 96); got != 0 {
		t.Fatalf("no candles: want 0, got %v", got)
	}
}

func TestMLHarForecast(t *testing.T) {
	// Short window (10 days of 15m candles): the OLS HAR needs 31+ daily
	// buckets, so the wrapper must take the weighted fallback and still
	// produce a positive daily-vol figure.
	short := mlCandles(func(i int) float64 {
		if i%2 == 0 {
			return 100 * 1.01
		}
		return 100 * 0.99
	}, 96*10)
	if got := mlHarForecast(short, 900); got <= 0 {
		t.Fatalf("short window fallback: want > 0, got %v", got)
	}

	// 40 days of 15m candles with day-alternating amplitude: enough history
	// for the OLS path, non-constant daily RV so the fit is not singular.
	// +-1% per candle => ~9.8% daily vol: the daily-scaled forecast must be
	// a sane single-digit-to-low-tens number, not the ~190% annualized one.
	long := mlCandles(func(i int) float64 {
		day := i / 96
		amp := 0.01 + 0.01*float64(day%2)
		if i%2 == 0 {
			return 100 * (1 + amp)
		}
		return 100 * (1 - amp)
	}, 96*40)
	got := mlHarForecast(long, 900)
	if got <= 0 || got > 50 {
		t.Fatalf("long window canonical path: want daily-scaled (0,50], got %v", got)
	}

	if got := mlHarForecast(nil, 900); got != 0 {
		t.Fatalf("no candles: want 0, got %v", got)
	}
}
