package autogrid

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gateMatcherFixtures pins one REAL persisted rejection text per matcher
// key (verbatim from marketdata/scanner.go, marketdata/targets.go,
// autogrid/worker.go, autogrid/entry_chain.go and autogrid/macro_gate.go).
// Every matcher in gateMatchers must have a fixture — adding a gate without
// a proven text fails the test.
var gateMatcherFixtures = map[string]string{
	"OU_HALF_LIFE":         "OU half-life 1.4ч < 2ч — режим не живёт достаточно долго, чтобы окупить издержки",
	"ANTI_FOMO":            "Anti-FOMO: RSI (32.9) < 40 - пара перепродана на лоях, вход в нейтральную сетку заблокирован",
	"MACRO":                "macro beta: BTC 24ч -4.2% — тренд-день вниз, NEUTRAL/LONG отложены",
	"CONFLUENCE_HURST":     "confluence veto: Hurst 0.61 > 0.58 — persistent trend regime, neutral grid would load one-sided inventory",
	"SEMI_TREND_DEAD_ZONE": "полутренд: ADX 27.4 в мёртвой зоне нейтрала 24-32 (недельный аудит: −$61 против +$9) — вход отложен",
	"TREND_TOO_STRONG":     "trend too strong for neutral grid (ADX: 35.1, EMA slope: 3.40%, 24h: +9.2%, 6h: +5.1%)",
	"VOLATILITY_CAP":       "volatility above risk threshold",
	"KNIFE_PAUSE":          "Knife Pause: 5-минутное падение 4.2% — деплой отложен для защиты от падающего ножа",
	"DEPTH_CUSHION":        "Стакан: bid-спред расширен — отказ по фильтру тонкой ликвидности",
	"LIQ_CASCADE":          "каскад ликвидаций лонгов $12M/час — входы LONG/NEUTRAL на паузе (SHORT доступны)",
	"MACRO_ALT_DRAIN":      "macro alt-drain: доминация BTC +0.62пп за 24ч при плоском BTC — альты понижаются, нейтральный сбор отложен",
	"SQUEEZE":              "volatility squeeze: impending explosive breakout",
	"CONFLUENCE_FLOW":      "confluence veto: flow supports SHORT (long 0.42 vs short 0.68)",
	"VOLATILITY_FLOOR":     "volatility below grid threshold",
	"FEES":                 "шаг уровня 0.24% < 2.5× round-trip издержек 0.10% — шаг ниже целевого буфера издержек (fee-gate)",
	"KAUFMAN_ER":           "Kaufman ER 0.64 — направленное движение, сетка не входит",
	"FLASH_SPIKE":          "recent flash candle spike (5.2%) - waiting for stabilization",
	"NON_ASCII_SYMBOL":     "не-ASCII символ пары: биржа отклоняет create для CJK-тикеров (P_TRADING_BOT_INVALID_ARGUMENT)",
	"VOLUME_FLOOR":         "24h quote turnover below limit",
	"MODEL_EV":             "model EV below limit",
	"MODEL_SHARPE":         "model Sharpe below limit",
	"MODEL_DRAWDOWN":       "model max drawdown above limit",
	"MODEL_PROFIT_FACTOR":  "model profit factor below limit",
}

// TestGateMatcherEveryKeyHits drives each matcher with its real persisted
// text: the fixture must attribute the sample to that gate.
func TestGateValueMatcherEveryKeyHits(t *testing.T) {
	if len(gateMatcherFixtures) != len(gateMatchers) {
		t.Fatalf("every matcher needs a fixture: %d matchers, %d fixtures",
			len(gateMatchers), len(gateMatcherFixtures))
	}
	for _, m := range gateMatchers {
		fixture, ok := gateMatcherFixtures[m.key]
		if !ok {
			t.Fatalf("matcher %s has no fixture reason", m.key)
		}
		keys := attributeGates(fixture)
		found := false
		for _, key := range keys {
			if key == m.key {
				found = true
			}
		}
		if !found {
			t.Errorf("reason %q must attribute to %s, got %v", fixture, m.key, keys)
		}
	}
}

// TestGateMatcherCyrillicAndCaseInsensitive pins the case-insensitive
// matching incl. Cyrillic: worker texts start "Стакан:" uppercase and the
// macro USD-event texts are pure Russian.
func TestGateValueMatcherCyrillicAndCaseInsensitive(t *testing.T) {
	for reason, want := range map[string]string{
		"Стакан: bid-спред расширен — отказ по фильтру тонкой ликвидности": "DEPTH_CUSHION",
		"entry gate: макро-событие USD «CPI» — редеплой отложен":           "MACRO",
	} {
		keys := attributeGates(reason)
		if len(keys) != 1 || keys[0] != want {
			t.Errorf("reason %q must attribute to [%s], got %v", reason, want, keys)
		}
	}
}

// TestGateMatcherCompoundReasonCreditsEveryGate pins the multi-gate rule:
// a compound "; "-joined scanner rejection attributes the sample to EVERY
// matched gate (each gate alone would have blocked the entry — the known,
// documented double-count).
func TestGateValueMatcherCompoundReasonCreditsEveryGate(t *testing.T) {
	keys := attributeGates("OU half-life 1.4ч < 2ч; Anti-FOMO: RSI (32.9) < 40")
	sort.Strings(keys)
	if strings.Join(keys, ",") != "ANTI_FOMO,OU_HALF_LIFE" {
		t.Fatalf("compound reason must credit both gates, got %v", keys)
	}
	// The macro overlap is intentional: "macro alt-drain" contains the
	// generic MACRO substring too, so the sample lands on both the specific
	// drain gate and the macro family.
	keys = attributeGates(gateMatcherFixtures["MACRO_ALT_DRAIN"])
	sort.Strings(keys)
	if strings.Join(keys, ",") != "MACRO,MACRO_ALT_DRAIN" {
		t.Fatalf("macro alt-drain must credit MACRO_ALT_DRAIN and MACRO, got %v", keys)
	}
	// CONFLUENCE_HURST needs BOTH parts: a flow veto without Hurst must
	// stay on the flow gate only.
	keys = attributeGates(gateMatcherFixtures["CONFLUENCE_FLOW"])
	if len(keys) != 1 || keys[0] != "CONFLUENCE_FLOW" {
		t.Fatalf("flow veto without Hurst must stay on CONFLUENCE_FLOW, got %v", keys)
	}
}

// TestGateMatcherUnknownFallsToOther pins the fallback: reasons no matcher
// recognizes (AI rejections, new gates, empty strings) attribute to OTHER
// so no shadow outcome is silently dropped.
func TestGateValueMatcherUnknownFallsToOther(t *testing.T) {
	for _, reason := range []string{
		"AI: риск-профиль не соответствует тезису",
		"entry gate: funding flush in progress, skip",
		"",
	} {
		keys := attributeGates(reason)
		if len(keys) != 1 || keys[0] != gateKeyOther {
			t.Errorf("reason %q must fall to OTHER, got %v", reason, keys)
		}
	}
}

// TestAggregateGateValuesSignConventions verifies the pure aggregation
// math: prevented_loss is the positive amount of blocked loss, missed
// profit the blocked gain, net = prevented − missed (positive pays), and
// compound rows are double-counted across gates by design.
func TestAggregateGateValuesSignConventions(t *testing.T) {
	stats := aggregateGateValues([]gateValueSample{
		{reason: gateMatcherFixtures["OU_HALF_LIFE"], pnl: -2.5},
		{reason: "OU half-life 1.9ч < 2ч; Anti-FOMO: RSI (32.9) < 40", pnl: -1.0},
		{reason: "OU half-life 1.7ч < 2ч; trend too strong", pnl: 1.2},
	})
	byGate := make(map[string]gateValueStat)
	for _, stat := range stats {
		byGate[stat.Gate] = stat
	}
	ou, ok := byGate["OU_HALF_LIFE"]
	if !ok {
		t.Fatalf("OU_HALF_LIFE missing from %v", stats)
	}
	if ou.Samples != 3 {
		t.Errorf("OU_HALF_LIFE samples = %d, want 3", ou.Samples)
	}
	if math.Abs(ou.PreventedLoss-3.5) > 1e-9 {
		t.Errorf("OU_HALF_LIFE prevented_loss = %v, want 3.5", ou.PreventedLoss)
	}
	if math.Abs(ou.MissedProfit-1.2) > 1e-9 {
		t.Errorf("OU_HALF_LIFE missed_profit = %v, want 1.2", ou.MissedProfit)
	}
	if math.Abs(ou.NetValue()-2.3) > 1e-9 {
		t.Errorf("OU_HALF_LIFE net_value = %v, want 2.3", ou.NetValue())
	}
	if math.Abs(ou.AvgOutcome()-(-2.3/3.0)) > 1e-9 {
		t.Errorf("OU_HALF_LIFE avg outcome = %v, want %.6f", ou.AvgOutcome(), -2.3/3.0)
	}
	if math.Abs(ou.WinRateBlocked()-1.0/3.0) > 1e-9 {
		t.Errorf("OU_HALF_LIFE win_rate_blocked = %v, want 1/3", ou.WinRateBlocked())
	}
	// The compound −1.0 row must also credit ANTI_FOMO and
	// TREND_TOO_STRONG (double-count by design).
	for key, want := range map[string]float64{"ANTI_FOMO": 1.0, "TREND_TOO_STRONG": -1.2} {
		stat, ok := byGate[key]
		if !ok {
			t.Fatalf("%s missing from compound attribution", key)
		}
		if stat.Samples != 1 || math.Abs(stat.NetValue()-want) > 1e-9 {
			t.Errorf("%s = {samples %d, net %v}, want {1, %v}", key, stat.Samples, stat.NetValue(), want)
		}
	}
}

// TestBuildGateValueReportNilDBWorker pins the nil-db guard: unit workers
// built without a pool must be a silent no-op, not a panic.
func TestBuildGateValueReportNilDBWorker(t *testing.T) {
	worker := &Worker{}
	worker.buildGateValueReport(context.Background())
	var nilWorker *Worker
	nilWorker.buildGateValueReport(context.Background())
}

// seedGateShadowOutcome lands one REJECTED candidate with the given
// rejection_reason plus an already-simulated shadow row carrying the
// counterfactual outcome. Cleanup flows through the scan-run delete.
func seedGateShadowOutcome(t *testing.T, pool *pgxpool.Pool, scanID, symbol, reason string, outcome float64) {
	t.Helper()
	ctx := context.Background()
	var candidateID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_candidates (
			scan_id, symbol, decision, score, rejection_reason,
			current_price, lower_price, upper_price, grid_num,
			recommended_trend, model_assumptions
		) VALUES (
			$1, $2, 'REJECTED', 90.5, $3, 100, 90, 110, 10,
			'no_trend', '{"atrPct": 1.0, "regime": "RANGE"}'::jsonb
		) RETURNING id
	`, scanID, symbol, reason).Scan(&candidateID); err != nil {
		t.Fatalf("insert candidate %s: %v", symbol, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO shadow_candidates (
			candidate_id, scan_id, symbol, direction, score,
			mesh_lower, mesh_upper, grid_num, entry_price,
			leverage, investment, fee_bps, captured_at,
			simulated, simulated_at, outcome_pnl_usdt, outcome_reason
		) VALUES (
			$1, $2, $3, 'NEUTRAL', 90.5,
			90, 110, 10, 100,
			2, 100, 15, NOW() - INTERVAL '25 hours',
			TRUE, NOW(), $4, 'WINDOW_CLOSE'
		)
	`, candidateID, scanID, symbol, outcome); err != nil {
		t.Fatalf("insert shadow row %s: %v", symbol, err)
	}
}

// gateValueSnapshotRow is the asserted slice of gate_value_snapshots.
type gateValueSnapshotRow struct {
	gate      string
	samples   int
	prevented float64
	missed    float64
	net       float64
	window    string
}

// TestBuildGateValueReportIntegration seeds two rejected candidates with
// simulated shadow outcomes — a loss the OU half-life gate blocked and a
// profit the Anti-FOMO gate blocked — runs the report and asserts the
// snapshots: OU_HALF_LIFE prevented 2.50 (net +2.50), ANTI_FOMO missed
// 1.20 (net −1.20), both tagged window=7d.
func TestBuildGateValueReportIntegration(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Self-sufficient schema (migration 0053, idempotent): the shared test
	// database may not have the newest migration applied yet.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS gate_value_snapshots (
		    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    gate VARCHAR(64) NOT NULL,
		    samples INT NOT NULL,
		    avg_outcome_pnl_usdt NUMERIC,
		    prevented_loss_usdt NUMERIC,
		    missed_profit_usdt NUMERIC,
		    net_value_usdt NUMERIC,
		    win_rate_blocked NUMERIC,
		    details JSONB NOT NULL DEFAULT '{}'::jsonb
		);
		CREATE INDEX IF NOT EXISTS idx_gate_value_created ON gate_value_snapshots (created_at DESC);
		CREATE INDEX IF NOT EXISTS idx_gate_value_gate ON gate_value_snapshots (gate, created_at DESC);
	`); err != nil {
		t.Fatalf("ensure gate_value_snapshots schema: %v", err)
	}
	started := time.Now()
	// Snapshots carry no FK — bound the cleanup to the rows this test wrote.
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx,
			`DELETE FROM gate_value_snapshots WHERE created_at >= $1`, started); err != nil {
			t.Errorf("cleanup snapshots: %v", err)
		}
	})

	// The worker logger records so the one-line observability contract
	// (top-5 by net value every run) is asserted, not just assumed.
	rec := &recordingHandler{}
	worker := NewWorker(pool, NewService(pool, risk.NewEngine(pool)),
		accounts.NewService(pool), risk.NewEngine(pool),
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(rec))

	var scanID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (status) VALUES ('SUCCEEDED') RETURNING id
	`).Scan(&scanID); err != nil {
		t.Fatalf("insert scan run: %v", err)
	}
	// The scan-run delete cascades autogrid_candidates, whose own delete
	// cascades shadow_candidates — one anchor for the whole fixture.
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM autogrid_scan_runs WHERE id = $1`, scanID); err != nil {
			t.Errorf("cleanup scan run: %v", err)
		}
	})

	seedGateShadowOutcome(t, pool, scanID, "GVALOU_USDT_PERP", "OU half-life 1.4ч < 2ч", -2.50)
	seedGateShadowOutcome(t, pool, scanID, "GVALAF_USDT_PERP", "Anti-FOMO: RSI (32.9) < 40", 1.20)

	worker.buildGateValueReport(ctx)

	// The report must always leave its one INFO line, with the seeded gates
	// ranked by net value (OU_HALF_LIFE +2.50 first, ANTI_FOMO −1.20 second).
	if !rec.contains("gate value report (7d shadow counterfactual)") {
		t.Fatalf("report must log the INFO line every run, logs: %s", rec.joined())
	}
	if !rec.contains("OU_HALF_LIFE:+2.50") || !rec.contains("ANTI_FOMO:-1.20") {
		t.Fatalf("top-5 ranking must carry the seeded net values, logs: %s", rec.joined())
	}

	for _, want := range []gateValueSnapshotRow{
		{gate: "OU_HALF_LIFE", samples: 1, prevented: 2.50, missed: 0, net: 2.50},
		{gate: "ANTI_FOMO", samples: 1, prevented: 0, missed: 1.20, net: -1.20},
	} {
		var got gateValueSnapshotRow
		err := pool.QueryRow(ctx, `
			SELECT gate, samples, prevented_loss_usdt, missed_profit_usdt,
			       net_value_usdt, details #>> '{window}'
			FROM gate_value_snapshots
			WHERE gate = $1 AND created_at >= $2
			ORDER BY created_at DESC
			LIMIT 1
		`, want.gate, started).Scan(&got.gate, &got.samples, &got.prevented,
			&got.missed, &got.net, &got.window)
		if err == pgx.ErrNoRows {
			t.Fatalf("gate %s must leave a snapshot row", want.gate)
		}
		if err != nil {
			t.Fatalf("load %s snapshot: %v", want.gate, err)
		}
		if got.samples != want.samples {
			t.Errorf("%s samples = %d, want %d", want.gate, got.samples, want.samples)
		}
		if math.Abs(got.prevented-want.prevented) > 1e-9 {
			t.Errorf("%s prevented_loss = %v, want %v", want.gate, got.prevented, want.prevented)
		}
		if math.Abs(got.missed-want.missed) > 1e-9 {
			t.Errorf("%s missed_profit = %v, want %v", want.gate, got.missed, want.missed)
		}
		if math.Abs(got.net-want.net) > 1e-9 {
			t.Errorf("%s net_value = %v, want %v", want.gate, got.net, want.net)
		}
		if got.window != gateValueWindow {
			t.Errorf("%s details window = %q, want %q", want.gate, got.window, gateValueWindow)
		}
	}
}
