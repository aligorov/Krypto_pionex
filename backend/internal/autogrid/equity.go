package autogrid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// TOTAL PnL = bot aggregate (v2.0.83). Raw /uapi/v1/account/detail captures
// closed the v2.0.75–82 wallet-truth chase: every coin row answers zero and
// positions is [] because the margins and floating PnL live INSIDE the
// isolated futures-grid bots — the account endpoints are structurally blind
// to them. The application truth (the figure the Pionex app itself shows) is
// the aggregate over bots:
//
//	epoch_pnl = Σ running (realized[grid profit + funding] + floating)
//	         + Σ closed-of-epoch (final where known; NULL final → the last
//	           telemetry total before closed_at, tagged estimated)
//
// The manage pass refreshes grid_bots.realized_pnl_usdt (profitReduce +
// fundingFeePayment) and unrealized_pnl_usdt (position × (mark − open)) from
// remote truth every cycle, so summing those columns IS summing the remote
// figures — no extra exchange round-trip.
const (
	// equitySnapshotMinSpacing is the capture throttle. The manage loop can
	// run as fast as every 15s; the durable table itself enforces the floor
	// so a restart cannot machine-gun the endpoint either.
	equitySnapshotMinSpacing = 5 * time.Minute
	// equityFailureDedup bounds the EQUITY_CAPTURE_FAILED marker: one durable
	// event per hour per failure mode. The manage pass runs every ~15-60s,
	// so an undeduped marker would flood bot_execution_events.
	equityFailureDedup = time.Hour
	// equityInfoEventDedup bounds the hourly EQUITY_SNAPSHOT Info marker —
	// the operator gets one aggregate heartbeat per hour, Telegram stays
	// silent (Info events never reach the outbox).
	equityInfoEventDedup = time.Hour
	// equityTelemetryFreshWindow is how close to closed_at the last telemetry
	// row must be to count as a fresh estimate; older rows are still used
	// (nearest-earlier) but the staleness is visible in the breakdown.
	equityTelemetryFreshWindow = 5 * time.Minute
	// equitySnapshotSourceBotAggregate tags rows written by the bot-aggregate
	// capture (v2.0.83 semantics) against the legacy wallet snapshots.
	equitySnapshotSourceBotAggregate = "bot_aggregate"
)

// pnlEpochDefaultStart seeds fresh installs (and predates the backfill):
// execution mode REAL went live 2026-09-03 16:33Z. The precise anchor lives
// in app_config('pnl_epoch_started_at'), backfilled by migration 0041 from
// the first REAL bot created after 16:30Z that day.
var pnlEpochDefaultStart = time.Date(2026, 9, 3, 16, 33, 0, 0, time.UTC)

// pnlEpochAnchorLayouts accepts the canonical RFC3339 (what migration 0041
// writes) plus the tolerant shapes an operator may hand-write into
// app_config — a Postgres-style "2026-09-03 16:33:00+00" or a naive UTC
// timestamp must re-anchor the epoch, not error the card.
var pnlEpochAnchorLayouts = []string{
	time.RFC3339,
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05",
}

// PNLEpochStart resolves the durable epoch anchor from app_config. Missing
// key → the default seed (fresh installs). A malformed value is an error,
// never silently re-anchored: the epoch boundary decides the headline PnL.
func (s *Service) PNLEpochStart(ctx context.Context) (time.Time, error) {
	var raw *string
	if err := s.db.QueryRow(ctx, `
		SELECT value #>> '{}' FROM app_config WHERE key = 'pnl_epoch_started_at'
	`).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pnlEpochDefaultStart, nil
		}
		return time.Time{}, fmt.Errorf("load pnl epoch anchor: %w", err)
	}
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return pnlEpochDefaultStart, nil
	}
	value := strings.TrimSpace(*raw)
	for _, layout := range pnlEpochAnchorLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("pnl epoch anchor %q matches no accepted timestamp layout", value)
}

// EquityBreakdown is the bot-aggregate epoch PnL split into its legs. Every
// component is exchange truth as persisted by the manage pass; the closed
// legs separate what the exchange settled (known) from telemetry-derived
// estimates so the UI can mark the confidence.
type EquityBreakdown struct {
	// RunningPnL = Σ running (realized + floating − entry fees) for epoch bots.
	// v2.0.89: the entry-fee leg is subtracted here — the exchange's grid
	// profit never carries the taker fee the wallet paid at deploy.
	RunningPnL decimal.Decimal
	// RunningFloating = Σ running unrealized (the "Floating PnL" leg).
	RunningFloating decimal.Decimal
	// RunningFeesPaid = Σ running fees_paid_usdt (entry fees at deploy/tranche).
	RunningFeesPaid decimal.Decimal
	// RunningInvestment = Σ running quote investment (the isolated margins).
	RunningInvestment decimal.Decimal
	// RunningBots = count of epoch bots in RUNNING/STOP_REQUESTED/STOPPING.
	RunningBots int
	// ClosedKnown = Σ final realized over closed epoch bots with a settled figure.
	ClosedKnown decimal.Decimal
	// ClosedKnownBots counts those finals.
	ClosedKnownBots int
	// ClosedFeesPaid = Σ closed-of-epoch entry fees (close costs already ride
	// INSIDE the telemetry_net_close finals — subtracting them here would
	// double-count).
	ClosedFeesPaid decimal.Decimal
	// ClosedEstimated = Σ telemetry-fallback totals over closed epoch bots
	// whose realized_pnl_usdt is NULL (no exchange total, no telemetry
	// either is NOT estimated — see UnknownCount). The v2.0.89 fallback is
	// the SAME telemetry-net-close estimate the settle paths write: last
	// total minus taker+slippage close cost, stop-floored.
	ClosedEstimated decimal.Decimal
	// ClosedEstimatedBots counts those estimates.
	ClosedEstimatedBots int
	// UnknownCount counts closed epoch bots with a NULL final and no usable
	// telemetry: deliberately NOT estimated, excluded from EpochPnL.
	UnknownCount int
}

// EpochPnL folds the breakdown into the operator's headline number:
// running + closed + fees (the fee legs enter the sums negative).
func (b EquityBreakdown) EpochPnL() decimal.Decimal {
	return b.RunningPnL.Add(b.ClosedKnown).Add(b.ClosedEstimated).Sub(b.ClosedFeesPaid)
}

// ComputeEquityBreakdown derives the epoch aggregate from grid_bots (+ the
// telemetry estimate for NULL finals), scoped to one account. Every query
// error is returned — the headline number must not degrade silently.
func ComputeEquityBreakdown(
	ctx context.Context, db *pgxpool.Pool, accountID string, epochStart time.Time,
) (EquityBreakdown, error) {
	breakdown := EquityBreakdown{}
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(SUM(realized_pnl_usdt), 0),
		       COALESCE(SUM(unrealized_pnl_usdt), 0),
		       COALESCE(SUM(fees_paid_usdt), 0),
		       COALESCE(SUM(quote_investment), 0),
		       COUNT(*)
		FROM grid_bots
		WHERE account_id = $1
		  AND bu_order_id IS NOT NULL
		  AND execution_mode = 'REAL'
		  AND created_at >= $2
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, accountID, epochStart).Scan(
		&breakdown.RunningPnL, &breakdown.RunningFloating,
		&breakdown.RunningFeesPaid, &breakdown.RunningInvestment, &breakdown.RunningBots,
	); err != nil {
		return breakdown, fmt.Errorf("equity breakdown: sum running legs: %w", err)
	}
	// The entry-fee leg (v2.0.89): the wallet paid it at deploy; no PnL
	// figure the exchange reports ever carries it.
	breakdown.RunningPnL = breakdown.RunningPnL.Add(breakdown.RunningFloating).Sub(breakdown.RunningFeesPaid)

	// Closed legs: settled finals count directly (their fee component — the
	// close cost — already rides inside telemetry_net_close estimates and
	// inside the exchange's netted totals; only the ENTRY fee is subtracted
	// again). NULL finals fall back to the same telemetry-net-close estimate
	// the settle paths write.
	closedRows, err := db.Query(ctx, `
		SELECT id, realized_pnl_usdt, COALESCE(max_loss_usdt, 0),
		       COALESCE(closed_reason, ''),
		       COALESCE(fees_paid_usdt, 0)
		         - COALESCE(NULLIF(model_state->>'closeCostUsdt','')::NUMERIC, 0),
		       COALESCE(closed_at, updated_at)
		FROM grid_bots
		WHERE account_id = $1
		  AND bu_order_id IS NOT NULL
		  AND execution_mode = 'REAL'
		  AND created_at >= $2
		  AND status IN ('STOPPED', 'COMPLETED', 'CANCELLED', 'LIQUIDATED')
	`, accountID, epochStart)
	if err != nil {
		return breakdown, fmt.Errorf("equity breakdown: load closed epoch bots: %w", err)
	}
	type closedRef struct {
		id           string
		maxLoss      decimal.Decimal
		closedReason string
		anchor       *time.Time
	}
	var unsettled []closedRef
	for closedRows.Next() {
		var ref closedRef
		var realized *decimal.Decimal
		var feesPaid decimal.Decimal
		// The anchor MUST scan into ref.anchor: scanning it into a local
		// and appending ref left ref.anchor nil, derefTime(nil) then dated
		// the telemetry probe at year zero — every estimate silently
		// became "unknown" (equity suite + closed-listing estimates red).
		if err := closedRows.Scan(&ref.id, &realized, &ref.maxLoss,
			&ref.closedReason, &feesPaid, &ref.anchor); err != nil {
			closedRows.Close()
			return breakdown, fmt.Errorf("equity breakdown: scan closed epoch bot: %w", err)
		}
		// fees_paid carries entry + close cost; the close-cost component
		// (model_state.closeCostUsdt, written by every telemetry_net_close
		// settle) is already netted inside the stored final — subtracting it
		// here too would double-count it. NULLIF guards the markerless rows
		// the pre-v2.0.89 chain settled.
		breakdown.ClosedFeesPaid = breakdown.ClosedFeesPaid.Add(feesPaid)
		if realized != nil {
			breakdown.ClosedKnown = breakdown.ClosedKnown.Add(*realized)
			breakdown.ClosedKnownBots++
			continue
		}
		unsettled = append(unsettled, ref)
	}
	closedRows.Close()
	if err := closedRows.Err(); err != nil {
		return breakdown, fmt.Errorf("equity breakdown: iterate closed epoch bots: %w", err)
	}

	for _, ref := range unsettled {
		snap, err := lastTelemetrySnapshotBefore(ctx, db, ref.id, derefTime(ref.anchor))
		if err != nil {
			return breakdown, fmt.Errorf("equity breakdown: telemetry fallback for %s: %w", ref.id, err)
		}
		if snap == nil {
			// No telemetry at all: do NOT invent a figure — the bot is
			// counted as unknown and stays out of the epoch sum.
			breakdown.UnknownCount++
			continue
		}
		estimate, _ := terminalTelemetryEstimate(
			snap.TotalPnl, snap.InventoryNotional, ref.maxLoss, ref.closedReason)
		breakdown.ClosedEstimated = breakdown.ClosedEstimated.Add(estimate)
		breakdown.ClosedEstimatedBots++
	}
	return breakdown, nil
}

// derefTime maps a *time.Time anchor to the value, or the zero instant when
// nil (a zero anchor simply matches no telemetry row).
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// captureBotAggregateEquity snapshots the fleet's bot-aggregate state into
// account_equity_snapshots (source='bot_aggregate'):
//
//	assets_usdt       = Σ running investment (the isolated margins)
//	unrealized_pnl    = Σ running floating
//	equity_usdt       = wallet USDT assets (0 in isolated reality) + assets
//	                    + Σ running (realized + floating)
//	available_usdt    = wallet USDT free
//
// It runs AFTER the manage bot loop, so the summed columns were just
// refreshed from remote truth. Fail-open by design — a failing capture must
// never disturb the manage pass — but never silent: real failures (fetch,
// decode, persist) leave a durable EQUITY_CAPTURE_FAILED marker. An account
// endpoint answering zero/empty is NOT a failure (that is the isolated-bot
// norm); the aggregate still lands and an hourly Info EQUITY_SNAPSHOT event
// carries the heartbeat without touching Telegram.
func (worker *Worker) captureBotAggregateEquity(ctx context.Context, settings Settings) {
	accountID := settings.AccountID
	if accountID == nil {
		resolved, err := worker.service.resolveAccount(ctx)
		if err != nil || resolved == nil {
			reason := "account resolution returned none"
			if err != nil {
				reason = err.Error()
			}
			worker.logger.Warn("equity snapshot: no managed account",
				"component", "autogrid_worker", "error", reason)
			worker.alertEquityCaptureFailure(ctx, "NO_ACCOUNT", reason)
			return
		}
		accountID = resolved
	}
	// Durable throttle: the newest bot_aggregate snapshot for THIS account
	// younger than the spacing means this pass has nothing to record.
	var lastAt *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(captured_at) FROM account_equity_snapshots
		WHERE account_id = $1 AND source = $2
	`, *accountID, equitySnapshotSourceBotAggregate).Scan(&lastAt); err == nil && lastAt != nil &&
		time.Since(*lastAt) < equitySnapshotMinSpacing {
		return
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// A broken throttle probe (missing table, connectivity) must not turn
		// into an unthrottled write path: report and stand down this pass.
		worker.logger.Warn("equity snapshot: throttle probe failed",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "THROTTLE_PROBE_FAILED", err.Error())
		return
	}

	client, err := worker.service.PrivateClient(ctx, worker.accounts, *accountID)
	if err != nil {
		worker.logger.Warn("equity snapshot: private client unavailable",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "CLIENT_UNAVAILABLE", err.Error())
		return
	}
	// The wallet leg: isolated grids park every cent inside the bots, so the
	// USDT row answers zero — that is the NORM, not an alarm (the v2.0.80–82
	// EMPTY_DECODE alarm fired weekly against a healthy fleet). Only a
	// transport/decode failure is a real capture error.
	walletAssets, walletAvailable := decimal.Zero, decimal.Zero
	balances, _, detailErr := client.GetFuturesAccountDetailRaw(ctx)
	if detailErr != nil {
		worker.logger.Warn("equity snapshot: account detail fetch failed",
			"component", "autogrid_worker", "error", detailErr)
		worker.alertEquityCaptureFailure(ctx, "FETCH_FAILED", detailErr.Error())
		return
	}
	for i := range balances {
		if strings.EqualFold(balances[i].Coin, "USDT") {
			walletAssets = balances[i].Assets
			walletAvailable = balances[i].Available
			break
		}
	}

	epochStart, err := worker.service.PNLEpochStart(ctx)
	if err != nil {
		worker.logger.Warn("equity snapshot: epoch anchor unavailable",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "EPOCH_ANCHOR_INVALID", err.Error())
		return
	}
	breakdown, err := ComputeEquityBreakdown(ctx, worker.db, *accountID, epochStart)
	if err != nil {
		worker.logger.Warn("equity snapshot: aggregate computation failed",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "AGGREGATE_FAILED", err.Error())
		return
	}

	runningPnL := breakdown.RunningPnL
	equity := walletAssets.Add(breakdown.RunningInvestment).Add(runningPnL)
	if _, err := worker.db.Exec(ctx, `
		INSERT INTO account_equity_snapshots
			(account_id, equity_usdt, assets_usdt, available_usdt, unrealized_pnl_usdt, source)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, *accountID, equity, breakdown.RunningInvestment, walletAvailable,
		breakdown.RunningFloating, equitySnapshotSourceBotAggregate); err != nil {
		worker.logger.Warn("equity snapshot persist failed",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "PERSIST_FAILED", err.Error())
		return
	}
	worker.logger.Info("epoch PnL (bot aggregate)",
		"component", "autogrid_worker",
		"account_id", *accountID,
		"epoch_pnl_usdt", breakdown.EpochPnL().StringFixed(4),
		"running_pnl_usdt", breakdown.RunningPnL.StringFixed(4),
		"closed_known_usdt", breakdown.ClosedKnown.StringFixed(4),
		"closed_estimated_usdt", breakdown.ClosedEstimated.StringFixed(4),
		"fees_usdt", breakdown.RunningFeesPaid.Add(breakdown.ClosedFeesPaid).StringFixed(4),
		"unknown_finals", breakdown.UnknownCount,
		"wallet_usdt", walletAssets.StringFixed(4))
	worker.recordEquitySnapshotHeartbeat(ctx, *accountID, breakdown)
}

// recordEquitySnapshotHeartbeat leaves one Info EQUITY_SNAPSHOT event per
// hour with the live aggregate — a durable heartbeat for the SQL journal
// that never reaches Telegram (only EQUITY_CAPTURE_FAILED alarms do).
func (worker *Worker) recordEquitySnapshotHeartbeat(
	ctx context.Context, accountID string, breakdown EquityBreakdown,
) {
	var recent bool
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bot_execution_events
			WHERE event_type = 'EQUITY_SNAPSHOT'
			  AND bot_id = 'equity'
			  AND created_at > NOW() - INTERVAL '1 hour'
		)
	`).Scan(&recent); err == nil && recent {
		return
	} else if err != nil {
		worker.logger.Warn("equity snapshot: heartbeat dedup probe failed",
			"component", "autogrid_worker", "error", err)
		return
	}
	epochPnL := breakdown.EpochPnL()
	if err := LogBotEvent(ctx, worker.db, "equity", 0, "SYSTEM", "", "EQUITY_SNAPSHOT", nil, &epochPnL, map[string]any{
		"account_id":            accountID,
		"epoch_pnl_usdt":        epochPnL.StringFixed(4),
		"running_pnl_usdt":      breakdown.RunningPnL.StringFixed(4),
		"running_bots":          breakdown.RunningBots,
		"running_investment":    breakdown.RunningInvestment.StringFixed(2),
		"closed_known_usdt":     breakdown.ClosedKnown.StringFixed(4),
		"closed_estimated_usdt": breakdown.ClosedEstimated.StringFixed(4),
		"fees_usdt":             breakdown.RunningFeesPaid.Add(breakdown.ClosedFeesPaid).StringFixed(4),
		"unknown_finals":        breakdown.UnknownCount,
	}); err != nil {
		worker.logger.Warn("equity snapshot: heartbeat event write failed",
			"component", "autogrid_worker", "error", err)
	}
}

// alertEquityCaptureFailure makes a dying equity capture visible outside
// docker stdout: one durable bot_execution_events marker plus a queued
// Telegram event, deduped to at most one per hour. Reserved for REAL
// failures (fetch/decode/persist/compute) — an empty account answer is the
// isolated-bot norm since v2.0.83 and never lands here.
func (worker *Worker) alertEquityCaptureFailure(ctx context.Context, reason, detail string) {
	var recentlyAlerted bool
	if err := worker.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM bot_execution_events
			WHERE event_type = 'EQUITY_CAPTURE_FAILED'
			  AND bot_id = 'equity'
			  AND created_at > NOW() - INTERVAL '1 hour'
		)
	`).Scan(&recentlyAlerted); err == nil && recentlyAlerted {
		return
	}
	if err := LogBotEvent(ctx, worker.db, "equity", 0, "SYSTEM", "", "EQUITY_CAPTURE_FAILED", nil, nil, map[string]any{
		"reason": reason, "detail": detail,
	}); err != nil {
		worker.logger.Warn("equity capture: failure marker write failed",
			"component", "autogrid_worker", "error", err)
	}
	_ = QueueTelegramEvent(ctx, worker.db, "EQUITY_CAPTURE_FAILED", map[string]any{
		"message": fmt.Sprintf("TOTAL PnL: агрегатный снапшот не пишется (%s): %s", reason, detail),
	})
}
