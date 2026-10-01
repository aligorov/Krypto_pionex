package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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

// futuresGridCheckParamsRequest is the checkParams wire format: unlike the
// create endpoint (camelCase buOrderData: quoteInvestment, extraMargin...),
// checkParams expects SNAKE_CASE inside buOrderData (quote_investment,
// grid_type, extra_margin...). Sending the create-shaped struct made the
// exchange ignore the investment/leverage context — every prod checkParams
// either failed (and the deployer fail-opened past it) or returned estimates
// computed without capital, so the liquidation estimates came back empty and
// the v2.0.161 guard never armed (prod 2026-10-01: all six running bots had
// NULL liq columns). v2.0.168: a dedicated request shape, contract-tested.
type futuresGridCheckParamsRequest struct {
	Base        string                    `json:"base"`
	Quote       string                    `json:"quote"`
	BUOrderData futuresGridCheckParamsData `json:"buOrderData"`
}

type futuresGridCheckParamsData struct {
	Top                string           `json:"top"`
	Bottom             string           `json:"bottom"`
	Row                int              `json:"row"`
	GridType           string           `json:"grid_type"`
	Trend              string           `json:"trend"`
	Leverage           int              `json:"leverage"`
	QuoteInvestment decimal.Decimal `json:"quote_investment"`
}

// checkParamsRequestFromCreate converts the create-shaped params into the
// checkParams wire format (field mapping is 1:1; only the casing differs).
// The base is normalized to the documented `X.PERP` form HERE — review P0
// (v2.0.168): the scanner REAL lane passed a bare "BTC" and the documented
// endpoint rejects it, so with fail-closed deploys every fresh candidate
// would have been refused. Normalizing in one place means no lane can
// diverge again (the create lifecycle re-normalizes independently).
func checkParamsRequestFromCreate(p NativeFuturesGridCreateParams) futuresGridCheckParamsRequest {
	base := p.Base
	if base != "" && !strings.HasSuffix(base, ".PERP") {
		base = base + ".PERP"
	}
	return futuresGridCheckParamsRequest{
		Base:  base,
		Quote: p.Quote,
		BUOrderData: futuresGridCheckParamsData{
			Top:               p.BUOrderData.Top.String(),
			Bottom:            p.BUOrderData.Bottom.String(),
			Row:               p.BUOrderData.Row,
			GridType:          p.BUOrderData.GridType,
			Trend:             p.BUOrderData.Trend,
			Leverage:          p.BUOrderData.Leverage,
			QuoteInvestment: p.BUOrderData.QuoteInvestment,
		},
	}
}

// CheckFuturesGridParams validates grid parameters against Pionex before any
// real capital is committed. It never places an order.
func (c *Client) CheckFuturesGridParams(
	ctx context.Context,
	params NativeFuturesGridCreateParams,
) (*FuturesGridCheckParamsResult, error) {
	body, err := json.Marshal(checkParamsRequestFromCreate(params))
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
//
// KeepInvestment (adjust_params only) maps to the documented keepInvestment
// flag: "only modify grid range/row without resetting the investment amount.
// Overrides isReinvest..., skips the PnL check (PROFIT_LESS_THAN_ZERO), but
// still validates the price range" — a pure range transfer with no PnL
// realization. nil omits the field (exchange default false = the floating
// PnL > 0 gate applies).
type AdjustFuturesGridParams struct {
	BUOrderID       string           `json:"buOrderId"`
	Type            string           `json:"type"`
	ExtraMargin     bool             `json:"extraMargin"`
	OpenPrice       *decimal.Decimal `json:"openPrice,omitempty"`
	Bottom          *decimal.Decimal `json:"bottom,omitempty"`
	Top             *decimal.Decimal `json:"top,omitempty"`
	Row             int              `json:"row,omitempty"`
	QuoteInvestment *decimal.Decimal `json:"quoteInvestment,omitempty"`
	KeepInvestment  *bool            `json:"keepInvestment,omitempty"`
}

// FuturesGridAdjustCheckResult carries the optional payload of the documented
// dry-run endpoint POST /api/v1/bot/orders/futuresGrid/adjustParamsCheck
// (weight 1). The verdict itself is the envelope: a refused check returns
// result=false (BOT_INVALID_ARGUMENT / PROFIT_LESS_THAN_ZERO / range
// validation) and surfaces as *APIError, exactly like the live call — that is
// the whole point of asking the check before committing the shift.
type FuturesGridAdjustCheckResult struct {
	MinInvestment      decimal.Decimal `json:"min_investment,omitempty"`
	MinInvestmentCamel decimal.Decimal `json:"minInvestment,omitempty"`
	Slippage           decimal.Decimal `json:"slippage,omitempty"`
	EstimateFee        decimal.Decimal `json:"estimate_fee,omitempty"`
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

// CheckAdjustFuturesGridBot is the dry-run twin of AdjustFuturesGridBot: the
// documented adjustParamsCheck endpoint (weight 1) with the IDENTICAL body —
// including keepInvestment — validates the move (range, row, PnL gate
// overrides) without touching the live grid. A non-nil error means the
// exchange already refused this exact payload, so the live call is a
// guaranteed rejection and must not be sent.
func (c *Client) CheckAdjustFuturesGridBot(
	ctx context.Context,
	params AdjustFuturesGridParams,
) (*FuturesGridAdjustCheckResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal futures grid adjust check request: %w", err)
	}
	var result FuturesGridAdjustCheckResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/adjustParamsCheck", nil, body, true, 1, &result); err != nil {
		return nil, err
	}
	return &result, nil
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
		// `results` is the DOCUMENTED key of BotOrderListResponse; the
		// others are tolerated aliases. Missing it made the finished-list
		// path decode an empty array and report "exchange has no final".
		Results          []BotOrder `json:"results"`
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
	if len(orders) == 0 {
		orders = envelope.Results
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

// FuturesGridTriggerProfitLossItem represents a single trigger in the
// POST /api/v1/bot/orders/futuresGrid/updateTriggerProfitLoss endpoint.
// Per official OpenAPI spec, field names are strictly snake_case.
type FuturesGridTriggerProfitLossItem struct {
	Type                string  `json:"type"`                           // "stop_loss" | "stop_profit"
	StopType            string  `json:"stop_type"`                      // "price" | "price_limit" | "profit_amount" | "profit_ratio"
	Value               string  `json:"value"`                          // target price/ratio, or "" to clear
	LimitPrice          *string `json:"limit_price,omitempty"`          // limit price when stop_type="price_limit"
	StopDelay           *int64  `json:"stop_delay,omitempty"`           // delay in seconds before triggering (0 = immediate)
	LossStopSellModel   string  `json:"loss_stop_sell_model,omitempty"` // "TO_QUOTE" | "TO_USDT"
	ProfitStopSellModel string  `json:"profit_stop_sell_model,omitempty"` // "TO_QUOTE" | "TO_USDT"
	StopHighPrice       *string `json:"stop_high_price,omitempty"`      // upper stop-loss for neutral grid (no_trend)
	LimitHighPrice      *string `json:"limit_high_price,omitempty"`     // upper limit stop for neutral grid
}

// FuturesGridUpdateTriggerProfitLossRequest payload for updating or clearing
// stop-loss and take-profit on a running futures grid bot in real time.
type FuturesGridUpdateTriggerProfitLossRequest struct {
	BUOrderID string                             `json:"bu_order_id"`
	List      []FuturesGridTriggerProfitLossItem `json:"list"`
}

// FuturesGridTriggerProfitLossResponse is the echo response returned by
// POST /api/v1/bot/orders/futuresGrid/updateTriggerProfitLoss.
type FuturesGridTriggerProfitLossResponse struct {
	BUOrderID string                             `json:"bu_order_id"`
	Result    int32                              `json:"result"`
	List      []FuturesGridTriggerProfitLossItem `json:"list"`
}

// UpdateFuturesGridTriggerProfitLoss sets, updates, or clears the take-profit
// and/or stop-loss of a running native Pionex Futures Grid order in place.
// Weight: 1. Field names are strictly snake_case.
func (c *Client) UpdateFuturesGridTriggerProfitLoss(
	ctx context.Context,
	req FuturesGridUpdateTriggerProfitLossRequest,
) (*FuturesGridTriggerProfitLossResponse, error) {
	if req.BUOrderID == "" {
		return nil, fmt.Errorf("bu_order_id is required")
	}
	if len(req.List) == 0 {
		return nil, fmt.Errorf("trigger list cannot be empty")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal futures grid updateTriggerProfitLoss request: %w", err)
	}
	var resp FuturesGridTriggerProfitLossResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/updateTriggerProfitLoss", nil, body, true, 1, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

