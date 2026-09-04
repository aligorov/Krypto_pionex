package pionex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestGetSpotGridAIStrategyContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/spotGrid/aiStrategy" {
			t.Errorf("expected path /api/v1/bot/orders/spotGrid/aiStrategy, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("base") != "BTC" || r.URL.Query().Get("quote") != "USDT" {
			t.Errorf("expected base/quote query params, got %s", r.URL.RawQuery)
		}
		if r.Header.Get("PIONEX-KEY") != "testKey" {
			t.Errorf("expected signed private request with PIONEX-KEY header")
		}
		resp := APIEnvelope[SpotGridAIStrategy]{
			Result: true, Code: "200", Timestamp: 1620000000000,
			Data: SpotGridAIStrategy{
				High:        decimal.NewFromFloat(70000),
				Low:         decimal.NewFromFloat(60000),
				GridCount:   33,
				Annualized:  decimal.NewFromFloat(0.24),
				Volatility:  decimal.NewFromFloat(0.05),
				MaxDrawDown: decimal.NewFromFloat(0.08),
				StrategyID:  "spot-grid-ai-1",
				Options: []AIOption{{
					Period: 7, GridCount: 21,
					SuitabilityMin: decimal.NewFromInt(100),
					SuitabilityMax: decimal.NewFromInt(500),
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	strategy, err := client.GetSpotGridAIStrategy(context.Background(), "BTC", "USDT")
	if err != nil {
		t.Fatalf("GetSpotGridAIStrategy failed: %v", err)
	}
	if strategy.GridCount != 33 || strategy.StrategyID != "spot-grid-ai-1" {
		t.Fatalf("unexpected strategy payload: %+v", strategy)
	}
	if !strategy.High.Equal(decimal.NewFromFloat(70000)) {
		t.Fatalf("expected high 70000, got %s", strategy.High)
	}
}

func TestCheckFuturesGridParamsContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/futuresGrid/checkParams" {
			t.Errorf("expected path /api/v1/bot/orders/futuresGrid/checkParams, got %s", r.URL.Path)
		}
		resp := APIEnvelope[FuturesGridCheckParamsResult]{
			Result: true, Code: "200", Timestamp: 1620000000000,
			Data: FuturesGridCheckParamsResult{
				MinInvestment:           decimal.NewFromInt(80),
				MaxInvestment:           decimal.NewFromInt(100000),
				EstimateLiquidationDown: decimal.NewFromFloat(52000.5),
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	result, err := client.CheckFuturesGridParams(context.Background(), NativeFuturesGridCreateParams{
		Base: "BTC.PERP", Quote: "USDT",
		BUOrderData: BUOrderData{
			GridType: "arithmetic", Trend: "no_trend",
			Bottom: decimal.NewFromInt(55000), Top: decimal.NewFromInt(65000),
			Row: 20, Leverage: 3, QuoteInvestment: decimal.NewFromInt(100),
		},
	})
	if err != nil {
		t.Fatalf("CheckFuturesGridParams failed: %v", err)
	}
	if !result.MinInvestment.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("expected min investment 80, got %s", result.MinInvestment)
	}
}

func TestAdjustFuturesGridBotContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/futuresGrid/adjustParams" {
			t.Errorf("expected adjustParams path, got %s", r.URL.Path)
		}
		var req AdjustFuturesGridParams
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode adjust request: %v", err)
		}
		if req.BUOrderID != "GRID_1" || req.Type != "adjust_params" || req.Row != 20 {
			t.Fatalf("unexpected adjust payload: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIEnvelope[json.RawMessage]{Result: true, Code: "200"})
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	bottom := decimal.NewFromInt(100)
	top := decimal.NewFromInt(120)
	err := client.AdjustFuturesGridBot(context.Background(), AdjustFuturesGridParams{
		BUOrderID: "GRID_1", Type: "adjust_params",
		Bottom: &bottom, Top: &top, Row: 20,
	})
	if err != nil {
		t.Fatalf("AdjustFuturesGridBot failed: %v", err)
	}
}

// keepInvestment is a pointer on purpose: nil must OMIT the field (the
// exchange default false = the floating-PnL gate applies), true must ship.
func TestAdjustFuturesGridKeepInvestmentMarshaling(t *testing.T) {
	raw, err := json.Marshal(AdjustFuturesGridParams{BUOrderID: "G", Type: "adjust_params"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "keepInvestment") {
		t.Fatalf("nil KeepInvestment must omit the field, got %s", raw)
	}
	keep := true
	raw, err = json.Marshal(AdjustFuturesGridParams{BUOrderID: "G", Type: "adjust_params", KeepInvestment: &keep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"keepInvestment":true`) {
		t.Fatalf("true KeepInvestment must ship, got %s", raw)
	}
	falseKeep := false
	raw, err = json.Marshal(AdjustFuturesGridParams{BUOrderID: "G", Type: "adjust_params", KeepInvestment: &falseKeep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"keepInvestment":false`) {
		t.Fatalf("explicit false KeepInvestment must ship, got %s", raw)
	}
}

// The dry-run contract: same body as the live adjust, endpoint
// .../adjustParamsCheck, and a refused check (result=false) surfaces as an
// error carrying the exchange code — never a silent pass.
func TestCheckAdjustFuturesGridBotContract(t *testing.T) {
	var refused atomic.Bool
	var lastBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/futuresGrid/adjustParamsCheck" {
			t.Errorf("expected adjustParamsCheck path, got %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.Header().Set("Content-Type", "application/json")
		if refused.Load() {
			json.NewEncoder(w).Encode(map[string]any{
				"result": false, "code": "BOT_INVALID_ARGUMENT",
				"message": "PROFIT_LESS_THAN_ZERO", "timestamp": time.Now().UnixMilli(),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "code": "200", "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{"min_investment": "5", "slippage": "0.1"},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	bottom, top := decimal.NewFromInt(100), decimal.NewFromInt(120)
	keep := true
	result, err := client.CheckAdjustFuturesGridBot(context.Background(), AdjustFuturesGridParams{
		BUOrderID: "GRID_1", Type: "adjust_params",
		Bottom: &bottom, Top: &top, Row: 20, KeepInvestment: &keep,
	})
	if err != nil {
		t.Fatalf("CheckAdjustFuturesGridBot failed: %v", err)
	}
	if lastBody["keepInvestment"] != true {
		t.Fatalf("the check must validate the IDENTICAL body incl. keepInvestment, got %v", lastBody)
	}
	if !result.MinInvestment.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("check payload must decode, got %+v", result)
	}

	refused.Store(true)
	if _, err := client.CheckAdjustFuturesGridBot(context.Background(), AdjustFuturesGridParams{
		BUOrderID: "GRID_1", Type: "adjust_params",
		Bottom: &bottom, Top: &top, Row: 20, KeepInvestment: &keep,
	}); err == nil {
		t.Fatal("a refused check must surface as an error")
	} else if !strings.Contains(err.Error(), "PROFIT_LESS_THAN_ZERO") {
		t.Fatalf("the refusal must carry the exchange reason, got %v", err)
	}
}
