package autogrid

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

func TestOverlayWSMarksReplacesFreshMarksWithAliases(t *testing.T) {
	lane := pionex.NewPublicStream("ws://unused", nil)
	lane.SetSymbols([]string{"XMR_USDT_PERP"})
	// Inject a fresh mark directly: the store is populated by the read pump
	// in production, but the overlay only reads it.
	lane.InjectForTest(pionex.MarkUpdate{
		Symbol:     "XMR_USDT_PERP",
		MarkPrice:  decimal.RequireFromString("572.10"),
		ReceivedAt: time.Now(),
	})

	worker := &Worker{wsLane: lane}
	// REST snapshot uses the priceMap alias convention: canonical + the
	// double-TrimSuffix base ("XMR_USDT").
	prices := map[string]decimal.Decimal{
		"XMR_USDT_PERP": decimal.RequireFromString("571.00"),
		"XMR_USDT":      decimal.RequireFromString("571.00"),
	}
	overlaid := worker.overlayWSMarks(prices)

	if overlaid != 1 {
		t.Fatalf("expected 1 overlaid symbol, got %d", overlaid)
	}
	for _, key := range []string{"XMR_USDT_PERP", "XMR_USDT", "XMR_USDT.PERP"} {
		if !prices[key].Equal(decimal.RequireFromString("572.10")) {
			t.Fatalf("alias %s not overlaid: %s", key, prices[key])
		}
	}
}

func TestOverlayWSMarksSkipsStaleAndEqual(t *testing.T) {
	lane := pionex.NewPublicStream("ws://unused", nil)
	lane.SetSymbols([]string{"OLD_USDT_PERP", "EQ_USDT_PERP"})
	lane.InjectForTest(pionex.MarkUpdate{
		Symbol:     "OLD_USDT_PERP",
		MarkPrice:  decimal.RequireFromString("10"),
		ReceivedAt: time.Now().Add(-10 * time.Minute),
	})
	lane.InjectForTest(pionex.MarkUpdate{
		Symbol:     "EQ_USDT_PERP",
		MarkPrice:  decimal.RequireFromString("5"),
		ReceivedAt: time.Now(),
	})

	worker := &Worker{wsLane: lane}
	prices := map[string]decimal.Decimal{
		"OLD_USDT_PERP": decimal.RequireFromString("9"),
		"EQ_USDT_PERP":  decimal.RequireFromString("5"),
	}
	if got := worker.overlayWSMarks(prices); got != 0 {
		t.Fatalf("stale+equal marks must not overlay, got %d", got)
	}
	if !prices["OLD_USDT_PERP"].Equal(decimal.RequireFromString("9")) {
		t.Fatalf("stale mark overwrote REST snapshot")
	}
}

func TestOverlayWSMarksNilLaneNoop(t *testing.T) {
	worker := &Worker{}
	prices := map[string]decimal.Decimal{"XMR_USDT_PERP": decimal.RequireFromString("571")}
	if got := worker.overlayWSMarks(prices); got != 0 {
		t.Fatalf("nil lane must be a no-op, got %d", got)
	}
}
