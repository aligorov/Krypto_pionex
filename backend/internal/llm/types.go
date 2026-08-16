package llm

import (
	"time"

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
	RequireAuditForReal     bool      `json:"requireAuditForReal"`
	GroundingEnabled        bool      `json:"groundingEnabled"`
	AuditIntervalSeconds    int       `json:"auditIntervalSeconds"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

// AuditRecord represents a recorded evaluation of a symbol/candidate by the LLM.
type AuditRecord struct {
	ID                string          `json:"id"`
	CandidateID       *string         `json:"candidateId,omitempty"`
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
	LowerPrice          decimal.Decimal `json:"lower_price"`
	UpperPrice          decimal.Decimal `json:"upper_price"`
	GridCount           int             `json:"grid_count"`
	Leverage            int             `json:"leverage"`
	StopLoss            decimal.Decimal `json:"stop_loss"`
	TakeProfitTargetUSD decimal.Decimal `json:"take_profit_target_usd"`
}

// NewsCatalyst is the structured news verdict the model must return for
// every candidate. A HIGH/CRITICAL catalyst vetoes the candidate regardless
// of the model's overall decision — news can only block an entry, never
// create one (latency makes LLM unsuited as a directional signal).
type NewsCatalyst struct {
	Detected bool   `json:"detected"`
	Type     string `json:"type"`     // UNLOCK, DELIST, EXPLOIT, FUNDING_SKEW, REGULATORY, PUMP_DUMP, NONE
	Severity string `json:"severity"` // LOW, MEDIUM, HIGH, CRITICAL
	Summary  string `json:"summary"`
	ETAHours int    `json:"eta_hours"`
}

// BlocksEntry reports whether the catalyst severity hard-vetoes a deploy.
func (n *NewsCatalyst) BlocksEntry() bool {
	return n != nil && n.Detected && (n.Severity == "HIGH" || n.Severity == "CRITICAL")
}

// AuditDecision carries the parsed structured verdict from LLM.
type AuditDecision struct {
	Decision         string                 `json:"decision"` // APPROVED or REJECTED
	Confidence       float64                `json:"confidence"`
	Regime           string                 `json:"regime"`
	ReasoningSummary string                 `json:"reasoning_summary"`
	RejectionReason  *string                `json:"rejection_reason"`
	NewsCatalyst     *NewsCatalyst          `json:"news_catalyst,omitempty"`
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
	Symbol              string  `json:"symbol"`
	CurrentPrice        float64 `json:"current_price"`
	Volume24h           float64 `json:"volume_24h_usd"`
	VolatilityParkinson float64 `json:"volatility_parkinson_pct"`
	ATRPct              float64 `json:"atr_pct"`
	ADX                 float64 `json:"adx"`
	Choppiness          float64 `json:"choppiness_index"`
	EMASlopePct         float64 `json:"ema_slope_pct"`
	IsSqueeze           bool    `json:"is_bbw_squeeze"`
	Hurst               float64 `json:"hurst_exponent"`
	ConfluenceVerdict   string  `json:"confluence_verdict"`
	RecommendedTrend    string  `json:"recommended_trend"`
	// ScannerFloor tells the LLM what quantitative gates the operator has
	// already applied, so the audit doesn't re-reject what passed them.
	ScannerFloor       ScannerFloor    `json:"scanner_floor"`
	ProposedLowerPrice float64         `json:"proposed_lower_price"`
	ProposedUpperPrice float64         `json:"proposed_upper_price"`
	ProposedGridCount  int             `json:"proposed_grid_count"`
	ProposedLeverage   int             `json:"proposed_leverage"`
	RecentCandles15m   []CandleSummary `json:"recent_candles_15m"`
}

// ScannerFloor carries the operator's quantitative scanner settings into
// the LLM prompt as CONTEXT — the audit should not re-reject candidates
// that already passed these thresholds, but may still flag qualitative
// concerns (wash trading, spoofing, catalyst risks) independent of them.
type ScannerFloor struct {
	MinVolume24hUSD  float64 `json:"min_volume_24h_usd"`
	MinVolatilityPct float64 `json:"min_volatility_pct"`
	MaxVolatilityPct float64 `json:"max_volatility_pct"`
	MinSharpe        float64 `json:"min_sharpe"`
}
