package autogrid

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// v2.0.75 exchange-rejection cooldown: a FAILED create (403 forbidden at the
// create stage, BOT_INTERNAL_ERROR) must cool the symbol down for one hour —
// the lifecycle's FAILED_AUTHORITATIVE grid row IS the durable marker. Prod
// PUMP hammered 8 refused creates, one per scan window.
func TestDeployRealCreateRejectionCooldown(t *testing.T) {
	h := newRealDeployHarness(t, 5, "PUMPX_USDT_PERP")
	ctx := context.Background()
	h.cleanupSymbol(t, "PUMPX_USDT_PERP")
	scanID := h.seedAcceptedCandidate(t, "PUMPX_USDT_PERP")

	// One authoritative FAILED create 10 minutes ago (the lifecycle persists
	// exactly this shape when the exchange refuses futuresGrid/create).
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO grid_bots (
			account_id, autogrid_settings_id, symbol, status, direction,
			grid_type, lower_price, upper_price, grid_num, leverage,
			quote_investment, extra_margin, request_fingerprint,
			execution_mode, reconciliation_state, last_error, updated_at
		) VALUES (
			$1, $2, 'PUMPX_USDT_PERP', 'FAILED', 'NEUTRAL',
			'ARITHMETIC', 90, 110, 20, 2,
			100, 0, $3, 'REAL', 'FAILED_AUTHORITATIVE',
			'BOT_INTERNAL_ERROR', NOW() - INTERVAL '10 minutes'
		)
	`, h.account.ID, h.settings.ID, fmt.Sprintf("rej-%d", time.Now().UnixNano())); err != nil {
		t.Fatalf("seed failed create: %v", err)
	}

	if err := h.worker.deployReal(ctx, *h.settings, scanID, false); err != nil {
		t.Fatalf("deployReal: %v", err)
	}
	decision, reason := h.candidateRow(t, scanID, "PUMPX_USDT_PERP")
	if decision != "REJECTED" {
		t.Fatalf("candidate must be rejected during the cooldown, got %s", decision)
	}
	if !strings.Contains(reason, "биржа отклонила создание") {
		t.Fatalf("rejection must carry the exchange-rejection wording, got %q", reason)
	}
	if !strings.Contains(reason, "кулдаун 1ч") {
		t.Fatalf("rejection must name the 1h cooldown, got %q", reason)
	}

	// The same FAILED row aged beyond the hour no longer blocks: the symbol
	// state may have healed (maintenance window closed).
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots
		SET updated_at = NOW() - INTERVAL '90 minutes'
		WHERE account_id = $1 AND symbol = 'PUMPX_USDT_PERP' AND status = 'FAILED'
	`, h.account.ID); err != nil {
		t.Fatalf("age out the failed row: %v", err)
	}
	scanID2 := h.seedAcceptedCandidate(t, "PUMPX_USDT_PERP")
	decision2, reason2 := h.candidateRow(t, scanID2, "PUMPX_USDT_PERP")
	if decision2 == "REJECTED" && strings.Contains(reason2, "кулдаун 1ч") {
		t.Fatalf("an expired rejection must not cool the symbol down, got %q", reason2)
	}

	// A SUBMISSION_UNKNOWN outcome (transport fog, the bot may exist
	// remotely) must NOT arm the cooldown — retrying is the correct posture.
	if _, err := h.pool.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'SUBMISSION_UNKNOWN', reconciliation_state = 'REMOTE_OUTCOME_UNKNOWN',
		    updated_at = NOW() - INTERVAL '5 minutes'
		WHERE account_id = $1 AND symbol = 'PUMPX_USDT_PERP' AND status = 'FAILED'
	`, h.account.ID); err != nil {
		t.Fatalf("flip to submission unknown: %v", err)
	}
	scanID3 := h.seedAcceptedCandidate(t, "PUMPX_USDT_PERP")
	decision3, reason3 := h.candidateRow(t, scanID3, "PUMPX_USDT_PERP")
	if decision3 == "REJECTED" && strings.Contains(reason3, "кулдаун 1ч") {
		t.Fatalf("SUBMISSION_UNKNOWN must not arm the cooldown, got %q", reason3)
	}
}
