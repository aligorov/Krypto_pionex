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
