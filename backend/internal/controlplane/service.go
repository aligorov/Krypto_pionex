package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	db      *pgxpool.Pool
	risk    *risk.Engine
	audit   *audit.Store
	logs    *observability.Store
	version string
	commit  string
	build   string
	now     func() time.Time
}

type Dashboard struct {
	Version            string    `json:"version"`
	GitCommit          string    `json:"gitCommit"`
	BuildTime          string    `json:"buildTime"`
	DatabaseHealthy    bool      `json:"databaseHealthy"`
	KillSwitchEnabled  bool      `json:"killSwitchEnabled"`
	ActiveAccounts     int       `json:"activeAccounts"`
	RunningGrids       int       `json:"runningGrids"`
	OpenPatternOrders  int       `json:"openPatternOrders"`
	OpenIncidents      int       `json:"openIncidents"`
	PendingCommands    int       `json:"pendingCommands"`
	MCPEnabled         bool      `json:"mcpEnabled"`
	RealGridEnabled    bool      `json:"realGridEnabled"`
	RealPatternEnabled bool      `json:"realPatternEnabled"`
	CheckedAt          time.Time `json:"checkedAt"`
}

type ConfigEntry struct {
	Key         string    `json:"key"`
	Value       any       `json:"value"`
	Description string    `json:"description"`
	IsSensitive bool      `json:"isSensitive"`
	UpdatedBy   *string   `json:"updatedBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type FeatureFlag struct {
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Description string    `json:"description"`
	UpdatedBy   *string   `json:"updatedBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Account struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	IsEnabled            bool      `json:"isEnabled"`
	IsPaper              bool      `json:"isPaper"`
	HasReadPermission    bool      `json:"hasReadPermission"`
	HasFuturesPermission bool      `json:"hasFuturesPermission"`
	HasBotPermission     bool      `json:"hasBotPermission"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type GridBot struct {
	ID                  string    `json:"id"`
	AccountID           string    `json:"accountId"`
	Symbol              string    `json:"symbol"`
	BUOrderID           *string   `json:"buOrderId"`
	Status              string    `json:"status"`
	Direction           string    `json:"direction"`
	GridType            string    `json:"gridType"`
	LowerPrice          string    `json:"lowerPrice"`
	UpperPrice          string    `json:"upperPrice"`
	GridNum             int       `json:"gridNum"`
	Leverage            int       `json:"leverage"`
	QuoteInvestment     string    `json:"quoteInvestment"`
	ReconciliationState string    `json:"reconciliationState,omitempty"`
	LastError           *string   `json:"lastError,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type PatternOrder struct {
	ID            string    `json:"id"`
	AccountID     string    `json:"accountId"`
	Symbol        string    `json:"symbol"`
	ClientOrderID string    `json:"clientOrderId"`
	PionexOrderID *string   `json:"pionexOrderId"`
	PatternType   string    `json:"patternType"`
	Side          string    `json:"side"`
	OrderType     string    `json:"orderType"`
	Price         string    `json:"price"`
	Quantity      string    `json:"quantity"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Command struct {
	ID                    string         `json:"id"`
	ActorID               *string        `json:"actorId"`
	ActorType             string         `json:"actorType"`
	CommandType           string         `json:"commandType"`
	ResourceType          string         `json:"resourceType"`
	ResourceID            string         `json:"resourceId"`
	SanitizedArguments    map[string]any `json:"sanitizedArguments"`
	IdempotencyKey        string         `json:"idempotencyKey"`
	Status                string         `json:"status"`
	ConfirmationExpiresAt *time.Time     `json:"confirmationExpiresAt"`
	RiskResult            map[string]any `json:"riskResult"`
	Result                map[string]any `json:"result"`
	ErrorMessage          *string        `json:"errorMessage"`
	CreatedAt             time.Time      `json:"createdAt"`
	ConfirmedAt           *time.Time     `json:"confirmedAt"`
	ExecutedAt            *time.Time     `json:"executedAt"`
	UpdatedAt             time.Time      `json:"updatedAt"`
}

type PrepareCommandInput struct {
	CommandType    string         `json:"commandType"`
	ResourceType   string         `json:"resourceType"`
	ResourceID     string         `json:"resourceId"`
	Arguments      map[string]any `json:"arguments"`
	IdempotencyKey string         `json:"idempotencyKey"`
}

type PreparedCommand struct {
	Command          Command `json:"command"`
	ConfirmationCode string  `json:"confirmationCode,omitempty"`
}

func NewService(
	db *pgxpool.Pool,
	riskEngine *risk.Engine,
	auditStore *audit.Store,
	logStore *observability.Store,
	version, commit, build string,
) *Service {
	return &Service{
		db: db, risk: riskEngine, audit: auditStore, logs: logStore,
		version: version, commit: commit, build: build, now: time.Now,
	}
}

func (s *Service) Dashboard(ctx context.Context) (*Dashboard, error) {
	dashboard := &Dashboard{
		Version: s.version, GitCommit: s.commit, BuildTime: s.build, CheckedAt: s.now(),
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dashboard.DatabaseHealthy = s.db.Ping(pingCtx) == nil
	if !dashboard.DatabaseHealthy {
		return dashboard, nil
	}
	// Auto-expire stale commands so ghost commands never pollute the queue count
	_, _ = s.db.Exec(ctx, `
		UPDATE control_commands
		SET status = 'EXPIRED', updated_at = NOW()
		WHERE (status = 'CONFIRMATION_REQUIRED' AND confirmation_expires_at < NOW())
		   OR (status = 'EXECUTING' AND lease_expiry < NOW() - INTERVAL '30 seconds')
	`)
	err := s.db.QueryRow(ctx, `
		SELECT
			(SELECT kill_switch_enabled FROM risk_settings WHERE id = 1),
			(SELECT COUNT(*) FROM pionex_accounts WHERE is_enabled),
			(SELECT COUNT(*) FROM grid_bots WHERE status IN ('RUNNING', 'ADJUSTING', 'REDUCING', 'STOPPING')),
			(SELECT COUNT(*) FROM pattern_orders WHERE status IN ('CREATED', 'SUBMITTING', 'SUBMITTED', 'PARTIALLY_FILLED', 'CANCEL_REQUESTED')),
			(SELECT COUNT(*) FROM system_incidents WHERE status IN ('OPEN', 'ACKNOWLEDGED')),
			(SELECT COUNT(*) FROM control_commands WHERE status IN ('QUEUED', 'EXECUTING') AND (lease_expiry IS NULL OR lease_expiry >= NOW())),
			COALESCE((SELECT (value #>> '{}')::BOOLEAN FROM app_config WHERE key = 'mcp_write_enabled'), false),
			COALESCE((SELECT (value #>> '{}')::BOOLEAN FROM app_config WHERE key = 'real_grid_execution_enabled'), false),
			COALESCE((SELECT (value #>> '{}')::BOOLEAN FROM app_config WHERE key = 'real_pattern_execution_enabled'), false)
	`).Scan(
		&dashboard.KillSwitchEnabled, &dashboard.ActiveAccounts, &dashboard.RunningGrids,
		&dashboard.OpenPatternOrders, &dashboard.OpenIncidents, &dashboard.PendingCommands,
		&dashboard.MCPEnabled, &dashboard.RealGridEnabled, &dashboard.RealPatternEnabled,
	)
	if err != nil {
		return nil, fmt.Errorf("load dashboard: %w", err)
	}
	return dashboard, nil
}

func (s *Service) ListConfig(ctx context.Context, includeSensitive bool) ([]ConfigEntry, error) {
	rows, err := s.db.Query(ctx, `
		SELECT key,
		       CASE WHEN is_sensitive AND NOT $1 THEN '"[REDACTED]"'::jsonb ELSE value END,
		       description, is_sensitive, updated_by, updated_at
		FROM app_config ORDER BY key
	`, includeSensitive)
	if err != nil {
		return nil, fmt.Errorf("list config: %w", err)
	}
	defer rows.Close()
	items := make([]ConfigEntry, 0)
	for rows.Next() {
		var item ConfigEntry
		if err := rows.Scan(
			&item.Key, &item.Value, &item.Description, &item.IsSensitive,
			&item.UpdatedBy, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan config: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) MCPWritesEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE((value #>> '{}')::BOOLEAN, false)
		FROM app_config WHERE key = 'mcp_write_enabled'
	`).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("load MCP write setting: %w", err)
	}
	return enabled, nil
}

func (s *Service) SetConfig(
	ctx context.Context,
	principal auth.Principal,
	key string,
	value any,
) (*ConfigEntry, error) {
	if !principal.HasRole(auth.RoleAdmin) {
		return nil, auth.ErrForbidden
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE app_config SET value = $2, updated_by = $3, updated_at = NOW()
		WHERE key = $1
	`, key, value, principal.UserID)
	if err != nil {
		return nil, fmt.Errorf("set config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New("unknown configuration key")
	}
	item, err := s.getConfig(ctx, key, principal.HasRole(auth.RoleAdmin))
	if err != nil {
		return nil, err
	}
	auditValue := value
	if item.IsSensitive {
		auditValue = "[REDACTED]"
	}
	_ = s.audit.Record(ctx, audit.EventFromPrincipal(
		principal, "config.update", "app_config", key, "SUCCESS",
		map[string]any{"value": auditValue},
	))
	return item, nil
}

func (s *Service) ListFeatureFlags(ctx context.Context) ([]FeatureFlag, error) {
	rows, err := s.db.Query(ctx, `
		SELECT name, enabled, description, updated_by, updated_at
		FROM feature_flags ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list feature flags: %w", err)
	}
	defer rows.Close()
	items := make([]FeatureFlag, 0)
	for rows.Next() {
		var item FeatureFlag
		if err := rows.Scan(
			&item.Name, &item.Enabled, &item.Description, &item.UpdatedBy, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan feature flag: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) SetFeatureFlag(
	ctx context.Context,
	principal auth.Principal,
	name string,
	enabled bool,
) (*FeatureFlag, error) {
	if !principal.HasRole(auth.RoleAdmin) {
		return nil, auth.ErrForbidden
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE feature_flags
		SET enabled = $2, updated_by = $3, updated_at = NOW()
		WHERE name = $1
	`, name, enabled, principal.UserID)
	if err != nil {
		return nil, fmt.Errorf("set feature flag: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New("unknown feature flag")
	}
	_ = s.audit.Record(ctx, audit.EventFromPrincipal(
		principal, "feature_flag.update", "feature_flag", name, "SUCCESS",
		map[string]any{"enabled": enabled},
	))
	var item FeatureFlag
	err = s.db.QueryRow(ctx, `
		SELECT name, enabled, description, updated_by, updated_at
		FROM feature_flags WHERE name = $1
	`, name).Scan(&item.Name, &item.Enabled, &item.Description, &item.UpdatedBy, &item.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("reload feature flag: %w", err)
	}
	return &item, nil
}

func (s *Service) RiskSettings(ctx context.Context) (*risk.RiskSettings, error) {
	return s.risk.LoadSettings(ctx)
}

func (s *Service) UpdateRiskSettings(
	ctx context.Context,
	principal auth.Principal,
	settings risk.RiskSettings,
) (*risk.RiskSettings, error) {
	if !principal.HasRole(auth.RoleAdmin) {
		return nil, auth.ErrForbidden
	}
	current, err := s.risk.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	updated, err := s.risk.UpdateSettings(ctx, settings)
	if err != nil {
		return nil, err
	}
	_ = s.audit.Record(ctx, audit.EventFromPrincipal(
		principal, "risk.settings.update", "risk_settings", "1", "SUCCESS",
		map[string]any{"before": current, "after": updated},
	))
	return updated, nil
}

func (s *Service) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, is_enabled, is_paper, has_read_permission,
		       has_futures_permission, has_bot_permission, created_at, updated_at
		FROM pionex_accounts ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		var item Account
		if err := rows.Scan(
			&item.ID, &item.Name, &item.IsEnabled, &item.IsPaper,
			&item.HasReadPermission, &item.HasFuturesPermission,
			&item.HasBotPermission, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts = append(accounts, item)
	}
	return accounts, rows.Err()
}

func (s *Service) ListGridBots(ctx context.Context, limit int) ([]GridBot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, account_id, symbol, bu_order_id, status, direction, grid_type,
		       lower_price::TEXT, upper_price::TEXT, grid_num, leverage,
		       quote_investment::TEXT, reconciliation_state, last_error, created_at, updated_at
		FROM grid_bots ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list grid bots: %w", err)
	}
	defer rows.Close()
	items := make([]GridBot, 0)
	for rows.Next() {
		var item GridBot
		if err := rows.Scan(
			&item.ID, &item.AccountID, &item.Symbol, &item.BUOrderID, &item.Status,
			&item.Direction, &item.GridType, &item.LowerPrice, &item.UpperPrice,
			&item.GridNum, &item.Leverage, &item.QuoteInvestment,
			&item.ReconciliationState, &item.LastError,
			&item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan grid bot: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) ListPatternOrders(ctx context.Context, limit int) ([]PatternOrder, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, account_id, symbol, client_order_id, pionex_order_id,
		       pattern_type, side, order_type, price::TEXT, quantity::TEXT,
		       status, created_at, updated_at
		FROM pattern_orders ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pattern orders: %w", err)
	}
	defer rows.Close()
	items := make([]PatternOrder, 0)
	for rows.Next() {
		var item PatternOrder
		if err := rows.Scan(
			&item.ID, &item.AccountID, &item.Symbol, &item.ClientOrderID,
			&item.PionexOrderID, &item.PatternType, &item.Side, &item.OrderType,
			&item.Price, &item.Quantity, &item.Status, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan pattern order: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) PrepareCommand(
	ctx context.Context,
	principal auth.Principal,
	input PrepareCommandInput,
) (*PreparedCommand, error) {
	if !principal.HasRole(requiredRole(input.CommandType, input.Arguments)) {
		return nil, auth.ErrForbidden
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, errors.New("idempotency key is required")
	}
	if input.Arguments == nil {
		input.Arguments = map[string]any{}
	}
	if err := validateCommand(input.CommandType); err != nil {
		return nil, err
	}

	confirmationRequired, err := s.confirmationRequired(ctx, input.CommandType, input.Arguments)
	if err != nil {
		return nil, err
	}
	status := "PREPARED"
	var confirmationCode string
	var confirmationHash *string
	var confirmationExpiresAt *time.Time
	if confirmationRequired {
		status = "CONFIRMATION_REQUIRED"
		code, err := randomConfirmationCode()
		if err != nil {
			return nil, err
		}
		confirmationCode = code
		hash := hashValue(code)
		confirmationHash = &hash
		expires := s.now().Add(5 * time.Minute)
		confirmationExpiresAt = &expires
	}

	sanitized := observability.RedactFields(input.Arguments)
	var command Command
	err = s.db.QueryRow(ctx, `
		INSERT INTO control_commands (
			actor_id, actor_type, command_type, resource_type, resource_id,
			arguments, sanitized_arguments, idempotency_key, status,
			confirmation_hash, confirmation_expires_at
		) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, $10, $11)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, actor_id, actor_type, command_type, resource_type,
		          COALESCE(resource_id, ''), sanitized_arguments, idempotency_key,
		          status, confirmation_expires_at, risk_result, result,
		          error_message, created_at, confirmed_at, executed_at, updated_at
	`, principal.UserID, principal.ActorType, input.CommandType, input.ResourceType,
		input.ResourceID, input.Arguments, sanitized, input.IdempotencyKey, status,
		confirmationHash, confirmationExpiresAt).Scan(commandScanTargets(&command)...)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, loadErr := s.GetCommandByIdempotencyKey(ctx, input.IdempotencyKey)
		if loadErr != nil {
			return nil, loadErr
		}
		return &PreparedCommand{Command: *existing}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("prepare control command: %w", err)
	}

	_ = s.audit.Record(ctx, audit.EventFromPrincipal(
		principal, "control_command.prepare", input.ResourceType, input.ResourceID, "PENDING",
		map[string]any{"commandId": command.ID, "commandType": input.CommandType, "arguments": sanitized},
	))

	if !confirmationRequired {
		executed, executeErr := s.executeCommand(ctx, principal, command.ID)
		if executeErr != nil {
			return nil, executeErr
		}
		command = *executed
	}
	return &PreparedCommand{Command: command, ConfirmationCode: confirmationCode}, nil
}

func (s *Service) ConfirmCommand(
	ctx context.Context,
	principal auth.Principal,
	commandID, confirmationCode string,
) (*Command, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin confirmation transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var storedHash string
	var expiresAt time.Time
	var actorID string
	var commandType string
	var arguments map[string]any
	err = tx.QueryRow(ctx, `
		SELECT actor_id, command_type, arguments, confirmation_hash, confirmation_expires_at
		FROM control_commands
		WHERE id = $1 AND status = 'CONFIRMATION_REQUIRED'
		FOR UPDATE
	`, commandID).Scan(&actorID, &commandType, &arguments, &storedHash, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("command is not awaiting confirmation")
	}
	if err != nil {
		return nil, fmt.Errorf("load command confirmation: %w", err)
	}
	if actorID != principal.UserID && !principal.HasRole(auth.RoleAdmin) {
		return nil, auth.ErrForbidden
	}
	if !principal.HasRole(requiredRole(commandType, arguments)) {
		return nil, auth.ErrForbidden
	}
	if s.now().After(expiresAt) {
		_, _ = tx.Exec(ctx, "UPDATE control_commands SET status = 'EXPIRED', updated_at = NOW() WHERE id = $1", commandID)
		_ = tx.Commit(ctx)
		return nil, errors.New("confirmation has expired")
	}
	if hashValue(confirmationCode) != storedHash {
		return nil, errors.New("invalid confirmation code")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE control_commands
		SET status = 'EXECUTING', confirmed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, commandID); err != nil {
		return nil, fmt.Errorf("mark command confirmed: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit confirmation: %w", err)
	}
	return s.executeCommand(ctx, principal, commandID)
}

func (s *Service) ListCommands(ctx context.Context, limit int) ([]Command, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, actor_id, actor_type, command_type, resource_type,
		       COALESCE(resource_id, ''), sanitized_arguments, idempotency_key,
		       status, confirmation_expires_at, risk_result, result,
		       error_message, created_at, confirmed_at, executed_at, updated_at
		FROM control_commands ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list control commands: %w", err)
	}
	defer rows.Close()
	commands := make([]Command, 0)
	for rows.Next() {
		var command Command
		if err := rows.Scan(commandScanTargets(&command)...); err != nil {
			return nil, fmt.Errorf("scan control command: %w", err)
		}
		commands = append(commands, command)
	}
	return commands, rows.Err()
}

func (s *Service) GetCommandByIdempotencyKey(ctx context.Context, key string) (*Command, error) {
	var command Command
	err := s.db.QueryRow(ctx, `
		SELECT id, actor_id, actor_type, command_type, resource_type,
		       COALESCE(resource_id, ''), sanitized_arguments, idempotency_key,
		       status, confirmation_expires_at, risk_result, result,
		       error_message, created_at, confirmed_at, executed_at, updated_at
		FROM control_commands WHERE idempotency_key = $1
	`, key).Scan(commandScanTargets(&command)...)
	if err != nil {
		return nil, fmt.Errorf("get control command: %w", err)
	}
	return &command, nil
}

func (s *Service) Logs() *observability.Store {
	return s.logs
}

func (s *Service) Audit() *audit.Store {
	return s.audit
}

func (s *Service) executeCommand(
	ctx context.Context,
	principal auth.Principal,
	commandID string,
) (*Command, error) {
	var commandType, resourceType, resourceID string
	var arguments map[string]any
	err := s.db.QueryRow(ctx, `
		SELECT command_type, resource_type, COALESCE(resource_id, ''), arguments
		FROM control_commands WHERE id = $1
	`, commandID).Scan(&commandType, &resourceType, &resourceID, &arguments)
	if err != nil {
		return nil, fmt.Errorf("load command for execution: %w", err)
	}

	result := map[string]any{}
	status := "SUCCEEDED"
	var executeErr error

	switch commandType {
	case "kill_switch.set":
		enabled, ok := boolArgument(arguments, "enabled")
		if !ok {
			executeErr = errors.New("enabled boolean is required")
			break
		}
		executeErr = s.risk.SetKillSwitch(ctx, enabled)
		result["killSwitchEnabled"] = enabled
	case "account.set_enabled":
		enabled, ok := boolArgument(arguments, "enabled")
		if !ok || resourceID == "" {
			executeErr = errors.New("account id and enabled boolean are required")
			break
		}
		var tag pgconn.CommandTag
		tag, executeErr = s.db.Exec(ctx, `
			UPDATE pionex_accounts SET is_enabled = $2, updated_at = NOW() WHERE id = $1
		`, resourceID, enabled)
		if executeErr == nil && tag.RowsAffected() == 0 {
			executeErr = errors.New("account not found")
		}
		result["enabled"] = enabled
	case "grid.stop":
		if resourceID == "" {
			executeErr = errors.New("grid bot id is required")
			break
		}
		var tag pgconn.CommandTag
		tag, executeErr = s.db.Exec(ctx, `
			UPDATE grid_bots
			SET status = 'STOP_REQUESTED', updated_at = NOW()
			WHERE id = $1 AND status NOT IN ('STOPPED', 'CANCELLED', 'COMPLETED', 'LIQUIDATED')
		`, resourceID)
		if executeErr == nil && tag.RowsAffected() == 0 {
			executeErr = errors.New("grid bot not found or already terminal")
		}
		status = "QUEUED"
		result["executor"] = "native_grid_worker"
		result["message"] = "durable stop request queued; remote terminal and flat-position verification are still required"
	case "autogrid.start", "autogrid.scan",
		"autogrid.stop", "autogrid.emergency_stop":
		if entryCommand(commandType, arguments) {
			allowed, reason, checkErr := s.entryAllowed(ctx, commandType)
			if checkErr != nil {
				executeErr = checkErr
				break
			}
			if !allowed {
				status = "DENIED"
				executeErr = errors.New(reason)
				break
			}
		}
		status = "QUEUED"
		result["executor"] = executorFor(commandType)
		result["message"] = "command accepted by control plane and queued for the domain executor"
	default:
		executeErr = fmt.Errorf("unsupported command type %q", commandType)
	}

	if executeErr != nil {
		if status != "DENIED" {
			status = "FAILED"
		}
		message := executeErr.Error()
		_, _ = s.db.Exec(ctx, `
			UPDATE control_commands
			SET status = $2, error_message = $3, result = $4,
			    executed_at = NOW(), updated_at = NOW()
			WHERE id = $1
		`, commandID, status, message, result)
		_ = s.audit.Record(ctx, audit.EventFromPrincipal(
			principal, "control_command.execute", resourceType, resourceID, status,
			map[string]any{"commandId": commandID, "commandType": commandType, "error": message},
		))
		return nil, executeErr
	}

	_, err = s.db.Exec(ctx, `
		UPDATE control_commands
		SET status = $2::VARCHAR, result = $3, error_message = NULL,
		    executed_at = CASE WHEN $2::VARCHAR = 'QUEUED' THEN executed_at ELSE NOW() END,
		    updated_at = NOW()
		WHERE id = $1
	`, commandID, status, result)
	if err != nil {
		return nil, fmt.Errorf("finalize command execution: %w", err)
	}
	_ = s.audit.Record(ctx, audit.EventFromPrincipal(
		principal, "control_command.execute", resourceType, resourceID,
		mapOutcome(status), map[string]any{"commandId": commandID, "commandType": commandType, "result": result},
	))
	return s.GetCommand(ctx, commandID)
}

func (s *Service) GetCommand(ctx context.Context, commandID string) (*Command, error) {
	var command Command
	err := s.db.QueryRow(ctx, `
		SELECT id, actor_id, actor_type, command_type, resource_type,
		       COALESCE(resource_id, ''), sanitized_arguments, idempotency_key,
		       status, confirmation_expires_at, risk_result, result,
		       error_message, created_at, confirmed_at, executed_at, updated_at
		FROM control_commands WHERE id = $1
	`, commandID).Scan(commandScanTargets(&command)...)
	if err != nil {
		return nil, fmt.Errorf("get control command: %w", err)
	}
	return &command, nil
}

func (s *Service) entryAllowed(ctx context.Context, commandType string) (bool, string, error) {
	settings, err := s.risk.LoadSettings(ctx)
	if err != nil {
		return false, "", err
	}
	if settings.KillSwitchEnabled {
		return false, "kill switch is enabled", nil
	}
	configKey := "real_pattern_execution_enabled"
	if commandType == "grid.create" || commandType == "autogrid.start" {
		configKey = "real_grid_execution_enabled"
	}
	featureName := "real_pattern_execution"
	if commandType == "grid.create" || commandType == "autogrid.start" {
		featureName = "real_native_grid"
	}
	var enabled, featureEnabled bool
	err = s.db.QueryRow(ctx, `
		SELECT
			COALESCE((SELECT (value #>> '{}')::BOOLEAN FROM app_config WHERE key = $1), false),
			COALESCE((SELECT enabled FROM feature_flags WHERE name = $2), false)
	`, configKey, featureName).Scan(&enabled, &featureEnabled)
	if err != nil {
		return false, "", fmt.Errorf("load execution flag: %w", err)
	}
	if !enabled {
		return false, configKey + " is false", nil
	}
	if !featureEnabled {
		return false, "feature flag " + featureName + " is disabled", nil
	}
	return true, "", nil
}

func (s *Service) confirmationRequired(
	ctx context.Context,
	commandType string,
	arguments map[string]any,
) (bool, error) {
	if !dangerousCommand(commandType, arguments) {
		return false, nil
	}
	var enabled bool
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE((value #>> '{}')::BOOLEAN, true)
		FROM app_config WHERE key = 'mcp_dangerous_confirmation_required'
	`).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("load command confirmation setting: %w", err)
	}
	return enabled, nil
}

func (s *Service) getConfig(ctx context.Context, key string, includeSensitive bool) (*ConfigEntry, error) {
	var item ConfigEntry
	err := s.db.QueryRow(ctx, `
		SELECT key,
		       CASE WHEN is_sensitive AND NOT $2 THEN '"[REDACTED]"'::jsonb ELSE value END,
		       description, is_sensitive, updated_by, updated_at
		FROM app_config WHERE key = $1
	`, key, includeSensitive).Scan(
		&item.Key, &item.Value, &item.Description, &item.IsSensitive,
		&item.UpdatedBy, &item.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}
	return &item, nil
}

// validateCommand accepts only command types that have a live executor:
// kill_switch.set / account.set_enabled execute inline in ExecuteCommand,
// grid.stop turns into a durable stop request the autogrid worker's
// reconcile loop acts on, and the autogrid.* family is claimed by the
// autogrid worker. Types whose domain executors were never implemented
// (grid.create, grid.reduce, pattern.cancel, position.emergency_close,
// scanner.run) used to queue silently and expire 15 minutes later — they are
// rejected at prepare time now so nobody trusts a dead red button.
func validateCommand(commandType string) error {
	switch commandType {
	case "kill_switch.set", "account.set_enabled",
		"grid.stop",
		"autogrid.start", "autogrid.scan", "autogrid.stop",
		"autogrid.emergency_stop":
		return nil
	default:
		return fmt.Errorf("unsupported command type %q", commandType)
	}
}

func requiredRole(commandType string, arguments map[string]any) string {
	if commandType == "account.set_enabled" {
		return auth.RoleAdmin
	}
	if commandType == "kill_switch.set" {
		enabled, ok := boolArgument(arguments, "enabled")
		if ok && !enabled {
			return auth.RoleAdmin
		}
	}
	return auth.RoleOperator
}

func dangerousCommand(commandType string, arguments map[string]any) bool {
	switch commandType {
	case "scanner.run", "autogrid.scan", "autogrid.stop":
		return false
	case "autogrid.start":
		real, ok := boolArgument(arguments, "real")
		return ok && real
	case "kill_switch.set":
		enabled, ok := boolArgument(arguments, "enabled")
		return !ok || !enabled
	default:
		return true
	}
}

func boolArgument(arguments map[string]any, key string) (bool, bool) {
	value, ok := arguments[key]
	if !ok {
		return false, false
	}
	result, ok := value.(bool)
	return result, ok
}

func entryCommand(commandType string, arguments map[string]any) bool {
	if commandType == "autogrid.start" {
		real, ok := boolArgument(arguments, "real")
		return ok && real
	}
	return false
}

func executorFor(commandType string) string {
	switch commandType {
	case "autogrid.start", "autogrid.scan", "autogrid.stop", "autogrid.emergency_stop":
		return "autogrid_worker"
	default:
		return "control_worker"
	}
}

func mapOutcome(status string) string {
	switch status {
	case "DENIED":
		return "DENIED"
	case "FAILED":
		return "FAILED"
	case "QUEUED":
		return "PENDING"
	default:
		return "SUCCESS"
	}
}

func commandScanTargets(command *Command) []any {
	return []any{
		&command.ID, &command.ActorID, &command.ActorType, &command.CommandType,
		&command.ResourceType, &command.ResourceID, &command.SanitizedArguments,
		&command.IdempotencyKey, &command.Status, &command.ConfirmationExpiresAt,
		&command.RiskResult, &command.Result, &command.ErrorMessage,
		&command.CreatedAt, &command.ConfirmedAt, &command.ExecutedAt, &command.UpdatedAt,
	}
}

func randomConfirmationCode() (string, error) {
	number, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate confirmation code: %w", err)
	}
	return fmt.Sprintf("%06d", number.Int64()), nil
}

func hashValue(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
