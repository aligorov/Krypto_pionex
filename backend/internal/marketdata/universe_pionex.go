package marketdata

import (
	"context"
	"sort"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// UniverseSource supplies the collector with the Pionex PERP universe ranked
// by 24h quote turnover (descending), so the head of the list is the most
// liquid set for per-symbol feeds like OKX.
type UniverseSource interface {
	RankedPerpSymbols(ctx context.Context) ([]string, error)
}

// PionexUniverse adapts the public (unauthenticated) Pionex market endpoints
// to UniverseSource. Pionex stays the source of truth for the symbol
// universe; Binance/Bybit/OKX are only filtered against it.
type PionexUniverse struct {
	client *pionex.Client
}

// NewPionexUniverse wraps a public Pionex client as a UniverseSource.
func NewPionexUniverse(client *pionex.Client) *PionexUniverse {
	return &PionexUniverse{client: client}
}

// RankedPerpSymbols returns every trading USDT perpetual in Pionex form
// (BTC_USDT_PERP), highest 24h turnover first.
func (u *PionexUniverse) RankedPerpSymbols(ctx context.Context) ([]string, error) {
	symbols, err := u.client.GetMarketSymbols(ctx, "PERP")
	if err != nil {
		return nil, err
	}
	tickers, err := u.client.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, err
	}
	amountBySymbol := make(map[string]decimal.Decimal, len(tickers))
	for _, t := range tickers {
		amountBySymbol[t.Symbol] = t.Amount
	}
	type entry struct {
		symbol string
		amount decimal.Decimal
	}
	ranked := make([]entry, 0, len(symbols))
	for _, s := range symbols {
		if s.Type != "PERP" || s.QuoteCurrency != "USDT" || !s.IsTrading() {
			continue
		}
		ranked = append(ranked, entry{symbol: s.Symbol, amount: amountBySymbol[s.Symbol]})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].amount.GreaterThan(ranked[j].amount)
	})
	out := make([]string, 0, len(ranked))
	for _, e := range ranked {
		out = append(out, e.symbol)
	}
	return out, nil
}
