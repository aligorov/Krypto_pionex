package autogrid

import (
	"encoding/json"
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
	// CRWVX #1440 shape: stop loss exit with positive net profit from accumulated grid earnings.
	// Unlock identity is ground truth cash returned to wallet, must NOT be rejected.
	if got := gateSettledProfit(pos, pionex.FinalProfitUnlockIdentity, "STOP_LOSS", ""); got == nil || !got.Equal(pos) {
		t.Fatalf("positive identity on loss-class must be accepted: %v", got)
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

// v2.0.196: the identity leg is a function of the record + OUR investment, so
// the re-check sweep can compute it even when the chain returned a total the
// sanity gate refuses. Absent/garbled carriers answer "no identity" (the row
// stays re-probeable) rather than a zero final.
func TestUnlockIdentityFinal(t *testing.T) {
	our := decimal.RequireFromString("100")
	record := func(unlock string) *pionex.BUOrderDataResponse {
		return &pionex.BUOrderDataResponse{UnlockUsdtAmountRaw: json.RawMessage(unlock)}
	}

	// CRV #1282 shape: the wallet's own settlement figure.
	net, source := unlockIdentityFinal(record(`"102.279854292"`), our)
	if source != pionex.FinalProfitUnlockIdentity || !net.Equal(decimal.RequireFromString("2.279854292")) {
		t.Fatalf("identity must settle +2.279854292, got %s / %s", net, source)
	}
	// A loss inside isolated margin is a valid identity too.
	net, source = unlockIdentityFinal(record(`33.35`), our)
	if source != pionex.FinalProfitUnlockIdentity || !net.Equal(decimal.RequireFromString("-66.65")) {
		t.Fatalf("numeric carrier must settle −66.65, got %s / %s", net, source)
	}
	for name, tc := range map[string]struct {
		record     *pionex.BUOrderDataResponse
		investment decimal.Decimal
	}{
		"нет записи":        {nil, our},
		"нет капитала":      {record(`"102.279854292"`), decimal.Zero},
		"поле пустое":       {record(``), our},
		"поле null":         {record(`null`), our},
		"мусор в поле":      {record(`"abc"`), our},
		"ничего не вернули": {record(`"0"`), our},
		"вне полосы маржи":  {record(`"250"`), our},
		// The band binds on the profit side: a loss deeper than isolated margin
		// is unreachable (unlock would be negative, caught above), so what the
		// band actually rejects is a net larger than the capital that went in.
		"прибыль больше вклада": {record(`"1.0"`), decimal.RequireFromString("0.4")},
	} {
		net, source := unlockIdentityFinal(tc.record, tc.investment)
		if source != pionex.FinalProfitNone || !net.IsZero() {
			t.Errorf("%s: ожидался отказ без цифры, получено %s / %s", name, net, source)
		}
	}
}
