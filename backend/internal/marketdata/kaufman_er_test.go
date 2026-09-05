package marketdata

import (
	"math"
	"strings"
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.89-A P2: Kaufman Efficiency Ratio = |p_N − p_0| / Σ|p_i − p_{i−1}|.
func TestKaufmanER(t *testing.T) {
	// Monotone advance: net equals path → ER = 1.
	trending := make([]float64, 0, 50)
	for i := 0; i < 50; i++ {
		trending = append(trending, 100+2*float64(i))
	}
	if er := KaufmanER(trending); er < 0.999 {
		t.Fatalf("monotone series must have ER 1.0, got %.4f", er)
	}
	// Pure sawtooth (+2/−2): ends where it started → ER ≈ 0.
	saw := make([]float64, 0, 50)
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			saw = append(saw, 100)
		} else {
			saw = append(saw, 102)
		}
	}
	if er := KaufmanER(saw); er > 0.05 {
		t.Fatalf("sawtooth must have ER ≈ 0, got %.4f", er)
	}
	// Persistent drift with pullbacks (+1/−0.5): net 0.5 per 2 bars of a
	// 1.5 path → ER ≈ 1/3.
	mixed := make([]float64, 0, 50)
	level := 100.0
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			level += 1
		} else {
			level -= 0.5
		}
		mixed = append(mixed, level)
	}
	if er := KaufmanER(mixed); er < 0.30 || er > 0.36 {
		t.Fatalf("+1/−0.5 series must have ER ≈ 0.33, got %.4f", er)
	}
	// Degenerate inputs: no divide-by-zero, no panic.
	if er := KaufmanER(nil); er != 0 {
		t.Fatalf("nil series must yield 0, got %v", er)
	}
	if er := KaufmanER([]float64{100}); er != 0 {
		t.Fatalf("single point must yield 0, got %v", er)
	}
	if er := KaufmanER([]float64{100, 100, 100}); er != 0 {
		t.Fatalf("flat series must yield 0, got %v", er)
	}
	// Regime labels follow the band edges.
	if kaufmanRegimeLabel(0.75) != "trending" ||
		kaufmanRegimeLabel(0.10) != "mean_reversion" ||
		kaufmanRegimeLabel(0.45) != "neutral" {
		t.Fatal("kaufmanRegimeLabel must bucket 0.75/0.10/0.45 into trending/mean_reversion/neutral")
	}
}

// The bundle must carry ER even on windows shorter than the confluence
// floor (40 candles) — the scanner's 30-candle minimum still gets telemetry.
func TestKaufmanERBundleShortWindow(t *testing.T) {
	short := ExtractSeries(synthCandles(func(i int) float64 { return 100 * math.Pow(1.01, float64(i)) }, 35))
	bundle := ComputeIndicatorBundle(short)
	if bundle.KaufmanER < 0.99 {
		t.Fatalf("35-candle monotone series must still read ER ≈ 1, got %.4f", bundle.KaufmanER)
	}
}

// Scanner wiring: a directional tape (ER ≈ 1) vetoes the NEUTRAL entry with
// the «Kaufman ER … сетка не входит» reason; a mean-reverting sawtooth
// passes with the kaufmanER / kaufmanRegime telemetry in model_assumptions.
func TestScoreCandidateKaufmanERGate(t *testing.T) {
	symbol := feeGateTestSymbol()
	volume := decimal.NewFromInt(5_000_000)
	config := feeGateScanConfig()
	// Zero friction so the fee-gate stays out of the way — this test pins
	// the ER gate alone.
	config.FeeBps, config.SlippageBps = 0, 0

	// Trending tape: +2% per candle compounds to ~4.9× over 80 candles; the
	// mature-trend LONG demotion (ADX ≥ 28) drops it to no_trend and the ER
	// veto finishes the job.
	trendTicker := pionex.TickerInfo{
		Symbol: "TST_USDT", Open: decimal.NewFromInt(100),
		Close: decimal.NewFromFloat(100 * math.Pow(1.02, 79)),
		Amount: volume,
	}
	trendCandles := synthCandles(func(i int) float64 { return 100 * math.Pow(1.02, float64(i)) }, 80)
	candidate, err := scoreCandidate(symbol, trendTicker, volume, trendCandles, config)
	if err != nil {
		t.Fatalf("scoreCandidate (trend): %v", err)
	}
	if candidate.Decision != "REJECTED" {
		t.Fatalf("trending tape must be rejected, got %s (%s)", candidate.Decision, candidate.RejectionReason)
	}
	if !strings.Contains(candidate.RejectionReason, "Kaufman ER") {
		t.Fatalf("trending tape must carry the Kaufman ER veto, got %q", candidate.RejectionReason)
	}
	er, ok := candidate.ModelAssumptions["kaufmanER"].(float64)
	if !ok || er <= 0.8 {
		t.Fatalf("model_assumptions.kaufmanER must be > 0.8 on a monotone tape, got %v (%T)", candidate.ModelAssumptions["kaufmanER"], candidate.ModelAssumptions["kaufmanER"])
	}
	if label, _ := candidate.ModelAssumptions["kaufmanRegime"].(string); label != "trending" {
		t.Fatalf("kaufmanRegime must read trending, got %q", label)
	}

	// Sawtooth: quiet mean-reverting range — ER ≈ 0, no ER veto, telemetry
	// rides along on an ACCEPTED candidate. The +2/−2 square wave ends at
	// the channel midline so no Anti-FOMO band fires.
	rangeTicker := pionex.TickerInfo{
		Symbol: "TST_USDT", Open: decimal.NewFromInt(100),
		Close: decimal.NewFromInt(100),
		Amount: volume,
	}
	rangeCandles := synthCandles(func(i int) float64 {
		if i%2 == 0 {
			return 102
		}
		return 100
	}, 80)
	candidate, err = scoreCandidate(symbol, rangeTicker, volume, rangeCandles, config)
	if err != nil {
		t.Fatalf("scoreCandidate (range): %v", err)
	}
	if strings.Contains(candidate.RejectionReason, "Kaufman ER") {
		t.Fatalf("mean-reverting tape must not trip the ER veto, got %q", candidate.RejectionReason)
	}
	if candidate.Decision != "ACCEPTED" {
		t.Fatalf("sawtooth must stay accepted under the permissive config, got %s (%s)",
			candidate.Decision, candidate.RejectionReason)
	}
	er, ok = candidate.ModelAssumptions["kaufmanER"].(float64)
	if !ok || er >= KaufmanERMeanReversion {
		t.Fatalf("sawtooth must log kaufmanER < 0.30, got %v", candidate.ModelAssumptions["kaufmanER"])
	}
	if label, _ := candidate.ModelAssumptions["kaufmanRegime"].(string); label != "mean_reversion" {
		t.Fatalf("kaufmanRegime must read mean_reversion, got %q", label)
	}
}
