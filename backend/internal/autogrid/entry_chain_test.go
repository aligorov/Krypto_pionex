package autogrid

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── pure unit: path constants, direction mapping, reason families ─────────

// TestEntryPathConstants pins the entry_decisions.path vocabulary to the
// migration-0052 contract — analytics group by these exact strings.
func TestEntryPathConstants(t *testing.T) {
	want := map[EntryPath]string{
		EntryPathScannerPaper: "SCANNER_PAPER",
		EntryPathScannerReal:  "SCANNER_REAL",
		EntryPathDGTReal:      "DGT_REAL",
		EntryPathDGTPaper:     "DGT_PAPER",
		EntryPathManualDeploy: "MANUAL_DEPLOY",
		EntryPathInvestIn:     "INVEST_IN",
		EntryPathTranche2:     "TRANCHE2",
	}
	for path, value := range want {
		if string(path) != value {
			t.Errorf("EntryPath %v = %q, want %q", path, string(path), value)
		}
	}
}

// TestEntryDirectionFromTrend pins the trend→Direction normalization for
// every vocabulary the callers feed it (scanner "no_trend", paper "neutral",
// stored uppercase directions, empty).
func TestEntryDirectionFromTrend(t *testing.T) {
	cases := map[string]string{
		"long": "LONG", "LONG": "LONG", " long ": "LONG",
		"short": "SHORT", "SHORT": "SHORT",
		"neutral": "NEUTRAL", "no_trend": "NEUTRAL", "": "NEUTRAL", "garbage": "NEUTRAL",
	}
	for in, want := range cases {
		if got := entryDirectionFromTrend(in); got != want {
			t.Errorf("entryDirectionFromTrend(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEntryBlockReasonsStableTexts pins the scanner-path reason families to
// the EXACT strings the inline call sites emitted — telegram/log texts must
// stay byte-stable across the composer migration.
func TestEntryBlockReasonsStableTexts(t *testing.T) {
	if got, want := circuitBreakerReason(EntryPathScannerPaper, 4),
		"circuit breaker: 4 защитных закрытий за последний час — новые деплои на паузе"; got != want {
		t.Errorf("paper breaker reason = %q, want %q", got, want)
	}
	if got, want := circuitBreakerReason(EntryPathScannerReal, 4),
		"REAL circuit breaker: 4 защитных закрытий за последний час — деплои на паузе"; got != want {
		t.Errorf("REAL breaker reason = %q, want %q", got, want)
	}
	if got, want := economicEventReason(EntryPathScannerPaper, "FOMC"),
		"деплой заблокирован: макро-событие USD «FOMC» (окно T−2ч…T+1ч)"; got != want {
		t.Errorf("paper econ reason = %q, want %q", got, want)
	}
	if got, want := economicEventReason(EntryPathScannerReal, "FOMC"),
		"REAL деплой заблокирован: макро-событие USD «FOMC» (окно T−2ч…T+1ч)"; got != want {
		t.Errorf("REAL econ reason = %q, want %q", got, want)
	}
	// Cascade: per-entry rejection vs pass-level note keep their historical
	// wording difference (the note says «деплои», the rejection «входы»).
	reason, note := cascadeReasons(EntryPathScannerPaper, 61_000_000)
	if want := "каскад ликвидаций лонгов $61M/час — входы LONG/NEUTRAL на паузе (SHORT доступны)"; reason != want {
		t.Errorf("paper cascade reason = %q, want %q", reason, want)
	}
	if want := "каскад ликвидаций лонгов $61M/час — LONG/NEUTRAL деплои на паузе, SHORT доступны"; note != want {
		t.Errorf("paper cascade note = %q, want %q", note, want)
	}
	reason, note = cascadeReasons(EntryPathScannerReal, 61_000_000)
	if want := "каскад ликвидаций лонгов $61M/час — входы LONG/NEUTRAL на паузе (SHORT доступны)"; reason != want {
		t.Errorf("REAL cascade reason = %q, want %q", reason, want)
	}
	if !strings.HasPrefix(note, "REAL: ") {
		t.Errorf("REAL cascade note must keep its REAL: prefix, got %q", note)
	}
	reason, note = feedHealthReasons(EntryPathScannerPaper, time.Time{})
	if want := "источник ликвидаций нестабилен (тишина >15м) — входы LONG/NEUTRAL на паузе до восстановления"; reason != want {
		t.Errorf("paper feed reason = %q, want %q", reason, want)
	}
	if want := "источник ликвидаций нестабилен (тишина >15м) — LONG/NEUTRAL деплои на паузе до восстановления"; note != want {
		t.Errorf("paper feed note = %q, want %q", note, want)
	}
}

// TestEvaluateSharedMarketBlockersStormFirstLeg: the storm leg runs before
// any DB touch — a nil pool with an active storm hook must still return the
// STORM code (entries defer; the journal/logs never depend on the gate's
// storage being reachable first).
func TestEvaluateSharedMarketBlockersStormFirstLeg(t *testing.T) {
	code, reason, _, _ := evaluateSharedMarketBlockersDB(context.Background(), nil, func() bool { return true },
		EntryChainInput{Path: EntryPathScannerReal, Fleet: "REAL"})
	if code != entryBlockedStorm {
		t.Fatalf("code = %q, want %q", code, entryBlockedStorm)
	}
	if reason == "" {
		t.Fatal("storm reason must be non-empty (it feeds noteDeployBlock/telegram texts)")
	}
}

// ── integration (disposable DB; skips without PIONEX_TEST_DATABASE_URL) ────

// entryChainFixture empties every table the composer reads and re-seeds the
// deterministic clean state: no economic calendar (the hard-coded FOMC
// window would masquerade as a block), no liquidations, a HEALTHY feed row
// (the gate is fail-closed — missing health reads as blocked), no CoinGecko
// window (macro fails open) and no seeded protective closes. integrationDatabaseURL
// already seeds the healthy bybit row for unrelated tests; this makes the
// state explicit per test.
type entryChainFixture struct {
	pool       *pgxpool.Pool
	worker     *Worker
	service    *Service
	settingsID string
}

func newEntryChainFixture(t *testing.T) *entryChainFixture {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	for _, stmt := range []string{
		`DELETE FROM economic_events`,
		`DELETE FROM fomc_meetings`,
		`DELETE FROM liquidation_events`,
		`DELETE FROM coingecko_snapshots`,
		`DELETE FROM entry_decisions`,
		`INSERT INTO liquidation_feed_health (source, connected, last_message_at)
		 VALUES ('bybit', true, NOW()) ON CONFLICT (source) DO UPDATE
		 SET connected = true, last_message_at = NOW()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture %q: %v", stmt, err)
		}
	}
	worker, service, settings := newCooldownTestWorker(t, pool)
	return &entryChainFixture{pool: pool, worker: worker, service: service, settingsID: settings.ID}
}

// seedPaperClose inserts one COMPLETED paper bot close with the given reason
// age (protective reasons arm the breaker; exempt reasons must not).
func (f *entryChainFixture) seedPaperClose(t *testing.T, symbol, reason string, age time.Duration) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, closed_reason, closed_at
		) VALUES (
			$1, $2, 'COMPLETED', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 10, 2, 100, 100, 100, $3, NOW() - $4::interval
		)
	`, f.settingsID, symbol, reason, age.String())
	if err != nil {
		t.Fatalf("seed paper close %s: %v", symbol, err)
	}
}

// TestEvaluateSharedMarketBlockersCleanDB: with every gate table in its
// deterministic clean state the composer must return "" for both fleets and
// both the worker and service variants.
func TestEvaluateSharedMarketBlockersCleanDB(t *testing.T) {
	f := newEntryChainFixture(t)
	for _, tc := range []struct {
		path  EntryPath
		fleet string
	}{
		{EntryPathScannerPaper, "PAPER"},
		{EntryPathScannerReal, "REAL"},
	} {
		in := EntryChainInput{Path: tc.path, Settings: Settings{ID: f.settingsID},
			Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: tc.fleet}
		if code, reason := f.worker.evaluateSharedMarketBlockers(context.Background(), in); code != "" || reason != "" {
			t.Errorf("%s worker composer on clean DB = (%q, %q), want clear", tc.path, code, reason)
		}
		if code, reason := f.service.evaluateSharedMarketBlockersSvc(context.Background(), in); code != "" || reason != "" {
			t.Errorf("%s svc composer on clean DB = (%q, %q), want clear", tc.path, code, reason)
		}
	}
}

// TestJointProtectiveClosesBreakerLeg pins THE single breaker reading:
// 2 protective closes do not arm it, 3 do (with the historical paper
// reason), and exempt closes never count.
func TestJointProtectiveClosesBreakerLeg(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	in := EntryChainInput{Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "PAPER"}

	f.seedPaperClose(t, "BRK_A_USDT_PERP", "STOP_LOSS", 10*time.Minute)
	f.seedPaperClose(t, "BRK_B_USDT_PERP", "STRUCT_INVALID", 20*time.Minute)
	if code, _ := f.worker.evaluateSharedMarketBlockers(ctx, in); code != "" {
		t.Fatalf("2 protective closes must not arm the breaker, got %q", code)
	}

	f.seedPaperClose(t, "BRK_C_USDT_PERP", "RANGE_BREAK_DOWN", 30*time.Minute)
	count, err := jointProtectiveClosesLastHour(ctx, f.pool, f.settingsID)
	if err != nil {
		t.Fatalf("jointProtectiveClosesLastHour: %v", err)
	}
	if count != 3 {
		t.Fatalf("joint count = %d, want 3", count)
	}
	code, reason := f.worker.evaluateSharedMarketBlockers(ctx, in)
	if code != entryBlockedCircuitBreaker {
		t.Fatalf("3 protective closes: code = %q, want %q", code, entryBlockedCircuitBreaker)
	}
	if want := "circuit breaker: 3 защитных закрытий за последний час — новые деплои на паузе"; reason != want {
		t.Fatalf("breaker reason = %q, want %q", reason, want)
	}

	// Profit takes (exempt reasons) must never arm the breaker.
	if _, err := f.pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE settings_id = $1`, f.settingsID); err != nil {
		t.Fatalf("clear closes: %v", err)
	}
	f.seedPaperClose(t, "BRK_TP1_USDT_PERP", "TAKE_PROFIT", 5*time.Minute)
	f.seedPaperClose(t, "BRK_TP2_USDT_PERP", "MANUAL_CLOSE", 5*time.Minute)
	f.seedPaperClose(t, "BRK_TP3_USDT_PERP", "GRID_AGED_HALF_LIFE", 5*time.Minute)
	if code, _ := f.worker.evaluateSharedMarketBlockers(ctx, in); code != "" {
		t.Fatalf("3 exempt closes must not arm the breaker, got %q", code)
	}
}

// TestEvaluateSharedMarketBlockersEconomicEvent pins the economic leg: a
// high-impact USD event inside T−2h blocks with the historical texts, and
// the empty calendar clears.
func TestEvaluateSharedMarketBlockersEconomicEvent(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country, source)
		VALUES ('Consumer Price Index', NOW() + INTERVAL '90 minutes', 'High', 'USD', 'FRED')
	`); err != nil {
		t.Fatalf("seed economic event: %v", err)
	}
	code, reason := f.worker.evaluateSharedMarketBlockers(ctx, EntryChainInput{
		Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "PAPER",
	})
	if code != entryBlockedEconomicEvent {
		t.Fatalf("code = %q, want %q", code, entryBlockedEconomicEvent)
	}
	if want := "деплой заблокирован: макро-событие USD «Consumer Price Index» (окно T−2ч…T+1ч)"; reason != want {
		t.Fatalf("paper econ reason = %q, want %q", reason, want)
	}
	if _, rsn := f.service.evaluateSharedMarketBlockersSvc(ctx, EntryChainInput{
		Path: EntryPathScannerReal, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "REAL",
	}); !strings.HasPrefix(rsn, "REAL деплой заблокирован") {
		t.Fatalf("REAL econ reason = %q, want the REAL-prefixed family", rsn)
	}
}

// TestEvaluateSharedMarketBlockersCascadeDirectionAware pins the cascade leg
// and its SHORT exemption: a >$50M long unwind blocks LONG/NEUTRAL with the
// per-candidate text while a SHORT entry stays live (the unwind window IS
// the short harvest).
func TestEvaluateSharedMarketBlockersCascadeDirectionAware(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO liquidation_events (symbol, side, value_usd, captured_at)
		VALUES ('BTC_USDT_PERP', 'long', 60000000, NOW() - INTERVAL '5 minutes')
	`); err != nil {
		t.Fatalf("seed cascade: %v", err)
	}
	base := EntryChainInput{Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Fleet: "PAPER", Symbol: "BTC_USDT_PERP"}
	neutral := base
	neutral.Direction = "NEUTRAL"
	code, reason, note, _ := f.worker.probeSharedMarketBlockers(ctx, neutral)
	if code != entryBlockedCascade {
		t.Fatalf("NEUTRAL during cascade: code = %q, want %q", code, entryBlockedCascade)
	}
	if want := "каскад ликвидаций лонгов $60M/час — входы LONG/NEUTRAL на паузе (SHORT доступны)"; reason != want {
		t.Fatalf("cascade reason = %q, want %q", reason, want)
	}
	if want := "каскад ликвидаций лонгов $60M/час — LONG/NEUTRAL деплои на паузе, SHORT доступны"; note != want {
		t.Fatalf("cascade note = %q, want %q", note, want)
	}
	long := base
	long.Direction = "LONG"
	if code, _ := f.worker.evaluateSharedMarketBlockers(ctx, long); code != entryBlockedCascade {
		t.Fatalf("LONG during cascade: code = %q, want %q", code, entryBlockedCascade)
	}
	short := base
	short.Direction = "SHORT"
	if code, rsn := f.worker.evaluateSharedMarketBlockers(ctx, short); code != "" || rsn != "" {
		t.Fatalf("SHORT during cascade = (%q, %q), want clear (SHORT exempt)", code, rsn)
	}
}

// TestEvaluateSharedMarketBlockersFeedHealthFailClosed pins the v2.0.119
// fail-closed contract through the composer: a missing feed-health row reads
// as FEED_HEALTH for LONG/NEUTRAL (never as "no cascade"), SHORT stays
// exempt.
func TestEvaluateSharedMarketBlockersFeedHealthFailClosed(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `DELETE FROM liquidation_feed_health`); err != nil {
		t.Fatalf("drop feed health: %v", err)
	}
	in := EntryChainInput{Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "PAPER"}
	if code, _ := f.worker.evaluateSharedMarketBlockers(ctx, in); code != entryBlockedFeedHealth {
		t.Fatalf("NEUTRAL with dead feed: code = %q, want %q", code, entryBlockedFeedHealth)
	}
	in.Direction = "SHORT"
	if code, _ := f.worker.evaluateSharedMarketBlockers(ctx, in); code != "" {
		t.Fatalf("SHORT with dead feed: code = %q, want clear (SHORT exempt)", code)
	}
}

// TestEntryDecisionJournalRoundTrip pins the journal write/read contract:
// path/fleet/symbol/outcome/code/reason/features round-trip, ref_id lands,
// and config_version carries the cv- fingerprint of the deciding config.
func TestEntryDecisionJournalRoundTrip(t *testing.T) {
	f := newEntryChainFixture(t)
	ctx := context.Background()
	in := EntryChainInput{Path: EntryPathScannerPaper, Settings: Settings{ID: f.settingsID},
		Symbol: "BTC_USDT_PERP", Direction: "NEUTRAL", Fleet: "PAPER", RefID: "cand-1"}
	journalEntryDecisionSvc(ctx, f.pool, in, entryOutcomeReject, entryBlockedCascade,
		"каскад ликвидаций лонгов $60M/час — входы LONG/NEUTRAL на паузе (SHORT доступны)",
		map[string]any{"usd_1h": 60_000_000.0})
	// The worker method shares the same insert path.
	f.worker.journalEntryDecision(ctx, EntryChainInput{Path: EntryPathTranche2,
		Settings: Settings{ID: f.settingsID}, Symbol: "ETH_USDT_PERP",
		Direction: "LONG", Fleet: "PAPER", RefID: "bot-9"}, entryOutcomeAllow, "CLEAR", "", nil)

	var outcome, code, configVersion, refID string
	var features map[string]any
	if err := f.pool.QueryRow(ctx, `
		SELECT outcome, code, config_version, ref_id, features
		FROM entry_decisions
		WHERE path = 'SCANNER_PAPER' AND symbol = 'BTC_USDT_PERP'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&outcome, &code, &configVersion, &refID, &features); err != nil {
		t.Fatalf("read REJECT row: %v", err)
	}
	if outcome != entryOutcomeReject || code != entryBlockedCascade || refID != "cand-1" {
		t.Fatalf("REJECT row = (%q, %q, %q), want (%q, %q, cand-1)",
			outcome, code, refID, entryOutcomeReject, entryBlockedCascade)
	}
	if usd, ok := features["usd_1h"].(float64); !ok || usd != 60_000_000.0 {
		t.Fatalf("features usd_1h = %v, want 60000000", features["usd_1h"])
	}
	if !strings.HasPrefix(configVersion, "cv-") {
		t.Fatalf("config_version = %q, want cv- prefix", configVersion)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT outcome, code FROM entry_decisions
		WHERE path = 'TRANCHE2' AND symbol = 'ETH_USDT_PERP'
		ORDER BY created_at DESC LIMIT 1
	`).Scan(&outcome, &code); err != nil {
		t.Fatalf("read ALLOW row: %v", err)
	}
	if outcome != entryOutcomeAllow || code != "CLEAR" {
		t.Fatalf("ALLOW row = (%q, %q), want (ALLOW, CLEAR)", outcome, code)
	}
}
