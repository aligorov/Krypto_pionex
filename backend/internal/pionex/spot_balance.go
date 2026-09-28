package pionex

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/shopspring/decimal"
)

// GetBotFundingUSDT reads spendable Spot USDT, not Futures trader margin.
// Official contract: /docs/api-docs/trade-api/account. Frozen funds and
// balances already allocated to bots/earn are not available for a new bot.
func (c *Client) GetBotFundingUSDT(ctx context.Context) (decimal.Decimal, error) {
	var data struct {
		Balances *[]struct {
			Coin string           `json:"coin"`
			Free *decimal.Decimal `json:"free"`
		} `json:"balances"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/account/balances", nil, nil, true, 1, &data); err != nil {
		return decimal.Zero, err
	}
	if data.Balances == nil {
		return decimal.Zero, fmt.Errorf("bot funding: missing Spot balances array")
	}
	var available decimal.Decimal
	found := false
	for _, balance := range *data.Balances {
		if !strings.EqualFold(balance.Coin, "USDT") {
			continue
		}
		if found || balance.Free == nil || balance.Free.IsNegative() {
			return decimal.Zero, fmt.Errorf("bot funding: invalid or duplicate Spot USDT balance")
		}
		available, found = *balance.Free, true
	}
	// An explicit balances list without USDT has no spendable USDT.
	return available, nil
}
