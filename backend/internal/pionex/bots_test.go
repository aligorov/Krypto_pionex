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
		// v2.0.168: the REQUEST BODY is part of the contract — checkParams
		// expects snake_case inside buOrderData (unlike create's camelCase).
		// The pre-168 client marshaled the create-shaped struct, the exchange
		// ignored the unrecognized camelCase investment/leverage and the
		// estimates came back capital-less (empty).
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode checkParams request body: %v", err)
		}
		order, ok := body["buOrderData"].(map[string]any)
		if !ok {
			t.Fatalf("request body missing buOrderData object: %+v", body)
		}
		for _, snake := range []string{"top", "bottom", "row", "grid_type", "trend", "leverage", "quote_investment"} {
			if _, present := order[snake]; !present {
				t.Errorf("checkParams request must carry snake_case %q, body: %+v", snake, order)
			}
		}
		for _, camel := range []string{"quoteInvestment", "extraMargin", "gridType"} {
			if _, present := order[camel]; present {
				t.Errorf("checkParams request must NOT carry create-side camelCase %q, body: %+v", camel, order)
			}
		}
		if inv, _ := order["quote_investment"].(string); inv != "100" {
			t.Errorf("quote_investment must serialize as string \"100\", got %#v", order["quote_investment"])
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

func TestUpdateFuturesGridTriggerProfitLossContract(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/futuresGrid/updateTriggerProfitLoss" {
			t.Errorf("expected updateTriggerProfitLoss path, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIEnvelope[FuturesGridTriggerProfitLossResponse]{
			Result: true,
			Code:   "200",
			Data: FuturesGridTriggerProfitLossResponse{
				BUOrderID: "GRID_99",
				Result:    0,
				List: []FuturesGridTriggerProfitLossItem{
					{Type: "stop_loss", StopType: "price", Value: "135.50"},
					{Type: "stop_profit", StopType: "price", Value: "155.00"},
				},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	delay := int64(15)
	highPrice := "170.00"
	resp, err := client.UpdateFuturesGridTriggerProfitLoss(context.Background(), FuturesGridUpdateTriggerProfitLossRequest{
		BUOrderID: "GRID_99",
		List: []FuturesGridTriggerProfitLossItem{
			{
				Type:              "stop_loss",
				StopType:          "price",
				Value:             "135.50",
				StopDelay:         &delay,
				LossStopSellModel: "TO_USDT",
				StopHighPrice:     &highPrice,
			},
			{
				Type:                "stop_profit",
				StopType:            "price",
				Value:               "155.00",
				StopDelay:           &delay,
				ProfitStopSellModel: "TO_USDT",
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateFuturesGridTriggerProfitLoss failed: %v", err)
	}
	if resp.BUOrderID != "GRID_99" || len(resp.List) != 2 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Verify snake_case serialization per official Pionex API docs
	if receivedBody["bu_order_id"] != "GRID_99" {
		t.Fatalf("expected bu_order_id snake_case, got %v", receivedBody)
	}
	items, ok := receivedBody["list"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected list of 2 items, got %v", receivedBody["list"])
	}
	item0 := items[0].(map[string]any)
	if item0["type"] != "stop_loss" || item0["stop_type"] != "price" || item0["value"] != "135.50" {
		t.Fatalf("unexpected item0 serialization: %+v", item0)
	}
	if item0["stop_high_price"] != "170.00" || item0["loss_stop_sell_model"] != "TO_USDT" {
		t.Fatalf("unexpected item0 extras: %+v", item0)
	}
}

