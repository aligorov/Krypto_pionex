package marketdata

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

type mockMarketClient struct {
	symbols []pionex.SymbolInfo
	tickers []pionex.TickerInfo
	klines  map[string][]pionex.KlineCandle
}

func (m *mockMarketClient) GetMarketSymbols(ctx context.Context, symbolType string) ([]pionex.SymbolInfo, error) {
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

func TestParkinsonVolatility(t *testing.T) {
	candles := synthCandles(func(i int) float64 { return 100 + math.Sin(float64(i)) }, 50)
	vol := parkinsonVolatility(candles, 24)
	if vol <= 0 {
		t.Fatalf("expected positive parkinson volatility, got %f", vol)
	}
}

func TestScannerMultiTierPipeline(t *testing.T) {
	symbols := []pionex.SymbolInfo{
		{Symbol: "BTC_USDT", BaseCurrency: "BTC", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "ETH_USDT", BaseCurrency: "ETH", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "SOL_USDT", BaseCurrency: "SOL", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "PUMP_USDT", BaseCurrency: "PUMP", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
	}

	tickers := []pionex.TickerInfo{
		{Symbol: "BTC_USDT", Open: decimal.NewFromFloat(60000), Close: decimal.NewFromFloat(60500), Amount: decimal.NewFromFloat(10000000)},
		{Symbol: "ETH_USDT", Open: decimal.NewFromFloat(3000), Close: decimal.NewFromFloat(3010), Amount: decimal.NewFromFloat(5000000)},
		{Symbol: "SOL_USDT", Open: decimal.NewFromFloat(150), Close: decimal.NewFromFloat(152), Amount: decimal.NewFromFloat(2000000)},
		// Extreme pump outlier (> 50% 24h change) should be filtered out at L1
		{Symbol: "PUMP_USDT", Open: decimal.NewFromFloat(1), Close: decimal.NewFromFloat(2.5), Amount: decimal.NewFromFloat(8000000)},
	}

	klines := map[string][]pionex.KlineCandle{
		"BTC_USDT": synthCandles(func(i int) float64 { return 60000 + 500*math.Sin(float64(i)/3) }, 80),
		"ETH_USDT": synthCandles(func(i int) float64 { return 3000 + 40*math.Sin(float64(i)/4) }, 80),
		"SOL_USDT": synthCandles(func(i int) float64 { return 150 + 3*math.Sin(float64(i)/2) }, 80),
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
		FeeBps:              5.0,
		SlippageBps:         5.0,
		BaseLeverage:        2,
		AdaptiveLeverage:    true,
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
		if c.Symbol == "PUMP_USDT" {
			t.Fatal("PUMP_USDT should have been excluded by L1 fast filter")
		}
	}

	// Verify that candidates have computed Choppiness and Parkinson metrics
	for _, c := range candidates {
		if c.Decision == "ACCEPTED" {
			if c.Score <= 0 {
				t.Fatalf("expected positive score for accepted candidate %s, got %f", c.Symbol, c.Score)
			}
			if c.GridNum <= 0 {
				t.Fatalf("expected positive grid count for %s, got %d", c.Symbol, c.GridNum)
			}
		}
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
	_, pf := winRateAndProfitFactor(values)
	if pf > 99 || pf < 0 {
		t.Fatalf("profit factor must clamp to [0,99], got %v", pf)
	}
	if pf != 99 {
		t.Fatalf("epsilon-loser should saturate PF at 99, got %v", pf)
	}
	// Normal case unaffected.
	_, pf = winRateAndProfitFactor([]float64{0.02, -0.01, 0.03, -0.02})
	if pf < 1.66 || pf > 1.67 {
		t.Fatalf("normal PF must be 0.05/0.03 ≈ 1.667, got %v", pf)
	}
}
