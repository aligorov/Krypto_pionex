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
	traded := BacktestJobSummary{Interval: "15M", State: "done", Folds: 4, OOSPct: 2.38, MaxDD: 0.0219, StopHits: 0}
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
	// v2.0.23 OOS floor: a shallow negative OOS (trend folds the grid
	// deliberately does not trade) passes; a deep negative or a stop-storm
	// still rejects.
	shallow := BacktestJobSummary{Interval: "60M", State: "done", Folds: 4, OOSPct: -0.36, MaxDD: 0.0223, StopHits: 0}
	if verdict := evaluateBacktestGate(shallow, nil); !verdict.Allowed {
		t.Fatalf("OOS -0.36%% above the %.1f%% floor must pass: %s", backtestMinOOSPct, verdict.Reason)
	}
	storm := BacktestJobSummary{Interval: "60M", State: "done", Folds: 4, OOSPct: -0.36, MaxDD: 0.0223, StopHits: 3}
	if verdict := evaluateBacktestGate(storm, nil); verdict.Allowed {
		t.Fatalf("stop-storm on traded TF must reject: %s", verdict.Reason)
	}
	deep := BacktestJobSummary{Interval: "60M", State: "done", Folds: 4, OOSPct: -2.0, MaxDD: 0.0223, StopHits: 0}
	if verdict := evaluateBacktestGate(deep, nil); verdict.Allowed {
		t.Fatalf("OOS below the %.1f%% floor must reject: %s", backtestMinOOSPct, verdict.Reason)
	}

	traded := BacktestJobSummary{Interval: "60M", State: "done", Folds: 4, OOSPct: 3.58, MaxDD: 0.0223, StopHits: 0}
	verdict := evaluateBacktestGate(traded, []BacktestJobSummary{
		{Interval: "30M", State: "done", Folds: 4, OOSPct: 1.2, MaxDD: 0.05, StopHits: 0},
		{Interval: "4H", State: "done", Folds: 4, OOSPct: 0.4, MaxDD: 0.03, StopHits: 1},
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
		BacktestJobSummary{Interval: "60M", State: "done", Folds: 4, OOSPct: 1.0, MaxDD: 0.02},
		[]BacktestJobSummary{{Interval: "30M", State: "pending"}, {Interval: "4H", State: "pending"}},
	)
	if !verdict.Allowed {
		t.Fatalf("pending neighbors must not block: %s", verdict.Reason)
	}
}
