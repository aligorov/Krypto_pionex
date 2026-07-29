package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	path := "/uapi/v1/trade/order"
	bodyBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal futures order: %w", err)
	}

	req, err := c.newSignedRequest(ctx, http.MethodPost, path, nil, bodyBytes)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("futures order place request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env APIEnvelope[FuturesOrderResult]
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal futures order response: %w", err)
	}

	if !env.Result {
		return nil, fmt.Errorf("pionex API order placement error [%s]: %s", env.Code, env.Message)
	}

	return &env.Data, nil
}

// GetFuturesPositions fetches active futures positions using /uapi/v1/trade/position.
func (c *Client) GetFuturesPositions(ctx context.Context) ([]FuturesPosition, error) {
	path := "/uapi/v1/trade/position"
	req, err := c.newSignedRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env APIEnvelope[struct {
		Positions []FuturesPosition `json:"positions"`
	}]

	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal positions: %w", err)
	}

	if !env.Result {
		return nil, fmt.Errorf("pionex API position error [%s]: %s", env.Code, env.Message)
	}

	return env.Data.Positions, nil
}
