package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/shopspring/decimal"
)

// SymbolInfo represents details of a market trading pair.
type SymbolInfo struct {
	Symbol          string          `json:"symbol"`
	BaseCurrency    string          `json:"baseCurrency"`
	QuoteCurrency   string          `json:"quoteCurrency"`
	Type            string          `json:"type"` // SPOT, PERP
	MinAmount       decimal.Decimal `json:"minAmount"`
	PricePrecision  int             `json:"pricePrecision"`
	AmountPrecision int             `json:"amountPrecision"`
	MinNotional     decimal.Decimal `json:"minNotional"`
}

// TickerInfo represents ticker price data.
type TickerInfo struct {
	Symbol    string          `json:"symbol"`
	Close     decimal.Decimal `json:"close"`
	High      decimal.Decimal `json:"high"`
	Low       decimal.Decimal `json:"low"`
	Volume    decimal.Decimal `json:"volume"`
	Timestamp int64           `json:"timestamp"`
}

// KlineCandle represents OHLCV candle.
type KlineCandle struct {
	OpenTime  int64           `json:"openTime"`
	Open      decimal.Decimal `json:"open"`
	High      decimal.Decimal `json:"high"`
	Low       decimal.Decimal `json:"low"`
	Close     decimal.Decimal `json:"close"`
	Volume    decimal.Decimal `json:"volume"`
	CloseTime int64           `json:"closeTime"`
}

// GetMarketSymbols fetches active Pionex symbols.
func (c *Client) GetMarketSymbols(ctx context.Context, symbolType string) ([]SymbolInfo, error) {
	path := "/api/v1/market/symbols"
	query := url.Values{}
	if symbolType != "" {
		query.Set("type", symbolType)
	}

	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, path, query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env APIEnvelope[struct {
		Symbols []SymbolInfo `json:"symbols"`
	}]

	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal symbols: %w", err)
	}

	if !env.Result {
		return nil, fmt.Errorf("pionex API error [%s]: %s", env.Code, env.Message)
	}

	return env.Data.Symbols, nil
}

// GetKlines fetches historical OHLCV candle data.
func (c *Client) GetKlines(ctx context.Context, symbol, interval string, limit int) ([]KlineCandle, error) {
	path := "/api/v1/market/klines"
	query := url.Values{}
	query.Set("symbol", symbol)
	query.Set("interval", interval)
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}

	reqURL := fmt.Sprintf("%s%s?%s", c.baseURL, path, query.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env APIEnvelope[struct {
		Klines []KlineCandle `json:"klines"`
	}]

	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("failed to unmarshal klines: %w", err)
	}

	if !env.Result {
		return nil, fmt.Errorf("pionex API error [%s]: %s", env.Code, env.Message)
	}

	return env.Data.Klines, nil
}
