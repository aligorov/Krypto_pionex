package autogrid

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
)

// v2.0.98 real-time lane glue: the public WebSocket (INDEX topic) feeds
// fresh markPrice for the RUNNING fleet's symbols into every priceMap call,
// replacing the pass-start REST snapshot for those symbols. REST stays the
// source of truth for lifecycle reads — the lane only sharpens the marks,
// and a dead/stale lane degrades to the previous REST-only behavior with
// zero code-path divergence (overlay is skipped, snapshot stands).
const (
	// A mark older than this is treated as absent: the exchange pushes
	// INDEX continuously for subscribed symbols, so staleness here means
	// the lane is half-dead even if the socket survives.
	wsMarkMaxAge = 90 * time.Second
	// Subscription cap mirrors pionex.MaxStreamSymbols; keep the constant
	// local so the autogrid package doesn't leak the client type into tests.
	wsMaxFleetSymbols = 60
)

// startWSLane launches the advisory WebSocket lane once per process. It is
// intentionally fire-and-forget: Run maintains its own reconnect loop and
// never panics upward.
func (worker *Worker) startWSLane(ctx context.Context) {
	if worker.wsLane == nil {
		worker.wsLane = pionex.NewPublicStream("", worker.logger)
	}
	// v2.0.102: every ingested mark feeds the event-driven trigger; the
	// callback ships only a buffered-channel signal, all decisions stay on
	// the manage goroutine.
	worker.wsLane.SetMarkListener(worker.onRealtimeMark)
	go worker.wsLane.Run(ctx)
}

// syncWSSubscriptions aligns the lane's INDEX subscriptions with the
// RUNNING fleet (paper + real). Called once per manage pass: the diff makes
// a steady fleet free on the wire, and open/close events cost two frames.
func (worker *Worker) syncWSSubscriptions(ctx context.Context, settings Settings) {
	if worker.wsLane == nil {
		return
	}
	rows, err := worker.db.Query(ctx, `
		SELECT symbol FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
		UNION
		SELECT symbol FROM grid_bots
		WHERE autogrid_settings_id = $1 AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
		  AND bu_order_id IS NOT NULL
	`, settings.ID)
	if err != nil {
		worker.logger.Warn("ws lane symbol sync query failed", "component", "autogrid_worker", "error", err)
		return
	}
	symbols := make([]string, 0, 32)
	seen := make(map[string]struct{}, 32)
	for rows.Next() {
		var sym string
		if err := rows.Scan(&sym); err != nil {
			rows.Close()
			worker.logger.Warn("ws lane symbol sync scan failed", "component", "autogrid_worker", "error", err)
			return
		}
		if sym == "" {
			continue
		}
		if _, dup := seen[sym]; dup {
			continue
		}
		seen[sym] = struct{}{}
		if len(symbols) >= wsMaxFleetSymbols {
			break
		}
		symbols = append(symbols, sym)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		worker.logger.Warn("ws lane symbol sync rows failed", "component", "autogrid_worker", "error", err)
		return
	}
	worker.wsLane.SetSymbols(symbols)
}

// overlayWSMarks replaces REST-snapshot prices with fresh WebSocket marks for
// the subscribed symbols and replicates the alias keys priceMap maintains.
// Returns how many symbols were overlaid (for the pass log).
func (worker *Worker) overlayWSMarks(prices map[string]decimal.Decimal) int {
	if worker.wsLane == nil || len(prices) == 0 {
		return 0
	}
	keys := make([]string, 0, len(prices))
	for sym := range prices {
		keys = append(keys, sym)
	}
	overlaid := 0
	for _, sym := range keys {
		update, ok := worker.wsLane.Mark(sym)
		if !ok || !update.Fresh(wsMarkMaxAge) || !update.MarkPrice.IsPositive() {
			continue
		}
		if prices[sym].Equal(update.MarkPrice) {
			continue
		}
		prices[sym] = update.MarkPrice
		trimmed := trimPERPAlias(sym)
		prices[trimmed] = update.MarkPrice
		prices[trimmed+"_PERP"] = update.MarkPrice
		prices[trimmed+".PERP"] = update.MarkPrice
		overlaid++
	}
	return overlaid
}

func trimPERPAlias(sym string) string {
	out := sym
	for _, suffix := range []string{"_PERP", ".PERP"} {
		if len(out) > len(suffix) && out[len(out)-len(suffix):] == suffix {
			out = out[:len(out)-len(suffix)]
		}
	}
	return out
}
