package autogrid

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// sequenceTickerServer serves one close price per request from a fixed
// series, wrapping on the last value — the manage pass pulls exactly one
// ticker per pass, so the series IS the synthetic day the bot lives through.
func sequenceTickerServer(t *testing.T, symbol string, prices []string) *httptest.Server {
	t.Helper()
	var cursor atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		idx := int(cursor.Add(1)-1) % len(prices)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result":    true,
			"timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"tickers": []map[string]any{
					{"symbol": symbol, "close": prices[idx], "open": prices[idx],
						"high": prices[idx], "low": prices[idx], "volume": "1000"},
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// TestPaperEngineFrictionOscillatingDay is the Part-2 acceptance: a synthetic
// oscillating day through the CALIBRATED engine must earn noticeably less
// than the same walk through the old engine semantics (boundary-touch fills
// at the raw level mapping, no entry fee, settings-composite close cost).
//
// The scenario: a NEUTRAL 90–110 / 20-level grid ($100, 2x, level width $1)
// rides a tape that kisses the 95.0 boundary ±0.02% eight times and then
// makes one genuine 1% traverse. The old mapping flips the level on every
// kiss — each flip books a fictional completed pair — while the calibrated
// buffer (0.03%) books zero pairs until the real traverse.
func TestPaperEngineFrictionOscillatingDay(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	const symbol = "OSC_USDT_PERP"
	// The kisses cross 95.0 by <0.03%; the traverse ends 1% below it.
	series := []string{"95.5", "94.99", "95.2", "94.985", "95.15", "94.99", "95.1",
		"94.98", "95.25", "94.995", "94.9", "94.6", "94.9", "94.55"}
	tickers := sequenceTickerServer(t, symbol, series)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	var botID string
	// Deployed exactly like the v2.0.89 deploy path books it: the entry fee
	// on the initial inventory at the deploy price (95.5 sits 4.5 levels
	// below mid → inventory 4.5 × $10 = $45 → taker 0.05% = $0.0225).
	deployFee := paperEntryFee("NEUTRAL", decimal.NewFromInt(90), decimal.NewFromInt(110),
		20, decimal.NewFromInt(100), 2, decimal.RequireFromString("95.5"))
	if !deployFee.Equal(decimal.NewFromFloat(0.0225)) {
		t.Fatalf("expected deploy fee 0.0225 (0.0005 × 45), got %s", deployFee)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, pnl_target_usdt, max_loss_usdt,
			realized_pnl_usdt, fees_paid_usdt
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 20, 2, 100,
			95.5, 95.5, 999, -999,
			-$3::NUMERIC, $3::NUMERIC
		)
		RETURNING id
	`, settings.ID, symbol, deployFee.String()).Scan(&botID)
	if err != nil {
		t.Fatalf("insert paper bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID)
	})

	worker := NewWorker(pool, service, accountService, riskEngine,
		llm.NewService(pool, slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler))
	worker.publicClient = pionex.NewClient(tickers.URL, "test-key", "test-secret")

	for range series {
		if err := worker.managePaperBots(ctx, *settings); err != nil {
			t.Fatalf("managePaperBots pass: %v", err)
		}
	}

	var realized, feesPaid string
	var lastLevel *int
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt::TEXT, fees_paid_usdt::TEXT, last_grid_level
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&realized, &feesPaid, &lastLevel); err != nil {
		t.Fatalf("load walked bot: %v", err)
	}

	// Old-engine comparator over the SAME walk: raw level mapping (a touch is
	// a fill), no entry fee, the pre-calibration pair economics. Each kiss
	// cycle that flips the raw level booked one fictional pair.
	calibrated := decimal.RequireFromString(realized)
	oldEngine := decimal.Zero
	rawLevel := 5 // 95.5 in a 90–110/20 grid
	perLevelNotional := decimal.NewFromInt(10)
	stepPct := decimal.NewFromFloat(1.0 / 95.0)
	feePct := decimal.NewFromFloat(pionexMakerFeeBps * 2 / 10000)
	pairProfit := perLevelNotional.Mul(stepPct.Sub(feePct))
	for _, price := range series[1:] {
		p := decimal.RequireFromString(price)
		level := gridLevelForPrice(decimal.NewFromInt(90), decimal.NewFromInt(110), 20, p)
		if level != rawLevel {
			oldEngine = oldEngine.Add(pairProfit)
			rawLevel = level
		}
	}

	if !oldEngine.IsPositive() {
		t.Fatalf("scenario must book fictional pairs under the old mapping, got %s", oldEngine)
	}
	if calibrated.GreaterThanOrEqual(oldEngine) {
		t.Fatalf("calibrated engine (%s) must earn noticeably less than the old boundary-touch engine (%s) on the oscillating day",
			calibrated.StringFixed(6), oldEngine.StringFixed(6))
	}
	if lastLevel == nil || *lastLevel == 5 {
		t.Fatalf("the genuine traverse to 94.55 must still cross levels under the buffer, got level %v", lastLevel)
	}
	// The entry fee rode in realized and the fee ledger column — the
	// calibrated walk earned exactly ONE genuine pair (the 1% traverse) net
	// of its maker fees and the deploy fee, while the old mapping banked
	// every kiss: the delta IS the calibration.
	if !calibrated.LessThan(oldEngine.Sub(decimal.NewFromFloat(0.05))) {
		t.Fatalf("the friction delta must be material: calibrated %s vs old %s",
			calibrated.StringFixed(6), oldEngine.StringFixed(6))
	}
}

// TestPaperInvestInBooksEntryFee drives a PAPER invest_in through AdjustBot —
// the pour must pay the taker entry fee on the notional it adds (v2.0.89
// Part-2 item 4).
func TestPaperInvestInBooksEntryFee(t *testing.T) {
	dbURL := integrationDatabaseURL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	accountService := accounts.NewService(pool)
	riskEngine := risk.NewEngine(pool)
	service := NewService(pool, riskEngine)
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	const symbol = "POUR_USDT_PERP"
	var botID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price
		) VALUES (
			$1, $2, 'RUNNING', 'NEUTRAL', 'ARITHMETIC',
			90, 110, 20, 4, 100,
			100, 100
		)
		RETURNING id
	`, settings.ID, symbol).Scan(&botID); err != nil {
		t.Fatalf("insert paper bot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM paper_grid_bots WHERE id = $1`, botID)
	})

	if _, err := service.AdjustBot(ctx, accountService, settings.ID, botID, AdjustBotInput{
		Mode:            "invest_in",
		QuoteInvestment: decimal.NewFromInt(100),
	}); err != nil {
		t.Fatalf("paper invest_in: %v", err)
	}

	var realized, feesPaid string
	if err := pool.QueryRow(ctx, `
		SELECT realized_pnl_usdt::TEXT, fees_paid_usdt::TEXT
		FROM paper_grid_bots WHERE id = $1
	`, botID).Scan(&realized, &feesPaid); err != nil {
		t.Fatalf("load poured bot: %v", err)
	}
	// Neutral pour at mid convention: half the leveraged pour —
	// 0.0005 × (100 × 4 / 2) = $0.10.
	wantFee := decimal.NewFromFloat(0.10)
	if fees := decimal.RequireFromString(feesPaid); !fees.Equal(wantFee) {
		t.Fatalf("pour fee = 0.0005 × 200 = 0.1, got %s", fees)
	}
	if real := decimal.RequireFromString(realized); !real.Equal(wantFee.Neg()) {
		t.Fatalf("pour fee must debit realized to −0.1, got %s", real)
	}
}
