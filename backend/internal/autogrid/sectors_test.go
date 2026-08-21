package autogrid

import "testing"

func TestSectorForSymbol(t *testing.T) {
	cases := map[string]string{
		"AMDX_USDT_PERP": "semis", "SOXLX_USDT_PERP": "semis",
		"CRWVX_USDT_PERP": "ai_compute", "NBISX_USDT_PERP": "ai_compute",
		"BTC_USDT_PERP": "", "ON_USDT_PERP": "", "SOLVX_USDT_PERP": "",
	}
	for symbol, want := range cases {
		if got := sectorForSymbol(symbol); got != want {
			t.Fatalf("sectorForSymbol(%s) = %q, want %q", symbol, got, want)
		}
	}
}
