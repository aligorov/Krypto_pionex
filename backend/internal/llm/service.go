package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Service provides high-level LLM operations and persistence.
type Service struct {
	db     *pgxpool.Pool
	client *Client
	logger *slog.Logger
}

// NewService instantiates a new LLM Service.
func NewService(db *pgxpool.Pool, logger *slog.Logger) *Service {
	return &Service{
		db:     db,
		client: NewClient(),
		logger: logger,
	}
}

// GetSettings retrieves the persistent LLM configuration.
func (s *Service) GetSettings(ctx context.Context) (*Settings, error) {
	var item Settings
	err := s.db.QueryRow(ctx, `
		SELECT id, enabled, provider, api_key, model, base_url,
		       temperature, thinking_enabled, require_approval_to_deploy,
		       require_audit_for_real,
		       audit_interval_seconds, created_at, updated_at
		FROM llm_settings WHERE id = 1
	`).Scan(
		&item.ID, &item.Enabled, &item.Provider, &item.APIKey, &item.Model,
		&item.BaseURL, &item.Temperature, &item.ThinkingEnabled,
		&item.RequireApprovalToDeploy, &item.RequireAuditForReal,
		&item.AuditIntervalSeconds,
		&item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("load llm settings: %w", err)
	}
	item.APIKeyMasked = maskKey(item.APIKey)
	return &item, nil
}

// UpdateSettings updates the persistent LLM configuration.
func (s *Service) UpdateSettings(ctx context.Context, patch Settings) (*Settings, error) {
	current, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}

	apiKey := strings.TrimSpace(patch.APIKey)
	if apiKey == "" || apiKey == current.APIKeyMasked {
		apiKey = current.APIKey
	}

	provider := strings.TrimSpace(patch.Provider)
	if provider == "" {
		provider = current.Provider
	}
	model := strings.TrimSpace(patch.Model)
	if model == "" {
		model = current.Model
	}
	// Persist gate for the SSRF allowlist (audit SEC-005): a bad base URL
	// must never reach the stored settings in the first place.
	if err := ValidateBaseURL(provider, patch.BaseURL); err != nil {
		return nil, fmt.Errorf("LLM base URL rejected: %w", err)
	}

	_, err = s.db.Exec(ctx, `
		UPDATE llm_settings
		SET enabled = $1,
		    provider = $2,
		    api_key = $3,
		    model = $4,
		    base_url = $5,
		    temperature = $6,
		    thinking_enabled = $7,
		    require_approval_to_deploy = $8,
		    require_audit_for_real = $10,
		    audit_interval_seconds = $9,
		    updated_at = NOW()
		WHERE id = 1
	`, patch.Enabled, provider, apiKey, model,
		strings.TrimSpace(patch.BaseURL), patch.Temperature, patch.ThinkingEnabled,
		patch.RequireApprovalToDeploy, patch.AuditIntervalSeconds, patch.RequireAuditForReal)
	if err != nil {
		return nil, fmt.Errorf("update llm settings: %w", err)
	}

	return s.GetSettings(ctx)
}

// TestConnection executes a small test completion against the configured LLM API.
func (s *Service) TestConnection(ctx context.Context, settings Settings) (string, int, error) {
	if strings.TrimSpace(settings.APIKey) == "" {
		// If key was not passed, load from DB
		current, err := s.GetSettings(ctx)
		if err == nil && current.APIKey != "" {
			settings.APIKey = current.APIKey
		}
	}
	if strings.TrimSpace(settings.APIKey) == "" {
		return "", 0, errors.New("API-ключ не заполнен")
	}

	testPrompt := fmt.Sprintf(`{"action": "ping", "provider": "%s", "model": "%s", "timestamp": %d}. Return ONLY valid JSON: {"status": "ok", "message": "API connection verified", "model": "%s"}`, settings.Provider, settings.Model, time.Now().Unix(), settings.Model)
	rawResp, latencyMs, err := s.client.ExecutePrompt(ctx, settings, "You are a quantitative API tester. Return strictly valid JSON with no markdown.", testPrompt)
	if err != nil {
		return "", latencyMs, err
	}
	return CleanJSONResponse(rawResp), latencyMs, nil
}

// AuditCandidate evaluates a symbol with the configured LLM and saves the audit record to PostgreSQL.
func (s *Service) AuditCandidate(
	ctx context.Context,
	candidateID *string,
	input CandidateInput,
) (*AuditDecision, *AuditRecord, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !settings.Enabled || strings.TrimSpace(settings.APIKey) == "" {
		return nil, nil, errors.New("LLM intelligence is not enabled or API key missing")
	}

	userPrompt, err := BuildCandidatePrompt(input)
	if err != nil {
		return nil, nil, fmt.Errorf("build prompt: %w", err)
	}

	rawResponse, latencyMs, err := s.client.ExecutePrompt(ctx, *settings, SystemPromptEvaluator, userPrompt)
	if err != nil {
		return nil, nil, fmt.Errorf("llm evaluation call failed: %w", err)
	}

	decision, err := ParseAuditDecision(rawResponse)
	if err != nil {
		s.logger.Warn("LLM parse failure — failing closed", "symbol", input.Symbol, "error", err, "raw", rawResponse)
		// A risk gate must fail CLOSED (audit SEC-009): a provider outage,
		// truncated JSON or injected content must never convert itself into
		// a trading approval.
		decision = &AuditDecision{
			Decision:         "REJECTED",
			Confidence:       0.0,
			Regime:           "UNCERTAIN",
			ReasoningSummary: "Ответ LLM не соответствовал JSON-схеме; кандидат отклонён (fail-closed)",
		}
	}

	var recParams map[string]any
	if decision.GridParams != nil {
		recParams = map[string]any{
			"lower_price":            decision.GridParams.LowerPrice.String(),
			"upper_price":            decision.GridParams.UpperPrice.String(),
			"grid_count":             decision.GridParams.GridCount,
			"leverage":               decision.GridParams.Leverage,
			"stop_loss":              decision.GridParams.StopLoss.String(),
			"take_profit_target_usd": decision.GridParams.TakeProfitTargetUSD.String(),
		}
	}

	record := &AuditRecord{
		ID:                newUUID(),
		CandidateID:       candidateID,
		Symbol:            input.Symbol,
		Provider:          settings.Provider,
		Model:             settings.Model,
		Decision:          decision.Decision,
		Confidence:        decimal.NewFromFloat(decision.Confidence),
		Regime:            decision.Regime,
		Reasoning:         decision.ReasoningSummary,
		RecommendedParams: recParams,
		RawResponse:       rawResponse,
		LatencyMs:         latencyMs,
		CreatedAt:         time.Now().UTC(),
	}

	if err := s.SaveAudit(ctx, record); err != nil {
		s.logger.Warn("Failed to persist LLM audit record", "symbol", input.Symbol, "error", err)
	}

	return decision, record, nil
}

// SaveAudit writes the audit record to PostgreSQL.
func (s *Service) SaveAudit(ctx context.Context, record *AuditRecord) error {
	paramsJSON, _ := json.Marshal(record.RecommendedParams)
	_, err := s.db.Exec(ctx, `
		INSERT INTO llm_audits (
			id, candidate_id, symbol, provider, model, decision,
			confidence, regime, reasoning, recommended_params, raw_response,
			latency_ms, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		)
	`, record.ID, record.CandidateID, record.Symbol, record.Provider,
		record.Model, record.Decision, record.Confidence, record.Regime,
		record.Reasoning, paramsJSON, record.RawResponse, record.LatencyMs,
		record.CreatedAt)
	return err
}

// ListRecentAudits retrieves the latest audits from PostgreSQL.
func (s *Service) ListRecentAudits(ctx context.Context, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, candidate_id, symbol, provider, model, decision,
		       confidence, regime, reasoning, recommended_params, latency_ms, created_at
		FROM llm_audits
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list llm audits: %w", err)
	}
	defer rows.Close()

	items := make([]AuditRecord, 0)
	for rows.Next() {
		var r AuditRecord
		var paramsJSON []byte
		if err := rows.Scan(
			&r.ID, &r.CandidateID, &r.Symbol, &r.Provider, &r.Model,
			&r.Decision, &r.Confidence, &r.Regime, &r.Reasoning,
			&paramsJSON, &r.LatencyMs, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(paramsJSON, &r.RecommendedParams)
		items = append(items, r)
	}
	return items, nil
}

func maskKey(key string) string {
	k := strings.TrimSpace(key)
	if len(k) <= 8 {
		return "••••••••"
	}
	return k[:4] + "••••••••" + k[len(k)-4:]
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ListModels queries the provider for available models using the given or stored API key.
func (s *Service) ListModels(ctx context.Context, settings Settings) ([]string, error) {
	if strings.TrimSpace(settings.APIKey) == "" {
		current, err := s.GetSettings(ctx)
		if err == nil && current.APIKey != "" {
			settings.APIKey = current.APIKey
		}
	}
	if strings.TrimSpace(settings.APIKey) == "" {
		return nil, errors.New("API-ключ не заполнен")
	}
	return s.client.ListAvailableModels(ctx, settings)
}
