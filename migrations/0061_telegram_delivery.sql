ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS last_error TEXT;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS sent_at TIMESTAMPTZ;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS telegram_message_id BIGINT;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS sent_parts INT NOT NULL DEFAULT 0;
ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS consecutive_failures INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_notification_delivery_due ON notification_outbox(status,scheduled_at,created_at);

CREATE TABLE IF NOT EXISTS telegram_poll_state (
    token_fingerprint TEXT PRIMARY KEY,
    last_update_id BIGINT NOT NULL DEFAULT 0,
    last_poll_at TIMESTAMPTZ,
    last_error TEXT,
    commands_registered BOOLEAN NOT NULL DEFAULT false,
    send_paused_until TIMESTAMPTZ
);

-- Recover recent legacy failures without replaying a historical backlog.
UPDATE notification_outbox SET status='PENDING', scheduled_at=NOW()
WHERE status='FAILED' AND attempts<8 AND created_at>NOW()-INTERVAL '24 hours';
