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
	// BetaDownShortMode (v2.0.147) marks a scan taken while the beta gate
	// itself reads BTC TREND_DOWN: short-side anti-FOMO floors lift for
	// pairs whose OWN tape confirms the downtrend (strong trend + falling
	// EMA — the same strongTrend band semantics the floors already widen
	// on). Counterfactual 2026-09-28 (5 167 rejected SHORT candidates, 30h):
	// Anti-FOMO-cut shorts fell a median −0.32% (win/loss 2.4:1) and
	// entry-timing-cut shorts −1.09% (4.3:1) while 67 NEUTRAL bots bled
	// −$19.62 — range logic ("don't enter at the channel extreme") applied
	// to a trend regime where the extreme IS the continuation. R1+Vision
	// stays armed everywhere (its 16 cuts went +0.66% AGAINST the short —
	// that gate is correct). LONG/NEUTRAL floors and every other veto stay
	// armed.
	BetaDownShortMode bool
	// NotionalPerBot (v2.0.75) is budget×leverage the fleet commits per bot.
	// Grid density scales with it (GridLevelsForRange): 0 = unknown, the
	// level count then follows the bare 0.25% step floor.
	NotionalPerBot float64
	// Quant & Vision Engine v3.0 controls
	UniverseScanCap int
	MaxSpreadPct    float64
	GaussianDensity bool
	// VolQuota (wave B "2-5% in 24-72h" plan) reserves pre-selection slots
	// for volatile pairs: beyond the turnover top-K that already enters L2
	// deep analysis, up to VolQuota candidates whose 24h ticker range sits
	// inside [volQuotaMinRangePct, volQuotaMaxRangePct] join the pool — the
	// movers a pure turnover top silently starves. ≤0 falls back to
	// DefaultVolQuota (the autogrid Settings path leaves it unset, so the
	// default rides the zero value).
	VolQuota int
}

const (
	// DefaultVolQuota is the size of the volatile-pair pre-selection quota
	// when ScanConfig.VolQuota is unset (zero).
	DefaultVolQuota = 8
	// volQuotaMinRangePct / volQuotaMaxRangePct bound the 24h ticker range
	// band that marks a pair "volatile enough to matter, sane enough to
	// grid" for the quota: below 2.5%/day there is nothing to harvest in
	// 24-72h, above 10%/day the tape is a lottery.
	volQuotaMinRangePct = 2.5
	volQuotaMaxRangePct = 10.0
)

type MarketClient interface {
	GetMarketSymbols(context.Context, string) ([]pionex.SymbolInfo, error)
	GetTickers(context.Context, string, string) ([]pionex.TickerInfo, error)
	GetKlines(context.Context, string, string, int) ([]pionex.KlineCandle, error)
}

type Scanner struct {
	client   MarketClient
	universe UniverseProvider
}

func NewScanner(client MarketClient) *Scanner {
	return &Scanner{client: client, universe: NewCachedUniverse(client)}
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
	symbols, err := s.universe.TradableUSDTPerps(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch Pionex PERP universe: %w", err)
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

		// Quant & Vision v3.0: Filter out illiquid pairs at L1 to save API budget
		if config.MinVolume24h.IsPositive() && amount.LessThan(config.MinVolume24h) {
			continue
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
		if config.UniverseScanCap > 0 {
			scanCap = config.UniverseScanCap
		} else {
			scanCap = config.MaxSymbols * 3
		}
	}
	if scanCap < 30 {
		scanCap = 30
	}
	if scanCap > 350 {
		scanCap = 350
	}
	activeRanked := ranked
	if len(activeRanked) > scanCap {
		activeRanked = activeRanked[:scanCap]
	}
	// Wave B volatile quota: keep the turnover top intact AND admit a bounded
	// set of volatile pairs from the turnover tail — the 24-72h movers that
	// never crack a pure liquidity top. The quota candidates passed the same
	// min-volume L1 floor as everyone; they just rank lower on turnover.
	activeRanked = appendVolatilityQuota(activeRanked, ranked[len(activeRanked):], config.VolQuota)

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

// volQuotaSize resolves the configured quota: unset (0) or negative falls
// back to DefaultVolQuota, the same use-site-default idiom UniverseScanCap
// follows (the autogrid Settings path does not plumb the field yet).
func volQuotaSize(configured int) int {
	if configured <= 0 {
		return DefaultVolQuota
	}
	return configured
}

// appendVolatilityQuota extends the turnover top-K pre-selection with a
// bounded quota of volatile pairs (wave B). `rest` is the turnover tail
// (already sorted by amount descending, already past the min-volume L1
// floor); among it, the first `quota` pairs whose 24h ticker range sits in
// [volQuotaMinRangePct, volQuotaMaxRangePct] — most liquid first — join the
// L2 pool. No symbol is ever duplicated (a defensive seen-set holds even for
// overlapping inputs) and no member of `active` is dropped: the quota only
// ADDS candidates the pure turnover top would starve.
func appendVolatilityQuota(active, rest []rankedSymbol, quota int) []rankedSymbol {
	quota = volQuotaSize(quota)
	if len(rest) == 0 {
		return active
	}
	seen := make(map[string]struct{}, len(active)+quota)
	for _, item := range active {
		seen[item.symbol.Symbol] = struct{}{}
	}
	out := active
	for _, item := range rest {
		if len(out)-len(active) >= quota {
			break
		}
		if _, duplicate := seen[item.symbol.Symbol]; duplicate {
			continue
		}
		rangePct := tickerRangePct(item.ticker)
		if rangePct < volQuotaMinRangePct || rangePct > volQuotaMaxRangePct {
			continue
		}
		seen[item.symbol.Symbol] = struct{}{}
		out = append(out, item)
	}
	return out
}

// tickerRangePct estimates daily volatility from the 24h ticker alone —
// (High−Low)/Open in percent. L1 has no candles yet, so this proxy bands
// the volatile quota; the L2 candle blend (VolatilityPct) remains the
// authoritative metric scored downstream. Zero when the ticker carries no
// usable High/Low (the pair is then simply not quota-eligible).
func tickerRangePct(ticker pionex.TickerInfo) float64 {
	open, _ := ticker.Open.Float64()
	high, _ := ticker.High.Float64()
	low, _ := ticker.Low.Float64()
	if open <= 0 || low <= 0 || high < low {
		return 0
	}
	return (high - low) / open * 100
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

	// Support/resistance bounds FIRST (v2.0.93 FIX-L): the persisted geometry
	// ships these bounds, so the level count AND the EV model below must be
	// derived from the S/R span, not the volatility-blend range. The old order
	// derived gridNum from the (up to 25%) volatility range, modeled crossings
	// on that step, then re-derived the count from the (usually 2.5× tighter)
	// S/R span at persist time — the model graded a geometry that never
	// existed while the shipped one starved at the fee-gate.
	price := ticker.Close
	priceFloat, _ := price.Float64()
	lowerFloat, upperFloat := supportResistanceRange(sorted, priceFloat, volatilityPct)
	rangeFraction := 0.0
	if upperFloat > lowerFloat && upperFloat > 0 {
		rangeFraction = (upperFloat - lowerFloat) / upperFloat / 2
	}

	// Grid level count: density scales with the margin (v2.0.75) — the count
	// comes from notional/level ≥ $8 with the PARAMETERIZED fee-gate step
	// floor (v2.0.93 FIX-A: config.FeeBps/SlippageBps, not the pinned fleet
	// default). The count is derived from the S/R span that actually ships;
	// the volatility-blend range is only the degenerate fallback when the S/R
	// range is unreadable.
	// One canonical span drives density, EV and the fee gate. Using the lower
	// bound for density and the midline for the gate made the same persisted
	// geometry receive two different step estimates.
	modelSpanPct := rangePct
	if srSpanPct := (upperFloat - lowerFloat) / ((upperFloat + lowerFloat) / 2) * 100; srSpanPct > 0 {
		modelSpanPct = srSpanPct
	}
	gridNum := GridLevelsForRange(modelSpanPct, config.NotionalPerBot, config.FeeBps, config.SlippageBps)
	gridStep := modelSpanPct / 100 / float64(gridNum)
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
	sharpe, sharpeValid := ratio(modelReturns, false, periodsPerDay*365)
	sortino, sortinoValid := ratio(modelReturns, true, periodsPerDay*365)
	maxDrawdown := maxDrawdown(modelReturns) * 100
	winRate, profitFactor, pfValid, wrValid := winRateAndProfitFactor(modelReturns)
	turnover := float64(crossings) * 2 / float64(len(modelReturns))

	pricePrec := symbol.GetPricePrecision()
	lower := decimal.NewFromFloat(lowerFloat).Round(int32(pricePrec))
	upper := decimal.NewFromFloat(upperFloat).Round(int32(pricePrec))
	// v2.0.93 FIX-L: gridNum above is ALREADY derived from the S/R span the
	// candidate persists (the re-derivation used to happen here, after the EV
	// model had graded the volatility-span geometry instead).

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
	// v2.0.162 (unlock plan): the mature-trend LONG demotion (v2.0.39,
	// ADX≥28 → no_trend) is REMOVED by operator directive — it confined
	// LONG to the 22–28 ADX window and fed strong rallies into the neutral
	// generator instead. The original 1W/3L evidence was a 4-trade sample
	// from the pre-directional-era exit machinery; the walk-forward
	// backtest gate and the 161/162 exemption cohorts (14-day outcome
	// partition) now carry the validation load.
	if rangeFraction < 0.005 {
		recommendedTrend = "no_trend"
	}

	reasons := make([]string, 0)
	// v2.0.100 non-ASCII symbol gate: Pionex lists CJK meme pairs (牛来,
	// 龙虾…) whose symbols the futuresGrid create endpoint refuses outright
	// (P_TRADING_BOT_INVALID_ARGUMENT "get symbol info failed") — burned two
	// deploy slots on 09-25 (#1287, #1304). The candidate never reaches the
	// deploy gate again.
	if !isASCIISymbol(symbol.Symbol) {
		reasons = append(reasons, "не-ASCII символ пары: биржа отклоняет create для CJK-тикеров (P_TRADING_BOT_INVALID_ARGUMENT)")
	}
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
	if evPct < minEV {
		reasons = append(reasons, "model EV below limit")
	}
	if sharpeValid && sharpe < config.MinSharpe {
		reasons = append(reasons, "model Sharpe below limit")
	}
	if maxDrawdown > config.MaxDrawdownPct {
		reasons = append(reasons, "model max drawdown above limit")
	}
	if pfValid && profitFactor < config.MinProfitFactor {
		reasons = append(reasons, "model profit factor below limit")
	}
	// v2.0.89-A fee-gate (P1, best-practice research; floor 2.5× since
	// v2.0.94): the invariant «level step ≥ StepFloorRoundTripMultiple ×
	// round-trip costs» evaluated on the FINAL persisted
	// geometry — the support/resistance span the candidate actually stores,
	// divided by the density-derived level count (the only count there is:
	// any AI Kit row clamp would land on this same GridNum). The old check
	// compared the volatility MODEL span, which passed while the persisted
	// S/R span ran ~2.5× tighter — a 7d prod audit put 46.5% of ACCEPTED
	// candidates on sub-0.28% steps, grids that pay the feed more per
	// traverse than they can harvest.
	feeGateStepPct := GridStepPctForSpan(modelSpanPct, gridNum)
	if feeReason, violated := FeeGateRejection(feeGateStepPct, config.FeeBps, config.SlippageBps); violated {
		reasons = append(reasons, feeReason)
	}
	// Compression can be a valid range when the tape is genuinely choppy and
	// weak. Keep the breakout veto for directional/low-choppiness squeezes.
	if neutralSqueezeRisk(regime) && recommendedTrend == "no_trend" {
		reasons = append(reasons, "volatility squeeze: impending explosive breakout")
	}
	if recommendedTrend == "no_trend" && (regime.ADX > 32.0 || math.Abs(regime.EMASlopePct) > 3.0 ||
		math.Abs(change24hPct) > 8.0 || math.Abs(change6hPct) > 4.0) {
		reasons = append(reasons, fmt.Sprintf(
			"trend too strong for neutral grid (ADX: %.1f, EMA slope: %.2f%%, 24h: %+.1f%%, 6h: %+.1f%%)",
			regime.ADX, regime.EMASlopePct, change24hPct, change6hPct))
	}
	// Semi-trend dead zone (v2.0.39, closed-ledger audit 2026-08-23..30):
	// NEUTRAL grids deployed with ADX 24-32 lost −$61.4 against +$9.0 won
	// below 24 (13 stop-outs: NEAR/SNXXX/ZAMA/LAB/BMNRX/ZRO/DOS/MSTRX/
	// SOXLX×2/BAND/GALA/PUMP). The pair is already trending while the >32
	// veto above still calls it a range — the single worst entry zone of
	// the week.
	if neutralSemiTrendBlocked(recommendedTrend, regime.ADX) {
		reasons = append(reasons, fmt.Sprintf(
			"полутренд: ADX %.1f в мёртвой зоне нейтрала 24-32 (недельный аудит: −$61 против +$9) — вход отложен",
			regime.ADX))
	}

	// v2.0.94 OU half-life entry gate: measure the regime's AR(1) mean-
	// reversion half-life on the same closes the model reads. Weekly mining
	// of 155 paper outcomes: neutral grids whose regime half-life measured
	// < 2h averaged −$0.07/bot (net −$1.59 on 23) while 2–4h paid +$0.46,
	// 4–6h +$1.22 and ≥6h +$1.37 — monotone. A regime that does not persist
	// cannot amortize the round-trip friction before the OU rotation timer
	// closes the grid; skipping it frees the slot for a persistent one.
	// Applies to neutral grids only — directional theses do not rest on
	// mean reversion. A non-fitting tape (trend/divergent, ok=false) is
	// left to the Hurst/ADX vetoes upstream.
	ouHalfLifeHours := 0.0
	if hlSteps, ok := OUHalfLifeSteps(CandleCloses(sorted)); ok {
		ouHalfLifeHours = hlSteps * CandleIntervalHours(config.Interval)
	}
	if recommendedTrend == "no_trend" && MinOUHalfLifeHours > 0 && ouHalfLifeHours > 0 &&
		ouHalfLifeHours < MinOUHalfLifeHours {
		reasons = append(reasons, fmt.Sprintf(
			"OU half-life %.1fч < %.0fч — режим не живёт достаточно долго, чтобы окупить издержки (майнинг недели: HL<2ч = −$0.07/бот, HL≥2ч = +$0.46+)",
			ouHalfLifeHours, MinOUHalfLifeHours))
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
			// v2.0.189: RSI cap stays lifted for a strong trend, but the
			// CHANNEL-POSITION cap no longer does — prod MSTRX #1546 entered
			// LONG at 80-88% of its range (the rally top) on a strong-trend
			// exemption and rode the reversal to -73% of its stop. Entries
			// at the ceiling are the long-side twin of "no shorts at the
			// floor": buy pullbacks in a trend, not its peak.
			rsiCap, posCap = 78.0, 75.0
		}
		if regime.RSI > rsiCap {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: RSI (%.1f) > %.0f - пара перекуплена на экстремуме, вход в LONG заблокирован", regime.RSI, rsiCap))
		}
		if regime.RangePositionPct > posCap {
			reasons = append(reasons, fmt.Sprintf("Anti-FOMO: положение в канале (%.1f%%) > %.0f%% - вход в LONG выше предела заблокирован", regime.RangePositionPct, posCap))
		}
	} else if recommendedTrend == "short" {
		if antiFomoShortFloorsLifted(config.CascadeShortMode, config.BetaDownShortMode, regime.ADX, regime.EMASlopePct, regime.RSI) {
			// v2.0.21 cascade window: skip the RSI/position floors for
			// shorts (see ScanConfig.CascadeShortMode) — the oversold
			// reading IS the signal during a forced unwind.
			// v2.0.147: same lift during a beta-down regime when the pair's
			// own tape confirms the downtrend (see ScanConfig.BetaDownShortMode).
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
	// Hard regime veto: a persistently trending memory (Hurst > 0.58)
	// loads one-sided inventory into a fresh neutral grid — the exact
	// failure the daily-loss breaker only sees after the damage.
	if recommendedTrend == "no_trend" && HurstHardVetoNeutral(bundle) {
		reasons = append(reasons, fmt.Sprintf(
			"confluence veto: Hurst %.2f > 0.58 — persistent trend regime, neutral grid would load one-sided inventory",
			bundle.Hurst))
	}
	// Kaufman Efficiency Ratio gate (v2.0.89-A, P2): ER = |p_N − p_0| /
	// Σ|p_i − p_{i−1}| over the SAME lookback candles the regime and ATR
	// readings come from — zero extra API calls. ER > 0.60 means the tape is
	// directional/effective: a NEUTRAL grid would harvest the trend's scraps
	// while loading one-sided inventory, so the candidate is vetoed. The
	// mean-reversion side (ER < 0.30) never blocks — it rides in
	// model_assumptions (kaufmanER / kaufmanRegime) as calibration telemetry.
	if recommendedTrend == "no_trend" && bundle.KaufmanER > KaufmanERTrendVeto {
		reasons = append(reasons, fmt.Sprintf(
			"Kaufman ER %.2f — направленное движение, сетка не входит", bundle.KaufmanER))
	}

	decision := "ACCEPTED"
	if len(reasons) > 0 {
		decision = "REJECTED"
	}

	score, rangeShare := scannerScore(
		volatilityPct, evPct, sharpe, maxDrawdown, profitFactor,
		regime.Choppiness, sharpeValid, pfValid, regime.IsSqueeze, config,
	)

	// Wave B separate ranking: directional candidates swap the neutral
	// baseline's range-relevance share for trend relevance, so the final
	// list orders NEUTRALs by range quality and LONG/SHORTs by trend
	// quality. The global score sort below is a linear extension of every
	// per-direction ordering — no gate or decision changes here.
	directionTrendFit := 0.0
	directionRangeShare := 0.0
	if adjust, ok := directionalScoreAdjust(
		recommendedTrend, rangeShare, regime.ADX, regime.EMASlopePct, regime.RangePositionPct,
	); ok {
		score = clamp(score-adjust.RangeShare+adjust.TrendShare, 0, 1)
		directionTrendFit = adjust.TrendFit
		directionRangeShare = adjust.RangeShare
	}

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
		// Fibonacci Golden Pocket boost for Longs (pullback to 0.618-0.786)
		if bundle.Fib.InGoldenPocket && bundle.Fib.TrendDir == 1 {
			entryFit = clamp(entryFit*1.25, 0.5, 1.0)
		} else if bundle.Fib.DistancePct <= 0.5 && bundle.Fib.NearRatio >= 0.382 {
			entryFit = clamp(entryFit*1.12, 0.5, 1.0)
		}
		// Support safety check: solid support right below entry protects the trade
		if bundle.SR.NearestSupport > 0 && bundle.SR.SupportDistPct <= 1.5 && bundle.SR.SupportStrength >= 0.6 {
			entryFit = clamp(entryFit*1.10, 0.5, 1.0)
		}
	case "short":
		if regime.RangePositionPct >= 50.0 {
			entryFit = clamp((90-regime.RangePositionPct)/40, 0.5, 1.0)
		} else {
			entryFit = clamp(1.0-(50-regime.RangePositionPct)/50, 0.5, 1.0)
		}
		// Fibonacci Golden Pocket boost for Shorts (relief bounce to 0.618-0.786)
		if bundle.Fib.InGoldenPocket && bundle.Fib.TrendDir == -1 {
			entryFit = clamp(entryFit*1.25, 0.5, 1.0)
		} else if bundle.Fib.DistancePct <= 0.5 && bundle.Fib.NearRatio >= 0.382 {
			entryFit = clamp(entryFit*1.12, 0.5, 1.0)
		}
		// Resistance safety check: solid resistance right above entry protects the trade
		if bundle.SR.NearestResist > 0 && bundle.SR.ResistDistPct <= 1.5 && bundle.SR.ResistStrength >= 0.6 {
			entryFit = clamp(entryFit*1.10, 0.5, 1.0)
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
			"isProxy":               true,
			"proxyReference":        true,
			"proxyEvaluation":       "indicative_reference_only",
			"proxyWarning":          "свечной прокси является справочной оценкой и не служит доказательством качества входа",
			"sharpeValid":           sharpeValid,
			"sharpeDisplay":         formatProxyDisplay(sharpe, sharpeValid),
			"sortinoValid":          sortinoValid,
			"sortinoDisplay":        formatProxyDisplay(sortino, sortinoValid),
			"profitFactorValid":     pfValid,
			"profitFactorDisplay":   formatProxyDisplay(profitFactor, pfValid),
			"winRateValid":          wrValid,
			"winRateDisplay":        formatWinRateDisplay(winRate, wrValid),
			"model":                 "neutral_grid_capture_proxy_v3_multitier",
			"interval":              config.Interval,
			"lookbackCandles":       len(sorted),
			"feeBpsPerFill":         config.FeeBps,
			"slippageBpsPerFill":    config.SlippageBps,
			"captureEfficiency":     0.40,
			"recommendedTrend":      recommendedTrend,
			"regime":                regime.Regime,
			"adx":                   regime.ADX,
			"rsi":                   regime.RSI,
			"choppiness":            regime.Choppiness,
			"isSqueeze":             regime.IsSqueeze,
			"emaSlopePct":           regime.EMASlopePct,
			"rangePositionPct":      regime.RangePositionPct,
			"atrPct":                regime.ATRPct,
			"volatilityParkinson":   volParkinson,
			"hurst":                 bundle.Hurst,
			"kaufmanER":             bundle.KaufmanER,
			"kaufmanRegime":         kaufmanRegimeLabel(bundle.KaufmanER),
			"ouHalfLifeHours":       ouHalfLifeHours,
			"directionalTrendFit":   directionTrendFit,
			"directionalRangeShare": directionRangeShare,
			"confluence": map[string]any{
				"verdict":           confluence.Verdict,
				"strength":          confluence.Strength,
				"longScore":         confluence.LongScore,
				"shortScore":        confluence.ShortScore,
				"rangeScore":        confluence.RangeScore,
				"hurstGate":         confluence.HurstGate,
				"obvDivDir":         bundle.OBVDiv.Direction,
				"rsiDivDir":         bundle.RSIDiv.Direction,
				"iftRsi":            bundle.IFT.Current,
				"avwapZ":            bundle.AVWAP.ZScore,
				"keltnerSqueeze":    bundle.Keltner.InSqueeze,
				"fibInGoldenPocket": bundle.Fib.InGoldenPocket,
				"fibNearRatio":      bundle.Fib.NearRatio,
				"fibNearLevel":      bundle.Fib.NearLevel,
				"macdCrossedUp":     bundle.MACD.CrossedUp,
				"macdCrossedDown":   bundle.MACD.CrossedDown,
				"macdHistogram":     bundle.MACD.Histogram,
				"stochK":            bundle.StochRSI.K,
				"stochD":            bundle.StochRSI.D,
				"stochCrossedUp":    bundle.StochRSI.CrossedUp,
				"stochCrossedDown":  bundle.StochRSI.CrossedDown,
				"srNearestSupport":  bundle.SR.NearestSupport,
				"srNearestResist":   bundle.SR.NearestResist,
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

func ratio(values []float64, downsideOnly bool, annualPeriods float64) (float64, bool) {
	average := mean(values)
	sample := values
	if downsideOnly {
		sample = make([]float64, 0, len(values))
		for _, value := range values {
			if value < 0 {
				sample = append(sample, value)
			}
		}
		if len(sample) < 2 {
			// Zero or fewer than 2 negative observations: downside deviation cannot be reliably measured.
			// Return 0.0 with valid=false rather than synthetic 4.0.
			return 0.0, false
		}
	} else if len(values) < 2 {
		return 0.0, false
	}
	deviation := sampleStdDev(sample)
	if deviation == 0 {
		return 0.0, false
	}
	// Scale by daily equivalent factor rather than 35,040 intraday periods
	// to prevent synthetic zero-drawdown toy paths from saturating.
	scale := math.Sqrt(math.Min(annualPeriods, 365.0))
	return clamp(average/deviation*scale, -10, 10), true
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

func winRateAndProfitFactor(values []float64) (float64, float64, bool, bool) {
	if len(values) == 0 {
		return 0.0, 0.0, false, false
	}
	wins, losses := 0, 0
	positive, negative := 0.0, 0.0
	for _, value := range values {
		if value > 0 {
			wins++
			positive += value
		} else if value < 0 {
			losses++
			negative += -value
		}
	}
	winRate := float64(wins) / float64(len(values)) * 100
	if losses == 0 {
		// Zero losing steps in proxy: cannot compute a statistically valid profit factor or win rate.
		// Return 0.0 with valid=false rather than fake 99 or claiming 100% verified win rate.
		return winRate, 0.0, false, false
	}
	// v2.0.29: profit factor is persisted into autogrid_candidates.profit_factor
	// NUMERIC(12,6) (max 999999.999999). Cap at 99 for DB boundary.
	profitFactor := clamp(positive/negative, 0, 99)
	return winRate, profitFactor, true, true
}

func formatProxyDisplay(val float64, valid bool) string {
	if !valid {
		return "недостаточно данных"
	}
	return fmt.Sprintf("%.2f", val)
}

func formatWinRateDisplay(val float64, valid bool) string {
	if !valid {
		return "недостаточно данных"
	}
	return fmt.Sprintf("%.1f%%", val)
}

// scannerScore produces the direction-agnostic ranking baseline plus the
// RANGE-relevance share embedded in it. The choppiness term below is the one
// range-shaped reward every candidate — neutral or directional — used to
// receive identically; reporting its exact final-score contribution lets
// directionalScoreAdjust swap that share for trend relevance so each
// direction ranks by its own thesis (wave B separate ranking).
func scannerScore(
	volatility, ev, sharpe, drawdown, profitFactor, choppiness float64,
	sharpeValid, pfValid bool,
	isSqueeze bool,
	config ScanConfig,
) (score float64, rangeShare float64) {
	volatilityFit := 1 - math.Abs(
		volatility-(config.MinVolatilityPct+config.MaxVolatilityPct)/2,
	)/math.Max(config.MaxVolatilityPct-config.MinVolatilityPct, 1)

	chopFit := clamp((choppiness-40.0)/30.0, 0, 1)
	squeezePenalty := 1.0
	if isSqueeze {
		squeezePenalty = 0.8
	}

	sharpeFactor := 0.5
	if sharpeValid {
		sharpeFactor = clamp(sharpe/math.Max(config.MinSharpe, 0.25), 0, 2)
	}

	pfFactor := 0.5
	if pfValid {
		pfFactor = clamp(profitFactor/math.Max(config.MinProfitFactor, 1), 0, 2)
	}

	raw := clamp(volatilityFit, 0, 1)*0.15 +
		clamp(ev/math.Max(config.MinExpectedValuePct+0.25, 0.25), 0, 2)*0.20 +
		sharpeFactor*0.20 +
		clamp(1-drawdown/math.Max(config.MaxDrawdownPct, 1), 0, 1)*0.15 +
		pfFactor*0.15 +
		chopFit*scannerRangeWeight

	score = clamp((raw/scannerScoreNorm)*squeezePenalty, 0, 1)
	rangeShare = chopFit * scannerRangeWeight / scannerScoreNorm * squeezePenalty
	return score, rangeShare
}

const (
	// scannerRangeWeight is the choppiness (range-relevance) term's weight
	// in the raw scannerScore sum; scannerScoreNorm is the divisor that
	// brings the capped raw sum back to 0..1. directionalScoreAdjust
	// re-uses the same budget for the trend-relevance term it swaps in.
	scannerRangeWeight = 0.15
	scannerScoreNorm   = 1.4
)

// DirectionalScoreAdjustment decomposes the ranking correction applied to a
// directional thesis: RangeShare is removed from the neutral baseline,
// TrendShare is added in its place (same weight budget), and TrendFit is the
// 0..1 trend relevance behind that share (surfaced in ModelAssumptions for
// telemetry).
type DirectionalScoreAdjustment struct {
	RangeShare float64
	TrendShare float64
	TrendFit   float64
}

// directionalScoreAdjust implements separate per-direction ranking (wave B):
// for recommendedTrend long/short the range-relevance share scannerScore paid
// (the choppiness term — the squeeze anchor, confluence range support and
// the OU half-life gate are neutral-scoped by construction and need no
// directional removal) is dropped and replaced by trend relevance:
//
//   - EMA(20) slope co-directional with the thesis (a counter-slope earns
//     nothing),
//   - ADX confirmation ramping 22→30 — DetectRegime calls a trend at 22 and
//     the 22-30 band is where directionals become reachable; the ramp stays
//     flat at 1.0 above 30 (v2.0.162 doctrine: mature trends feed LONG
//     grids, they are not punished),
//   - channel position matching the anti-FOMO floors the gates enforce —
//     long ≤75%, short ≥25%; the deeper the compliance margin the better.
//
// Gates are untouched: this ONLY reorders candidates inside each direction
// so neutrals rank by range quality and directionals by trend quality.
// ok=false for neutral candidates — their baseline score already ranks by
// range relevance.
func directionalScoreAdjust(
	trend string,
	rangeShare, adx, emaSlopePct, rangePositionPct float64,
) (DirectionalScoreAdjustment, bool) {
	if trend != "long" && trend != "short" {
		return DirectionalScoreAdjustment{}, false
	}
	fit := directionalTrendFit(trend, adx, emaSlopePct, rangePositionPct)
	return DirectionalScoreAdjustment{
		RangeShare: rangeShare,
		TrendShare: fit * scannerRangeWeight / scannerScoreNorm,
		TrendFit:   fit,
	}, true
}

// directionalTrendFit blends the three trend-voice components into 0..1.
func directionalTrendFit(trend string, adx, emaSlopePct, rangePositionPct float64) float64 {
	emaFit := 0.0
	if (trend == "long" && emaSlopePct > 0) || (trend == "short" && emaSlopePct < 0) {
		// Full credit at |slope| ≥ 1%/10 candles (the confirmed-trend band
		// the engine keys on is ±0.5%).
		emaFit = clamp(math.Abs(emaSlopePct), 0, 1)
	}
	// ADX 22→30 ramp, flat 1.0 above — see the v2.0.162 note in the
	// directionalScoreAdjust doc.
	adxFit := clamp((adx-adxTrendThreshold)/8.0, 0, 1)
	posFit := 0.0
	if trend == "long" {
		// Anti-FOMO LONG cap is 75%: full credit entering deep in the lower
		// half of the channel, nothing at the cap itself.
		posFit = clamp((75.0-rangePositionPct)/50.0, 0, 1)
	} else {
		// Anti-FOMO SHORT floor is 25%: mirrored.
		posFit = clamp((rangePositionPct-25.0)/50.0, 0, 1)
	}
	return clamp(0.40*emaFit+0.35*adxFit+0.25*posFit, 0, 1)
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

// neutralSemiTrendBlocked is the closed-ledger-audited dead zone: NEUTRAL
// candidates whose tape already reads ADX 24-32 (v2.0.39). The pair trends
// while the classic >32 veto still calls it a range.
func neutralSemiTrendBlocked(trend string, adx float64) bool {
	return trend == "no_trend" && adx >= 24.0 && adx <= 32.0
}

// neutralSqueezeRisk distinguishes a quiet, choppy compression from a
// compression that is more likely to break. A squeeze remains a score
// penalty in both cases; only the latter is a hard neutral-grid rejection.
func neutralSqueezeRisk(regime RegimeResult) bool {
	return regime.IsSqueeze && !(regime.ADX < 20.0 && regime.Choppiness > 55.0)
}

// neutralSqueezeRisk is documented above; the mature-trend LONG demotion
// helper (v2.0.39) was removed in v2.0.162 by operator directive — see the
// call-site comment in scoreCandidate.

// antiFomoShortFloorsLifted decides whether the SHORT RSI/position floors
// stand down for this scan: the v2.0.21 cascade window lifts them
// unconditionally (forced-unwind pass). v2.0.147 lifted them for the
// beta-down regime when the pair's own tape confirmed the downtrend;
// v2.0.162 (unlock plan step 5) widens that lift to ANY confirmed own
// downtrend — strong trend (the same >22 ADX / >0.5 slope band the floors
// already widen on) AND a falling EMA. Slope sign is the direction proof:
// an impulse breakdown sits at the channel bottom by construction, and
// the oversold reading IS the signal, not a trap, while the trend carries.
func antiFomoShortFloorsLifted(cascadeMode, betaDownShortMode bool, adx, emaSlopePct float64, rsi float64) bool {
	if cascadeMode {
		return true
	}
	// v2.0.169 (consensus-1 debt): the RSI >= 30 floor is RESTORED even for
	// confirmed downtrends — shorting into a fresh oversold impulse is the
	// worst entry point (negative funding against shorts, extreme funding
	// predicts squeeze, bear rallies ~10% every ~44d). The v2.0.166 exempt
	// function guards the same thing at deploy time; this restores the
	// scanner-level rejection so the candidate never reaches the deploy gate.
	if rsi > 0 && rsi < 30.0 {
		return false
	}
	_ = betaDownShortMode // subsumed by the own-tape confirmation below
	strongTrend := adx > 22.0 || math.Abs(emaSlopePct) > 0.5
	return strongTrend && emaSlopePct < 0
}

// isASCIISymbol reports whether the exchange symbol is pure printable
// ASCII — the futuresGrid create endpoint cannot resolve non-ASCII tickers.
func isASCIISymbol(symbol string) bool {
	for i := 0; i < len(symbol); i++ {
		if symbol[i] < 0x21 || symbol[i] > 0x7E {
			return false
		}
	}
	return true
}
