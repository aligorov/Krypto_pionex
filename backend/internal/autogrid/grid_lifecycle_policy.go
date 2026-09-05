package autogrid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/grid"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Grid lifecycle policy (v2.0.89 part B) — the two research-driven arms the
// manage loops now carry, symmetrically for PAPER and REAL:
//
//  1. DGT break re-deploy (arXiv 2506.11921). The in-place ShouldResetGrid
//     shift handles a break while the adjustment budget lasts; the close
//     paths below fire when it does not (budget exhausted, adverse regime).
//     Closing there used to free the slot for the scanner — which is then
//     held out by the per-symbol protective-close cooldown, so the fleet sat
//     flat through exactly the trending tape DGT says a follower grid must
//     ride. The re-deploy closes AND re-opens in the same manage pass: same
//     symbol, same slot capital, fresh tranche-1 contract, center at the
//     BREAK price, geometry from fresh data (HAR fit when available, ATR
//     stub otherwise). It is a RE-CENTER, not a re-entry: the cooldown gate
//     (v2.0.28) exists to stop re-entry into the SAME place that killed a
//     bot — the redeployed grid never traded the new geometry — so the
//     deploy paths' cooldown is deliberately not consulted here. The
//     economic/macro/portfolio gates, the kill switch and the slot budget
//     still run, and a durable per-symbol 24h ladder cap bounds the runaway
//     case (a tape that keeps breaking pays for a new grid each time; six
//     fees a day is the documented ceiling).
//
//  2. OU half-life age rotation. A mean-reverting price series is an
//     Ornstein-Uhlenbeck process; its AR(1) slope b (regress Δp on p) gives
//     the reversion rate and the half-life HL = -ln(2)/ln(1+b) steps. A grid
//     earns while the range holds; after ~2 half-lives the range the mesh
//     was fitted to has statistically decayed, so a bot older than
//     clamp(2×HL, 4h, 48h) is rotated out at market (GRID_AGED_HALF_LIFE)
//     and the slot returns to the SCANNER (plain recycle — deliberately not
//     a DGT re-deploy, the aged thesis says nothing about where to recenter).
//     Non-mean-reverting tapes (b ≥ 0, trending) have no finite HL and age
//     out at the 48h ceiling.

const (
	// dgtRedeployEvent is the durable/telegram event type both fleets write
	// on a successful re-deploy (the runaway ladder below counts them).
	dgtRedeployEvent = "DGT_REDEPLOY"
	// dgtRedeployMaxPerSymbolPer24h caps the re-center ladder: a market that
	// keeps breaking the fresh grid pays a create + a break fee per rung;
	// beyond six in 24h the tape is a trend the scanner should own, not the
	// follower.
	dgtRedeployMaxPerSymbolPer24h = 6

	// gridAgedHalfLifeReason is the close reason (and event type) of the OU
	// half-life rotation. It is listed in protectiveCloseExemptReasons: a
	// planned thesis-expiry exit must neither arm the per-symbol cooldown
	// (the scanner may legitimately want the SAME symbol back immediately)
	// nor feed the portfolio circuit breaker.
	gridAgedHalfLifeReason = "GRID_AGED_HALF_LIFE"

	// ouKlineCacheTTL bounds the per-symbol 15m klines fetch the OU reading
	// and the DGT fresh-ATR share: half-life is a slow statistic, 30 minutes
	// of staleness costs nothing against one HTTP batch per manage pass.
	ouKlineCacheTTL = 30 * time.Minute
	// ouMinCandles is the fewest closes the AR(1) fit accepts; fewer means
	// "no reading" (fail-open: no age rotation, no fresh ATR).
	ouMinCandles = 20

	// OU age clamps: max_grid_age = clamp(2×HL, 4h, 48h).
	ouMaxAgeFloorHours = 4.0
	ouMaxAgeCeilHours  = 48.0
	// ouHalfLifeMultiple is how many half-lives a grid thesis is allowed to
	// outlive its fitted range.
	ouHalfLifeMultiple = 2.0
)

// dgtBreakRedeployReason reports whether a manage close reason is a range
// break the DGT policy re-opens. The family is exactly the close arms of
// decideBotAction's break matrix:
//
//   - RANGE_BREAK_DOWN / RANGE_BREAK_UP — the adverse-regime closes;
//   - RANGE_BREAK_UP_TREND_STOP — NEUTRAL underwater in TREND_UP (the
//     down-move twin closes as plain RANGE_BREAK_DOWN);
//   - RANGE_SHIFT_*_NO_ADJUSTMENTS_LEFT — the in-place shift ladder is
//     exhausted; a fresh grid re-arms it AT the break price.
//
// RANGE_BREAK_UP_PROFIT_TAKE is deliberately excluded: it is a profit exit
// (cooldown-exempt already), and the scanner re-owns that slot immediately.
func dgtBreakRedeployReason(reason string) bool {
	switch reason {
	case "RANGE_BREAK_DOWN", "RANGE_BREAK_UP", "RANGE_BREAK_UP_TREND_STOP",
		"RANGE_SHIFT_DOWN_NO_ADJUSTMENTS_LEFT", "RANGE_SHIFT_UP_NO_ADJUSTMENTS_LEFT":
		return true
	}
	return false
}

// ouHalfLifeSteps fits the AR(1) regression Δp_t = a + b·p_{t-1} on the
// close series and returns the Ornstein-Uhlenbeck half-life in STEP units:
//
//	b  = cov(p_{t-1}, Δp) / var(p_{t-1})
//	HL = -ln(2) / ln(1+b)
//
// (the exact discrete-OU form; -ln(2)/b is the small-b approximation).
// ok=false when the fit is impossible or the tape is NOT mean-reverting:
// b ≥ 0 (trending) or b ≤ -1 (oscillating/divergent — not an OU process).
func ouHalfLifeSteps(prices []float64) (float64, bool) {
	if len(prices) < ouMinCandles {
		return 0, false
	}
	n := float64(len(prices) - 1)
	var sumLag, sumDelta, sumLagDelta, sumLagSq float64
	for i := 1; i < len(prices); i++ {
		lag := prices[i-1]
		delta := prices[i] - lag
		sumLag += lag
		sumDelta += delta
		sumLagDelta += lag * delta
		sumLagSq += lag * lag
	}
	cov := sumLagDelta - sumLag*sumDelta/n
	varLag := sumLagSq - sumLag*sumLag/n
	if varLag <= 0 {
		return 0, false
	}
	b := cov / varLag
	if b >= -1e-9 || b <= -1+1e-9 {
		return 0, false
	}
	hl := -math.Ln2 / math.Log(1+b)
	if math.IsNaN(hl) || math.IsInf(hl, 0) || hl <= 0 {
		return 0, false
	}
	return hl, true
}

// candleIntervalHours converts the settings candle interval ("15M", "60M",
// "4H", "1D"…) to its step length in hours; the default matches the 15m
// scanner cadence when the string is unparseable.
func candleIntervalHours(interval string) float64 {
	s := strings.ToUpper(strings.TrimSpace(interval))
	num, start := 0, 0
	for start < len(s) && s[start] >= '0' && s[start] <= '9' {
		num = num*10 + int(s[start]-'0')
		start++
	}
	if num == 0 {
		num = 1
	}
	switch {
	case strings.HasSuffix(s, "M") && !strings.HasSuffix(s, "MO"):
		return float64(num) / 60.0
	case strings.HasSuffix(s, "H"):
		return float64(num)
	case strings.HasSuffix(s, "D"):
		return float64(num) * 24.0
	case strings.HasSuffix(s, "W"):
		return float64(num) * 168.0
	default:
		return 0.25
	}
}

// ouSymbolReading is one cached per-symbol statistic bundle over the
// scanner's own candle window (settings.CandleInterval × LookbackCandles):
// the OU half-life in HOURS (0 = not mean-reverting) and the fresh ATR% per
// candle the DGT re-deploy geometry reuses.
type ouSymbolReading struct {
	fetchedAt     time.Time
	ok            bool    // candles were fetched and usable
	halfLifeHours float64 // >0 mean-reverting; 0 = trend/no fit
	atrPct        float64 // ATR as % of last close, per candle step
}

// ouReadingForSymbol serves the cached reading (TTL 30m). The manage loop is
// single-goroutine — the same plain-map argument as trancheTBRegime; the
// lazy init keeps zero-value Workers (tests) safe.
func (worker *Worker) ouReadingForSymbol(ctx context.Context, symbol string, settings Settings) ouSymbolReading {
	if worker.ouReadings == nil {
		worker.ouReadings = make(map[string]ouSymbolReading)
	}
	if cached, ok := worker.ouReadings[symbol]; ok && time.Since(cached.fetchedAt) < ouKlineCacheTTL {
		return cached
	}
	reading := ouSymbolReading{fetchedAt: time.Now()}
	candles, err := worker.publicClient.GetKlines(ctx, symbol, settings.CandleInterval, settings.LookbackCandles)
	if err == nil && len(candles) >= ouMinCandles {
		closes := make([]float64, 0, len(candles))
		for _, candle := range candles {
			f, _ := candle.Close.Float64()
			if f > 0 {
				closes = append(closes, f)
			}
		}
		if len(closes) >= ouMinCandles {
			reading.ok = true
			if hl, okHL := ouHalfLifeSteps(closes); okHL {
				reading.halfLifeHours = hl * candleIntervalHours(settings.CandleInterval)
			}
			reading.atrPct = candleATRPct(candles)
		}
	}
	worker.ouReadings[symbol] = reading
	return reading
}

// candleATRPct is the mean true range of the window as % of the last close
// (per candle step) — the fresh volatility ruler the DGT stub geometry and
// the anti-hunt stop share.
func candleATRPct(candles []pionex.KlineCandle) float64 {
	if len(candles) < 2 {
		return 0
	}
	var sumTR float64
	prevClose, _ := candles[0].Close.Float64()
	lastClose := prevClose
	count := 0
	for _, candle := range candles[1:] {
		high, _ := candle.High.Float64()
		low, _ := candle.Low.Float64()
		closeP, _ := candle.Close.Float64()
		if high <= 0 || low <= 0 || closeP <= 0 {
			continue
		}
		tr := high - low
		if diff := high - prevClose; diff > tr {
			tr = diff
		}
		if diff := prevClose - low; diff > tr {
			tr = diff
		}
		sumTR += tr
		count++
		prevClose = closeP
		lastClose = closeP
	}
	if count == 0 || lastClose <= 0 {
		return 0
	}
	return sumTR / float64(count) / lastClose * 100.0
}

// gridAgeVerdict is the OU age policy for one RUNNING bot.
type gridAgeVerdict struct {
	halfLifeHours float64 // 0 = tape not mean-reverting (trend) — the 48h ceiling governs
	maxAgeHours   float64
	rotate        bool
}

// gridAgeVerdictFor applies max_grid_age = clamp(2×HL, 4h, 48h) to the bot's
// age. Fail-open on unreadable candles (no reading → no rotation); a
// trending tape (no finite HL) rotates at the 48h ceiling — the fitted range
// is stale there by any definition.
func gridAgeVerdictFor(reading ouSymbolReading, age time.Duration) gridAgeVerdict {
	if !reading.ok {
		return gridAgeVerdict{rotate: false}
	}
	maxAge := ouMaxAgeCeilHours
	if reading.halfLifeHours > 0 {
		maxAge = ouHalfLifeMultiple * reading.halfLifeHours
		if maxAge < ouMaxAgeFloorHours {
			maxAge = ouMaxAgeFloorHours
		}
		if maxAge > ouMaxAgeCeilHours {
			maxAge = ouMaxAgeCeilHours
		}
	}
	return gridAgeVerdict{
		halfLifeHours: reading.halfLifeHours,
		maxAgeHours:   maxAge,
		rotate:        age.Hours() > maxAge,
	}
}

// dgtRedeploySpec is everything the re-deploy needs to know about the bot
// the break just closed — one struct, both fleets, so the two arms below
// cannot drift apart (parity by directive).
type dgtRedeploySpec struct {
	symbol       string
	direction    string          // same as the closed bot — re-center, not re-direction
	breakPrice   decimal.Decimal // the fresh grid's center
	slotBudget   decimal.Decimal // the closed bot's OWN slot capital (tranche base preferred)
	oldBotID     string
	oldBotNumber int
	candidateID  *string // lineage: the redeployed bot keeps the closed bot's candidate link
	atrFallback  float64 // deploy-time atrPctEntry of the closed bot (fresh ATR preferred)
	accountID    string  // REAL only
}

// slotCapital resolves the closed bot's slot budget: the stored tranche base
// (the amount the slot was designed for) when present, else the committed
// investment. Today's settings are deliberately NOT consulted — a budget
// change since deploy must not resize a DGT re-center.
func slotCapital(trancheBase *string, investment decimal.Decimal) decimal.Decimal {
	if trancheBase != nil {
		if base, err := decimal.NewFromString(strings.TrimSpace(*trancheBase)); err == nil && base.GreaterThan(decimal.Zero) {
			return base
		}
	}
	return investment
}

// dgtSharedGateBlockers runs the gates a DGT re-deploy must still pass —
// macro / economic / portfolio / risk-engine / slot — and returns "" when
// clear, else the human-readable reason (logged on a DGT_REDEPLOY_SKIPPED
// event so an absent re-deploy is always explainable).
//
// Deliberately NOT here: the per-symbol protective-close cooldown (a
// re-center is not a re-entry into the dead zone — see the file header), the
// entry-timing/channel gate, the confluence verdict and the DOM gate (the
// symbol already passed them at the original deploy; the break is new
// information about LOCATION, not quality).
func (worker *Worker) dgtSharedGateBlockers(ctx context.Context, settings Settings, spec dgtRedeploySpec) string {
	// Economic-event gate: same window the deploy paths run (T−2h…T+1h).
	if blocked, blockReason := worker.CheckEconomicEvents(ctx, 2); blocked {
		return "макро-событие USD «" + blockReason + "» — редеплой отложен"
	}
	// Liquidation-cascade gate, direction-aware mirror of the deploy paths:
	// a forced long unwind blocks LONG/NEUTRAL re-entries (SHORT stays — the
	// unwind window is precisely when short grids harvest).
	trend := strings.ToLower(spec.direction)
	if trend == "no_trend" || trend == "neutral" || trend == "" {
		trend = "neutral"
	}
	if cascadeLong, cascadeUSD := worker.CheckLiquidationCascade(ctx, 50_000_000); cascadeLong && trend != "short" {
		return fmt.Sprintf("каскад ликвидаций лонгов $%.0fM/час — LONG/NEUTRAL редеплой на паузе", cascadeUSD/1_000_000)
	}
	// Macro gate (CoinGecko beta-drift / alt-drain), same exemption shape.
	if veto, reason, _ := macroVeto(trend, false, worker.loadMacroContext(ctx)); veto {
		return reason
	}
	// Portfolio circuit breaker: the SAME joint paper+REAL 1h protective-close
	// count the deploy paths use — a fleet under stress stays defensive.
	var recentStops int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM paper_grid_bots
			WHERE settings_id = $1
			  AND status = 'COMPLETED'
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND closed_at > NOW() - INTERVAL '1 hour'
			UNION ALL
			SELECT 1 FROM grid_bots
			WHERE status IN ('STOPPED', 'LIQUIDATED')
			  AND COALESCE(closed_reason, '') NOT IN (
			      `+protectiveCloseExemptReasons+`)
			  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '1 hour'
		) recent_stops
	`, settings.ID).Scan(&recentStops); err == nil && recentStops >= 3 {
		return fmt.Sprintf("circuit breaker: %d защитных закрытий за последний час — редеплой на паузе", recentStops)
	}
	// Runaway ladder: durable per-symbol DGT_REDEPLOY count over 24h (the
	// migration-0046 partial index serves it).
	var ladder int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM bot_execution_events
		WHERE symbol = $1 AND event_type = $2
		  AND created_at > NOW() - INTERVAL '24 hours'
	`, spec.symbol, dgtRedeployEvent).Scan(&ladder); err == nil && ladder >= dgtRedeployMaxPerSymbolPer24h {
		return fmt.Sprintf("DGT-лестница: %d редеплоев за 24ч (потолок %d) — слот уходит сканеру",
			ladder, dgtRedeployMaxPerSymbolPer24h)
	}
	// Slot budget: the closing bot's own seat must actually be free (paper
	// settles synchronously above; a REAL row may still sit in
	// STOP_REQUESTED, which the deploy count includes — exclude it, it is
	// the very bot this re-deploy replaces).
	var active int
	if spec.accountID == "" {
		if err := worker.db.QueryRow(ctx, `
			SELECT COUNT(*) FROM paper_grid_bots
			WHERE settings_id = $1 AND symbol = $2 AND status = 'RUNNING'
		`, settings.ID, spec.symbol).Scan(&active); err == nil && active > 0 {
			return "символ уже в работе (paper) — редеплой не нужен"
		}
		if err := worker.db.QueryRow(ctx, `
			SELECT COUNT(*) FROM paper_grid_bots
			WHERE settings_id = $1 AND status = 'RUNNING'
		`, settings.ID).Scan(&active); err == nil && active >= settings.MaxActiveBots {
			return fmt.Sprintf("портфель полон (%d/%d) — редеплой отложен", active, settings.MaxActiveBots)
		}
		// Kill switch + exposure caps, the paper deploy exam (v2.0.27 parity).
		if err := worker.risk.ValidateNewPaperGrid(ctx, spec.symbol, settings.Leverage, spec.slotBudget); err != nil {
			return "risk engine: " + err.Error()
		}
		return ""
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE account_id = $1 AND symbol = $2
		  AND status IN ('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING', 'STOP_REQUESTED', 'STOPPING')
		  AND id <> $3
	`, spec.accountID, spec.symbol, spec.oldBotID).Scan(&active); err == nil && active > 0 {
		return "символ уже в работе (REAL) — редеплой не нужен"
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots
		WHERE account_id = $1
		  AND status IN ('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING', 'STOP_REQUESTED', 'STOPPING')
		  AND id <> $2
	`, spec.accountID, spec.oldBotID).Scan(&active); err == nil && active >= settings.MaxActiveBots {
		return fmt.Sprintf("портфель полон (%d/%d) — редеплой отложен", active, settings.MaxActiveBots)
	}
	if err := worker.risk.ValidateNewGrid(ctx, spec.accountID, spec.symbol, settings.Leverage, spec.slotBudget); err != nil {
		return "risk engine: " + err.Error()
	}
	return ""
}

// dgtFreshGeometry builds the re-centered grid from FRESH data: ATR% from
// the live candle window (cached with the OU reading; never below the closed
// bot's deploy-time ATR — a break tape must not shrink the stop ruler), a
// stub span of clamp(6×ATR, 4%, 25%) pushed through the SAME mesh/HAR
// pipeline the deploys use (ComputeAdaptiveMesh density + HAR applyToMesh
// re-centering when the daily fit is good).
func (worker *Worker) dgtFreshGeometry(
	ctx context.Context,
	settings Settings,
	spec dgtRedeploySpec,
) (mesh AdaptiveMeshResult, harGeo *harGeometryResult, atrPct float64) {
	reading := worker.ouReadingForSymbol(ctx, spec.symbol, settings)
	atrPct = reading.atrPct
	if spec.atrFallback > atrPct {
		atrPct = spec.atrFallback
	}
	if atrPct <= 0 {
		atrPct = 2.0
	}
	spanPct := 6.0 * atrPct
	if spanPct < 4.0 {
		spanPct = 4.0
	}
	if spanPct > 25.0 {
		spanPct = 25.0
	}
	half := spec.breakPrice.Mul(decimal.NewFromFloat(spanPct / 200.0))
	stubLower := spec.breakPrice.Sub(half)
	stubUpper := spec.breakPrice.Add(half)
	mesh = ComputeAdaptiveMesh(
		stubLower, stubUpper, spec.breakPrice,
		atrPct, "RANGE", spec.slotBudget, settings.Leverage,
		decimalFloat(settings.FeeBps), decimalFloat(settings.SlippageBps),
	)
	harGeo = worker.harGridGeometry(ctx, spec.symbol,
		decimalFloat(settings.FeeBps.Add(settings.SlippageBps)), spec.slotBudget.InexactFloat64())
	if harGeo != nil {
		harGeo.applyToMesh(spec.breakPrice, &mesh)
	}
	return mesh, harGeo, atrPct
}

// dgtStubCandidate is the minimal Candidate computeBotTargets needs from a
// re-deploy: symbol, live break price and the fresh ATR (both the atrPct
// assumption the dynamic targets read and the volatility slot).
func dgtStubCandidate(spec dgtRedeploySpec, atrPct float64) Candidate {
	return Candidate{
		Symbol:       spec.symbol,
		CurrentPrice: spec.breakPrice,
		ModelAssumptions: map[string]any{
			"atrPct": atrPct,
			"regime": "RANGE",
		},
	}
}

// noteDgtSkip leaves the durable audit trail for a blocked re-deploy on the
// CLOSED bot's ledger — "почему после пробоя нет новой сетки" must always
// have an answer in bot_execution_events.
func (worker *Worker) noteDgtSkip(ctx context.Context, spec dgtRedeploySpec, source, reason string) {
	worker.logger.Info("DGT re-deploy skipped",
		"component", "autogrid_worker", "symbol", spec.symbol, "source", source, "reason", reason)
	_ = LogBotEvent(ctx, worker.db, spec.oldBotID, spec.oldBotNumber, source, spec.symbol,
		"DGT_REDEPLOY_SKIPPED", &spec.breakPrice, nil, map[string]any{
			"reason": reason, "break_reason_family": "RANGE_BREAK",
		})
}

// queueDgtRedeployTelegram is the one notification both fleets send on a
// successful re-deploy (template lives in events.go).
func queueDgtRedeployTelegram(ctx context.Context, worker *Worker, spec dgtRedeploySpec, botNumber int, mesh AdaptiveMeshResult, budget decimal.Decimal) {
	_ = QueueTelegramEvent(ctx, worker.db, dgtRedeployEvent, map[string]any{
		"bot_number":   botNumber,
		"symbol":       spec.symbol,
		"center_price": spec.breakPrice.StringFixed(6),
		"budget":       budget.StringFixed(2),
		"lower_price":  mesh.LowerPrice.StringFixed(6),
		"upper_price":  mesh.UpperPrice.StringFixed(6),
	})
}

// dgtBreakReasonSQL is the SQL twin of dgtBreakRedeployReason — one literal,
// referenced by every query that filters close reasons by the DGT family.
const dgtBreakReasonSQL = `'RANGE_BREAK_DOWN', 'RANGE_BREAK_UP', 'RANGE_BREAK_UP_TREND_STOP',
	'RANGE_SHIFT_DOWN_NO_ADJUSTMENTS_LEFT', 'RANGE_SHIFT_UP_NO_ADJUSTMENTS_LEFT'`

// dgtRealIntentMaxAge bounds how long a queued REAL re-deploy intent stays
// executable: the native cancel normally settles within one manage pass
// (~30s), so two hours covers a wedged reconcile loop without leaving zombie
// intents from an epoch the operator has since re-thought.
const dgtRealIntentMaxAge = 2 * time.Hour

// dgtQueueRealRedeploy writes the durable re-deploy intent onto the closing
// REAL bot (called from the manage close branch, after the native cancel was
// accepted). The re-deploy itself runs from processDgtRealRedeployIntents
// once the row reaches a terminal settle: the exchange physically closes the
// position between the accepted cancel and the terminal state, and creating
// a second grid on the same symbol mid-close would trade against our own
// unwind — the one-manage-pass fence is a correctness requirement, not a
// delay. Paper needs no fence (its settle is synchronous and inline).
func (worker *Worker) dgtQueueRealRedeploy(ctx context.Context, settings Settings, spec dgtRedeploySpec) {
	if _, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object(
			'dgtRedeployPendingAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			'dgtBreakPrice', $2::TEXT,
			'dgtSlotBudget', $3::TEXT),
		    updated_at = NOW()
		WHERE id = $1 AND status IN ('STOP_REQUESTED', 'STOPPING')
	`, spec.oldBotID, spec.breakPrice.String(), spec.slotBudget.String()); err != nil {
		worker.logger.Warn("DGT REAL intent write failed — re-deploy after this break will not fire",
			"component", "autogrid_worker", "bot_id", spec.oldBotID, "symbol", spec.symbol, "error", err)
		return
	}
	worker.logger.Info("DGT REAL re-deploy queued (fires on terminal settle)",
		"component", "autogrid_worker", "symbol", spec.symbol,
		"break_price", spec.breakPrice.String(), "budget", spec.slotBudget.String())
}

// markDgtRealIntentDone consumes the intent exactly once per outcome. The
// create's idempotency key (autogrid-dgt:<bot>:<price>) makes an
// attempt-then-crash replay safe, so consuming AFTER the attempt cannot
// double-deploy.
func (worker *Worker) markDgtRealIntentDone(ctx context.Context, botID, outcome string) {
	_, _ = worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object(
			'dgtRedeployDoneAt', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			'dgtRedeployOutcome', $2::TEXT),
		    updated_at = NOW()
		WHERE id = $1
	`, botID, outcome)
}

// processDgtRealRedeployIntents executes queued REAL re-deploys whose parent
// row has settled terminal (whichever pass performed the settle — the bot
// loop's terminal paths or the closed-bot sync). Bounded to a small batch
// per manage tick; one attempt per intent.
func (worker *Worker) processDgtRealRedeployIntents(ctx context.Context, settings Settings) {
	rows, err := worker.db.Query(ctx, `
		SELECT id, COALESCE(bot_number, 0), symbol, direction, account_id, quote_investment,
		       NULLIF(model_state->>'trancheBase', ''),
		       COALESCE(NULLIF(model_state->>'atrPctEntry', '')::FLOAT8, 0),
		       NULLIF(model_state->>'dgtBreakPrice', ''),
		       model_state->>'dgtRedeployPendingAt'
		FROM grid_bots
		WHERE COALESCE(closed_reason, '') IN (`+dgtBreakReasonSQL+`)
		  AND model_state->>'dgtRedeployPendingAt' IS NOT NULL
		  AND model_state->>'dgtRedeployDoneAt' IS NULL
		  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
		  AND COALESCE(closed_at, updated_at) > NOW() - INTERVAL '2 hours'
		ORDER BY COALESCE(closed_at, updated_at)
		LIMIT 5
	`)
	if err != nil {
		worker.logger.Warn("DGT REAL intent scan failed",
			"component", "autogrid_worker", "error", err)
		return
	}
	type dgtIntent struct {
		botID                        string
		botNumber                    int
		symbol, direction, accountID string
		trancheBase                  *string
		pendingAt                    *string
		breakPriceRaw                *string
		investment                   decimal.Decimal
		atrEntry                     float64
	}
	intents := make([]dgtIntent, 0, 5)
	for rows.Next() {
		var item dgtIntent
		if err := rows.Scan(&item.botID, &item.botNumber, &item.symbol, &item.direction,
			&item.accountID, &item.investment, &item.trancheBase, &item.atrEntry,
			&item.breakPriceRaw, &item.pendingAt); err != nil {
			continue
		}
		intents = append(intents, item)
	}
	rows.Close()

	for _, item := range intents {
		spec := dgtRedeploySpec{
			symbol:       item.symbol,
			direction:    item.direction,
			slotBudget:   slotCapital(item.trancheBase, item.investment),
			oldBotID:     item.botID,
			oldBotNumber: item.botNumber,
			atrFallback:  item.atrEntry,
			accountID:    item.accountID,
		}
		switch {
		case !settings.DgtRedeployEnabled:
			worker.markDgtRealIntentDone(ctx, item.botID, "disabled")
		case !trancheMarkerFresh(item.pendingAt, dgtRealIntentMaxAge):
			worker.noteDgtSkip(ctx, spec, "REAL", "интент старше 2ч — отменён")
			worker.markDgtRealIntentDone(ctx, item.botID, "expired")
		case item.breakPriceRaw == nil || strings.TrimSpace(*item.breakPriceRaw) == "":
			worker.noteDgtSkip(ctx, spec, "REAL", "интент без цены пробоя — отменён")
			worker.markDgtRealIntentDone(ctx, item.botID, "no_break_price")
		default:
			breakPrice, pErr := decimal.NewFromString(strings.TrimSpace(*item.breakPriceRaw))
			if pErr != nil || !breakPrice.GreaterThan(decimal.Zero) {
				worker.noteDgtSkip(ctx, spec, "REAL", "цена пробоя не читается — отменён")
				worker.markDgtRealIntentDone(ctx, item.botID, "bad_break_price")
				continue
			}
			spec.breakPrice = breakPrice
			if worker.dgtRedeployReal(ctx, settings, spec) {
				worker.markDgtRealIntentDone(ctx, item.botID, "deployed")
			} else {
				worker.markDgtRealIntentDone(ctx, item.botID, "blocked")
			}
		}
	}
}

// dgtRedeployPaper re-opens the symbol on the paper fleet immediately after
// the RANGE_BREAK close: same slot capital, fresh tranche-1 contract, center
// at the break price. Returns true when a new RUNNING row exists.
func (worker *Worker) dgtRedeployPaper(ctx context.Context, settings Settings, spec dgtRedeploySpec) bool {
	if reason := worker.dgtSharedGateBlockers(ctx, settings, spec); reason != "" {
		worker.noteDgtSkip(ctx, spec, "PAPER", reason)
		return false
	}
	mesh, harGeo, atrPct := worker.dgtFreshGeometry(ctx, settings, spec)
	stub := dgtStubCandidate(spec, atrPct)

	baseLev := settings.Leverage
	if baseLev <= 0 {
		baseLev = 3
	}
	botLev := baseLev
	levMode := "BASE"
	if settings.AdaptiveLeverageEnabled {
		spanPct := 0.0
		if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && spec.breakPrice.IsPositive() {
			spanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(spec.breakPrice).Mul(decimal.NewFromInt(100)).Float64()
		}
		dyn := ComputeDynamicLeverage(atrPct, baseLev, spanPct)
		botLev = dyn.Leverage
		levMode = "ADAPTIVE"
	} else if harGeo != nil && harGeo.geo.Leverage < botLev {
		botLev = harGeo.geo.Leverage
		levMode = "HAR"
	}

	investAmount := spec.slotBudget
	trancheOn := settings.TrancheDeployEnabled
	if trancheOn {
		investAmount = spec.slotBudget.Div(decimal.NewFromInt(2))
	}

	meshSpanPct := 0.0
	if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && spec.breakPrice.IsPositive() {
		meshSpanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(spec.breakPrice).Mul(decimal.NewFromInt(100)).Float64()
	}
	target, maxLoss := computeBotTargets(settings, stub, botLev, meshSpanPct)
	if trancheOn {
		// Half capital → half target and half max loss for tranche 1 (the
		// manage loop's top-up doubles them back — the deploy contract).
		if target != nil {
			half := target.Div(decimal.NewFromInt(2))
			target = &half
		}
		if maxLoss != nil {
			half := maxLoss.Div(decimal.NewFromInt(2))
			maxLoss = &half
		}
	}

	atrPrice := spec.breakPrice.Mul(decimal.NewFromFloat(atrPct / 100.0))
	antiHuntStop := ComputeAntiHuntStop(
		spec.direction, mesh.LowerPrice, mesh.UpperPrice,
		spec.breakPrice, atrPrice, 1.5,
	)
	// v2.0.93 FIX-I (paper/REAL parity): the ±2% boundary clamp the REAL DGT
	// arm (below) and both deploy paths apply — a degenerate ATR must not
	// park the paper re-center's stop inside its own range.
	antiHuntStop = ClampAntiHuntStopIntoBounds(spec.direction, mesh.LowerPrice, mesh.UpperPrice, antiHuntStop)

	// Entry friction parity: the fresh paper grid books its taker entry fee
	// exactly like a deploy (v2.0.89 calibrated block).
	paperEntryFeePaid := paperEntryFee(spec.direction, mesh.LowerPrice, mesh.UpperPrice,
		mesh.GridNum, investAmount, botLev, spec.breakPrice)

	var botID string
	var botNumber int
	err := worker.db.QueryRow(ctx, `
		INSERT INTO paper_grid_bots (
			settings_id, candidate_id, symbol, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			entry_price, mark_price, model_state,
			pnl_target_usdt, max_loss_usdt,
			grid_step_pct, anti_hunt_stop_price,
			realized_pnl_usdt, fees_paid_usdt
		) VALUES (
			$1, $2, $3, 'RUNNING', $4, $5, $6, $7, $8, $9, $10, $11, $11,
			jsonb_build_object(
				'model', 'adaptive_confluence_mesh_v2',
				'gridFillsSimulated', false,
				'pnlTargetSource', $12::TEXT,
				'leverageMode', $15::TEXT,
				'baseLeverage', $16::INT,
				'dgtRedeploy', true,
				'dgtParentBot', $17::INT,
				'dgtBreakReason', $18::TEXT,
				'trancheDeployed', $19::INT,
				'trancheBase', $20::TEXT,
				'atrPctEntry', $21::FLOAT8,
				'entryFeeUsdt', $22::TEXT,
				'warning', 'paper PnL is not a native Pionex grid backtest'
			),
			$13, $14,
			$23, $24,
			-$22::NUMERIC, $22::NUMERIC
		)
		ON CONFLICT (settings_id, symbol) WHERE status = 'RUNNING'
		DO NOTHING
		RETURNING id, bot_number
	`, settings.ID, spec.candidateID, spec.symbol,
		spec.direction, mesh.GridType,
		mesh.LowerPrice, mesh.UpperPrice, mesh.GridNum,
		botLev, investAmount,
		spec.breakPrice, settings.PnLTargetMode, target, maxLoss,
		levMode, settings.Leverage, spec.oldBotNumber, "RANGE_BREAK",
		trancheFlag(trancheOn), spec.slotBudget.String(), atrPct,
		paperEntryFeePaid.Round(8).String(),
		mesh.GridStepPct, antiHuntStop).Scan(&botID, &botNumber)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			worker.noteDgtSkip(ctx, spec, "PAPER", "символ уже в работе — конфликт INSERT, редеплой не нужен")
			return false
		}
		worker.logger.Error("DGT paper re-deploy INSERT failed",
			"component", "autogrid_worker", "symbol", spec.symbol, "error", err)
		return false
	}

	worker.logger.Info("DGT re-deployed paper grid at break price",
		"component", "autogrid_worker", "symbol", spec.symbol,
		"center", spec.breakPrice.String(), "investment", investAmount.String(),
		"lower", mesh.LowerPrice.String(), "upper", mesh.UpperPrice.String())

	_ = LogBotEvent(ctx, worker.db, botID, botNumber, "PAPER", spec.symbol, dgtRedeployEvent,
		&spec.breakPrice, nil, map[string]any{
			"parent_bot":    spec.oldBotNumber,
			"direction":     spec.direction,
			"lower_price":   mesh.LowerPrice.String(),
			"upper_price":   mesh.UpperPrice.String(),
			"grid_num":      mesh.GridNum,
			"leverage":      botLev,
			"investment":    investAmount.String(),
			"budget":        spec.slotBudget.String(),
			"atr_pct":       math.Round(atrPct*100) / 100,
			"leverage_mode": levMode,
		})
	queueDgtRedeployTelegram(ctx, worker, spec, botNumber, mesh, spec.slotBudget)
	return true
}

// dgtRedeployReal is the REAL arm: the same gates, the same geometry, the
// same capital contract — executed through the SAME native lifecycle the
// deploy path uses (checkParams preflight → CreateGridBot → entry-fee
// ledger), so the exchange-side contract cannot drift between callers.
// Returns true when a new native bot row exists.
func (worker *Worker) dgtRedeployReal(ctx context.Context, settings Settings, spec dgtRedeploySpec) bool {
	// The closed bot carries its own account — a settings row without a
	// pinned accountId must not strand the re-deploy (the manage loop
	// supervises REAL bots regardless of settings.account_id; the local copy
	// mirrors deployReal's resolve step without persisting a pin).
	if settings.AccountID == nil {
		accountID := spec.accountID
		settings.AccountID = &accountID
	}
	if err := worker.realExecutionAllowed(ctx, settings); err != nil {
		worker.noteDgtSkip(ctx, spec, "REAL", "real execution: "+err.Error())
		return false
	}
	client, err := worker.service.PrivateClient(ctx, worker.accounts, spec.accountID)
	if err != nil {
		worker.noteDgtSkip(ctx, spec, "REAL", "клиент аккаунта недоступен: "+err.Error())
		return false
	}
	if reason := worker.dgtSharedGateBlockers(ctx, settings, spec); reason != "" {
		worker.noteDgtSkip(ctx, spec, "REAL", reason)
		return false
	}
	mesh, harGeo, atrPct := worker.dgtFreshGeometry(ctx, settings, spec)
	stub := dgtStubCandidate(spec, atrPct)

	baseLev := settings.Leverage
	if baseLev <= 0 {
		baseLev = 3
	}
	botLev := baseLev
	if settings.AdaptiveLeverageEnabled {
		spanPct := 0.0
		if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && spec.breakPrice.IsPositive() {
			spanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(spec.breakPrice).Mul(decimal.NewFromInt(100)).Float64()
		}
		botLev = ComputeDynamicLeverage(atrPct, baseLev, spanPct).Leverage
	} else if harGeo != nil && harGeo.geo.Leverage < botLev {
		botLev = harGeo.geo.Leverage
	}

	// Price precision from the break price itself (the deploy path's
	// exponent fallback — a re-center has no candidate assumption to read).
	pricePrecision := 6
	if spec.breakPrice.GreaterThan(decimal.Zero) {
		if exp := spec.breakPrice.Exponent(); exp < 0 {
			pricePrecision = int(-exp)
		}
	}
	if pricePrecision > 8 {
		pricePrecision = 8
	}
	lowerPrice := mesh.LowerPrice.Round(int32(pricePrecision))
	upperPrice := mesh.UpperPrice.Round(int32(pricePrecision))
	if upperPrice.LessThanOrEqual(lowerPrice) {
		minStep := decimal.New(1, -int32(pricePrecision))
		upperPrice = lowerPrice.Add(minStep.Mul(decimal.NewFromInt(int64(mesh.GridNum))))
	}

	atrPrice := spec.breakPrice.Mul(decimal.NewFromFloat(atrPct / 100.0))
	antiHuntStop := ComputeAntiHuntStop(
		spec.direction, lowerPrice, upperPrice,
		spec.breakPrice, atrPrice, 1.5,
	).Round(int32(pricePrecision))
	// The REAL DGT arm shares the deploy clamp via the same helper.
	antiHuntStop = ClampAntiHuntStopIntoBounds(spec.direction, lowerPrice, upperPrice, antiHuntStop).
		Round(int32(pricePrecision))

	investAmount := spec.slotBudget
	trancheOn := settings.TrancheDeployEnabled
	if trancheOn {
		investAmount = spec.slotBudget.Div(decimal.NewFromInt(2))
	}

	meshSpanPct := 0.0
	if mesh.UpperPrice.GreaterThan(mesh.LowerPrice) && spec.breakPrice.IsPositive() {
		meshSpanPct, _ = mesh.UpperPrice.Sub(mesh.LowerPrice).Div(spec.breakPrice).Mul(decimal.NewFromInt(100)).Float64()
	}
	botTarget, botMaxLoss := computeBotTargets(settings, stub, botLev, meshSpanPct)
	if trancheOn {
		if botTarget != nil {
			half := botTarget.Div(decimal.NewFromInt(2))
			botTarget = &half
		}
		if botMaxLoss != nil {
			half := botMaxLoss.Div(decimal.NewFromInt(2))
			botMaxLoss = &half
		}
	}

	base, quote, err := SplitPionexPerp(spec.symbol)
	if err != nil {
		worker.noteDgtSkip(ctx, spec, "REAL", "символ: "+err.Error())
		return false
	}
	data := pionex.BUOrderData{
		Top: upperPrice, Bottom: lowerPrice,
		Row: mesh.GridNum, GridType: mapGridType(settings.DensityGridEnabled),
		Trend:           trendForExchange(spec.direction),
		Leverage:        botLev,
		QuoteInvestment: investAmount.Round(2),
	}
	if settings.StopLossMode == "ADAPTIVE_ATR" {
		data.LossStopType = "price"
		data.LossStop = &antiHuntStop
	}
	if botTarget != nil && botTarget.GreaterThan(decimal.Zero) {
		targetVal := botTarget.Round(2)
		data.ProfitStopType = "profit_amount"
		data.ProfitStop = &targetVal
	} else if settings.SmartPNLEnabled && spec.direction != "NEUTRAL" {
		profit := upperPrice
		if spec.direction == "SHORT" {
			profit = lowerPrice
		}
		data.ProfitStopType = "price"
		data.ProfitStop = &profit
	}
	futuresBase := base
	if !strings.HasSuffix(futuresBase, ".PERP") && !strings.HasSuffix(futuresBase, "_PERP") {
		futuresBase = fmt.Sprintf("%s.PERP", base)
	}
	params := pionex.NativeFuturesGridCreateParams{
		Base: futuresBase, Quote: quote, BUOrderData: data,
	}
	if check, checkErr := client.CheckFuturesGridParams(ctx, params); checkErr != nil {
		worker.noteDgtSkip(ctx, spec, "REAL", "checkParams: "+checkErr.Error())
		return false
	} else if check != nil && check.GetMinInvestment().GreaterThan(decimal.Zero) &&
		investAmount.LessThan(check.GetMinInvestment()) {
		worker.noteDgtSkip(ctx, spec, "REAL",
			fmt.Sprintf("бюджет %s ниже минимума биржи %s", investAmount.String(), check.GetMinInvestment().String()))
		return false
	}

	manager := grid.NewLifecycleManager(worker.db, client)
	botID, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
		AccountID:          spec.accountID,
		AutoGridSettingsID: &settings.ID,
		// One idempotency key per (closed bot, break price): the same break
		// can never mint two re-deploys, the next break lands at a new price.
		IdempotencyKey: fmt.Sprintf("autogrid-dgt:%s:%s", spec.oldBotID, spec.breakPrice.Round(6).String()),
		Params:         params,
		PnLTargetUSDT:  botTarget,
		MaxLossUSDT:    botMaxLoss,
		AntiHuntStop:   &antiHuntStop,
		StructContext: map[string]any{
			"dgtRedeploy":   true,
			"dgtParentBot":  spec.oldBotNumber,
			"dgtBreakPrice": spec.breakPrice.String(),
		},
		TrancheState: map[string]any{
			"trancheDeployed": trancheFlag(trancheOn),
			"trancheBase":     spec.slotBudget.String(),
			"trancheEntry":    spec.breakPrice.String(),
			"atrPctEntry":     atrPct,
		},
	})
	if createErr != nil {
		if errors.Is(createErr, grid.ErrDuplicateActiveBot) {
			worker.noteDgtSkip(ctx, spec, "REAL", "активный грид уже существует — редеплой не нужен")
			return false
		}
		worker.logger.Error("DGT REAL re-deploy create failed",
			"component", "autogrid_worker", "symbol", spec.symbol, "error", createErr)
		return false
	}

	// v2.0.89 fee-ledger parity: the native create's taker entry fee is
	// booked exactly like a deploy's.
	if feeErr := recordRealEntryFee(ctx, worker.db, botID,
		pionex.EntryFeeUSDT(investAmount, botLev), "entryFeeUsdt"); feeErr != nil {
		worker.logger.Error("DGT re-deploy entry fee booking failed — ledger fee leg incomplete",
			"component", "autogrid_worker", "bot_id", botID, "error", feeErr)
	}

	var botNum int
	_ = worker.db.QueryRow(ctx, `SELECT COALESCE(bot_number, 0) FROM grid_bots WHERE id = $1`, botID).Scan(&botNum)

	worker.logger.Info("DGT re-deployed REAL grid at break price",
		"component", "autogrid_worker", "symbol", spec.symbol,
		"center", spec.breakPrice.String(), "investment", investAmount.String(),
		"lower", lowerPrice.String(), "upper", upperPrice.String())

	_ = LogBotEvent(ctx, worker.db, botID, botNum, "REAL", spec.symbol, dgtRedeployEvent,
		&spec.breakPrice, nil, map[string]any{
			"parent_bot":  spec.oldBotNumber,
			"direction":   spec.direction,
			"lower_price": lowerPrice.String(),
			"upper_price": upperPrice.String(),
			"grid_num":    mesh.GridNum,
			"leverage":    botLev,
			"investment":  investAmount.String(),
			"budget":      spec.slotBudget.String(),
			"atr_pct":     math.Round(atrPct*100) / 100,
		})
	queueDgtRedeployTelegram(ctx, worker, spec, botNum, mesh, spec.slotBudget)
	return true
}

// trendForExchange maps the stored direction to the native API's trend
// vocabulary (the deploy paths' smartParam mapping: NEUTRAL → no_trend).
func trendForExchange(direction string) string {
	switch strings.ToUpper(strings.TrimSpace(direction)) {
	case "LONG":
		return "long"
	case "SHORT":
		return "short"
	default:
		return "no_trend"
	}
}

// gridAgeModelStateSQL is the shared model_state telemetry patch the age
// rotation writes on BOTH fleets at close: halfLifeHours (0 = not
// mean-reverting) and maxAgeHours — the decision becomes auditable from the
// row itself.
func gridAgeModelStateSQL(halfLifeHours, maxAgeHours float64) map[string]any {
	return map[string]any{
		"halfLifeHours": math.Round(halfLifeHours*100) / 100,
		"maxAgeHours":   math.Round(maxAgeHours*100) / 100,
	}
}

// queueGridAgedTelegram renders the rotation notice (template in events.go).
func queueGridAgedTelegram(ctx context.Context, worker *Worker, source string, botNumber int, symbol string, verdict gridAgeVerdict, age time.Duration) {
	_ = QueueTelegramEvent(ctx, worker.db, gridAgedHalfLifeReason, map[string]any{
		"bot_number":      botNumber,
		"symbol":          symbol,
		"age_hours":       math.Round(age.Hours()*10) / 10,
		"max_age_hours":   math.Round(verdict.maxAgeHours*10) / 10,
		"half_life_hours": math.Round(verdict.halfLifeHours*10) / 10,
		"bot_source":      source,
	})
}
