package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/shopspring/decimal"
)

// FuturesOrderRequest represents params to place an ordinary Futures order.
type FuturesOrderRequest struct {
	Symbol        string          `json:"symbol"`
	Side          string          `json:"side"` // BUY, SELL
	OrderType     string          `json:"type"` // LIMIT, MARKET
	ClientOrderID string          `json:"clientOrderId"`
	Price         decimal.Decimal `json:"price,omitempty"`
	Amount        decimal.Decimal `json:"amount"`
}

// FuturesOrderResult represents the returned order placement response.
type FuturesOrderResult struct {
	OrderID       string `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
}

// FuturesPosition represents current active futures position details.
type FuturesPosition struct {
	Symbol           string          `json:"symbol"`
	Side             string          `json:"side"` // LONG, SHORT
	Amount           decimal.Decimal `json:"amount"`
	EntryPrice       decimal.Decimal `json:"entryPrice"`
	MarkPrice        decimal.Decimal `json:"markPrice"`
	UnrealizedPNL    decimal.Decimal `json:"unrealizedPnl"`
	Leverage         int             `json:"leverage"`
	LiquidationPrice decimal.Decimal `json:"liquidationPrice"`
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
