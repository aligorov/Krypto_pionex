-- Transport liveness is independent of the number of liquidation events.
CREATE TABLE IF NOT EXISTS liquidation_feed_health (
    source TEXT PRIMARY KEY,
    connected BOOLEAN NOT NULL DEFAULT false,
    last_message_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
