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
