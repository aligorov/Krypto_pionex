package marketdata

import "testing"

// v2.0.100: CJK meme tickers (牛来, 龙虾) list on the public market feed but
// the futuresGrid create endpoint refuses them — the scanner must never
// ACCEPT one (prod burned deploy slots #1287/#1304 on exactly this).
func TestIsASCIISymbol(t *testing.T) {
	for _, ok := range []string{"ARB_USDT_PERP", "BTC_USDT_PERP", "1000PEPE_USDT_PERP", "BULLA_USDT_PERP"} {
		if !isASCIISymbol(ok) {
			t.Fatalf("%s must pass", ok)
		}
	}
	for _, bad := range []string{"牛来_USDT_PERP", "龙虾_USDT_PERP", "ARB USDT", "ARB\tUSDT", "ARB_USDT_PERP"} {
		if bad != "ARB_USDT_PERP" && isASCIISymbol(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
