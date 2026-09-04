package pionex

import (
	"context"
	"encoding/json"
	"fmt"
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

// accountDetailServer serves GET /uapi/v1/account/detail with a canned data
// payload (the envelope is added by the helper).
func accountDetailServer(t *testing.T, data string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/uapi/v1/account/detail" {
			t.Errorf("expected signed GET /uapi/v1/account/detail, got %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("timestamp") == "" {
			t.Error("signed request must carry the timestamp query parameter")
		}
		if r.Header.Get("PIONEX-KEY") == "" || r.Header.Get("PIONEX-SIGNATURE") == "" {
			t.Error("signed request must carry PIONEX-KEY and PIONEX-SIGNATURE headers")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"result":true,"timestamp":1620000000000,"data":%s}`, data)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestGetFuturesAccountDetailOfficialContract pins the documented shape:
// envelope.result + data.balances[] with camelCase string fields
// (coin/assets/free/frozen/transferable/available/unrealizedPnL/
// totalInitialMargin/debts) — the exact AccountDetailBalance schema of the
// official OpenAPI. A regression here is what left the prod equity ledger
// at zero rows.
func TestGetFuturesAccountDetailOfficialContract(t *testing.T) {
	server := accountDetailServer(t, `{"balances":[
		{"coin":"USDT","assets":"500.00000000","free":"480.5","frozen":"19.5",
		 "transferable":"480.5","available":"480.5","unrealizedPnL":"-2.6",
		 "totalInitialMargin":"19.5","debts":"0.00000000"}]}`)
	client := NewClient(server.URL, "testKey", "testSecret")

	balances, err := client.GetFuturesAccountDetail(context.Background())
	if err != nil {
		t.Fatalf("GetFuturesAccountDetail failed: %v", err)
	}
	if len(balances) != 1 || balances[0].Coin != "USDT" {
		t.Fatalf("expected one USDT row, got %+v", balances)
	}
	row := balances[0]
	if !row.Assets.Equal(decimal.NewFromInt(500)) ||
		!row.UnrealizedPnL.Equal(decimal.NewFromFloat(-2.6)) ||
		!row.Available.Equal(decimal.NewFromFloat(480.5)) ||
		!row.Debts.IsZero() {
		t.Fatalf("official-contract decode wrong: %+v", row)
	}
}

// TestGetFuturesAccountDetailLiveShapeTolerance covers the deviations the
// live API has already shown on other endpoints (v2.0.74: unquoted numbers
// where the spec promises strings; empty strings for absent values) plus
// the double data-envelope: none of them may zero out the USDT row or fail
// the call — a silent empty decode is an unwritten equity snapshot.
func TestGetFuturesAccountDetailLiveShapeTolerance(t *testing.T) {
	t.Run("numbers instead of strings", func(t *testing.T) {
		server := accountDetailServer(t, `{"balances":[
			{"coin":"USDT","assets":500.5,"free":480,"frozen":20.5,
			 "transferable":480,"available":480,"unrealizedPnL":-2.6,
			 "totalInitialMargin":20.5,"debts":0}]}`)
		client := NewClient(server.URL, "k", "s")
		balances, err := client.GetFuturesAccountDetail(context.Background())
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if len(balances) != 1 || !balances[0].Assets.Equal(decimal.NewFromFloat(500.5)) ||
			!balances[0].UnrealizedPnL.Equal(decimal.NewFromFloat(-2.6)) {
			t.Fatalf("numeric-fields decode wrong: %+v", balances)
		}
	})
	t.Run("empty string and null fields stay zero", func(t *testing.T) {
		server := accountDetailServer(t, `{"balances":[
			{"coin":"USDT","assets":"500","free":"","frozen":null,
			 "transferable":"","available":"500","unrealizedPnL":null,
			 "totalInitialMargin":"","debts":""}]}`)
		client := NewClient(server.URL, "k", "s")
		balances, err := client.GetFuturesAccountDetail(context.Background())
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if len(balances) != 1 || !balances[0].Assets.Equal(decimal.NewFromInt(500)) ||
			!balances[0].Free.IsZero() || !balances[0].UnrealizedPnL.IsZero() ||
			!balances[0].Debts.IsZero() {
			t.Fatalf("null/empty tolerance decode wrong: %+v", balances)
		}
	})
	t.Run("double data envelope recovers", func(t *testing.T) {
		server := accountDetailServer(t,
			`{"data":{"balances":[{"coin":"USDT","assets":"500","unrealizedPnL":"-1.1"}]}}`)
		client := NewClient(server.URL, "k", "s")
		balances, err := client.GetFuturesAccountDetail(context.Background())
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if len(balances) != 1 || !balances[0].Assets.Equal(decimal.NewFromInt(500)) {
			t.Fatalf("double-envelope probe failed: %+v", balances)
		}
	})
	t.Run("empty balances stays empty", func(t *testing.T) {
		server := accountDetailServer(t, `{"balances":[]}`)
		client := NewClient(server.URL, "k", "s")
		balances, err := client.GetFuturesAccountDetail(context.Background())
		if err != nil {
			t.Fatalf("decode failed: %v", err)
		}
		if len(balances) != 0 {
			t.Fatalf("empty balances must stay empty, got %+v", balances)
		}
	})
}
