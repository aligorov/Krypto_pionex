package autogrid

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

func rtd(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	v, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q: %v", s, err)
	}
	return v
}

func TestShouldTriggerRealtimePass(t *testing.T) {
	cases := []struct {
		name          string
		baseline, now decimal.Decimal
		atrPct        float64
		want          bool
	}{
		// No ATR: floor 0.30% governs.
		{"sub-floor drift", rtd(t, "100"), rtd(t, "100.20"), 0, false},
		{"floor move no ATR", rtd(t, "100"), rtd(t, "100.31"), 0, true},
		{"down move floor", rtd(t, "100"), rtd(t, "99.69"), 0, true},
		// ATR 1.0% → threshold 0.6%: a 0.4% move is noise, 0.65% triggers.
		{"sub-ATR move", rtd(t, "100"), rtd(t, "100.40"), 1.0, false},
		{"ATR-scaled trigger", rtd(t, "100"), rtd(t, "100.65"), 1.0, true},
		// Tiny ATR (0.1%) never lowers the floor below 0.30%.
		{"floor wins over tiny ATR", rtd(t, "100"), rtd(t, "100.29"), 0.1, false},
		{"floor trigger tiny ATR", rtd(t, "100"), rtd(t, "100.30"), 0.1, true},
		// Degenerates never fire.
		{"zero baseline", rtd(t, "0"), rtd(t, "100"), 1.0, false},
		{"zero now", rtd(t, "100"), rtd(t, "0"), 1.0, false},
	}
	for _, tc := range cases {
		if got := shouldTriggerRealtimePass(tc.baseline, tc.now, tc.atrPct); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestOnRealtimeMarkSignalsSharpMove(t *testing.T) {
	worker := &Worker{
		realtimeSignal: make(chan string, 1),
		realtimeWatch: map[string]realtimePoint{
			"SUI_USDT_PERP": {price: rtd(t, "1.0000"), atrPct: 1.5},
		},
	}
	// 0.9×ATR15m dump — the SUI-overshoot class.
	worker.onRealtimeMark(pionex.MarkUpdate{Symbol: "SUI_USDT_PERP", MarkPrice: rtd(t, "0.9900")})
	select {
	case sym := <-worker.realtimeSignal:
		if sym != "SUI_USDT_PERP" {
			t.Fatalf("wrong symbol signalled: %s", sym)
		}
	default:
		t.Fatalf("sharp move did not signal")
	}

	// Noise must not queue a second signal.
	worker.onRealtimeMark(pionex.MarkUpdate{Symbol: "SUI_USDT_PERP", MarkPrice: rtd(t, "0.9999")})
	select {
	case sym := <-worker.realtimeSignal:
		t.Fatalf("noise signalled: %s", sym)
	default:
	}

	// Unknown symbol is ignored entirely.
	worker.onRealtimeMark(pionex.MarkUpdate{Symbol: "NOT_IN_FLEET", MarkPrice: rtd(t, "1.5")})
}

func TestHandleRealtimeSignalDebounce(t *testing.T) {
	worker := &Worker{
		realtimeSignal: make(chan string, 1),
		realtimeWatch:  make(map[string]realtimePoint),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ticker := &fakeResettable{}
	worker.lastEventPass = time.Now() // fired a moment ago
	worker.handleRealtimeSignal("XMR_USDT_PERP", ticker)
	if ticker.reset {
		t.Fatalf("debounced call must not run a pass")
	}

	worker.lastEventPass = time.Now().Add(-2 * minEventPassSpacing)
	// Without a db the pass errors inside runGuarded but the debounce gate
	// must have opened and Reset been invoked by the ticker path only after
	// a real pass; assert the gate opened via lastEventPass refresh.
	before := worker.lastEventPass
	worker.handleRealtimeSignal("XMR_USDT_PERP", ticker)
	if !worker.lastEventPass.After(before) {
		t.Fatalf("debounce window did not refresh on an open gate")
	}
}

func TestRememberRealtimeBaselines(t *testing.T) {
	worker := &Worker{
		realtimeSignal: make(chan string, 1),
		realtimeWatch: map[string]realtimePoint{
			"OLD_USDT_PERP": {price: rtd(t, "5"), atrPct: 2.0},
		},
	}
	prices := map[string]decimal.Decimal{
		"SUI_USDT_PERP":  rtd(t, "1.02"),
		"OLD_USDT_PERP":  rtd(t, "6"),
		"ZERO_USDT_PERP": decimal.Zero,
	}
	atrs := map[string]float64{"SUI_USDT_PERP": 1.4}
	worker.rememberRealtimeBaselines(prices, atrs)

	if got := worker.realtimeWatch["SUI_USDT_PERP"]; !got.price.Equal(rtd(t, "1.02")) || got.atrPct != 1.4 {
		t.Fatalf("sui baseline wrong: %+v", got)
	}
	// Missing ATR keeps the previous reading for a known symbol.
	if got := worker.realtimeWatch["OLD_USDT_PERP"]; got.atrPct != 2.0 {
		t.Fatalf("old ATR not carried: %+v", got)
	}
	if _, ok := worker.realtimeWatch["ZERO_USDT_PERP"]; ok {
		t.Fatalf("zero price must not be baselined")
	}
}

type fakeResettable struct{ reset bool }

func (f *fakeResettable) Reset(time.Duration) { f.reset = true }
