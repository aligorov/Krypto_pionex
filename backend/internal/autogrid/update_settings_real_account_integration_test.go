package autogrid

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fullInput mirrors the MCP/controlplane update payloads: every field copied
// from the stored settings except the two under test (mode + accountId).
func fullInput(settings Settings) UpdateSettingsInput {
	return UpdateSettingsInput{
		ExecutionMode:           settings.ExecutionMode,
		ScanMode:                settings.ScanMode,
		BudgetUSDT:              settings.BudgetUSDT,
		MaxActiveBots:           settings.MaxActiveBots,
		Leverage:                settings.Leverage,
		MinSharpe:               settings.MinSharpe,
		MinEVPct:                settings.MinEVPct,
		StopLossMode:            settings.StopLossMode,
		SmartPNLEnabled:         settings.SmartPNLEnabled,
		AdaptiveLeverageEnabled: settings.AdaptiveLeverageEnabled,
		DensityGridEnabled:      settings.DensityGridEnabled,
		CandleInterval:          settings.CandleInterval,
		LookbackCandles:         settings.LookbackCandles,
		MaxSymbolsPerScan:       settings.MaxSymbolsPerScan,
		ScanIntervalSeconds:     settings.ScanIntervalSeconds,
		MinVolume24h:            settings.MinVolume24h,
		MinVolatilityPct:        settings.MinVolatilityPct,
		MaxVolatilityPct:        settings.MaxVolatilityPct,
		MaxDrawdownPct:          settings.MaxDrawdownPct,
		MinProfitFactor:         settings.MinProfitFactor,
		FeeBps:                  settings.FeeBps,
		SlippageBps:             settings.SlippageBps,
		PnLTargetMode:           settings.PnLTargetMode,
		PnLTargetUSDT:           settings.PnLTargetUSDT,
		MaxLossUSDT:             settings.MaxLossUSDT,
		ManageIntervalSeconds:   settings.ManageIntervalSeconds,
		RangeBreakBufferPct:     settings.RangeBreakBufferPct,
		MaxAdjustmentsPerBot:    settings.MaxAdjustmentsPerBot,
		StopForecastMode:        settings.StopForecastMode,
		AIKitEnabled:            settings.AIKitEnabled,
		AIAutotuneEnabled:       settings.AIAutotuneEnabled,
		AIAutotuneInterval:      settings.AIAutotuneInterval,
	}
}

// (c)+(d) REAL mode without an explicit accountId: when exactly one enabled
// verified account exists the update must auto-resolve AND persist it (both
// the operator UI flow and MCP omit accountId); with no resolvable account
// the original honest error stands; PAPER never requires one (regression).
func TestUpdateSettingsRealAccountAutoResolve(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()

	service := NewService(pool, risk.NewEngine(pool))
	settings, err := service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	snapshotManualSettings(t, pool, settings.ID)

	// No resolvable account: the settings pointer is detached and every
	// leftover test account is disabled for the duration of this test.
	// max_active_bots is pinned to the migration seed: the shared snapshot
	// helper does not own that column, and a deploy test may have left 5
	// behind against the risk limit 3 restored from its own snapshot.
	var savedMaxActiveBots int
	if err := pool.QueryRow(ctx, `SELECT max_active_bots FROM autogrid_settings WHERE id = $1`, settings.ID).Scan(&savedMaxActiveBots); err != nil {
		t.Fatalf("snapshot max_active_bots: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET max_active_bots = $2 WHERE id = $1`, settings.ID, savedMaxActiveBots); err != nil {
			t.Errorf("restore max_active_bots: %v", err)
		}
	})
	_, _ = pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL, execution_mode = 'PAPER', max_active_bots = 1 WHERE id = $1`, settings.ID)
	// Reload AFTER the pin: fullInput copies the struct, not the row.
	settings, err = service.GetSettings(ctx)
	if err != nil {
		t.Fatalf("reload pinned settings: %v", err)
	}
	savedEnabled := make(map[string]bool)
	rows, err := pool.Query(ctx, `SELECT id, is_enabled FROM pionex_accounts`)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	for rows.Next() {
		var id string
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			t.Fatalf("scan account: %v", err)
		}
		savedEnabled[id] = enabled
	}
	rows.Close()
	t.Cleanup(func() {
		for id, enabled := range savedEnabled {
			if _, err := pool.Exec(ctx, `UPDATE pionex_accounts SET is_enabled = $2 WHERE id = $1`, id, enabled); err != nil {
				t.Errorf("restore account %s: %v", id, err)
			}
		}
	})
	for id := range savedEnabled {
		_, _ = pool.Exec(ctx, `UPDATE pionex_accounts SET is_enabled = false WHERE id = $1`, id)
	}

	// (c, no-account branch) REAL without accountId and nothing resolvable
	// keeps the original validation error.
	input := fullInput(*settings)
	input.ExecutionMode = "REAL"
	input.AccountID = nil
	_, err = service.UpdateSettings(ctx, input)
	if err == nil || !strings.Contains(err.Error(), "a verified Pionex account is required for REAL mode") {
		t.Fatalf("REAL without any resolvable account must keep the original error, got %v", err)
	}

	// (d) PAPER regression: no account needed.
	input = fullInput(*settings)
	input.ExecutionMode = "PAPER"
	input.AccountID = nil
	if _, err := service.UpdateSettings(ctx, input); err != nil {
		t.Fatalf("PAPER must not require an account: %v", err)
	}

	// (c, resolve branch) one enabled verified account exists → REAL without
	// accountId auto-resolves and PERSISTS it.
	accountService := accounts.NewService(pool)
	accountName := "integration-update-real-" + time.Now().Format("150405.000000000")
	_, _ = pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE name LIKE 'integration-update-real-%'`)
	account, err := accountService.Create(ctx, accounts.CreateInput{
		Name: accountName, APIKey: "itest-key", APISecret: "itest-secret",
		HasFuturesPermission: true, HasBotPermission: true,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Detach before delete: autogrid_settings.account_id has the FK.
		if _, err := pool.Exec(ctx, `UPDATE autogrid_settings SET account_id = NULL WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("detach settings account: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account_permission_health WHERE account_id = $1`, account.ID); err != nil {
			t.Errorf("cleanup permission health: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM pionex_accounts WHERE id = $1`, account.ID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})
	if _, err := pool.Exec(ctx, `
		UPDATE pionex_accounts
		SET is_enabled = true, has_read_permission = true, last_verified_at = NOW()
		WHERE id = $1
	`, account.ID); err != nil {
		t.Fatalf("verify account: %v", err)
	}

	input = fullInput(*settings)
	input.ExecutionMode = "REAL"
	input.AccountID = nil
	updated, err := service.UpdateSettings(ctx, input)
	if err != nil {
		t.Fatalf("REAL with one resolvable account must auto-resolve, got %v", err)
	}
	if updated.AccountID == nil || *updated.AccountID != account.ID {
		t.Fatalf("REAL update must persist the resolved account %s, got %v", account.ID, updated.AccountID)
	}
	if updated.ExecutionMode != "REAL" {
		t.Fatalf("update must switch the mode to REAL, got %s", updated.ExecutionMode)
	}
}
