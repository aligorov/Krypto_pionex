package autogrid

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func lgD(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

func TestLiquidationGuardReason(t *testing.T) {
	// LONG: stop 95, liq estimate 94.7 → only 0.3% of entry apart → refuse.
	reason := liquidationGuardReason("long", lgD(100), lgD(94.7), decimal.Zero, lgD(95), nil, 4)
	if reason == "" {
		t.Fatal("expected refusal for a stop glued to the liquidation estimate")
	}
	if !strings.Contains(reason, "плече 4x") {
		t.Fatalf("reason must name the leverage for the de-gear hint: %s", reason)
	}

	// LONG: stop 95, liq estimate 91 → 4% apart → pass.
	if reason := liquidationGuardReason("long", lgD(100), lgD(91), decimal.Zero, lgD(95), nil, 4); reason != "" {
		t.Fatalf("safe geometry refused: %s", reason)
	}

	// NEUTRAL: both sides checked. Lower fine, upper glued → refuse.
	high := lgD(104)
	reason = liquidationGuardReason("no_trend", lgD(100), lgD(90), lgD(104.4), lgD(95), &high, 5)
	if reason == "" {
		t.Fatal("expected refusal on the upside wall of a neutral grid")
	}

	// SHORT: the single stop is ABOVE entry — checked against liq_up.
	if reason := liquidationGuardReason("short", lgD(100), lgD(90), lgD(110), lgD(105), nil, 3); reason != "" {
		t.Fatalf("safe short geometry refused: %s", reason)
	}
	if reason := liquidationGuardReason("short", lgD(100), lgD(90), lgD(105.2), lgD(105), nil, 3); reason == "" {
		t.Fatal("expected refusal for a short stop glued to the upside liquidation")
	}

	// Missing estimates → fail-open (exchange returned nothing).
	if reason := liquidationGuardReason("long", lgD(100), decimal.Zero, decimal.Zero, lgD(95), nil, 4); reason != "" {
		t.Fatalf("missing estimates must not block: %s", reason)
	}
	// Degenerate entry → skip.
	if reason := liquidationGuardReason("long", decimal.Zero, lgD(90), lgD(110), lgD(95), nil, 4); reason != "" {
		t.Fatalf("degenerate entry must not block: %s", reason)
	}
	// Wrong-side / absurd estimates (SEC-001 bounds): liq ABOVE the lower
	// stop, or beyond half the entry — exchange garbage stands down.
	if reason := liquidationGuardReason("long", lgD(100), lgD(96), decimal.Zero, lgD(95), nil, 4); reason != "" {
		t.Fatalf("wrong-side down estimate must stand down, not refuse: %s", reason)
	}
	if reason := liquidationGuardReason("long", lgD(100), lgD(45), decimal.Zero, lgD(95), nil, 4); reason != "" {
		t.Fatalf("absurdly far down estimate must stand down: %s", reason)
	}
	if reason := liquidationGuardReason("short", lgD(100), lgD(90), lgD(104), lgD(105), nil, 3); reason != "" {
		t.Fatalf("wrong-side up estimate must stand down: %s", reason)
	}
}

func TestLiquidationProximityBreached(t *testing.T) {
	if !liquidationProximityBreached(lgD(100), lgD(101.5)) {
		t.Fatal("1.5% from the wall must breach")
	}
	if liquidationProximityBreached(lgD(100), lgD(103)) {
		t.Fatal("3% from the wall must not breach")
	}
	if liquidationProximityBreached(lgD(100), decimal.Zero) {
		t.Fatal("missing running liq must never breach")
	}
}

func TestDirectionalTrendExempt(t *testing.T) {
	mk := func(trend string, adx, slope float64) Candidate {
		return Candidate{
			RecommendedTrend: trend,
			ModelAssumptions: map[string]any{"adx": adx, "emaSlopePct": slope},
		}
	}
	mkWithRSI := func(trend string, adx, slope, rsi, rangePos float64) Candidate {
		return Candidate{
			RecommendedTrend: trend,
			ModelAssumptions: map[string]any{
				"adx": adx, "emaSlopePct": slope, "rsi": rsi, "rangePositionPct": rangePos,
			},
		}
	}
	// Proven cohort keeps priority: BTC down + confirmed own downtrend short.
	if v := directionalTrendExempt(mk("short", 25, -1.2), true); !v.Exempt || v.Cohort != "betaDownExempt" {
		t.Fatalf("beta-down cohort lost: %+v", v)
	}
	// v2.0.161: confirmed own downtrend short WITHOUT BTC down — exempt now.
	if v := directionalTrendExempt(mk("short", 25, -1.2), false); !v.Exempt || v.Cohort != "dirTrendShort" {
		t.Fatalf("own-trend short not exempt: %+v", v)
	}
	// Slope-only confirmation (ADX below threshold but steep).
	if v := directionalTrendExempt(mk("short", 15, -0.8), false); !v.Exempt || v.Cohort != "dirTrendShort" {
		t.Fatalf("steep-slope short not exempt: %+v", v)
	}
	// Confirmed own uptrend long, BTC flat/up — exempt (RSI/channel defaults sane).
	if v := directionalTrendExempt(mk("long", 25, 1.2), false); !v.Exempt || v.Cohort != "dirTrendLong" {
		t.Fatalf("own-trend long not exempt: %+v", v)
	}
	// v2.0.162 (operator override): the long cohort may enter breakouts up
	// to 90% of the channel; the overheating RSI cap stays.
	if v := directionalTrendExempt(mkWithRSI("long", 25, 1.2, 75, 50), false); v.Exempt {
		t.Fatalf("overheated long (RSI 75 > cap 70) must not exempt: %+v", v)
	}
	if v := directionalTrendExempt(mkWithRSI("long", 25, 1.2, 65, 80), false); !v.Exempt || v.Cohort != "dirTrendLong" {
		t.Fatalf("breakout long (pos 80%% ≤ 90%%) must exempt: %+v", v)
	}
	if v := directionalTrendExempt(mkWithRSI("long", 25, 1.2, 65, 95), false); v.Exempt {
		t.Fatalf("exhausted channel (pos 95%%) must not exempt: %+v", v)
	}
	if v := directionalTrendExempt(mkWithRSI("long", 25, 1.2, 65, 50), false); !v.Exempt || v.Cohort != "dirTrendLong" {
		t.Fatalf("sane long must exempt: %+v", v)
	}
	// Long in a confirmed uptrend but BTC falling — beta-gate wins, no exempt.
	if v := directionalTrendExempt(mk("long", 25, 1.2), true); v.Exempt {
		t.Fatalf("long exempt must not bypass the BTC beta veto: %+v", v)
	}
	// Weak trend — no exemption (the dead zone stays armed).
	if v := directionalTrendExempt(mk("short", 18, -0.3), false); v.Exempt {
		t.Fatalf("weak trend must not exempt: %+v", v)
	}
	// Direction against the slope — never.
	if v := directionalTrendExempt(mk("short", 30, 1.5), false); v.Exempt {
		t.Fatalf("short against an uptrend must not exempt: %+v", v)
	}
	// Neutral never.
	if v := directionalTrendExempt(mk("no_trend", 30, -1.5), true); v.Exempt {
		t.Fatalf("neutral must never exempt: %+v", v)
	}
}
