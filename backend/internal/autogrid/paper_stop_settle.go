package autogrid

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

// Fleet-stop settlement (v2.0.45): stopping the autopilot used to flip every
// RUNNING paper bot to STOPPED with a frozen unrealized mark and no exit
// cost — rows that the full-history PnL card (v2.0.43) then sums as if they
// were final. A real Pionex cancel settles the same way any close does:
// inventory marked at the live price minus the taker+slippage exit fee.
// Funding is intentionally NOT accrued here — the exchange charges funding
// only to positions held at an 8h boundary, and a stop between boundaries
// owes nothing (mirrored by the manage loop's boundary-gated accrual).
//
// Bots whose live price is unavailable fall back to the legacy un-settled
// stop: a stop command must never fail because one delisted symbol has no
// ticker.
func (worker *Worker) settleAndStopPaperBots(ctx context.Context, settings Settings, status, reason string) error {
	priceBySymbol, priceErr := worker.priceMap(ctx)
	if priceErr != nil {
		worker.logger.Warn("fleet stop: ticker fetch failed, closing un-settled",
			"component", "autogrid_worker", "error", priceErr)
		return nil
	}

	rows, err := worker.db.Query(ctx, `
		SELECT id, symbol, direction, entry_price, leverage, quote_investment,
		       lower_price, upper_price, grid_num, last_grid_level,
		       realized_pnl_usdt, COALESCE(peak_pnl_usdt, 0)
		FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID)
	if err != nil {
		return err
	}
	type stopBot struct {
		id                       string
		symbol, direction        string
		entry                    decimal.Decimal
		leverage                 int
		investment, lower, upper decimal.Decimal
		gridNum                  int
		lastLevel                *int
		realized, peak           decimal.Decimal
	}
	bots := make([]stopBot, 0)
	for rows.Next() {
		var b stopBot
		if err := rows.Scan(
			&b.id, &b.symbol, &b.direction, &b.entry, &b.leverage, &b.investment,
			&b.lower, &b.upper, &b.gridNum, &b.lastLevel, &b.realized, &b.peak,
		); err != nil {
			rows.Close()
			return err
		}
		bots = append(bots, b)
	}
	rows.Close()

	feeRate := settings.FeeBps.Add(settings.SlippageBps).Div(decimal.NewFromInt(10000))
	pairFeeBps := decimal.NewFromFloat(pionexMakerFeeBps)
	settled, frozen := 0, 0
	for _, bot := range bots {
		price, ok := paperPriceFor(priceBySymbol, bot.symbol)
		if !ok || price.IsZero() {
			frozen++
			continue
		}
		unrealized := decimal.Zero
		exitNotional := decimal.Zero
		if bot.direction == "NEUTRAL" {
			currentLevel := gridLevelForPrice(bot.lower, bot.upper, bot.gridNum, price)
			previousLevel := currentLevel
			if bot.lastLevel != nil {
				previousLevel = *bot.lastLevel
			}
			var _, inventoryNotional decimal.Decimal
			_, unrealized, inventoryNotional = neutralGridPaperPNL(
				bot.lower, bot.upper, bot.gridNum, bot.investment, bot.leverage,
				previousLevel, currentLevel, price, pairFeeBps,
			)
			exitNotional = inventoryNotional
		} else if bot.entry.GreaterThan(decimal.Zero) {
			notional := bot.investment.Mul(decimal.NewFromInt(int64(bot.leverage)))
			entryCost := notional.Mul(feeRate)
			exitNotional = notional
			switch bot.direction {
			case "LONG":
				unrealized = notional.Mul(price.Div(bot.entry).Sub(decimal.NewFromInt(1))).Sub(entryCost)
			case "SHORT":
				unrealized = notional.Mul(decimal.NewFromInt(1).Sub(price.Div(bot.entry))).Sub(entryCost)
			}
		}
		if exitNotional.IsPositive() {
			unrealized = unrealized.Sub(exitNotional.Mul(feeRate))
		}
		total := bot.realized.Add(unrealized)
		if _, err := worker.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET status = $5, closed_reason = $2,
			    realized_pnl_usdt = $3, unrealized_pnl_usdt = 0,
			    peak_pnl_usdt = GREATEST(peak_pnl_usdt, $3),
			    mark_price = $4,
			    closed_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = 'RUNNING'
		`, bot.id, reason, total, price, status); err != nil {
			worker.logger.Warn("fleet stop: settle failed",
				"component", "autogrid_worker", "symbol", bot.symbol, "error", err)
			continue
		}
		settled++
	}
	// Anything left RUNNING (no price, or a failed settle) gets the legacy
	// un-settled close so the stop command always lands.
	tag, err := worker.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET status = $3, closed_reason = $2, closed_at = NOW(), updated_at = NOW()
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID, reason, status)
	if err == nil && tag.RowsAffected() > 0 {
		frozen += int(tag.RowsAffected())
	}
	worker.logger.Info("fleet stop settled paper bots",
		"component", "autogrid_worker", "settled", settled, "un_settled", frozen)
	return nil
}

// paperPriceFor resolves a Pionex symbol against the alias-filled price map
// (symbol, trimmed base, base_PERP and base.PERP keys) exactly like the
// manage loop's lookup.
func paperPriceFor(prices map[string]decimal.Decimal, symbol string) (decimal.Decimal, bool) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if p, ok := prices[sym]; ok {
		return p, true
	}
	trimmed := strings.TrimSuffix(strings.TrimSuffix(sym, "_PERP"), ".PERP")
	if p, ok := prices[trimmed]; ok {
		return p, true
	}
	if p, ok := prices[trimmed+"_PERP"]; ok {
		return p, true
	}
	p, ok := prices[trimmed+".PERP"]
	return p, ok
}
