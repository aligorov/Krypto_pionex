package autogrid

import (
	"context"
	"github.com/shopspring/decimal"
)

// recordCloseIntent atomically captures the first close observation. A second
// writer may request the same exit, but cannot rewrite its financial baseline
// or attach a new observation to a terminal bot.
func (worker *Worker) recordCloseIntent(ctx context.Context, id, reason string, total decimal.Decimal) (bool, error) {
	tag, err := worker.db.Exec(ctx, `
        UPDATE grid_bots
        SET status = CASE WHEN status = 'RUNNING' THEN 'STOP_REQUESTED' ELSE status END,
            closed_reason = CASE WHEN status = 'RUNNING' THEN $2 ELSE closed_reason END,
            model_state = CASE WHEN model_state->>'stopIntentTotal' IS NOT NULL THEN model_state
                ELSE COALESCE(model_state, '{}'::jsonb) || jsonb_build_object(
                    'stopIntentTotal', $3::NUMERIC, 'stopIntentAt', NOW()) END,
            updated_at = NOW()
        WHERE id = $1 AND status IN ('RUNNING', 'STOP_REQUESTED', 'STOPPING')
    `, id, reason, total)
	return tag.RowsAffected() > 0, err
}

// recordCloseIntentFirstWriter is the escape-lane variant (v2.0.155, review
// SEC-003): it flips the row only from RUNNING, atomically — the pre-check
// SELECT + recordCloseIntent pair had a TOCTOU window in which a concurrent
// operator close left the row STOP_REQUESTED yet returned active=true, so
// the emergency lane paged twice and queued a DGT re-deploy behind an
// operator's deliberate close. First-writer semantics: only the writer that
// actually flipped RUNNING stamps the baseline.
func (worker *Worker) recordCloseIntentFirstWriter(ctx context.Context, id, reason string, total decimal.Decimal) (bool, error) {
	tag, err := worker.db.Exec(ctx, `
        UPDATE grid_bots
        SET status = 'STOP_REQUESTED', closed_reason = $2,
            model_state = COALESCE(model_state, '{}'::jsonb) || jsonb_build_object(
                'stopIntentTotal', $3::NUMERIC, 'stopIntentAt', NOW()),
            updated_at = NOW()
        WHERE id = $1 AND status = 'RUNNING'
    `, id, reason, total)
	return tag.RowsAffected() > 0, err
}
