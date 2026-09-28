package autogrid

import (
	"github.com/shopspring/decimal"
	"testing"
)

func TestFitBotFundingBudget(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		equity, committed, budget, want int64
		tranche                         bool
	}{
		{"zero cannot restore budget", 0, 0, 100, 0, true},
		{"negative equity", -5, 0, 100, 0, true},
		{"reserve exhausted", 100, 70, 100, 0, true},
		{"reserve overdrawn", 100, 90, 100, 0, true},
		{"whole slot counted once", 253, 0, 100, 100, true},
		{"scale whole slot", 100, 0, 100, 70, true},
		{"existing commitments", 253, 100, 100, 77, true},
		{"below tranche minimum", 12, 0, 100, 0, true},
		{"minimum tranche slot", 15, 0, 100, 10, true},
		{"below single minimum", 6, 0, 100, 0, false},
		{"single minimum", 8, 0, 100, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, scaled := fitBotFundingBudget(decimal.NewFromInt(tc.equity), decimal.NewFromInt(tc.committed), decimal.NewFromInt(tc.budget), tc.tranche)
			if !v.Equal(decimal.NewFromInt(tc.want)) || scaled != (tc.want != tc.budget) {
				t.Fatalf("got %s scaled=%v; want %d", v, scaled, tc.want)
			}
		})
	}
}
