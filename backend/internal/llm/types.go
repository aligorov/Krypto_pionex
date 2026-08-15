package llm

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Provider constants
const (
	ProviderGemini     = "gemini"
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
	ProviderCustom     = "custom"
)

// Settings represents the persistent LLM configuration stored in PostgreSQL.
type Settings struct {
	ID                      int       `json:"id"`
	Enabled                 bool      `json:"enabled"`
	Provider                string    `json:"provider"`
	APIKey                  string    `json:"apiKey,omitempty"`
	APIKeyMasked            string    `json:"apiKeyMasked"`
	Model                   string    `json:"model"`
	BaseURL                 string    `json:"baseUrl"`
	Temperature             float64   `json:"temperature"`
	ThinkingEnabled         bool      `json:"thinkingEnabled"`
	RequireApprovalToDeploy bool      `json:"requireApprovalToDeploy"`
	AuditIntervalSeconds    int       `json:"auditIntervalSeconds"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

// AuditRecord represents a recorded evaluation of a symbol/candidate by the LLM.
type AuditRecord struct {
	ID                uuid.UUID       `json:"id"`
	CandidateID       *uuid.UUID      `json:"candidateId,omitempty"`
	Symbol            string          `json:"symbol"`
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	Decision          string          `json:"decision"` // APPROVED or REJECTED
	Confidence        decimal.Decimal `json:"confidence"`
	Regime            string          `json:"regime"`
	Reasoning         string          `json:"reasoning"`
	RecommendedParams map[string]any  `json:"recommendedParams"`
	RawResponse       string          `json:"rawResponse,omitempty"`
	LatencyMs         int             `json:"latencyMs"`
	CreatedAt         time.Time       `json:"createdAt"`
}

// RecommendedGridParams carries quantitative grid parameters parsed from the LLM.
type RecommendedGridParams struct {
	LowerPrice         decimal.Decimal `json:"lower_price"`
	UpperPrice         decimal.Decimal `json:"upper_price"`
	GridCount          int             `json:"grid_count"`
	Leverage           int             `json:"leverage"`
	StopLoss           decimal.Decimal `json:"stop_loss"`
	TakeProfitTargetUSD decimal.Decimal `json:"take_profit_target_usd"`
}

// AuditDecision carries the parsed structured verdict from LLM.
type AuditDecision struct {
	Decision         string                 `json:"decision"` // APPROVED or REJECTED
	Confidence       float64                `json:"confidence"`
	Regime           string                 `json:"regime"`
	ReasoningSummary string                 `json:"reasoning_summary"`
	RejectionReason  *string                `json:"rejection_reason"`
	GridParams       *RecommendedGridParams `json:"grid_params,omitempty"`
}

// CandleSummary provides minimal compact OHLCV representation for the prompt.
type CandleSummary struct {
	Time   string  `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
}

// CandidateInput is passed to the LLM evaluator to audit a symbol.
type CandidateInput struct {
	Symbol               string          `json:"symbol"`
	CurrentPrice         float64         `json:"current_price"`
	Volume24h            float64         `json:"volume_24h_usd"`
	VolatilityParkinson  float64         `json:"volatility_parkinson_pct"`
	ATRPct               float64         `json:"atr_pct"`
	ADX                  float64         `json:"adx"`
	Choppiness           float64         `json:"choppiness_index"`
	EMASlopePct          float64         `json:"ema_slope_pct"`
	IsSqueeze            bool            `json:"is_bbw_squeeze"`
	RecommendedTrend     string          `json:"recommended_trend"`
	ProposedLowerPrice   float64         `json:"proposed_lower_price"`
	ProposedUpperPrice   float64         `json:"proposed_upper_price"`
	ProposedGridCount    int             `json:"proposed_grid_count"`
	ProposedLeverage     int             `json:"proposed_leverage"`
	RecentCandles15m     []CandleSummary `json:"recent_candles_15m"`
}
