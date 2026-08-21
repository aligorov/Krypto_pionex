package marketdata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
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
	BBWPercentile       float64 `json:"bbwPercentile"`
	Hurst               float64 `json:"hurst"`
	ConfluenceVerdict   string  `json:"confluenceVerdict"`
	ConfluenceStrength  float64 `json:"confluenceStrength"`
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
	ScanMode            string // TOP_K (fast, default) or FULL (all pairs)
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
	// CascadeShortMode (v2.0.21) marks an out-of-turn scan queued during a
	// long-liquidation cascade: short-side anti-FOMO floors are lifted for
	// that pass — by definition every symbol is oversold at the channel
	// bottom during forced unwinding, and the floors would reject exactly
	// the continuation entries this scan exists to deploy. All other
	// vetoes (volatility caps, LONG floors, Hurst, backtest) stay armed.
	CascadeShortMode bool
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

	// L1→top-K pipeline: the ticker-only prefilter above has already ranked
	// every PERP by 24h turnover and dropped pump/dump anomalies. Fetching
	// klines for all ~400 pairs at 10 req/s took 6-10 minutes; taking only
	// the top-K by L1 ranking (3× the MaxSymbols the operator wants to keep)
	// cuts the scan to ~1 minute while preserving the candidates that matter
	// — illiquid tail symbols were rejected downstream anyway.
	scanCap := 10000 // FULL mode: effectively no cap
	if config.ScanMode != "FULL" {
		scanCap = config.MaxSymbols * 3
	}
	if scanCap < 30 {
		scanCap = 30
	}
	if scanCap > 150 {
		scanCap = 150
	}
	activeRanked := ranked
	if len(activeRanked) > scanCap {
		activeRanked = ranked[:scanCap]
	}

	// L2 Concurrent Worker Pool for Deep Kline Analysis across all Pionex pairs
	type scanJob struct {
		item rankedSymbol
	}
	type scanResult struct {
		candidate ScannerCandidate
	}

	workerCount := 24
	if len(activeRanked) < workerCount {
		workerCount = len(activeRanked)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	jobs := make(chan scanJob, len(activeRanked))
	results := make(chan scanResult, len(activeRanked))
	var wg sync.WaitGroup

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				// A quant bug in one symbol's candles must never kill the
				// backend: recover per job, report the symbol as rejected
				// and keep the worker pool alive.
				func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("L2 scan worker panic recovered",
								"symbol", job.item.symbol.Symbol,
								"panic", r, "stack", string(debug.Stack()))
							results <- scanResult{candidate: rejectedDataCandidate(
								job.item, fmt.Errorf("internal scanner panic: %v", r),
							)}
						}
					}()
					candles, candleErr := s.client.GetKlines(
						ctx, job.item.symbol.Symbol, config.Interval, config.LookbackCandles,
					)
					if candleErr != nil {
						results <- scanResult{candidate: rejectedDataCandidate(job.item, candleErr)}
						return
					}
					candidate, metricErr := scoreCandidate(job.item.symbol, job.item.ticker, job.item.amount, candles, config)
					if metricErr != nil {
						results <- scanResult{candidate: rejectedDataCandidate(job.item, metricErr)}
						return
					}
					results <- scanResult{candidate: candidate}
				}()
			}
		}()
	}

	for _, item := range activeRanked {
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
	pricePrec := symbol.GetPricePrecision()
	lower := decimal.NewFromFloat(lowerFloat).Round(int32(pricePrec))
	upper := decimal.NewFromFloat(upperFloat).Round(int32(pricePrec))

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

	var change24hPct float64
	if ticker.Open.GreaterThan(decimal.Zero) {
		change24hPct, _ = ticker.Close.Sub(ticker.Open).Div(ticker.Open).Mul(decimal.NewFromInt(100)).Float64()
	}
	var change6hPct float64
	if sixHCandles := int(periodsPerDay / 4); sixHCandles > 0 && len(sorted) > sixHCandles+1 {
		startClose, _ := sorted[len(sorted)-1-sixHCandles].Close.Float64()
		endClose, _ := sorted[len(sorted)-1].Close.Float64()
		if startClose > 0 && endClose > 0 {
			change6hPct = (endClose - startClose) / startClose * 100
		}
	}

	recommendedTrend := regime.RecommendedTrend()
	// Counter-trend guards, symmetric on both sides: a strongly trending
	// 24h tape must not be fought with a directional grid in the opposite
	// direction. The previous asymmetry (+3% blocked shorts while longs were
	// allowed down to -6%) systematically longed into obvious downtrends.
	if change24hPct >= 3.0 && recommendedTrend == "short" {
		recommendedTrend = "no_trend"
	} else if change24hPct <= -3.0 && recommendedTrend == "long" {
		recommendedTrend = "no_trend"
	}
	if rangeFraction < 0.005 {
		recommendedTrend = "no_trend"
	}

	reasons := make([]string, 0)
	if volume.LessThan(config.MinVolume24h) {
		reasons = append(reasons, "24h quote turnover below limit")
	}
	// v2.0.26 majors priority, scoped by v2.0.27 to the momentum thesis the
	// feature exists for: in quiet RANGE majors face the same operator
	// floors as everyone (the waivers used to lower the floors
	// unconditionally — a risk-floor reduction, not a priority).
	majorMomentum := isMajorSymbol(symbol.BaseCurrency, symbol.Symbol) &&
		(recommendedTrend == "long" || regime.Regime == "TREND_UP")
	minVol := config.MinVolatilityPct
	if majorMomentum && minVol > 0.4 {
		minVol = 0.4
	}
	if volatilityPct < minVol {
		reasons = append(reasons, "volatility below grid threshold")
	}
	if volatilityPct > config.MaxVolatilityPct {
		reasons = append(reasons, "volatility above risk threshold")
	}
	minEV := config.MinExpectedValuePct
	if majorMomentum && minEV > 0.0 {
		minEV = 0.0
	}
	if evPct < minEV {
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
	if regime.IsSqueeze && recommendedTrend == "no_trend" {
		reasons = append(reasons, "volatility squeeze: impending explosive breakout")
	}
	if recommendedTrend == "no_trend" && (regime.ADX > 32.0 || math.Abs(regime.EMASlopePct) > 3.0 ||
		math.Abs(change24hPct) > 8.0 || math.Abs(change6hPct) > 4.0) {
		reasons = append(reasons, fmt.Sprintf(
			"trend too strong for neutral grid (ADX: %.1f, EMA slope: %.2f%%, 24h: %+.1f%%, 6h: %+.1f%%)",
			regime.ADX, regime.EMASlopePct, change24hPct, change6hPct))
	}

	// Anti-FOMO Overbought / Oversold protection. v2.0.14: the LONG/SHORT
	// bands widen in a genuine trend — "trending" and "overextended" are
	// different states; the ADX gate separates them. NEUTRAL bands stay
	// static. v2.0.19: threshold 25 (was 30) — DetectRegime calls a trend
	// at ADX ≥ 22, and the 22–30 dead zone left directionals unreachable
	// for the entire duration of every moderate trend (prod: PRL/SNXXX
	// SHORT rejections at RSI 39/pos 43 in confirmed TREND_DOWN).
	strongTrend := regime.ADX > 22.0 || math.Abs(regime.EMASlopePct) > 0.5
	if recommendedTrend == "long" {
		rsiCap, posCap := 70.0, 75.0
		if strongTrend {
			rsiCap, posCap = 78.0, 88.0
		}
		if regime.RSI > rsiCap {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: RSI (%.1f) > %.0f - пара перекуплена на экстремуме, вход в LONG заблокирован", regime.RSI, rsiCap))
		}
		if regime.RangePositionPct > posCap {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: положение в канале (%.1f%%) > %.0f%% - вход в LONG выше предела заблокирован", regime.RangePositionPct, posCap))
		}
	} else if recommendedTrend == "short" {
		if config.CascadeShortMode {
			// v2.0.21 cascade window: skip the RSI/position floors for
			// shorts (see ScanConfig.CascadeShortMode) — the oversold
			// reading IS the signal during a forced unwind.
		} else {
			rsiFloor, posFloor := 30.0, 25.0
			if strongTrend {
				rsiFloor, posFloor = 22.0, 12.0
			}
			if regime.RSI < rsiFloor {
				reasons = append(reasons, fmt.Sprintf("Anti-FOMO: RSI (%.1f) < %.0f - пара перепродана на экстремуме, вход в SHORT заблокирован", regime.RSI, rsiFloor))
			}
			if regime.RangePositionPct < posFloor {
				reasons = append(reasons, fmt.Sprintf("Anti-FOMO: положение в канале (%.1f%%) < %.0f%% - вход в SHORT ниже предела заблокирован", regime.RangePositionPct, posFloor))
			}
		}
	} else {
		// NEUTRAL grid
		if regime.RSI > 60.0 {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: RSI (%.1f) > 60 - пара перекуплена на хаях, вход в нейтральную сетку заблокирован", regime.RSI))
		} else if regime.RSI < 40.0 {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: RSI (%.1f) < 40 - пара перепродана на лоях, вход в нейтральную сетку заблокирован", regime.RSI))
		}
		// v2.0.24: the NEUTRAL position band widens to [20,80] — aligned with
		// isEntryTimingFavorable's NEUTRAL gate, which reads the SAME
		// scan-time Donchian position. The scanner band was strictly tighter
		// (pure double-filter), and the extra width only admits "quiet
		// extremes" already cleared by the trend/Hurst/squeeze vetoes (5-agent
		// review 2026-08-20; unlocked HEMI/SOXSX/SOXLX/SKHY-class candidates,
		// break-up tail capped at −MaxLoss/2 by the v2.0.14 up-trend stop).
		// RSI stays [40,60]: the worker gate has NO RSI component, so widening
		// it loosens protection for zero empirical unlocks.
		if regime.RangePositionPct > 80.0 || regime.RangePositionPct < 20.0 {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: положение в канале (%.1f%%) на экстремуме - вход в нейтральную сетку заблокирован", regime.RangePositionPct))
		}
	}

	if len(sorted) >= 3 && !(config.CascadeShortMode && recommendedTrend == "short") {
		// v2.0.21: the flash-spike veto waits for "stabilization" — in a
		// liquidation cascade every short target spikes by definition, and
		// the whole point of the cascade scan is to enter DURING the
		// instability. Lifted for shorts in that window only.
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

	// --- Confluence engine (v1.2: soft multiplier + direction veto).
	// Independent information classes only — Hurst regime memory, OBV flow,
	// one IFT-RSI momentum voice, anchored-VWAP stretch, Keltner phase —
	// computed on the already-fetched L2 candles, zero extra API calls.
	series := ExtractSeries(sorted)
	bundle := ComputeIndicatorBundle(series)
	confluence := EvaluateConfluence(regime, bundle)
	switch {
	case recommendedTrend == "long" && confluence.Verdict == ConfluenceSupportShort:
		reasons = append(reasons, fmt.Sprintf(
			"confluence veto: flow supports SHORT (long %.2f vs short %.2f)",
			confluence.LongScore, confluence.ShortScore))
		recommendedTrend = "no_trend"
	case recommendedTrend == "short" && confluence.Verdict == ConfluenceSupportLong:
		reasons = append(reasons, fmt.Sprintf(
			"confluence veto: flow supports LONG (short %.2f vs long %.2f)",
			confluence.ShortScore, confluence.LongScore))
		recommendedTrend = "no_trend"
	}
	// Hard regime veto: a persistently trending memory (Hurst > 0.60)
	// loads one-sided inventory into a fresh neutral grid — the exact
	// failure the daily-loss breaker only sees after the damage.
	if recommendedTrend == "no_trend" && HurstHardVetoNeutral(bundle) {
		reasons = append(reasons, fmt.Sprintf(
			"confluence veto: Hurst %.2f > 0.60 — persistent trend regime, neutral grid would load one-sided inventory",
			bundle.Hurst))
	}

	decision := "ACCEPTED"
	if len(reasons) > 0 {
		decision = "REJECTED"
	}

	score := scannerScore(
		volatilityPct, evPct, sharpe, maxDrawdown, profitFactor,
		regime.Choppiness, regime.IsSqueeze, config,
	)

	// Entry Fit: directional grids require entry within viable channel structure,
	// neutral grids prefer entries near the midpoint.
	entryFit := 1.0
	switch recommendedTrend {
	case "long":
		if regime.RangePositionPct <= 50.0 {
			entryFit = clamp((regime.RangePositionPct-10)/40, 0.5, 1.0)
		} else {
			entryFit = clamp(1.0-(regime.RangePositionPct-50)/50, 0.5, 1.0)
		}
	case "short":
		if regime.RangePositionPct >= 50.0 {
			entryFit = clamp((90-regime.RangePositionPct)/40, 0.5, 1.0)
		} else {
			entryFit = clamp(1.0-(50-regime.RangePositionPct)/50, 0.5, 1.0)
		}
	default:
		entryFit = clamp(1.0-math.Abs(regime.RangePositionPct-50)/50, 0, 1)
	}
	score = clamp(score*(0.7+0.3*entryFit), 0, 1)

	// Squeeze-anchored neutral entries: bands tighter than most of the
	// window (low BBW percentile rank) are the documented best-practice
	// entry regime for range grids — reward, but never gate on it alone.
	if regime.Regime == "RANGE" {
		squeezeFit := clamp(1.0-regime.BBWPercentile/100.0, 0, 1)
		score = clamp(score*(0.9+0.1*squeezeFit), 0, 1)
	}

	// Confluence score shaping: aligned verdicts lift the candidate up to
	// +20%, a directional conflict cuts it by 15% (size down, never pick a
	// side automatically).
	switch {
	case confluence.Verdict == ConfluenceConflict:
		score = clamp(score*0.85, 0, 1)
	case (recommendedTrend == "no_trend" && confluence.Verdict == ConfluenceSupportRange) ||
		(recommendedTrend == "long" && confluence.Verdict == ConfluenceSupportLong) ||
		(recommendedTrend == "short" && confluence.Verdict == ConfluenceSupportShort):
		score = clamp(score*(0.8+0.2*confluence.Strength), 0, 1)
	}

	// Majors Priority Boost (v2.0.26): when a major is actually trending
	// long, lift its ranking. v2.0.27: multiplicative only and clamped —
	// the old max(score*1.4, 0.88) could exceed the 0..1 score contract,
	// and its 0.88 floor let three mediocre majors evict the entire honest
	// top-K through the MaxSymbols truncation.
	if majorMomentum {
		score = clamp(score*1.15, 0, 1)
	}

	return ScannerCandidate{
		Symbol: symbol.Symbol, BaseCurrency: symbol.BaseCurrency,
		QuoteCurrency: symbol.QuoteCurrency, Price: price,
		VolatilityPct: volatilityPct, Volume24h: volume,
		ExpectedValuePct: evPct, Sharpe: sharpe, Sortino: sortino,
		MaxDrawdownPct: maxDrawdown, WinRatePct: winRate,
		ProfitFactor: profitFactor, TurnoverProxy: turnover, Score: score,
		Regime: regime.Regime, ADXPct: regime.ADX,
		RangePositionPct: regime.RangePositionPct, ATRPct: regime.ATRPct,
		Choppiness: regime.Choppiness, BBWPercentile: regime.BBWPercentile,
		Hurst: bundle.Hurst, ConfluenceVerdict: confluence.Verdict,
		ConfluenceStrength: confluence.Strength,
		IsSqueeze:          regime.IsSqueeze,
		Decision:           decision, RejectionReason: strings.Join(reasons, "; "),
		LowerPrice: lower, UpperPrice: upper, GridNum: gridNum,
		RecommendedLeverage: leverage, RecommendedTrend: recommendedTrend,
		ModelAssumptions: map[string]any{
			"model":               "neutral_grid_capture_proxy_v3_multitier",
			"interval":            config.Interval,
			"lookbackCandles":     len(sorted),
			"feeBpsPerFill":       config.FeeBps,
			"slippageBpsPerFill":  config.SlippageBps,
			"captureEfficiency":   0.40,
			"recommendedTrend":    recommendedTrend,
			"regime":              regime.Regime,
			"adx":                 regime.ADX,
			"rsi":                 regime.RSI,
			"choppiness":          regime.Choppiness,
			"isSqueeze":           regime.IsSqueeze,
			"emaSlopePct":         regime.EMASlopePct,
			"rangePositionPct":    regime.RangePositionPct,
			"atrPct":              regime.ATRPct,
			"volatilityParkinson": volParkinson,
			"hurst":               bundle.Hurst,
			"confluence": map[string]any{
				"verdict":        confluence.Verdict,
				"strength":       confluence.Strength,
				"longScore":      confluence.LongScore,
				"shortScore":     confluence.ShortScore,
				"rangeScore":     confluence.RangeScore,
				"hurstGate":      confluence.HurstGate,
				"obvDivDir":      bundle.OBVDiv.Direction,
				"iftRsi":         bundle.IFT.Current,
				"avwapZ":         bundle.AVWAP.ZScore,
				"keltnerSqueeze": bundle.Keltner.InSqueeze,
			},
			"rangeSource":     "support_resistance_atr_buffered",
			"fundingIncluded": false,
			"pricePrecision":  symbol.GetPricePrecision(),
			"amountPrecision": symbol.GetAmountPrecision(),
			"warning":         "backtest proxy is not live trading performance",
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
	if config.ScanMode == "" {
		config.ScanMode = "TOP_K"
	}
	if config.MaxSymbols < 1 || config.MaxSymbols > 500 {
		return errors.New("max symbols per scan must be between 1 and 500")
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
	// v2.0.29: profit factor is persisted into autogrid_candidates.profit_factor
	// NUMERIC(12,6) (max 999999.999999). When the negative-return sum shrinks to
	// a single marginal candle's epsilon, positive/negative explodes past any
	// fixed precision (prod COHRX 2026-08-21 12:40Z: scan persist died with
	// SQLSTATE 22003 and every scheduled scan failed while the razor window
	// lasted). Cap at 99 like the zero-negative branch above: PF >= 99 already
	// means "no meaningful losing candles" and every consumer (MinProfitFactor
	// gate ~1.05, scannerScore clamp at 2) saturates far below it.
	return winRate, clamp(positive/negative, 0, 99)
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

func isMajorSymbol(baseCurrency, _ string) bool {
	// Exact base match only (v2.0.27): the old full-symbol prefix fallback
	// matched SOLVX/ETHW-style tickers as SOL/ETH and handed them the
	// majors waivers and score boost.
	switch strings.ToUpper(baseCurrency) {
	case "BTC", "ETH", "SOL":
		return true
	}
	return false
}
