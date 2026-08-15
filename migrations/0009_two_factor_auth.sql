-- Migration 0009: Two-Factor Authentication (2FA / TOTP RFC 6238)

ALTER TABLE app_users
    ADD COLUMN IF NOT EXISTS totp_secret TEXT,
    ADD COLUMN IF NOT EXISTS totp_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS totp_recovery_codes TEXT[];
