package autogrid

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// Portfolio correlation gate constants (closing Master-Plan P0 §4).
const (
	// MaxCorrelatedBotsSameDirection is the maximum number of simultaneous
	// running bots allowed to share high correlation in the same direction.
	MaxCorrelatedBotsSameDirection = 2

	// HighCorrelationThreshold marks strong co-movement across crypto assets.
	HighCorrelationThreshold = 0.75

	// CorrelationLookbackCandles is the window of 15M returns used (8 hours).
	CorrelationLookbackCandles = 32
)

// PearsonCorrelation computes Pearson correlation coefficient between two equal-length return slices.
func PearsonCorrelation(a, b []float64) float64 {
	n := len(a)
	if n < 10 || len(b) != n {
		return 0
	}
	sumA, sumB := 0.0, 0.0
	for i := 0; i < n; i++ {
		sumA += a[i]
		sumB += b[i]
	}
	meanA := sumA / float64(n)
	meanB := sumB / float64(n)

	var cov, varA, varB float64
	for i := 0; i < n; i++ {
		diffA := a[i] - meanA
		diffB := b[i] - meanB
		cov += diffA * diffB
		varA += diffA * diffA
		varB += diffB * diffB
	}
	if varA <= 0 || varB <= 0 {
		return 0
	}
	return cov / math.Sqrt(varA*varB)
}

// extractReturnsFromCandles calculates consecutive log-returns from klines sorted ascending.
func extractReturnsFromCandles(candles []pionex.KlineCandle) []float64 {
	if len(candles) < 2 {
		return nil
	}
	sorted := append([]pionex.KlineCandle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })

	returns := make([]float64, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		prev, _ := sorted[i-1].Close.Float64()
		curr, _ := sorted[i].Close.Float64()
		if prev <= 0 || curr <= 0 {
			continue
		}
		returns = append(returns, math.Log(curr/prev))
	}
	return returns
}

// runningDirectionalBot carries symbol and trend for an active bot.
type runningDirectionalBot struct {
	Symbol string
	Trend  string
}

// getRunningPaperDirectionalBots queries active paper bots.
func (worker *Worker) getRunningPaperDirectionalBots(ctx context.Context, settingsID string) []runningDirectionalBot {
	if worker == nil || worker.db == nil {
		return nil
	}
	rows, err := worker.db.Query(ctx, `
		SELECT symbol, trend FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settingsID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var bots []runningDirectionalBot
	for rows.Next() {
		var b runningDirectionalBot
		if err := rows.Scan(&b.Symbol, &b.Trend); err == nil {
			bots = append(bots, b)
		}
	}
	return bots
}

// getRunningRealDirectionalBots queries active real bots.
func (worker *Worker) getRunningRealDirectionalBots(ctx context.Context, accountID string) []runningDirectionalBot {
	if worker == nil || worker.db == nil {
		return nil
	}
	rows, err := worker.db.Query(ctx, `
		SELECT symbol, trend FROM grid_bots
		WHERE account_id = $1
		  AND status IN ('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var bots []runningDirectionalBot
	for rows.Next() {
		var b runningDirectionalBot
		if err := rows.Scan(&b.Symbol, &b.Trend); err == nil {
			bots = append(bots, b)
		}
	}
	return bots
}

// checkPortfolioCorrelation evaluates whether the prospective candidate symbol
// is overly correlated with existing active bots sharing the same directional trend.
func (worker *Worker) checkPortfolioCorrelation(
	ctx context.Context,
	candidateSymbol string,
	candidateTrend string,
	activeBots []runningDirectionalBot,
) (blocked bool, reason string) {
	normTrend := strings.ToLower(strings.TrimSpace(candidateTrend))
	if normTrend != "long" && normTrend != "short" {
		// Neutral range grids oscillate and do not add net directional cluster risk
		return false, ""
	}
	if len(activeBots) == 0 || worker.publicClient == nil {
		return false, ""
	}

	// Filter active bots to those with the same directional exposure
	sameDirSymbols := make([]string, 0, len(activeBots))
	for _, bot := range activeBots {
		if strings.EqualFold(strings.TrimSpace(bot.Trend), normTrend) &&
			!strings.EqualFold(strings.TrimSpace(bot.Symbol), candidateSymbol) {
			sameDirSymbols = append(sameDirSymbols, bot.Symbol)
		}
	}
	if len(sameDirSymbols) == 0 {
		return false, ""
	}

	// Fetch candidate klines
	candCandles, err := worker.publicClient.GetKlines(ctx, candidateSymbol, "15M", CorrelationLookbackCandles+4)
	if err != nil || len(candCandles) < CorrelationLookbackCandles {
		return false, "" // fail-open on candle fetch failure
	}
	candReturns := extractReturnsFromCandles(candCandles)
	if len(candReturns) < 15 {
		return false, ""
	}

	correlatedCount := 0
	var firstCorrelatedSymbol string
	var firstCorrelation float64

	for _, activeSym := range sameDirSymbols {
		activeCandles, err := worker.publicClient.GetKlines(ctx, activeSym, "15M", CorrelationLookbackCandles+4)
		if err != nil || len(activeCandles) < CorrelationLookbackCandles {
			continue
		}
		activeReturns := extractReturnsFromCandles(activeCandles)
		if len(activeReturns) < 15 {
			continue
		}

		// Align lengths
		minLen := len(candReturns)
		if len(activeReturns) < minLen {
			minLen = len(activeReturns)
		}
		subCand := candReturns[len(candReturns)-minLen:]
		subActive := activeReturns[len(activeReturns)-minLen:]

		rho := PearsonCorrelation(subCand, subActive)
		if rho >= HighCorrelationThreshold {
			correlatedCount++
			if firstCorrelatedSymbol == "" {
				firstCorrelatedSymbol = activeSym
				firstCorrelation = rho
			}
		}

		if correlatedCount >= MaxCorrelatedBotsSameDirection {
			return true, fmt.Sprintf(
				"portfolio correlation cap: пара %s коррелирует (ρ=%.2f с %s) с %d активными %s-ботами — превышен лимит концентрации",
				candidateSymbol, firstCorrelation, firstCorrelatedSymbol, correlatedCount, strings.ToUpper(normTrend),
			)
		}
	}

	return false, ""
}
