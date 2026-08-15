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
	MaxInvestment           decimal.Decimal `json:"max_investment"`
	Slippage                decimal.Decimal `json:"slippage"`
	EstimatePerVolume       decimal.Decimal `json:"estimate_per_volume"`
	EstimateInvestment      decimal.Decimal `json:"estimate_investment"`
	EstimateFee             decimal.Decimal `json:"estimate_fee"`
	EstimateLiquidationUp   decimal.Decimal `json:"estimate_liquidation_price_up"`
	EstimateLiquidationDown decimal.Decimal `json:"estimate_liquidation_price_down"`
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
type AdjustFuturesGridParams struct {
	BUOrderID       string          `json:"buOrderId"`
	Type            string          `json:"type"`
	Bottom          decimal.Decimal `json:"bottom,omitempty"`
	Top             decimal.Decimal `json:"top,omitempty"`
	Row             int             `json:"row,omitempty"`
	QuoteInvestment decimal.Decimal `json:"quoteInvestment,omitempty"`
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
