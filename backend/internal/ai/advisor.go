package ai

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

var (
	ErrFuturesAiUnsupported = errors.New("ai advisor: Pionex Spot AI strategy parameters CANNOT be applied to Futures Grid bots")
)

// Proposal represents an AI advisory recommendation.
type Proposal struct {
	CapabilityID string          `json:"capabilityId"`
	Symbol       string          `json:"symbol"`
	IsFutures    bool            `json:"isFutures"`
	LowerPrice   decimal.Decimal `json:"lowerPrice"`
	UpperPrice   decimal.Decimal `json:"upperPrice"`
	GridNum      int             `json:"gridNum"`
	Explanation  string          `json:"explanation"`
}

// Advisor enforces strict boundaries on AI proposals.
type Advisor struct{}

// NewAdvisor creates a new AI Advisor.
func NewAdvisor() *Advisor {
	return &Advisor{}
}

// ValidateProposal inspects AI proposals and rejects invalid/dangerous Futures AI applications.
func (a *Advisor) ValidateProposal(ctx context.Context, p Proposal) error {
	if p.IsFutures {
		return fmt.Errorf("%w: attempted symbol %s", ErrFuturesAiUnsupported, p.Symbol)
	}

	if p.LowerPrice.GreaterThanOrEqual(p.UpperPrice) {
		return errors.New("ai advisor: lower price must be strictly less than upper price")
	}

	if p.GridNum <= 1 {
		return errors.New("ai advisor: grid count must be greater than 1")
	}

	return nil
}
