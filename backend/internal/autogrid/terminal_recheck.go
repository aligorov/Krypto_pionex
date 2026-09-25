package autogrid

import (
	"context"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.99: a terminal final estimated from our telemetry chain
// (finalProfitSource=telemetry_net_close / NONE) is a placeholder, not the
// exchange's truth. The ARB #1286 incident: our estimate +2.23 vs the app's
// netted +1.66 (grid +2.01 + trend −0.345) — the finished-list record was
// not reachable at settle time, and the old one-shot settle froze the
// estimate forever. Rows now land in TERMINAL_FINAL_PENDING_EXCHANGE and
// this sweep re-probes the finished list until the exchange total surfaces
// or the window closes (48h), then freezes whatever is the best known.
const (
	TerminalFinalPendingExchange = "TERMINAL_FINAL_PENDING_EXCHANGE"

	// How long a pending row keeps being re-probed. The finished-list
	// horizon on the exchange side is the practical bound: records older
	// than this have never surfaced for any probe in prod history.
	terminalRecheckWindow = 48 * time.Hour
	// Sweep throttle: the finished list is paged REST, so pending rows cost
	// real calls. Once per 5 minutes is ample for a finalization lag of
	// seconds-to-minutes.
	terminalRecheckInterval = 5 * time.Minute
	// Rows probed per sweep — burst protection on the REST budget.
	terminalRecheckBatch = 5
)

// pendingTerminalFinal is one STOPPED row awaiting the exchange's settled
// total.
type pendingTerminalFinal struct {
	id, accountID, remoteID, symbol, closedReason string
	botNumber                                     int
	currentRealized                               string
}

// pendingOrConfirmedRecon maps a terminal decision marker to the row's
// reconciliation state: only a real exchange total confirms terminally.
func pendingOrConfirmedRecon(marker string) string {
	if marker == string(pionex.FinalProfitTelemetryNetClose) ||
		marker == string(pionex.FinalProfitNone) {
		return TerminalFinalPendingExchange
	}
	return "REMOTE_TERMINAL_CONFIRMED"
}

// recheckPendingExchangeFinals overwrites estimate-class finals with the
// exchange's netted total once the finished record surfaces. Runs on the
// single manage goroutine, throttled to terminalRecheckInterval.
func (worker *Worker) recheckPendingExchangeFinals(ctx context.Context, settings Settings) {
	// v2.0.99 upgrade heal (once per process): rows the OLD binary froze as
	// telemetry estimates under REMOTE_TERMINAL_CONFIRMED reopen as pending,
	// so the sweep can still fetch their exchange truth inside the window
	// (the ARB #1286 class that already shipped before this fix).
	if !worker.terminalReopenDone {
		worker.terminalReopenDone = true
		tag, err := worker.db.Exec(ctx, `
			UPDATE grid_bots
			SET reconciliation_state = $2, updated_at = NOW()
			WHERE autogrid_settings_id = $1
			  AND status = 'STOPPED'
			  AND reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED'
			  AND COALESCE(model_state->>'finalProfitSource', '') IN ('telemetry_net_close', 'none')
			  AND closed_at > NOW() - INTERVAL '48 hours'
		`, settings.ID, TerminalFinalPendingExchange)
		if err != nil {
			worker.logger.Warn("terminal final upgrade reopen failed", "component", "autogrid_worker", "error", err)
		} else if tag.RowsAffected() > 0 {
			worker.logger.Info("estimate-class terminal finals reopened for exchange re-check",
				"component", "autogrid_worker", "rows", tag.RowsAffected())
		}
	}

	if !worker.terminalRecheckAt.IsZero() && time.Since(worker.terminalRecheckAt) < terminalRecheckInterval {
		return
	}
	worker.terminalRecheckAt = time.Now()

	// Freeze rows that outlived the re-check window: the estimate is then
	// the best figure that will ever exist for them.
	tag, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED', updated_at = NOW()
		WHERE autogrid_settings_id = $1
		  AND status = 'STOPPED'
		  AND reconciliation_state = $2
		  AND closed_at < NOW() - INTERVAL '48 hours'
	`, settings.ID, TerminalFinalPendingExchange)
	if err != nil {
		worker.logger.Warn("terminal final freeze sweep failed", "component", "autogrid_worker", "error", err)
	} else if tag.RowsAffected() > 0 {
		worker.logger.Warn("terminal finals frozen as telemetry estimates after the re-check window",
			"component", "autogrid_worker", "rows", tag.RowsAffected(),
			"window", terminalRecheckWindow.String())
	}

	rows, err := worker.db.Query(ctx, `
		SELECT id, account_id, bu_order_id, COALESCE(bot_number, 0), symbol,
		       COALESCE(closed_reason, ''), COALESCE(realized_pnl_usdt, 0)::TEXT
		FROM grid_bots
		WHERE autogrid_settings_id = $1
		  AND status = 'STOPPED'
		  AND reconciliation_state = $2
		  AND bu_order_id IS NOT NULL
		ORDER BY closed_at
		LIMIT $3
	`, settings.ID, TerminalFinalPendingExchange, terminalRecheckBatch)
	if err != nil {
		worker.logger.Warn("terminal final recheck query failed", "component", "autogrid_worker", "error", err)
		return
	}
	pending := make([]pendingTerminalFinal, 0, terminalRecheckBatch)
	for rows.Next() {
		var item pendingTerminalFinal
		if err := rows.Scan(&item.id, &item.accountID, &item.remoteID, &item.botNumber,
			&item.symbol, &item.closedReason, &item.currentRealized); err != nil {
			rows.Close()
			worker.logger.Warn("terminal final recheck scan failed", "component", "autogrid_worker", "error", err)
			return
		}
		pending = append(pending, item)
	}
	rows.Close()
	if len(pending) == 0 {
		return
	}

	clients := make(map[string]*pionex.Client)
	for _, item := range pending {
		client, ok := clients[item.accountID]
		if !ok {
			resolved, err := worker.service.PrivateClient(ctx, worker.accounts, item.accountID)
			if err != nil {
				worker.logger.Warn("terminal final recheck: client resolve failed",
					"component", "autogrid_worker", "bot_id", item.id, "error", err)
				continue
			}
			client = resolved
			clients[item.accountID] = client
		}
		finished, rawPayload := findFinishedGridRaw(ctx, client, item.remoteID)
		if finished == nil {
			// Stays pending — either the record has not surfaced yet or the
			// list probe failed (findFinishedGridRaw logs its own
			// visibility line). Next sweep retries within the window.
			continue
		}
		settled, source := finished.SettledProfit()
		if source == pionex.FinalProfitNone {
			// v2.0.100: the record exists but carries none of the settle
			// carriers. Log the RAW payload once per row — a live witness of
			// the finished-record field shapes — and keep the row pending:
			// totals have never been present on ANY of the 32 prod closures,
			// so the next parser leg (unlock identity etc.) gets pinned from
			// this exact line instead of from doc guesses. The 48h window
			// still freezes what never resolves.
			if worker.terminalRawLogged == nil {
				worker.terminalRawLogged = make(map[string]bool)
			}
			if !worker.terminalRawLogged[item.id] {
				worker.terminalRawLogged[item.id] = true
				worker.logger.Warn("terminal final: finished record carries no settle carrier — raw payload witness",
					"component", "autogrid_worker", "bot_number", item.botNumber,
					"symbol", item.symbol, "raw", truncateForLog(string(rawPayload)))
			}
			continue
		}
		if gated := gateSettledProfit(settled, source, item.closedReason, strings.TrimSpace(finished.ReasonBy)); gated != nil {
			settled = *gated
		} else {
			worker.logger.Warn("terminal final recheck: exchange total refused by sanity gate — estimate kept",
				"component", "autogrid_worker", "bot_number", item.botNumber, "symbol", item.symbol,
				"exchange_total", settled.StringFixed(4), "stored_reason", item.closedReason,
				"exchange_reason", finished.ReasonBy)
			worker.confirmTerminalFinal(ctx, item, source)
			continue
		}
		worker.applyExchangeFinal(ctx, item, settled, source)
	}
}

// applyExchangeFinal overwrites the estimate with the exchange's netted
// total. The guard re-checks the pending state so nothing can clobber a
// confirmed row.
func (worker *Worker) applyExchangeFinal(ctx context.Context, item pendingTerminalFinal, settled decimal.Decimal, source pionex.FinalProfitSource) {
	tag, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET realized_pnl_usdt = $2::NUMERIC,
		    unrealized_pnl_usdt = 0,
		    reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED',
		    model_state = COALESCE(model_state, '{}'::jsonb)
		        || jsonb_build_object('finalProfitSource', to_jsonb($3::TEXT),
		                              'finalUsdtExchange', to_jsonb($2::NUMERIC)),
		    last_error = NULL, updated_at = NOW()
		WHERE id = $1 AND reconciliation_state = $4
	`, item.id, settled, string(source), TerminalFinalPendingExchange)
	if err != nil {
		worker.logger.Error("terminal final correction persist failed",
			"component", "autogrid_worker", "bot_id", item.id, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return
	}
	worker.logger.Info("terminal final corrected to exchange truth",
		"component", "autogrid_worker", "bot_number", item.botNumber, "symbol", item.symbol,
		"estimate_was", item.currentRealized, "exchange_final", settled.StringFixed(4),
		"final_profit_source", string(source))
	_ = QueueTelegramEvent(ctx, worker.db, "TERMINAL_FINAL_CORRECTED", map[string]any{
		"bot_number": item.botNumber, "symbol": item.symbol,
		"estimate_was": item.currentRealized, "exchange_final": settled.StringFixed(4),
	})
}

// confirmTerminalFinal freezes a pending row whose finished record offers no
// better figure than the estimate already stored.
func (worker *Worker) confirmTerminalFinal(ctx context.Context, item pendingTerminalFinal, source pionex.FinalProfitSource) {
	_, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET reconciliation_state = 'REMOTE_TERMINAL_CONFIRMED', updated_at = NOW()
		WHERE id = $1 AND reconciliation_state = $2
	`, item.id, TerminalFinalPendingExchange)
	if err != nil {
		worker.logger.Warn("terminal final confirm persist failed",
			"component", "autogrid_worker", "bot_id", item.id, "error", err)
		return
	}
	worker.logger.Info("terminal final confirmed at telemetry estimate — finished record carries no exchange total",
		"component", "autogrid_worker", "bot_number", item.botNumber, "symbol", item.symbol,
		"estimate", item.currentRealized, "probe_source", string(source))
}

// truncateForLog keeps raw payload witnesses bounded in structured logs.
func truncateForLog(s string) string {
	if len(s) > 900 {
		return s[:900]
	}
	return s
}
