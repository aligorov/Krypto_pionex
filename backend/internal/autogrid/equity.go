package autogrid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
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
	// equityFailureDedup bounds the EQUITY_CAPTURE_FAILED marker: one durable
	// event per hour per failure mode. The manage pass runs every ~15-60s,
	// so an undeduped marker would flood bot_execution_events.
	equityFailureDedup = time.Hour
)

// equityWallet is one captured USDT wallet state, source-agnostic: the
// primary /uapi/v1/account/detail row or the balances+positions fallback.
type equityWallet struct {
	assets        decimal.Decimal
	available     decimal.Decimal
	unrealizedPnL decimal.Decimal
	debts         decimal.Decimal
}

func (w *equityWallet) equity() decimal.Decimal {
	value := w.assets.Add(w.unrealizedPnL)
	if w.debts.IsPositive() {
		value = value.Sub(w.debts)
	}
	return value
}

// captureAccountEquity snapshots the futures wallet's USDT equity for the
// managed account. Fail-open by design: a failing endpoint or a missing
// account must never disturb the manage pass that supervises real grids —
// but never silent again (v2.0.80): every outcome that does NOT produce a
// snapshot row leaves a durable EQUITY_CAPTURE_FAILED marker, so an empty
// ledger is diagnosable from the DB instead of from docker stdout.
func (worker *Worker) captureAccountEquity(ctx context.Context, settings Settings) {
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
	// Durable throttle: the newest snapshot for THIS account younger than the
	// spacing means this pass has nothing to record.
	var lastAt *time.Time
	if err := worker.db.QueryRow(ctx, `
		SELECT MAX(captured_at) FROM account_equity_snapshots WHERE account_id = $1
	`, *accountID).Scan(&lastAt); err == nil && lastAt != nil &&
		time.Since(*lastAt) < equitySnapshotMinSpacing {
		return
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		// A broken throttle probe (missing table, connectivity) must not turn
		// into an unthrottled endpoint hammer: report and stand down this pass.
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
	wallet, walletSource, walletErr := worker.fetchEquityWallet(ctx, client)
	if walletErr != nil {
		worker.logger.Warn("equity snapshot: account detail fetch failed",
			"component", "autogrid_worker", "error", walletErr)
		worker.alertEquityCaptureFailure(ctx, "FETCH_FAILED", walletErr.Error())
		return
	}
	if wallet == nil {
		// No USDT row anywhere: either the detail endpoint decoded empty
		// (live-shape deviation — the v2.0.75–79 prod state that left the
		// ledger at 0 rows with zero traces) or the wallet is genuinely
		// empty. Either way the ledger is not being written: that is exactly
		// what the operator must see, once per hour, not never.
		worker.logger.Warn("equity snapshot: no USDT wallet decoded (empty balances)")
		worker.alertEquityCaptureFailure(ctx, "EMPTY_DECODE",
			"account/detail вернул без USDT-строки (пустой balances или декод мимо полей)")
		return
	}

	equity := wallet.equity()
	if _, err := worker.db.Exec(ctx, `
		INSERT INTO account_equity_snapshots
			(account_id, equity_usdt, assets_usdt, available_usdt, unrealized_pnl_usdt)
		VALUES ($1, $2, $3, $4, $5)
	`, *accountID, equity, wallet.assets, wallet.available, wallet.unrealizedPnL); err != nil {
		worker.logger.Warn("equity snapshot persist failed",
			"component", "autogrid_worker", "error", err)
		worker.alertEquityCaptureFailure(ctx, "PERSIST_FAILED", err.Error())
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
			"source", walletSource,
			"epoch_pnl_usdt", epochPnL.StringFixed(4),
			"equity_usdt", equity.StringFixed(4),
			"epoch_started_at", firstAt.UTC().Format(time.RFC3339),
			"delta_vs_prev", hasPrev && moved)
	}
}

// fetchEquityWallet builds the USDT wallet state. Primary source is
// /uapi/v1/account/detail (carries unrealizedPnL per the docs); when the
// detail payload decodes to no USDT row, the documented sibling endpoints
// /uapi/v1/account/balances + /uapi/v1/account/positions reconstruct the
// same figure (assets = free+frozen, floating PnL summed over positions).
// Both sources are Pionex-only and per the official spec (AGENTS.md rule 1).
func (worker *Worker) fetchEquityWallet(
	ctx context.Context, client *pionex.Client,
) (*equityWallet, string, error) {
	balances, err := client.GetFuturesAccountDetail(ctx)
	if err != nil {
		return nil, "", err
	}
	for i := range balances {
		if strings.EqualFold(balances[i].Coin, "USDT") && balances[i].Assets.GreaterThan(decimal.Zero) {
			return &equityWallet{
				assets:        balances[i].Assets,
				available:     balances[i].Available,
				unrealizedPnL: balances[i].UnrealizedPnL,
				debts:         balances[i].Debts,
			}, "account_detail", nil
		}
	}
	// Fallback: balances has no unrealizedPnL field; positions does.
	walletBalances, balErr := client.GetFuturesBalances(ctx)
	if balErr != nil {
		return nil, "", fmt.Errorf("detail empty, balances fallback failed: %w", balErr)
	}
	wallet := &equityWallet{}
	found := false
	for i := range walletBalances {
		if strings.EqualFold(walletBalances[i].Coin, "USDT") {
			wallet.assets = walletBalances[i].Free.Add(walletBalances[i].Frozen)
			wallet.available = walletBalances[i].Free
			wallet.debts = walletBalances[i].Debts
			found = wallet.assets.GreaterThan(decimal.Zero)
			break
		}
	}
	if !found {
		return nil, "", nil
	}
	positions, posErr := client.GetFuturesPositions(ctx)
	if posErr != nil {
		// Positions only refine the floating leg; the equity without it is
		// still the wallet truth minus the floating PnL — accept, but carry
		// the failure into the source tag so audits see the degradation.
		return wallet, "balances_no_positions", nil
	}
	for _, position := range positions {
		wallet.unrealizedPnL = wallet.unrealizedPnL.Add(position.UnrealizedPNL)
	}
	return wallet, "balances_positions", nil
}

// alertEquityCaptureFailure makes a dying equity capture visible outside
// docker stdout: one durable bot_execution_events marker plus a queued
// Telegram event, deduped to at most one per hour (the SHADOW_CAPTURE_FAILED
// pattern). Without it an empty account_equity_snapshots table is
// indistinguishable from a healthy one (v2.0.75–v2.0.79 prod: 0 rows, 0
// traces, the operator's TOTAL PnL card left without data).
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
		"message": fmt.Sprintf("TOTAL PnL: снапшоты кошелька не пишутся (%s): %s", reason, detail),
	})
}
