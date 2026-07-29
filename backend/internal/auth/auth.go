package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	RoleViewer   = "VIEWER"
	RoleOperator = "OPERATOR"
	RoleAdmin    = "ADMIN"

	SessionCookieName = "pionex_session"
	CSRFCookieName    = "pionex_csrf"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrAccountLocked      = errors.New("account is temporarily locked")
	ErrInactiveUser       = errors.New("user is disabled")
	ErrUnauthorized       = errors.New("authentication required")
	ErrForbidden          = errors.New("insufficient permissions")

	usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)
)

type User struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	DisplayName        string     `json:"displayName"`
	Email              *string    `json:"email"`
	Role               string     `json:"role"`
	IsActive           bool       `json:"isActive"`
	MustChangePassword bool       `json:"mustChangePassword"`
	LockedUntil        *time.Time `json:"lockedUntil"`
	LastLoginAt        *time.Time `json:"lastLoginAt"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

type UserSettings struct {
	UserID           string         `json:"userId"`
	Language         string         `json:"language"`
	Timezone         string         `json:"timezone"`
	Theme            string         `json:"theme"`
	DefaultAccountID *string        `json:"defaultAccountId"`
	Preferences      map[string]any `json:"preferences"`
	UpdatedAt        time.Time      `json:"updatedAt"`
}

type Principal struct {
	UserID      string   `json:"userId"`
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Role        string   `json:"role"`
	Scopes      []string `json:"scopes"`
	ActorType   string   `json:"actorType"`
	SessionID   string   `json:"-"`
	CSRFHash    string   `json:"-"`
}

func (p Principal) HasRole(required string) bool {
	return roleRank(p.Role) >= roleRank(required)
}

func (p Principal) HasScope(required string) bool {
	if p.ActorType != "MCP" {
		return true
	}
	for _, scope := range p.Scopes {
		if scope == required || scope == "mcp:admin" {
			return true
		}
	}
	return false
}

type CreateUserInput struct {
	Username           string  `json:"username"`
	DisplayName        string  `json:"displayName"`
	Email              *string `json:"email"`
	Password           string  `json:"password"`
	Role               string  `json:"role"`
	MustChangePassword bool    `json:"mustChangePassword"`
}

type UpdateUserInput struct {
	DisplayName *string `json:"displayName"`
	Email       *string `json:"email"`
	Role        *string `json:"role"`
	IsActive    *bool   `json:"isActive"`
}

type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type Service struct {
	db  *pgxpool.Pool
	now func() time.Time
}

func NewService(db *pgxpool.Pool) *Service {
	return &Service{
		db:  db,
		now: time.Now,
	}
}

func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM app_users").Scan(&count); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (s *Service) CreateUser(ctx context.Context, input CreateUserInput) (*User, error) {
	username := strings.ToLower(strings.TrimSpace(input.Username))
	displayName := strings.TrimSpace(input.DisplayName)
	role := strings.ToUpper(strings.TrimSpace(input.Role))
	if !usernamePattern.MatchString(username) {
		return nil, errors.New("username must be 3-64 characters: lowercase letters, digits, dot, underscore or hyphen")
	}
	if displayName == "" {
		return nil, errors.New("display name is required")
	}
	if !validRole(role) {
		return nil, errors.New("role must be VIEWER, OPERATOR or ADMIN")
	}
	if err := validatePassword(input.Password); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	var email *string
	if input.Email != nil && strings.TrimSpace(*input.Email) != "" {
		normalized := strings.ToLower(strings.TrimSpace(*input.Email))
		email = &normalized
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin create user transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var user User
	err = tx.QueryRow(ctx, `
		INSERT INTO app_users (
			username, display_name, email, password_hash, role, must_change_password
		) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, username, display_name, email, role, is_active,
		          must_change_password, locked_until, last_login_at, created_at, updated_at
	`, username, displayName, email, string(hash), role, input.MustChangePassword).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role,
		&user.IsActive, &user.MustChangePassword, &user.LockedUntil,
		&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	_, err = tx.Exec(ctx, "INSERT INTO user_settings (user_id) VALUES ($1)", user.ID)
	if err != nil {
		return nil, fmt.Errorf("create user settings: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit create user transaction: %w", err)
	}
	return &user, nil
}

func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	normalized := strings.ToLower(strings.TrimSpace(username))
	var user User
	var passwordHash string
	var failedAttempts int
	err := s.db.QueryRow(ctx, `
		SELECT id, username, display_name, email, password_hash, role, is_active,
		       must_change_password, failed_login_attempts, locked_until,
		       last_login_at, created_at, updated_at
		FROM app_users
		WHERE username = $1
	`, normalized).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &passwordHash,
		&user.Role, &user.IsActive, &user.MustChangePassword, &failedAttempts,
		&user.LockedUntil, &user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoO5lP8iWMZUdKc4pF0yGXrW5pQ6G8hP6y"),
			[]byte(password),
		)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if !user.IsActive {
		return nil, ErrInactiveUser
	}
	if user.LockedUntil != nil && s.now().Before(*user.LockedUntil) {
		return nil, ErrAccountLocked
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		failedAttempts++
		var lockedUntil *time.Time
		if failedAttempts >= 5 {
			locked := s.now().Add(15 * time.Minute)
			lockedUntil = &locked
		}
		_, _ = s.db.Exec(ctx, `
			UPDATE app_users
			SET failed_login_attempts = $2, locked_until = $3, updated_at = NOW()
			WHERE id = $1
		`, user.ID, failedAttempts, lockedUntil)
		return nil, ErrInvalidCredentials
	}

	now := s.now()
	_, err = s.db.Exec(ctx, `
		UPDATE app_users
		SET failed_login_attempts = 0, locked_until = NULL, last_login_at = $2, updated_at = NOW()
		WHERE id = $1
	`, user.ID, now)
	if err != nil {
		return nil, fmt.Errorf("record successful login: %w", err)
	}
	user.LastLoginAt = &now
	user.LockedUntil = nil
	return &user, nil
}

func (s *Service) CreateSession(
	ctx context.Context,
	userID string,
	ip net.IP,
	userAgent string,
) (sessionToken string, csrfToken string, expiresAt time.Time, err error) {
	sessionToken, err = randomToken(32)
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrfToken, err = randomToken(24)
	if err != nil {
		return "", "", time.Time{}, err
	}
	var configuredHours int
	if loadErr := s.db.QueryRow(ctx, `
		SELECT (value #>> '{}')::INT FROM app_config WHERE key = 'session_ttl_hours'
	`).Scan(&configuredHours); loadErr != nil {
		return "", "", time.Time{}, fmt.Errorf("load session TTL: %w", loadErr)
	}
	if configuredHours < 1 || configuredHours > 720 {
		return "", "", time.Time{}, errors.New("session TTL must be between 1 and 720 hours")
	}
	expiresAt = s.now().Add(time.Duration(configuredHours) * time.Hour)
	_, err = s.db.Exec(ctx, `
		INSERT INTO user_sessions (
			user_id, token_hash, csrf_hash, expires_at, ip_address, user_agent
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, userID, hashToken(sessionToken), hashToken(csrfToken), expiresAt, nullableIP(ip), truncate(userAgent, 512))
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return sessionToken, csrfToken, expiresAt, nil
}

func (s *Service) ValidateSession(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrUnauthorized
	}
	var principal Principal
	err := s.db.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.role, s.id, s.csrf_hash
		FROM user_sessions s
		JOIN app_users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > NOW()
		  AND u.is_active = true
	`, hashToken(token)).Scan(
		&principal.UserID, &principal.Username, &principal.DisplayName,
		&principal.Role, &principal.SessionID, &principal.CSRFHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("validate session: %w", err)
	}
	principal.ActorType = "USER"
	_, _ = s.db.Exec(ctx, "UPDATE user_sessions SET last_seen_at = NOW() WHERE id = $1", principal.SessionID)
	return &principal, nil
}

func (s *Service) ValidateCSRF(principal Principal, token string) bool {
	if token == "" {
		return false
	}
	actual := []byte(hashToken(token))
	expected := []byte(principal.CSRFHash)
	return len(actual) == len(expected) && subtle.ConstantTimeCompare(actual, expected) == 1
}

func (s *Service) RevokeSession(ctx context.Context, sessionID string) error {
	_, err := s.db.Exec(ctx, "UPDATE user_sessions SET revoked_at = NOW() WHERE id = $1", sessionID)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, username, display_name, email, role, is_active,
		       must_change_password, locked_until, last_login_at, created_at, updated_at
		FROM app_users
		ORDER BY username
	`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(
			&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role,
			&user.IsActive, &user.MustChangePassword, &user.LockedUntil,
			&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Service) UpdateUser(ctx context.Context, userID string, input UpdateUserInput) (*User, error) {
	if input.Role != nil {
		role := strings.ToUpper(strings.TrimSpace(*input.Role))
		if !validRole(role) {
			return nil, errors.New("invalid role")
		}
		input.Role = &role
	}
	_, err := s.db.Exec(ctx, `
		UPDATE app_users
		SET display_name = COALESCE($2, display_name),
		    email = CASE WHEN $3::TEXT IS NULL THEN email ELSE NULLIF(lower(trim($3)), '') END,
		    role = COALESCE($4, role),
		    is_active = COALESCE($5, is_active),
		    updated_at = NOW()
		WHERE id = $1
	`, userID, input.DisplayName, input.Email, input.Role, input.IsActive)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	return s.GetUser(ctx, userID)
}

func (s *Service) GetUser(ctx context.Context, userID string) (*User, error) {
	var user User
	err := s.db.QueryRow(ctx, `
		SELECT id, username, display_name, email, role, is_active,
		       must_change_password, locked_until, last_login_at, created_at, updated_at
		FROM app_users WHERE id = $1
	`, userID).Scan(
		&user.ID, &user.Username, &user.DisplayName, &user.Email, &user.Role,
		&user.IsActive, &user.MustChangePassword, &user.LockedUntil,
		&user.LastLoginAt, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return &user, nil
}

func (s *Service) ChangePassword(ctx context.Context, userID, password string, mustChange bool) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin password transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE app_users
		SET password_hash = $2, must_change_password = $3,
		    failed_login_attempts = 0, locked_until = NULL, updated_at = NOW()
		WHERE id = $1
	`, userID, string(hash), mustChange); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE user_sessions SET revoked_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID); err != nil {
		return fmt.Errorf("revoke sessions after password change: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Service) GetUserSettings(ctx context.Context, userID string) (*UserSettings, error) {
	var settings UserSettings
	err := s.db.QueryRow(ctx, `
		SELECT user_id, language, timezone, theme, default_account_id, preferences, updated_at
		FROM user_settings WHERE user_id = $1
	`, userID).Scan(
		&settings.UserID, &settings.Language, &settings.Timezone, &settings.Theme,
		&settings.DefaultAccountID, &settings.Preferences, &settings.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get user settings: %w", err)
	}
	return &settings, nil
}

func (s *Service) UpdateUserSettings(ctx context.Context, settings UserSettings) (*UserSettings, error) {
	if settings.Language != "ru" && settings.Language != "en" {
		return nil, errors.New("language must be ru or en")
	}
	if settings.Theme != "dark" && settings.Theme != "light" && settings.Theme != "system" {
		return nil, errors.New("theme must be dark, light or system")
	}
	if strings.TrimSpace(settings.Timezone) == "" {
		return nil, errors.New("timezone is required")
	}
	if settings.Preferences == nil {
		settings.Preferences = map[string]any{}
	}
	_, err := s.db.Exec(ctx, `
		UPDATE user_settings
		SET language = $2, timezone = $3, theme = $4,
		    default_account_id = $5, preferences = $6, updated_at = NOW()
		WHERE user_id = $1
	`, settings.UserID, settings.Language, settings.Timezone, settings.Theme,
		settings.DefaultAccountID, settings.Preferences)
	if err != nil {
		return nil, fmt.Errorf("update user settings: %w", err)
	}
	return s.GetUserSettings(ctx, settings.UserID)
}

func (s *Service) CreateAPIToken(
	ctx context.Context,
	userID, name string,
	scopes []string,
	expiresAt *time.Time,
) (*APIToken, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", errors.New("token name is required")
	}
	if len(scopes) == 0 {
		scopes = []string{"mcp:read"}
	}
	for _, scope := range scopes {
		if !validScope(scope) {
			return nil, "", fmt.Errorf("invalid scope %q", scope)
		}
	}
	random, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	raw := "pxmcp_" + random
	prefix := truncate(raw, 18)
	var token APIToken
	err = s.db.QueryRow(ctx, `
		INSERT INTO api_tokens (user_id, name, token_prefix, token_hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, name, token_prefix, scopes, expires_at, revoked_at,
		          last_used_at, created_at
	`, userID, strings.TrimSpace(name), prefix, hashToken(raw), scopes, expiresAt).Scan(
		&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.ExpiresAt,
		&token.RevokedAt, &token.LastUsedAt, &token.CreatedAt,
	)
	if err != nil {
		return nil, "", fmt.Errorf("create API token: %w", err)
	}
	return &token, raw, nil
}

func (s *Service) ValidateAPIToken(ctx context.Context, raw string) (*Principal, error) {
	if !strings.HasPrefix(raw, "pxmcp_") {
		return nil, ErrUnauthorized
	}
	var principal Principal
	var tokenID string
	err := s.db.QueryRow(ctx, `
		SELECT u.id, u.username, u.display_name, u.role, t.id, t.scopes
		FROM api_tokens t
		JOIN app_users u ON u.id = t.user_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > NOW())
		  AND u.is_active = true
	`, hashToken(raw)).Scan(
		&principal.UserID, &principal.Username, &principal.DisplayName,
		&principal.Role, &tokenID, &principal.Scopes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("validate API token: %w", err)
	}
	principal.ActorType = "MCP"
	_, _ = s.db.Exec(ctx, "UPDATE api_tokens SET last_used_at = NOW() WHERE id = $1", tokenID)
	return &principal, nil
}

func (s *Service) ListAPITokens(ctx context.Context, userID string, all bool) ([]APIToken, error) {
	query := `
		SELECT id, name, token_prefix, scopes, expires_at, revoked_at, last_used_at, created_at
		FROM api_tokens
		WHERE ($1::BOOLEAN OR user_id = $2)
		ORDER BY created_at DESC
	`
	rows, err := s.db.Query(ctx, query, all, userID)
	if err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	defer rows.Close()
	tokens := make([]APIToken, 0)
	for rows.Next() {
		var token APIToken
		if err := rows.Scan(
			&token.ID, &token.Name, &token.Prefix, &token.Scopes, &token.ExpiresAt,
			&token.RevokedAt, &token.LastUsedAt, &token.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan API token: %w", err)
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Service) RevokeAPIToken(ctx context.Context, tokenID, userID string, admin bool) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE api_tokens
		SET revoked_at = NOW()
		WHERE id = $1 AND ($3::BOOLEAN OR user_id = $2) AND revoked_at IS NULL
	`, tokenID, userID, admin)
	if err != nil {
		return fmt.Errorf("revoke API token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("token not found or already revoked")
	}
	return nil
}

func validatePassword(password string) error {
	if len(password) < 12 {
		return errors.New("password must contain at least 12 characters")
	}
	if len(password) > 128 {
		return errors.New("password must contain at most 128 characters")
	}
	var hasLetter, hasDigit, hasSymbol bool
	for _, char := range password {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
			hasLetter = true
		case char >= '0' && char <= '9':
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	if !hasLetter || !hasDigit || !hasSymbol {
		return errors.New("password must contain letters, digits and symbols")
	}
	return nil
}

func validRole(role string) bool {
	return role == RoleViewer || role == RoleOperator || role == RoleAdmin
}

func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

func validScope(scope string) bool {
	switch scope {
	case "mcp:read", "mcp:write", "mcp:admin", "mcp:trade":
		return true
	default:
		return false
	}
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func nullableIP(ip net.IP) any {
	if ip == nil {
		return nil
	}
	return ip.String()
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
