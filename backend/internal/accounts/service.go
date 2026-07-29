package accounts

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Account struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	KeyFingerprint       string     `json:"keyFingerprint"`
	IsEnabled            bool       `json:"isEnabled"`
	IsPaper              bool       `json:"isPaper"`
	HasReadPermission    bool       `json:"hasReadPermission"`
	HasFuturesPermission bool       `json:"hasFuturesPermission"`
	HasBotPermission     bool       `json:"hasBotPermission"`
	CapabilityStatus     string     `json:"capabilityStatus"`
	LastVerifiedAt       *time.Time `json:"lastVerifiedAt"`
	LastError            *string    `json:"lastError"`
	CreatedAt            time.Time  `json:"createdAt"`
	UpdatedAt            time.Time  `json:"updatedAt"`
}

type CreateInput struct {
	Name                 string `json:"name"`
	APIKey               string `json:"apiKey"`
	APISecret            string `json:"apiSecret"`
	IsPaper              bool   `json:"isPaper"`
	HasFuturesPermission bool   `json:"hasFuturesPermission"`
	HasBotPermission     bool   `json:"hasBotPermission"`
}

type UpdateInput struct {
	Name                 *string `json:"name"`
	APIKey               *string `json:"apiKey"`
	APISecret            *string `json:"apiSecret"`
	IsEnabled            *bool   `json:"isEnabled"`
	IsPaper              *bool   `json:"isPaper"`
	HasFuturesPermission *bool   `json:"hasFuturesPermission"`
	HasBotPermission     *bool   `json:"hasBotPermission"`
}

type Credentials struct {
	AccountID string
	APIKey    string
	APISecret string
}

type balanceReader interface {
	GetFuturesBalances(context.Context) ([]pionex.FuturesBalance, error)
}

type ClientFactory func(apiKey, apiSecret string) balanceReader

type Service struct {
	db            *pgxpool.Pool
	clientFactory ClientFactory
	random        io.Reader
}

func NewService(db *pgxpool.Pool) *Service {
	return NewServiceWithFactory(db, func(apiKey, apiSecret string) balanceReader {
		return pionex.NewClient("", apiKey, apiSecret)
	})
}

func NewServiceWithFactory(db *pgxpool.Pool, factory ClientFactory) *Service {
	return &Service{db: db, clientFactory: factory, random: rand.Reader}
}

func (s *Service) List(ctx context.Context) ([]Account, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, COALESCE(key_fingerprint, ''), is_enabled, is_paper,
		       has_read_permission, has_futures_permission, has_bot_permission,
		       capability_status, last_verified_at, last_error, created_at, updated_at
		FROM pionex_accounts ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list pionex accounts: %w", err)
	}
	defer rows.Close()
	items := make([]Account, 0)
	for rows.Next() {
		var item Account
		if err := rows.Scan(accountScanTargets(&item)...); err != nil {
			return nil, fmt.Errorf("scan pionex account: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) Get(ctx context.Context, accountID string) (*Account, error) {
	var item Account
	err := s.db.QueryRow(ctx, `
		SELECT id, name, COALESCE(key_fingerprint, ''), is_enabled, is_paper,
		       has_read_permission, has_futures_permission, has_bot_permission,
		       capability_status, last_verified_at, last_error, created_at, updated_at
		FROM pionex_accounts WHERE id = $1
	`, accountID).Scan(accountScanTargets(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("pionex account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("get pionex account: %w", err)
	}
	return &item, nil
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*Account, error) {
	if err := validateCreate(input); err != nil {
		return nil, err
	}
	keyMaterial, err := s.keyMaterial(ctx)
	if err != nil {
		return nil, err
	}
	apiKeyEncrypted, err := encrypt(keyMaterial, "api-key", input.APIKey, s.random)
	if err != nil {
		return nil, err
	}
	apiSecretEncrypted, err := encrypt(keyMaterial, "api-secret", input.APISecret, s.random)
	if err != nil {
		return nil, err
	}
	fingerprint := keyFingerprint(input.APIKey)
	var accountID string
	err = s.db.QueryRow(ctx, `
		INSERT INTO pionex_accounts (
			name, api_key_encrypted, api_secret_encrypted, key_fingerprint,
			is_enabled, is_paper, has_read_permission,
			has_futures_permission, has_bot_permission, capability_status
		) VALUES ($1, $2, $3, $4, false, $5, false, $6, $7, 'UNVERIFIED')
		RETURNING id
	`, strings.TrimSpace(input.Name), apiKeyEncrypted, apiSecretEncrypted, fingerprint,
		input.IsPaper, input.HasFuturesPermission, input.HasBotPermission,
	).Scan(&accountID)
	if err != nil {
		return nil, fmt.Errorf("create pionex account: %w", err)
	}
	return s.Get(ctx, accountID)
}

func (s *Service) Update(
	ctx context.Context,
	accountID string,
	input UpdateInput,
) (*Account, error) {
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if len(name) < 3 || len(name) > 64 {
			return nil, errors.New("account name must contain 3-64 characters")
		}
		input.Name = &name
	}
	if (input.APIKey == nil) != (input.APISecret == nil) {
		return nil, errors.New("apiKey and apiSecret must be replaced together")
	}
	var encryptedKey, encryptedSecret, fingerprint *string
	credentialsReplaced := input.APIKey != nil
	if credentialsReplaced {
		if err := validateCredentials(*input.APIKey, *input.APISecret); err != nil {
			return nil, err
		}
		keyMaterial, err := s.keyMaterial(ctx)
		if err != nil {
			return nil, err
		}
		keyValue, err := encrypt(keyMaterial, "api-key", *input.APIKey, s.random)
		if err != nil {
			return nil, err
		}
		secretValue, err := encrypt(keyMaterial, "api-secret", *input.APISecret, s.random)
		if err != nil {
			return nil, err
		}
		fingerprintValue := keyFingerprint(*input.APIKey)
		encryptedKey, encryptedSecret, fingerprint = &keyValue, &secretValue, &fingerprintValue
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE pionex_accounts
		SET name = COALESCE($2, name),
		    api_key_encrypted = COALESCE($3, api_key_encrypted),
		    api_secret_encrypted = COALESCE($4, api_secret_encrypted),
		    key_fingerprint = COALESCE($5, key_fingerprint),
		    is_enabled = COALESCE($6, is_enabled),
		    is_paper = COALESCE($7, is_paper),
		    has_futures_permission = COALESCE($8, has_futures_permission),
		    has_bot_permission = COALESCE($9, has_bot_permission),
		    has_read_permission = CASE WHEN $10 THEN false ELSE has_read_permission END,
		    capability_status = CASE WHEN $10 THEN 'UNVERIFIED' ELSE capability_status END,
		    last_verified_at = CASE WHEN $10 THEN NULL ELSE last_verified_at END,
		    last_error = CASE WHEN $10 THEN NULL ELSE last_error END,
		    updated_at = NOW()
		WHERE id = $1
	`, accountID, input.Name, encryptedKey, encryptedSecret, fingerprint,
		input.IsEnabled, input.IsPaper, input.HasFuturesPermission,
		input.HasBotPermission, credentialsReplaced)
	if err != nil {
		return nil, fmt.Errorf("update pionex account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New("pionex account not found")
	}
	return s.Get(ctx, accountID)
}

func (s *Service) Delete(ctx context.Context, accountID string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM pionex_accounts WHERE id = $1", accountID)
	if err != nil {
		return fmt.Errorf("delete pionex account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("pionex account not found")
	}
	return nil
}

// Verify performs a read-only call against the official Pionex Futures
// balances endpoint. Pionex does not provide a non-mutating endpoint that
// proves bot-write permission, so that permission remains operator-declared.
func (s *Service) Verify(ctx context.Context, accountID string) (*Account, error) {
	credentials, err := s.Credentials(ctx, accountID, false)
	if err != nil {
		return nil, err
	}
	_, verifyErr := s.clientFactory(credentials.APIKey, credentials.APISecret).
		GetFuturesBalances(ctx)
	if verifyErr != nil {
		message := verifyErr.Error()
		_, _ = s.db.Exec(ctx, `
			UPDATE pionex_accounts
			SET is_enabled = false, has_read_permission = false,
			    capability_status = 'VERIFICATION_FAILED', last_error = $2,
			    updated_at = NOW()
			WHERE id = $1
		`, accountID, message)
		_, _ = s.db.Exec(ctx, `
			INSERT INTO account_permission_health (
				account_id, can_read, can_trade, can_bot_trade, checked_at, error_message
			) VALUES ($1, false, false, false, NOW(), $2)
			ON CONFLICT (account_id) DO UPDATE SET
				can_read = false, can_trade = false, can_bot_trade = false,
				checked_at = NOW(), error_message = EXCLUDED.error_message
		`, accountID, message)
		return nil, fmt.Errorf("verify pionex futures credentials: %w", verifyErr)
	}
	_, err = s.db.Exec(ctx, `
		UPDATE pionex_accounts
		SET is_enabled = true, has_read_permission = true,
		    capability_status = 'FUTURES_READ_VERIFIED',
		    last_verified_at = NOW(), last_error = NULL, updated_at = NOW()
		WHERE id = $1
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("persist pionex verification: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO account_permission_health (
			account_id, can_read, can_trade, can_bot_trade, checked_at, error_message
		)
		SELECT id, true, has_futures_permission, has_bot_permission, NOW(), NULL
		FROM pionex_accounts WHERE id = $1
		ON CONFLICT (account_id) DO UPDATE SET
			can_read = EXCLUDED.can_read,
			can_trade = EXCLUDED.can_trade,
			can_bot_trade = EXCLUDED.can_bot_trade,
			checked_at = NOW(), error_message = NULL
	`, accountID)
	if err != nil {
		return nil, fmt.Errorf("persist pionex permission health: %w", err)
	}
	return s.Get(ctx, accountID)
}

func (s *Service) Credentials(
	ctx context.Context,
	accountID string,
	requireEnabled bool,
) (*Credentials, error) {
	var encryptedKey, encryptedSecret string
	var enabled bool
	err := s.db.QueryRow(ctx, `
		SELECT api_key_encrypted, api_secret_encrypted, is_enabled
		FROM pionex_accounts WHERE id = $1
	`, accountID).Scan(&encryptedKey, &encryptedSecret, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("pionex account not found")
	}
	if err != nil {
		return nil, fmt.Errorf("load pionex credentials: %w", err)
	}
	if requireEnabled && !enabled {
		return nil, errors.New("pionex account is disabled")
	}
	keyMaterial, err := s.keyMaterial(ctx)
	if err != nil {
		return nil, err
	}
	apiKey, err := decrypt(keyMaterial, "api-key", encryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt pionex api key: %w", err)
	}
	apiSecret, err := decrypt(keyMaterial, "api-secret", encryptedSecret)
	if err != nil {
		return nil, fmt.Errorf("decrypt pionex api secret: %w", err)
	}
	return &Credentials{AccountID: accountID, APIKey: apiKey, APISecret: apiSecret}, nil
}

func (s *Service) keyMaterial(ctx context.Context) ([]byte, error) {
	var key []byte
	err := s.db.QueryRow(ctx, "SELECT key_material FROM credential_keyring WHERE id = 1").Scan(&key)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load credential encryption key: %w", err)
	}
	generated := make([]byte, 32)
	if _, err := io.ReadFull(s.random, generated); err != nil {
		return nil, fmt.Errorf("generate credential encryption key: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO credential_keyring (id, key_material)
		VALUES (1, $1) ON CONFLICT (id) DO NOTHING
	`, generated)
	if err != nil {
		return nil, fmt.Errorf("store credential encryption key: %w", err)
	}
	if err := s.db.QueryRow(ctx, "SELECT key_material FROM credential_keyring WHERE id = 1").Scan(&key); err != nil {
		return nil, fmt.Errorf("reload credential encryption key: %w", err)
	}
	return key, nil
}

func validateCreate(input CreateInput) error {
	name := strings.TrimSpace(input.Name)
	if len(name) < 3 || len(name) > 64 {
		return errors.New("account name must contain 3-64 characters")
	}
	return validateCredentials(input.APIKey, input.APISecret)
}

func validateCredentials(apiKey, apiSecret string) error {
	if len(strings.TrimSpace(apiKey)) < 8 || len(apiKey) > 512 {
		return errors.New("invalid Pionex API key length")
	}
	if len(strings.TrimSpace(apiSecret)) < 8 || len(apiSecret) > 512 {
		return errors.New("invalid Pionex API secret length")
	}
	return nil
}

func keyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func encrypt(key []byte, purpose, plaintext string, random io.Reader) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initialize AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize AES-GCM: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, []byte(plaintext), []byte(purpose))
	payload := append(nonce, sealed...)
	return "v1:" + base64.RawStdEncoding.EncodeToString(payload), nil
}

func decrypt(key []byte, purpose, encoded string) (string, error) {
	if !strings.HasPrefix(encoded, "v1:") {
		return "", errors.New("unsupported credential ciphertext version")
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, "v1:"))
	if err != nil {
		return "", errors.New("invalid credential ciphertext encoding")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initialize AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("initialize AES-GCM: %w", err)
	}
	if len(payload) < aead.NonceSize() {
		return "", errors.New("credential ciphertext is truncated")
	}
	plaintext, err := aead.Open(
		nil, payload[:aead.NonceSize()], payload[aead.NonceSize():], []byte(purpose),
	)
	if err != nil {
		return "", errors.New("credential ciphertext authentication failed")
	}
	return string(plaintext), nil
}

func accountScanTargets(item *Account) []any {
	return []any{
		&item.ID, &item.Name, &item.KeyFingerprint, &item.IsEnabled, &item.IsPaper,
		&item.HasReadPermission, &item.HasFuturesPermission, &item.HasBotPermission,
		&item.CapabilityStatus, &item.LastVerifiedAt, &item.LastError,
		&item.CreatedAt, &item.UpdatedAt,
	}
}
