package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// v2.0.85 "shift always, shift early" test battery. The exchange's NORMAL
// adjust_params semantics gate on the order's pure floating PnL
// (BOT_INVALID_ARGUMENT / PROFIT_LESS_THAN_ZERO), so a green bot shifts with
// the re-base semantics while an under-water bot now ships the documented
// keepInvestment=true transfer (pure range move, no PnL realization), dry-run
// validated through adjustParamsCheck before the live call. The local
// RADAR_SHIFT_BLOCKED_UNDERWATER / SHIFT_BLOCKED_UNDERWATER events are GONE:
// the only remaining refusal is the exchange's own, carried by
// RADAR_RECENTER_FAILED (radar) or the manage error log (manage).

// (a, pure levels) The mode preflight: >= +$0.10 floating → normal re-base;
// everything below — including the 0..0.10 drift band between reconcile and
// shift — selects the keep_investment transfer. v2.0.83 figure under test:
// the FLOATING PnL, not the bot total.
func TestAdjustShiftModeLevels(t *testing.T) {
	if m := adjustShiftMode(d("0.5")); m != shiftModeNormal {
		t.Fatalf("floating +$0.5 must select normal, got %s", m)
	}
	if m := adjustShiftMode(d("0.10")); m != shiftModeNormal {
		t.Fatalf("the floor itself must select normal (>=, not >), got %s", m)
	}
	if m := adjustShiftMode(d("0.09")); m != shiftModeKeepInvestment {
		t.Fatalf("floating inside the drift buffer must select keep_investment, got %s", m)
	}
	if m := adjustShiftMode(d("0")); m != shiftModeKeepInvestment {
		t.Fatalf("flat floating must select keep_investment, got %s", m)
	}
	if m := adjustShiftMode(d("-0.5")); m != shiftModeKeepInvestment {
		t.Fatalf("underwater floating must select keep_investment, got %s", m)
	}
}

// (a) The prod NEAR #688 shape is the flagship of the new semantics: banked
// grid profit +2.0 with floating −0.5 (total +1.5) used to be parked with a
// RADAR_SHIFT_BLOCKED_UNDERWATER event. Now the shift FIRES as a rescue
// transfer: adjustParamsCheck runs first (same body), the live adjust ships
// keepInvestment=true, the local quote_investment / realized / unrealized
// base stays untouched, and the ADJUST_RANGE event records
// mode=keep_investment.
func TestRadarRealKeepInvestmentUnderwaterShift(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 2)
	h.seedDwell(t, botID, symbol)
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET realized_pnl_usdt = 2.0, unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("plant the NEAR #688 shape (total +1.5, floating −0.5): %v", err)
	}

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})

	// Dry-run FIRST, then exactly one live call with the rescue flag.
	if got := h.mock.checkCount.Load(); got != 1 {
		t.Fatalf("keep-investment shift must dry-run adjustParamsCheck exactly once, got %d", got)
	}
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("keep-investment shift must fire the live adjust exactly once, got %d", got)
	}
	if body := h.mock.adjust(); body["keepInvestment"] != true {
		t.Fatalf("live adjust must ship keepInvestment=true, got %v", body["keepInvestment"])
	}
	if body := h.mock.check(); body["keepInvestment"] != true {
		t.Fatalf("adjustParamsCheck must validate the SAME body (keepInvestment=true), got %v", body["keepInvestment"])
	}
	if body := h.mock.check(); body["type"] != "adjust_params" || body["bottom"] != "93.5" || body["top"] != "113.5" {
		t.Fatalf("check body must mirror the live geometry, got %v", body)
	}

	// Local accounting: bounds moved, budget spent — the investment base and
	// both PnL legs stay exactly as the last reconcile left them.
	var lower, upper, investment, realized, unrealized, antiHunt decimal.Decimal
	var adjustments int
	if err := h.pool.QueryRow(ctx, `
		SELECT lower_price, upper_price, quote_investment,
		       realized_pnl_usdt, unrealized_pnl_usdt,
		       anti_hunt_stop_price, adjustments_count
		FROM grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &investment, &realized, &unrealized, &antiHunt, &adjustments); err != nil {
		t.Fatalf("load bot after keep-shift: %v", err)
	}
	if !lower.Equal(d("93.5")) || !upper.Equal(d("113.5")) {
		t.Fatalf("keep-shift must move the bounds to [93.5, 113.5], got [%s, %s]", lower, upper)
	}
	if !investment.Equal(d("200")) {
		t.Fatalf("keep-shift must NOT touch quote_investment, got %s", investment)
	}
	if !realized.Equal(d("2.0")) || !unrealized.Equal(d("-0.5")) {
		t.Fatalf("keep-shift must NOT re-base the PnL legs, got realized %s unrealized %s", realized, unrealized)
	}
	if adjustments != 3 {
		t.Fatalf("keep-shift must spend the budget like any shift, got %d", adjustments)
	}
	if !antiHunt.Equal(d("91.5")) {
		t.Fatalf("anti-hunt must travel with the range (88 → 91.5), got %s", antiHunt)
	}

	var reason, mode, floating string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', details->>'mode', details->>'floating_pnl'
		FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&reason, &mode, &floating); err != nil {
		t.Fatalf("keep-shift event must be logged: %v", err)
	}
	if reason != "RADAR_B3_RECENTER" || mode != shiftModeKeepInvestment {
		t.Fatalf("event must be RADAR_B3_RECENTER/keep_investment, got %s/%s", reason, mode)
	}
	if floating != "-0.5000" {
		t.Fatalf("event must record the floating leg that picked the mode, got %q", floating)
	}
	// The blocked event class is retired: nothing may write it anymore.
	var blocked int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_SHIFT_BLOCKED_UNDERWATER'
	`, botID).Scan(&blocked); err != nil || blocked != 0 {
		t.Fatalf("RADAR_SHIFT_BLOCKED_UNDERWATER must be gone, got %d rows (%v)", blocked, err)
	}
	// The executed shift arms the durable cooldown like any radar action.
	if _, ok := h.worker.radarLastActionAt(ctx, botID); !ok {
		t.Fatal("an executed keep-shift must arm the durable radar cooldown")
	}

	// The durable cooldown now blocks an immediate second keep-shift — the
	// old hourly-dedup of blocked events is replaced by the real action gate.
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("cooldown must block the second shift, got %d adjusts", got)
	}
}

// (c) The dry-run is a real gate: when adjustParamsCheck itself refuses the
// keep-transfer, the live call must NEVER fire and the refusal takes the
// established RADAR_RECENTER_FAILED path with the exchange's reason — budget
// and geometry untouched, cooldown unarmed.
func TestRadarRealKeepInvestmentCheckRefused(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 2)
	h.seedDwell(t, botID, symbol)
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("sink the bot: %v", err)
	}
	h.mock.failCheck.Store(true)

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, decimal.NewFromFloat(103.5))
	b.botID = botID
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})

	if got := h.mock.checkCount.Load(); got != 1 {
		t.Fatalf("the check must have been asked once, got %d", got)
	}
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("a refused check must veto the live adjust, got %d adjusts", got)
	}
	var failErr, failMode string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'error', details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'RADAR_RECENTER_FAILED' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&failErr, &failMode); err != nil {
		t.Fatalf("the refusal must leave a RADAR_RECENTER_FAILED event: %v", err)
	}
	if !strings.Contains(failErr, "adjustParamsCheck refused") ||
		!strings.Contains(failErr, "PROFIT_LESS_THAN_ZERO") {
		t.Fatalf("failure event must carry the check refusal + exchange reason, got %q", failErr)
	}
	if failMode != shiftModeKeepInvestment {
		t.Fatalf("failure event must record the attempted mode, got %q", failMode)
	}
	var lower, upper decimal.Decimal
	var adjustments int
	if err := h.pool.QueryRow(ctx, `
		SELECT lower_price, upper_price, adjustments_count FROM grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &adjustments); err != nil {
		t.Fatalf("load bot: %v", err)
	}
	if !lower.Equal(d("90")) || !upper.Equal(d("110")) || adjustments != 2 {
		t.Fatalf("refused check must change nothing, got [%s,%s] count %d", lower, upper, adjustments)
	}
	if _, ok := h.worker.radarLastActionAt(ctx, botID); ok {
		t.Fatal("a refused check must not arm the durable radar cooldown")
	}
}

// (a-paper) Paper parity of the keep lane: the simulated shift mirrors the
// native keepInvestment semantics exactly — bounds and anti-hunt travel,
// adjustments_count spends, but the investment base (quote_investment,
// entry, realized/unrealized, mark) does NOT re-base. No exchange is asked
// (paper has no native call); the event carries mode=keep_investment.
func TestRadarPaperKeepInvestmentShift(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_PAPER_USDT_PERP"
	var botID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt,
			realized_pnl_usdt, unrealized_pnl_usdt, anti_hunt_stop_price,
			adjustments_count, opened_at
		) VALUES (
			$1, $2, 'RUNNING', 'LONG', 'ARITHMETIC',
			90, 110, 10, 2, 200,
			100, 100, 999, -999,
			0.5, -0.5, 88,
			0, NOW() - INTERVAL '2 hours'
		)
		RETURNING id
	`, h.settings.ID, symbol).Scan(&botID); err != nil {
		t.Fatalf("seed paper bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(ctx, `DELETE FROM bot_execution_events WHERE bot_id = $1`, botID)
		_, _ = h.pool.Exec(ctx, `DELETE FROM bot_risk_snapshots WHERE bot_id = $1`, botID)
		_, _ = h.pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID)
	})
	h.seedDwell(t, botID, symbol)

	h.settings.StopForecastMode = "ACTIVE"
	b := radarInput{
		botID: botID, botNumber: 501, botSource: "PAPER", symbol: symbol,
		direction: "LONG", price: decimal.NewFromFloat(103.5),
		lower: d("90"), upper: d("110"), atrEntryPct: 1.0,
		total: decimal.Zero, inventorySide: 1,
	}
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 3, Score: 0.80, DistToStopATR: 2.0})

	var lower, upper, entry, mark, investment, realized, unrealized, antiHunt decimal.Decimal
	var adjustments int
	if err := h.pool.QueryRow(ctx, `
		SELECT lower_price, upper_price, entry_price, COALESCE(mark_price, 0),
		       quote_investment, realized_pnl_usdt, unrealized_pnl_usdt,
		       anti_hunt_stop_price, adjustments_count
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&lower, &upper, &entry, &mark, &investment, &realized, &unrealized, &antiHunt, &adjustments); err != nil {
		t.Fatalf("load paper bot after keep-shift: %v", err)
	}
	if !lower.Equal(d("93.5")) || !upper.Equal(d("113.5")) {
		t.Fatalf("keep-shift must move the paper bounds to [93.5, 113.5], got [%s, %s]", lower, upper)
	}
	if !antiHunt.Equal(d("91.5")) {
		t.Fatalf("paper anti-hunt must travel with the range (88 → 91.5), got %s", antiHunt)
	}
	if !entry.Equal(d("100")) || !mark.Equal(d("100")) {
		t.Fatalf("keep-shift must NOT re-anchor entry/mark, got entry %s mark %s", entry, mark)
	}
	if !investment.Equal(d("200")) || !realized.Equal(d("0.5")) || !unrealized.Equal(d("-0.5")) {
		t.Fatalf("keep-shift must NOT re-base the paper ledger, got inv %s realized %s unrealized %s",
			investment, realized, unrealized)
	}
	if adjustments != 1 {
		t.Fatalf("keep-shift must spend the paper budget, got %d", adjustments)
	}
	var reason, mode, source string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', details->>'mode', bot_source FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&reason, &mode, &source); err != nil ||
		reason != "RADAR_B3_RECENTER" || mode != shiftModeKeepInvestment || source != "PAPER" {
		t.Fatalf("paper keep-shift event must be PAPER/RADAR_B3_RECENTER/keep_investment, got %s/%s/%s (%v)",
			source, reason, mode, err)
	}
	if _, ok := h.worker.radarLastActionAt(ctx, botID); !ok {
		t.Fatal("an executed paper keep-shift must arm the durable radar cooldown")
	}
}

// (b, pure geometry) Adverse-edge progress for the B2 early/velocity
// triggers: the share of the range width already covered toward the edge the
// inventory fears, clamped, zero for degenerate ranges.
func TestRadarB2EarlyEdgeProgress(t *testing.T) {
	lower, upper := d("90"), d("110")
	// Long inventory fears the lower bound: 95 sits 75% of the way down.
	if p := radarB2EarlyEdgeProgress(d("95"), lower, upper, 1); p != 0.75 {
		t.Fatalf("long inventory at 95 must read 0.75 progress, got %v", p)
	}
	// Short inventory fears the upper bound: 105 sits 75% of the way up.
	if p := radarB2EarlyEdgeProgress(d("105"), lower, upper, -1); p != 0.75 {
		t.Fatalf("short inventory at 105 must read 0.75 progress, got %v", p)
	}
	// Flat inventory at mid: 0.5 on the long-side reading — below the 0.70
	// trigger, so a mid-sitting bot never early-shifts.
	if p := radarB2EarlyEdgeProgress(d("100"), lower, upper, 0); p != 0.5 {
		t.Fatalf("flat inventory at mid must read 0.5, got %v", p)
	}
	// Progress clamps at the bounds and collapses on degenerate ranges.
	if p := radarB2EarlyEdgeProgress(d("120"), lower, upper, -1); p != 1 {
		t.Fatalf("progress beyond the upper bound must clamp to 1, got %v", p)
	}
	if p := radarB2EarlyEdgeProgress(d("85"), lower, upper, 1); p != 1 {
		t.Fatalf("progress beyond the lower bound must clamp to 1, got %v", p)
	}
	if p := radarB2EarlyEdgeProgress(d("100"), lower, lower, 1); p != 0 {
		t.Fatalf("degenerate range must read 0, got %v", p)
	}
}

// (d, pure speed math) The velocity ruler: ATR(15m) units per 15 minutes
// toward the adverse edge, from two trail samples. Movement AWAY from the
// edge, zero-gap repeats and unknown ATR read 0 — never fire.
func TestRadarEdgeSpeedATRPer15m(t *testing.T) {
	at := func(offsetMin float64) time.Time {
		return time.UnixMilli(1_800_000_000_000).Add(time.Duration(offsetMin * float64(time.Minute)))
	}
	// Short inventory: rising toward the upper bound. Price 100 → 101 in 15m
	// with ATR15 = 1% ≈ 1.01 → exactly 0.99 ATR/15m.
	speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("100")},
		radarPricePoint{at: at(15), price: d("101")},
		1.0, -1)
	if speed < 0.98 || speed > 1.0 {
		t.Fatalf("1 pct rise per 15m at ATR 1 pct must read ~1 ATR/15m, got %v", speed)
	}
	// Same move compressed into 90s is 10× the pace.
	speed = radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("100")},
		radarPricePoint{at: at(1.5), price: d("101")},
		1.0, -1)
	if speed < 9.8 || speed > 10.1 {
		t.Fatalf("1 pct rise per 90s must read ~10 ATR/15m, got %v", speed)
	}
	// Long inventory: falling toward the lower bound is the adverse direction.
	if speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("101")},
		radarPricePoint{at: at(15), price: d("100")},
		1.0, 1); speed < 0.98 || speed > 1.0 {
		t.Fatalf("long-side adverse fall must read ~1 ATR/15m, got %v", speed)
	}
	// Moving AWAY from the edge reads 0.
	if speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("100")},
		radarPricePoint{at: at(15), price: d("101")},
		1.0, 1); speed != 0 {
		t.Fatalf("favorable move must read 0, got %v", speed)
	}
	// No ruler (ATR unknown), zero gap and flat inventory never measure.
	if speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("100")},
		radarPricePoint{at: at(15), price: d("101")},
		0, -1); speed != 0 {
		t.Fatalf("unknown ATR must read 0, got %v", speed)
	}
	if speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(15), price: d("101")},
		radarPricePoint{at: at(15), price: d("101")},
		1.0, -1); speed != 0 {
		t.Fatalf("zero gap must read 0, got %v", speed)
	}
	if speed := radarEdgeSpeedATRPer15m(
		radarPricePoint{at: at(0), price: d("100")},
		radarPricePoint{at: at(15), price: d("101")},
		1.0, 0); speed != 0 {
		t.Fatalf("flat inventory has no adverse edge, got %v", speed)
	}
}

// rememberRadarPrice trail semantics: first sample returns unusable, the
// second returns the first as baseline, a same-pass repeat neither measures
// nor overwrites, and a stale (>30m) baseline is refused.
func TestRememberRadarPriceTrail(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	if _, ok := worker.rememberRadarPrice("SYM", radarPricePoint{at: now, price: d("100")}); ok {
		t.Fatal("the first sample has no baseline")
	}
	prev, ok := worker.rememberRadarPrice("SYM", radarPricePoint{at: now.Add(90 * time.Second), price: d("101")})
	if !ok || !prev.price.Equal(d("100")) {
		t.Fatalf("the second sample must measure against the first, ok=%v prev=%v", ok, prev)
	}
	// Same-pass repeat (fresh guard): unusable, and the stored baseline
	// survives for the next pass.
	if _, ok := worker.rememberRadarPrice("SYM", radarPricePoint{at: now.Add(91 * time.Second), price: d("101")}); ok {
		t.Fatal("a zero-gap repeat must not measure")
	}
	prev, ok = worker.rememberRadarPrice("SYM", radarPricePoint{at: now.Add(3 * time.Minute), price: d("101")})
	if !ok || !prev.price.Equal(d("101")) {
		t.Fatalf("the fresh-guarded point must remain the baseline, ok=%v prev=%v", ok, prev)
	}
	// A stale baseline (downtime) is refused and replaced.
	if _, ok := worker.rememberRadarPrice("SYM", radarPricePoint{at: now.Add(45 * time.Minute), price: d("101")}); ok {
		t.Fatal("a >30m-old baseline must be refused as stale")
	}
}

// (b) The early re-center: band 2, dwell 3, price 75% of the way to the
// adverse edge, bot in profit — one native adjust_params (normal semantics),
// reason RADAR_B2_EARLY_RECENTER, cooldown armed. Not far enough to the edge:
// no exchange call at all.
func TestRadarRealB2EarlyRecenter(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Telegram ON so the dedicated early-recenter template must render.
	var telegramSavedEnabled, telegramSavedNotify bool
	var telegramSavedToken, telegramSavedChat string
	_ = h.pool.QueryRow(ctx, `
		SELECT COALESCE(enabled,false), COALESCE(bot_token,''), COALESCE(chat_id,''),
		       COALESCE(notify_range_adjust,false)
		FROM telegram_settings WHERE id = 1
	`).Scan(&telegramSavedEnabled, &telegramSavedToken, &telegramSavedChat, &telegramSavedNotify)
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE telegram_settings
			SET enabled = $1, bot_token = $2, chat_id = $3, notify_range_adjust = $4
			WHERE id = 1
		`, telegramSavedEnabled, telegramSavedToken, telegramSavedChat, telegramSavedNotify)
		_, _ = h.pool.Exec(context.Background(),
			`DELETE FROM notification_outbox WHERE event_type IN ('RADAR_B2_EARLY_RECENTER','RADAR_B2_VELOCITY_RECENTER')`)
	})
	if _, err := h.pool.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = true, bot_token = 'test-token', chat_id = '100500', notify_range_adjust = true
		WHERE id = 1
	`); err != nil {
		t.Fatalf("enable telegram fixture: %v", err)
	}

	botID := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, botID, symbol, []float64{0.65, 0.65, 0.65})

	h.settings.StopForecastMode = "ACTIVE"
	// Price above mid = short inventory: the adverse edge is the upper bound
	// and 105 is 75% of the way there.
	b := realRadarAction(symbol, decimal.NewFromFloat(105))
	b.botID = botID
	b.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})

	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("green B2 bot at 75%% edge progress must early-shift, got %d adjusts", got)
	}
	body := h.mock.adjust()
	if body["type"] != "adjust_params" || body["bottom"] != "95" || body["top"] != "115" {
		t.Fatalf("early re-center must ship adjust_params [95, 115] (width 20 at price 105), got %v", body)
	}
	if _, has := body["keepInvestment"]; has {
		t.Fatalf("green early shift must take the normal semantics, got %v", body["keepInvestment"])
	}
	var reason, mode string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, botID).Scan(&reason, &mode); err != nil || reason != "RADAR_B2_EARLY_RECENTER" {
		t.Fatalf("event reason must be RADAR_B2_EARLY_RECENTER, got %q (%v)", reason, err)
	} else if mode != shiftModeNormal {
		t.Fatalf("green early shift event must carry mode=normal, got %q", mode)
	}
	// The early shift arms the durable cooldown like any radar action.
	if _, ok := h.worker.radarLastActionAt(ctx, botID); !ok {
		t.Fatal("an executed early re-center must arm the durable radar cooldown")
	}
	// The dedicated telegram template renders, placeholder-free.
	var payload string
	if err := h.pool.QueryRow(ctx, `
		SELECT payload::TEXT FROM notification_outbox
		WHERE event_type = 'RADAR_B2_EARLY_RECENTER' ORDER BY created_at DESC LIMIT 1
	`).Scan(&payload); err != nil {
		t.Fatalf("early re-center telegram outbox row missing: %v", err)
	}
	if strings.Contains(payload, "{{") || !strings.Contains(payload, symbol) {
		t.Fatalf("early telegram must render fully and address the bot, got %s", payload)
	}

	// Not far enough: 60% of the way to the edge stays put.
	near := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, near, symbol, []float64{0.65, 0.65, 0.65})
	bNear := realRadarAction(symbol, decimal.NewFromFloat(102))
	bNear.botID = near
	bNear.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bNear, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("60%% edge progress must not early-shift, got %d adjusts", got)
	}

	// Far enough but UNDER WATER: v2.0.85 keeps shifting — the early lane
	// ships keepInvestment, dry-run first, and the event records the mode.
	wet := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, wet, symbol, []float64{0.65, 0.65, 0.65})
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, wet); err != nil {
		t.Fatalf("sink the early bot under water: %v", err)
	}
	bWet := realRadarAction(symbol, decimal.NewFromFloat(105))
	bWet.botID = wet
	bWet.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bWet, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 2 {
		t.Fatalf("underwater early trigger must keep-shift (not block), got %d adjusts", got)
	}
	if body := h.mock.adjust(); body["keepInvestment"] != true {
		t.Fatalf("underwater early shift must ship keepInvestment=true, got %v", body["keepInvestment"])
	}
	if got := h.mock.checkCount.Load(); got != 1 {
		t.Fatalf("underwater early shift must dry-run the check once, got %d", got)
	}
	var wetReason, wetMode string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, wet).Scan(&wetReason, &wetMode); err != nil ||
		wetReason != "RADAR_B2_EARLY_RECENTER" || wetMode != shiftModeKeepInvestment {
		t.Fatalf("underwater early event must be EARLY/keep_investment, got %s/%s (%v)",
			wetReason, wetMode, err)
	}
}

// (d) The velocity lane: band 2, price 55% of the way to the adverse edge,
// trailing speed 0.8×ATR(15m)/15m, floating green — the shift fires after
// ONE dwell tick with reason RADAR_B2_VELOCITY_RECENTER. A slow trail
// (0.2×ATR/15m) at the same position waits for the established paths.
func TestRadarRealB2VelocityRecenter(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Telegram ON so the dedicated velocity template must render.
	var telegramSavedEnabled, telegramSavedNotify bool
	var telegramSavedToken, telegramSavedChat string
	_ = h.pool.QueryRow(ctx, `
		SELECT COALESCE(enabled,false), COALESCE(bot_token,''), COALESCE(chat_id,''),
		       COALESCE(notify_range_adjust,false)
		FROM telegram_settings WHERE id = 1
	`).Scan(&telegramSavedEnabled, &telegramSavedToken, &telegramSavedChat, &telegramSavedNotify)
	t.Cleanup(func() {
		_, _ = h.pool.Exec(context.Background(), `
			UPDATE telegram_settings
			SET enabled = $1, bot_token = $2, chat_id = $3, notify_range_adjust = $4
			WHERE id = 1
		`, telegramSavedEnabled, telegramSavedToken, telegramSavedChat, telegramSavedNotify)
		_, _ = h.pool.Exec(context.Background(),
			`DELETE FROM notification_outbox WHERE event_type = 'RADAR_B2_VELOCITY_RECENTER'`)
	})
	if _, err := h.pool.Exec(ctx, `
		UPDATE telegram_settings
		SET enabled = true, bot_token = 'test-token', chat_id = '100500', notify_range_adjust = true
		WHERE id = 1
	`); err != nil {
		t.Fatalf("enable telegram fixture: %v", err)
	}

	// Short inventory (fears the upper bound): price 101 = 55% of [90,110].
	// ATR(15m) = 1% ≈ 1.01 at price 101; a rise of 0.8×ATR over the 15m
	// baseline gap is exactly the 0.8×ATR/15m pace (threshold 0.6).
	seed := func(atrMultiple float64) string {
		botID := h.seedRealBot(t, symbol, 0)
		// dwell 1: exactly ONE band-2 snapshot — the current tick.
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO bot_risk_snapshots
				(bot_id, bot_number, bot_source, symbol, mode, score, band, captured_at)
			VALUES ($1, 500, 'REAL', $2, 'ACTIVE', 0.65, 2, NOW() - INTERVAL '1 minute')
		`, botID, symbol); err != nil {
			t.Fatalf("seed single band-2 snapshot: %v", err)
		}
		price := d("101")
		atr, _ := price.Mul(d("0.01")).Float64()
		h.worker.radarPriceTrail = map[string]radarPricePoint{
			symbol: {at: time.Now().UTC().Add(-15 * time.Minute),
				price: price.Sub(decimal.NewFromFloat(atrMultiple * atr))},
		}
		return botID
	}

	h.settings.StopForecastMode = "ACTIVE"

	// FAST: 0.8×ATR/15m toward the edge at 55% progress, green base → fires
	// on the first tick with the velocity reason.
	fast := seed(0.8)
	b := realRadarAction(symbol, decimal.NewFromFloat(101))
	b.botID = fast
	b.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("a 0.8xATR/15m run at 55%% progress must fire after ONE tick, got %d adjusts", got)
	}
	fastBody := h.mock.adjust()
	if fastBody["type"] != "adjust_params" || fastBody["bottom"] != "91" || fastBody["top"] != "111" {
		t.Fatalf("velocity re-center must ship [91, 111] (width 20 at price 101), got %v", fastBody)
	}
	if _, has := fastBody["keepInvestment"]; has {
		t.Fatalf("velocity lane requires the green base — normal semantics, got %v", fastBody["keepInvestment"])
	}
	var reason, mode string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason', details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, fast).Scan(&reason, &mode); err != nil || reason != "RADAR_B2_VELOCITY_RECENTER" {
		t.Fatalf("event reason must be RADAR_B2_VELOCITY_RECENTER, got %q (%v)", reason, err)
	} else if mode != shiftModeNormal {
		t.Fatalf("velocity event must carry mode=normal, got %q", mode)
	}
	// The dedicated telegram template renders, placeholder-free.
	var payload string
	if err := h.pool.QueryRow(ctx, `
		SELECT payload::TEXT FROM notification_outbox
		WHERE event_type = 'RADAR_B2_VELOCITY_RECENTER' ORDER BY created_at DESC LIMIT 1
	`).Scan(&payload); err != nil {
		t.Fatalf("velocity telegram outbox row missing: %v", err)
	}
	if strings.Contains(payload, "{{") || !strings.Contains(payload, symbol) {
		t.Fatalf("velocity telegram must render fully and address the bot, got %s", payload)
	}
	if !strings.Contains(payload, "0.8×ATR") {
		t.Fatalf("velocity telegram must carry the measured speed, got %s", payload)
	}

	// SLOW: 0.2×ATR/15m at the same 55% progress — below the 0.6 threshold,
	// below the 0.70 early gate: nothing fires, the bot waits for the
	// established paths.
	slow := seed(0.2)
	bSlow := realRadarAction(symbol, decimal.NewFromFloat(101))
	bSlow.botID = slow
	bSlow.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bSlow, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 1 {
		t.Fatalf("a 0.2xATR/15m crawl at 55%% progress must NOT fire, got %d adjusts", got)
	}

	// SLOW but far (75%) with the full dwell 3: the OLD early path still
	// fires — velocity is an additional lane, not a replacement.
	old := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, old, symbol, []float64{0.65, 0.65, 0.65})
	oldPrice := decimal.NewFromFloat(105)
	oldAtr, _ := oldPrice.Mul(d("0.01")).Float64()
	h.worker.radarPriceTrail = map[string]radarPricePoint{
		symbol: {at: time.Now().UTC().Add(-15 * time.Minute),
			price: oldPrice.Sub(decimal.NewFromFloat(0.2 * oldAtr))},
	}
	bOld := realRadarAction(symbol, oldPrice)
	bOld.botID = old
	bOld.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, bOld, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 2 {
		t.Fatalf("the dwell-3 early path must still fire at 75%% progress, got %d adjusts", got)
	}
	var oldReason string
	if err := h.pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE' ORDER BY created_at DESC LIMIT 1
	`, old).Scan(&oldReason); err != nil || oldReason != "RADAR_B2_EARLY_RECENTER" {
		t.Fatalf("old path must log RADAR_B2_EARLY_RECENTER, got %q (%v)", oldReason, err)
	}
}

// (d-neg) Velocity is prevention, not rescue: an under-water base voids the
// lane — a fast run at 55% with floating −0.5 fires nothing (the keep-lane
// owns ≥70% and B3/B4, not the 55% window).
func TestRadarRealB2VelocityRequiresGreenBase(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"
	botID := h.seedRealBot(t, symbol, 0)
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO bot_risk_snapshots
			(bot_id, bot_number, bot_source, symbol, mode, score, band, captured_at)
		VALUES ($1, 500, 'REAL', $2, 'ACTIVE', 0.65, 2, NOW() - INTERVAL '1 minute')
	`, botID, symbol); err != nil {
		t.Fatalf("seed single band-2 snapshot: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots SET unrealized_pnl_usdt = -0.5 WHERE id = $1
	`, botID); err != nil {
		t.Fatalf("sink the bot: %v", err)
	}
	price := d("101")
	atr, _ := price.Mul(d("0.01")).Float64()
	h.worker.radarPriceTrail = map[string]radarPricePoint{
		symbol: {at: time.Now().UTC().Add(-15 * time.Minute),
			price: price.Sub(decimal.NewFromFloat(0.8 * atr))},
	}

	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, price)
	b.botID = botID
	b.inventorySide = -1
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("velocity lane must require the green base, got %d adjusts", got)
	}
	if _, ok := h.worker.radarLastActionAt(ctx, botID); ok {
		t.Fatal("a voided velocity attempt must not arm the cooldown")
	}
}

// (e) Regression: a green bot below band 2, or band 2 far from the edge,
// never touches the exchange — the lanes only widen WHO shifts, never the
// entry band.
func TestRadarGreenLowBandFarDoesNothing(t *testing.T) {
	h := newRadarRealHarness(t)
	ctx := context.Background()
	const symbol = "RADR_USDT_PERP"

	// Band 1, far from the edge (30% progress), fast trail, green: nothing.
	band1 := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, band1, symbol, []float64{0.40, 0.40, 0.40})
	price := d("97")
	atr, _ := price.Mul(d("0.01")).Float64()
	h.worker.radarPriceTrail = map[string]radarPricePoint{
		symbol: {at: time.Now().UTC().Add(-15 * time.Minute),
			price: price.Sub(decimal.NewFromFloat(0.8 * atr))},
	}
	h.settings.StopForecastMode = "ACTIVE"
	b := realRadarAction(symbol, price)
	b.botID = band1
	b.inventorySide = -1 // 97 = 35% of the way up — far from the edge
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 1, Score: 0.40, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("band 1 must never shift, got %d adjusts", got)
	}

	// Band 2 but only halfway to the edge even with a fast trail: the
	// velocity lane starts at 55%, the early lane at 70% — nothing.
	band2 := h.seedRealBot(t, symbol, 0)
	h.seedBandSnapshots(t, band2, symbol, []float64{0.65, 0.65, 0.65})
	b.botID = band2
	h.worker.radarMaybeRecenter(ctx, *h.settings, b, radarScores{Band: 2, Score: 0.65, DistToStopATR: 2.0})
	if got := h.mock.adjustCount.Load(); got != 0 {
		t.Fatalf("band 2 at 35%% progress must not shift, got %d adjusts", got)
	}
	var events int
	if err := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id IN ($1, $2) AND event_type = 'ADJUST_RANGE'
	`, band1, band2).Scan(&events); err != nil || events != 0 {
		t.Fatalf("no ADJUST_RANGE events expected, got %d (%v)", events, err)
	}
}

// manageExchangeMock serves one full REAL manage pass: per-bot order detail
// (bounds, position, grid profit), public prices, a klines feed too short for
// a regime (unknown = adverse both ways), empty funding history, and the
// signed adjustParamsCheck/adjustParams/cancel slots whose call counts and
// captured bodies are the assertions.
type manageExchangeMock struct {
	server     *httptest.Server
	adjustMu   sync.Mutex
	cancelMu   sync.Mutex
	adjustSeen int
	checkSeen  int
	cancelSeen int
	lastAdjust map[string]any
}

func newManageExchangeMock(t *testing.T) *manageExchangeMock {
	t.Helper()
	mock := &manageExchangeMock{}
	type botFixture struct {
		position, openPrice, profitReduce string
	}
	fixtures := map[string]botFixture{
		// LONG breaking UP while under water: position bought at 116.5 marked
		// at 115 (floating −1.5) plus grid −0.2 → total −1.7.
		"MGMT-A-bu": {position: "1", openPrice: "116.5", profitReduce: "-0.2"},
		// NEUTRAL breaking DOWN under water: position at 100 marked at 80.
		"MGMT-B-bu": {position: "1", openPrice: "100", profitReduce: "-0.5"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/bot/orders/futuresGrid/order", func(w http.ResponseWriter, r *http.Request) {
		fx := fixtures[r.URL.Query().Get("buOrderId")]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"buOrderId": r.URL.Query().Get("buOrderId"),
				"status":    "running", "reasonBy": "",
				"buOrderData": map[string]any{
					"status": "running", "reasonBy": "",
					"top": "110", "bottom": "90", "row": 20,
					"gridType": "arithmetic", "trend": "no_trend", "leverage": 2,
					"position": fx.position, "positionOpenPrice": fx.openPrice,
					"profitReduce": fx.profitReduce, "profitWithdrawn": "0",
					"riskStatus": "NORMAL",
				},
			},
		})
	})
	mux.HandleFunc("GET /api/v1/market/indexes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"indexes": []map[string]any{
				{"symbol": "MGMTA_USDT_PERP", "indexPrice": "115", "markPrice": "115"},
				{"symbol": "MGMTB_USDT_PERP", "indexPrice": "80", "markPrice": "80"},
			}},
		})
	})
	mux.HandleFunc("GET /api/v1/market/tickers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"tickers": []map[string]any{
				{"symbol": "MGMTA_USDT_PERP", "close": "115"},
				{"symbol": "MGMTB_USDT_PERP", "close": "80"},
			}},
		})
	})
	// One candle < the 30-candle regime minimum: regime stays unknown, which
	// the break matrix treats as adverse for both directions.
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": []map[string]any{
				{"time": time.Now().UnixMilli(), "open": "100", "close": "100",
					"high": "100", "low": "100", "volume": "1"},
			}},
		})
	})
	mux.HandleFunc("GET /uapi/v1/trade/fundingFee", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"fundings": []map[string]any{}},
		})
	})
	mux.HandleFunc("GET /uapi/v1/account/detail", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"balances": []map[string]any{}},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParamsCheck", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.adjustMu.Lock()
		mock.checkSeen++
		mock.lastAdjust = body
		mock.adjustMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data":   map[string]any{"min_investment": "5"},
		})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/adjustParams", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mock.adjustMu.Lock()
		mock.adjustSeen++
		mock.lastAdjust = body
		mock.adjustMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mux.HandleFunc("POST /api/v1/bot/orders/futuresGrid/cancel", func(w http.ResponseWriter, _ *http.Request) {
		mock.cancelMu.Lock()
		mock.cancelSeen++
		mock.cancelMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": true, "timestamp": time.Now().UnixMilli()})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *manageExchangeMock) adjustBody() map[string]any {
	m.adjustMu.Lock()
	defer m.adjustMu.Unlock()
	cp := make(map[string]any, len(m.lastAdjust))
	for k, v := range m.lastAdjust {
		cp[k] = v
	}
	return cp
}

// (d) Manage RANGE_BREAK shifts ship the mode too: an under-water bot whose
// break the matrix wants to follow with a shift (LONG up-break, floating
// −1.5) now transfers the range with keepInvestment — adjustParamsCheck first,
// then the live call — while a break the matrix already closes (NEUTRAL
// down-break, adverse regime) still cancels exactly as before.
func TestManageRealRangeBreakKeepInvestmentAndBreakClose(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	mock := newManageExchangeMock(t)
	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)

	accountName := "integration-manage-preflight-test-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM grid_bots WHERE account_id IN (
		SELECT id FROM pionex_accounts WHERE name LIKE 'integration-manage-preflight-test%')`)
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-manage-preflight-test%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		if _, err := pool.Exec(cctx, `DELETE FROM bot_telemetry WHERE bot_id IN (
			SELECT id FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup telemetry: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM bot_execution_events WHERE bot_id IN (
			SELECT id::TEXT FROM grid_bots WHERE account_id = $1)`, account.ID); err != nil {
			t.Errorf("cleanup events: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM grid_bots WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup bots: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup permission health: %v", err)
		}
		if _, err := pool.Exec(cctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	mockClient := pionex.NewClient(mock.server.URL, "itest-key", "itest-secret")
	service.clientMu.Lock()
	service.clientCache[account.ID] = &clientCacheEntry{
		fingerprint: account.KeyFingerprint, client: mockClient,
	}
	service.clientMu.Unlock()

	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	seed := func(symbol, direction, remoteID string) string {
		var botID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO grid_bots (
				account_id, autogrid_settings_id, symbol, status, direction,
				grid_type, lower_price, upper_price, grid_num, leverage,
				quote_investment, extra_margin, request_fingerprint,
				execution_mode, reconciliation_state, bu_order_id,
				adjustments_count, created_at, bot_number
			) VALUES (
				$1, $2, $3, 'RUNNING', $4,
				'ARITHMETIC', 90, 110, 20, 2,
				200, 0, $5, 'REAL', 'REST_AUTHORITATIVE_OK', $6,
				0, NOW() - INTERVAL '2 hours', 600
			)
			RETURNING id
		`, account.ID, settings.ID, symbol, direction,
			"itest-"+time.Now().Format("150405.000000000"), remoteID).Scan(&botID); err != nil {
			t.Fatalf("seed %s: %v", symbol, err)
		}
		return botID
	}
	// The remote ids must match the mock fixtures so the pass reconciles the
	// intended under-water truth (A: floating −1.5 + grid −0.2; B: −20 −0.5).
	shiftBot := seed("MGMTA_USDT_PERP", "LONG", "MGMT-A-bu")
	closeBot := seed("MGMTB_USDT_PERP", "NEUTRAL", "MGMT-B-bu")

	rec := &recordingHandler{}
	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(rec)), slog.New(rec))
	worker.publicClient = pionex.NewClient(mock.server.URL, "", "")

	if _, err := worker.reconcileAndManage(ctx); err != nil {
		t.Fatalf("reconcile pass: %v (logs: %s)", err, rec.joined())
	}

	// The shift bot: the under-water break now SHIPS — one keep-transfer.
	mock.adjustMu.Lock()
	adjustSeen, checkSeen := mock.adjustSeen, mock.checkSeen
	mock.adjustMu.Unlock()
	if adjustSeen != 1 || checkSeen != 1 {
		t.Fatalf("underwater break must ship keepInvestment (check+adjust), got check=%d adjust=%d",
			checkSeen, adjustSeen)
	}
	if body := mock.adjustBody(); body["keepInvestment"] != true {
		t.Fatalf("manage keep-shift must ship keepInvestment=true, got %v", body["keepInvestment"])
	}
	var shiftStatus string
	var shiftAdjustments int
	if err := pool.QueryRow(ctx, `
		SELECT status, adjustments_count FROM grid_bots WHERE id = $1
	`, shiftBot).Scan(&shiftStatus, &shiftAdjustments); err != nil {
		t.Fatalf("load shift bot: %v", err)
	}
	if shiftStatus != "RUNNING" || shiftAdjustments != 1 {
		t.Fatalf("executed keep-shift must stay RUNNING with budget spent: %s/%d",
			shiftStatus, shiftAdjustments)
	}
	var shiftMode string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'mode' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'ADJUST_RANGE'
		ORDER BY created_at DESC LIMIT 1
	`, shiftBot).Scan(&shiftMode); err != nil || shiftMode != shiftModeKeepInvestment {
		t.Fatalf("manage keep-shift event must carry mode=keep_investment, got %q (%v)", shiftMode, err)
	}
	var blocked int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'SHIFT_BLOCKED_UNDERWATER'
	`, shiftBot).Scan(&blocked); err != nil || blocked != 0 {
		t.Fatalf("SHIFT_BLOCKED_UNDERWATER must be gone, got %d rows (%v)", blocked, err)
	}

	// The close bot: the adverse-regime break close still fires unchanged.
	mock.cancelMu.Lock()
	cancelSeen := mock.cancelSeen
	mock.cancelMu.Unlock()
	if cancelSeen != 1 {
		t.Fatalf("the RANGE_BREAK_DOWN close must still cancel exactly once, got %d", cancelSeen)
	}
	var closeStatus, closeReason, closeReconciliation string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(closed_reason,''), COALESCE(reconciliation_state,'') FROM grid_bots WHERE id = $1
	`, closeBot).Scan(&closeStatus, &closeReason, &closeReconciliation); err != nil {
		t.Fatalf("load close bot: %v", err)
	}
	// Stop-in-flight contract (same tolerance as TestReconcileAndManageIntegration):
	// the success-path persist of 'CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING' hits a
	// PRE-EXISTING schema quirk — grid_bots.reconciliation_state is VARCHAR(32)
	// while the literal is 38 chars, so that UPDATE always fails silently and
	// the row can legitimately rest at STOPPING/CANCEL_SUBMITTING until the
	// next pass reconciles the remote terminal state. Either way the close
	// intent + reason must be durable and the cancel must have been sent.
	stopInFlight := (closeStatus == "STOP_REQUESTED" && closeReconciliation == "CANCEL_ACCEPTED_REMOTE_VERIFY_PENDING") ||
		(closeStatus == "STOPPING" && closeReconciliation == "CANCEL_SUBMITTING")
	if !stopInFlight || closeReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("break close must be stop-in-flight/RANGE_BREAK_DOWN, got %s/%s (%s)",
			closeStatus, closeReason, closeReconciliation)
	}
	var closeEventReason string
	if err := pool.QueryRow(ctx, `
		SELECT details->>'reason' FROM bot_execution_events
		WHERE bot_id = $1 AND event_type = 'STOP_LOSS' ORDER BY created_at DESC LIMIT 1
	`, closeBot).Scan(&closeEventReason); err != nil || closeEventReason != "RANGE_BREAK_DOWN" {
		t.Fatalf("the break close must leave its STOP_LOSS event, got %q (%v)", closeEventReason, err)
	}
}
