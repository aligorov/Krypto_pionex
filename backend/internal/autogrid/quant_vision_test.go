package autogrid

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestEvaluateWickShield_AlreadyArmed_Holding(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	// Price is 50.5 (above extreme 50.0 for LONG/NEUTRAL), 30s elapsed < 90s grace
	currentPrice := decimal.NewFromFloat(50.5)
	shouldHold, newTrig, newExt, reason := worker.evaluateWickShield(
		context.Background(), "SOL_USDT_PERP", "LONG", currentPrice, &triggeredAt, &extreme, 90,
	)

	if !shouldHold {
		t.Fatalf("expected shouldHold=true, got false, reason=%s", reason)
	}
	if newTrig == nil || *newTrig != triggeredAt {
		t.Fatalf("expected triggeredAt preserved, got %v", newTrig)
	}
	if newExt == nil || !newExt.Equal(extreme) {
		t.Fatalf("expected extreme preserved, got %v", newExt)
	}
}

func TestEvaluateWickShield_AlreadyArmed_Breached(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	// Price dropped to 49.5 (below extreme 50.0 for LONG) -> breach confirmed
	currentPrice := decimal.NewFromFloat(49.5)
	shouldHold, _, _, reason := worker.evaluateWickShield(
		context.Background(), "SOL_USDT_PERP", "LONG", currentPrice, &triggeredAt, &extreme, 90,
	)

	if shouldHold {
		t.Fatalf("expected shouldHold=false on breach, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

func TestEvaluateWickShield_AlreadyArmed_Expired(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	// 95s elapsed > 90s grace
	triggeredAt := now.Add(-95 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	currentPrice := decimal.NewFromFloat(50.5)
	shouldHold, _, _, reason := worker.evaluateWickShield(
		context.Background(), "SOL_USDT_PERP", "LONG", currentPrice, &triggeredAt, &extreme, 90,
	)

	if shouldHold {
		t.Fatalf("expected shouldHold=false on expired grace, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

func TestEvaluateWickShield_Short_Breached(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(100.0)

	// For SHORT, breach is when price rises ABOVE extreme
	currentPrice := decimal.NewFromFloat(101.0)
	shouldHold, _, _, reason := worker.evaluateWickShield(
		context.Background(), "BTC_USDT_PERP", "SHORT", currentPrice, &triggeredAt, &extreme, 90,
	)

	if shouldHold {
		t.Fatalf("expected shouldHold=false on SHORT breach, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}
