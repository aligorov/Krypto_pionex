package autogrid

import (
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// v2.0.149 (exit audit F1): the exchange-total floating estimate is a pure
// loss-signal — a missing (zero) total must never be read as an invented
// loss against booked grid profit, and a positive total must never flatter
// the books.
func TestBlindFloatingEstimate(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		remoteTotal, realized string
		want                  string
	}{
		{"loss deeper than grid profit", "-18.5", "1.5", "-20"},
		{"grid profit absorbs the float", "-0.5", "2", "-2.5"},
		{"positive total never accepted", "2.5", "1.5", "0"},
		{"missing total never invents a loss", "0", "5", "0"},
		{"missing total with booked profit", "0", "1.5", "0"},
		{"positive total with implied negative leg", "3", "5", "0"},
		{"flat total, no realized", "0", "0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := blindFloatingEstimate(
				decimal.RequireFromString(tc.remoteTotal),
				decimal.RequireFromString(tc.realized))
			if !got.Equal(decimal.RequireFromString(tc.want)) {
				t.Fatalf("blindFloatingEstimate(%s, %s) = %s, want %s",
					tc.remoteTotal, tc.realized, got, tc.want)
			}
		})
	}
}

// A zero price must disarm only the price exits: the PnL exits (max-loss,
// take-profit, trailing, breakeven lock) keep firing, and neither anti-hunt
// nor the range-break matrix may act without a price.
func TestDecideBotActionBlindPrice(t *testing.T) {
	antiHunt := decimal.NewFromInt(90)
	base := botActionInput{
		Direction:     "NEUTRAL",
		Lower:         decimal.NewFromInt(100),
		Upper:         decimal.NewFromInt(120),
		CurrentPrice:  decimal.Zero,
		RealizedPNL:   decimal.Zero,
		UnrealizedPNL: decimal.Zero,
		Budget:        decimal.NewFromInt(100),
		PnLTarget:     decimal.NewFromInt(9),
		MaxLoss:       decimal.NewFromInt(4),
		AntiHuntStop:  &antiHunt,
		Regime:        "",
	}

	maxLoss := base
	maxLoss.UnrealizedPNL = decimal.NewFromInt(-5)
	if d := decideBotAction(maxLoss); d.Action != ActionCloseStopLoss || d.Reason != "STOP_LOSS" {
		t.Fatalf("blind max-loss: got %s/%s, want CLOSE_STOP_LOSS/STOP_LOSS", d.Action, d.Reason)
	}

	takeProfit := base
	takeProfit.UnrealizedPNL = decimal.NewFromInt(10)
	if d := decideBotAction(takeProfit); d.Action != ActionCloseTakeProfit || d.Reason != "TAKE_PROFIT" {
		t.Fatalf("blind take-profit: got %s/%s, want CLOSE_TAKE_PROFIT/TAKE_PROFIT", d.Action, d.Reason)
	}

	// Armed peak (>= 75% of target, v2.0.164) decaying back under the
	// +0.2% budget floor must lock breakeven without any price.
	breakeven := base
	breakeven.Direction = "LONG"
	breakeven.PeakPNL = decimal.NewFromInt(10)
	breakeven.UnrealizedPNL = decimal.NewFromFloat(0.1)
	if d := decideBotAction(breakeven); d.Action != ActionCloseTakeProfit || d.Reason != "BREAKEVEN_LOCK" {
		t.Fatalf("blind breakeven lock: got %s/%s, want CLOSE_TAKE_PROFIT/BREAKEVEN_LOCK", d.Action, d.Reason)
	}

	// Price exits stay inert on a zero price even with an armed anti-hunt
	// stop and an unknown regime.
	if d := decideBotAction(base); d.Action != ActionHold {
		t.Fatalf("blind hold: got %s/%s, want HOLD (price exits must disarm)", d.Action, d.Reason)
	}
}

// The blindness episode lifecycle: a failed map or a blind bot starts (once
// per episode), 10 blind minutes page the EMERGENCY alarm at most hourly.
// A PARTIAL outage (healthy map, blind bot) must survive the map-health
// note — episode churn would spam the start Error every tick and keep the
// 10-minute page unreachable (review P2-1) — and only a fully clean pass
// closes the episode.
func TestNotePriceFeedBlindness(t *testing.T) {
	worker := &Worker{logger: slog.New(slog.DiscardHandler)}

	worker.notePriceFeedMapHealth(false)
	if worker.priceFeedBlindSince != nil {
		t.Fatal("healthy map must keep the episode clear")
	}

	worker.notePriceFeedBotBlind("BLINDA_USDT_PERP", 1)
	if worker.priceFeedBlindSince == nil {
		t.Fatal("blind bot must start an episode")
	}

	// Partial outage: the next pass's map is HEALTHY, yet the episode must
	// survive it (the previous pass ended blind) and age toward the page.
	worker.notePriceFeedMapHealth(false)
	if worker.priceFeedBlindSince == nil {
		t.Fatal("partial outage: a healthy map alone must not close the episode")
	}

	// Within the 10-minute window: no page.
	worker.markPriceFeedBlind("still blind")
	if !worker.priceFeedBlindLastAlarm.IsZero() {
		t.Fatal("no alarm before 10 minutes of blindness")
	}

	// Aged past 10 minutes: exactly one alarm, then rate-limited for an hour.
	aged := time.Now().UTC().Add(-11 * time.Minute)
	worker.priceFeedBlindSince = &aged
	worker.markPriceFeedBlind("aged")
	if worker.priceFeedBlindLastAlarm.IsZero() {
		t.Fatal("alarm must fire after 10 blind minutes")
	}
	first := worker.priceFeedBlindLastAlarm
	worker.markPriceFeedBlind("aged again")
	if !worker.priceFeedBlindLastAlarm.Equal(first) {
		t.Fatal("alarm must be rate-limited to one per hour")
	}

	// Full recovery takes one clean pass to close the episode: a still-blind
	// pass re-arms lastPass (the bots loop is what keeps the episode alive),
	// then the first fully clean map note keeps it for that pass and the
	// second clears it.
	worker.notePriceFeedBotBlind("BLINDA_USDT_PERP", 1)
	worker.notePriceFeedMapHealth(false)
	if worker.priceFeedBlindSince == nil {
		t.Fatal("episode must survive the pass right after a blind one")
	}
	worker.notePriceFeedMapHealth(false)
	if worker.priceFeedBlindSince != nil {
		t.Fatal("a fully clean pass must close the episode")
	}

	// A fresh episode must not re-page inside the rate-limit window.
	worker.notePriceFeedMapHealth(true)
	if worker.priceFeedBlindSince == nil {
		t.Fatal("failed map must start an episode")
	}
	if !worker.priceFeedBlindLastAlarm.Equal(first) {
		t.Fatal("a fresh episode must not re-page inside the rate-limit window")
	}
}
