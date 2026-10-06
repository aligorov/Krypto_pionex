package autogrid

// Model-vs-REAL calibration harness (offline, opt-in).
//
// Replays the EXACT paper-model core the shadow portfolio runs
// (neutralGridPaperPNL ladder + decideBotAction per 5M candle — see
// simulateShadowPortfolio in shadow_portfolio.go) over the public Pionex
// 5-minute klines of each CLOSED REAL grid bot from
// testdata/calib_real_history.json, and compares the model outcome PnL with
// the exchange-settled realized_pnl_usdt recorded at close.
//
// The question this answers: WHERE does the paper model lie relative to the
// exchange — stops, grid harvesting, or both (per close-reason class MAE /
// mean delta / sign agreement).
//
// Guarded: skipped unless CALIB_HISTORY is set, so prod suites are
// unaffected. Network: public /api/v1/market/klines only (weight 5,
// documented params: symbol, interval, endTime inclusive, limit<=500,
// newest-first). Symbol format is the bot's own _USDT_PERP record value —
// the same format every worker.publicClient.GetKlines call site uses.
//
// Deliberate simplifications vs the manage loop (mirroring the shadow
// module's own list): no tranche-2, no re-centers, regime unknown
// (adverse close on break), funding NOT accrued (no funding_snapshots
// offline; runtime default 10 bps/8h on inventory — negligible at the
// median 20h bot life), no entry/exit fee booking in the replay (the
// aggregate paperEntryFee figure is reported separately in the log).

import (
	"math"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Input rows (testdata/calib_real_history.json — grid_bots export).
// ---------------------------------------------------------------------------

type calibBotRow struct {
	BotNumber     int              `json:"bot_number"`
	Symbol        string           `json:"symbol"`
	Direction     string           `json:"direction"`
	GridNum       int              `json:"grid_num"`
	Leverage      int              `json:"leverage"`
	LowerPrice    decimal.Decimal  `json:"-"`
	UpperPrice    decimal.Decimal  `json:"-"`
	QuoteInvest   decimal.Decimal  `json:"-"`
	RealizedPNL   *decimal.Decimal `json:"-"`
	PnLTarget     decimal.Decimal  `json:"-"`
	MaxLoss       decimal.Decimal  `json:"-"`
	StopLossPrice *decimal.Decimal `json:"-"`
	CreatedAt     time.Time        `json:"created_at"`
	ClosedAt      time.Time        `json:"closed_at"`
	ClosedReason  string           `json:"closed_reason"`
}

// UnmarshalJSON keeps decimal fidelity (json.Number → decimal) instead of
// float round-trips.
func (r *calibBotRow) UnmarshalJSON(data []byte) error {
	type alias calibBotRow
	var raw struct {
		alias
		LowerPrice    json.Number `json:"lower_price"`
		UpperPrice    json.Number `json:"upper_price"`
		QuoteInvest   json.Number `json:"quote_investment"`
		RealizedPNL   json.Number `json:"realized_pnl_usdt"`
		PnLTarget     json.Number `json:"pnl_target_usdt"`
		MaxLoss       json.Number `json:"max_loss_usdt"`
		StopLossPrice json.Number `json:"stop_loss_price"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = calibBotRow(raw.alias)
	num := func(n json.Number) decimal.Decimal {
		d, err := decimal.NewFromString(n.String())
		if err != nil {
			return decimal.Zero
		}
		return d
	}
	numPtr := func(n json.Number) *decimal.Decimal {
		if n.String() == "" || n.String() == "null" {
			return nil
		}
		d := num(n)
		return &d
	}
	r.LowerPrice = num(raw.LowerPrice)
	r.UpperPrice = num(raw.UpperPrice)
	r.QuoteInvest = num(raw.QuoteInvest)
	r.RealizedPNL = numPtr(raw.RealizedPNL)
	r.PnLTarget = num(raw.PnLTarget)
	r.MaxLoss = num(raw.MaxLoss)
	r.StopLossPrice = numPtr(raw.StopLossPrice)
	return nil
}

// ---------------------------------------------------------------------------
// Public klines fetch (direct HTTP — the pionex client wrapper carries no
// endTime parameter and lives on the Worker; AGENTS.md forbids touching
// prod code for this harness).
// ---------------------------------------------------------------------------

const (
	calibBaseURL      = "https://api.pionex.com"
	calibInterval     = "5M"
	calibKlineLimit   = 500 // documented hard ceiling (live-probed in shadow_portfolio.go)
	calibMaxPages     = 10  // 5000 candles ≈ 17 days per symbol union window
	calibRequestPause = 150 * time.Millisecond
	calibHTTPTimeout  = 20 * time.Second
)

type calibCandle struct {
	Time  int64 // open time, ms
	Open  decimal.Decimal
	High  decimal.Decimal
	Low   decimal.Decimal
	Close decimal.Decimal
}

type calibKlinesHTTPClient struct {
	client *http.Client
	reqs   int
}

func (c *calibKlinesHTTPClient) getKlines(ctx context.Context, symbol string, endTimeMs int64) ([]calibCandle, error) {
	q := url.Values{
		"symbol":   {symbol},
		"interval": {calibInterval},
		"endTime":  {strconv.FormatInt(endTimeMs, 10)},
		"limit":    {strconv.Itoa(calibKlineLimit)},
	}
	reqURL := calibBaseURL + "/api/v1/market/klines?" + q.Encode()
	var body []byte
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if c.reqs > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(calibRequestPause):
			}
		}
		c.reqs++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 3 {
				continue
			}
			return nil, fmt.Errorf("klines %s: %w", symbol, err)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			if attempt < 3 {
				continue
			}
			return nil, fmt.Errorf("klines %s read: %w", symbol, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("klines %s: 429 rate limited", symbol)
			time.Sleep(2 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("klines %s: HTTP %d: %s", symbol, resp.StatusCode, string(body[:min(len(body), 200)]))
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}
	var envelope struct {
		Result  bool   `json:"result"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Klines []struct {
				Time   json.Number `json:"time"`
				Open   json.Number `json:"open"`
				High   json.Number `json:"high"`
				Low    json.Number `json:"low"`
				Close  json.Number `json:"close"`
				Volume json.Number `json:"volume"`
			} `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("klines %s decode: %w (body %s)", symbol, err, string(body[:min(len(body), 200)]))
	}
	if !envelope.Result {
		return nil, fmt.Errorf("klines %s: API error [%s]: %s", symbol, envelope.Code, envelope.Message)
	}
	out := make([]calibCandle, 0, len(envelope.Data.Klines))
	for _, k := range envelope.Data.Klines {
		ms, err := strconv.ParseInt(strings.TrimSpace(k.Time.String()), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("klines %s time %q: %w", symbol, k.Time.String(), err)
		}
		dec := func(n json.Number) decimal.Decimal {
			d, err := decimal.NewFromString(strings.TrimSpace(n.String()))
			if err != nil {
				return decimal.Zero
			}
			return d
		}
		out = append(out, calibCandle{Time: ms, Open: dec(k.Open), High: dec(k.High), Low: dec(k.Low), Close: dec(k.Close)})
	}
	return out, nil
}

// calibFetchWindow pulls the union window [start, end] for one symbol with
// backwards endTime pagination (the endpoint documents endTime inclusive,
// limit<=500, newest-first; there is no startTime parameter).
func calibFetchWindow(ctx context.Context, hc *calibKlinesHTTPClient, symbol string, start, end time.Time) ([]calibCandle, error) {
	endTime := end.UnixMilli()
	startMs := start.UnixMilli()
	seen := make(map[int64]bool, 1024)
	all := make([]calibCandle, 0, 1024)
	for page := 0; page < calibMaxPages; page++ {
		candles, err := hc.getKlines(ctx, symbol, endTime)
		if err != nil {
			return nil, err
		}
		if len(candles) == 0 {
			break
		}
		oldest := candles[0].Time
		for _, c := range candles {
			if c.Time < oldest {
				oldest = c.Time
			}
			if !seen[c.Time] {
				seen[c.Time] = true
				all = append(all, c)
			}
		}
		if oldest <= startMs {
			break
		}
		endTime = oldest - 1
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Time < all[j].Time })
	return all, nil
}

// ---------------------------------------------------------------------------
// Model replay — a faithful copy of the simulateShadowPortfolio loop with
// the REAL record's own direction/targets/stop.
// ---------------------------------------------------------------------------

type calibReplayResult struct {
	ModelPNL    decimal.Decimal
	ModelReson  string
	CandlesUsed int
	EntryPrice  decimal.Decimal
}

func calibReplayBot(bot calibBotRow, candles []calibCandle, rangeBreakBuffer decimal.Decimal) (calibReplayResult, error) {
	direction := strings.ToUpper(strings.TrimSpace(bot.Direction))
	if direction == "" {
		direction = "NEUTRAL"
	}
	lower, upper := bot.LowerPrice, bot.UpperPrice
	gridNum, leverage := bot.GridNum, bot.Leverage
	investment := bot.QuoteInvest
	if !investment.IsPositive() || leverage < 1 {
		return calibReplayResult{}, fmt.Errorf("degenerate investment/leverage")
	}
	if !upper.GreaterThan(lower) || gridNum < 2 {
		return calibReplayResult{}, fmt.Errorf("degenerate geometry")
	}

	// Slice the bot's own life window: candles opening at/after created_at,
	// no later than closed_at (the shadow loop's Before/After semantics).
	startMs := bot.CreatedAt.UnixMilli()
	endMs := bot.ClosedAt.UnixMilli()
	life := make([]calibCandle, 0, len(candles))
	for _, c := range candles {
		if c.Time < startMs {
			continue
		}
		if c.Time > endMs {
			break
		}
		life = append(life, c)
	}
	if len(life) == 0 {
		return calibReplayResult{}, fmt.Errorf("no candles in life window")
	}
	entry := life[0].Close
	if !entry.GreaterThan(decimal.Zero) {
		return calibReplayResult{}, fmt.Errorf("zero entry price")
	}

	// Anti-hunt / card stop: the record's stop_loss_price when the bot
	// carried one (intrabar breach semantics — SHORT swept above, everything
	// else dipped below → revalue AT the stop), else the shadow module's
	// deploy-time computation (atrPct default 2.0, 1.5x mult) clamped into
	// bounds the way the REAL deploy path clamps it.
	antiHunt := decimal.Zero
	if bot.StopLossPrice != nil && bot.StopLossPrice.GreaterThan(decimal.Zero) {
		antiHunt = *bot.StopLossPrice
	} else {
		atrPrice := entry.Mul(decimal.NewFromFloat(0.02))
		antiHunt = ComputeAntiHuntStop(direction, lower, upper, entry, atrPrice, 1.5)
		antiHunt = ClampAntiHuntStopIntoBounds(direction, lower, upper, antiHunt)
	}

	realized := decimal.Zero
	lastLevel := gridLevelForPrice(lower, upper, gridNum, entry)
	peak := decimal.Zero
	total := decimal.Zero
	outcomeReason := "WINDOW_END_AT_CLOSED_AT"
	used := 0
	prevInvNotional := decimal.Zero
	exitCostBooked := false
	fundingLast := bot.CreatedAt
	openedAt := bot.CreatedAt
	worstLevel := gridNum - 1

	for _, c := range life {
		used++
		candleTime := time.UnixMilli(c.Time)

		// v2.0.190 calibration semantics — mirror of the shadow core:
		// directional stops breach on CLOSE (mark), NEUTRAL keeps the
		// pessimistic intrabar wick; entry fees on ladder build; 30-minute
		// inventory ramp for fresh grids; one-shot taker+slippage exit cost.
		breached := false
		if antiHunt.GreaterThan(decimal.Zero) {
			if direction == "SHORT" {
				breached = c.Close.GreaterThanOrEqual(antiHunt)
			} else if direction == "LONG" {
				breached = c.Close.LessThanOrEqual(antiHunt)
			} else {
				breached = c.Low.LessThanOrEqual(antiHunt)
			}
		}
		closePrice := c.Close
		if breached {
			closePrice = antiHunt
		}

		unrealized := decimal.Zero
		exposure := decimal.Zero
		notional := investment.Mul(decimal.NewFromInt(int64(leverage)))
		switch direction {
		case "LONG", "SHORT":
			if direction == "LONG" {
				unrealized = notional.Mul(closePrice.Div(entry).Sub(decimal.NewFromInt(1)))
			} else {
				unrealized = notional.Mul(decimal.NewFromInt(1).Sub(closePrice.Div(entry)))
			}
			exposure = notional
		default: // NEUTRAL — the paper ladder
			level := gridLevelForPrice(lower, upper, gridNum, closePrice)
			if level < worstLevel {
				worstLevel = level
			}
			pairProfit, uninv, invNotional := neutralGridPaperPNLWithExtreme(
				lower, upper, gridNum, investment, leverage, lastLevel, level, worstLevel, closePrice,
				decimal.NewFromFloat(pionexMakerFeeBps))
			ageMin := candleTime.Sub(openedAt).Minutes()
			if ageMin < 30 {
				ramp := decimal.NewFromFloat(math.Max(0, ageMin) / 30.0)
				uninv = uninv.Mul(ramp)
				invNotional = invNotional.Mul(ramp)
			}
			if invNotional.GreaterThan(prevInvNotional) {
				feeEntry := invNotional.Sub(prevInvNotional).
					Mul(decimal.NewFromFloat(pionexMakerFeeBps)).Div(decimal.NewFromInt(10000))
				realized = realized.Sub(feeEntry)
			}
			prevInvNotional = invNotional
			realized = realized.Add(pairProfit)
			unrealized = uninv
			lastLevel = level
			exposure = invNotional
		}
		// funding accrual: market norm 10 bps/8h on exposure (v2.0.190)
		if exposure.GreaterThan(decimal.Zero) && candleTime.Sub(fundingLast).Hours() >= 8 {
			steps := math.Floor(candleTime.Sub(fundingLast).Hours() / 8)
			if steps > 0 {
				realized = realized.Sub(exposure.Mul(decimal.NewFromInt(10)).
					Div(decimal.NewFromInt(10000)).Mul(decimal.NewFromInt(int64(steps))))
				fundingLast = fundingLast.Add(time.Duration(steps * 8 * float64(time.Hour)))
			}
		}
		if breached && exposure.GreaterThan(decimal.Zero) && !exitCostBooked {
			realized = realized.Sub(exposure.Mul(decimal.NewFromFloat(0.0010)))
			exitCostBooked = true
		}
		_ = candleTime

		total = realized.Add(unrealized)
		if total.GreaterThan(peak) {
			peak = total
		}

		decision := decideBotAction(botActionInput{
			Direction:        direction,
			Lower:            lower,
			Upper:            upper,
			CurrentPrice:     closePrice,
			RealizedPNL:      realized,
			UnrealizedPNL:    unrealized,
			PeakPNL:          peak,
			Budget:           investment,
			PnLTarget:        bot.PnLTarget,
			MaxLoss:          bot.MaxLoss,
			RangeBreakBuffer: rangeBreakBuffer,
			AdjustmentsLeft:  0, // shadow = no-skill baseline: breaks close, never re-center
			Regime:           "",
			AntiHuntStop:     &antiHunt,
		})
		if strings.HasPrefix(decision.Action, "CLOSE") || breached {
			if breached {
				outcomeReason = "STRUCT_INVALID_STOP"
			} else {
				outcomeReason = decision.Reason
			}
			break
		}
	}
	return calibReplayResult{ModelPNL: total, ModelReson: outcomeReason, CandlesUsed: used, EntryPrice: entry}, nil
}

// ---------------------------------------------------------------------------
// Aggregation.
// ---------------------------------------------------------------------------

func calibReasonClass(reason string) string {
	switch {
	case strings.HasPrefix(reason, "STOP_LOSS"):
		return "STOP_LOSS*"
	case strings.HasPrefix(reason, "RANGE_BREAK"):
		return "RANGE_BREAK*"
	case reason == "GRID_AGED_HALF_LIFE":
		return "AGED"
	case strings.HasPrefix(reason, "TAKE_PROFIT") || reason == "TRAILING_TAKE_PROFIT":
		return "TP"
	default:
		return "OTHER"
	}
}

func calibF(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

func TestModelVsRealCalibration(t *testing.T) {
	if os.Getenv("CALIB_HISTORY") == "" {
		t.Skip("CALIB_HISTORY not set — model-vs-real calibration skipped (needs network for public 5M klines)")
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "calib_real_history.json"))
	if err != nil {
		t.Fatalf("read calib_real_history.json: %v", err)
	}
	var rows []calibBotRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode calib_real_history.json: %v", err)
	}
	// realized NULL → the exchange never settled the bot: not calibratable.
	bots := make([]calibBotRow, 0, len(rows))
	skippedNull := 0
	for _, r := range rows {
		if r.RealizedPNL == nil {
			skippedNull++
			continue
		}
		bots = append(bots, r)
	}
	t.Logf("loaded %d closed bots (%d skipped: realized NULL), window %s .. %s",
		len(bots), skippedNull,
		calibMinCreated(bots).Format(time.RFC3339), calibMaxClosed(bots).Format(time.RFC3339))

	// Runtime parity inputs (autogrid_settings at export time): the range
	// break buffer the manage loop actually supervises with.
	rangeBreakBuffer := decimal.NewFromFloat(2.0)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	hc := &calibKlinesHTTPClient{client: &http.Client{Timeout: calibHTTPTimeout}}

	// One union-window fetch per symbol (bots overlap heavily on symbols).
	bySymbol := make(map[string][]calibBotRow)
	for _, b := range bots {
		bySymbol[b.Symbol] = append(bySymbol[b.Symbol], b)
	}
	windows := make(map[string][]calibCandle, len(bySymbol))
	fetchFailures := 0
	for symbol, group := range bySymbol {
		start, end := calibMinCreated(group), calibMaxClosed(group)
		candles, err := calibFetchWindow(ctx, hc, symbol, start, end)
		if err != nil {
			fetchFailures++
			t.Errorf("klines fetch %s: %v", symbol, err)
			continue
		}
		windows[symbol] = candles
	}
	if fetchFailures > 0 {
		t.Fatalf("%d/%d symbol window fetches failed — refusing to calibrate on partial data", fetchFailures, len(bySymbol))
	}
	t.Logf("klines: %d HTTP requests for %d symbols", hc.reqs, len(bySymbol))

	type resultRow struct {
		bot         calibBotRow
		real        decimal.Decimal
		model       decimal.Decimal
		delta       decimal.Decimal
		hours       float64
		modelReason string
	}
	results := make([]resultRow, 0, len(bots))
	replayFailures := 0
	for _, b := range bots {
		res, err := calibReplayBot(b, windows[b.Symbol], rangeBreakBuffer)
		if err != nil {
			replayFailures++
			t.Logf("bot #%d %s: replay skipped: %v", b.BotNumber, b.Symbol, err)
			continue
		}
		results = append(results, resultRow{
			bot:         b,
			real:        *b.RealizedPNL,
			model:       res.ModelPNL,
			delta:       res.ModelPNL.Sub(*b.RealizedPNL),
			hours:       b.ClosedAt.Sub(b.CreatedAt).Hours(),
			modelReason: res.ModelReson,
		})
	}
	if len(results) == 0 {
		t.Fatalf("no calibrated bots")
	}

	// --- CSV: docs/model-vs-real-calibration.csv ---
	csvPath := os.Getenv("CALIB_CSV_PATH")
	if csvPath == "" {
		abs, err := filepath.Abs(filepath.Join("..", "..", "..", "docs", "model-vs-real-calibration.csv"))
		if err != nil {
			t.Fatalf("csv path: %v", err)
		}
		csvPath = abs
	}
	if err := os.MkdirAll(filepath.Dir(csvPath), 0o755); err != nil {
		t.Fatalf("mkdir csv dir: %v", err)
	}
	f, err := os.Create(csvPath)
	if err != nil {
		t.Fatalf("create csv: %v", err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"bot_number", "symbol", "direction", "reason", "real_pnl", "model_pnl", "delta", "hours", "model_reason"})
	for _, r := range results {
		_ = w.Write([]string{
			strconv.Itoa(r.bot.BotNumber),
			r.bot.Symbol,
			r.bot.Direction,
			r.bot.ClosedReason,
			r.real.StringFixed(4),
			r.model.StringFixed(4),
			r.delta.StringFixed(4),
			calibF(r.hours),
			r.modelReason,
		})
	}
	w.Flush()
	_ = f.Close()
	if err := w.Error(); err != nil {
		t.Fatalf("csv write: %v", err)
	}
	t.Logf("csv written: %s", csvPath)

	// --- Summary statistics ---
	var sumAbs, sumDelta, sumReal, sumModel float64
	signAgree, n := 0.0, 0.0
	classStats := make(map[string]*struct {
		n         int
		sumAbs    float64
		sumDelta  float64
		signAgree int
	})
	entryFeeSum := decimal.Zero
	for _, r := range results {
		df, _ := r.delta.Float64()
		rf, _ := r.real.Float64()
		mf, _ := r.model.Float64()
		absD := df
		if absD < 0 {
			absD = -absD
		}
		sumAbs += absD
		sumDelta += df
		sumReal += rf
		sumModel += mf
		if (rf >= 0) == (mf >= 0) {
			signAgree++
		}
		n++
		class := calibReasonClass(r.bot.ClosedReason)
		cs := classStats[class]
		if cs == nil {
			cs = &struct {
				n         int
				sumAbs    float64
				sumDelta  float64
				signAgree int
			}{}
			classStats[class] = cs
		}
		cs.n++
		cs.sumAbs += absD
		cs.sumDelta += df
		if (rf >= 0) == (mf >= 0) {
			cs.signAgree++
		}
		// Diagnostic (not part of the replay): the entry fee the LIVE paper
		// engine books at deploy — quantifies one known model gap.
		entryFeeSum = entryFeeSum.Add(paperEntryFee(
			r.bot.Direction, r.bot.LowerPrice, r.bot.UpperPrice, r.bot.GridNum,
			r.bot.QuoteInvest, r.bot.Leverage, decimal.Zero))
	}

	t.Logf("=== MODEL vs REAL calibration (n=%d, replay-skipped=%d) ===", int(n), replayFailures)
	t.Logf("MAE(all)      = %.4f USDT", sumAbs/n)
	t.Logf("mean delta    = %+.4f USDT (model − real; positive = model too optimistic)", sumDelta/n)
	t.Logf("sign match    = %.1f%%", 100*signAgree/n)
	t.Logf("sum real      = %+.4f USDT | sum model = %+.4f USDT", sumReal, sumModel)
	t.Logf("entry-fee gap = %s USDT total (what the live paper engine books at deploy, replay doesn't)", entryFeeSum.StringFixed(4))

	t.Logf("--- per close-reason class ---")
	for _, class := range []string{"STOP_LOSS*", "RANGE_BREAK*", "AGED", "TP", "OTHER"} {
		cs, ok := classStats[class]
		if !ok {
			continue
		}
		t.Logf("%-14s n=%-3d MAE=%.4f meanDelta=%+.4f signMatch=%.0f%%",
			class, cs.n, cs.sumAbs/float64(cs.n), cs.sumDelta/float64(cs.n),
			100*float64(cs.signAgree)/float64(cs.n))
	}

	// --- Top-10 divergences by |delta| ---
	sorted := make([]resultRow, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool {
		di, _ := sorted[i].delta.Abs().Float64()
		dj, _ := sorted[j].delta.Abs().Float64()
		return di > dj
	})
	t.Logf("--- top-10 divergences (|delta|) ---")
	for i, r := range sorted {
		if i >= 10 {
			break
		}
		t.Logf("#%d bot=%d %-18s %-7s real=%+9.4f model=%+9.4f delta=%+9.4f hours=%.1f realReason=%s modelReason=%s",
			i+1, r.bot.BotNumber, r.bot.Symbol, r.bot.Direction,
			mustF(r.real), mustF(r.model), mustF(r.delta), r.hours,
			r.bot.ClosedReason, r.modelReason)
	}

	// --- Model exit-class distribution (where the model cuts vs reality) ---
	modelClass := make(map[string]int)
	for _, r := range results {
		modelClass[calibReasonClass(r.modelReason)]++
	}
	t.Logf("--- model exit classes (n=%d) ---", len(results))
	for _, k := range sortedKeys(modelClass) {
		t.Logf("%-14s %d", k, modelClass[k])
	}

	// --- Raw model exit reasons (STRUCT_INVALID_STOP = the replay's
	// intrabar stop-breach exit, which the class bucket above hides) ---
	rawReasons := make(map[string]int)
	for _, r := range results {
		rawReasons[r.modelReason]++
	}
	t.Logf("--- model exit reasons (raw) ---")
	for _, k := range sortedKeys(rawReasons) {
		t.Logf("%-28s %d", k, rawReasons[k])
	}

	// --- Per-direction split (NEUTRAL ladder vs directional marks) ---
	dirStats := make(map[string]*struct {
		n        int
		sumAbs   float64
		sumDelta float64
		sumReal  float64
	})
	for _, r := range results {
		key := r.bot.Direction
		if key != "NEUTRAL" {
			key = "DIRECTIONAL"
		}
		ds := dirStats[key]
		if ds == nil {
			ds = &struct {
				n        int
				sumAbs   float64
				sumDelta float64
				sumReal  float64
			}{}
			dirStats[key] = ds
		}
		df, _ := r.delta.Float64()
		rf, _ := r.real.Float64()
		absD := df
		if absD < 0 {
			absD = -absD
		}
		ds.n++
		ds.sumAbs += absD
		ds.sumDelta += df
		ds.sumReal += rf
	}
	dirKeys := make([]string, 0, len(dirStats))
	for k := range dirStats {
		dirKeys = append(dirKeys, k)
	}
	sort.Strings(dirKeys)
	t.Logf("--- per direction ---")
	for _, k := range dirKeys {
		ds := dirStats[k]
		t.Logf("%-12s n=%-3d MAE=%.4f meanDelta=%+.4f sumReal=%+.4f",
			k, ds.n, ds.sumAbs/float64(ds.n), ds.sumDelta/float64(ds.n), ds.sumReal)
	}
}

func calibMinCreated(bots []calibBotRow) time.Time {
	min := time.Time{}
	for _, b := range bots {
		if min.IsZero() || b.CreatedAt.Before(min) {
			min = b.CreatedAt
		}
	}
	return min
}

func calibMaxClosed(bots []calibBotRow) time.Time {
	max := time.Time{}
	for _, b := range bots {
		if b.ClosedAt.After(max) {
			max = b.ClosedAt
		}
	}
	return max
}

func mustF(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
