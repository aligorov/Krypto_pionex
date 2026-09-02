package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestDeriveDailyLossBreaker pins the fleet-design derivation the operator
// validated against prod: the breaker equals N bots × the tranche-1 stop a
// designed bot stores (budget×leverage×2% floor, halved while tranches are
// on) × 1.25 headroom — so the 0.8 envelope ceiling lands exactly on the
// design fleet of stored stops (0.8×1.25 = 1.0) and a design-exact stop wave
// never trips its own breaker.
func TestDeriveDailyLossBreaker(t *testing.T) {
	base := Settings{
		BudgetUSDT:           decimal.NewFromInt(200),
		Leverage:             4,
		TrancheDeployEnabled: true,
	}

	// Historical prod shape: N=5/$200/4x → breaker $50, envelope $40.
	five := base
	five.MaxActiveBots = 5
	breaker := DeriveDailyLossBreaker(five)
	if !breaker.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("N=5/$200/4x must derive the prod breaker $50, got %s", breaker)
	}
	if envelope := breaker.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction)); !envelope.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("N=5 envelope must be $40, got %s", envelope)
	}

	// Target prod shape: N=10/$200/4x → breaker $100, envelope $80 — the
	// 10×$200 fleet finally fits its own design.
	ten := base
	ten.MaxActiveBots = 10
	breaker = DeriveDailyLossBreaker(ten)
	if !breaker.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("N=10/$200/4x must derive breaker $100, got %s", breaker)
	}
	if envelope := breaker.Mul(decimal.NewFromFloat(riskStopEnvelopeFraction)); !envelope.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("N=10 envelope must be $80, got %s", envelope)
	}

	// Tranches off: a designed bot stores the FULL floor stop, so the fleet
	// design doubles (N=5 → $100).
	noTranche := five
	noTranche.TrancheDeployEnabled = false
	if breaker := DeriveDailyLossBreaker(noTranche); !breaker.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("N=5/$200/4x without tranches must derive breaker $100, got %s", breaker)
	}

	// Non-integer product rounds to cents, never truncates silently.
	odd := five
	odd.MaxActiveBots = 3
	if got := DeriveDailyLossBreaker(odd); !got.Equal(decimal.NewFromFloat(30)) {
		t.Fatalf("N=3/$200/4x must derive $30, got %s", got)
	}
	odd.Leverage = 0 // deploy-path default (baseLev<=0 → 3) applies
	if got := DeriveDailyLossBreaker(odd); got.StringFixed(2) != "22.50" {
		t.Fatalf("leverage<=0 must fall back to the deploy default 3x (N=3 → $22.50), got %s", got)
	}
}

// TestTranche2MaxLossCap pins the derived per-bot tranche-2 ceiling
// budget×leverage×2%×1.25: the old static $12 refused the 4x design stop
// ($16) forever (prod BEX); the derived cap admits design stops with 25%
// overshoot headroom and still blocks σ-scaled outliers.
func TestTranche2MaxLossCap(t *testing.T) {
	budget := decimal.NewFromInt(200)
	if cap := tranche2MaxLossCap(budget, 2); !cap.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("2x cap must be $10, got %s", cap)
	}
	if cap := tranche2MaxLossCap(budget, 4); !cap.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("4x cap must be $20, got %s", cap)
	}
	// Degenerate leverage falls back to 1x, never to zero.
	if cap := tranche2MaxLossCap(budget, 0); !cap.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("0x (fallback 1x) cap must be $5, got %s", cap)
	}
}
