package marketdata

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

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
	Regime              string  `json:"regime"`
	ADXPct              float64 `json:"adxPct"`
	RangePositionPct    float64 `json:"rangePositionPct"`
	ATRPct              float64 `json:"atrPct"`
	Choppiness          float64 `json:"choppiness"`
	IsSqueeze           bool    `json:"isSqueeze"`
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

type rankedSymbol struct {
	symbol pionex.SymbolInfo
	ticker pionex.TickerInfo
	amount decimal.Decimal
}

// ScanMarkets evaluates symbols returned by the official Pionex PERP symbol endpoint.
// It uses a 2-tier pipeline: L1 fast filtering across all market tickers, followed by
// L2 concurrent candle fetching and deep quant analysis.
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

	// L1 Fast Screener: Filter all trading PERP pairs
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

		// Filter out pairs with extreme anomalous 24h pump/dumps (> 50% change)
		if ticker.Open.GreaterThan(decimal.Zero) {
			changeRatio, _ := ticker.Close.Sub(ticker.Open).Div(ticker.Open).Abs().Float64()
			if changeRatio > 0.50 {
				continue
			}
		}

		ranked = append(ranked, rankedSymbol{symbol: symbol, ticker: ticker, amount: amount})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].amount.GreaterThan(ranked[j].amount)
	})

	// Expand pool limit beyond MaxSymbols to discover quality pairs from top 60-80
	poolLimit := config.MaxSymbols * 3
	if poolLimit < 60 {
		poolLimit = 60
	}
	if len(ranked) > poolLimit {
		ranked = ranked[:poolLimit]
	}

	// L2 Concurrent Worker Pool for Deep Kline Analysis
	type scanJob struct {
		item rankedSymbol
	}
	type scanResult struct {
		candidate ScannerCandidate
	}

	workerCount := 8
	if len(ranked) < workerCount {
		workerCount = len(ranked)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	jobs := make(chan scanJob, len(ranked))
	results := make(chan scanResult, len(ranked))
	var wg sync.WaitGroup

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				candles, candleErr := s.client.GetKlines(
					ctx, job.item.symbol.Symbol, config.Interval, config.LookbackCandles,
				)
				if candleErr != nil {
					results <- scanResult{candidate: rejectedDataCandidate(job.item, candleErr)}
					continue
				}
				candidate, metricErr := scoreCandidate(job.item.symbol, job.item.ticker, job.item.amount, candles, config)
				if metricErr != nil {
					results <- scanResult{candidate: rejectedDataCandidate(job.item, metricErr)}
					continue
				}
				results <- scanResult{candidate: candidate}
			}
		}()
	}

	for _, item := range ranked {
		jobs <- scanJob{item: item}
	}
	close(jobs)

	wg.Wait()
	close(results)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	candidates := make([]ScannerCandidate, 0, len(ranked))
	for res := range results {
		candidates = append(candidates, res.candidate)
	}

	// Sort candidates: ACCEPTED first, then by Score descending
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Decision != candidates[j].Decision {
			return candidates[i].Decision == "ACCEPTED"
		}
		return candidates[i].Score > candidates[j].Score
	})

	// Truncate to MaxSymbols if needed
	if len(candidates) > config.MaxSymbols {
		candidates = candidates[:config.MaxSymbols]
	}

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
	volClose := rawStd * math.Sqrt(periodsPerDay) * 100
	volParkinson := parkinsonVolatility(sorted, periodsPerDay)

	// Blend Close-to-Close with Parkinson intra-candle volatility
	volatilityPct := volClose
	if volParkinson > 0 {
		volatilityPct = 0.5*volClose + 0.5*volParkinson
	}

	regime := DetectRegime(sorted)
	rangePct := clamp(math.Max(volatilityPct*2.5, 2.0), 2.0, 25.0)

	// Grid Level count derived from pair's ATR
	gridNum := GridLevelsForRange(rangePct, regime.ATRPct)
	gridStep := rangePct / 100 / float64(gridNum)
	friction := 2 * (config.FeeBps + config.SlippageBps) / 10_000

	modelReturns := make([]float64, len(returns))
	crossings := 0
	trendPenalty := math.Abs(mean(returns)) * 0.5
	netStep := gridStep - friction

	// Enhanced crossing model: uses both Close returns and Intra-candle High/Low travel
	for index := 0; index < len(returns); index++ {
		currIdx := index + 1
		high, _ := sorted[currIdx].High.Float64()
		low, _ := sorted[currIdx].Low.Float64()
		currClose, _ := sorted[currIdx].Close.Float64()

		closeReturn := math.Abs(returns[index])
		hlSpread := 0.0
		if currClose > 0 && high >= low {
			hlSpread = (high - low) / currClose
		}
		effectiveTravel := math.Max(closeReturn, hlSpread*0.65)
		candleCrossings := clamp(effectiveTravel/gridStep, 0, 6)

		crossings += int(candleCrossings)
		gain := 0.0
		if netStep > 0 {
			gain = candleCrossings * netStep / 2
		}
		modelReturns[index] = gain - trendPenalty
	}

	evPct := mean(modelReturns) * 100
	sharpe := ratio(modelReturns, false, periodsPerDay*365)
	sortino := ratio(modelReturns, true, periodsPerDay*365)
	maxDrawdown := maxDrawdown(modelReturns) * 100
	winRate, profitFactor := winRateAndProfitFactor(modelReturns)
	turnover := float64(crossings) * 2 / float64(len(modelReturns))

	price := ticker.Close
	priceFloat, _ := price.Float64()
	lowerFloat, upperFloat := supportResistanceRange(sorted, priceFloat, volatilityPct)
	rangeFraction := 0.0
	if upperFloat > lowerFloat && upperFloat > 0 {
		rangeFraction = (upperFloat - lowerFloat) / upperFloat / 2
	}
	lower := decimal.NewFromFloat(lowerFloat)
	upper := decimal.NewFromFloat(upperFloat)

	leverage := config.BaseLeverage
	if config.AdaptiveLeverage {
		minLev := 1
		if config.BaseLeverage >= 2 {
			minLev = 2
		}
		volatilityCap := int(math.Round(24.0 / math.Max(volatilityPct, 1.0)))
		if volatilityCap < minLev {
			volatilityCap = minLev
		}
		if leverage > volatilityCap {
			leverage = volatilityCap
		}
		if leverage < minLev {
			leverage = minLev
		}
	} else if leverage < 2 && config.BaseLeverage >= 2 {
		leverage = 2
	}

	recommendedTrend := regime.RecommendedTrend()
	if rangeFraction < 0.005 {
		recommendedTrend = "no_trend"
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
	if !ValidateMinGridStep(gridStep*100, config.FeeBps, config.SlippageBps) {
		reasons = append(reasons, "grid step too narrow for trading fees")
	}
	if regime.IsSqueeze {
		reasons = append(reasons, "volatility squeeze: impending explosive breakout")
	}
	if recommendedTrend == "no_trend" && (regime.ADX > 32.0 || math.Abs(regime.EMASlopePct) > 3.0) {
		reasons = append(reasons, fmt.Sprintf("trend too strong for neutral grid (ADX: %.1f, EMA slope: %.2f%%)", regime.ADX, regime.EMASlopePct))
	}
	if len(sorted) >= 3 {
		for i := len(sorted) - 3; i < len(sorted); i++ {
			cOpen, _ := sorted[i].Open.Float64()
			cClose, _ := sorted[i].Close.Float64()
			if cOpen > 0 {
				pctChange := math.Abs(cClose-cOpen) / cOpen * 100
				if pctChange > 4.5 {
					reasons = append(reasons, fmt.Sprintf("recent flash candle spike (%.1f%%) - waiting for stabilization", pctChange))
					break
				}
			}
		}
	}

	decision := "ACCEPTED"
	if len(reasons) > 0 {
		decision = "REJECTED"
	}

	score := scannerScore(
		volatilityPct, evPct, sharpe, maxDrawdown, profitFactor,
		regime.Choppiness, regime.IsSqueeze, config,
	)

	// Entry Fit: directional grids require entry on favorable side of structure,
	// neutral grids prefer entries near the midpoint.
	entryFit := 1.0
	switch recommendedTrend {
	case "long":
		entryFit = clamp((70-regime.RangePositionPct)/45, 0, 1)
	case "short":
		entryFit = clamp((regime.RangePositionPct-30)/45, 0, 1)
	default:
		entryFit = clamp(1.0-math.Abs(regime.RangePositionPct-50)/50, 0, 1)
	}
	score = clamp(score*(0.7+0.3*entryFit), 0, 1)

	return ScannerCandidate{
		Symbol: symbol.Symbol, BaseCurrency: symbol.BaseCurrency,
		QuoteCurrency: symbol.QuoteCurrency, Price: price,
		VolatilityPct: volatilityPct, Volume24h: volume,
		ExpectedValuePct: evPct, Sharpe: sharpe, Sortino: sortino,
		MaxDrawdownPct: maxDrawdown, WinRatePct: winRate,
		ProfitFactor: profitFactor, TurnoverProxy: turnover, Score: score,
		Regime: regime.Regime, ADXPct: regime.ADX,
		RangePositionPct: regime.RangePositionPct, ATRPct: regime.ATRPct,
		Choppiness: regime.Choppiness, IsSqueeze: regime.IsSqueeze,
		Decision: decision, RejectionReason: strings.Join(reasons, "; "),
		LowerPrice: lower, UpperPrice: upper, GridNum: gridNum,
		RecommendedLeverage: leverage, RecommendedTrend: recommendedTrend,
		ModelAssumptions: map[string]any{
			"model":              "neutral_grid_capture_proxy_v3_multitier",
			"interval":           config.Interval,
			"lookbackCandles":    len(sorted),
			"feeBpsPerFill":      config.FeeBps,
			"slippageBpsPerFill": config.SlippageBps,
			"captureEfficiency":  0.40,
			"recommendedTrend":   recommendedTrend,
			"regime":             regime.Regime,
			"adx":                regime.ADX,
			"choppiness":         regime.Choppiness,
			"isSqueeze":          regime.IsSqueeze,
			"emaSlopePct":        regime.EMASlopePct,
			"rangePositionPct":   regime.RangePositionPct,
			"atrPct":             regime.ATRPct,
			"volatilityParkinson": volParkinson,
			"rangeSource":        "support_resistance_atr_buffered",
			"fundingIncluded":    false,
			"warning":            "backtest proxy is not live trading performance",
		},
	}, nil
}

func rejectedDataCandidate(item rankedSymbol, err error) ScannerCandidate {
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
	case "1M", "5M", "15M", "30M", "60M", "1H", "4H", "8H", "12H", "1D":
	default:
		return errors.New("unsupported Pionex candle interval")
	}
	if config.LookbackCandles < 30 || config.LookbackCandles > 500 {
		return errors.New("lookback candles must be between 30 and 500")
	}
	if config.MaxSymbols < 1 || config.MaxSymbols > 100 {
		return errors.New("max symbols per scan must be between 1 and 100")
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
	case "60M", "1H":
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

func parkinsonVolatility(candles []pionex.KlineCandle, periodsPerDay float64) float64 {
	if len(candles) < 2 {
		return 0
	}
	sumHL := 0.0
	counted := 0
	for _, candle := range candles {
		high, _ := candle.High.Float64()
		low, _ := candle.Low.Float64()
		if high > 0 && low > 0 && high >= low {
			hlRatio := high / low
			if hlRatio > 1 {
				ln := math.Log(hlRatio)
				sumHL += ln * ln
				counted++
			}
		}
	}
	if counted == 0 {
		return 0
	}
	dailyVariance := sumHL / (4.0 * math.Ln2 * float64(counted))
	return math.Sqrt(dailyVariance*periodsPerDay) * 100.0
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
	volatility, ev, sharpe, drawdown, profitFactor, choppiness float64,
	isSqueeze bool,
	config ScanConfig,
) float64 {
	volatilityFit := 1 - math.Abs(
		volatility-(config.MinVolatilityPct+config.MaxVolatilityPct)/2,
	)/math.Max(config.MaxVolatilityPct-config.MinVolatilityPct, 1)

	chopFit := clamp((choppiness-40.0)/30.0, 0, 1)
	squeezePenalty := 1.0
	if isSqueeze {
		squeezePenalty = 0.8
	}

	score := clamp(volatilityFit, 0, 1)*0.15 +
		clamp(ev/math.Max(config.MinExpectedValuePct+0.25, 0.25), 0, 2)*0.20 +
		clamp(sharpe/math.Max(config.MinSharpe, 0.25), 0, 2)*0.20 +
		clamp(1-drawdown/math.Max(config.MaxDrawdownPct, 1), 0, 1)*0.15 +
		clamp(profitFactor/math.Max(config.MinProfitFactor, 1), 0, 2)*0.15 +
		chopFit*0.15

	return clamp((score/1.4)*squeezePenalty, 0, 1)
}

func clamp(value, minimum, maximum float64) float64 {
	return math.Max(minimum, math.Min(maximum, value))
}

