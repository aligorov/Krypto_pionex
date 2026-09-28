package autogrid

import "testing"

func TestNeighborBacktestTFs(t *testing.T) {
	if got := neighborBacktestTFs("60M"); len(got) != 2 || got[0] != "30M" || got[1] != "4H" {
		t.Fatalf("60M neighbors must be [30M 4H], got %v", got)
	}
	if got := neighborBacktestTFs("15M"); len(got) != 1 || got[0] != "30M" {
		t.Fatalf("15M neighbors must be [30M], got %v", got)
	}
	if got := neighborBacktestTFs("1D"); len(got) != 1 || got[0] != "4H" {
		t.Fatalf("1D neighbors must be [4H], got %v", got)
	}
}

func TestNormalizeBacktestTF(t *testing.T) {
	cases := map[string]string{"1H": "60M", "60M": "60M", "4H": "4H", "8H": "4H", "5M": "15M", "": "60M", "weird": "60M"}
	for input, want := range cases {
		if got := normalizeBacktestTF(input); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

// Regression of the prod MUBARAK case: traded TF healthy, one TF away a
// 71% drawdown — the symbol must be rejected as fragile.
func TestBacktestGateFragileNeighbor(t *testing.T) {
	traded := BacktestJobSummary{
		Interval: "15M", State: "done", Folds: 4, RoundTrips: 25,
		OOSPct: 2.38, MaxDD: 0.0219, StopHits: 0,
		CI95Positive: true, CI95Lower: 0.001, SampleSufficient: true, LiquidityOK: true,
	}
	neighbors := []BacktestJobSummary{
		{Interval: "30M", State: "done", Folds: 4, OOSPct: -18.18, MaxDD: 0.7163, StopHits: 2},
	}
	verdict := evaluateBacktestGate(traded, neighbors)
	if verdict.Allowed || verdict.Pending {
		t.Fatalf("fragile neighbor must reject, got allowed=%v pending=%v reason=%s",
			verdict.Allowed, verdict.Pending, verdict.Reason)
	}
}

func TestBacktestGateTradedTFMustPass(t *testing.T) {
	// Strict OOS floor: any non-positive OOS return must reject (tightened to > 0.0%).
	shallow := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 20,
		OOSPct: -0.36, MaxDD: 0.0223, StopHits: 0, SampleSufficient: true,
	}
	if verdict := evaluateBacktestGate(shallow, nil); verdict.Allowed {
		t.Fatalf("OOS -0.36%% below or at %.1f%% floor must reject: %s", backtestMinOOSPct, verdict.Reason)
	}

	storm := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 20,
		OOSPct: 2.5, MaxDD: 0.0223, StopHits: 3, SampleSufficient: true,
	}
	if verdict := evaluateBacktestGate(storm, nil); verdict.Allowed {
		t.Fatalf("stop-storm on traded TF must reject: %s", verdict.Reason)
	}

	deep := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 20,
		OOSPct: -2.0, MaxDD: 0.0223, StopHits: 0, SampleSufficient: true,
	}
	if verdict := evaluateBacktestGate(deep, nil); verdict.Allowed {
		t.Fatalf("OOS below the %.1f%% floor must reject: %s", backtestMinOOSPct, verdict.Reason)
	}

	// 95% Confidence Interval lower bound must be > 0.
	ciNegative := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 20,
		OOSPct: 1.5, MaxDD: 0.03, StopHits: 0,
		CI95Positive: false, CI95Lower: -0.0005, SampleSufficient: true,
	}
	if verdict := evaluateBacktestGate(ciNegative, nil); verdict.Allowed {
		t.Fatalf("negative 95%% CI lower bound must reject: %s", verdict.Reason)
	}

	// Sample size sufficiency: round trips < 15 must reject.
	insufficientTrades := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 10,
		OOSPct: 2.0, MaxDD: 0.02, StopHits: 0, SampleSufficient: false,
	}
	if verdict := evaluateBacktestGate(insufficientTrades, nil); verdict.Allowed {
		t.Fatalf("insufficient trades (< 15) must reject: %s", verdict.Reason)
	}

	// Drawdown strictly bounded <= 8%.
	highDD := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 25,
		OOSPct: 3.0, MaxDD: 0.09, StopHits: 0, SampleSufficient: true,
		CI95Positive: true, CI95Lower: 0.001,
	}
	if verdict := evaluateBacktestGate(highDD, nil); verdict.Allowed {
		t.Fatalf("drawdown 9%% > 8%% cap must reject: %s", verdict.Reason)
	}

	// Liquidity check failure must reject.
	liqFail := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 25,
		OOSPct: 3.0, MaxDD: 0.03, StopHits: 0, SampleSufficient: true,
		CI95Positive: true, CI95Lower: 0.001, LiquidityOK: false,
		LiquidityReason: "order size exceeds 10% of candle volume",
	}
	if verdict := evaluateBacktestGate(liqFail, nil); verdict.Allowed {
		t.Fatalf("liquidity failure must reject: %s", verdict.Reason)
	}

	traded := BacktestJobSummary{
		Interval: "60M", State: "done", Folds: 4, RoundTrips: 30,
		OOSPct: 3.58, MaxDD: 0.0223, StopHits: 0,
		CI95Positive: true, CI95Lower: 0.0025, SampleSufficient: true, LiquidityOK: true,
	}
	verdict := evaluateBacktestGate(traded, []BacktestJobSummary{
		{Interval: "30M", State: "done", Folds: 4, RoundTrips: 25, OOSPct: 1.2, MaxDD: 0.05, StopHits: 0},
		{Interval: "4H", State: "done", Folds: 4, RoundTrips: 20, OOSPct: 0.4, MaxDD: 0.03, StopHits: 1},
	})
	if !verdict.Allowed {
		t.Fatalf("healthy TF family must pass: %s", verdict.Reason)
	}
	if potential := verdict.PotentialPct; potential < 1.7 || potential > 1.9 {
		t.Fatalf("potential must average available TFs (~1.8), got %.2f", potential)
	}
}

func TestBacktestGatePending(t *testing.T) {
	verdict := evaluateBacktestGate(BacktestJobSummary{Interval: "60M", State: "pending"}, nil)
	if !verdict.Pending || verdict.Allowed {
		t.Fatalf("missing results must pend, not decide")
	}
	// Pending neighbors never block a passing traded TF.
	verdict = evaluateBacktestGate(
		BacktestJobSummary{
			Interval: "60M", State: "done", Folds: 4, RoundTrips: 25,
			OOSPct: 1.0, MaxDD: 0.02, StopHits: 0,
			CI95Positive: true, CI95Lower: 0.001, SampleSufficient: true, LiquidityOK: true,
		},
		[]BacktestJobSummary{{Interval: "30M", State: "pending"}, {Interval: "4H", State: "pending"}},
	)
	if !verdict.Allowed {
		t.Fatalf("pending neighbors must not block: %s", verdict.Reason)
	}
}
