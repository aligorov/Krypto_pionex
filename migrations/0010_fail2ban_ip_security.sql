-- Migration 0010: Fail2ban IP security and Whitelist

CREATE TABLE IF NOT EXISTS ip_bans (
    ip VARCHAR(64) PRIMARY KEY,
    failed_attempts INT NOT NULL DEFAULT 1,
    first_failed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_failed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    banned_until TIMESTAMPTZ,
    reason VARCHAR(128) NOT NULL DEFAULT 'failed_login_attempts',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ip_bans_banned_until ON ip_bans (banned_until);

CREATE TABLE IF NOT EXISTS ip_whitelist (
    id BIGSERIAL PRIMARY KEY,
    ip_or_cidr VARCHAR(64) UNIQUE NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_by VARCHAR(64) NOT NULL DEFAULT 'system',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Default loopback and internal ranges
INSERT INTO ip_whitelist (ip_or_cidr, description, created_by)
VALUES 
    ('127.0.0.1/32', 'Localhost IPv4', 'system'),
    ('::1/128', 'Localhost IPv6', 'system')
ON CONFLICT (ip_or_cidr) DO NOTHING;
