package autogrid

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Stop-radar (Phase 1, SHADOW) — anticipate each running bot's stop before
// it fires. Four-agent design 2026-08-31, recalibrated v2.0.56 (F5) against
// 5336 bot_risk_snapshots rows of the 2026-09-01 adverse day:
//
//	S = m5 · (0.80·s1 + 0.10·s2 + 0.10·s4) + 0.30·s3
//
//	s1 first-passage probability of touching the anti-hunt stop within 6h,
//	   distance measured in hourly-vol units, weighted by inventory side;
//	s2 volatility expansion: current Parkinson vs the symbol's 24h median
//	   from recent scan candidates (running-bot analog of the entry RV gate);
//	s3 fleet-wide alt-drain: share of running bots under water together
//	   while BTC dominance climbs (2026-08-30 class, lead 1-3h);
//	s4 cascade composite: funding z-score × OI flush × liquidation spike,
//	   armed when at least two of three legs are hot;
//	m5 regime multiplier: latest-scan Hurst against the inventory side.
//
// Calibration facts that drove V4: s1 saturates (p50 0.55, ≥0.9 in 17.8% of
// rows) while s2/s3/s4 never armed on the adverse day — the old 0.40 weight
// capped the composite at 0.62, making B3 mathematically unreachable and the
// whole matrix inert (1 action/21h). 0.80·s1 catches the HEMI/AXTIX class
// 55-67 minutes before the event; the action-side dwell/cooldown gates in
// radar_actions.go keep s1-saturation from becoming a fee machine.
//
// SHADOW mode computes and persists only — it never touches the exit
// ladder. Bands (B1 ≥0.30, B2 ≥0.60, B3 ≥0.75, B4 ≥0.90) are recorded so
// the closed ledger can price each band's would-have-saved separately.

const (
	radarHorizonHours  = 6.0
	radarTailFactor    = 1.25 // BM underestimates barrier contact under fat tails
	radarScoreEvery    = 5 * time.Minute
	radarVolMedianMins = 24 * 60

	bandB1 = 0.30
	bandB2 = 0.60
	bandB3 = 0.75
	bandB4 = 0.90
)

// radarInput is one running bot's per-tick state, assembled by the manage
// loop (all of it already computed there — the radar adds no fetches on the
// hot path; table lookups happen once per pass, not per bot). botSource is
// 'PAPER' (paper_grid_bots) or 'REAL' (grid_bots with a remote buOrderId);
// v2.0.72 extended the radar to the REAL fleet, which previously flew
// unscored and un-notified.
type radarInput struct {
	botID     string
	botNumber int
	botSource string
	symbol    string
	direction string

	price        decimal.Decimal
	antiHunt     *decimal.Decimal // nil = no stop anchored
	lower, upper decimal.Decimal
	atrEntryPct  float64 // model_state.atrPctEntry, % per 15m bar
	total        decimal.Decimal

	// inventorySide: +1 long inventory (price below mid / LONG bot),
	// −1 short inventory, 0 flat.
	inventorySide float64
}

// radarPriceFor resolves a bot's live price from the pass-wide ticker map,
// tolerating the _PERP/.PERP suffix variance between table conventions —
// the same triple fallback the paper and REAL manage loops apply inline.
func radarPriceFor(priceBySymbol map[string]decimal.Decimal, symbol string) (decimal.Decimal, bool) {
	price, ok := priceBySymbol[symbol]
	if !ok || price.IsZero() {
		trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(symbol), "_PERP"), ".PERP")
		price, ok = priceBySymbol[trimmed]
		if !ok || price.IsZero() {
			price, ok = priceBySymbol[trimmed+"_PERP"]
		}
	}
	if !ok || price.IsZero() {
		return decimal.Zero, false
	}
	return price, true
}

// realRadarInputs assembles radarInput rows for the REAL fleet: RUNNING
// grid_bots with a remote buOrderId under the active settings. PnL comes
// from the durable columns the reconcile loop persists from remote truth
// every pass; anti_hunt_stop_price is NULL-ed at value 0 because grid_bots
// defaults the column to 0 (never set), which must score as "no stop
// anchored", not as a stop at zero. REAL entry lives only in
// model_state->>'trancheEntry'/remote PositionOpenPrice and neither scoring
// nor re-centering reads it, so it is not selected.
func (worker *Worker) realRadarInputs(ctx context.Context, settings Settings, priceBySymbol map[string]decimal.Decimal) []radarInput {
	rows, err := worker.db.Query(ctx, `
		SELECT id, COALESCE(bot_number, 0), symbol, direction,
		       lower_price, upper_price, NULLIF(anti_hunt_stop_price, 0),
		       COALESCE(NULLIF(model_state->>'atrPctEntry','')::FLOAT8, 0),
		       COALESCE(realized_pnl_usdt, 0) + COALESCE(unrealized_pnl_usdt, 0)
		FROM grid_bots
		WHERE autogrid_settings_id = $1 AND bu_order_id IS NOT NULL
		  AND status = 'RUNNING'
	`, settings.ID)
	if err != nil {
		worker.logger.Warn("stop-radar: REAL fleet select failed",
			"component", "autogrid_worker", "error", err)
		return nil
	}
	defer rows.Close()
	inputs := make([]radarInput, 0, 8)
	for rows.Next() {
		var in radarInput
		if err := rows.Scan(
			&in.botID, &in.botNumber, &in.symbol, &in.direction,
			&in.lower, &in.upper, &in.antiHunt, &in.atrEntryPct, &in.total,
		); err != nil {
			worker.logger.Warn("stop-radar: REAL fleet row scan failed",
				"component", "autogrid_worker", "error", err)
			continue
		}
		price, ok := radarPriceFor(priceBySymbol, in.symbol)
		if !ok {
			continue // no live price this pass: unscoreable, same contract as paper
		}
		in.botSource = "REAL"
		in.price = price
		in.inventorySide = inventorySideOf(in.direction, price, in.lower, in.upper)
		inputs = append(inputs, in)
	}
	return inputs
}

type radarFleet struct {
	rhoNeg      float64 // share of running bots with total < 0
	domSlopeBps float64 // BTC dominance OLS slope over 2h, bps/hour (0 = unknown)
	btc24h      float64
	n           int
}

type radarScores struct {
	S1, S2, S3, S4, M5, Score float64
	Band                      int
	DistToStopATR             float64
}

// firstPassageHit returns P(X hits barrier a within T) for driftless BM with
// per-hour vol sigma, a > 0 in log units: Φ(−a/(σ√T))·2 for one barrier —
// the classic reflexion-principle result. Drift is folded in via the tail
// factor and the band dwell instead of a fragile per-tick estimate.
func firstPassageHit(aLog, sigmaHour, hours float64) float64 {
	if aLog <= 0 {
		return 1
	}
	if sigmaHour <= 0 {
		return 0
	}
	z := aLog / (sigmaHour * math.Sqrt(hours))
	p := 2 * normalCDF(-z)
	if p > 1 {
		p = 1
	}
	return p
}

func normalCDF(x float64) float64 { return 0.5 * (1 + math.Erf(x/math.Sqrt2)) }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// radarBand maps a composite score to its policy band.
func radarBand(score float64) int {
	switch {
	case score >= bandB4:
		return 4
	case score >= bandB3:
		return 3
	case score >= bandB2:
		return 2
	case score >= bandB1:
		return 1
	default:
		return 0
	}
}

// scoreBot computes the composite for one bot from per-tick state, per-symbol
// scan stats (Parkinson median + latest Hurst), fleet context and the
// cascade composite legs. Pure function — unit-testable.
func scoreBot(in radarInput, parkNow, parkMedian24h, latestHurst float64, fleet radarFleet, cascadeLegs int, cascadeMax float64) radarScores {
	var rs radarScores

	// s1 — distance to the adverse stop in hourly-vol units.
	if in.antiHunt != nil && in.price.IsPositive() && in.atrEntryPct > 0 {
		stop := *in.antiHunt
		// The barrier the inventory makes lethal: long inventory fears the
		// DOWN anti-hunt stop, short inventory the upper bound.
		var barrier decimal.Decimal
		if in.inventorySide >= 0 {
			barrier = stop
			if in.price.LessThan(stop) {
				barrier = in.price // already through: certain
			}
		} else {
			barrier = in.upper
			if in.price.GreaterThan(in.upper) {
				barrier = in.price
			}
		}
		distLog := math.Abs(math.Log(barrier.Div(in.price).InexactFloat64()))
		if barrier.Equal(in.price) {
			distLog = 0
		}
		// ATR% is per 15m bar → hourly vol ≈ atr·√4, expressed as a fraction.
		sigmaHour := (in.atrEntryPct / 100.0) * 2.0
		p := firstPassageHit(distLog, sigmaHour, radarHorizonHours) * radarTailFactor
		rs.S1 = clamp01(p)
		rs.DistToStopATR = distLog / math.Max(sigmaHour, 1e-9)
	}

	// s2 — Parkinson expansion vs the symbol's own 24h scan median.
	if parkMedian24h > 0 && parkNow > 0 {
		rs.S2 = clamp01((parkNow/parkMedian24h - 1.0) / 0.5)
	}

	// s3 — fleet alt-drain (applies to every non-flat bot).
	if fleet.n >= 4 && fleet.rhoNeg >= 0.8 && fleet.domSlopeBps >= 3.0 && fleet.btc24h > -0.5 {
		rs.S3 = clamp01((fleet.domSlopeBps / 5.0) * fleet.rhoNeg)
	}

	// s4 — cascade composite: needs 2-of-3 legs hot, scaled by the hottest leg.
	if cascadeLegs >= 2 {
		rs.S4 = clamp01(cascadeMax)
	}

	// m5 — persistent-trend regime against the inventory side.
	rs.M5 = 1.0
	if latestHurst > 0.55 && in.inventorySide != 0 {
		// Hurst above the scanner veto while holding one-sided inventory is
		// exactly the "neutral grid in a trend" failure shape.
		rs.M5 = 1.0 + 0.5*clamp01((latestHurst-0.55)/0.20)*math.Abs(in.inventorySide)
	}

	// Composite: bot-local components under the regime multiplier, plus the
	// fleet-wide drain as an ADDITIVE term (agent design: it applies to
	// every non-BTC bot regardless of local state) — a full alt-drain arm
	// (s3 ≈ 0.9) lifts the whole fleet into B1 on its own, without which a
	// systemic night with individually-safe distances stays invisible.
	// v2.0.56 (F5) V4 weights: S1 carries the signal (saturates at p50 0.55),
	// S2/S4 never armed on the calibration day — the old 0.40·S1 ceiling
	// kept the composite ≤0.62 and B3 unreachable. S3 keeps its full 0.30:
	// it never fired in the calibration window, so its weight changes none
	// of the V4 frequency/precision numbers while preserving the systemic
	// alt-drain early warning.
	rs.Score = clamp01(rs.M5*(0.80*rs.S1+0.10*rs.S2+0.10*rs.S4) + 0.30*rs.S3)
	rs.Band = radarBand(rs.Score)
	return rs
}

// inventorySideOf mirrors the funding-sign logic the manage loop already
// applies: below mid on a neutral grid = long inventory accumulated.
func inventorySideOf(direction string, price, lower, upper decimal.Decimal) float64 {
	mid := lower.Add(upper).Div(decimal.NewFromInt(2))
	switch direction {
	case "LONG":
		return 1
	case "SHORT":
		return -1
	}
	if mid.IsZero() {
		return 0
	}
	if price.LessThan(mid) {
		return 1 // grid bought the way down
	}
	if price.GreaterThan(mid) {
		return -1
	}
	return 0
}

// radarPass drives one SHADOW scoring pass over the fleet. Throttled to one
// snapshot per bot per radarScoreEvery; band TRANSITIONS emit a
// STOP_FORECAST_SHADOW bot event + a rate-limited Telegram advisory.
func (worker *Worker) radarPass(ctx context.Context, settings Settings, bots []radarInput) {
	if settings.StopForecastMode == "OFF" || len(bots) == 0 {
		return
	}

	// Fleet context once per pass.
	fleet := radarFleet{n: len(bots)}
	for _, b := range bots {
		if b.total.IsNegative() {
			fleet.rhoNeg++
		}
	}
	fleet.rhoNeg /= float64(len(bots))
	fleet.domSlopeBps, fleet.btc24h = worker.dominanceSlope(ctx)

	// Cascade legs (once per pass, market-wide).
	legs, maxLeg := worker.cascadeLegs(ctx)

	// ACTIVE re-scores fast (the action matrix is only as early as its
	// slowest input); SHADOW keeps the calm 5-minute observational cadence.
	scoreThrottle := radarScoreEvery
	if settings.StopForecastMode == "ACTIVE" {
		scoreThrottle = 90 * time.Second
	}

	now := time.Now().UTC()
	for _, b := range bots {
		parkNow, parkMed, hurst := worker.symbolScanStats(ctx, b.symbol)
		rs := scoreBot(b, parkNow, parkMed, hurst, fleet, legs, maxLeg)

		// Band state from model_state via the last snapshot band stored
		// in-bot (read through a tiny query, throttled by insert time).
		prevBand, lastAt, ok := worker.lastRadarSnapshot(ctx, b.botID)
		if ok && now.Sub(lastAt) < scoreThrottle {
			continue
		}

		if _, err := worker.db.Exec(ctx, `
			INSERT INTO bot_risk_snapshots
				(bot_id, bot_number, bot_source, symbol, mode, s1, s2, s3, s4, m5, score, band,
				 dist_to_stop_atr, inventory_skew, fleet_rho_neg, dom_slope_bps_h, total_pnl)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		`,
			b.botID, b.botNumber, b.botSource, b.symbol, settings.StopForecastMode,
			decimal.NewFromFloat(rs.S1).Round(6), decimal.NewFromFloat(rs.S2).Round(6),
			decimal.NewFromFloat(rs.S3).Round(6), decimal.NewFromFloat(rs.S4).Round(6),
			decimal.NewFromFloat(rs.M5).Round(4), decimal.NewFromFloat(rs.Score).Round(6),
			rs.Band,
			decimal.NewFromFloat(rs.DistToStopATR).Round(4),
			decimal.NewFromFloat(b.inventorySide).Round(4),
			decimal.NewFromFloat(fleet.rhoNeg).Round(4),
			decimal.NewFromFloat(fleet.domSlopeBps).Round(4),
			b.total.Round(8),
		); err != nil {
			slog.Warn("stop-radar: snapshot persist failed", "symbol", b.symbol, "error", err)
			continue
		}

		// v2.0.72: the shadow transition flows from the bot's own source —
		// a REAL band change must be identifiable in the ledger and Telegram.
		if ok && rs.Band != prevBand && rs.Band >= 2 {
			_ = LogBotEvent(ctx, worker.db, b.botID, b.botNumber, b.botSource, b.symbol,
				"STOP_FORECAST_SHADOW", &b.price, nil, map[string]any{
					"band": rs.Band, "prev_band": prevBand, "score": decimal.NewFromFloat(rs.Score).Round(4).String(),
					"s1": decimal.NewFromFloat(rs.S1).Round(3).String(), "s2": decimal.NewFromFloat(rs.S2).Round(3).String(),
					"s3": decimal.NewFromFloat(rs.S3).Round(3).String(), "s4": decimal.NewFromFloat(rs.S4).Round(3).String(),
				})
			_ = QueueTelegramEvent(ctx, worker.db, "STOP_FORECAST_SHADOW", map[string]any{
				"bot_number": b.botNumber, "symbol": b.symbol, "band": rs.Band,
				"score": decimal.NewFromFloat(rs.Score).Round(3).String(), "total": b.total.Round(2).String(),
			})
		}

		// Phase B (v2.0.52): ACTIVE executes the matrix — a B3/B4 score
		// re-centers the grid instead of donating the stop; in SHADOW the
		// call below no-ops (mode gate inside).
		worker.radarMaybeRecenter(ctx, settings, b, rs)
	}

	// Bounded retention, batched like every other smart-data table.
	_, _ = worker.db.Exec(ctx, `DELETE FROM bot_risk_snapshots WHERE captured_at < NOW() - INTERVAL '14 days' AND id % 500 = 0`)
}

// dominanceSlope: OLS slope of BTC dominance over the last 2h (bps/hour) and
// the latest BTC 24h return. Zero slope when history is still young.
func (worker *Worker) dominanceSlope(ctx context.Context) (slopeBps, btc24h float64) {
	rows, err := worker.db.Query(ctx, `
		SELECT EXTRACT(EPOCH FROM captured_at) AS t, btc_dominance_pct, btc_24h_pct
		FROM coingecko_snapshots
		WHERE captured_at > NOW() - INTERVAL '2 hours' AND btc_dominance_pct > 0
		ORDER BY captured_at
	`)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	type pt struct{ t, dom float64 }
	pts := make([]pt, 0, 14)
	for rows.Next() {
		var t, dom, b24 decimal.Decimal
		if err := rows.Scan(&t, &dom, &b24); err != nil {
			return 0, 0
		}
		f, _ := dom.Float64()
		tf, _ := t.Float64()
		btc24h, _ = b24.Float64()
		pts = append(pts, pt{tf, f})
	}
	if len(pts) < 6 {
		return 0, btc24h
	}
	// OLS slope per second → bps/hour.
	var sx, sy, sxx, sxy float64
	n := float64(len(pts))
	for _, p := range pts {
		sx += p.t
		sy += p.dom
		sxx += p.t * p.t
		sxy += p.t * p.dom
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, btc24h
	}
	slope := (n*sxy - sx*sy) / den    // dominance % per second
	return slope * 3600 * 100, btc24h // → bps per hour (1% dominance = 100 bps)
}

// cascadeLegs: how many of {funding extreme, OI flush, liquidation spike}
// are hot right now, and the hottest normalized leg 0-1. Market-wide, so
// computed once per pass.
func (worker *Worker) cascadeLegs(ctx context.Context) (legs int, maxLeg float64) {
	// Funding: market-wide mean |rate| over the last 10 minutes;
	// ≥0.05%/8h counts as a leg, 0.1% = full.
	var fundingAvg float64
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE(AVG(ABS(funding_rate)), 0)
		FROM funding_snapshots
		WHERE captured_at > NOW() - INTERVAL '10 minutes'
	`).Scan(&fundingAvg); err == nil && fundingAvg >= 0.0005 {
		legs++
		maxLeg = math.Max(maxLeg, clamp01(fundingAvg/0.001))
	}
	// OI flush on BTC over 1h (USD-normalized against its own price move).
	var oiLatest, oiOldest, pxLatest, pxOldest float64
	if err := worker.db.QueryRow(ctx, `
		WITH points AS (
			SELECT oi_usd, price,
			       ROW_NUMBER() OVER (ORDER BY captured_at DESC) AS rn_desc,
			       ROW_NUMBER() OVER (ORDER BY captured_at ASC)  AS rn_asc
			FROM oi_history
			WHERE symbol = 'BTC_USDT_PERP' AND captured_at > NOW() - INTERVAL '1 hour'
		)
		SELECT
			(SELECT oi_usd FROM points WHERE rn_desc = 1), (SELECT price FROM points WHERE rn_desc = 1),
			(SELECT oi_usd FROM points WHERE rn_asc = 1),  (SELECT price FROM points WHERE rn_asc = 1)
	`).Scan(&oiLatest, &pxLatest, &oiOldest, &pxOldest); err == nil &&
		oiOldest > 0 && pxOldest > 0 && oiLatest > 0 {
		contractsNow, contractsThen := oiLatest/pxLatest, oiOldest/pxOldest
		changePct := (contractsNow/contractsThen - 1) * 100
		if changePct <= -3.0 {
			legs++
			maxLeg = math.Max(maxLeg, clamp01(-changePct/6.0))
		}
	}
	// Liquidations ≥ 20M/1h (entry-cascade level), 50M = full leg.
	var liq float64
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(value_usd), 0)
		FROM liquidation_events
		WHERE captured_at > NOW() - INTERVAL '1 hour'
	`).Scan(&liq); err == nil && liq >= 20_000_000 {
		legs++
		maxLeg = math.Max(maxLeg, clamp01(liq/50_000_000))
	}
	return legs, maxLeg
}

// symbolScanStats: latest Parkinson and Hurst for a symbol from the recent
// scan candidates (fresh enough: scans run every ~2.5m).
func (worker *Worker) symbolScanStats(ctx context.Context, symbol string) (parkNow, parkMedian24h, hurst float64) {
	var park decimal.Decimal
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE((model_assumptions->>'volatilityParkinson')::FLOAT8, 0)
		FROM autogrid_candidates
		WHERE symbol = $1 AND created_at > NOW() - INTERVAL '30 minutes'
		ORDER BY created_at DESC LIMIT 1
	`, symbol).Scan(&park); err == nil {
		parkNow, _ = park.Float64()
	}
	var med decimal.Decimal
	if err := worker.db.QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY (model_assumptions->>'volatilityParkinson')::FLOAT8)
		FROM autogrid_candidates
		WHERE symbol = $1 AND created_at > NOW() - INTERVAL '24 hours'
	`, symbol).Scan(&med); err == nil {
		parkMedian24h, _ = med.Float64()
	}
	var h decimal.Decimal
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE((model_assumptions->>'hurst')::FLOAT8, 0)
		FROM autogrid_candidates
		WHERE symbol = $1 AND created_at > NOW() - INTERVAL '30 minutes'
		ORDER BY created_at DESC LIMIT 1
	`, symbol).Scan(&h); err == nil {
		hurst, _ = h.Float64()
	}
	return parkNow, parkMedian24h, hurst
}

// lastRadarSnapshot: previous band + time for transition detection.
func (worker *Worker) lastRadarSnapshot(ctx context.Context, botID string) (band int, at time.Time, ok bool) {
	err := worker.db.QueryRow(ctx, `
		SELECT band, captured_at FROM bot_risk_snapshots
		WHERE bot_id = $1 ORDER BY captured_at DESC LIMIT 1
	`, botID).Scan(&band, &at)
	return band, at, err == nil
}
