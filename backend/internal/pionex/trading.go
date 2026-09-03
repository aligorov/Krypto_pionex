package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

// FuturesOrderRequest represents params to place an ordinary Futures order
// via /uapi/v1/trade/order. The documented quantity field is "size" (string)
// and the documented type enum is LIMIT / MARKET_QTY / IOC / FOK / POSTONLY.
type FuturesOrderRequest struct {
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"` // BUY, SELL
	OrderType     string          `json:"type"`
	ClientOrderID string          `json:"clientOrderId"`
	Price         decimal.Decimal `json:"price,omitempty"`
	Size          decimal.Decimal `json:"size"`
}

// FuturesOrderResult represents the returned order placement response.
// orderId is documented as int64 but is decoded tolerantly because Pionex
// mixes numeric and string ids across endpoints.
type FuturesOrderResult struct {
	OrderID       string
	ClientOrderID string `json:"clientOrderId"`
}

func (r *FuturesOrderResult) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode futures order result: %w", err)
	}
	if value, ok := raw["orderId"]; ok {
		r.OrderID = fmt.Sprintf("%v", value)
	}
	if value, ok := raw["clientOrderId"]; ok {
		r.ClientOrderID = fmt.Sprintf("%v", value)
	}
	return nil
}

// FuturesPosition represents current active futures position details.
// Field aliases follow both the legacy and documented naming
// (positionSide/avgPrice/netSize/unrealizedPnL), and leverage tolerates
// string or numeric encoding.
type FuturesPosition struct {
	Symbol           string
	Side             string
	Amount           decimal.Decimal
	EntryPrice       decimal.Decimal
	MarkPrice        decimal.Decimal
	UnrealizedPNL    decimal.Decimal
	Leverage         int
	LiquidationPrice decimal.Decimal
}

func (p *FuturesPosition) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	pick := func(keys ...string) json.RawMessage {
		for _, key := range keys {
			if value, ok := raw[key]; ok && string(value) != "null" {
				return value
			}
		}
		return nil
	}
	decodeDecimal := func(target *decimal.Decimal, keys ...string) {
		if value := pick(keys...); value != nil {
			_ = json.Unmarshal(value, target)
		}
	}
	decodeString := func(target *string, keys ...string) {
		if value := pick(keys...); value != nil {
			_ = json.Unmarshal(value, target)
		}
	}
	decodeString(&p.Symbol, "symbol")
	decodeString(&p.Side, "side", "positionSide")
	decodeDecimal(&p.Amount, "amount", "netSize")
	decodeDecimal(&p.EntryPrice, "entryPrice", "avgPrice")
	decodeDecimal(&p.MarkPrice, "markPrice")
	decodeDecimal(&p.UnrealizedPNL, "unrealizedPnl", "unrealizedPnL")
	decodeDecimal(&p.LiquidationPrice, "liquidationPrice")
	if value := pick("leverage"); value != nil {
		var asInt int
		if err := json.Unmarshal(value, &asInt); err == nil {
			p.Leverage = asInt
		} else {
			var asString string
			if err := json.Unmarshal(value, &asString); err == nil {
				if parsed, err := strconv.Atoi(asString); err == nil {
					p.Leverage = parsed
				}
			}
		}
	}
	return nil
}

// PlaceFuturesOrder places an ordinary futures order using official endpoint /uapi/v1/trade/order.
func (c *Client) PlaceFuturesOrder(ctx context.Context, params FuturesOrderRequest) (*FuturesOrderResult, error) {
	bodyBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal futures order: %w", err)
	}
	var result FuturesOrderResult
	if err := c.do(ctx, http.MethodPost, "/uapi/v1/trade/order", nil, bodyBytes, true, 1, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetFuturesPositions fetches active futures positions using /uapi/v1/account/positions.
func (c *Client) GetFuturesPositions(ctx context.Context) ([]FuturesPosition, error) {
	var data struct {
		Positions []FuturesPosition `json:"positions"`
	}
	if err := c.do(ctx, http.MethodGet, "/uapi/v1/account/positions", nil, nil, true, 5, &data); err != nil {
		return nil, err
	}
	return data.Positions, nil
}

// FundingFeeRecord is one entry of the documented funding history
// (GET /uapi/v1/trade/fundingFee). FundingFee keeps the exchange's signed
// decimal verbatim: per the official docs the field carries the paid amount
// ("Total funding fee paid" in the bot contract), so positive = paid by the
// account, negative = received. FundingRate is the rate used for settlement.
type FundingFeeRecord struct {
	Symbol       string          `json:"symbol"`
	IsolatedMode string          `json:"isolatedMode"`
	FundingFee   decimal.Decimal `json:"fundingFee"`
	FundingCoin  string          `json:"fundingCoin"`
	TimestampMS  int64           `json:"timestamp"`
	FundingRate  decimal.Decimal `json:"fundingRate"`

	Timestamp time.Time `json:"-"`
}

func (f *FundingFeeRecord) UnmarshalJSON(data []byte) error {
	type Alias FundingFeeRecord
	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*f = FundingFeeRecord(aux)
	f.Timestamp = time.UnixMilli(f.TimestampMS).UTC()
	return nil
}

// GetFundingFeeHistory pages the documented funding fee records, newest first.
// All parameters mirror the official contract: empty symbol = all symbols,
// zero times = unbounded, limit is clamped to the documented 1-200 range.
func (c *Client) GetFundingFeeHistory(
	ctx context.Context,
	symbol string,
	startTimeMs, endTimeMs int64,
	limit int,
) ([]FundingFeeRecord, error) {
	query := url.Values{}
	if symbol != "" {
		query.Set("symbol", symbol)
	}
	if startTimeMs > 0 {
		query.Set("startTime", strconv.FormatInt(startTimeMs, 10))
	}
	if endTimeMs > 0 {
		query.Set("endTime", strconv.FormatInt(endTimeMs, 10))
	}
	if limit > 0 {
		if limit > 200 {
			limit = 200
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	var data struct {
		Fundings []FundingFeeRecord `json:"fundings"`
	}
	if err := c.do(ctx, http.MethodGet, "/uapi/v1/trade/fundingFee", query, nil, true, 5, &data); err != nil {
		return nil, err
	}
	return data.Fundings, nil
}
