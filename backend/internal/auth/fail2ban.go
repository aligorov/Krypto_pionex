package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	DefaultMaxFailedAttempts = 5
	DefaultFindTime          = 10 * time.Minute
	DefaultBanDuration       = 15 * time.Minute
)

var (
	ErrIPBanned           = errors.New("ip address temporarily banned due to excessive failed attempts")
	ErrInvalidIPOrCIDR    = errors.New("invalid IP or CIDR network format")
	ErrCannotDeleteDefault = errors.New("cannot delete default loopback whitelist entry")
)

type IPBan struct {
	IP             string     `json:"ip"`
	FailedAttempts int        `json:"failedAttempts"`
	FirstFailedAt  time.Time  `json:"firstFailedAt"`
	LastFailedAt   time.Time  `json:"lastFailedAt"`
	BannedUntil    *time.Time `json:"bannedUntil"`
	Reason         string     `json:"reason"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type WhitelistEntry struct {
	ID          int64     `json:"id"`
	IPOrCIDR    string    `json:"ipOrCidr"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Fail2Ban struct {
	db          *pgxpool.Pool
	maxAttempts int
	findTime    time.Duration
	banDuration time.Duration

	mu         sync.RWMutex
	whitelist  []*net.IPNet
	whiteExact map[string]struct{}
}

func NewFail2Ban(db *pgxpool.Pool) *Fail2Ban {
	f := &Fail2Ban{
		db:          db,
		maxAttempts: DefaultMaxFailedAttempts,
		findTime:    DefaultFindTime,
		banDuration: DefaultBanDuration,
		whiteExact:  make(map[string]struct{}),
	}
	_ = f.ReloadWhitelist(context.Background())
	return f
}

// ExtractClientIP extracts the real client IP, respecting proxy headers in order of authority.
func ExtractClientIP(r *http.Request) net.IP {
	// 1. Cloudflare header
	if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
		if ip := net.ParseIP(cfIP); ip != nil {
			return ip
		}
	}

	// 2. Nginx / reverse proxy X-Real-IP
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if ip := net.ParseIP(realIP); ip != nil {
			return ip
		}
	}

	// 3. X-Forwarded-For (client, proxy1, proxy2...)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			trimmed := strings.TrimSpace(p)
			if ip := net.ParseIP(trimmed); ip != nil {
				return ip
			}
		}
	}

	// 4. RemoteAddr fallback
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// ReloadWhitelist refreshes in-memory parsed CIDRs and exact IPs from PostgreSQL.
func (f *Fail2Ban) ReloadWhitelist(ctx context.Context) error {
	if f.db == nil {
		return nil
	}
	rows, err := f.db.Query(ctx, `SELECT ip_or_cidr FROM ip_whitelist`)
	if err != nil {
		return fmt.Errorf("query ip_whitelist: %w", err)
	}
	defer rows.Close()

	var subnets []*net.IPNet
	exact := make(map[string]struct{})

	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			continue
		}
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		if strings.Contains(item, "/") {
			_, ipNet, err := net.ParseCIDR(item)
			if err == nil {
				subnets = append(subnets, ipNet)
			}
		} else {
			parsed := net.ParseIP(item)
			if parsed != nil {
				exact[parsed.String()] = struct{}{}
			}
		}
	}

	f.mu.Lock()
	f.whitelist = subnets
	f.whiteExact = exact
	f.mu.Unlock()

	return nil
}

// IsWhitelisted checks if the given IP matches any exact whitelist entry or CIDR subnet.
func (f *Fail2Ban) IsWhitelisted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	ipStr := ip.String()
	if _, ok := f.whiteExact[ipStr]; ok {
		return true
	}

	for _, subnet := range f.whitelist {
		if subnet.Contains(ip) {
			return true
		}
	}

	return false
}

// CheckIP determines if an IP is currently banned. Returns banned=true and ban expiration time if active.
func (f *Fail2Ban) CheckIP(ctx context.Context, ip net.IP) (bool, time.Time, error) {
	if ip == nil || f.IsWhitelisted(ip) || f.db == nil {
		return false, time.Time{}, nil
	}

	var bannedUntil *time.Time
	err := f.db.QueryRow(ctx, `
		SELECT banned_until
		FROM ip_bans
		WHERE ip = $1
	`, ip.String()).Scan(&bannedUntil)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, nil
		}
		return false, time.Time{}, fmt.Errorf("check ip ban: %w", err)
	}

	if bannedUntil != nil && bannedUntil.After(time.Now()) {
		return true, *bannedUntil, nil
	}

	return false, time.Time{}, nil
}

// RecordFailure registers a failed authentication attempt. Returns banned=true and until if threshold was exceeded.
func (f *Fail2Ban) RecordFailure(ctx context.Context, ip net.IP, username, reason string) (bool, time.Time, error) {
	if ip == nil || f.IsWhitelisted(ip) || f.db == nil {
		return false, time.Time{}, nil
	}

	ipStr := ip.String()
	now := time.Now()
	banUntil := now.Add(f.banDuration)

	// Log structured failure event for external fail2ban/journald/syslog parsers
	log.Printf("[AUTH_FAILURE] ip=%q user=%q reason=%q time=%q", ipStr, username, reason, now.Format(time.RFC3339))

	// Upsert failed attempts
	var (
		attempts      int
		firstFailedAt time.Time
		bannedUntil   *time.Time
	)

	err := f.db.QueryRow(ctx, `
		INSERT INTO ip_bans (ip, failed_attempts, first_failed_at, last_failed_at, reason)
		VALUES ($1, 1, $2, $2, $3)
		ON CONFLICT (ip) DO UPDATE SET
			failed_attempts = CASE
				WHEN ip_bans.last_failed_at < $2 - $4::interval THEN 1
				ELSE ip_bans.failed_attempts + 1
			END,
			first_failed_at = CASE
				WHEN ip_bans.last_failed_at < $2 - $4::interval THEN $2
				ELSE ip_bans.first_failed_at
			END,
			last_failed_at = $2,
			reason = $3
		RETURNING failed_attempts, first_failed_at, banned_until
	`, ipStr, now, reason, fmt.Sprintf("%d seconds", int(f.findTime.Seconds()))).Scan(
		&attempts, &firstFailedAt, &bannedUntil,
	)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("record ip failure: %w", err)
	}

	// Check if attempts exceeded threshold
	if attempts >= f.maxAttempts {
		_, err = f.db.Exec(ctx, `
			UPDATE ip_bans
			SET banned_until = $2
			WHERE ip = $1
		`, ipStr, banUntil)
		if err != nil {
			return false, time.Time{}, fmt.Errorf("update banned_until: %w", err)
		}
		log.Printf("[IP_BANNED] ip=%q attempts=%d until=%q", ipStr, attempts, banUntil.Format(time.RFC3339))
		return true, banUntil, nil
	}

	return false, time.Time{}, nil
}

// RecordSuccess clears failure count upon successful authentication.
func (f *Fail2Ban) RecordSuccess(ctx context.Context, ip net.IP) error {
	if ip == nil || f.db == nil {
		return nil
	}
	_, err := f.db.Exec(ctx, `DELETE FROM ip_bans WHERE ip = $1`, ip.String())
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("clear ip ban: %w", err)
	}
	return nil
}

// ListBans returns all currently banned or recorded IPs.
func (f *Fail2Ban) ListBans(ctx context.Context) ([]IPBan, error) {
	if f.db == nil {
		return []IPBan{}, nil
	}
	rows, err := f.db.Query(ctx, `
		SELECT ip, failed_attempts, first_failed_at, last_failed_at, banned_until, reason, created_at
		FROM ip_bans
		WHERE banned_until IS NOT NULL AND banned_until > NOW()
		ORDER BY banned_until DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query ip bans: %w", err)
	}
	defer rows.Close()

	var bans []IPBan
	for rows.Next() {
		var b IPBan
		if err := rows.Scan(
			&b.IP, &b.FailedAttempts, &b.FirstFailedAt, &b.LastFailedAt,
			&b.BannedUntil, &b.Reason, &b.CreatedAt,
		); err != nil {
			return nil, err
		}
		bans = append(bans, b)
	}
	return bans, nil
}

// UnbanIP manually removes a ban on an IP address.
func (f *Fail2Ban) UnbanIP(ctx context.Context, ip string) error {
	if f.db == nil {
		return nil
	}
	_, err := f.db.Exec(ctx, `DELETE FROM ip_bans WHERE ip = $1`, strings.TrimSpace(ip))
	if err != nil {
		return fmt.Errorf("unban ip: %w", err)
	}
	log.Printf("[IP_UNBANNED] ip=%q", ip)
	return nil
}

// ListWhitelist returns all whitelist entries.
func (f *Fail2Ban) ListWhitelist(ctx context.Context) ([]WhitelistEntry, error) {
	if f.db == nil {
		return []WhitelistEntry{}, nil
	}
	rows, err := f.db.Query(ctx, `
		SELECT id, ip_or_cidr, description, created_by, created_at
		FROM ip_whitelist
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query whitelist: %w", err)
	}
	defer rows.Close()

	var list []WhitelistEntry
	for rows.Next() {
		var w WhitelistEntry
		if err := rows.Scan(&w.ID, &w.IPOrCIDR, &w.Description, &w.CreatedBy, &w.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, w)
	}
	return list, nil
}

// AddWhitelist inserts a new IP or CIDR into the whitelist and updates memory cache.
func (f *Fail2Ban) AddWhitelist(ctx context.Context, ipOrCidr, description, createdBy string) error {
	item := strings.TrimSpace(ipOrCidr)
	if item == "" {
		return ErrInvalidIPOrCIDR
	}

	// Validate format
	if strings.Contains(item, "/") {
		_, _, err := net.ParseCIDR(item)
		if err != nil {
			return fmt.Errorf("%w: invalid CIDR subnet", ErrInvalidIPOrCIDR)
		}
	} else {
		ip := net.ParseIP(item)
		if ip == nil {
			return fmt.Errorf("%w: invalid IP address", ErrInvalidIPOrCIDR)
		}
	}

	if f.db == nil {
		return nil
	}

	_, err := f.db.Exec(ctx, `
		INSERT INTO ip_whitelist (ip_or_cidr, description, created_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (ip_or_cidr) DO UPDATE SET
			description = EXCLUDED.description
	`, item, strings.TrimSpace(description), createdBy)
	if err != nil {
		return fmt.Errorf("insert whitelist: %w", err)
	}

	_ = f.ReloadWhitelist(ctx)
	// If the IP was previously in bans table, remove it
	if !strings.Contains(item, "/") {
		_ = f.UnbanIP(ctx, item)
	}
	return nil
}

// RemoveWhitelist deletes a whitelist entry by ID.
func (f *Fail2Ban) RemoveWhitelist(ctx context.Context, id int64) error {
	if f.db == nil {
		return nil
	}

	var ipOrCidr string
	err := f.db.QueryRow(ctx, `SELECT ip_or_cidr FROM ip_whitelist WHERE id = $1`, id).Scan(&ipOrCidr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	if ipOrCidr == "127.0.0.1/32" || ipOrCidr == "::1/128" {
		return ErrCannotDeleteDefault
	}

	_, err = f.db.Exec(ctx, `DELETE FROM ip_whitelist WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete whitelist: %w", err)
	}

	_ = f.ReloadWhitelist(ctx)
	return nil
}
