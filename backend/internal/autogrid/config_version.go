package autogrid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ConfigVersion fingerprints the exact runtime configuration that produced a
// trading decision: the autogrid settings revision (moved by every operator
// edit AND every autotune round), the risk settings revision, the durable
// REAL execution gates, and the applied migration head (which is itself
// checksum-bound to the code). Written into entry_decisions and
// ofi_decision_snapshots so any later analysis can reproduce WHICH config
// decided — the "config version" leg of the decision journal (v2.0.138).
//
// Cheap by design: scalar reads, cached for a minute — the radar consults it
// per bot per pass.

type configVersionEntry struct {
	value     string
	fetchedAt time.Time
}

type configVersionCache struct {
	mu      sync.Mutex
	entries map[string]configVersionEntry
}

var cvCache = &configVersionCache{entries: make(map[string]configVersionEntry)}

// defaultSettingsID resolves the default-scope settings row lazily with a
// process-lifetime cache, so telemetry call sites that lack a Settings
// value (knife-pause, radar passes) can still stamp config versions
// without touching their signatures.
var defaultSettingsIDCache struct {
	mu   sync.Mutex
	id   string
	done bool
}

func defaultSettingsID(ctx context.Context, db *pgxpool.Pool) string {
	if db == nil {
		return ""
	}
	defaultSettingsIDCache.mu.Lock()
	defer defaultSettingsIDCache.mu.Unlock()
	if defaultSettingsIDCache.done {
		return defaultSettingsIDCache.id
	}
	var id string
	if err := db.QueryRow(ctx, `
		SELECT id FROM autogrid_settings WHERE scope_key = 'default' LIMIT 1
	`).Scan(&id); err != nil {
		return ""
	}
	defaultSettingsIDCache.id, defaultSettingsIDCache.done = id, true
	return id
}

// ConfigVersion returns "cv-<12 hex>" for the given autogrid settings row.
// Errors degrade to "cv-unknown" — the journal must never block a decision.
func ConfigVersion(ctx context.Context, db *pgxpool.Pool, settingsID string) string {
	if db == nil || settingsID == "" {
		return "cv-unknown"
	}
	cvCache.mu.Lock()
	if entry, ok := cvCache.entries[settingsID]; ok && time.Since(entry.fetchedAt) < time.Minute {
		cvCache.mu.Unlock()
		return entry.value
	}
	cvCache.mu.Unlock()

	// v2.0.143 (audit-2): only successful computations are cached. Caching
	// the "cv-unknown" degradation would let a transient DB blip stamp a
	// minute of decisions cv-unknown — the cached failure outlives the blip
	// by the full TTL. On error return the degraded marker WITHOUT storing,
	// so the next call retries immediately.
	value, err := computeConfigVersion(ctx, db, settingsID)
	if err != nil {
		return "cv-unknown"
	}

	cvCache.mu.Lock()
	cvCache.entries[settingsID] = configVersionEntry{value: value, fetchedAt: time.Now()}
	cvCache.mu.Unlock()
	return value
}

func computeConfigVersion(ctx context.Context, db *pgxpool.Pool, settingsID string) (string, error) {
	var settingsRev, riskRev string
	if err := db.QueryRow(ctx, `
		SELECT to_char(s.updated_at, 'YYYYMMDDHH24MISSUF'),
		       to_char(r.updated_at, 'YYYYMMDDHH24MISSUF')
		FROM autogrid_settings s
		CROSS JOIN risk_settings r
		WHERE s.id = $1 AND r.id = 1
	`, settingsID).Scan(&settingsRev, &riskRev); err != nil {
		return "", err
	}
	var realGateConfig, realGateFlag string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE((SELECT (value #>> '{}')::TEXT FROM app_config WHERE key = 'real_grid_execution_enabled'), ''),
		       COALESCE((SELECT enabled::TEXT FROM feature_flags WHERE name = 'real_native_grid'), '')
	`).Scan(&realGateConfig, &realGateFlag); err != nil {
		return "", err
	}
	var migrationHead string
	if err := db.QueryRow(ctx, `
		SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1
	`).Scan(&migrationHead); err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(settingsRev + "|" + riskRev + "|" + realGateConfig + "|" + realGateFlag + "|" + migrationHead))
	return "cv-" + hex.EncodeToString(sum[:])[:12], nil
}
