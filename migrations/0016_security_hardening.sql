-- Migration 0016: Security hardening (audit 2026-08-16, SEC-001 / SEC-008)
--
-- SEC-001: migration 0014 created an extra `postgres` superuser whose
-- password was committed to the repository. The application connects
-- exclusively as the bootstrap user (POSTGRES_USER), so the secondary
-- role must not accept logins. Operators who need a `postgres` admin
-- role can re-enable it with a private password of their own.

DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'postgres' AND rolcanlogin) THEN
        ALTER ROLE postgres NOLOGIN;
    END IF;
END
$$;

-- SEC-008: failed-attempt counter for the two-step confirmation of
-- dangerous trade commands (brute-force protection for the 6-digit code).
ALTER TABLE control_commands
    ADD COLUMN IF NOT EXISTS confirm_attempts INT NOT NULL DEFAULT 0;
