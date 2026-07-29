package pionex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

const (
	DefaultHost      = "https://api.pionex.com"
	RequestTimeout   = 15 * time.Second
)

// Client represents the Pionex API REST client.
type Client struct {
	baseURL    string
	signer     *Signer
	httpClient *http.Client
}

// NewClient initializes a Pionex REST Client.
func NewClient(baseURL string, apiKey, apiSecret string) *Client {
	if baseURL == "" {
		baseURL = DefaultHost
	}
	return &Client{
		baseURL: baseURL,
		signer:  NewSigner(apiKey, apiSecret),
		httpClient: &http.Client{
			Timeout: RequestTimeout,
		},
	}
}

// APIEnvelope represents the standard Pionex API response wrapper.
type APIEnvelope[T any] struct {
	Result    bool   `json:"result"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
	Data      T      `json:"data"`
}

// FuturesBalance represents account balance data for Futures.
type FuturesBalance struct {
	Coin      string          `json:"coin"`
	Free      decimal.Decimal `json:"free"`
	Used      decimal.Decimal `json:"used"`
	Total     decimal.Decimal `json:"total"`
}

// NativeFuturesGridCreateParams matches official Pionex Futures Grid creation spec.
type NativeFuturesGridCreateParams struct {
	Symbol          string          `json:"symbol"`
	GridType        string          `json:"gridType"` // ARITHMETIC or GEOMETRIC
	Direction       string          `json:"direction"` // LONG, SHORT, NEUTRAL
	LowerPrice      decimal.Decimal `json:"lowerPrice"`
	UpperPrice      decimal.Decimal `json:"upperPrice"`
	GridNum         int             `json:"gridNum"`
	Leverage        int             `json:"leverage"`
	QuoteInvestment decimal.Decimal `json:"quoteInvestment"`
	ExtraMargin     decimal.Decimal `json:"extraMargin"`
	StopLoss        decimal.Decimal `json:"stopLoss,omitempty"`
	TakeProfit      decimal.Decimal `json:"takeProfit,omitempty"`
}

// NativeFuturesGridCreateResult contains the resulting remote buOrderId.
type NativeFuturesGridCreateResult struct {
	BUOrderID string `json:"buOrderId"`
}

// CreateFuturesGridBot submits a native grid bot request to Pionex.
func (c *Client) CreateFuturesGridBot(ctx context.Context, params NativeFuturesGridCreateParams) (*NativeFuturesGridCreateResult, error) {
	path := "/api/v1/bot/futuresGrid/create"
	bodyBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal grid params: %w", err)
	}

	req, err := c.newSignedRequest(ctx, http.MethodPost, path, nil, bodyBytes)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var env APIEnvelope[NativeFuturesGridCreateResult]
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pionex response: %w", err)
	}

	if !env.Result {
		return nil, fmt.Errorf("pionex API error [%s]: %s", env.Code, env.Message)
	}

	return &env.Data, nil
}

func (c *Client) newSignedRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	if query == nil {
		query = url.Values{}
	}
	timestamp := time.Now().UnixMilli()

	sig, err := c.signer.SignRequest(method, path, query, body, timestamp)
	if err != nil {
		return nil, fmt.Errorf("signing failed: %w", err)
	}

	query.Set("timestamp", strconv.FormatInt(timestamp, 10))
	fullURL := fmt.Sprintf("%s%s?%s", c.baseURL, path, query.Encode())

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PIONEX-KEY", c.signer.apiKey)
	req.Header.Set("PIONEX-SIGNATURE", sig)

	return req, nil
}
