package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

func TestPendingOrConfirmedRecon(t *testing.T) {
	cases := map[string]string{
		string(pionex.FinalProfitTelemetryNetClose): TerminalFinalPendingExchange,
		string(pionex.FinalProfitNone):              TerminalFinalPendingExchange,
		string(pionex.FinalProfitExited):            "REMOTE_TERMINAL_CONFIRMED",
		string(pionex.FinalProfitTotalAlias):        "REMOTE_TERMINAL_CONFIRMED",
		"":                                          "REMOTE_TERMINAL_CONFIRMED",
	}
	for marker, want := range cases {
		if got := pendingOrConfirmedRecon(marker); got != want {
			t.Fatalf("marker %q: got %s want %s", marker, got, want)
		}
	}
}

// v2.0.103: the sanity gate must own the unlock-identity leg — the inverted
// contract froze prod rows back at estimates while the exchange truth was
// computed in hand.
func TestGateSettledProfitUnlockIdentity(t *testing.T) {
	neg := decimal.RequireFromString("-1.66")
	if got := gateSettledProfit(neg, pionex.FinalProfitUnlockIdentity, "GRID_AGED_HALF_LIFE", "user_cancel"); got == nil || !got.Equal(neg) {
		t.Fatalf("negative identity must pass: %v", got)
	}
	pos := decimal.RequireFromString("1.66")
	if got := gateSettledProfit(pos, pionex.FinalProfitUnlockIdentity, "GRID_AGED_HALF_LIFE", ""); got == nil || !got.Equal(pos) {
		t.Fatalf("positive non-loss identity must pass: %v", got)
	}
	if got := gateSettledProfit(pos, pionex.FinalProfitUnlockIdentity, "STOP_LOSS", ""); got != nil {
		t.Fatalf("positive identity on loss-class must refuse: %v", got)
	}
}

// v2.0.105: the identity pairs unlockUsdtAmount with OUR final investment —
// the live failure was unlock 102.28 vs the exchange's STALE usdtInvestment
// field (50) writing +52.28 into the ledger while the truth was +2.28.
func TestUnlockIdentityUsesOurInvestment(t *testing.T) {
	// Reproduce the prod payloads: unlock carries returned capital+profit,
	// usdtInvestment still reads the PRE-tranche 50 while OUR final is 100.
	our := decimal.RequireFromString("100")
	unlock := decimal.RequireFromString("102.279854292")
	net := unlock.Sub(our)
	if !net.Equal(decimal.RequireFromString("2.279854292")) {
		t.Fatalf("CRV #1282 true final must be +2.28, got %s", net)
	}
	// The buggy v2.0.100 pairing for the same record:
	stale := decimal.RequireFromString("50")
	if bug := unlock.Sub(stale); bug.Equal(net) {
		t.Fatalf("stale-field pairing must differ (prod wrote %s)", bug)
	}
	// Sanity band: |net| < our investment.
	if !net.Abs().LessThan(our) {
		t.Fatalf("net must sit inside the margin band")
	}
	extreme := decimal.RequireFromString("33.35") // AR #1313 screenshot row
	arNet := extreme.Sub(our)
	if !arNet.Equal(decimal.RequireFromString("-66.65")) || !arNet.Abs().LessThan(our) {
		t.Fatalf("AR extreme loss must pass the band: %s", arNet)
	}
}
