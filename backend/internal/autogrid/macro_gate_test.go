package autogrid

import "testing"

func floatPtr(v float64) *float64 { return &v }

// The 2026-08-30 night class: BTC flat, dominance climbing — every non-short
// entry must veto while shorts stay untouched.
func TestMacroVetoAltDrain(t *testing.T) {
	mc := macroContext{loaded: true, btc24h: floatPtr(-0.5), domDelta: floatPtr(0.52)}
	if veto, _, _ := macroVeto("no_trend", false, mc); !veto {
		t.Fatal("alt-drain must veto NEUTRAL on flat BTC with rising dominance")
	}
	if veto, _, _ := macroVeto("long", false, mc); !veto {
		t.Fatal("alt-drain must veto LONG")
	}
	if veto, _, _ := macroVeto("short", false, mc); veto {
		t.Fatal("alt-drain must never veto shorts")
	}
	if veto, _, _ := macroVeto("no_trend", true, mc); veto {
		t.Fatal("cascade mode is exempt from the macro gate")
	}
}

func TestMacroVetoBetaDrift(t *testing.T) {
	mc := macroContext{loaded: true, btc24h: floatPtr(-3.4)}
	if veto, _, _ := macroVeto("no_trend", false, mc); !veto {
		t.Fatal("BTC 24h ≤ −3% must veto NEUTRAL")
	}
	if veto, _, _ := macroVeto("short", false, mc); veto {
		t.Fatal("beta-drift must never veto shorts")
	}
}

func TestMacroVetoPassesNormalRegime(t *testing.T) {
	mc := macroContext{loaded: true, btc24h: floatPtr(0.8), domDelta: floatPtr(-0.1)}
	for _, trend := range []string{"no_trend", "long", "short"} {
		if veto, _, _ := macroVeto(trend, false, mc); veto {
			t.Fatalf("normal regime must pass %s", trend)
		}
	}
}

// Fail-open contract: no data (fresh deploy before migration filled), stale
// data (>1h since collector died), or unarmed dominance delta (<24h of
// history) can never block entries.
func TestMacroVetoFailOpen(t *testing.T) {
	if veto, _, _ := macroVeto("no_trend", false, macroContext{}); veto {
		t.Fatal("unloaded context must pass")
	}
	if veto, _, _ := macroVeto("no_trend", false, macroContext{loaded: true, stale: true, btc24h: floatPtr(-5)}); veto {
		t.Fatal("stale context must pass even in beta-drift")
	}
	if veto, _, _ := macroVeto("no_trend", false, macroContext{loaded: true, btc24h: floatPtr(-0.5)}); veto {
		t.Fatal("dominance delta nil (history accumulating) must pass")
	}
}

// Boundary: dominance climbing while BTC itself dumps is NOT alt-drain —
// that regime is covered by the beta-drift branch instead.
func TestMacroVetoDrainRequiresFlatBTC(t *testing.T) {
	mc := macroContext{loaded: true, btc24h: floatPtr(-2.5), domDelta: floatPtr(0.5)}
	if veto, _, _ := macroVeto("no_trend", false, mc); veto {
		t.Fatal("dominance rise on a falling BTC must not double-veto (−2.5 < −2 floor)")
	}
}
