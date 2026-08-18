package marketdata

import "strings"

// Symbol unification across exchanges.
//
// Pionex perpetual: BTC_USDT_PERP
// Binance futures:  BTCUSDT
// Bybit linear:     BTCUSDT (identical to Binance)
// OKX swap:         BTC-USDT-SWAP
//
// Pionex spot pairs (BTC_USDT) are accepted as well: the "_PERP" suffix is
// simply absent and the remaining underscore is collapsed the same way.

// PerpSuffix is the Pionex marker for perpetual symbols.
const PerpSuffix = "_PERP"

// ToBinanceSymbol converts a Pionex symbol to the Binance form.
// BTC_USDT_PERP -> BTCUSDT (drop _PERP, drop the underscore before USDT).
// The same mapping applies to Bybit linear contracts.
func ToBinanceSymbol(pionexSymbol string) string {
	s := strings.TrimSpace(pionexSymbol)
	s = strings.TrimSuffix(s, PerpSuffix)
	return strings.ReplaceAll(s, "_", "")
}

// FromBinanceSymbol converts a Binance symbol back to the Pionex perpetual
// form. BTCUSDT -> BTC_USDT_PERP.
func FromBinanceSymbol(binanceSymbol string) string {
	s := strings.TrimSpace(binanceSymbol)
	// Longest quote first so "USDT" is not misread as "USD".
	for _, quote := range []string{"USDT", "USDC", "USD"} {
		if strings.HasSuffix(s, quote) && len(s) > len(quote) {
			return s[:len(s)-len(quote)] + "_" + quote + PerpSuffix
		}
	}
	return s + PerpSuffix
}

// ToOKXSymbol converts a Pionex symbol to the OKX swap form.
// BTC_USDT_PERP -> BTC-USDT-SWAP.
func ToOKXSymbol(pionexSymbol string) string {
	s := strings.TrimSpace(pionexSymbol)
	s = strings.TrimSuffix(s, PerpSuffix)
	return strings.ReplaceAll(s, "_", "-") + "-SWAP"
}

// BuildExchangeAliasMap maps Binance/Bybit native symbol forms back to the
// Pionex perpetual form for every given Pionex symbol. Low-priced assets are
// quoted on those venues with 1000/10000/1000000 contract-multiplier prefixes
// (PEPE -> 1000PEPEUSDT, SHIB -> 1000SHIBUSDT, BABYDOGE -> 1000000BABYDOGEUSDT),
// so each prefix variant is registered as an alias of the same Pionex symbol.
// Bulk exchange responses are filtered through this map: anything absent on
// Pionex (BTCDOMUSDT, USDC pairs, venue-exclusive listings) never reaches the
// database, and FromBinanceSymbol edge cases are sidestepped entirely.
func BuildExchangeAliasMap(pionexSymbols []string) map[string]string {
	aliases := make(map[string]string, len(pionexSymbols)*2)
	for _, p := range pionexSymbols {
		base := strings.TrimSpace(p)
		base = strings.TrimSuffix(base, PerpSuffix)
		base = strings.ReplaceAll(base, "_", "")
		if base == "" {
			continue
		}
		aliases[base] = p
		for _, prefix := range []string{"1000", "10000", "1000000"} {
			aliases[prefix+base] = p
		}
	}
	return aliases
}
