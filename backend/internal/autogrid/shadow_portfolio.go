package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Shadow portfolio of rejected candidates (v2.0.56 F10). The 2026-09-01
// checkpoint left two questions unanswerable: does the score rank entries
// (r = −0.01 on outcomes said no, but n was 10), and do the entry gates add
// alpha or only turnover (no counterfactual exists). This module captures
// the top-scored REJECTED candidates per scan and replays them through the
// same pure paper-model core the live fleet runs on (neutralGridPaperPNL +
// decideBotAction) over public 5-minute klines — gate/score alpha becomes
// measurable instead of assumed.
//
// Deliberate simplifications (recorded per-row in sim_notes): no tranche-2,
// no radar re-centers, regime unknown (adverse-close on break), scanner
// geometry instead of the post-HAR deploy mesh. Shadow therefore measures
// the ENTRY SIGNAL under the exit policy — comparisons against real bots
// are ranking-level, not absolute-PnL-level.

const (
	shadowFlagKey  = "shadow_portfolio"
	shadowTopZ     = 5               // top-scored rejected candidates captured per scan
	shadowOpenCap  = 200             // max unsimulated rows before capture pauses
	shadowSimBatch = 50              // rows per simulation run
	shadowSimDue   = 20 * time.Hour  // min spacing between simulation runs
	shadowSimAge   = 24 * time.Hour  // row must mature (tranche time-box horizon)
	shadowKlines   = 500             // 5M candles ≈ 42h; /market/klines hard limit is 500 (live-probed: 600 → MARKET_PARAMETER_ERROR "limit error", 500 → ok)
)

func (worker *Worker) shadowPortfolioEnabled(ctx context.Context) bool {
	enabled := true
	_ = worker.db.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = $1), true)
	`, shadowFlagKey).Scan(&enabled)
	return enabled
}

// captureShadowCandidates runs once per completed paper deploy round: one
// INSERT..SELECT over that scan's rejected candidates. The hot gate gauntlet
// and rejectCandidate are untouched — zero latency added to the scan; the
// partial unique index on (symbol) WHERE NOT simulated dedups repeats
// without code.
func (worker *Worker) captureShadowCandidates(ctx context.Context, settings Settings, scanID string) {
	if !worker.shadowPortfolioEnabled(ctx) {
		return
	}
	var open int
	if err := worker.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM shadow_candidates WHERE NOT simulated`,
	).Scan(&open); err != nil || open >= shadowOpenCap {
		return
	}
	// v2.0.65 observability: 13h of zero shadow rows with SUCCEEDED scans
	// proved a silent capture failure is undiagnosable from the outside.
	// The eligible set uses the exact WHERE of the insert below, so
	// eligible>0 with inserted=0 is a visible red flag in every run's log.
	eligible := -1
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM autogrid_candidates c
		WHERE c.scan_id = $1 AND c.decision = 'REJECTED'
		  AND NOT EXISTS (SELECT 1 FROM shadow_candidates s
		                  WHERE s.symbol = c.symbol AND s.simulated = FALSE)
	`, scanID).Scan(&eligible); err != nil {
		worker.logger.Warn("shadow capture: eligible count failed",
			"component", "autogrid_worker", "scan", scanID, "error", err)
		eligible = -1
	}
	tag, err := worker.db.Exec(ctx, `
		WITH ranked AS (
			SELECT c.id, c.symbol, c.score, c.rejection_reason, c.recommended_trend,
			       c.lower_price, c.upper_price, c.grid_num, c.current_price, c.scan_id,
			       ROW_NUMBER() OVER (ORDER BY c.score DESC NULLS LAST) AS rn
			FROM autogrid_candidates c
			WHERE c.scan_id = $1 AND c.decision = 'REJECTED'
			  AND NOT EXISTS (SELECT 1 FROM shadow_candidates s
			                  WHERE s.symbol = c.symbol AND s.simulated = FALSE)
		)
		INSERT INTO shadow_candidates
			(candidate_id, scan_id, symbol, score, rejection_reason, direction,
			 mesh_lower, mesh_upper, grid_num, entry_price, leverage, investment, fee_bps)
		SELECT id, scan_id, symbol, score, rejection_reason,
		       UPPER(CASE COALESCE(NULLIF(recommended_trend, ''), 'no_trend')
		             WHEN 'no_trend' THEN 'NEUTRAL'
		             ELSE recommended_trend END),
		       lower_price, upper_price, grid_num, current_price,
		       $2::INT, $3::NUMERIC, $4::NUMERIC
		FROM ranked WHERE rn <= $5
	`, scanID, settings.Leverage, settings.BudgetUSDT,
		settings.FeeBps.Add(settings.SlippageBps), shadowTopZ)
	if err != nil {
		worker.logger.Warn("shadow capture failed",
			"component", "autogrid_worker", "scan", scanID,
			"eligible", eligible, "error", err)
		worker.alertShadowCaptureFailure(ctx, scanID, eligible, err)
		return
	}
	worker.logger.Info(fmt.Sprintf("shadow capture: eligible=%d inserted=%d", eligible, tag.RowsAffected()),
		"component", "autogrid_worker", "scan", scanID)
}

// alertShadowCaptureFailure makes a failing capture visible outside docker
// logs: one durable bot_execution_events marker plus a queued Telegram
// event, deduped to at most one per hour — the scan cycle is ~150s, so an
// undeduped alert would flood the event stream with hundreds of copies of
// the same error per day.
func (worker *Worker) alertShadowCaptureFailure(ctx context.Context, scanID string, eligible int, cause error) {
	var recentlyAlerted bool
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bot_execution_events
			WHERE event_type = 'SHADOW_CAPTURE_FAILED'
			  AND bot_id = 'shadow-capture'
			  AND created_at > NOW() - INTERVAL '1 hour'
		)
	`).Scan(&recentlyAlerted); err == nil && recentlyAlerted {
		return
	}
	if err := LogBotEvent(ctx, worker.db, "shadow-capture", 0, "SYSTEM", "", "SHADOW_CAPTURE_FAILED", nil, nil, map[string]any{
		"scan_id": scanID, "eligible": eligible, "error": cause.Error(),
	}); err != nil {
		worker.logger.Warn("shadow capture: failure marker write failed",
			"component", "autogrid_worker", "error", err)
	}
	_ = QueueTelegramEvent(ctx, worker.db, "SHADOW_CAPTURE_FAILED", map[string]any{
		"message": fmt.Sprintf("shadow-портфель: захват REJECTED-кандидатов падает (eligible=%d, scan=%s): %v",
			eligible, scanID, cause),
	})
}

// shadowSimIfDue replays matured shadow rows in bounded batches. The anchor
// is MAX(simulated_at) itself — no marker row needed.
func (worker *Worker) shadowSimIfDue(ctx context.Context, settings Settings) {
	if !worker.shadowPortfolioEnabled(ctx) {
		return
	}
	// Due-anchor via the pending index (WHERE simulated ORDER BY captured_at
	// DESC) — the newest simulated row is always a safe lower bound for the
	// last run, and MAX(simulated_at) has no index to lean on. The scalar
	// subquery is COALESCEd to epoch: on an empty table the bare query
	// returned ErrNoRows and the early return below meant the FIRST batch
	// could never start (prod: 147 pending / 0 simulated for 13h). The
	// anchor advances only through rows actually marked simulated — an empty
	// batch (nothing matured yet, or all-transient failures) leaves it where
	// it was, so the first matured candidate is picked up by the very next
	// pass instead of waiting out the 20h spacing.
	var lastSim time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT simulated_at FROM shadow_candidates
			WHERE simulated ORDER BY captured_at DESC LIMIT 1
		), TIMESTAMPTZ '1970-01-01')
	`).Scan(&lastSim); err != nil {
		return
	}
	if time.Since(lastSim) < shadowSimDue {
		return
	}

	type shadowRow struct {
		id           string
		candidateID  string
		symbol       string
		direction    string
		meshLower    decimal.Decimal
		meshUpper    decimal.Decimal
		gridNum      int
		entry        decimal.Decimal
		leverage     int
		investment   decimal.Decimal
		feeBps       decimal.Decimal
		capturedAt   time.Time
	}
	rows, err := worker.db.Query(ctx, `
		SELECT id, candidate_id::TEXT, symbol, direction,
		       mesh_lower, mesh_upper, grid_num, entry_price,
		       leverage, investment, fee_bps, captured_at
		FROM shadow_candidates
		WHERE NOT simulated AND captured_at < NOW() - INTERVAL '24 hours'
		ORDER BY captured_at
		LIMIT $1
	`, shadowSimBatch)
	if err != nil {
		worker.logger.Warn("shadow sim select failed",
			"component", "autogrid_worker", "error", err)
		return
	}
	pending := make([]shadowRow, 0, shadowSimBatch)
	for rows.Next() {
		var r shadowRow
		if err := rows.Scan(
			&r.id, &r.candidateID, &r.symbol, &r.direction,
			&r.meshLower, &r.meshUpper, &r.gridNum, &r.entry,
			&r.leverage, &r.investment, &r.feeBps, &r.capturedAt,
		); err != nil {
			rows.Close()
			return
		}
		pending = append(pending, r)
	}
	rows.Close()

	for _, r := range pending {
		worker.simulateShadowRow(ctx, settings, r.id, r.candidateID, r.symbol, r.direction,
			r.meshLower, r.meshUpper, r.gridNum, r.entry, r.leverage, r.investment,
			r.feeBps, r.capturedAt)
	}

	// Bounded retention, batched like every other smart-data table.
	_, _ = worker.db.Exec(ctx, `
		DELETE FROM shadow_candidates
		WHERE simulated AND simulated_at < NOW() - INTERVAL '90 days' AND id % 100 = 0
	`)
}

// transientSimFailure reports whether a simulation error is retryable:
// context cancellation/deadline (a worker restart or shutdown mid-batch)
// and transport blips (5xx, 429 cooldown, dial/read failures) must leave
// the row pending — marking it simulated on a restart silently ate the
// counterfactual this module exists to collect. Permanent failures
// (degenerate geometry, a gone source candidate, 4xx rejections) consume
// the row so one bad row cannot wedge the batch.
func transientSimFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	var apiErr *pionex.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500 || apiErr.StatusCode == http.StatusTooManyRequests
	}
	// Non-API errors off the public client are transport-level (dial, read,
	// decode blips) — retryable by nature; a permanently broken feed shows
	// up as a growing pending backlog (capture pauses at shadowOpenCap),
	// which is visible rather than silent.
	return true
}

// simulateShadowRow replays one captured rejection through the paper-model
// core. Best-effort per row: a PERMANENT failure marks the row simulated
// with the error note so one bad row cannot wedge the batch; a TRANSIENT
// one leaves it pending for the next run (see transientSimFailure).
func (worker *Worker) simulateShadowRow(
	ctx context.Context, settings Settings,
	id, candidateID, symbol, direction string,
	lower, upper decimal.Decimal, gridNum int,
	entry decimal.Decimal, leverage int,
	investment, feeBps decimal.Decimal, capturedAt time.Time,
) {
	fail := func(note string, cause error) {
		if transientSimFailure(cause) {
			worker.logger.Warn("shadow sim transient failure; row stays pending",
				"component", "autogrid_worker", "row", id, "error", cause)
			return
		}
		nb, _ := json.Marshal(map[string]string{"error": note})
		if _, err := worker.db.Exec(ctx, `
			UPDATE shadow_candidates
			SET simulated = TRUE, simulated_at = NOW(), sim_notes = $2::JSONB
			WHERE id = $1 AND NOT simulated
		`, id, string(nb)); err != nil {
			worker.logger.Warn("shadow sim failure note write failed",
				"component", "autogrid_worker", "row", id, "error", err)
		}
	}

	// Division guard: a zero entry price would panic the directional replay
	// and wedge the rest of the manage pass.
	if !entry.GreaterThan(decimal.Zero) || !upper.GreaterThan(lower) || gridNum < 2 {
		fail("degenerate geometry", nil)
		return
	}

	// Candidate features for the dynamic target computation.
	var volD, ddD decimal.Decimal
	var maRaw []byte
	if err := worker.db.QueryRow(ctx, `
		SELECT COALESCE(volatility, 0), COALESCE(max_drawdown_pct, 0), model_assumptions
		FROM autogrid_candidates WHERE id = $1
	`, candidateID).Scan(&volD, &ddD, &maRaw); err != nil {
		// A missing source row is permanent (the FK cascade already removed
		// this row in practice); any other DB error is transient.
		fail("candidate load: "+err.Error(), err)
		return
	}
	assumptions := map[string]any{}
	_ = json.Unmarshal(maRaw, &assumptions)
	cand := Candidate{
		ModelAssumptions: assumptions,
		VolatilityPct:    volD,
		MaxDrawdownPct:   ddD,
		LowerPrice:       lower,
		UpperPrice:       upper,
		CurrentPrice:     entry,
	}
	spanPct := 0.0
	if upper.GreaterThan(lower) && entry.GreaterThan(decimal.Zero) {
		spanPct, _ = upper.Sub(lower).Div(entry).Mul(decimal.NewFromInt(100)).Float64()
	}
	target, maxLoss := computeBotTargets(settings, cand, leverage, spanPct)
	pnlTarget, maxLossUSDT := decimal.Zero, decimal.Zero
	if target != nil {
		pnlTarget = *target
	}
	if maxLoss != nil {
		maxLossUSDT = *maxLoss
	}

	atrPct := 2.0
	if v, ok := assumptions["atrPct"].(float64); ok && v > 0 {
		atrPct = v
	}
	atrPrice := entry.Mul(decimal.NewFromFloat(atrPct / 100.0))
	antiHunt := ComputeAntiHuntStop(direction, lower, upper, entry, atrPrice, 1.5)

	candles, err := worker.publicClient.GetKlines(ctx, symbol, "5M", shadowKlines)
	if err != nil {
		fail("klines: "+err.Error(), err)
		return
	}
	windowEnd := capturedAt.Add(24 * time.Hour)
	started := false
	windowStart := time.Time{}
	used := 0

	// Replay state (mirrors the manage loop's paper bot).
	realized := decimal.Zero
	lastLevel := gridLevelForPrice(lower, upper, gridNum, entry)
	peak, trough := decimal.Zero, decimal.Zero
	var fundingLast *time.Time

	// Average snapshot funding rate for the symbol (fraction → bps).
	var fundingAvg decimal.Decimal
	_ = worker.db.QueryRow(ctx, `
		SELECT COALESCE(AVG(funding_rate), 0) FROM funding_snapshots
		WHERE symbol = $1 AND captured_at > NOW() - INTERVAL '24 hours'
	`, symbol).Scan(&fundingAvg)
	rateBps := fundingAvg.Mul(decimal.NewFromInt(10000))

	outcome, outcomeReason := "", "WINDOW_END"
	total := decimal.Zero

	candleTime := time.Time{}
	for _, c := range candles {
		candleTime = time.UnixMilli(c.Time)
		if candleTime.Before(capturedAt) {
			continue
		}
		if candleTime.After(windowEnd) {
			break
		}
		if !started {
			started = true
			windowStart = candleTime
		}
		used++

		// Intrabar anti-hunt breach (pessimistic SL-first: checked on the
		// candle extremes before the close-based decision).
		breached := false
		if direction == "SHORT" {
			breached = antiHunt.GreaterThan(decimal.Zero) && c.High.GreaterThanOrEqual(antiHunt)
		} else {
			breached = antiHunt.GreaterThan(decimal.Zero) && c.Low.LessThanOrEqual(antiHunt)
		}

		unrealized := decimal.Zero
		exposure := decimal.Zero
		switch {
		case breached:
			// fall through to decision below with breach price
			c.Close = antiHunt
		case direction == "LONG" || direction == "SHORT":
			unrealized = investment.Mul(decimal.NewFromInt(int64(leverage)))
			if direction == "LONG" {
				unrealized = unrealized.Mul(c.Close.Div(entry).Sub(decimal.NewFromInt(1)))
			} else {
				unrealized = unrealized.Mul(decimal.NewFromInt(1).Sub(c.Close.Div(entry)))
			}
			exposure = investment.Mul(decimal.NewFromInt(int64(leverage)))
		default:
			level := gridLevelForPrice(lower, upper, gridNum, c.Close)
			// Pairs pay the maker fee (the live loop's ladder contract), not
			// the captured taker+slippage composite — otherwise every shadow
			// NEUTRAL row is ~26 bps pessimistic per pair and gate/score
			// alpha drowns in a systematic fee error.
			pairProfit, uninv, invNotional := neutralGridPaperPNL(
				lower, upper, gridNum, investment, leverage, lastLevel, level, c.Close,
				decimal.NewFromFloat(pionexMakerFeeBps))
			realized = realized.Add(pairProfit)
			unrealized = uninv
			lastLevel = level
			exposure = invNotional
		}

		if delta, anchor := fundingAccrual(exposure, rateBps, capturedAt, fundingLast, candleTime); delta != nil {
			realized = realized.Sub(*delta)
			fundingLast = anchor
		} else if anchor != nil {
			fundingLast = anchor
		}

		total = realized.Add(unrealized)
		if total.GreaterThan(peak) {
			peak = total
		}
		if total.LessThan(trough) {
			trough = total
		}

		decision := decideBotAction(botActionInput{
			Direction:        direction,
			Lower:            lower,
			Upper:            upper,
			CurrentPrice:     c.Close,
			RealizedPNL:      realized,
			UnrealizedPNL:    unrealized,
			PeakPNL:          peak,
			Budget:           investment,
			PnLTarget:        pnlTarget,
			MaxLoss:          maxLossUSDT,
			RangeBreakBuffer: settings.RangeBreakBufferPct,
			AdjustmentsLeft:  0, // shadow = no-skill baseline: breaks close, never re-center
			Regime:           "",
			AntiHuntStop:     &antiHunt,
		})
		if strings.HasPrefix(decision.Action, "CLOSE") || breached {
			if breached {
				outcomeReason = "STRUCT_INVALID_ANTI_HUNT"
			} else {
				outcomeReason = decision.Reason
			}
			outcome = total.StringFixed(4)
			break
		}
	}
	if outcome == "" {
		outcome = total.StringFixed(4)
	}

	notes, _ := json.Marshal(map[string]any{
		"model":           "klines_5m_replay_v1",
		"simplifications": []string{"no_tranche2", "no_recenter", "regime_unknown", "scanner_mesh"},
		"window_clipped":  started && windowStart.Sub(capturedAt) > 15*time.Minute,
	})
	if _, err := worker.db.Exec(ctx, `
		UPDATE shadow_candidates
		SET simulated = TRUE, simulated_at = NOW(),
		    pnl_target_usdt = $2, max_loss_usdt = $3,
		    sim_window_start = $4, sim_window_end = $5, candles_used = $6,
		    outcome_pnl_usdt = $7::NUMERIC, outcome_reason = $8,
		    mfe_usdt = $9, mae_usdt = $10, sim_notes = $11::JSONB
		WHERE id = $1 AND NOT simulated
	`, id, pnlTarget, maxLossUSDT, windowStart, candleTime, used,
		outcome, outcomeReason, peak, trough, string(notes)); err != nil {
		// The replay result is gone if this write fails, but the row stays
		// pending and the next run redoes it — visible in logs either way.
		worker.logger.Warn("shadow sim result write failed; row stays pending",
			"component", "autogrid_worker", "row", id, "error", err)
	}
}
