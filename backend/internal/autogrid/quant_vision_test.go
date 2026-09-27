package autogrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.119: the shield's side comes from the SIGNED POSITION (decimal), not
// bot.direction. Helper keeps the call sites in the tests short.
func shieldCall(worker *Worker, signedPos float64, price float64, triggeredAt *string, extreme *decimal.Decimal, lastClearedAt *string) (bool, *string, *decimal.Decimal, string) {
	return worker.evaluateWickShield(
		context.Background(), "SOL_USDT_PERP",
		decimal.NewFromFloat(signedPos), decimal.NewFromFloat(price),
		triggeredAt, extreme, lastClearedAt, 90,
	)
}

func TestEvaluateWickShield_AlreadyArmed_Holding(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	// LONG inventory (+10), price 50.5 above the wick low 50.0, 30s < 90s grace
	shouldHold, newTrig, newExt, reason := shieldCall(worker, 10, 50.5, &triggeredAt, &extreme, nil)

	if !shouldHold {
		t.Fatalf("expected shouldHold=true, got false, reason=%s", reason)
	}
	if newTrig == nil || *newTrig != triggeredAt {
		t.Fatalf("expected triggeredAt preserved, got %v", newTrig)
	}
	if newExt == nil || !newExt.Equal(extreme) {
		t.Fatalf("expected extreme preserved, got %v", newExt)
	}
}

func TestEvaluateWickShield_AlreadyArmed_Breached(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	// LONG inventory: price 49.5 punched below the wick low -> breach
	shouldHold, _, _, reason := shieldCall(worker, 10, 49.5, &triggeredAt, &extreme, nil)
	if shouldHold {
		t.Fatalf("expected shouldHold=false on breach, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

func TestEvaluateWickShield_AlreadyArmed_Expired(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-95 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(50.0)

	shouldHold, _, _, reason := shieldCall(worker, 10, 50.5, &triggeredAt, &extreme, nil)
	if shouldHold {
		t.Fatalf("expected shouldHold=false on expired grace, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

// TestEvaluateWickShield_ShortByPosition_Breached pins the NEAR #1401 fix at
// the evaluator level: a NEUTRAL-labeled bot holding a SHORT inventory must
// breach when price rises above the wick HIGH. Under v2.0.114-118 the shield
// keyed off direction="NEUTRAL" and read the LOWER wick — the exact inversion
// that deferred NEAR's stop twice while the price ran up.
func TestEvaluateWickShield_ShortByPosition_Breached(t *testing.T) {
	worker := &Worker{}
	now := time.Now().UTC()
	triggeredAt := now.Add(-30 * time.Second).Format(time.RFC3339)
	extreme := decimal.NewFromFloat(100.0)

	// SHORT inventory (−64, NEAR's actual bag), price 101 > wick high 100
	shouldHold, _, _, reason := shieldCall(worker, -64, 101.0, &triggeredAt, &extreme, nil)
	if shouldHold {
		t.Fatalf("expected shouldHold=false on short-inventory breach, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

// TestEvaluateWickShield_FlatPositionNoShield: zero inventory means the close
// carries no directional risk — nothing for a wick to damage, no grace.
func TestEvaluateWickShield_FlatPositionNoShield(t *testing.T) {
	worker := &Worker{}
	shouldHold, _, _, reason := shieldCall(worker, 0, 50.5, nil, nil, nil)
	if shouldHold {
		t.Fatalf("expected shouldHold=false for flat position, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

// TestEvaluateWickShield_ReArmCooldown: a shield cleared two minutes ago may
// not re-arm — the NEAR double-deferral (cleared 05:47:24 "recovered",
// re-armed 05:49:47) class.
func TestEvaluateWickShield_ReArmCooldown(t *testing.T) {
	worker := &Worker{}
	clearedAt := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	shouldHold, _, _, reason := shieldCall(worker, 10, 50.5, nil, nil, &clearedAt)
	if shouldHold {
		t.Fatalf("expected shouldHold=false inside re-arm cooldown, got true")
	}
	if reason == "" {
		t.Fatalf("expected non-empty reason")
	}
}

// wickKlinesServer serves a 3-candle 5M feed: the LIVE bar (len-1) carries a
// bullish lower wick, the CLOSED bar (len-2) carries a bearish UPPER wick —
// opposite wick shapes, so which bar the shield reads is fully observable.
func wickKlinesServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/klines", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"klines": []map[string]any{
				{"time": time.Now().UnixMilli() - 300000, "open": "100", "close": "100.2",
					"high": "100.3", "low": "99.2", "volume": "1"}, // older bar, unused
				// CLOSED bar (len-2): upper wick dominates — open 100, high
				// 103, close 100.2 → rejection from the top.
				{"time": time.Now().UnixMilli() - 150000, "open": "100", "close": "100.2",
					"high": "103", "low": "99.95", "volume": "1"},
				// LIVE bar (len-1): lower wick dominates — must NOT arm anything.
				{"time": time.Now().UnixMilli(), "open": "100.2", "close": "100.4",
					"high": "100.5", "low": "97.5", "volume": "1"},
			}},
		})
	})
	return httptest.NewServer(mux)
}

// TestEvaluateWickShield_ArmsOnClosedCandleOnly_UpperWickForShort pins the
// full NEAR #1401 fix at arming time: a SHORT inventory (even on a bot whose
// direction label is NEUTRAL) arms only on the CLOSED bar's UPPER wick. The
// live bar's fat lower wick — the exact pattern that fooled v2.0.114-118 —
// must contribute nothing.
func TestEvaluateWickShield_ArmsOnClosedCandleOnly_UpperWickForShort(t *testing.T) {
	server := wickKlinesServer(t)
	worker := &Worker{publicClient: pionex.NewClient(server.URL, "", "")}
	t.Cleanup(server.Close)

	shouldHold, newTrig, newExtreme, reason := shieldCall(worker, -64, 100.4, nil, nil, nil)
	if !shouldHold {
		t.Fatalf("expected short inventory to arm on the closed upper wick, got hold=false reason=%s", reason)
	}
	if newTrig == nil || newExtreme == nil {
		t.Fatalf("expected armed timestamps, got nil")
	}
	// The extreme must be the CLOSED bar's HIGH (103), not the live bar's low.
	if !newExtreme.Equal(decimal.NewFromInt(103)) {
		t.Fatalf("expected extreme = closed-bar high 103, got %s", newExtreme.String())
	}
}

// TestEvaluateWickShield_LiveBarWickAloneDoesNotArmForShort: same feed, LONG
// inventory. The closed bar has no meaningful lower wick (99.95 vs range
// 100-103) and the live bar's lower wick must be ignored → no arming.
func TestEvaluateWickShield_LiveBarWickAloneDoesNotArmForShort(t *testing.T) {
	server := wickKlinesServer(t)
	worker := &Worker{publicClient: pionex.NewClient(server.URL, "", "")}
	t.Cleanup(server.Close)

	shouldHold, _, _, _ := shieldCall(worker, 50, 100.4, nil, nil, nil)
	if shouldHold {
		t.Fatalf("expected no arming for LONG inventory (no closed-bar lower wick; live bar must be ignored)")
	}
}

// TestCheckOrderBookCushion_FailClosedAndSpread covers the v2.0.119 gate
// changes: a REAL deploy (failClosed=true) is rejected when the depth fetch
// fails or the client is missing, and a book whose best bid/ask gap exceeds
// the limit is rejected regardless of cushion depth.
func TestCheckOrderBookCushion_FailClosedAndSpread(t *testing.T) {
	// No client: fail-closed rejects, fail-open passes.
	worker := &Worker{}
	if ok, _, reason := worker.checkOrderBookCushion(context.Background(), "X_USDT_PERP", decimal.NewFromInt(100), 200, 50, true, decimal.Zero); ok {
		t.Fatalf("fail-closed must reject when the public client is missing")
	} else if reason == "" {
		t.Fatalf("expected a reason on fail-closed rejection")
	}
	if ok, _, _ := worker.checkOrderBookCushion(context.Background(), "X_USDT_PERP", decimal.NewFromInt(100), 200, 50, false, decimal.Zero); !ok {
		t.Fatalf("fail-open (paper) must pass when the public client is missing")
	}

	// Depth server with a 0.5% spread and deep both sides.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/market/depth", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true,
			"data": map[string]any{
				"bids": [][2]string{{"99.75", "1000"}},
				"asks": [][2]string{{"100.25", "1000"}},
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	workerWithBook := &Worker{publicClient: pionex.NewClient(server.URL, "", "")}

	// Spread 0.5% > 0.20% limit (stored as a fraction) → reject with the spread reason.
	if ok, _, reason := workerWithBook.checkOrderBookCushion(context.Background(), "X_USDT_PERP", decimal.NewFromInt(100), 200, 50, true, decimal.RequireFromString("0.0020")); ok {
		t.Fatalf("wide spread must reject the candidate")
	} else if !strings.Contains(reason, "спред") {
		t.Fatalf("expected spread rejection reason, got %q", reason)
	}

	// Same book, limit 1% → spread passes; depth: ~$99.8k bid side vs
	// 200 notional ≈ 499x ≥ 50 → accept.
	if ok, _, reason := workerWithBook.checkOrderBookCushion(context.Background(), "X_USDT_PERP", decimal.NewFromInt(100), 200, 50, true, decimal.RequireFromString("0.01")); !ok {
		t.Fatalf("deep two-sided book within spread limit must pass, got %q", reason)
	}
}
