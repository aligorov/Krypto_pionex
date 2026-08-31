package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchCoinGeckoParsesBothEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/simple/price":
			_, _ = w.Write([]byte(`{"bitcoin":{"usd":77512.5,"usd_24h_change":-0.76}}`))
		case "/api/v3/global":
			_, _ = w.Write([]byte(`{"data":{"market_cap_percentage":{"btc":58.31},"total_market_cap":{"usd":2410000000000},"market_cap_change_percentage_24h_usd":-1.2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.CoinGeckoBaseURL = srv.URL
	cfg.MinRequestInterval = 0
	collector := NewCollectorWithConfig(nil, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := collector.fetchCoinGecko(ctx)
	if err != nil {
		t.Fatalf("fetchCoinGecko: %v", err)
	}
	if !almostEqual(snap.BTCUSD, 77512.5) || !almostEqual(snap.BTC24hPct, -0.76) {
		t.Fatalf("price context wrong: %+v", snap)
	}
	if !almostEqual(snap.BTCDominancePct, 58.31) {
		t.Fatalf("dominance wrong: %v", snap.BTCDominancePct)
	}
	if !almostEqual(snap.TotalMCapUSD, 2.41e12) || !almostEqual(snap.MCap24hPct, -1.2) {
		t.Fatalf("mcap wrong: %+v", snap)
	}
}

// /global failing must degrade to a price-only snapshot, not an error — the
// dominance gate then simply stays unarmed until history returns.
func TestFetchCoinGeckoGlobalAdvisory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/simple/price" {
			_, _ = w.Write([]byte(`{"bitcoin":{"usd":79000,"usd_24h_change":1.2}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.CoinGeckoBaseURL = srv.URL
	cfg.MinRequestInterval = 0
	collector := NewCollectorWithConfig(nil, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := collector.fetchCoinGecko(ctx)
	if err != nil {
		t.Fatalf("fetchCoinGecko: %v", err)
	}
	if snap.BTCDominancePct != 0 || snap.TotalMCapUSD != 0 {
		t.Fatalf("global fields must stay zero on failure: %+v", snap)
	}
	if !almostEqual(snap.BTCUSD, 79000) {
		t.Fatalf("price wrong: %v", snap.BTCUSD)
	}
}

func TestFetchCoinGeckoEmptyPriceFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bitcoin":{}}`))
	}))
	defer srv.Close()

	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.CoinGeckoBaseURL = srv.URL
	cfg.MinRequestInterval = 0
	collector := NewCollectorWithConfig(nil, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := collector.fetchCoinGecko(ctx); err == nil {
		t.Fatal("expected error on empty price payload")
	}
}
