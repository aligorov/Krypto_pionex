package autogrid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Paper-engine friction constants — ONE calibrated block (Part 2). The REAL
// ledger rates live in pionex/fees.go (fixed exchange truth: taker 0.05%,
// close slippage 0.05%).
//
// calibrated vs REAL epoch 2026-09-03..05: wallet −31 vs uncalibrated paper
// +70/day. Vars, not consts, so a future epoch can re-calibrate without
// touching call sites.
var (
	// paperFillBufferPct is how far BEYOND a grid level the price must trade
	// (in percent) before the level counts as crossed — a touch is not a
	// fill. Kills the optimistic fills that painted +$70/day on paper while
	// the same fleet's wallet bled.
	paperFillBufferPct = decimal.NewFromFloat(0.03)
	// paperStopTakerBps + paperStopSlippageBps price a protective close of
	// the whole inventory in the simulator (maker pair fees stay at
	// pionexMakerFeeBps in manage.go).
	paperStopTakerBps    = decimal.NewFromFloat(5)
	paperStopSlippageBps = decimal.NewFromFloat(5)
	// paperStopCostComposite = taker + slippage charged on inventory at a
	// stop/structural close.
	paperStopCostComposite = paperStopTakerBps.Add(paperStopSlippageBps)
)

// exchangeStopClass reports whether the close reason is an executed stop:
// either the exchange's own stop (STOP_LOSS_NATIVE, liquidations) or our
// manage-stop submitted at total ≤ −max_loss. For these closes the exchange
// executed AT the stop, so the last pre-stop telemetry mark (which can lag
// the trigger by minutes on a fast tape) must be floored at the stop level.
func exchangeStopClass(reason string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(reason))
	switch {
	case normalized == "STOP_LOSS",
		normalized == "STOP_LOSS_NATIVE",
		normalized == "LIQUIDATION",
		normalized == "FORCE_LIQUIDATION",
		normalized == "LOSS_STOP":
		return true
	}
	return false
}

// terminalTelemetryEstimate is the pure v2.0.89 terminal-final formula for a
// bot the exchange did not net (no profitExited / total-alias):
//
//	final = last_telemetry_total − close_cost(taker 0.05% + slip 0.05% on
//	         last inventory notional)
//
// with a stop floor for executed stops: the stop fired at −max_loss, so the
// final can never be better than −max_loss − close_cost. The floor closed the
// epoch's real gap (prod 2026-09-04: APT's native stop executed at −12 while
// the last telemetry mark still read −2.27 — the mark-to-execution leg of the
// wallet's −$31 that telemetry alone cannot see). closeCost is returned so the
// caller can persist it next to the figure.
func terminalTelemetryEstimate(
	telemetryTotal, inventoryNotional, maxLoss decimal.Decimal,
	closedReason string,
) (final, closeCost decimal.Decimal) {
	closeCost = pionex.CloseCostUSDT(inventoryNotional)
	final = telemetryTotal.Sub(closeCost)
	if exchangeStopClass(closedReason) && maxLoss.IsPositive() {
		floor := maxLoss.Neg().Sub(closeCost)
		if final.GreaterThan(floor) {
			final = floor
		}
	}
	return final, closeCost
}

// telemetrySnapshot is the last observation row before a terminal close.
type telemetrySnapshot struct {
	TotalPnl          decimal.Decimal
	InventoryNotional decimal.Decimal
	CapturedAt        time.Time
}

// lastTelemetrySnapshotBefore loads the freshest telemetry row at-or-before
// the anchor (fresh-window preference falls out of the DESC ordering), or nil
// when the bot has no telemetry at all — "no telemetry → no estimate".
func lastTelemetrySnapshotBefore(
	ctx context.Context, db *pgxpool.Pool, botID string, anchor time.Time,
) (*telemetrySnapshot, error) {
	snap := &telemetrySnapshot{}
	err := db.QueryRow(ctx, `
		SELECT total_pnl, COALESCE(inventory_notional, 0), captured_at
		FROM bot_telemetry
		WHERE bot_id = $1 AND captured_at <= $2
		ORDER BY captured_at DESC
		LIMIT 1
	`, botID, anchor).Scan(&snap.TotalPnl, &snap.InventoryNotional, &snap.CapturedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("telemetry snapshot probe for %s: %w", botID, err)
	}
	return snap, nil
}

// estimateTerminalFinal settles a terminal bot that carries no exchange-total
// figure: telemetry_last_total − estimated close cost, stop-floored. A NULL
// anchor or missing telemetry returns (nil, 0, nil) — the caller leaves the
// final NULL rather than inventing a figure.
func estimateTerminalFinal(
	ctx context.Context, db *pgxpool.Pool, botID string, anchor *time.Time,
	maxLoss decimal.Decimal, closedReason string,
) (*decimal.Decimal, decimal.Decimal, error) {
	if anchor == nil {
		return nil, decimal.Zero, nil
	}
	snap, err := lastTelemetrySnapshotBefore(ctx, db, botID, *anchor)
	if err != nil {
		return nil, decimal.Zero, err
	}
	if snap == nil {
		return nil, decimal.Zero, nil
	}
	final, closeCost := terminalTelemetryEstimate(
		snap.TotalPnl, snap.InventoryNotional, maxLoss, closedReason)
	return &final, closeCost, nil
}

// recordRealEntryFee durably books the taker entry fee of a REAL deploy into
// grid_bots.fees_paid_usdt + the model_state fee breakdown. The fee itself is
// priced by pionex.EntryFeeUSDT (taker 0.05% × investment × leverage — ONE
// model shared with the invest_in pours). The write is additive
// (fees_paid_usdt = fees_paid_usdt + $fee) so a deploy fee and later pours
// stack. A failed write is returned, never swallowed — the ledger's fee leg
// must not silently miss a deploy.
func recordRealEntryFee(
	ctx context.Context, db *pgxpool.Pool, botID string, fee decimal.Decimal, feeKey string,
) error {
	if !fee.IsPositive() {
		return nil
	}
	tag, err := db.Exec(ctx, `
		UPDATE grid_bots
		SET fees_paid_usdt = fees_paid_usdt + $2::NUMERIC,
		    model_state = jsonb_set(
		        COALESCE(model_state, '{}'::jsonb),
		        ARRAY[$3::TEXT],
		        to_jsonb(((COALESCE((model_state->>$3::TEXT)::NUMERIC, 0)) + $2::NUMERIC)::TEXT)),
		    updated_at = NOW()
		WHERE id = $1
	`, botID, fee.Round(8).String(), feeKey)
	if err != nil {
		return fmt.Errorf("persist entry fee for %s: %w", botID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("persist entry fee for %s: bot row not found", botID)
	}
	return nil
}

// terminalRefusalError reports whether an order-detail error is the exchange
// REFUSING a finished grid (not_found / already closed / forbidden-state
// class) rather than a transport failure. The distinction decides whether a
// backfill row may settle from local telemetry (refusal = the grid is
// finished, no exchange total will ever come) or must retry next pass
// (transport = the answer is still unknown, an estimate could shadow a
// profitExited still owed to us).
func terminalRefusalError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "not_found") || strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "already_closed") || strings.Contains(errStr, "already closed") ||
		strings.Contains(errStr, "not_exist") || strings.Contains(errStr, "404") ||
		strings.Contains(errStr, "invalid_order") ||
		strings.Contains(errStr, "forbidden current state") || strings.Contains(errStr, "can not cancel")
}
