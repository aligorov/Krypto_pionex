package marketdata

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

type mockMarketClient struct {
	symbols    []pionex.SymbolInfo
	symbolsErr error
	tickers    []pionex.TickerInfo
	klines     map[string][]pionex.KlineCandle
}

func (m *mockMarketClient) GetMarketSymbols(ctx context.Context, symbolType string) ([]pionex.SymbolInfo, error) {
	if m.symbolsErr != nil {
		return nil, m.symbolsErr
	}
	return m.symbols, nil
}

func (m *mockMarketClient) GetTickers(ctx context.Context, symbol, symbolType string) ([]pionex.TickerInfo, error) {
	return m.tickers, nil
}

func (m *mockMarketClient) GetKlines(ctx context.Context, symbol, interval string, limit int) ([]pionex.KlineCandle, error) {
	if candles, ok := m.klines[symbol]; ok {
		return candles, nil
	}
	// Fallback generated candles
	return synthCandles(func(i int) float64 { return 100 + 2*math.Sin(float64(i)/3) }, limit), nil
}

func squareWave(base, amplitude float64) func(i int) float64 {
	return func(i int) float64 {
		if i%2 == 0 {
			return base + amplitude
		}
		return base - amplitude
	}
}

func TestParkinsonVolatility(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 + math.Sin(float64(i)) }, 50)
	vol := parkinsonVolatility(candles, 24)
	if vol <= 0 {
		t.Fatalf("expected positive parkinson volatility, got %f", vol)
	}
}

func TestScannerMultiTierPipeline(t *testing.T) {
	symbols := []pionex.SymbolInfo{
		{Symbol: "BTC_USDT_PERP", BaseCurrency: "BTC", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "ETH_USDT_PERP", BaseCurrency: "ETH", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "SOL_USDT_PERP", BaseCurrency: "SOL", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "PUMP_USDT_PERP", BaseCurrency: "PUMP", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
	}

	tickers := []pionex.TickerInfo{
		{Symbol: "BTC_USDT_PERP", Open: decimal.NewFromFloat(60000), Close: decimal.NewFromFloat(60500), Amount: decimal.NewFromFloat(10000000)},
		{Symbol: "ETH_USDT_PERP", Open: decimal.NewFromFloat(3000), Close: decimal.NewFromFloat(3010), Amount: decimal.NewFromFloat(5000000)},
		{Symbol: "SOL_USDT_PERP", Open: decimal.NewFromFloat(150), Close: decimal.NewFromFloat(152), Amount: decimal.NewFromFloat(2000000)},
		// Extreme pump outlier (> 50% 24h change) should be filtered out at L1
		{Symbol: "PUMP_USDT_PERP", Open: decimal.NewFromFloat(1), Close: decimal.NewFromFloat(2.5), Amount: decimal.NewFromFloat(8000000)},
	}

	// Alternating ±amplitude square waves (wicks from synthCandles keep the
	// last close ~25-35% into the channel and RSI mid-band): the accept
	// branch of this pipeline test stays live in the v2.0.89-A world where
	// realistic fees reject these synthetic grids (exact fee-gate numbers
	// are pinned in fee_gate_test.go).
	klines := map[string][]pionex.KlineCandle{
		"BTC_USDT_PERP": synthCandles(squareWave(60000, 250), 80),
		"ETH_USDT_PERP": synthCandles(squareWave(3000, 40), 80),
		"SOL_USDT_PERP": synthCandles(squareWave(150, 3), 80),
	}

	mock := &mockMarketClient{symbols: symbols, tickers: tickers, klines: klines}
	scanner := NewScanner(mock)

	config := ScanConfig{
		Interval:            "60M",
		LookbackCandles:     60,
		MaxSymbols:          10,
		MinVolume24h:        decimal.NewFromInt(100000),
		MinVolatilityPct:    0.5,
		MaxVolatilityPct:    30.0,
		MinExpectedValuePct: 0.0,
		MinSharpe:           0.1,
		MaxDrawdownPct:      25.0,
		MinProfitFactor:     1.0,
		// Zero friction keeps this PIPELINE test on the accept path: the
		// v2.0.89-A fee-gate (2× round-trip, see fee_gate_test.go) would
		// rightly reject these synthetic ~0.1-0.25%-step sawtooth grids at
		// realistic 5/5 bps — its exact numbers are pinned in the dedicated
		// fee-gate tests, not here.
		FeeBps:           0.0,
		SlippageBps:      0.0,
		BaseLeverage:     2,
		AdaptiveLeverage: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	candidates, err := scanner.ScanMarkets(ctx, config)
	if err != nil {
		t.Fatalf("ScanMarkets failed: %v", err)
	}

	if len(candidates) == 0 {
		t.Fatal("expected candidates, got 0")
	}

	// Verify that anomalous pump symbol was excluded by L1 screener
	for _, c := range candidates {
		if c.Symbol == "PUMP_USDT_PERP" {
			t.Fatal("PUMP_USDT_PERP should have been excluded by L1 fast filter")
		}
	}

	// Verify that candidates have computed Choppiness and Parkinson metrics
	acceptedCount := 0
	for _, c := range candidates {
		if c.Decision == "ACCEPTED" {
			acceptedCount++
			if c.Score <= 0 {
				t.Fatalf("expected positive score for accepted candidate %s, got %f", c.Symbol, c.Score)
			}
			if c.GridNum <= 0 {
				t.Fatalf("expected positive grid count for %s, got %d", c.Symbol, c.GridNum)
			}
		}
	}
	// Non-vacuous accept branch: the zero-friction config must keep at
	// least one candidate ACCEPTED (2026-09 probe: the old sine patterns
	// ended at channel extremes and silently emptied this loop).
	if acceptedCount == 0 {
		t.Fatal("expected at least one ACCEPTED candidate under the frictionless config")
	}
}

func TestScannerMajorSymbolPriority(t *testing.T) {
	if !isMajorSymbol("BTC", "BTC_USDT_PERP") {
		t.Fatal("expected BTC to be recognized as major symbol")
	}
	if !isMajorSymbol("ETH", "ETH_USDT_PERP") {
		t.Fatal("expected ETH to be recognized as major symbol")
	}
	if !isMajorSymbol("SOL", "SOL_USDT_PERP") {
		t.Fatal("expected SOL to be recognized as major symbol")
	}
	if isMajorSymbol("TUT", "TUT_USDT_PERP") {
		t.Fatal("expected TUT NOT to be recognized as major symbol")
	}
}

// v2.0.27: exact base match only — the old full-symbol prefix fallback
// matched SOLVX/ETHW-class tickers as SOL/ETH.
func TestScannerMajorSymbolNoPrefixFalsePositives(t *testing.T) {
	for _, tc := range [][2]string{
		{"SOLV", "SOLVX_USDT_PERP"},
		{"ETHW", "ETHW_USDT_PERP"},
		{"BTCA", "BTCA_USDT_PERP"},
	} {
		if isMajorSymbol(tc[0], tc[1]) {
			t.Fatalf("isMajorSymbol(%q, %q) must be false — prefix false positive", tc[0], tc[1])
		}
	}
}

// v2.0.29 regression: profit factor must stay inside the persistence bound —
// a near-zero negative-return sum used to explode positive/negative past
// NUMERIC(12,6) and kill the whole scan persist (prod COHRX 12:40Z
// 2026-08-21, SQLSTATE 22003, two scheduled scans FAILED).
func TestWinRateProfitFactorClamped(t *testing.T) {
	// Two winning candles and one epsilon-loser: PF would be ~1e9 unclamped.
	values := []float64{0.01, 0.02, -1e-12}
	_, pf, pfValid, _ := winRateAndProfitFactor(values)
	if !pfValid {
		t.Fatalf("epsilon-loser has negative return so pfValid must be true")
	}
	if pf > 99 || pf < 0 {
		t.Fatalf("profit factor must clamp to [0,99], got %v", pf)
	}
	if pf != 99 {
		t.Fatalf("epsilon-loser should saturate PF at 99, got %v", pf)
	}
	// Zero losing steps case: cannot return fake 99 or mark valid
	_, zeroLossPf, zeroLossPfValid, _ := winRateAndProfitFactor([]float64{0.01, 0.02, 0.03})
	if zeroLossPfValid || zeroLossPf != 0.0 {
		t.Fatalf("zero losing steps must report pfValid=false and pf=0, got valid=%v pf=%v", zeroLossPfValid, zeroLossPf)
	}
	// Zero downside for Sortino:
	sortino, sortinoValid := ratio([]float64{0.01, 0.02, 0.03}, true, 365)
	if sortinoValid || sortino != 0.0 {
		t.Fatalf("zero downside returns must report sortinoValid=false and sortino=0 (not 4.0), got valid=%v sortino=%v", sortinoValid, sortino)
	}
	// Normal case unaffected.
	_, pf, pfValid, _ = winRateAndProfitFactor([]float64{0.02, -0.01, 0.03, -0.02})
	if !pfValid || pf < 1.66 || pf > 1.67 {
		t.Fatalf("normal PF must be 0.05/0.03 ≈ 1.667, got %v", pf)
	}
}

func TestLedgerAuditedEntryZones(t *testing.T) {
	// Semi-trend dead zone: neutral blocked at ADX 24-32 only.
	if !neutralSemiTrendBlocked("no_trend", 28) {
		t.Fatal("ADX 28 neutral must be blocked")
	}
	if neutralSemiTrendBlocked("no_trend", 23) || neutralSemiTrendBlocked("no_trend", 33) {
		t.Fatal("below 24 and above 32 belong to the classic vetoes")
	}
	if neutralSemiTrendBlocked("long", 28) || neutralSemiTrendBlocked("short", 28) {
		t.Fatal("semi-trend veto is NEUTRAL-only")
	}
	// v2.0.162: the mature-trend LONG demotion (ADX ≥28 → no_trend) is
	// REMOVED by operator directive — aged strong trends now feed the LONG
	// grid; the walk-forward gate and the 14-day cohort partition carry
	// the validation load.
}

func TestNeutralSqueezeRisk(t *testing.T) {
	if neutralSqueezeRisk(RegimeResult{IsSqueeze: true, ADX: 18, Choppiness: 60}) {
		t.Fatal("calm, choppy squeeze must not be a hard breakout veto")
	}
	if !neutralSqueezeRisk(RegimeResult{IsSqueeze: true, ADX: 24, Choppiness: 60}) {
		t.Fatal("stronger ADX squeeze must remain a breakout risk")
	}
	if !neutralSqueezeRisk(RegimeResult{IsSqueeze: true, ADX: 18, Choppiness: 45}) {
		t.Fatal("low-choppiness squeeze must remain a breakout risk")
	}
}

// --- Wave B ("2-5% in 24-72h" plan): universe, volatile quota, separate
// per-direction ranking. ---

func TestTickerRangePct(t *testing.T) {
	ticker := pionex.TickerInfo{
		Open:  decimal.NewFromFloat(100),
		High:  decimal.NewFromFloat(107),
		Low:   decimal.NewFromFloat(102),
		Close: decimal.NewFromFloat(104),
	}
	if rp := tickerRangePct(ticker); rp < 4.99 || rp > 5.01 {
		t.Fatalf("expected (107-102)/100 = 5%% range, got %v", rp)
	}
	// No usable High/Low (mock-style tickers): not quota-eligible, not an error.
	if rp := tickerRangePct(pionex.TickerInfo{Open: decimal.NewFromFloat(100), Close: decimal.NewFromFloat(101)}); rp != 0 {
		t.Fatalf("ticker without High/Low must report range 0, got %v", rp)
	}
}

func TestVolatilityQuotaNoDuplicatesNoLoss(t *testing.T) {
	item := func(symbol string, amount float64, open, high, low float64) rankedSymbol {
		return rankedSymbol{
			symbol: pionex.SymbolInfo{Symbol: symbol, QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
			ticker: pionex.TickerInfo{
				Open:   decimal.NewFromFloat(open),
				High:   decimal.NewFromFloat(high),
				Low:    decimal.NewFromFloat(low),
				Close:  decimal.NewFromFloat(open),
				Amount: decimal.NewFromFloat(amount),
			},
			amount: decimal.NewFromFloat(amount),
		}
	}

	// Turnover-sorted list: the top 2 form the primary pre-selection; the
	// tail mixes quota-eligible movers (range 2.5-10%), a flat pair (1%),
	// and an insane pair (40%) that both must stay out.
	ranked := []rankedSymbol{
		item("TOP1_USDT_PERP", 1_000_000, 100, 101, 99),
		item("TOP2_USDT_PERP", 900_000, 100, 101, 99),
		item("MOVE1_USDT_PERP", 500_000, 100, 105, 100),
		item("FLAT_USDT_PERP", 400_000, 100, 100.5, 100),
		item("MOVE2_USDT_PERP", 300_000, 100, 110, 104),
		item("CRAZY_USDT_PERP", 200_000, 100, 140, 90),
		item("MOVE3_USDT_PERP", 150_000, 100, 102.6, 100),
	}

	active := ranked[:2]
	rest := ranked[2:]

	got := appendVolatilityQuota(active, rest, 0 /* default quota 8 */)
	if len(got) != 5 {
		t.Fatalf("expected primary 2 + all 3 eligible movers, got %d: %v", len(got), got)
	}
	seen := make(map[string]int)
	for _, entry := range got {
		seen[entry.symbol.Symbol]++
	}
	for _, entry := range ranked {
		count := seen[entry.symbol.Symbol]
		switch entry.symbol.Symbol {
		case "TOP1_USDT_PERP", "TOP2_USDT_PERP", "MOVE1_USDT_PERP", "MOVE2_USDT_PERP", "MOVE3_USDT_PERP":
			if count != 1 {
				t.Fatalf("%s must appear exactly once, got %d", entry.symbol.Symbol, count)
			}
		default:
			// FLAT (below band) and CRAZY (above band) are not admitted but
			// never evict a primary member either.
			if count != 0 {
				t.Fatalf("%s must not be admitted by the quota, got %d", entry.symbol.Symbol, count)
			}
		}
	}

	// Quota size is honored: cap of 1 admits only the most liquid mover.
	capped := appendVolatilityQuota(active, rest, 1)
	if len(capped) != 3 || capped[2].symbol.Symbol != "MOVE1_USDT_PERP" {
		t.Fatalf("quota=1 must admit exactly MOVE1 (most liquid mover), got %+v", capped)
	}

	// Defensive dedup: an overlapping rest (primary member repeated) must not
	// duplicate the pair even if a future caller passes the wrong slice.
	overlapping := append([]rankedSymbol{active[0]}, rest...)
	deduped := appendVolatilityQuota(active, overlapping, 8)
	count := 0
	for _, entry := range deduped {
		if entry.symbol.Symbol == "TOP1_USDT_PERP" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("primary member passed again via rest must not duplicate, got %d occurrences", count)
	}
}

func TestScanMarketsVolatilityQuota(t *testing.T) {
	const totalSymbols = 35
	symbols := make([]pionex.SymbolInfo, 0, totalSymbols)
	tickers := make([]pionex.TickerInfo, 0, totalSymbols)
	for i := 0; i < totalSymbols; i++ {
		name := fmt.Sprintf("SYM%02d_USDT_PERP", i)
		symbols = append(symbols, pionex.SymbolInfo{
			Symbol: name, BaseCurrency: fmt.Sprintf("SYM%02d", i),
			QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true,
		})
		if i < 30 {
			// Turnover top: no High/Low → flat 24h range → never quota-eligible.
			tickers = append(tickers, pionex.TickerInfo{
				Symbol: name,
				Open:   decimal.NewFromFloat(100), Close: decimal.NewFromFloat(100),
				Amount: decimal.NewFromFloat(1_000_000 + float64(i)),
			})
		} else {
			// Turnover tail: 5% 24h range (band 2.5-10) → quota-eligible.
			tickers = append(tickers, pionex.TickerInfo{
				Symbol: name,
				Open:   decimal.NewFromFloat(100), Close: decimal.NewFromFloat(104),
				High: decimal.NewFromFloat(107), Low: decimal.NewFromFloat(102),
				Amount: decimal.NewFromFloat(500_000 + float64(i)),
			})
		}
	}

	config := ScanConfig{
		Interval: "60M", LookbackCandles: 60,
		MaxSymbols: 50, UniverseScanCap: 30,
		MinVolume24h:     decimal.NewFromInt(100_000),
		MinVolatilityPct: 0.1, MaxVolatilityPct: 40,
		MinExpectedValuePct: 0, MinSharpe: 0.1,
		MaxDrawdownPct: 60, MinProfitFactor: 0,
		FeeBps: 0, SlippageBps: 0, BaseLeverage: 2,
	}

	scanner := NewScanner(&mockMarketClient{symbols: symbols, tickers: tickers})
	candidates, err := scanner.ScanMarkets(context.Background(), config)
	if err != nil {
		t.Fatalf("ScanMarkets failed: %v", err)
	}

	// All 30 turnover-top pairs survive (nothing lost), each exactly once
	// (nothing duplicated), plus every quota-eligible mover.
	seen := make(map[string]int, len(candidates))
	for _, c := range candidates {
		seen[c.Symbol]++
	}
	if len(candidates) != totalSymbols {
		t.Fatalf("expected %d candidates (30 top + 5 quota movers), got %d", totalSymbols, len(candidates))
	}
	for i := 0; i < totalSymbols; i++ {
		name := fmt.Sprintf("SYM%02d_USDT_PERP", i)
		if seen[name] != 1 {
			t.Fatalf("%s must appear exactly once, got %d", name, seen[name])
		}
	}

	// Quota size is honored end-to-end: VolQuota=2 admits only the two most
	// liquid movers from the tail.
	config.VolQuota = 2
	scanner = NewScanner(&mockMarketClient{symbols: symbols, tickers: tickers})
	capped, err := scanner.ScanMarkets(context.Background(), config)
	if err != nil {
		t.Fatalf("ScanMarkets (VolQuota=2) failed: %v", err)
	}
	if len(capped) != 32 {
		t.Fatalf("VolQuota=2 must yield 30 top + 2 movers = 32 candidates, got %d", len(capped))
	}
}

func TestScannerScoreReportsRangeShare(t *testing.T) {
	config := ScanConfig{
		MinVolatilityPct: 0.5, MaxVolatilityPct: 30,
		MinExpectedValuePct: 0, MinSharpe: 0.1, MaxDrawdownPct: 25, MinProfitFactor: 1,
	}
	// Choppiness 30 → chopFit clamps to 0: no range share paid.
	_, rangeShare := scannerScore(5, 0.5, 1, 5, 1.5, 30, true, true, false, config)
	if rangeShare != 0 {
		t.Fatalf("choppiness 30 must pay no range share, got %v", rangeShare)
	}
	// Choppiness 70 → chopFit = 1: the full choppiness budget of the final
	// score (0.15/1.4 ≈ 0.1071).
	_, rangeShare = scannerScore(5, 0.5, 1, 5, 1.5, 70, true, true, false, config)
	if rangeShare < 0.106 || rangeShare > 0.109 {
		t.Fatalf("choppiness 70 must pay the full range share 0.15/1.4, got %v", rangeShare)
	}
}

func TestDirectionalScoreAdjustPrefersTrendFeatures(t *testing.T) {
	// Equal neutral baselines = the equal-turnover condition of the wave B
	// plan: turnover never enters scannerScore, so with every neutral input
	// identical the ONLY ranking signal left is the directional adjustment —
	// the property the separate ranking exists for.
	const baseline = 0.60
	const rangeShare = 0.08

	// Trendy LONG: co-directional slope, confirmed ADX, mid-channel entry
	// (anti-FOMO compliant: pos 40 ≤ 75).
	trendy, ok := directionalScoreAdjust("long", rangeShare, 26, 0.8, 40)
	if !ok {
		t.Fatal("long candidate must receive a directional adjustment")
	}
	// Range-flavored LONG: counter-slope, sub-threshold ADX, channel
	// ceiling (the exact tape the old choppiness reward used to promote).
	rangey, ok := directionalScoreAdjust("long", rangeShare, 15, -0.3, 85)
	if !ok {
		t.Fatal("long candidate must receive a directional adjustment")
	}

	trendyScore := baseline - trendy.RangeShare + trendy.TrendShare
	rangeyScore := baseline - rangey.RangeShare + rangey.TrendShare
	if trendyScore <= rangeyScore {
		t.Fatalf("trendy long must outrank range-y long: %v vs %v (fits %.3f vs %.3f)",
			trendyScore, rangeyScore, trendy.TrendFit, rangey.TrendFit)
	}
	if trendy.RangeShare != rangeShare || rangey.RangeShare != rangeShare {
		t.Fatal("the adjustment must remove exactly the baseline range share")
	}

	// Neutral candidates keep the untouched baseline ranking.
	if _, ok := directionalScoreAdjust("no_trend", rangeShare, 26, 0.8, 40); ok {
		t.Fatal("neutral candidates must not receive a directional adjustment")
	}

	// SHORT mirror: co-directional (falling) slope and channel-floor entry
	// compliance score the same as the LONG mirror.
	shortTrendy, _ := directionalScoreAdjust("short", rangeShare, 26, -0.8, 60)
	if shortTrendy.TrendFit != trendy.TrendFit {
		t.Fatalf("short mirror must score identically: %.3f vs %.3f", shortTrendy.TrendFit, trendy.TrendFit)
	}
}

// TestScoreCandidateSwapsRangeShareForTrendShare pins the end-to-end effect
// of the wave B separate ranking inside the real scoring pipeline: two LONG
// candidates at EQUAL turnover — one carrying genuine trend voices (clean
// rally, low choppiness), one whose old score was paid almost entirely for
// range shape (choppy mild drift). The directional adjustment must DROP the
// choppiness share the range-y tape was riding and ADD trend relevance to
// the clean rally, narrowing the gap toward per-direction relevance. Gates
// and decisions stay untouched; with equal neutral baselines the ordering
// itself flips — that property is pinned deterministically in
// TestDirectionalScoreAdjustPrefersTrendFeatures.
func TestScoreCandidateSwapsRangeShareForTrendShare(t *testing.T) {
	config := ScanConfig{
		Interval: "60M", LookbackCandles: 60,
		MinVolume24h:     decimal.NewFromInt(100_000),
		MinVolatilityPct: 0.1, MaxVolatilityPct: 40,
		MinExpectedValuePct: 0, MinSharpe: -10,
		MaxDrawdownPct: 100, MinProfitFactor: 0,
		FeeBps: 0, SlippageBps: 0, BaseLeverage: 2,
		NotionalPerBot: 200,
	}

	mk := func(symbol string, pattern func(i int) float64) (pionex.SymbolInfo, pionex.TickerInfo, []pionex.KlineCandle) {
		info := pionex.SymbolInfo{
			Symbol: symbol, BaseCurrency: strings.TrimSuffix(symbol, "_USDT_PERP"),
			QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true,
		}
		ticker := pionex.TickerInfo{
			Symbol: symbol,
			Open:   decimal.NewFromFloat(100), Close: decimal.NewFromFloat(102),
			Amount: decimal.NewFromInt(2_000_000),
		}
		return info, ticker, synthCandles(pattern, 80)
	}

	// Trend-y LONG: a clean geometric rally — stacked EMAs, steep slope,
	// maximal ADX, choppiness 15 (no range share to drop).
	trendyInfo, trendyTicker, trendyCandles := mk(
		"TRENDY_USDT_PERP",
		func(i int) float64 { return 100 * math.Pow(1.02, float64(i)) * (1 + 0.012*math.Sin(float64(i)/1.5)) },
	)
	// Range-y LONG: a mild, choppy drift that still stacks EMAs — the exact
	// tape the old choppiness reward promoted inside the LONG direction
	// (choppiness ~80 → the full range share).
	rangeyInfo, rangeyTicker, rangeyCandles := mk(
		"RANGEY_USDT_PERP",
		func(i int) float64 { return 100 * math.Pow(1.001, float64(i)) * (1 + 0.002*math.Sin(float64(i)/1.5)) },
	)

	turnover := decimal.NewFromInt(2_000_000) // equal turnover by construction
	trendy, err := scoreCandidate(trendyInfo, trendyTicker, turnover, trendyCandles, config)
	if err != nil {
		t.Fatalf("score trendy: %v", err)
	}
	rangey, err := scoreCandidate(rangeyInfo, rangeyTicker, turnover, rangeyCandles, config)
	if err != nil {
		t.Fatalf("score rangey: %v", err)
	}

	if trendy.RecommendedTrend != "long" || rangey.RecommendedTrend != "long" {
		t.Fatalf("precondition: both candidates must be LONG, got %q and %q",
			trendy.RecommendedTrend, rangey.RecommendedTrend)
	}

	trendyFit, _ := trendy.ModelAssumptions["directionalTrendFit"].(float64)
	trendyRange, _ := trendy.ModelAssumptions["directionalRangeShare"].(float64)
	rangeyFit, _ := rangey.ModelAssumptions["directionalTrendFit"].(float64)
	rangeyRange, _ := rangey.ModelAssumptions["directionalRangeShare"].(float64)

	// The range-y long must LOSE its choppiness share (dropped range bonus
	// exceeds the trend share its weak voices earn back).
	if rangeyRange <= 0 {
		t.Fatal("range-y long must carry a positive dropped range share")
	}
	if delta := rangeyFit*scannerRangeWeight/scannerScoreNorm - rangeyRange; delta >= 0 {
		t.Fatalf("range-y long must net-lose from the swap, got delta %+.4f (fit %.3f, rangeShare %.4f)",
			delta, rangeyFit, rangeyRange)
	}
	// The clean-trend long must GAIN (no range share to drop, full trend
	// share added).
	if trendyRange != 0 {
		t.Fatalf("clean-trend long must have no range share to drop, got %.4f", trendyRange)
	}
	if trendyFit <= rangeyFit {
		t.Fatalf("trend fit must favor the clean rally: %.3f vs %.3f", trendyFit, rangeyFit)
	}

	// The swap must move the two candidates toward per-direction relevance:
	// reconstruct the pre-adjustment (old single-metric) scores and verify
	// the ranking gap between the range-shaped long and the trend-shaped
	// long NARROWS.
	oldGap := (rangey.Score + rangeyRange - rangeyFit*scannerRangeWeight/scannerScoreNorm) -
		(trendy.Score + trendyRange - trendyFit*scannerRangeWeight/scannerScoreNorm)
	newGap := rangey.Score - trendy.Score
	if newGap >= oldGap {
		t.Fatalf("separate ranking must narrow the range-vs-trend gap inside LONG: old %.4f, new %.4f", oldGap, newGap)
	}
}
