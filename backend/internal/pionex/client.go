package pionex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	DefaultHost    = "https://api.pionex.com"
	RequestTimeout = 15 * time.Second
)

// Client is a Pionex-only REST client. Mutating requests are never retried
// automatically because a transport failure can leave their remote outcome unknown.
type Client struct {
	baseURL        string
	signer         *Signer
	httpClient     *http.Client
	publicLimiter  *RateLimiter
	privateLimiter *RateLimiter
	now            func() time.Time
}

func NewClient(baseURL string, apiKey, apiSecret string) *Client {
	return NewClientWithHTTPClient(baseURL, apiKey, apiSecret, &http.Client{Timeout: RequestTimeout})
}

// NewClientWithHTTPClient exists for contract tests and controlled transports.
func NewClientWithHTTPClient(baseURL, apiKey, apiSecret string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultHost
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: RequestTimeout}
	}
	return &Client{
		baseURL:        strings.TrimRight(baseURL, "/"),
		signer:         NewSigner(apiKey, apiSecret),
		httpClient:     httpClient,
		publicLimiter:  NewRateLimiter(20, 20),
		privateLimiter: NewRateLimiter(10, 10),
		now:            time.Now,
	}
}

type APIEnvelope[T any] struct {
	Result    bool   `json:"result"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
	Data      T      `json:"data"`
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	// OutcomeUnknown is true only when a mutating request was sent but no
	// authoritative response was received.
	OutcomeUnknown bool
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("pionex API error [%s]: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("pionex HTTP error [%d]: %s", e.StatusCode, e.Message)
}

func IsOutcomeUnknown(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.OutcomeUnknown
}

type FuturesBalance struct {
	Coin   string          `json:"coin"`
	Free   decimal.Decimal `json:"free"`
	Frozen decimal.Decimal `json:"frozen"`
	Debts  decimal.Decimal `json:"debts"`
}

// BUOrderData follows the official Futures Grid create contract exactly.
type BUOrderData struct {
	Top                 decimal.Decimal  `json:"top"`
	Bottom              decimal.Decimal  `json:"bottom"`
	Row                 int              `json:"row"`
	GridType            string           `json:"grid_type"`
	Trend               string           `json:"trend"`
	Leverage            int              `json:"leverage"`
	ExtraMargin         *decimal.Decimal `json:"extraMargin,omitempty"`
	QuoteInvestment     decimal.Decimal  `json:"quoteInvestment"`
	LossStopType        string           `json:"lossStopType,omitempty"`
	LossStop            *decimal.Decimal `json:"lossStop,omitempty"`
	ProfitStopType      string           `json:"profitStopType,omitempty"`
	ProfitStop          *decimal.Decimal `json:"profitStop,omitempty"`
	InvestCoin          string           `json:"investCoin,omitempty"`
	InvestmentFrom      string           `json:"investmentFrom,omitempty"`
	MovingIndicatorType string           `json:"movingIndicatorType,omitempty"`
	EnableFollowClosed  bool             `json:"enableFollowClosed,omitempty"`
}

type NativeFuturesGridCreateParams struct {
	Base        string      `json:"base"`
	Quote       string      `json:"quote"`
	BUOrderData BUOrderData `json:"buOrderData"`
}

type NativeFuturesGridCreateResult struct {
	BUOrderID string `json:"buOrderId"`
}

type FuturesGridOrder struct {
	BUOrderID   string              `json:"buOrderId"`
	Base        string              `json:"base"`
	Quote       string              `json:"quote"`
	Status      string              `json:"status"`
	ReasonBy    string              `json:"reasonBy"`
	BUOrderData BUOrderDataResponse `json:"buOrderData"`
}

type BUOrderDataResponse struct {
	Status               string          `json:"status"`
	ReasonBy             string          `json:"reasonBy"`
	TopRaw               json.RawMessage `json:"top"`
	BottomRaw            json.RawMessage `json:"bottom"`
	Row                  int             `json:"row"`
	GridType             string          `json:"gridType"`
	Trend                string          `json:"trend"`
	Leverage             int             `json:"leverage"`
	OpenPriceRaw         json.RawMessage `json:"openPrice"`
	PositionRaw          json.RawMessage `json:"position"`
	PositionOpenPriceRaw json.RawMessage `json:"positionOpenPrice"`
	ProfitWithdrawnRaw   json.RawMessage `json:"profitWithdrawn"`
	TotalProfitRaw       json.RawMessage `json:"totalProfit"`
	RiskStatus           string          `json:"riskStatus"`
	LiquidationPriceRaw  json.RawMessage `json:"liquidationPrice"`

	Top               decimal.Decimal `json:"-"`
	Bottom            decimal.Decimal `json:"-"`
	OpenPrice         decimal.Decimal `json:"-"`
	Position          decimal.Decimal `json:"-"`
	PositionOpenPrice decimal.Decimal `json:"-"`
	ProfitWithdrawn   decimal.Decimal `json:"-"`
	LiquidationPrice  decimal.Decimal `json:"-"`
}

func parseDecimalRaw(raw json.RawMessage) decimal.Decimal {
	if len(raw) == 0 {
		return decimal.Zero
	}
	s := strings.Trim(string(raw), "\" \t\n\r")
	if s == "" || s == "null" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func (b *BUOrderDataResponse) UnmarshalJSON(data []byte) error {
	type Alias BUOrderDataResponse
	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*b = BUOrderDataResponse(aux)
	b.Top = parseDecimalRaw(b.TopRaw)
	b.Bottom = parseDecimalRaw(b.BottomRaw)
	b.OpenPrice = parseDecimalRaw(b.OpenPriceRaw)
	b.Position = parseDecimalRaw(b.PositionRaw)
	b.PositionOpenPrice = parseDecimalRaw(b.PositionOpenPriceRaw)
	b.ProfitWithdrawn = parseDecimalRaw(b.ProfitWithdrawnRaw)
	if b.ProfitWithdrawn.IsZero() {
		b.ProfitWithdrawn = parseDecimalRaw(b.TotalProfitRaw)
	}
	b.LiquidationPrice = parseDecimalRaw(b.LiquidationPriceRaw)
	return nil
}

func (f *FuturesGridOrder) UnmarshalJSON(data []byte) error {
	type Alias FuturesGridOrder
	var aux struct {
		Alias
		KeyID   string `json:"keyId"`
		OrderID string `json:"orderId"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*f = FuturesGridOrder(aux.Alias)
	if f.BUOrderID == "" {
		if aux.OrderID != "" {
			f.BUOrderID = aux.OrderID
		} else if aux.KeyID != "" {
			f.BUOrderID = aux.KeyID
		} else if aux.ID != "" {
			f.BUOrderID = aux.ID
		}
	}
	return nil
}

func (c *Client) CreateFuturesGridBot(
	ctx context.Context,
	params NativeFuturesGridCreateParams,
) (*NativeFuturesGridCreateResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal futures grid create request: %w", err)
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/create", nil, body, true, 1, &raw); err != nil {
		return nil, err
	}
	result := &NativeFuturesGridCreateResult{BUOrderID: findStringField(raw, "buOrderId")}
	return result, nil
}

func (c *Client) GetFuturesGridBot(ctx context.Context, buOrderID string) (*FuturesGridOrder, error) {
	query := url.Values{"buOrderId": []string{buOrderID}}
	var result FuturesGridOrder
	if err := c.do(ctx, http.MethodGet, "/api/v1/bot/orders/futuresGrid/order", query, nil, true, 1, &result); err != nil {
		return nil, err
	}
	if result.BUOrderID == "" {
		result.BUOrderID = buOrderID
	}
	return &result, nil
}

type CancelFuturesGridParams struct {
	BUOrderID     string `json:"buOrderId,omitempty"`
	BotOrderID    string `json:"botOrderId,omitempty"`
	CloseNote     string `json:"closeNote,omitempty"`
	CloseSellMode string `json:"closeSellModel,omitempty"`
	Immediate     bool   `json:"immediate"`
	CloseSlippage string `json:"closeSlippage,omitempty"`
}

func (c *Client) CancelFuturesGridBot(ctx context.Context, params CancelFuturesGridParams) error {
	if params.BUOrderID != "" && params.BotOrderID == "" {
		params.BotOrderID = params.BUOrderID
	}
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal futures grid cancel request: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/cancel", nil, body, true, 1, nil)
}

func (c *Client) GetFuturesBalances(ctx context.Context) ([]FuturesBalance, error) {
	var data struct {
		Balances []FuturesBalance `json:"balances"`
	}
	if err := c.do(ctx, http.MethodGet, "/uapi/v1/account/balances", nil, nil, true, 5, &data); err != nil {
		return nil, err
	}
	return data.Balances, nil
}

func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
	private bool,
	weight int,
	out any,
) error {
	limiter := c.publicLimiter
	if private {
		limiter = c.privateLimiter
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := limiter.Wait(ctx, weight); err != nil {
			return err
		}
		var req *http.Request
		var err error
		if private {
			req, err = c.newSignedRequest(ctx, method, path, query, body)
		} else {
			req, err = c.newPublicRequest(ctx, method, path, query, body)
		}
		if err != nil {
			return err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if method != http.MethodGet {
				return &APIError{Message: err.Error(), OutcomeUnknown: true}
			}
			if attempt < attempts {
				if err := waitRetry(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("pionex request failed: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			if method != http.MethodGet {
				return &APIError{StatusCode: resp.StatusCode, Message: readErr.Error(), OutcomeUnknown: true}
			}
			return fmt.Errorf("read pionex response: %w", readErr)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			limiter.TriggerCooldown(60 * time.Second)
			return &APIError{StatusCode: resp.StatusCode, Code: "RATE_LIMITED", Message: "60 second cooldown activated"}
		}
		if resp.StatusCode >= 500 && method == http.MethodGet && attempt < attempts {
			if err := waitRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}

		var envelope APIEnvelope[json.RawMessage]
		if err := json.Unmarshal(responseBody, &envelope); err != nil {
			return &APIError{
				StatusCode:     resp.StatusCode,
				Message:        "invalid JSON response",
				OutcomeUnknown: method != http.MethodGet && resp.StatusCode >= 500,
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Result {
			message := envelope.Message
			if message == "" {
				message = http.StatusText(resp.StatusCode)
			}
			return &APIError{StatusCode: resp.StatusCode, Code: envelope.Code, Message: message}
		}
		if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return nil
		}
		if raw, ok := out.(*json.RawMessage); ok {
			*raw = append((*raw)[:0], envelope.Data...)
			return nil
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode pionex response data: %w", err)
		}
		return nil
	}
	return errors.New("pionex request retries exhausted")
}

func (c *Client) newPublicRequest(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
) (*http.Request, error) {
	if query == nil {
		query = url.Values{}
	}
	requestURL := c.baseURL + path
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) newSignedRequest(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
) (*http.Request, error) {
	if query == nil {
		query = url.Values{}
	}
	timestamp := c.now().UnixMilli()
	signature, err := c.signer.SignRequest(method, path, query, body, timestamp)
	if err != nil {
		return nil, fmt.Errorf("sign pionex request: %w", err)
	}
	signedQuery := cloneValues(query)
	signedQuery.Set("timestamp", strconv.FormatInt(timestamp, 10))
	req, err := c.newPublicRequest(ctx, method, path, signedQuery, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PIONEX-KEY", c.signer.apiKey)
	req.Header.Set("PIONEX-SIGNATURE", signature)
	return req, nil
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

func waitRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt*200) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func findStringField(raw json.RawMessage, field string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(current any) string {
		switch item := current.(type) {
		case map[string]any:
			if candidate, ok := item[field].(string); ok && candidate != "" {
				return candidate
			}
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}
