package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Migrate(ctx context.Context, db *pgxpool.Pool, directory string) error {
	if _, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(128) PRIMARY KEY,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}

	if err := recognizeLegacySchema(ctx, db); err != nil {
		return err
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read migrations directory %q: %w", directory, err)
	}
	names := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(directory, name)
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		checksumBytes := sha256.Sum256(contents)
		checksum := hex.EncodeToString(checksumBytes[:])

		var storedChecksum string
		err = db.QueryRow(ctx,
			"SELECT checksum FROM schema_migrations WHERE version = $1", name,
		).Scan(&storedChecksum)
		if err == nil {
			storedChecksum = strings.TrimSpace(storedChecksum)
			if storedChecksum != checksum && storedChecksum != "legacy" {
				return fmt.Errorf("migration %s checksum changed after application", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		tx, err := db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(contents)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)
		`, name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func recognizeLegacySchema(ctx context.Context, db *pgxpool.Pool) error {
	var migrationCount int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		return fmt.Errorf("count schema migrations: %w", err)
	}
	if migrationCount != 0 {
		return nil
	}

	var hasInitial, hasControl bool
	if err := db.QueryRow(ctx, `
		SELECT to_regclass('public.pionex_accounts') IS NOT NULL,
		       to_regclass('public.app_users') IS NOT NULL
	`).Scan(&hasInitial, &hasControl); err != nil {
		return fmt.Errorf("inspect legacy schema: %w", err)
	}
	if hasInitial {
		if _, err := db.Exec(ctx, `
			INSERT INTO schema_migrations (version, checksum)
			VALUES ('0001_initial.sql', 'legacy')
			ON CONFLICT (version) DO NOTHING
		`); err != nil {
			return fmt.Errorf("recognize initial schema: %w", err)
		}
	}
	if hasControl {
		if _, err := db.Exec(ctx, `
			INSERT INTO schema_migrations (version, checksum)
			VALUES ('0002_control_plane.sql', 'legacy')
			ON CONFLICT (version) DO NOTHING
		`); err != nil {
			return fmt.Errorf("recognize control plane schema: %w", err)
		}
	}
	return nil
}
