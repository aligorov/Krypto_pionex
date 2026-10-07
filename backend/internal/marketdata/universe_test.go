package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

func TestFilterTradableUSDTPerps(t *testing.T) {
	symbols := []pionex.SymbolInfo{
		// Keeper: full PERP form, USDT quote, trading, ASCII.
		{Symbol: "BTC_USDT_PERP", BaseCurrency: "BTC", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		// Drop: not a PERP type (spot listing).
		{Symbol: "ETH_USDT", BaseCurrency: "ETH", QuoteCurrency: "USDT", Type: "SPOT", Status: "TRADING", Enabled: true},
		// Drop: non-USDT quote.
		{Symbol: "BTC_USDC_PERP", BaseCurrency: "BTC", QuoteCurrency: "USDC", Type: "PERP", Status: "TRADING", Enabled: true},
		// Drop: PERP type but missing the _PERP trading-form suffix.
		{Symbol: "SOL_USDT", BaseCurrency: "SOL", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		// Drop: halted (neither Enabled nor TRADING status).
		{Symbol: "ZAMA_USDT_PERP", BaseCurrency: "ZAMA", QuoteCurrency: "USDT", Type: "PERP", Status: "HALTED"},
		// Drop: CJK meme ticker the futuresGrid create endpoint refuses.
		{Symbol: "牛来_USDT_PERP", BaseCurrency: "牛来", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
	}

	got := FilterTradableUSDTPerps(symbols)
	if len(got) != 1 || got[0].Symbol != "BTC_USDT_PERP" {
		t.Fatalf("expected exactly BTC_USDT_PERP to survive the universe filters, got %+v", got)
	}
}

func TestCachedUniverseTTLAndLoudFailure(t *testing.T) {
	mock := &mockMarketClient{symbols: []pionex.SymbolInfo{
		{Symbol: "BTC_USDT_PERP", BaseCurrency: "BTC", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
	}}
	universe := NewCachedUniverse(mock)

	clock := time.Unix(1_700_000_000, 0)
	universe.now = func() time.Time { return clock }

	// First call fetches.
	first, err := universe.TradableUSDTPerps(context.Background())
	if err != nil {
		t.Fatalf("first fetch failed: %v", err)
	}
	if len(first) != 1 || first[0].Symbol != "BTC_USDT_PERP" {
		t.Fatalf("unexpected universe: %+v", first)
	}

	// Mutating the returned slice must not touch the cache.
	first[0].Symbol = "MUTATED_USDT_PERP"

	// Inside the TTL the cache serves without refetching.
	clock = clock.Add(UniverseCacheTTL - time.Second)
	second, err := universe.TradableUSDTPerps(context.Background())
	if err != nil {
		t.Fatalf("cached fetch failed: %v", err)
	}
	if len(second) != 1 || second[0].Symbol != "BTC_USDT_PERP" {
		t.Fatalf("cache must be isolated from caller mutations, got %+v", second)
	}

	// Beyond the TTL the feed is refetched and a failure degrades LOUDLY:
	// the stale universe is never served as fresh.
	clock = clock.Add(2 * time.Second)
	mock.symbolsErr = errors.New("endpoint down")
	if _, err := universe.TradableUSDTPerps(context.Background()); err == nil {
		t.Fatal("a dead universe feed beyond the TTL must return an error, not stale data")
	}

	// Recovery: the next call refetches successfully.
	mock.symbolsErr = nil
	clock = clock.Add(2 * time.Second)
	mock.symbols = []pionex.SymbolInfo{
		{Symbol: "BTC_USDT_PERP", BaseCurrency: "BTC", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
		{Symbol: "ETH_USDT_PERP", BaseCurrency: "ETH", QuoteCurrency: "USDT", Type: "PERP", Status: "TRADING", Enabled: true},
	}
	recovered, err := universe.TradableUSDTPerps(context.Background())
	if err != nil {
		t.Fatalf("recovered fetch failed: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("expected the refetched 2-symbol universe, got %+v", recovered)
	}
}
