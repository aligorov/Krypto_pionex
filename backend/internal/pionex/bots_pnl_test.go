package pionex

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func mustDecimal(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

func decodeOrderData(t *testing.T, payload string) *BUOrderDataResponse {
	t.Helper()
	var order FuturesGridOrder
	if err := json.Unmarshal([]byte(`{"buOrderId":"T","buOrderData":`+payload+`}`), &order); err != nil {
		t.Fatalf("decode futures grid order: %v", err)
	}
	return &order.BUOrderData
}

// TestGridProfitAccessor pins the v2.0.74 realized-PnL source: profitReduce
// is the documented carrier of the accumulated grid profit, profitWithdrawn
// (0 while a grid compounds) must never shadow it, and the legacy gridProfit
// field survives as a fallback.
func TestGridProfitAccessor(t *testing.T) {
	running := decodeOrderData(t, `{
		"status": "running",
		"position": "0.5", "positionOpenPrice": "100",
		"profitReduce": "0.087", "profitWithdrawn": "0", "fundingFeePayment": "-0.02"
	}`)
	if got := running.GridProfit(); !got.Equal(mustDecimal(t, "0.087")) {
		t.Fatalf("GridProfit = %s, want 0.087 (profitReduce)", got)
	}
	if got := running.FundingFeePayment(); !got.Equal(mustDecimal(t, "-0.02")) {
		t.Fatalf("FundingFeePayment = %s, want -0.02", got)
	}
	if !running.FundingFeePaymentReported() {
		t.Fatal("fundingFeePayment present in payload must be reported")
	}

	legacy := decodeOrderData(t, `{"status": "running", "gridProfit": "1.25"}`)
	if got := legacy.GridProfit(); !got.Equal(mustDecimal(t, "1.25")) {
		t.Fatalf("GridProfit legacy fallback = %s, want 1.25", got)
	}

	withdrawnOnly := decodeOrderData(t, `{"status": "running", "profitWithdrawn": "0.9"}`)
	if got := withdrawnOnly.GridProfit(); !got.IsZero() {
		t.Fatalf("GridProfit must stay 0 when only profitWithdrawn is set, got %s", got)
	}

	absentFunding := decodeOrderData(t, `{"status": "running", "profitReduce": "1"}`)
	if absentFunding.FundingFeePaymentReported() {
		t.Fatal("absent fundingFeePayment must not be reported")
	}
	if !absentFunding.FundingFeePayment().IsZero() {
		t.Fatal("absent fundingFeePayment must decode as zero")
	}
	nullFunding := decodeOrderData(t, `{"status": "running", "fundingFeePayment": null}`)
	if nullFunding.FundingFeePaymentReported() {
		t.Fatal("null fundingFeePayment must not be reported")
	}
}

// TestFinalProfitAccessor pins the closed-grid chain: the exchange's settled
// profitExited wins; without it the grid profit plus per-bot funding is the
// figure; older exchange variants fall back to profitWithdrawn and the
// legacy totalProfit chain in that order.
func TestFinalProfitAccessor(t *testing.T) {
	settled := decodeOrderData(t, `{
		"status": "canceled", "reasonBy": "user_cancel",
		"profitExited": "0.42", "profitReduce": "0.10", "fundingFeePayment": "-0.01", "profitWithdrawn": "0"
	}`)
	if got := settled.FinalProfit(); !got.Equal(mustDecimal(t, "0.42")) {
		t.Fatalf("FinalProfit = %s, want 0.42 (profitExited wins)", got)
	}

	noExited := decodeOrderData(t, `{
		"status": "canceled", "profitReduce": "0.10", "fundingFeePayment": "-0.01"
	}`)
	if got := noExited.FinalProfit(); !got.Equal(mustDecimal(t, "0.09")) {
		t.Fatalf("FinalProfit = %s, want 0.09 (grid + funding)", got)
	}

	withdrawn := decodeOrderData(t, `{"status": "canceled", "profitWithdrawn": "1.5"}`)
	if got := withdrawn.FinalProfit(); !got.Equal(mustDecimal(t, "1.5")) {
		t.Fatalf("FinalProfit = %s, want 1.5 (profitWithdrawn fallback)", got)
	}

	legacyProfit := decodeOrderData(t, `{"status": "canceled", "profit": "2.5"}`)
	if got := legacyProfit.FinalProfit(); !got.Equal(mustDecimal(t, "2.5")) {
		t.Fatalf("FinalProfit = %s, want 2.5 (legacy profit chain)", got)
	}

	empty := decodeOrderData(t, `{"status": "canceled"}`)
	if got := empty.FinalProfit(); !got.IsZero() {
		t.Fatalf("FinalProfit = %s, want 0 for a bare record", got)
	}
}

// TestBotOrderFuturesGridData proves the finished-bot list entries decode
// into the same typed payload as the order-detail endpoint, so final PnL is
// recoverable when the detail endpoint refuses a finished grid.
func TestBotOrderFuturesGridData(t *testing.T) {
	raw := `{
		"buOrderId": "FIN-1", "buOrderType": "futures_grid", "status": "finished",
		"base": "WDCX", "quote": "USDT",
		"buOrderData": {"status": "canceled", "profitExited": "0.31", "profitReduce": "0.05"}
	}`
	var order BotOrder
	if err := json.Unmarshal([]byte(raw), &order); err != nil {
		t.Fatalf("decode bot order: %v", err)
	}
	data, err := order.FuturesGridData()
	if err != nil {
		t.Fatalf("FuturesGridData: %v", err)
	}
	if got := data.FinalProfit(); !got.Equal(mustDecimal(t, "0.31")) {
		t.Fatalf("FinalProfit from list record = %s, want 0.31", got)
	}

	var bare BotOrder
	if err := json.Unmarshal([]byte(`{"buOrderId": "X", "status": "finished"}`), &bare); err != nil {
		t.Fatalf("decode bare bot order: %v", err)
	}
	if _, err := bare.FuturesGridData(); err == nil {
		t.Fatal("record without buOrderData must fail decoding, not fabricate profits")
	}
}
