package pionex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestGetFundingFeeHistoryContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/uapi/v1/trade/fundingFee" {
			t.Errorf("expected path /uapi/v1/trade/fundingFee, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("PIONEX-KEY") != "testKey" {
			t.Errorf("expected signed private request with PIONEX-KEY header")
		}
		if r.URL.Query().Get("timestamp") == "" {
			t.Errorf("expected signed timestamp query parameter")
		}
		if got := r.URL.Query().Get("symbol"); got != "BTC_USDT_PERP" {
			t.Errorf("expected symbol BTC_USDT_PERP, got %q", got)
		}
		if got := r.URL.Query().Get("startTime"); got != "1620000000000" {
			t.Errorf("expected startTime 1620000000000, got %q", got)
		}
		if got := r.URL.Query().Get("endTime"); got != "1620086400000" {
			t.Errorf("expected endTime 1620086400000, got %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "200" {
			t.Errorf("expected limit 200, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"result": true, "code": "200", "timestamp": time.Now().UnixMilli(),
			"data": map[string]any{
				"fundings": []map[string]any{
					{
						"symbol":       "BTC_USDT_PERP",
						"isolatedMode": "CROSS",
						"fundingFee":   "-0.01234567",
						"fundingCoin":  "USDT",
						"timestamp":    1620057600000,
						"fundingRate":  "0.0001",
					},
					{
						"symbol":       "BTC_USDT_PERP",
						"isolatedMode": "CROSS",
						"fundingFee":   "0.0042",
						"fundingCoin":  "USDT",
						"timestamp":    1620028800000,
						"fundingRate":  "-0.000031",
					},
				},
			},
		})
	}))
	defer server.Close()

	client := NewClient(server.URL, "testKey", "testSecret")
	records, err := client.GetFundingFeeHistory(
		context.Background(), "BTC_USDT_PERP", 1620000000000, 1620086400000, 500)
	if err != nil {
		t.Fatalf("GetFundingFeeHistory failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	first := records[0]
	if !first.FundingFee.Equal(decimal.RequireFromString("-0.01234567")) {
		t.Fatalf("expected signed paid fee -0.01234567, got %s", first.FundingFee)
	}
	if !first.FundingRate.Equal(decimal.RequireFromString("0.0001")) {
		t.Fatalf("expected funding rate 0.0001, got %s", first.FundingRate)
	}
	if first.Symbol != "BTC_USDT_PERP" || first.IsolatedMode != "CROSS" || first.FundingCoin != "USDT" {
		t.Fatalf("unexpected record metadata: %+v", first)
	}
	wantTime := time.UnixMilli(1620057600000).UTC()
	if !first.Timestamp.Equal(wantTime) {
		t.Fatalf("expected timestamp %s, got %s", wantTime, first.Timestamp)
	}
	if !records[1].FundingFee.Equal(decimal.RequireFromString("0.0042")) {
		t.Fatalf("expected received fee 0.0042, got %s", records[1].FundingFee)
	}
}
