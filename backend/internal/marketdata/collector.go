package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exchange identifiers persisted in the funding_snapshots / oi_history tables.
const (
	ExchangeBinance = "binance"
	ExchangeBybit   = "bybit"
	ExchangeOKX     = "okx"
)

// Default endpoints. These are market-data references only: Pionex remains
// the sole trading venue; Binance/Bybit/OKX are read for cross-exchange
// funding and open-interest context.
const (
	DefaultBinanceBaseURL       = "https://fapi.binance.com"
	DefaultBybitBaseURL         = "https://api.bybit.com"
	DefaultOKXBaseURL           = "https://www.okx.com"
	DefaultFNGEndpoint          = "https://api.alternative.me/fng/?limit=1"
	DefaultForexFactoryEndpoint = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"
)

// limiterKeySentiment / limiterKeyEvents rate-limit the two non-exchange feeds.
const (
	limiterKeySentiment = "sentiment"
	limiterKeyEvents    = "events"

	maxResponseBytes = 4 << 20 // 4 MiB safety cap per response
)

// CollectorConfig controls every collector loop. Zero values are replaced
// with defaults by withDefaults, so callers only override what they need.
type CollectorConfig struct {
	// Symbols is the watch list (top-20) in Pionex perpetual form.
	Symbols []string

	FundingInterval   time.Duration // default 60s
	OIInterval        time.Duration // default 5m
	SentimentInterval time.Duration // default 1h
	EventsInterval    time.Duration // default 1h

	HTTPTimeout        time.Duration // default 10s per request
	MinRequestInterval time.Duration // default 1s per host (rate limit)

	BinanceBaseURL       string
	BybitBaseURL         string
	OKXBaseURL           string
	FNGEndpoint          string
	ForexFactoryEndpoint string
}

// DefaultCollectorConfig returns the production configuration for a symbol list.
func DefaultCollectorConfig(symbols []string) CollectorConfig {
	return CollectorConfig{
		Symbols:              symbols,
		FundingInterval:      60 * time.Second,
		OIInterval:           5 * time.Minute,
		SentimentInterval:    time.Hour,
		EventsInterval:       time.Hour,
		HTTPTimeout:          10 * time.Second,
		MinRequestInterval:   time.Second,
		BinanceBaseURL:       DefaultBinanceBaseURL,
		BybitBaseURL:         DefaultBybitBaseURL,
		OKXBaseURL:           DefaultOKXBaseURL,
		FNGEndpoint:          DefaultFNGEndpoint,
		ForexFactoryEndpoint: DefaultForexFactoryEndpoint,
	}
}

func (cfg CollectorConfig) withDefaults() CollectorConfig {
	if cfg.FundingInterval <= 0 {
		cfg.FundingInterval = 60 * time.Second
	}
	if cfg.OIInterval <= 0 {
		cfg.OIInterval = 5 * time.Minute
	}
	if cfg.SentimentInterval <= 0 {
		cfg.SentimentInterval = time.Hour
	}
	if cfg.EventsInterval <= 0 {
		cfg.EventsInterval = time.Hour
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if cfg.MinRequestInterval < 0 {
		cfg.MinRequestInterval = 0
	}
	if cfg.BinanceBaseURL == "" {
		cfg.BinanceBaseURL = DefaultBinanceBaseURL
	}
	if cfg.BybitBaseURL == "" {
		cfg.BybitBaseURL = DefaultBybitBaseURL
	}
	if cfg.OKXBaseURL == "" {
		cfg.OKXBaseURL = DefaultOKXBaseURL
	}
	if cfg.FNGEndpoint == "" {
		cfg.FNGEndpoint = DefaultFNGEndpoint
	}
	if cfg.ForexFactoryEndpoint == "" {
		cfg.ForexFactoryEndpoint = DefaultForexFactoryEndpoint
	}
	return cfg
}

// exchangeLimiter is a minimal-interval gate: it admits at most one request
// per interval, which is exactly the "no more than 1 req/s per exchange"
// requirement. Interval 0 disables limiting (used by tests).
type exchangeLimiter struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

func (l *exchangeLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	var delay time.Duration
	if now.Before(l.next) {
		delay = l.next.Sub(now)
		l.next = l.next.Add(l.interval)
	} else {
		l.next = now.Add(l.interval)
	}
	l.mu.Unlock()

	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// fundingSample is one exchange observation of funding conditions.
type fundingSample struct {
	Symbol    string // Pionex form, e.g. BTC_USDT_PERP
	Exchange  string
	Rate      float64
	MarkPrice float64 // 0 = unknown
}

// oiSample is one exchange observation of open interest in USD terms.
type oiSample struct {
	Symbol   string // Pionex form
	Exchange string
	OIUSD    float64
	Price    float64
}

// Collector periodically pulls cross-exchange market context into PostgreSQL.
// It never trades and never talks to Pionex order endpoints.
type Collector struct {
	db         *pgxpool.Pool
	cfg        CollectorConfig
	httpClient *http.Client
	limiters   map[string]*exchangeLimiter

	// Dynamic universe (v2.0.3): when a UniverseSource is configured the
	// funding/OI loops switch from the static watch list to bulk endpoints
	// covering every Pionex PERP symbol.
	universe     UniverseSource
	universeMu   sync.Mutex
	aliases      map[string]string // exchange-native symbol -> Pionex form
	universeSize int
	okxWatch     []string        // top-N by turnover for the per-symbol OKX feed
	okxValid     map[string]bool // instruments confirmed to exist on OKX
	universeTime time.Time
}

// SetUniverse switches the collector to full-market mode: the symbol list is
// derived from the Pionex PERP universe and refreshed hourly.
func (c *Collector) SetUniverse(source UniverseSource) {
	c.universe = source
}

// universeRefreshInterval is how often the Pionex PERP universe is re-derived.
const universeRefreshInterval = time.Hour

// okxWatchSize is the per-symbol OKX feed breadth: OKX has no bulk funding
// endpoint, so it covers only the most liquid tail by turnover.
const okxWatchSize = 20

// ensureUniverse refreshes the alias map and OKX watch list at most hourly.
// A failed refresh keeps the last known universe so a transient Pionex outage
// cannot blind the collector.
func (c *Collector) ensureUniverse(ctx context.Context) bool {
	c.universeMu.Lock()
	defer c.universeMu.Unlock()
	if c.universeSize > 0 && time.Since(c.universeTime) < universeRefreshInterval {
		return true
	}
	symbols, err := c.universe.RankedPerpSymbols(ctx)
	if err != nil {
		slog.Warn("collector universe refresh failed; keeping last known", "error", err)
		return c.universeSize > 0
	}
	if len(symbols) == 0 {
		return false
	}
	c.universeSize = len(symbols)
	c.aliases = BuildExchangeAliasMap(symbols)
	c.okxWatch = symbols
	if len(c.okxWatch) > okxWatchSize {
		c.okxWatch = c.okxWatch[:okxWatchSize]
	}
	c.okxValid = make(map[string]bool)
	c.universeTime = time.Now()
	slog.Info("collector universe refreshed",
		"symbols", c.universeSize, "aliases", len(c.aliases), "okx_watch", len(c.okxWatch))
	return true
}

func (c *Collector) aliasesSnapshot() map[string]string {
	c.universeMu.Lock()
	defer c.universeMu.Unlock()
	return c.aliases
}

// NewCollector creates a collector for the given Pionex symbols with
// production defaults.
func NewCollector(db *pgxpool.Pool, symbols []string) *Collector {
	return NewCollectorWithConfig(db, DefaultCollectorConfig(symbols))
}

// NewCollectorWithConfig creates a collector with explicit configuration;
// zero values fall back to defaults. db may be nil in tests that only
// exercise the fetch/parse layer.
func NewCollectorWithConfig(db *pgxpool.Pool, cfg CollectorConfig) *Collector {
	cfg = cfg.withDefaults()
	return &Collector{
		db:         db,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.HTTPTimeout},
		limiters: map[string]*exchangeLimiter{
			ExchangeBinance:     {interval: cfg.MinRequestInterval},
			ExchangeBybit:       {interval: cfg.MinRequestInterval},
			ExchangeOKX:         {interval: cfg.MinRequestInterval},
			limiterKeySentiment: {interval: cfg.MinRequestInterval},
			limiterKeyEvents:    {interval: cfg.MinRequestInterval},
		},
	}
}

// Run starts all collector loops and blocks until ctx is cancelled. Each
// collector runs in its own goroutine with its own ticker; a failed request
// is logged and skipped, never propagated.
func (c *Collector) Run(ctx context.Context) {
	jobs := []struct {
		name     string
		interval time.Duration
		tick     func(context.Context)
	}{
		{"funding", c.cfg.FundingInterval, c.collectFunding},
		{"open_interest", c.cfg.OIInterval, c.collectOpenInterest},
		{"sentiment", c.cfg.SentimentInterval, c.collectSentiment},
		{"economic_events", c.cfg.EventsInterval, c.collectEconomicEvents},
		{"retention", time.Hour, c.collectRetention},
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(name string, interval time.Duration, tick func(context.Context)) {
			defer wg.Done()
			c.loop(ctx, name, interval, tick)
		}(job.name, job.interval, job.tick)
	}
	wg.Wait()
	slog.Info("marketdata collector stopped")
}

func (c *Collector) loop(ctx context.Context, name string, interval time.Duration, tick func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	slog.Info("marketdata collector started",
		"collector", name, "interval", interval.String(), "symbols", len(c.cfg.Symbols))

	c.runTick(ctx, name, tick) // warm up immediately instead of waiting one interval
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runTick(ctx, name, tick)
		}
	}
}

// runTick executes one pass, converting any panic into an error log so a
// single bad payload can never take the process down.
func (c *Collector) runTick(ctx context.Context, name string, tick func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("marketdata collector panic recovered", "collector", name, "panic", r)
		}
	}()
	tick(ctx)
}

// ---------------------------------------------------------------------------
// Funding collector (every FundingInterval, default 60s)
// ---------------------------------------------------------------------------

func (c *Collector) collectFunding(ctx context.Context) {
	if c.universe != nil {
		if !c.ensureUniverse(ctx) {
			return
		}
		c.collectFundingBulk(ctx)
		return
	}
	for _, symbol := range c.cfg.Symbols {
		if ctx.Err() != nil {
			return
		}
		c.collectFundingSymbol(ctx, symbol)
	}
}

// collectFundingBulk covers the whole Pionex PERP universe with two bulk
// requests (Binance premiumIndex array + Bybit linear tickers), then adds the
// per-symbol OKX feed for the most liquid tail. OKX has no bulk endpoint.
func (c *Collector) collectFundingBulk(ctx context.Context) {
	var samples []fundingSample
	if s, err := c.fetchBinancePremiumIndexAll(ctx); err != nil {
		slog.Warn("funding collector: binance bulk fetch failed", "error", err)
	} else {
		samples = append(samples, s...)
	}
	if s, _, err := c.fetchBybitTickersAll(ctx); err != nil {
		slog.Warn("funding collector: bybit bulk fetch failed", "error", err)
	} else {
		samples = append(samples, s...)
	}
	if err := c.insertFundingSamples(ctx, samples); err != nil {
		slog.Warn("funding collector: bulk persist failed", "error", err)
	}

	c.universeMu.Lock()
	okxWatch := append([]string(nil), c.okxWatch...)
	c.universeMu.Unlock()

	var okxSamples []fundingSample
	for _, symbol := range okxWatch {
		if ctx.Err() != nil {
			break
		}
		if c.okxAbsent(symbol) {
			continue // confirmed absent on OKX (Pionex-exclusive listing)
		}
		sample, err := c.fetchOKXFunding(ctx, symbol)
		if err != nil {
			if ctx.Err() == nil {
				c.markOKXAbsent(symbol)
			}
			continue
		}
		okxSamples = append(okxSamples, *sample)
	}
	if err := c.insertFundingSamples(ctx, okxSamples); err != nil {
		slog.Warn("funding collector: okx persist failed", "error", err)
	}
}

func (c *Collector) okxAbsent(symbol string) bool {
	c.universeMu.Lock()
	defer c.universeMu.Unlock()
	return c.okxValid != nil && c.okxValid[symbol] == false
}

// markOKXAbsent remembers that an instrument is not listed on OKX so the
// per-symbol feed stops retrying it until the hourly universe refresh.
func (c *Collector) markOKXAbsent(symbol string) {
	c.universeMu.Lock()
	defer c.universeMu.Unlock()
	if c.okxValid != nil {
		c.okxValid[symbol] = false
	}
}

func (c *Collector) collectFundingSymbol(ctx context.Context, symbol string) {
	if sample, err := c.fetchBinancePremiumIndex(ctx, symbol); err != nil {
		slog.Warn("funding collector: binance fetch failed", "symbol", symbol, "error", err)
	} else if err := c.insertFundingSnapshot(ctx, *sample); err != nil {
		slog.Warn("funding collector: binance persist failed", "symbol", symbol, "error", err)
	}

	if ticker, err := c.fetchBybitTicker(ctx, symbol); err != nil {
		slog.Warn("funding collector: bybit fetch failed", "symbol", symbol, "error", err)
	} else if err := c.insertFundingSnapshot(ctx, fundingSample{
		Symbol:    symbol,
		Exchange:  ExchangeBybit,
		Rate:      ticker.FundingRate,
		MarkPrice: ticker.MarkPrice,
	}); err != nil {
		slog.Warn("funding collector: bybit persist failed", "symbol", symbol, "error", err)
	}

	if sample, err := c.fetchOKXFunding(ctx, symbol); err != nil {
		slog.Warn("funding collector: okx fetch failed", "symbol", symbol, "error", err)
	} else if err := c.insertFundingSnapshot(ctx, *sample); err != nil {
		slog.Warn("funding collector: okx persist failed", "symbol", symbol, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Open Interest collector (every OIInterval, default 5m)
// ---------------------------------------------------------------------------

func (c *Collector) collectOpenInterest(ctx context.Context) {
	if c.universe != nil {
		if !c.ensureUniverse(ctx) {
			return
		}
		// Bybit reports USD open interest (openInterestValue) directly in the
		// bulk tickers response. Binance OI is per-symbol only (1 req/s would
		// exceed the 5-minute cycle for ~400 symbols), so the bulk mode relies
		// on Bybit alone — GetOIChange24h needs a single reporting exchange.
		_, oi, err := c.fetchBybitTickersAll(ctx)
		if err != nil {
			slog.Warn("oi collector: bybit bulk fetch failed", "error", err)
			return
		}
		if err := c.insertOISamples(ctx, oi); err != nil {
			slog.Warn("oi collector: bulk persist failed", "error", err)
		}
		return
	}
	for _, symbol := range c.cfg.Symbols {
		if ctx.Err() != nil {
			return
		}

		// Binance reports open interest in base-asset contracts, so the mark
		// price is fetched from premiumIndex to convert into USD.
		premium, err := c.fetchBinancePremiumIndex(ctx, symbol)
		if err != nil {
			slog.Warn("oi collector: binance mark price fetch failed", "symbol", symbol, "error", err)
		} else {
			contracts, err := c.fetchBinanceOpenInterest(ctx, symbol)
			if err != nil {
				slog.Warn("oi collector: binance openInterest fetch failed", "symbol", symbol, "error", err)
			} else if err := c.insertOISnapshot(ctx, oiSample{
				Symbol:   symbol,
				Exchange: ExchangeBinance,
				OIUSD:    contracts * premium.MarkPrice,
				Price:    premium.MarkPrice,
			}); err != nil {
				slog.Warn("oi collector: binance persist failed", "symbol", symbol, "error", err)
			}
		}

		// Bybit open interest comes from the same tickers endpoint used for
		// funding; the mark price is part of the payload.
		ticker, err := c.fetchBybitTicker(ctx, symbol)
		if err != nil {
			slog.Warn("oi collector: bybit fetch failed", "symbol", symbol, "error", err)
		} else if ticker.OpenInterest > 0 && ticker.MarkPrice > 0 {
			if err := c.insertOISnapshot(ctx, oiSample{
				Symbol:   symbol,
				Exchange: ExchangeBybit,
				OIUSD:    ticker.OpenInterest * ticker.MarkPrice,
				Price:    ticker.MarkPrice,
			}); err != nil {
				slog.Warn("oi collector: bybit persist failed", "symbol", symbol, "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Sentiment collector (every SentimentInterval, default 1h)
// ---------------------------------------------------------------------------

func (c *Collector) collectSentiment(ctx context.Context) {
	var resp fngResponse
	if err := c.getJSON(ctx, limiterKeySentiment, c.cfg.FNGEndpoint, &resp); err != nil {
		slog.Warn("sentiment collector: fetch failed", "error", err)
		return
	}
	if len(resp.Data) == 0 {
		slog.Warn("sentiment collector: empty response")
		return
	}
	entry := resp.Data[0]
	value, err := strconv.ParseFloat(entry.Value, 64)
	if err != nil {
		slog.Warn("sentiment collector: cannot parse value", "value", entry.Value, "error", err)
		return
	}
	if err := c.insertSentiment(ctx, value, entry.ValueClassification); err != nil {
		slog.Warn("sentiment collector: persist failed", "error", err)
	}
}

// ---------------------------------------------------------------------------
// Economic events collector (every EventsInterval, default 1h)
// ---------------------------------------------------------------------------

func (c *Collector) collectEconomicEvents(ctx context.Context) {
	var feed []forexFactoryEvent
	if err := c.getJSON(ctx, limiterKeyEvents, c.cfg.ForexFactoryEndpoint, &feed); err != nil {
		slog.Warn("economic events collector: fetch failed", "error", err)
		return
	}

	inserted := 0
	for _, event := range filterHighImpactEvents(feed) {
		if ctx.Err() != nil {
			return
		}
		ok, err := c.insertEconomicEvent(ctx, event.Title, event.EventTime, event.Country)
		if err != nil {
			slog.Warn("economic events collector: persist failed", "title", event.Title, "error", err)
			continue
		}
		if ok {
			inserted++
		}
	}
	if inserted > 0 {
		slog.Info("economic events collector stored new high impact events", "count", inserted)
	}
}

// economicEventRecord is a validated, de-duplication-ready event.
type economicEventRecord struct {
	Title     string
	EventTime time.Time
	Country   string
}

// filterHighImpactEvents keeps only High-impact events with a parseable
// RFC3339 date and normalizes the country code.
func filterHighImpactEvents(feed []forexFactoryEvent) []economicEventRecord {
	records := make([]economicEventRecord, 0)
	for _, event := range feed {
		if event.Impact != "High" {
			continue
		}
		eventTime, err := time.Parse(time.RFC3339, event.Date)
		if err != nil {
			continue
		}
		records = append(records, economicEventRecord{
			Title:     event.Title,
			EventTime: eventTime,
			Country:   strings.ToUpper(strings.TrimSpace(event.Country)),
		})
	}
	return records
}

// ---------------------------------------------------------------------------
// HTTP layer
// ---------------------------------------------------------------------------

// getJSON performs a rate-limited GET and decodes the JSON body into out.
func (c *Collector) getJSON(ctx context.Context, limiterKey, endpoint string, out any) error {
	limiter := c.limiters[limiterKey]
	if limiter == nil {
		return fmt.Errorf("unknown rate limiter %q", limiterKey)
	}
	if err := limiter.wait(ctx); err != nil {
		return fmt.Errorf("rate limiter: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request %s: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s returned HTTP %d: %s", limiterKey, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read body %s: %w", endpoint, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode json %s: %w", endpoint, err)
	}
	return nil
}

func joinBase(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

// ---------------------------------------------------------------------------
// Exchange response shapes
// ---------------------------------------------------------------------------

type binancePremiumIndexResponse struct {
	LastFundingRate string `json:"lastFundingRate"`
	MarkPrice       string `json:"markPrice"`
}

// binancePremiumIndexEntry is one element of the ALL-symbols premiumIndex
// array (no symbol query parameter).
type binancePremiumIndexEntry struct {
	Symbol          string `json:"symbol"`
	LastFundingRate string `json:"lastFundingRate"`
	MarkPrice       string `json:"markPrice"`
}

type binanceOpenInterestResponse struct {
	OpenInterest string `json:"openInterest"`
	Symbol       string `json:"symbol"`
}

type bybitTickerEntry struct {
	Symbol            string `json:"symbol"`
	FundingRate       string `json:"fundingRate"`
	MarkPrice         string `json:"markPrice"`
	OpenInterest      string `json:"openInterest"`
	OpenInterestValue string `json:"openInterestValue"` // USD, bulk response only
}

type bybitTickersResponse struct {
	Result struct {
		List []bybitTickerEntry `json:"list"`
	} `json:"result"`
}

type okxFundingEntry struct {
	FundingRate string `json:"fundingRate"`
	MarkPx      string `json:"markPx"`
}

type okxFundingRateResponse struct {
	Data []okxFundingEntry `json:"data"`
}

type fngEntry struct {
	Value               string `json:"value"`
	ValueClassification string `json:"value_classification"`
}

type fngResponse struct {
	Data []fngEntry `json:"data"`
}

type forexFactoryEvent struct {
	Title   string `json:"title"`
	Country string `json:"country"`
	Date    string `json:"date"`
	Impact  string `json:"impact"`
}

// bybitTickerSnapshot is the decoded Bybit ticker payload.
type bybitTickerSnapshot struct {
	FundingRate  float64
	MarkPrice    float64
	OpenInterest float64 // contracts
}

// ---------------------------------------------------------------------------
// Fetchers: fetch + parse only, no persistence (keeps them unit-testable)
// ---------------------------------------------------------------------------

// fetchBinancePremiumIndex returns the funding rate and mark price for a symbol
// from GET /fapi/v1/premiumIndex.
func (c *Collector) fetchBinancePremiumIndex(ctx context.Context, pionexSymbol string) (*fundingSample, error) {
	query := url.Values{"symbol": {ToBinanceSymbol(pionexSymbol)}}
	endpoint := joinBase(c.cfg.BinanceBaseURL, "/fapi/v1/premiumIndex") + "?" + query.Encode()

	var resp binancePremiumIndexResponse
	if err := c.getJSON(ctx, ExchangeBinance, endpoint, &resp); err != nil {
		return nil, err
	}
	rate, err := strconv.ParseFloat(resp.LastFundingRate, 64)
	if err != nil {
		return nil, fmt.Errorf("parse binance lastFundingRate %q: %w", resp.LastFundingRate, err)
	}
	sample := &fundingSample{Symbol: pionexSymbol, Exchange: ExchangeBinance, Rate: rate}
	if resp.MarkPrice != "" {
		mark, err := strconv.ParseFloat(resp.MarkPrice, 64)
		if err != nil {
			return nil, fmt.Errorf("parse binance markPrice %q: %w", resp.MarkPrice, err)
		}
		sample.MarkPrice = mark
	}
	return sample, nil
}

// fetchBinanceOpenInterest returns open interest in base-asset contracts from
// GET /fapi/v1/openInterest.
func (c *Collector) fetchBinanceOpenInterest(ctx context.Context, pionexSymbol string) (float64, error) {
	query := url.Values{"symbol": {ToBinanceSymbol(pionexSymbol)}}
	endpoint := joinBase(c.cfg.BinanceBaseURL, "/fapi/v1/openInterest") + "?" + query.Encode()

	var resp binanceOpenInterestResponse
	if err := c.getJSON(ctx, ExchangeBinance, endpoint, &resp); err != nil {
		return 0, err
	}
	contracts, err := strconv.ParseFloat(resp.OpenInterest, 64)
	if err != nil {
		return 0, fmt.Errorf("parse binance openInterest %q: %w", resp.OpenInterest, err)
	}
	return contracts, nil
}

// fetchBybitTicker returns funding, mark price and open interest from
// GET /v5/market/tickers?category=linear.
func (c *Collector) fetchBybitTicker(ctx context.Context, pionexSymbol string) (*bybitTickerSnapshot, error) {
	query := url.Values{"category": {"linear"}, "symbol": {ToBinanceSymbol(pionexSymbol)}}
	endpoint := joinBase(c.cfg.BybitBaseURL, "/v5/market/tickers") + "?" + query.Encode()

	var resp bybitTickersResponse
	if err := c.getJSON(ctx, ExchangeBybit, endpoint, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("bybit tickers: empty list for %s", pionexSymbol)
	}
	entry := resp.Result.List[0]
	snapshot := &bybitTickerSnapshot{}
	var err error
	if snapshot.FundingRate, err = strconv.ParseFloat(entry.FundingRate, 64); err != nil {
		return nil, fmt.Errorf("parse bybit fundingRate %q: %w", entry.FundingRate, err)
	}
	if snapshot.MarkPrice, err = strconv.ParseFloat(entry.MarkPrice, 64); err != nil {
		return nil, fmt.Errorf("parse bybit markPrice %q: %w", entry.MarkPrice, err)
	}
	if snapshot.OpenInterest, err = strconv.ParseFloat(entry.OpenInterest, 64); err != nil {
		return nil, fmt.Errorf("parse bybit openInterest %q: %w", entry.OpenInterest, err)
	}
	return snapshot, nil
}

// fetchOKXFunding returns the funding rate and mark price from
// GET /api/v5/public/funding-rate.
func (c *Collector) fetchOKXFunding(ctx context.Context, pionexSymbol string) (*fundingSample, error) {
	query := url.Values{"instId": {ToOKXSymbol(pionexSymbol)}}
	endpoint := joinBase(c.cfg.OKXBaseURL, "/api/v5/public/funding-rate") + "?" + query.Encode()

	var resp okxFundingRateResponse
	if err := c.getJSON(ctx, ExchangeOKX, endpoint, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("okx funding-rate: empty data for %s", pionexSymbol)
	}
	entry := resp.Data[0]
	rate, err := strconv.ParseFloat(entry.FundingRate, 64)
	if err != nil {
		return nil, fmt.Errorf("parse okx fundingRate %q: %w", entry.FundingRate, err)
	}
	sample := &fundingSample{Symbol: pionexSymbol, Exchange: ExchangeOKX, Rate: rate}
	if entry.MarkPx != "" {
		mark, err := strconv.ParseFloat(entry.MarkPx, 64)
		if err != nil {
			return nil, fmt.Errorf("parse okx markPx %q: %w", entry.MarkPx, err)
		}
		sample.MarkPrice = mark
	}
	return sample, nil
}

// fetchBinancePremiumIndexAll returns funding samples for every Binance
// perp that maps back to a Pionex symbol, in ONE request (GET /fapi/v1/premiumIndex
// without the symbol parameter returns the full array).
func (c *Collector) fetchBinancePremiumIndexAll(ctx context.Context) ([]fundingSample, error) {
	aliases := c.aliasesSnapshot()
	endpoint := joinBase(c.cfg.BinanceBaseURL, "/fapi/v1/premiumIndex")

	var resp []binancePremiumIndexEntry
	if err := c.getJSON(ctx, ExchangeBinance, endpoint, &resp); err != nil {
		return nil, err
	}
	samples := make([]fundingSample, 0, len(resp))
	for _, e := range resp {
		pionexSymbol, ok := aliases[e.Symbol]
		if !ok {
			continue
		}
		rate, err := strconv.ParseFloat(e.LastFundingRate, 64)
		if err != nil {
			continue
		}
		s := fundingSample{Symbol: pionexSymbol, Exchange: ExchangeBinance, Rate: rate}
		if mark, err := strconv.ParseFloat(e.MarkPrice, 64); err == nil {
			s.MarkPrice = mark
		}
		samples = append(samples, s)
	}
	return samples, nil
}

// fetchBybitTickersAll returns funding AND USD open-interest samples for
// every Bybit linear contract that maps back to a Pionex symbol, in ONE
// request (GET /v5/market/tickers?category=linear without the symbol
// parameter returns the full list).
func (c *Collector) fetchBybitTickersAll(ctx context.Context) ([]fundingSample, []oiSample, error) {
	aliases := c.aliasesSnapshot()
	endpoint := joinBase(c.cfg.BybitBaseURL, "/v5/market/tickers") + "?category=linear"

	var resp bybitTickersResponse
	if err := c.getJSON(ctx, ExchangeBybit, endpoint, &resp); err != nil {
		return nil, nil, err
	}
	funding := make([]fundingSample, 0, len(resp.Result.List))
	oi := make([]oiSample, 0, len(resp.Result.List))
	for _, e := range resp.Result.List {
		pionexSymbol, ok := aliases[e.Symbol]
		if !ok {
			continue
		}
		mark, _ := strconv.ParseFloat(e.MarkPrice, 64)
		if rate, err := strconv.ParseFloat(e.FundingRate, 64); err == nil {
			funding = append(funding, fundingSample{
				Symbol: pionexSymbol, Exchange: ExchangeBybit, Rate: rate, MarkPrice: mark,
			})
		}
		if v, err := strconv.ParseFloat(e.OpenInterestValue, 64); err == nil && v > 0 {
			oi = append(oi, oiSample{
				Symbol: pionexSymbol, Exchange: ExchangeBybit, OIUSD: v, Price: mark,
			})
		}
	}
	return funding, oi, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (c *Collector) insertFundingSnapshot(ctx context.Context, sample fundingSample) error {
	var markPrice any
	if sample.MarkPrice > 0 {
		markPrice = sample.MarkPrice
	}
	_, err := c.db.Exec(ctx, `
		INSERT INTO funding_snapshots (symbol, exchange, funding_rate, mark_price)
		VALUES ($1, $2, $3, $4)
	`, sample.Symbol, sample.Exchange, sample.Rate, markPrice)
	if err != nil {
		return fmt.Errorf("insert funding snapshot %s/%s: %w", sample.Symbol, sample.Exchange, err)
	}
	return nil
}

// insertFundingSamples persists a bulk cycle (~800 rows at full market
// coverage) in one batch instead of one round-trip per row.
func (c *Collector) insertFundingSamples(ctx context.Context, samples []fundingSample) error {
	if len(samples) == 0 || c.db == nil {
		return nil
	}
	batch := &pgx.Batch{}
	for _, s := range samples {
		var markPrice any
		if s.MarkPrice > 0 {
			markPrice = s.MarkPrice
		}
		batch.Queue(`INSERT INTO funding_snapshots (symbol, exchange, funding_rate, mark_price) VALUES ($1, $2, $3, $4)`,
			s.Symbol, s.Exchange, s.Rate, markPrice)
	}
	if err := c.db.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("insert %d funding snapshots: %w", len(samples), err)
	}
	return nil
}

// insertOISamples persists a bulk OI cycle in one batch.
func (c *Collector) insertOISamples(ctx context.Context, samples []oiSample) error {
	if len(samples) == 0 || c.db == nil {
		return nil
	}
	batch := &pgx.Batch{}
	for _, s := range samples {
		batch.Queue(`INSERT INTO oi_history (symbol, exchange, oi_usd, price) VALUES ($1, $2, $3, $4)`,
			s.Symbol, s.Exchange, s.OIUSD, s.Price)
	}
	if err := c.db.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("insert %d oi snapshots: %w", len(samples), err)
	}
	return nil
}

// Retention windows: funding is only ever queried within a 10-minute window
// and OI within 24 hours, so anything older is dead weight. Liquidation
// events share the OI window (the summary reads the trailing hour).
const (
	fundingRetention = 48 * time.Hour
	oiRetention      = 8 * 24 * time.Hour
	retentionBatch   = 50_000
)

// collectRetention prunes the smart-data tables in bounded batches so the
// BRIN-indexed deletes never lock the tables for long.
func (c *Collector) collectRetention(ctx context.Context) {
	for _, table := range []struct {
		name string
		keep time.Duration
	}{
		{"funding_snapshots", fundingRetention},
		{"oi_history", oiRetention},
		{"liquidation_events", oiRetention},
	} {
		c.retentionDelete(ctx, table.name, table.keep)
	}
}

func (c *Collector) retentionDelete(ctx context.Context, table string, keep time.Duration) {
	if c.db == nil {
		return
	}
	deleted := int64(0)
	for {
		if ctx.Err() != nil {
			return
		}
		tag, err := c.db.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE ctid IN (SELECT ctid FROM %s WHERE captured_at < NOW() - $1::INTERVAL LIMIT %d)`,
			table, table, retentionBatch,
		), keep.String())
		if err != nil {
			slog.Warn("retention delete failed", "table", table, "error", err)
			return
		}
		deleted += tag.RowsAffected()
		if tag.RowsAffected() < int64(retentionBatch) {
			break
		}
	}
	if deleted > 0 {
		slog.Info("retention pruned old rows", "table", table, "deleted", deleted)
	}
}

func (c *Collector) insertOISnapshot(ctx context.Context, sample oiSample) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO oi_history (symbol, exchange, oi_usd, price)
		VALUES ($1, $2, $3, $4)
	`, sample.Symbol, sample.Exchange, sample.OIUSD, sample.Price)
	if err != nil {
		return fmt.Errorf("insert oi snapshot %s/%s: %w", sample.Symbol, sample.Exchange, err)
	}
	return nil
}

func (c *Collector) insertSentiment(ctx context.Context, value float64, classification string) error {
	_, err := c.db.Exec(ctx, `
		INSERT INTO sentiment_snapshots (source, value, classification)
		VALUES ('fng', $1, $2)
	`, value, classification)
	if err != nil {
		return fmt.Errorf("insert sentiment snapshot: %w", err)
	}
	return nil
}

// insertEconomicEvent stores a High-impact event, de-duplicating on
// (title, event_time). It reports whether a new row was written.
// Explicit ::VARCHAR/::TIMESTAMPTZ casts are required: the parameters appear
// both as bare SELECT items (unknown type) and inside the NOT EXISTS
// comparison, which PostgreSQL rejects with SQLSTATE 42P08 "inconsistent
// types deduced for parameter" when left to inference.
func (c *Collector) insertEconomicEvent(ctx context.Context, title string, eventTime time.Time, country string) (bool, error) {
	tag, err := c.db.Exec(ctx, `
		INSERT INTO economic_events (title, event_time, impact, country)
		SELECT $1::VARCHAR(255), $2::TIMESTAMPTZ, 'High', NULLIF($3::VARCHAR(8), '')
		WHERE NOT EXISTS (
			SELECT 1 FROM economic_events
			WHERE title = $1::VARCHAR(255) AND event_time = $2::TIMESTAMPTZ
		)
	`, title, eventTime, country)
	if err != nil {
		return false, fmt.Errorf("insert economic event %q: %w", title, err)
	}
	return tag.RowsAffected() > 0, nil
}
