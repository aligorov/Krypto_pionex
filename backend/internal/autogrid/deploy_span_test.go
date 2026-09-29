package autogrid

import (
	"testing"

	"github.com/shopspring/decimal"
)

func spanD(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

// TestEnsureDeploySpan pins the v2.0.163 wide-grid doctrine: the deploy span
// never ships below 8% of price; an already-wide side is never shrunk.
func TestEnsureDeploySpan(t *testing.T) {
	// A 3% band around 100 must widen to ≥8% centered on price.
	nl, nu := EnsureDeploySpan(spanD(98.5), spanD(101.5), spanD(100))
	span := nu.Sub(nl).Div(spanD(100)).Mul(spanD(100))
	if span.LessThan(spanD(7.99)) {
		t.Fatalf("narrow span not widened: %s..%s (%s%%)", nl, nu, span.StringFixed(2))
	}
	if nl.GreaterThan(spanD(100)) || nu.LessThan(spanD(100)) {
		t.Fatalf("price must stay inside the widened span: %s..%s", nl, nu)
	}

	// An asymmetric band keeps its wide side and still reaches 8%.
	nl, nu = EnsureDeploySpan(spanD(99.5), spanD(105), spanD(100))
	if !nl.Equal(spanD(96)) && nl.GreaterThan(spanD(96)) {
		t.Fatalf("lower bound must reach the 4%%-below floor or stay wider, got %s", nl)
	}
	if !nu.Equal(spanD(105)) {
		t.Fatalf("the already-wide upper side must be preserved, got %s", nu)
	}

	// An already-wide span passes untouched.
	l, u := spanD(90), spanD(115)
	nl, nu = EnsureDeploySpan(l, u, spanD(100))
	if !nl.Equal(l) || !nu.Equal(u) {
		t.Fatalf("wide span must pass untouched, got %s..%s", nl, nu)
	}

	// Degenerate inputs pass through.
	nl, nu = EnsureDeploySpan(spanD(0), spanD(0), spanD(0))
	if !nl.IsZero() || !nu.IsZero() {
		t.Fatalf("degenerate must pass through, got %s..%s", nl, nu)
	}
}
