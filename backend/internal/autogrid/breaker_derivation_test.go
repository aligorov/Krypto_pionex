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

// TestTranche2MaxLossCap pins the per-bot tranche-2 ceiling. v2.0.177 made
// the wide-grid floor (flat 8% × 1.25) exceed the 5% × 1.25 base; v2.0.183
// scales that floor with the ACTUAL deployed span — a 12% vol-scaled grid is
// judged against 12%, not the 8% yardstick (prod DOT $41.79 > $37.50 cut).
func TestTranche2MaxLossCap(t *testing.T) {
	budget := decimal.NewFromInt(200)
	// v2.0.177: wide-grid floor (8% × 1.25 = 10%) now exceeds the old 5% × 1.25 base
	if cap := tranche2MaxLossCap(budget, 2, 0); !cap.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("2x cap must be $40 (flat wide-grid floor), got %s", cap)
	}
	if cap := tranche2MaxLossCap(budget, 4, 0); !cap.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("4x cap must be $80 (flat wide-grid floor), got %s", cap)
	}
	// Degenerate leverage falls back to 1x, never to zero.
	if cap := tranche2MaxLossCap(budget, 0, 0); !cap.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("0x (fallback 1x) cap must be $20 (flat wide-grid floor), got %s", cap)
	}
	// v2.0.183: the floor follows the ACTUAL span — a 12% vol-scaled grid at
	// 2x/$200 → $200×2×12%×1.25 = $60, not the flat $40.
	if cap := tranche2MaxLossCap(budget, 2, 12.0); !cap.Equal(decimal.NewFromInt(60)) {
		t.Fatalf("2x/12%%-span cap must be $60, got %s", cap)
	}
	// Span beyond the doctrine cap clamps at 25%: $100×6×25%×1.25 = $187.50.
	if cap := tranche2MaxLossCap(decimal.NewFromInt(100), 6, 40.0); !cap.Equal(decimal.NewFromFloat(187.5)) {
		t.Fatalf("6x/40%%-span cap must clamp to $187.50, got %s", cap)
	}

	// Operator case: 6x on $100 → cap $37.50; a $21.57 dynamic stop passes,
	// a $40 overshoot does not.
	// v2.0.177: 6x/$100 wide-grid floor: $100 × 6 × 8% × 1.25 = $60
	sixCap := tranche2MaxLossCap(decimal.NewFromInt(100), 6, 0)
	if !sixCap.Equal(decimal.NewFromInt(60)) {
		t.Fatalf("6x/$100 cap must be $60 (wide-grid floor), got %s", sixCap)
	}
	if stop := decimal.NewFromFloat(21.57); stop.GreaterThan(sixCap) {
		t.Fatalf("$21.57 must fit under the 6x cap %s", sixCap)
	}
	// $40 no longer exceeds — wide-grid floor accommodates it
	if stop := decimal.NewFromFloat(70); !stop.GreaterThan(sixCap) {
		t.Fatalf("$70 must exceed the 6x cap %s", sixCap)
	}
}
