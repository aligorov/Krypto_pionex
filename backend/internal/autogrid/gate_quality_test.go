package autogrid

import (
	"math"
	"strings"
	"testing"
	"time"
)

// TestProofStrengthBoundaries pins the exact contract bars: LOW while
// coverage < 0.5 OR completed < 20; MEDIUM while coverage < 0.8 OR
// completed < 50; HIGH only above both. The boundary values themselves
// (0.5 / 0.8 / 20 / 50) must clear their bar, not fail it.
func TestProofStrengthBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		coverage  float64
		completed int
		want      string
	}{
		// Coverage bar.
		{"coverage 0.49 is LOW", 0.49, 100, "LOW"},
		{"coverage 0.50 clears the LOW bar", 0.50, 100, "MEDIUM"},
		{"coverage 0.79 stays MEDIUM", 0.79, 100, "MEDIUM"},
		{"coverage 0.80 clears the MEDIUM bar", 0.80, 100, "HIGH"},
		// Completed-outcomes bar.
		{"19 completed is LOW", 1.0, 19, "LOW"},
		{"20 completed clears the LOW bar", 1.0, 20, "MEDIUM"},
		{"49 completed stays MEDIUM", 1.0, 49, "MEDIUM"},
		{"50 completed clears the MEDIUM bar", 1.0, 50, "HIGH"},
		// OR semantics: one failing bar is enough to demote.
		{"high coverage cannot rescue 19 completed", 0.99, 19, "LOW"},
		{"50 completed cannot rescue 0.49 coverage", 0.49, 50, "LOW"},
		{"50 completed cannot rescue 0.79 coverage", 0.79, 50, "MEDIUM"},
		{"0.80 coverage cannot rescue 49 completed", 0.80, 49, "MEDIUM"},
		// Full clear.
		{"1.0 coverage with 50 completed is HIGH", 1.0, 50, "HIGH"},
	}
	for _, tc := range cases {
		if got := ProofStrength(tc.coverage, tc.completed); got != tc.want {
			t.Errorf("%s: ProofStrength(%v, %d) = %q, want %q",
				tc.name, tc.coverage, tc.completed, got, tc.want)
		}
	}
}

// TestGateQualityWindowStart pins the day-aligned anchors: 24H → start of
// the current UTC day, 7D → now−7d truncated to the UTC day; anything else
// is an error, not a guess.
func TestGateQualityWindowStart(t *testing.T) {
	now := time.Date(2026, 10, 1, 14, 37, 12, 500_000_000, time.UTC)
	start, err := gateQualityWindowStart(gateQualityWindow24H, now)
	if err != nil {
		t.Fatalf("24H window: %v", err)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("24H window start = %v, want %v", start, want)
	}
	start, err = gateQualityWindowStart(gateQualityWindow7D, now)
	if err != nil {
		t.Fatalf("7D window: %v", err)
	}
	if want := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("7D window start = %v, want %v", start, want)
	}
	// A non-UTC wall clock must anchor to the same UTC day.
	shifted := now.In(time.FixedZone("test", 3*3600))
	start, err = gateQualityWindowStart(gateQualityWindow24H, shifted)
	if err != nil {
		t.Fatalf("24H window (shifted zone): %v", err)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("24H window start (shifted zone) = %v, want %v", start, want)
	}
	if _, err := gateQualityWindowStart("30D", now); err == nil {
		t.Error("unknown window must be an error, not a guess")
	}
}

// gateQualitySummaryFixture builds a mixed 24H/7D row set: a high-|pnl|
// gate ranked first, a nil-pnl LOW-proof gate that must land in the
// "not enough data" line, and a LOW-proof gate WITH completed outcomes that
// must NOT (LOW proof alone is not "no data").
func gateQualitySummaryFixture() []gateQualityRow {
	pnl := func(v float64) *float64 { return &v }
	return []gateQualityRow{
		{windowKind: "24H", gate: "KNIFE_PAUSE", regime: "TREND_DOWN", episodes: 14,
			coverage: 0.62, completed: 12, blocked: pnl(-3.10), proof: "LOW"},
		{windowKind: "24H", gate: "STRESS", regime: "RANGE", episodes: 30,
			coverage: 0.83, completed: 40, blocked: pnl(4.20), proof: "MEDIUM"},
		{windowKind: "24H", gate: "ALLOW_CHAIN", regime: "RANGE", episodes: 9,
			coverage: 1.0, completed: 25, blocked: pnl(1.05), proof: "MEDIUM"},
		{windowKind: "24H", gate: "SQUEEZE", regime: "RANGE", episodes: 5,
			coverage: 0.30, completed: 0, blocked: nil, proof: "LOW"},
		{windowKind: "24H", gate: "OU_HALF_LIFE", regime: "TREND_UP", episodes: 7,
			coverage: 0.90, completed: 2, blocked: pnl(-0.40), proof: "LOW"},
		{windowKind: "7D", gate: "STRESS", regime: "RANGE", episodes: 120,
			coverage: 0.91, completed: 80, blocked: pnl(12.50), proof: "HIGH"},
	}
}

// TestBuildGateQualitySummaryFormat pins the Telegram text: both window
// sections, top-3 ranked by |blocked ±$| with regime/episodes/coverage/
// proof, the insufficient-data line, and the lost-observation counter.
func TestBuildGateQualitySummaryFormat(t *testing.T) {
	text := buildGateQualitySummary(gateQualitySummaryFixture(), 37)
	lines := strings.Split(text, "\n")

	if lines[0] != "🧪 Качество гейтов (24H):" {
		t.Errorf("first line = %q, want the 24H header", lines[0])
	}
	// 24H top-3 by |blocked|: STRESS 4.20 > KNIFE_PAUSE 3.10 > ALLOW_CHAIN
	// 1.05 — SQUEEZE (nil → 0) and OU_HALF_LIFE (0.40) stay out of the top.
	want24 := []string{
		"1. STRESS [RANGE] — эп. 30, cov 83%, model +4.20, proof MEDIUM",
		"2. KNIFE_PAUSE [TREND_DOWN] — эп. 14, cov 62%, model -3.10, proof LOW",
		"3. ALLOW_CHAIN [RANGE] — эп. 9, cov 100%, model +1.05, proof MEDIUM",
	}
	for i, want := range want24 {
		if lines[1+i] != want {
			t.Errorf("24H rank %d = %q, want %q", i+1, lines[1+i], want)
		}
	}
	// Insufficient-data line: only SQUEEZE qualifies (LOW AND completed=0);
	// OU_HALF_LIFE is LOW but has 2 completed outcomes, KNIFE_PAUSE has 12.
	insufficient := lines[4]
	if !strings.HasPrefix(insufficient, "мало данных (LOW, 0 исходов): SQUEEZE [RANGE]") {
		t.Errorf("insufficient line = %q, want SQUEEZE only", insufficient)
	}
	if strings.Contains(insufficient, "OU_HALF_LIFE") || strings.Contains(insufficient, "KNIFE_PAUSE") {
		t.Errorf("insufficient line must exclude gates with completed outcomes: %q", insufficient)
	}
	// 7D section carries its own header and its single row.
	if !strings.Contains(text, "\n🧪 Качество гейтов (7D):") {
		t.Errorf("7D header missing:\n%s", text)
	}
	if !strings.Contains(text, "1. STRESS [RANGE] — эп. 120, cov 91%, model +12.50, proof HIGH") {
		t.Errorf("7D top-1 line missing:\n%s", text)
	}
	// Lost-observation counter is the closing line.
	if got := lines[len(lines)-1]; got != "потерянные наблюдения: 37" {
		t.Errorf("closing line = %q, want the lost counter", got)
	}
}

// TestBuildGateQualitySummaryEmpty pins the empty-window branch: no rows →
// an explicit "нет наблюдений" line per window (the summary must say "no
// data", never stay silent), and a zero lost counter still prints.
func TestBuildGateQualitySummaryEmpty(t *testing.T) {
	text := buildGateQualitySummary(nil, 0)
	if strings.Count(text, "— нет наблюдений") != 2 {
		t.Errorf("both windows must report no observations:\n%s", text)
	}
	if !strings.HasSuffix(text, "потерянные наблюдения: 0") {
		t.Errorf("summary must end with the lost counter:\n%s", text)
	}
}

// TestBuildGateCalibration pins the §6 control-group math: below 5 pairs
// the verdict is "insufficient"; at 5+ pairs MAE and sign-match are
// computed overall and per direction, with sign(0)=0 matching sign(0)=0.
func TestBuildGateCalibration(t *testing.T) {
	insufficient := buildGateCalibration([]gateQualityPair{
		{direction: "LONG", model: 1.0, actual: 0.8},
		{direction: "LONG", model: -0.5, actual: -0.4},
		{direction: "SHORT", model: 0.2, actual: -0.1},
		{direction: "NEUTRAL", model: 0.0, actual: 0.0},
	})
	if got := insufficient["status"]; got != "insufficient" {
		t.Errorf("4 pairs: status = %v, want insufficient", got)
	}
	if got, ok := insufficient["pairs"].(int); !ok || got != 4 {
		t.Errorf("4 pairs: pairs = %v, want 4", insufficient["pairs"])
	}

	ok := buildGateCalibration([]gateQualityPair{
		{direction: "LONG", model: 2.0, actual: 1.0},    // |err| 1.0, signs match
		{direction: "LONG", model: -1.0, actual: -0.5},  // |err| 0.5, signs match
		{direction: "LONG", model: 0.0, actual: 0.0},    // |err| 0.0, sign(0)=sign(0)
		{direction: "SHORT", model: 0.5, actual: -0.5},  // |err| 1.0, signs differ
		{direction: "NEUTRAL", model: 1.0, actual: 0.9}, // |err| 0.1, signs match
	})
	if got := ok["status"]; got != "ok" {
		t.Errorf("5 pairs: status = %v, want ok", got)
	}
	// MAE = (1.0+0.5+0.0+1.0+0.1)/5 = 0.52; sign match = 4/5 = 0.8.
	if got := ok["mae"].(float64); math.Abs(got-0.52) > 1e-9 {
		t.Errorf("mae = %v, want 0.52", got)
	}
	if got := ok["sign_match"].(float64); math.Abs(got-0.8) > 1e-9 {
		t.Errorf("sign_match = %v, want 0.8", got)
	}
	byDirection, isMap := ok["by_direction"].(map[string]any)
	if !isMap {
		t.Fatalf("by_direction missing from %v", ok)
	}
	for direction, wantMAE := range map[string]float64{"LONG": 0.5, "SHORT": 1.0, "NEUTRAL": 0.1} {
		stats, isMap := byDirection[direction].(map[string]float64)
		if !isMap {
			t.Fatalf("direction %s stats missing from %v", direction, byDirection)
		}
		if math.Abs(stats["mae"]-wantMAE) > 1e-9 {
			t.Errorf("%s mae = %v, want %v", direction, stats["mae"], wantMAE)
		}
	}
}

// TestGateQualityTruncateGate pins the VARCHAR(32) guard: codes fit as-is,
// oversized codes cut at 32 runes without failing the upsert.
func TestGateQualityTruncateGate(t *testing.T) {
	if got := gateQualityTruncateGate("OFI_REENTRY_VETO"); got != "OFI_REENTRY_VETO" {
		t.Errorf("short gate mutated: %q", got)
	}
	long := strings.Repeat("G", 40)
	if got := gateQualityTruncateGate(long); len([]rune(got)) != gateQualityGateLen {
		t.Errorf("long gate len = %d, want %d", len([]rune(got)), gateQualityGateLen)
	}
}
