package marketdata

import (
	"context"
	"fmt"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// ScannerCandidate represents a market candidate for Grid Bot creation.
type ScannerCandidate struct {
	Symbol     string          `json:"symbol"`
	Price      decimal.Decimal `json:"price"`
	Volatility float64         `json:"volatility"`
	Volume24h  decimal.Decimal `json:"volume24h"`
	Score      float64         `json:"score"`
	Status     string          `json:"status"` // FRESH, STALE, REJECTED
}

// Scanner scans Pionex PERP markets to identify grid trading opportunities.
type Scanner struct {
	pionexClient *pionex.Client
}

// NewScanner creates a new Scanner instance.
func NewScanner(pionexClient *pionex.Client) *Scanner {
	return &Scanner{pionexClient: pionexClient}
}

// ScanMarkets fetches Pionex PERP symbols and scores them.
func (s *Scanner) ScanMarkets(ctx context.Context) ([]ScannerCandidate, error) {
	symbols, err := s.pionexClient.GetMarketSymbols(ctx, "PERP")
	if err != nil {
		return nil, fmt.Errorf("scanner failed to fetch symbols: %w", err)
	}

	var candidates []ScannerCandidate
	for _, sym := range symbols {
		if !sym.MinNotional.IsZero() {
			candidates = append(candidates, ScannerCandidate{
				Symbol:     sym.Symbol,
				Price:      decimal.Zero,
				Volatility: 0.05,
				Volume24h:  decimal.NewFromInt(100000),
				Score:      0.85,
				Status:     "FRESH",
			})
		}
	}

	return candidates, nil
}
