package pionex

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/shopspring/decimal"
)

type SymbolInfo struct {
	Symbol          string          `json:"symbol"`
	Name            string          `json:"name"`
	BaseCurrency    string          `json:"baseCurrency"`
	QuoteCurrency   string          `json:"quoteCurrency"`
	Type            string          `json:"type"`
	MinAmount       decimal.Decimal `json:"minAmount"`
	BasePrecision   int             `json:"basePrecision"`
	QuotePrecision  int             `json:"quotePrecision"`
	PricePrecision  int             `json:"pricePrecision"`
	AmountPrecision int             `json:"amountPrecision"`
	MinNotional     decimal.Decimal `json:"minNotional"`
	Enabled         bool            `json:"enable"`
	Status          string          `json:"status"`
}

func (s SymbolInfo) GetPricePrecision() int {
	if s.QuotePrecision > 0 {
		return s.QuotePrecision
	}
	if s.PricePrecision > 0 {
		return s.PricePrecision
	}
	return 4
}

func (s SymbolInfo) GetAmountPrecision() int {
	if s.BasePrecision > 0 {
		return s.BasePrecision
	}
	if s.AmountPrecision > 0 {
		return s.AmountPrecision
	}
	return 2
}

func (symbol SymbolInfo) IsTrading() bool {
	return symbol.Enabled || symbol.Status == "TRADING"
}

type TickerInfo struct {
	Symbol string          `json:"symbol"`
	Time   int64           `json:"time"`
	Open   decimal.Decimal `json:"open"`
	Close  decimal.Decimal `json:"close"`
	High   decimal.Decimal `json:"high"`
	Low    decimal.Decimal `json:"low"`
	Volume decimal.Decimal `json:"volume"`
	Amount decimal.Decimal `json:"amount"`
	Count  decimal.Decimal `json:"count"`
}

type KlineCandle struct {
	Time   int64           `json:"time"`
	Open   decimal.Decimal `json:"open"`
	Close  decimal.Decimal `json:"close"`
	High   decimal.Decimal `json:"high"`
	Low    decimal.Decimal `json:"low"`
	Volume decimal.Decimal `json:"volume"`
}

// GetMarketSymbols uses the official Pionex common symbols endpoint.
func (c *Client) GetMarketSymbols(ctx context.Context, symbolType string) ([]SymbolInfo, error) {
	query := url.Values{}
	if symbolType != "" {
		query.Set("type", symbolType)
	}
	var data struct {
		Symbols []SymbolInfo `json:"symbols"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/common/symbols", query, nil, false, 5, &data); err != nil {
		return nil, err
	}
	return data.Symbols, nil
}

func (c *Client) GetTickers(ctx context.Context, symbol, symbolType string) ([]TickerInfo, error) {
	query := url.Values{}
	if symbol != "" {
		query.Set("symbol", symbol)
	} else if symbolType != "" {
		query.Set("type", symbolType)
	}
	var data struct {
		Tickers []TickerInfo `json:"tickers"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/market/tickers", query, nil, false, 5, &data); err != nil {
		return nil, err
	}
	return data.Tickers, nil
}

func (c *Client) GetKlines(
	ctx context.Context,
	symbol, interval string,
	limit int,
) ([]KlineCandle, error) {
	query := url.Values{"symbol": []string{symbol}, "interval": []string{interval}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var data struct {
		Klines []KlineCandle `json:"klines"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/market/klines", query, nil, false, 5, &data); err != nil {
		return nil, err
	}
	return data.Klines, nil
}
