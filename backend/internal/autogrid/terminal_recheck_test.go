package autogrid

import (
	"testing"

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
