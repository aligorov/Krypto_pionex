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
