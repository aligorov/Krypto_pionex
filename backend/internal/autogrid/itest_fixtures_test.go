package autogrid

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// itestKlinesHandler serves GET /api/v1/market/klines for the deploy-path
// fixtures.
//
// Since v2.0.193 (audit F15) volExpansionBlocked fails CLOSED when the 15M feed
// carries fewer than volExpansionBaseline+1 candles, so the old "60 candles for
// every interval" fixture reads as a dead feed and blocks the entry with
// «расширение волатильности (RV/базлайн -1.00 ≥ 1.5)». The 15M branch answers
// the requested limit (at least the baseline window) and the other intervals
// keep the 60-candle hourly shape the sizing fixtures were written against.
//
// The series alternates 99.5/100.5 around 100: ATR stays positive for the
// fresh-price and turn gates while trailing and baseline realized vol are the
// same number, so the expansion ratio is ≈1 and the gate passes on the merits
// instead of on missing data.
func itestKlinesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		count, stepMS := 60, int64(3_600_000)
		if strings.EqualFold(r.URL.Query().Get("interval"), "15M") {
			count, stepMS = volExpansionBaseline+8, 900_000
		}
		if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && limit > count {
			count = limit
		}
		now := time.Now().UnixMilli()
		klines := make([]map[string]any, 0, count)
		for i := 0; i < count; i++ {
			close := "100.5"
			if i%2 == 0 {
				close = "99.5"
			}
			klines = append(klines, map[string]any{
				"time": now - int64(count-i)*stepMS,
				"open": "100", "close": close, "high": "101", "low": "99", "volume": "10",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": now,
			"data": map[string]any{"klines": klines},
		})
	}
}

// itestLiquidationEstimates are the checkParams liquidation figures the
// v2.0.168 fail-closed guard requires: a deploy with no estimates at all is
// refused as unknown risk. For the fixtures' 2x grids around 100 the walls sit
// ~50% away, i.e. far beyond the grid bound, so liquidationGuardReason passes
// on the distance check rather than standing down on implausible values.
func itestLiquidationEstimates() map[string]any {
	return map[string]any{
		"estimate_liquidation_price_up":   "150",
		"estimate_liquidation_price_down": "51",
	}
}

// itestPublicMux wraps a fixture's catch-all ticker handler into a mux that
// also answers GET /api/v1/market/klines. The deploy-path stubs used to reply
// with the ticker payload on every path, so the klines call decoded zero
// candles and the RV expansion gate — fail-closed since v2.0.193 — refused the
// entry with «RV/базлайн -1.00».
func itestPublicMux(tickers http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/klines", itestKlinesHandler())
	mux.HandleFunc("/", tickers)
	return mux
}

// seedHealthyLiquidationFeed marks the configured liquidation source connected
// with a fresh heartbeat and restores the row on cleanup. Since v2.0.119 the
// cascade gate fails CLOSED on a silent source, so a deploy-path fixture
// without this row is refused with «источник ликвидаций нестабилен
// (тишина >15м)» before it ever reaches the gate under test.
func seedHealthyLiquidationFeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var source string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(
		    (SELECT NULLIF(value#>>'{}', '') FROM app_config WHERE key = 'liquidation_source'),
		    'bybit')
	`).Scan(&source); err != nil {
		t.Fatalf("resolve liquidation source: %v", err)
	}
	var hadRow bool
	var wasConnected bool
	var wasLast *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT connected, last_message_at FROM liquidation_feed_health WHERE source = $1
	`, source).Scan(&wasConnected, &wasLast); err == nil {
		hadRow = true
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO liquidation_feed_health (source, connected, last_message_at)
		VALUES ($1, true, NOW())
		ON CONFLICT (source) DO UPDATE SET connected = true, last_message_at = NOW()
	`, source); err != nil {
		t.Fatalf("seed liquidation feed health: %v", err)
	}
	t.Cleanup(func() {
		if !hadRow {
			_, _ = pool.Exec(ctx, `DELETE FROM liquidation_feed_health WHERE source = $1`, source)
			return
		}
		_, _ = pool.Exec(ctx, `
			UPDATE liquidation_feed_health SET connected = $2, last_message_at = $3 WHERE source = $1
		`, source, wasConnected, wasLast)
	})
}
