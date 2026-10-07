package marketdata

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// UniverseCacheTTL bounds how long one /api/v1/common/symbols fetch serves
// the scanner. The Pionex PERP listing set changes on the scale of days
// (new listings, TRADING status flips); the endpoint itself is one public
// weight-5 call covering ~400 symbols. A 10-minute memo stops every
// scheduled scan from re-paying it while never hiding a listing change for
// long. Plain market data only — no DB persistence attached (zero-ENV
// runtime policy is untouched).
const UniverseCacheTTL = 10 * time.Minute

// UniverseProvider hands the scanner the tradable Pionex USDT perpetual
// universe in Pionex form (BTC_USDT_PERP). Pionex stays the single source
// of truth for the symbol set (AGENTS.md rule 1); nothing else may add or
// remove members.
type UniverseProvider interface {
	// TradableUSDTPerps returns every currently tradable USDT PERP,
	// deterministically sorted by symbol.
	TradableUSDTPerps(ctx context.Context) ([]pionex.SymbolInfo, error)
}

// CachedUniverse implements UniverseProvider over the official Pionex
// /api/v1/common/symbols endpoint (via MarketClient) with an in-memory TTL
// cache. The fetch runs under the mutex so concurrent callers share one
// round-trip instead of stampeding the endpoint; the TTL makes that lock
// cheap. A feed that fails beyond the TTL degrades loudly — the error
// propagates and the scan fails — stale data is never served as fresh.
type CachedUniverse struct {
	client MarketClient
	now    func() time.Time

	mu        sync.Mutex
	fetchedAt time.Time
	symbols   []pionex.SymbolInfo
}

// NewCachedUniverse wraps a Pionex market client as a cached UniverseProvider.
func NewCachedUniverse(client MarketClient) *CachedUniverse {
	return &CachedUniverse{client: client, now: time.Now}
}

// TradableUSDTPerps serves the cached universe inside the TTL and refetches
// outside it. The returned slice is a copy: callers cannot mutate the cache.
func (u *CachedUniverse) TradableUSDTPerps(ctx context.Context) ([]pionex.SymbolInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.symbols != nil && u.now().Sub(u.fetchedAt) < UniverseCacheTTL {
		return append([]pionex.SymbolInfo(nil), u.symbols...), nil
	}
	raw, err := u.client.GetMarketSymbols(ctx, "PERP")
	if err != nil {
		return nil, err
	}
	filtered := FilterTradableUSDTPerps(raw)
	if len(filtered) == 0 {
		return nil, errors.New("pionex PERP universe empty after tradable-USDT-PERP filters")
	}
	u.symbols = filtered
	u.fetchedAt = u.now()
	return append([]pionex.SymbolInfo(nil), filtered...), nil
}

// FilterTradableUSDTPerps applies the universe membership rules in one
// place:
//
//   - Type PERP from the official symbols endpoint contract,
//   - USDT quote currency,
//   - the _PERP suffix of the Pionex trading form (BTC_USDT_PERP),
//   - TRADING status (Enabled flag or status == "TRADING"),
//   - pure printable-ASCII ticker — Pionex lists CJK meme pairs (牛来, 龙虾…)
//     that the futuresGrid create endpoint refuses outright
//     (P_TRADING_BOT_INVALID_ARGUMENT); scanner.scoreCandidate keeps the
//     same check as a defense-in-depth rejection reason.
func FilterTradableUSDTPerps(symbols []pionex.SymbolInfo) []pionex.SymbolInfo {
	filtered := make([]pionex.SymbolInfo, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol.Type != "PERP" ||
			symbol.QuoteCurrency != "USDT" ||
			!strings.HasSuffix(symbol.Symbol, PerpSuffix) ||
			!symbol.IsTrading() ||
			!isASCIISymbol(symbol.Symbol) {
			continue
		}
		filtered = append(filtered, symbol)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Symbol < filtered[j].Symbol })
	return filtered
}
