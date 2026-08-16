package pionex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCreateFuturesGridBotOfficialContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bot/orders/futuresGrid/create" {
			t.Errorf("Expected path /api/v1/bot/orders/futuresGrid/create, got %s", r.URL.Path)
		}

		var req NativeFuturesGridCreateParams
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Failed to decode request body: %v", err)
		}

		if req.Base != "BTC.PERP" && req.Base != "BTC" || req.Quote != "USDT" {
			t.Errorf("Expected base BTC.PERP/BTC quote USDT, got %s %s", req.Base, req.Quote)
		}

		if req.BUOrderData.Row != 10 {
			t.Errorf("Expected row 10, got %d", req.BUOrderData.Row)
		}
		if req.BUOrderData.GridType != "arithmetic" || req.BUOrderData.Trend != "long" {
			t.Errorf("Expected official lowercase grid_type/trend, got %s/%s", req.BUOrderData.GridType, req.BUOrderData.Trend)
		}

		resp := APIEnvelope[NativeFuturesGridCreateResult]{
			Result:    true,
			Code:      "200",
			Message:   "Success",
			Timestamp: 1620000000000,
			Data: NativeFuturesGridCreateResult{
				BUOrderID: "GRID_987654321",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")

	params := NativeFuturesGridCreateParams{
		Base:  "BTC",
		Quote: "USDT",
		BUOrderData: BUOrderData{
			GridType:        "arithmetic",
			Trend:           "long",
			Bottom:          decimal.NewFromInt(50000),
			Top:             decimal.NewFromInt(60000),
			Row:             10,
			Leverage:        5,
			QuoteInvestment: decimal.NewFromInt(100),
			ExtraMargin:     decimal.Zero,
		},
	}

	result, err := client.CreateFuturesGridBot(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateFuturesGridBot failed: %v", err)
	}

	if result.BUOrderID != "GRID_987654321" {
		t.Errorf("Expected BUOrderID GRID_987654321, got %s", result.BUOrderID)
	}
}
