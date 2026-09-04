package autogrid

import (
	"context"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// Wallet-equity truth (v2.0.75). The bots' own PnL attribution
// (profitReduce/profitExited → grid_bots.realized_pnl_usdt) never sees the
// entry/exit/invest_in fees — Pionex charges those straight to the futures
// wallet — so any epoch PnL summed from bot rows overstates reality by the
// accumulated fee bleed. The wallet itself is the only complete ledger:
// every manage pass (self-throttled to 5 minutes by the snapshot table)
// captures the USDT cross-margin equity from /uapi/v1/account/detail and the
// epoch PnL is equity_now − equity_first_snapshot.
const (
	// equitySnapshotMinSpacing is the capture throttle. The manage loop can
	// run as fast as every 15s; the durable table itself enforces the floor
	// so a restart cannot machine-gun the endpoint either.
	equitySnapshotMinSpacing = 5 * time.Minute
	// equityChangeLogThresholdUSDT gates the Info log: silent ticks stay
	// silent, a real wallet move (fee batch, close, funding) gets one line.
	equityChangeLogThresholdUSDT = 1.0
)

// captureAccountEquity snapshots the futures wallet's USDT equity for the
// managed account. Fail-open by design: a failing endpoint or a missing
// account must never disturb the manage pass that supervises real grids.
func (worker *Worker) captureAccountEquity(ctx context.Context, settings Settings) {
	accountID := settings.AccountID
	if accountID == nil {
		resolved, err := worker.service.resolveAccount(ctx)
		if err != nil || resolved == nil {
			return
		}
		accountID = resolved
	}
	// Durable throttle: the newest snapshot for THIS account younger than the
	// spacing means this pass has nothing to record.
	var lastAt *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(captured_at) FROM account_equity_snapshots WHERE account_id = $1
	`, *accountID).Scan(&lastAt); err == nil && lastAt != nil &&
		time.Since(*lastAt) < equitySnapshotMinSpacing {
		return
	}

	client, err := worker.service.PrivateClient(ctx, worker.accounts, *accountID)
	if err != nil {
		worker.logger.Warn("equity snapshot: private client unavailable",
			"component", "autogrid_worker", "error", err)
		return
	}
	balances, err := client.GetFuturesAccountDetail(ctx)
	if err != nil {
		worker.logger.Warn("equity snapshot: account detail fetch failed",
			"component", "autogrid_worker", "error", err)
		return
	}
	var usdt *pionex.FuturesDetailBalance
	for i := range balances {
		if strings.EqualFold(balances[i].Coin, "USDT") {
			usdt = &balances[i]
			break
		}
	}
	if usdt == nil || !usdt.Assets.GreaterThan(decimal.Zero) {
		// An empty/zero futures wallet is a legitimate state (everything
		// parked on spot) — nothing to mark, and no error either.
		return
	}
	// Wallet equity = everything the wallet is worth right now: assets
	// (free + position/order margin) plus the floating PnL, net of debts.
	equity := usdt.Assets.Add(usdt.UnrealizedPnL)
	if usdt.Debts.IsPositive() {
		equity = equity.Sub(usdt.Debts)
	}

	if _, err := worker.db.Exec(ctx, `
		INSERT INTO account_equity_snapshots
			(account_id, equity_usdt, assets_usdt, available_usdt, unrealized_pnl_usdt)
		VALUES ($1, $2, $3, $4, $5)
	`, *accountID, equity, usdt.Assets, usdt.Available, usdt.UnrealizedPnL); err != nil {
		worker.logger.Warn("equity snapshot persist failed",
			"component", "autogrid_worker", "error", err)
		return
	}

	// Epoch accounting: the FIRST snapshot row is the epoch anchor — the
	// snapshots only exist since this feature runs, which is the only epoch
	// the wallet can prove. Log on the anchor and on any ≥$1 move.
	var firstEquity, prevEquity decimal.Decimal
	var firstAt time.Time
	hasPrev := true
	if err := worker.db.QueryRow(ctx, `
		SELECT equity_usdt FROM account_equity_snapshots
		WHERE account_id = $1 ORDER BY captured_at DESC LIMIT 1 OFFSET 1
	`, *accountID).Scan(&prevEquity); err != nil {
		hasPrev = false
	}
	if err := worker.db.QueryRow(ctx, `
		SELECT equity_usdt, captured_at FROM account_equity_snapshots
		WHERE account_id = $1 ORDER BY captured_at ASC LIMIT 1
	`, *accountID).Scan(&firstEquity, &firstAt); err != nil {
		return
	}
	epochPnL := equity.Sub(firstEquity)
	anchorLine := !hasPrev
	moved := hasPrev && equity.Sub(prevEquity).Abs().
		GreaterThanOrEqual(decimal.NewFromFloat(equityChangeLogThresholdUSDT))
	if anchorLine || moved {
		worker.logger.Info("epoch PnL (wallet truth)",
			"component", "autogrid_worker",
			"account_id", *accountID,
			"epoch_pnl_usdt", epochPnL.StringFixed(4),
			"equity_usdt", equity.StringFixed(4),
			"epoch_started_at", firstAt.UTC().Format(time.RFC3339),
			"delta_vs_prev", hasPrev && moved)
	}
}
