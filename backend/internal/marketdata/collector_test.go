package marketdata

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestCollector builds a Collector wired to mock exchange servers with
// rate limiting disabled and no database (only fetch/parse paths are used).
func newTestCollector(t *testing.T, binance, bybit, okx string) *Collector {
	t.Helper()
	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.BinanceBaseURL = binance
	cfg.BybitBaseURL = bybit
	cfg.OKXBaseURL = okx
	cfg.HTTPTimeout = 5 * time.Second
	cfg.MinRequestInterval = 0
	return NewCollectorWithConfig(nil, cfg)
}

func almostEqual(got, want float64) bool {
	return math.Abs(got-want) <= 1e-12
}

func TestToBinanceSymbol(t *testing.T) {
	cases := []struct{ in, want string }{
		{"BTC_USDT_PERP", "BTCUSDT"},
		{"ETH_USDT_PERP", "ETHUSDT"},
		{"1000PEPE_USDT_PERP", "1000PEPEUSDT"},
		{"BTC_USDT", "BTCUSDT"}, // spot form is accepted too
		{" SOL_USDT_PERP ", "SOLUSDT"},
	}
	for _, tc := range cases {
		if got := ToBinanceSymbol(tc.in); got != tc.want {
			t.Errorf("ToBinanceSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFromBinanceSymbol(t *testing.T) {
	cases := []struct{ in, want string }{
		{"BTCUSDT", "BTC_USDT_PERP"},
		{"ETHUSDT", "ETH_USDT_PERP"},
		{"1000PEPEUSDT", "1000PEPE_USDT_PERP"},
		{"BTCUSDC", "BTC_USDC_PERP"},
		{"BTCUSD", "BTC_USD_PERP"},
	}
	for _, tc := range cases {
		if got := FromBinanceSymbol(tc.in); got != tc.want {
			t.Errorf("FromBinanceSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSymbolRoundTrip(t *testing.T) {
	for _, pionex := range []string{"BTC_USDT_PERP", "ETH_USDT_PERP", "1000PEPE_USDT_PERP"} {
		if got := FromBinanceSymbol(ToBinanceSymbol(pionex)); got != pionex {
			t.Errorf("round trip for %q produced %q", pionex, got)
		}
	}
}

func TestToOKXSymbol(t *testing.T) {
	cases := []struct{ in, want string }{
		{"BTC_USDT_PERP", "BTC-USDT-SWAP"},
		{"ETH_USDT_PERP", "ETH-USDT-SWAP"},
		{"1000PEPE_USDT_PERP", "1000PEPE-USDT-SWAP"},
	}
	for _, tc := range cases {
		if got := ToOKXSymbol(tc.in); got != tc.want {
			t.Errorf("ToOKXSymbol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFetchBinancePremiumIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/premiumIndex" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("symbol"); got != "BTCUSDT" {
			http.Error(w, "unexpected symbol "+got, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lastFundingRate":"0.0001","markPrice":"50000","indexPrice":"49999"}`))
	}))
	defer server.Close()

	collector := newTestCollector(t, server.URL, "", "")
	sample, err := collector.fetchBinancePremiumIndex(context.Background(), "BTC_USDT_PERP")
	if err != nil {
		t.Fatalf("fetchBinancePremiumIndex: %v", err)
	}
	if sample.Symbol != "BTC_USDT_PERP" {
		t.Errorf("symbol = %q, want BTC_USDT_PERP", sample.Symbol)
	}
	if sample.Exchange != ExchangeBinance {
		t.Errorf("exchange = %q, want %q", sample.Exchange, ExchangeBinance)
	}
	if !almostEqual(sample.Rate, 0.0001) {
		t.Errorf("rate = %v, want 0.0001", sample.Rate)
	}
	if !almostEqual(sample.MarkPrice, 50000) {
		t.Errorf("markPrice = %v, want 50000", sample.MarkPrice)
	}
}

func TestFetchBybitTicker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/market/tickers" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("category") != "linear" || query.Get("symbol") != "BTCUSDT" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"retCode":0,"result":{"list":[{"fundingRate":"0.0001","markPrice":"50000","openInterest":"1000"}]}}`))
	}))
	defer server.Close()

	collector := newTestCollector(t, "", server.URL, "")
	snapshot, err := collector.fetchBybitTicker(context.Background(), "BTC_USDT_PERP")
	if err != nil {
		t.Fatalf("fetchBybitTicker: %v", err)
	}
	if !almostEqual(snapshot.FundingRate, 0.0001) {
		t.Errorf("fundingRate = %v, want 0.0001", snapshot.FundingRate)
	}
	if !almostEqual(snapshot.MarkPrice, 50000) {
		t.Errorf("markPrice = %v, want 50000", snapshot.MarkPrice)
	}
	if !almostEqual(snapshot.OpenInterest, 1000) {
		t.Errorf("openInterest = %v, want 1000", snapshot.OpenInterest)
	}
}

func TestFetchBybitTickerEmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"retCode":0,"result":{"list":[]}}`))
	}))
	defer server.Close()

	collector := newTestCollector(t, "", server.URL, "")
	if _, err := collector.fetchBybitTicker(context.Background(), "BTC_USDT_PERP"); err == nil {
		t.Fatal("expected error for empty bybit list, got nil")
	}
}

func TestFetchOKXFunding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/public/funding-rate" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("instId"); got != "BTC-USDT-SWAP" {
			http.Error(w, "unexpected instId "+got, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","data":[{"fundingRate":"0.0001","markPx":"50000"}]}`))
	}))
	defer server.Close()

	collector := newTestCollector(t, "", "", server.URL)
	sample, err := collector.fetchOKXFunding(context.Background(), "BTC_USDT_PERP")
	if err != nil {
		t.Fatalf("fetchOKXFunding: %v", err)
	}
	if sample.Exchange != ExchangeOKX {
		t.Errorf("exchange = %q, want %q", sample.Exchange, ExchangeOKX)
	}
	if !almostEqual(sample.Rate, 0.0001) {
		t.Errorf("rate = %v, want 0.0001", sample.Rate)
	}
	if !almostEqual(sample.MarkPrice, 50000) {
		t.Errorf("markPrice = %v, want 50000", sample.MarkPrice)
	}
}

func TestFundingIsExtreme(t *testing.T) {
	cases := []struct {
		average float64
		want    bool
	}{
		{0.0001, false},
		{0.0005, false},
		{0.001, false}, // threshold itself is not extreme
		{0.0011, true},
		{-0.0011, true}, // absolute value
		{-0.0009, false},
		{0, false},
	}
	for _, tc := range cases {
		if got := FundingIsExtreme(tc.average); got != tc.want {
			t.Errorf("FundingIsExtreme(%v) = %v, want %v", tc.average, got, tc.want)
		}
	}
}

func TestFilterHighImpactEvents(t *testing.T) {
	feed := []forexFactoryEvent{
		{Title: "FOMC Rate Decision", Country: "usd", Date: "2026-08-18T18:00:00Z", Impact: "High"},
		{Title: "CPI m/m", Country: "USD", Date: "2026-08-19T12:30:00Z", Impact: "High"},
		{Title: "Crude Oil Inventories", Country: "USD", Date: "2026-08-19T15:00:00Z", Impact: "Medium"},
		{Title: "Broken date", Country: "USD", Date: "not-a-date", Impact: "High"},
	}
	records := filterHighImpactEvents(feed)
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Title != "FOMC Rate Decision" {
		t.Errorf("records[0].Title = %q, want FOMC Rate Decision", records[0].Title)
	}
	if records[0].Country != "USD" {
		t.Errorf("records[0].Country = %q, want normalized USD", records[0].Country)
	}
	wantTime := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	if !records[0].EventTime.Equal(wantTime) {
		t.Errorf("records[0].EventTime = %v, want %v", records[0].EventTime, wantTime)
	}
}

func TestBinanceHTTPErrorPropagates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	collector := newTestCollector(t, server.URL, "", "")
	if _, err := collector.fetchBinancePremiumIndex(context.Background(), "BTC_USDT_PERP"); err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}
