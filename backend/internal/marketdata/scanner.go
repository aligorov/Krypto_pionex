package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

type ScannerCandidate struct {
	Symbol              string
	BaseCurrency        string
	QuoteCurrency       string
	Price               decimal.Decimal
	VolatilityPct       float64
	Volume24h           decimal.Decimal
	FundingRate         *decimal.Decimal
	ExpectedValuePct    float64
	Sharpe              float64
	Sortino             float64
	MaxDrawdownPct      float64
	WinRatePct          float64
	ProfitFactor        float64
	TurnoverProxy       float64
	Score               float64
	Decision            string
	RejectionReason     string
	LowerPrice          decimal.Decimal
	UpperPrice          decimal.Decimal
	GridNum             int
	RecommendedLeverage int
	RecommendedTrend    string
	ModelAssumptions    map[string]any
}

type ScanConfig struct {
	Interval            string
	LookbackCandles     int
	MaxSymbols          int
	MinVolume24h        decimal.Decimal
	MinVolatilityPct    float64
	MaxVolatilityPct    float64
	MinExpectedValuePct float64
	MinSharpe           float64
	MaxDrawdownPct      float64
	MinProfitFactor     float64
	FeeBps              float64
	SlippageBps         float64
	BaseLeverage        int
	AdaptiveLeverage    bool
	GridType            string
}

type MarketClient interface {
	GetMarketSymbols(context.Context, string) ([]pionex.SymbolInfo, error)
	GetTickers(context.Context, string, string) ([]pionex.TickerInfo, error)
	GetKlines(context.Context, string, string, int) ([]pionex.KlineCandle, error)
}

type Scanner struct {
	client MarketClient
}

func NewScanner(client MarketClient) *Scanner {
	return &Scanner{client: client}
}

// ScanMarkets evaluates only symbols returned by the official Pionex PERP
// symbol endpoint. The EV and risk values are model estimates, never exchange PnL.
func (s *Scanner) ScanMarkets(
	ctx context.Context,
	config ScanConfig,
) ([]ScannerCandidate, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	symbols, err := s.client.GetMarketSymbols(ctx, "PERP")
	if err != nil {
		return nil, fmt.Errorf("fetch Pionex PERP symbols: %w", err)
	}
	tickers, err := s.client.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, fmt.Errorf("fetch Pionex PERP tickers: %w", err)
	}
	tickerBySymbol := make(map[string]pionex.TickerInfo, len(tickers))
	for _, ticker := range tickers {
		tickerBySymbol[ticker.Symbol] = ticker
	}

	type rankedSymbol struct {
		symbol pionex.SymbolInfo
		ticker pionex.TickerInfo
		amount decimal.Decimal
	}
	ranked := make([]rankedSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol.Type != "PERP" || symbol.QuoteCurrency != "USDT" || !symbol.IsTrading() {
			continue
		}
		ticker, ok := tickerBySymbol[symbol.Symbol]
		if !ok || ticker.Close.LessThanOrEqual(decimal.Zero) {
			continue
		}
		amount := ticker.Amount
		if amount.LessThanOrEqual(decimal.Zero) {
			amount = ticker.Volume.Mul(ticker.Close)
		}
		ranked = append(ranked, rankedSymbol{symbol: symbol, ticker: ticker, amount: amount})
	}
	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].amount.GreaterThan(ranked[j].amount)
	})
	if len(ranked) > config.MaxSymbols {
		ranked = ranked[:config.MaxSymbols]
	}

	candidates := make([]ScannerCandidate, 0, len(ranked))
	for _, item := range ranked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candles, candleErr := s.client.GetKlines(
			ctx, item.symbol.Symbol, config.Interval, config.LookbackCandles,
		)
		if candleErr != nil {
			candidates = append(candidates, rejectedDataCandidate(item, candleErr))
			continue
		}
		candidate, metricErr := scoreCandidate(item.symbol, item.ticker, item.amount, candles, config)
		if metricErr != nil {
			candidates = append(candidates, rejectedDataCandidate(item, metricErr))
			continue
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Decision != candidates[j].Decision {
			return candidates[i].Decision == "ACCEPTED"
		}
		return candidates[i].Score > candidates[j].Score
	})
	return candidates, nil
}

func scoreCandidate(
	symbol pionex.SymbolInfo,
	ticker pionex.TickerInfo,
	volume decimal.Decimal,
	candles []pionex.KlineCandle,
	config ScanConfig,
) (ScannerCandidate, error) {
	if len(candles) < 30 {
		return ScannerCandidate{}, errors.New("fewer than 30 valid candles")
	}
	sorted := append([]pionex.KlineCandle(nil), candles...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })
	returns := make([]float64, 0, len(sorted)-1)
	for index := 1; index < len(sorted); index++ {
		previous, _ := sorted[index-1].Close.Float64()
		current, _ := sorted[index].Close.Float64()
		if previous <= 0 || current <= 0 {
			continue
		}
		returns = append(returns, current/previous-1)
	}
	if len(returns) < 29 {
		return ScannerCandidate{}, errors.New("fewer than 29 valid returns")
	}

	periodsPerDay := intervalPeriodsPerDay(config.Interval)
	rawStd := sampleStdDev(returns)
	volatilityPct := rawStd * math.Sqrt(periodsPerDay) * 100
	rangePct := clamp(math.Max(volatilityPct*2.5, 2.0), 2.0, 25.0)
	gridNum := 20
	gridStep := rangePct / 100 / float64(gridNum)
	friction := 2 * (config.FeeBps + config.SlippageBps) / 10_000
	modelReturns := make([]float64, len(returns))
	crossings := 0
	trendPenalty := math.Abs(mean(returns)) * 0.5
	for index, value := range returns {
		capture := math.Min(math.Abs(value), gridStep) * 0.35
		cost := 0.0
		if math.Abs(value) >= gridStep*0.5 {
			cost = friction
			crossings++
		}
		modelReturns[index] = capture - cost - trendPenalty
	}

	evPct := mean(modelReturns) * 100
	sharpe := ratio(modelReturns, false, periodsPerDay*365)
	sortino := ratio(modelReturns, true, periodsPerDay*365)
	maxDrawdown := maxDrawdown(modelReturns) * 100
	winRate, profitFactor := winRateAndProfitFactor(modelReturns)
	turnover := float64(crossings) * 2 / float64(len(modelReturns))

	price := ticker.Close
	rangeFraction := decimal.NewFromFloat(rangePct / 200)
	lower := price.Mul(decimal.NewFromInt(1).Sub(rangeFraction))
	upper := price.Mul(decimal.NewFromInt(1).Add(rangeFraction))
	leverage := config.BaseLeverage
	if config.AdaptiveLeverage {
		volatilityCap := int(math.Floor(12 / math.Max(volatilityPct, 1)))
		if volatilityCap < 1 {
			volatilityCap = 1
		}
		if leverage > volatilityCap {
			leverage = volatilityCap
		}
	}
	recommendedTrend := "no_trend"
	rawMean := mean(returns)
	if rawMean > rawStd*0.10 {
		recommendedTrend = "long"
	} else if rawMean < -rawStd*0.10 {
		recommendedTrend = "short"
	}

	reasons := make([]string, 0)
	if volume.LessThan(config.MinVolume24h) {
		reasons = append(reasons, "24h quote turnover below limit")
	}
	if volatilityPct < config.MinVolatilityPct {
		reasons = append(reasons, "volatility below grid threshold")
	}
	if volatilityPct > config.MaxVolatilityPct {
		reasons = append(reasons, "volatility above risk threshold")
	}
	if evPct < config.MinExpectedValuePct {
		reasons = append(reasons, "model EV below limit")
	}
	if sharpe < config.MinSharpe {
		reasons = append(reasons, "model Sharpe below limit")
	}
	if maxDrawdown > config.MaxDrawdownPct {
		reasons = append(reasons, "model max drawdown above limit")
	}
	if profitFactor < config.MinProfitFactor {
		reasons = append(reasons, "model profit factor below limit")
	}
	decision := "ACCEPTED"
	if len(reasons) > 0 {
		decision = "REJECTED"
	}
	score := scannerScore(
		volatilityPct, evPct, sharpe, maxDrawdown, profitFactor,
		config,
	)
	return ScannerCandidate{
		Symbol: symbol.Symbol, BaseCurrency: symbol.BaseCurrency,
		QuoteCurrency: symbol.QuoteCurrency, Price: price,
		VolatilityPct: volatilityPct, Volume24h: volume,
		ExpectedValuePct: evPct, Sharpe: sharpe, Sortino: sortino,
		MaxDrawdownPct: maxDrawdown, WinRatePct: winRate,
		ProfitFactor: profitFactor, TurnoverProxy: turnover, Score: score,
		Decision: decision, RejectionReason: strings.Join(reasons, "; "),
		LowerPrice: lower, UpperPrice: upper, GridNum: gridNum,
		RecommendedLeverage: leverage, RecommendedTrend: recommendedTrend,
		ModelAssumptions: map[string]any{
			"model":              "neutral_grid_capture_proxy_v1",
			"interval":           config.Interval,
			"lookbackCandles":    len(sorted),
			"feeBpsPerFill":      config.FeeBps,
			"slippageBpsPerFill": config.SlippageBps,
			"captureEfficiency":  0.35,
			"recommendedTrend":   recommendedTrend,
			"fundingIncluded":    false,
			"warning":            "backtest proxy is not live trading performance",
		},
	}, nil
}

func rejectedDataCandidate(item struct {
	symbol pionex.SymbolInfo
	ticker pionex.TickerInfo
	amount decimal.Decimal
}, err error) ScannerCandidate {
	return ScannerCandidate{
		Symbol: item.symbol.Symbol, BaseCurrency: item.symbol.BaseCurrency,
		QuoteCurrency: item.symbol.QuoteCurrency, Price: item.ticker.Close,
		Volume24h: item.amount, Decision: "REJECTED",
		RejectionReason:     "market data unavailable: " + err.Error(),
		RecommendedLeverage: 1, RecommendedTrend: "no_trend",
		ModelAssumptions: map[string]any{"fundingIncluded": false},
	}
}

func validateConfig(config ScanConfig) error {
	switch config.Interval {
	case "1M", "5M", "15M", "30M", "60M", "4H", "8H", "12H", "1D":
	default:
		return errors.New("unsupported Pionex candle interval")
	}
	if config.LookbackCandles < 30 || config.LookbackCandles > 500 {
		return errors.New("lookback candles must be between 30 and 500")
	}
	if config.MaxSymbols < 1 || config.MaxSymbols > 50 {
		return errors.New("max symbols per scan must be between 1 and 50")
	}
	if config.BaseLeverage < 1 || config.BaseLeverage > 100 {
		return errors.New("base leverage must be between 1 and 100")
	}
	if config.FeeBps < 0 || config.SlippageBps < 0 {
		return errors.New("fee and slippage assumptions cannot be negative")
	}
	return nil
}

func intervalPeriodsPerDay(interval string) float64 {
	switch interval {
	case "1M":
		return 1440
	case "5M":
		return 288
	case "15M":
		return 96
	case "30M":
		return 48
	case "60M":
		return 24
	case "4H":
		return 6
	case "8H":
		return 3
	case "12H":
		return 2
	default:
		return 1
	}
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func sampleStdDev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	average := mean(values)
	sum := 0.0
	for _, value := range values {
		difference := value - average
		sum += difference * difference
	}
	return math.Sqrt(sum / float64(len(values)-1))
}

func ratio(values []float64, downsideOnly bool, annualPeriods float64) float64 {
	average := mean(values)
	sample := values
	if downsideOnly {
		sample = make([]float64, 0, len(values))
		for _, value := range values {
			if value < 0 {
				sample = append(sample, value)
			}
		}
	}
	deviation := sampleStdDev(sample)
	if deviation == 0 {
		if average > 0 {
			return 99
		}
		return 0
	}
	return clamp(average/deviation*math.Sqrt(annualPeriods), -99, 99)
}

func maxDrawdown(returns []float64) float64 {
	equity, peak, drawdown := 1.0, 1.0, 0.0
	for _, value := range returns {
		equity *= 1 + value
		if equity > peak {
			peak = equity
		}
		if peak > 0 {
			current := (peak - equity) / peak
			if current > drawdown {
				drawdown = current
			}
		}
	}
	return drawdown
}

func winRateAndProfitFactor(values []float64) (float64, float64) {
	wins, positive, negative := 0, 0.0, 0.0
	for _, value := range values {
		if value > 0 {
			wins++
			positive += value
		} else if value < 0 {
			negative += -value
		}
	}
	winRate := float64(wins) / float64(len(values)) * 100
	if negative == 0 {
		if positive > 0 {
			return winRate, 99
		}
		return winRate, 0
	}
	return winRate, positive / negative
}

func scannerScore(
	volatility, ev, sharpe, drawdown, profitFactor float64,
	config ScanConfig,
) float64 {
	volatilityFit := 1 - math.Abs(
		volatility-(config.MinVolatilityPct+config.MaxVolatilityPct)/2,
	)/math.Max(config.MaxVolatilityPct-config.MinVolatilityPct, 1)
	score := clamp(volatilityFit, 0, 1)*0.15 +
		clamp(ev/math.Max(config.MinExpectedValuePct+0.25, 0.25), 0, 2)*0.25 +
		clamp(sharpe/math.Max(config.MinSharpe, 0.25), 0, 2)*0.20 +
		clamp(1-drawdown/math.Max(config.MaxDrawdownPct, 1), 0, 1)*0.20 +
		clamp(profitFactor/math.Max(config.MinProfitFactor, 1), 0, 2)*0.20
	return clamp(score/1.4, 0, 1)
}

func clamp(value, minimum, maximum float64) float64 {
	return math.Max(minimum, math.Min(maximum, value))
}
