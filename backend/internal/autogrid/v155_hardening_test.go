package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

// v2.0.155: the trailing-SL advance is gated on a card stop actually
// existing (StopLossPrice set — ADAPTIVE_ATR deploy) and rounds to the
// symbol's price precision instead of a fixed Round(4).
func TestTrailingSLGateAndPrecision(t *testing.T) {
	newInput := func() botActionInput {
		in := baseActionInput()
		in.Direction = "LONG"
		in.RealizedPNL = mustDecimal("8") // >= 50% of the 12 target
		in.UnrealizedPNL = mustDecimal("0")
		in.CurrentPrice = mustDecimal("0.00012345") // sub-cent perp price
		return in
	}

	// No card stop (NONE-mode deploy): the update must never fire — a
	// NONE-mode bot cannot gain a card stop through the update endpoint.
	noCard := newInput()
	noCard.StopLossPrice = nil
	if d := decideBotAction(noCard); d.Action == ActionUpdateTrailingSL {
		t.Fatalf("trailing SL must not fire without a native card stop, got %+v", d)
	}

	// Card stop present + precision 8: the candidate keeps the sub-cent
	// scale (0.985 × price rounded to 8, not zeroed to a 4-decimal round).
	prec8 := newInput()
	sl := mustDecimal("0.00006")
	prec8.StopLossPrice = &sl
	prec8.PricePrecision = 8
	d := decideBotAction(prec8)
	if d.Action != ActionUpdateTrailingSL {
		t.Fatalf("expected UPDATE_TRAILING_SL on an armed LONG with a card stop, got %+v", d)
	}
	want := mustDecimal("0.0001216") // 0.00012345 * 0.985 rounded to 8
	if d.TrailingSLPrice == nil || !d.TrailingSLPrice.Equal(want) {
		got := "nil"
		if d.TrailingSLPrice != nil {
			got = d.TrailingSLPrice.String()
		}
		t.Fatalf("trailing candidate = %s, want %s (precision 8, not Round(4)=0.0001)", got, want)
	}

	// Precision 0 falls back to 4: a BTC-class price rounds sanely.
	prec4 := newInput()
	prec4.CurrentPrice = mustDecimal("83474.10")
	prec4.StopLossPrice = &sl
	d = decideBotAction(prec4)
	if d.Action != ActionUpdateTrailingSL {
		t.Fatalf("expected UPDATE_TRAILING_SL on the fallback precision, got %+v", d)
	}
	wantBTC := mustDecimal("82221.9885") // 83474.10 * 0.985 rounded to 4
	if d.TrailingSLPrice == nil || !d.TrailingSLPrice.Equal(wantBTC) {
		t.Fatalf("trailing candidate = %s, want %s", d.TrailingSLPrice.String(), wantBTC)
	}
}

// v2.0.155: the OU rotation respects the v2.0.89 four-hour floor and the
// v2.0.111 storm deferral — the raw 2×HL comparison must not rotate a young
// flat bot, and no rotation fires INTO a fleet storm.
func TestOURotationFloorAndStorm(t *testing.T) {
	newInput := func() botActionInput {
		in := baseActionInput()
		in.Budget = mustDecimal("200")
		in.RealizedPNL = mustDecimal("0.5")
		in.UnrealizedPNL = mustDecimal("0") // flat: |total| < 1% budget
		in.OURotationEnabled = true
		in.OUHalfLifeHours = 1.0
		return in
	}

	// Fast OU fit, bot younger than the 4h floor: HOLD.
	young := newInput()
	young.AgeHours = 3.0 // > 2×HL (2h) but < the 4h floor
	if d := decideBotAction(young); d.Action == ActionCloseOURotation {
		t.Fatalf("OU rotation must respect the 4h floor (age 3h, HL 1h), got %+v", d)
	}

	// Past the floor, storm unknown (nil): rotate.
	calm := newInput()
	calm.AgeHours = 5.0
	if d := decideBotAction(calm); d.Action != ActionCloseOURotation || d.Reason != "OU_HALFLIFE_ROTATION" {
		t.Fatalf("expected OU rotation past the floor without a storm, got %+v", d)
	}

	// Past the floor, storm active: defer.
	storm := newInput()
	storm.AgeHours = 5.0
	stormOn := true
	storm.StormActive = &stormOn
	if d := decideBotAction(storm); d.Action == ActionCloseOURotation {
		t.Fatalf("OU rotation must defer inside a fleet storm (v2.0.111), got %+v", d)
	}

	// Storm flag false: rotate (explicitly calm).
	stormOff := newInput()
	stormOff.AgeHours = 5.0
	stormOff.StormActive = &stormOn
	stormOff.StormActive = &[]bool{false}[0]
	if d := decideBotAction(stormOff); d.Action != ActionCloseOURotation {
		t.Fatalf("OU rotation must fire with the storm explicitly off, got %+v", d)
	}
}

// v2.0.155 (SEC-001): the blind loss signal is bounded by the bot's whole
// notional — anything beyond is a glitch or poison, not a market move.
func TestBlindEstimatePlausible(t *testing.T) {
	cases := []struct {
		name       string
		est        string
		investment string
		leverage   int
		want       bool
	}{
		{"inside notional", "-20", "100", 1, true},
		{"exactly notional", "-100", "100", 1, true},
		{"beyond notional", "-100.01", "100", 1, false},
		{"leveraged notional", "-500", "100", 5, true},
		{"poisoned far beyond", "-5000", "100", 5, false},
		{"zero leverage treated as 1x", "-100", "100", 0, true},
		{"tiny bot tiny bound", "-3", "2", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blindEstimatePlausible(
				mustDecimal(tc.est), mustDecimal(tc.investment), tc.leverage)
			if got != tc.want {
				t.Fatalf("blindEstimatePlausible(%s, %s, %d) = %v, want %v",
					tc.est, tc.investment, tc.leverage, got, tc.want)
			}
		})
	}
}

var _ = decimal.Zero // keep the import stable if cases above change shape
