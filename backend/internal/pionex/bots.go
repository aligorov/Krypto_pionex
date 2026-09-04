package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/shopspring/decimal"
)

// SpotGridAIStrategy is the official Pionex AI Kit recommendation for a spot
// grid pair (GET /api/v1/bot/orders/spotGrid/aiStrategy). Per AGENTS.md these
// price parameters are SPOT-only and must never configure a Futures Grid bot;
// the bot uses them as native market intelligence (volatility/APR/drawdown).
type SpotGridAIStrategy struct {
	Annualized  decimal.Decimal `json:"annualized"`
	TotalAPR    decimal.Decimal `json:"totalApr"`
	High        decimal.Decimal `json:"high"`
	Low         decimal.Decimal `json:"low"`
	GridCount   int             `json:"gridCount"`
	StrategyID  string          `json:"strategyId"`
	Volatility  decimal.Decimal `json:"volatility"`
	MaxDrawDown decimal.Decimal `json:"maxDrawDown"`
	Options     []AIOption      `json:"options"`
}

type AIOption struct {
	Period         int             `json:"period"`
	Annualized     decimal.Decimal `json:"annualized"`
	High           decimal.Decimal `json:"high"`
	Low            decimal.Decimal `json:"low"`
	GridCount      int             `json:"gridCount"`
	Volatility     decimal.Decimal `json:"volatility"`
	MaxDrawDown    decimal.Decimal `json:"maxDrawDown"`
	SuitabilityMin decimal.Decimal `json:"suitabilityMin"`
	SuitabilityMax decimal.Decimal `json:"suitabilityMax"`
}

// GetSpotGridAIStrategy calls the native Pionex AI Kit endpoint.
func (c *Client) GetSpotGridAIStrategy(
	ctx context.Context,
	base, quote string,
) (*SpotGridAIStrategy, error) {
	query := url.Values{"base": []string{base}, "quote": []string{quote}}
	var strategy SpotGridAIStrategy
	if err := c.do(ctx, http.MethodGet, "/api/v1/bot/orders/spotGrid/aiStrategy", query, nil, true, 1, &strategy); err != nil {
		return nil, err
	}
	return &strategy, nil
}

// FuturesGridCheckParamsResult carries the native exchange-side validation
// verdict returned by POST /api/v1/bot/orders/futuresGrid/checkParams.
type FuturesGridCheckParamsResult struct {
	MinInvestment           decimal.Decimal `json:"min_investment"`
	MinInvestmentCamel      decimal.Decimal `json:"minInvestment,omitempty"`
	MaxInvestment           decimal.Decimal `json:"max_investment"`
	MaxInvestmentCamel      decimal.Decimal `json:"maxInvestment,omitempty"`
	Slippage                decimal.Decimal `json:"slippage"`
	EstimatePerVolume       decimal.Decimal `json:"estimate_per_volume"`
	EstimateInvestment      decimal.Decimal `json:"estimate_investment"`
	EstimateFee             decimal.Decimal `json:"estimate_fee"`
	EstimateLiquidationUp   decimal.Decimal `json:"estimate_liquidation_price_up"`
	EstimateLiquidationDown decimal.Decimal `json:"estimate_liquidation_price_down"`
}

func (r *FuturesGridCheckParamsResult) GetMinInvestment() decimal.Decimal {
	if r == nil {
		return decimal.Zero
	}
	if r.MinInvestment.GreaterThan(decimal.Zero) {
		return r.MinInvestment
	}
	return r.MinInvestmentCamel
}

// CheckFuturesGridParams validates grid parameters against Pionex before any
// real capital is committed. It never places an order.
func (c *Client) CheckFuturesGridParams(
	ctx context.Context,
	params NativeFuturesGridCreateParams,
) (*FuturesGridCheckParamsResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal futures grid checkParams request: %w", err)
	}
	var result FuturesGridCheckParamsResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/checkParams", nil, body, true, 1, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AdjustFuturesGridParams adjusts a running native futures grid bot through
// POST /api/v1/bot/orders/futuresGrid/adjustParams. Type is "invest_in"
// (add quoteInvestment) or "adjust_params" (move bottom/top/row).
// extraMargin and openPrice are REQUIRED by the official contract for every
// type; openPrice is the current market price used to re-anchor the grid.
type AdjustFuturesGridParams struct {
	BUOrderID       string           `json:"buOrderId"`
	Type            string           `json:"type"`
	ExtraMargin     bool             `json:"extraMargin"`
	OpenPrice       *decimal.Decimal `json:"openPrice,omitempty"`
	Bottom          *decimal.Decimal `json:"bottom,omitempty"`
	Top             *decimal.Decimal `json:"top,omitempty"`
	Row             int              `json:"row,omitempty"`
	QuoteInvestment *decimal.Decimal `json:"quoteInvestment,omitempty"`
}

func (c *Client) AdjustFuturesGridBot(
	ctx context.Context,
	params AdjustFuturesGridParams,
) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal futures grid adjust request: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/adjustParams", nil, body, true, 1, nil)
}

// BotOrder is an entry of the documented account-wide bot order list
// (GET /api/v1/bot/orders). The shape is shared across bot types, so the
// type-specific grid payload stays raw.
type BotOrder struct {
	BUOrderID    string          `json:"buOrderId"`
	BUOrderType  string          `json:"buOrderType"`
	Status       string          `json:"status"`
	Canceling    bool            `json:"canceling"`
	Base         string          `json:"base"`
	Quote        string          `json:"quote"`
	CreateTimeMS int64           `json:"createTime"`
	BUOrderData  json.RawMessage `json:"buOrderData"`
}

// GridInvestment extracts the quote investment from the dynamic buOrderData
// payload, tolerating camelCase and snake_case keys with string or numeric
// values. Returns false when no investment field is present.
func (order BotOrder) GridInvestment() (decimal.Decimal, bool) {
	if len(order.BUOrderData) == 0 {
		return decimal.Zero, false
	}
	var payload map[string]any
	if err := json.Unmarshal(order.BUOrderData, &payload); err != nil {
		return decimal.Zero, false
	}
	for _, key := range []string{"quoteInvestment", "quote_investment", "investment", "investAmount"} {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			if parsed, err := decimal.NewFromString(value); err == nil {
				return parsed, true
			}
		case float64:
			return decimal.NewFromFloat(value), true
		}
	}
	return decimal.Zero, false
}

// FuturesGridData decodes the dynamic buOrderData of a futures-grid list
// entry into the typed detail payload, so a finished bot found through
// GET /api/v1/bot/orders carries the same profit accessors as the
// order-detail endpoint response.
func (order BotOrder) FuturesGridData() (*BUOrderDataResponse, error) {
	if len(order.BUOrderData) == 0 {
		return nil, fmt.Errorf("bot order %s carries no buOrderData", order.BUOrderID)
	}
	var data BUOrderDataResponse
	if err := json.Unmarshal(order.BUOrderData, &data); err != nil {
		return nil, fmt.Errorf("decode bot order buOrderData: %w", err)
	}
	return &data, nil
}

// ListBotOrders pages through the documented bot order list
// (GET /api/v1/bot/orders) filtered to futures grids. status is "running"
// (default) or "finished"; pass an empty pageToken to start. The next token
// is returned empty at the end of the list.
func (c *Client) ListBotOrders(
	ctx context.Context,
	status, pageToken string,
) ([]BotOrder, string, error) {
	query := url.Values{"buOrderTypes": []string{"futures_grid"}}
	if status != "" {
		query.Set("status", status)
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/api/v1/bot/orders", query, nil, true, 1, &raw); err != nil {
		return nil, "", err
	}
	var envelope struct {
		Orders           []BotOrder `json:"orders"`
		BotOrders        []BotOrder `json:"botOrders"`
		Items            []BotOrder `json:"items"`
		NextPageToken    string     `json:"nextPageToken"`
		NextPageTokenAlt string     `json:"next_page_token"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", fmt.Errorf("decode bot order list: %w", err)
	}
	orders := envelope.Orders
	if len(orders) == 0 {
		orders = envelope.BotOrders
	}
	if len(orders) == 0 {
		orders = envelope.Items
	}
	next := envelope.NextPageToken
	if next == "" {
		next = envelope.NextPageTokenAlt
	}
	return orders, next, nil
}

// SpotBalance is a trading-account balance entry from the official spot
// endpoint GET /api/v1/account/balances (excludes bot and earn accounts).
type SpotBalance struct {
	Coin   string          `json:"coin"`
	Free   decimal.Decimal `json:"free"`
	Frozen decimal.Decimal `json:"frozen"`
}

func (c *Client) GetSpotBalances(ctx context.Context) ([]SpotBalance, error) {
	var data struct {
		Balances []SpotBalance `json:"balances"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/account/balances", nil, nil, true, 1, &data); err != nil {
		return nil, err
	}
	return data.Balances, nil
}
