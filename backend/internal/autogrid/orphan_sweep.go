package autogrid

import (
	"context"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// v2.0.104 orphan sweep: a live exchange grid that our DB does not actively
// track runs with NO supervision — no stops, no radar, no rotation. The
// operator confirmed he opens nothing manually, yet the exchange showed a
// second AAVE 2x bot (our #684 AAVE 2x was batch-settled STOPPED on 09-05;
// if that fleet-stop cancel never landed, the exchange twin ran unsupervised
// for three weeks). This sweep is the exchange→us direction the unknown-
// submission reconciler never covered: list the exchange's running futures
// grids, diff against our active rows, and adopt every orphan so the manage
// loop takes it over immediately.
const (
	orphanSweepInterval = 15 * time.Minute
	// Resurrected/adopted rows carry these states so telemetry can tell the
	// story; the manage loop treats them exactly like any RUNNING bot.
	orphanResurrected = "ORPHAN_RESURRECTED"
	orphanAdopted     = "ORPHAN_ADOPTED"
)

// reconcileOrphanExchangeBots adopts exchange-side running grids missing
// from our active fleet. Runs on the manage goroutine, throttled.
func (worker *Worker) reconcileOrphanExchangeBots(ctx context.Context, settings Settings) {
	if !worker.orphanSweepAt.IsZero() && time.Since(worker.orphanSweepAt) < orphanSweepInterval {
		return
	}
	worker.orphanSweepAt = time.Now()

	accountID := settings.AccountID
	if accountID == nil || strings.TrimSpace(*accountID) == "" {
		// Unpinned settings (no managed account yet) — nothing to sweep.
		return
	}
	client, err := worker.service.PrivateClient(ctx, worker.accounts, *accountID)
	if err != nil {
		worker.logger.Warn("orphan sweep: client resolve failed", "component", "autogrid_worker", "error", err)
		return
	}

	remote := make([]pionex.BotOrder, 0, 32)
	token := ""
	for page := 0; page < 10; page++ {
		orders, next, listErr := client.ListBotOrders(ctx, "running", token)
		if listErr != nil {
			worker.logger.Warn("orphan sweep: running-list probe failed", "component", "autogrid_worker", "error", listErr)
			return
		}
		remote = append(remote, orders...)
		if next == "" {
			break
		}
		token = next
	}

	active := make(map[string]struct{}, 16)
	rows, err := worker.db.Query(ctx, `
		SELECT bu_order_id FROM grid_bots
		WHERE autogrid_settings_id = $1
		  AND bu_order_id IS NOT NULL
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, settings.ID)
	if err != nil {
		worker.logger.Warn("orphan sweep: active query failed", "component", "autogrid_worker", "error", err)
		return
	}
	for rows.Next() {
		var bu string
		if err := rows.Scan(&bu); err != nil {
			rows.Close()
			return
		}
		active[bu] = struct{}{}
	}
	rows.Close()

	for _, order := range remote {
		if _, ok := active[order.BUOrderID]; ok {
			continue
		}
		worker.adoptOrphan(ctx, settings, *accountID, order)
	}
}

// adoptOrphan brings one exchange-only grid under supervision. A row that
// exists in a terminal state is resurrected (its history stays); a row that
// never existed is inserted from the remote payload.
func (worker *Worker) adoptOrphan(ctx context.Context, settings Settings, accountID string, order pionex.BotOrder) {
	tag, err := worker.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'RUNNING', closed_reason = NULL, closed_at = NULL,
		    reconciliation_state = $2, last_error = NULL,
		    updated_at = NOW()
		WHERE bu_order_id = $1 AND status NOT IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, order.BUOrderID, orphanResurrected)
	if err != nil {
		worker.logger.Error("orphan sweep: resurrect failed", "component", "autogrid_worker",
			"bu_order_id", order.BUOrderID, "error", err)
		return
	}
	if tag.RowsAffected() > 0 {
		worker.logger.Warn("orphan sweep: terminal row resurrected — exchange twin still running",
			"component", "autogrid_worker", "bu_order_id", order.BUOrderID,
			"symbol", order.Base+"_"+order.Quote+"_PERP")
		_ = QueueTelegramEvent(ctx, worker.db, "EMERGENCY", map[string]any{
			"message": "Orphan resurrected: exchange grid " + order.BUOrderID +
				" (" + order.Base + ") was live while our row sat terminal — supervision restored",
		})
		return
	}

	// No terminal row matched: either it is genuinely unknown, or it is
	// already active (race with the main loop) — the active re-check keeps
	// the latter from double-inserting.
	var activeCount int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM grid_bots WHERE bu_order_id = $1
		  AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
	`, order.BUOrderID).Scan(&activeCount); err != nil || activeCount > 0 {
		return
	}

	data, decodeErr := order.FuturesGridData()
	if decodeErr != nil || data.Row <= 0 {
		worker.logger.Warn("orphan sweep: undecodable remote payload — alerting without adoption",
			"component", "autogrid_worker", "bu_order_id", order.BUOrderID, "error", decodeErr)
		_ = QueueTelegramEvent(ctx, worker.db, "EMERGENCY", map[string]any{
			"message": "Unknown exchange grid " + order.BUOrderID + " (" + order.Base + ") cannot be decoded — manual review",
		})
		return
	}
	symbol := strings.ToUpper(order.Base) + "_" + strings.ToUpper(order.Quote) + "_PERP"
	investment, hasInvestment := order.GridInvestment()
	if !hasInvestment || !investment.IsPositive() {
		investment = decimal.Zero
	}
	if _, err := worker.db.Exec(ctx, `
		INSERT INTO grid_bots (
			account_id, symbol, bu_order_id, status, direction, grid_type,
			lower_price, upper_price, grid_num, leverage, quote_investment,
			request_fingerprint, autogrid_settings_id, reconciliation_state,
			model_state
		) VALUES (
			$1, $2, $3, 'RUNNING', $4, $5,
			$6, $7, $8, $9, $10,
			md5($3::text), $11, $12,
			jsonb_build_object('adoptedOrphan', true, 'adoptedAt', NOW())
		)
	`, accountID, symbol, order.BUOrderID, adoptDirection(data.Trend), "GEOMETRIC",
		data.Top, data.Bottom, data.Row, adoptLeverage(data.Leverage), investment,
		settings.ID, orphanAdopted); err != nil {
		worker.logger.Error("orphan sweep: adopt insert failed", "component", "autogrid_worker",
			"bu_order_id", order.BUOrderID, "error", err)
		return
	}
	worker.logger.Warn("orphan sweep: unknown exchange grid adopted",
		"component", "autogrid_worker", "bu_order_id", order.BUOrderID,
		"symbol", symbol, "investment", investment.String())
	_ = QueueTelegramEvent(ctx, worker.db, "EMERGENCY", map[string]any{
		"message": "Orphan adopted: exchange grid " + order.BUOrderID + " (" + symbol +
			") was running unsupervised — now under fleet management",
	})
}

func adoptDirection(trend string) string {
	switch strings.ToLower(strings.TrimSpace(trend)) {
	case "long":
		return "LONG"
	case "short":
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}

func adoptLeverage(lev int) int {
	if lev <= 0 {
		return 1
	}
	return lev
}
