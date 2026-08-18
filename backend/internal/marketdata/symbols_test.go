package marketdata

import "testing"

func TestBuildExchangeAliasMap(t *testing.T) {
	aliases := BuildExchangeAliasMap([]string{
		"BTC_USDT_PERP", "PEPE_USDT_PERP", "BABYDOGE_USDT_PERP", "COINX_USDT_PERP",
	})
	if aliases["BTCUSDT"] != "BTC_USDT_PERP" {
		t.Errorf("plain symbol must map, got %q", aliases["BTCUSDT"])
	}
	if aliases["1000PEPEUSDT"] != "PEPE_USDT_PERP" {
		t.Errorf("1000-prefixed variant must map to the Pionex symbol, got %q", aliases["1000PEPEUSDT"])
	}
	if aliases["1000000BABYDOGEUSDT"] != "BABYDOGE_USDT_PERP" {
		t.Errorf("1000000-prefixed variant must map to the Pionex symbol, got %q", aliases["1000000BABYDOGEUSDT"])
	}
	// Venue-only listings that share no Pionex symbol must NOT map — the
	// bulk filter drops them instead of inventing rows.
	for _, native := range []string{"BTCDOMUSDT", "1000BONKPERP", "BIOUSDC"} {
		if p, ok := aliases[native]; ok {
			t.Errorf("venue-only symbol %q must not map, got %q", native, p)
		}
	}
	// The Pionex-exclusive symbol maps only under its own native form.
	if aliases["COINXUSDT"] != "COINX_USDT_PERP" {
		t.Errorf("COINX native form must map, got %q", aliases["COINXUSDT"])
	}
}
