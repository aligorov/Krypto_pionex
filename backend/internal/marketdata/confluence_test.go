package marketdata

import (
	"math"
	"math/rand"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func seriesFromCloses(closes []float64) *Series {
	s := &Series{
		Time:   make([]int64, len(closes)),
		Open:   make([]float64, len(closes)),
		High:   make([]float64, len(closes)),
		Low:    make([]float64, len(closes)),
		Close:  append([]float64{}, closes...),
		Volume: make([]float64, len(closes)),
	}
	for i := range closes {
		s.Time[i] = int64(i) * 900
		s.Open[i] = closes[i]
		s.High[i] = closes[i] * 1.001
		s.Low[i] = closes[i] * 0.999
		s.Volume[i] = 100
	}
	return s
}

// deterministic pseudo-noise in [-1, 1]
func pseudoNoise(i int) float64 {
	return math.Sin(float64(i)*12.9898) * 0.5
}

// AR(1) with a negative coefficient is genuinely anti-persistent (H<0.5);
// a positive coefficient clusters returns and is persistent (H>0.5). Note
// DFA detrends the mean, so a constant drift alone would NOT register as
// persistence — the autocorrelation structure is what it measures.
func TestHurstDFARegimes(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	noise := func() float64 { return rng.NormFloat64() }

	meanRev := make([]float64, 0, 300)
	price, ret := 100.0, 0.0
	for i := 0; i < 300; i++ {
		ret = -0.55*ret + 0.004*noise()
		price *= 1 + ret
		meanRev = append(meanRev, price)
	}
	hMR, ok := HurstDFA(meanRev)
	if !ok || hMR >= 0.5 {
		t.Fatalf("mean reversion must read H<0.5, got %.3f ok=%v", hMR, ok)
	}

	persistent := make([]float64, 0, 300)
	price, ret = 100.0, 0.0
	for i := 0; i < 300; i++ {
		ret = 0.5*ret + 0.004*noise()
		price *= 1 + ret
		persistent = append(persistent, price)
	}
	hT, ok := HurstDFA(persistent)
	if !ok || hT <= 0.5 {
		t.Fatalf("persistent returns must read H>0.5, got %.3f ok=%v", hT, ok)
	}
}

func TestOBVDivergenceBullish(t *testing.T) {
	// Decline on heavy volume -> rally on HEAVY volume (OBV up) -> deeper
	// price low on LIGHT volume: price LL while OBV holds a higher low.
	n := 60
	closes := make([]float64, n)
	volumes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100
		volumes[i] = 50
	}
	for i := 20; i < 40; i++ { // decline 100 -> 90.2
		closes[i] = 100 - float64(i-20)*0.49
		volumes[i] = 100
	}
	for i := 40; i < 50; i++ { // rally to 95.7 on heavy volume
		closes[i] = 90.2 + float64(i-40)*0.55
		volumes[i] = 300
	}
	for i := 50; i < 60; i++ { // lower low 88 on light volume
		closes[i] = 95.7 - float64(i-50)*0.77
		volumes[i] = 30
	}
	div := DetectOBVDivergence(closes, volumes, 40)
	if div.Direction != 1 {
		t.Fatalf("expected bullish divergence, got dir=%.0f strength=%.2f", div.Direction, div.Strength)
	}
}

func TestOBVDivergenceNoneOnTrend(t *testing.T) {
	// Monotonous decline: OBV falls with price, no divergence.
	n := 60
	closes := make([]float64, n)
	volumes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100 - float64(i)*0.2
		volumes[i] = 100
	}
	if div := DetectOBVDivergence(closes, volumes, 40); div.Direction != 0 {
		t.Fatalf("monotone decline must not produce divergence, got %.0f", div.Direction)
	}
}

func TestIFTRSICross(t *testing.T) {
	closes := make([]float64, 0, 120)
	price := 100.0
	for i := 0; i < 100; i++ {
		price *= 0.995
		closes = append(closes, price)
	}
	for i := 0; i < 20; i++ {
		price *= 1.02
		closes = append(closes, price)
	}
	ift := ComputeIFTRSI(closes)
	if !ift.CrossedUp && ift.Current <= ift.Prev {
		t.Fatalf("reversal leg must lift IFT-RSI (cur=%.2f prev=%.2f)", ift.Current, ift.Prev)
	}
}

func TestAnchoredVWAPStretch(t *testing.T) {
	// Flat 100 for 60 bars, then a rally: price stretched above the
	// anchored fair value. The rally top is the LAST bar — the anchor must
	// fall back instead of degenerating into a one-bar VWAP.
	closes := make([]float64, 0, 90)
	for i := 0; i < 60; i++ {
		closes = append(closes, 100)
	}
	for i := 0; i < 30; i++ {
		closes = append(closes, 100+float64(i)*0.5+0.5)
	}
	res := ComputeAnchoredVWAP(seriesFromCloses(closes))
	if res.Value == 0 || res.ZScore < 1.0 {
		t.Fatalf("rally must read stretched above anchored VWAP, value=%.2f z=%.2f", res.Value, res.ZScore)
	}
}

func TestConfluenceRangeOnCompression(t *testing.T) {
	// Exponentially decaying oscillation: by the end the bands are tight
	// (low BBW percentile), volatility is compressed and the series is
	// mean-reverting — the neutral grid's habitat.
	closes := make([]float64, 0, 240)
	for i := 0; i < 240; i++ {
		amplitude := 4.0 * math.Exp(-float64(i)/70)
		closes = append(closes, 100+amplitude*math.Sin(float64(i)/4)+0.05*pseudoNoise(i))
	}
	s := seriesFromCloses(closes)
	bundle := ComputeIndicatorBundle(s)
	regime := DetectRegime(klinesFromSeries(s))
	result := EvaluateConfluence(regime, bundle)
	if result.Verdict != ConfluenceSupportRange {
		t.Fatalf("compressed market must support RANGE, got %s (range=%.2f squeeze=%v bbwPct=%.1f gate=%s)",
			result.Verdict, result.RangeScore, bundle.Keltner.InSqueeze, regime.BBWPercentile, result.HurstGate)
	}
}

func TestHurstHardVetoNeutral(t *testing.T) {
	if !HurstHardVetoNeutral(IndicatorBundle{Hurst: 0.62, HurstOK: true}) {
		t.Fatalf("H=0.62 must veto neutral entries")
	}
	if HurstHardVetoNeutral(IndicatorBundle{Hurst: 0.70, HurstOK: false}) {
		t.Fatalf("unreliable estimate must not veto")
	}
	if HurstHardVetoNeutral(IndicatorBundle{Hurst: 0.50, HurstOK: true}) {
		t.Fatalf("H=0.50 must not veto")
	}
}

func klinesFromSeries(s *Series) []pionex.KlineCandle {
	candles := make([]pionex.KlineCandle, s.Len())
	for i := range candles {
		candles[i] = pionex.KlineCandle{
			Time:   s.Time[i],
			Open:   decimal.NewFromFloat(s.Open[i]),
			High:   decimal.NewFromFloat(s.High[i]),
			Low:    decimal.NewFromFloat(s.Low[i]),
			Close:  decimal.NewFromFloat(s.Close[i]),
			Volume: decimal.NewFromFloat(s.Volume[i]),
		}
	}
	return candles
}

func TestConfluenceFibonacciAndMACDLong(t *testing.T) {
	bundle := IndicatorBundle{
		Hurst:   0.48,
		HurstOK: true,
		OBVDiv:  OBVDivergence{Direction: 1, Strength: 0.8},
		Fib:     FibonacciRetracement{InGoldenPocket: true, TrendDir: 1},
		MACD:    MACDResult{CrossedUp: true},
		StochRSI: StochRSIResult{CrossedUp: true},
	}
	regime := RegimeResult{Regime: "TREND_UP"}
	res := EvaluateConfluence(regime, bundle)
	if res.Verdict != ConfluenceSupportLong {
		t.Fatalf("expected SUPPORT_LONG, got %s (longScore=%.2f)", res.Verdict, res.LongScore)
	}
	if res.LongScore < 0.50 {
		t.Fatalf("expected longScore >= 0.50, got %.2f", res.LongScore)
	}
}

func TestConfluenceDirectionalConflict(t *testing.T) {
	bundle := IndicatorBundle{
		Hurst:    0.48,
		HurstOK:  true,
		OBVDiv:   OBVDivergence{Direction: 1, Strength: 0.9}, // long
		IFT:      IFTRSIResult{CrossedUp: true},              // long
		Fib:      FibonacciRetracement{InGoldenPocket: true, TrendDir: 1}, // long
		MACD:     MACDResult{CrossedDown: true},              // short
		StochRSI: StochRSIResult{CrossedDown: true},          // short
		AVWAP:    AVWAPResult{ZScore: 2.0},                   // short
	}
	regime := RegimeResult{Regime: "RANGE"}
	res := EvaluateConfluence(regime, bundle)
	if res.Verdict != ConfluenceConflict {
		t.Fatalf("expected CONFLICT when both sides strongly supported, got %s (long=%.2f short=%.2f)",
			res.Verdict, res.LongScore, res.ShortScore)
	}
}

