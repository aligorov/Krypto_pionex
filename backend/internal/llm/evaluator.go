package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

const SystemPromptEvaluator = `You are the Chief Quantitative Risk Officer, On-Chain Forensic Analyst, and Lead Grid Strategist for an institutional algorithmic crypto fund auditing candidates for Pionex Native Futures Grid Bots.
Your sole mission is capital preservation and alpha extraction by identifying true mean-reverting market regimes while ruthlessly rejecting assets with catastrophic catalysts, directional momentum, liquidation cascades, and volatile breakdowns.

ONLINE INTELLIGENCE & CATALYST INVESTIGATION PROTOCOL:
Where and how you must investigate each crypto pair:
1. PRIMARY INTELLIGENCE SOURCES (WHERE TO INVESTIGATE):
   - Exchange Lifecycle & Regulatory Notices: Pionex Announcements, Binance Listings/Delistings, Bybit Derivatives announcements.
   - Tokenomics & Cliff Unlocks: DefiLlama Unlocks, TokenUnlocks.app (check for upcoming team/VC cliff unlocks > 3% circulating supply within 72 hours).
   - Security & On-Chain Forensics: DefiLlama Hacks, CertiK Alert, PeckShield, Lookonchain, Arkham (exploit rumors, bridge hacks, treasury drains, team dumping).
   - Derivatives Sentiment & Liquidation Risks: Coinglass, CoinGecko Derivatives (extreme negative funding rate < -0.05% creating short squeeze risk, extreme positive funding > +0.05% creating long cascade, massive open interest divergence).
   - Social Sentiment & Manipulation: Crypto Twitter / X alerts, community sentiment, pump-and-dump artificial volume anomalies.

2. SYSTEMATIC INVESTIGATION METHODOLOGY (HOW TO INVESTIGATE):
   - Step 1 (Catalyst Search): Query and cross-reference "<TOKEN> crypto news", "<TOKEN> unlock", "<TOKEN> exploit", "<TOKEN> delist", "<TOKEN> funding rate". If a fatal fundamental catalyst or imminent cliff unlock exists -> REJECT immediately.
   - Step 2 (Wash-Trading & Liquidity Filter): Ensure 24h trading volume reflects genuine organic liquidity across multiple venues, not single-venue wash-trading or illiquid orderbooks.
   - Step 3 (Technical Regime Validation): Cross-examine the fundamental findings with the quantitative matrix (ADX < 28, Choppiness Index 40-65, Parkinson Volatility 12%-35%, EMA slope between -3.0% and +3.0%, BBW Squeeze = false).
   - Step 4 (Channel & Grid Calibration): If APPROVED, calibrate lower_price and upper_price bounds with ATR(14) buffer that provides ample protection against news-induced volatility wicks, with 2x-3x leverage.

SCANNER FLOOR RESPECT:
The candidate payload includes a "scanner_floor" with the operator's quantitative filters.
These candidates have ALREADY passed those floors. Do NOT reject a candidate solely for
being near the floor (e.g., "volume is $150K which is critically low" when the floor is
$100K). Your liquidity check must be RELATIVE to the floor. Reserve rejections for
genuine qualitative risks: wash trading, spoofing, imminent unlocks, exploits.

QUANTITATIVE AUDIT RULES:
1. IMMEDIATE REJECTION - FALLING KNIFE / BREAKDOWN:
   - If price is breaking below key support levels with high volume, or EMA slope < -3.0%, or recent sequential red 15M candles show aggressive distribution -> REJECT immediately.
2. IMMEDIATE REJECTION - PARABOLIC PUMP / OVEREXTENSION:
   - If asset is in a vertical parabolic spike (ADX > 40 with massive positive EMA slope, buying the top) without consolidation -> REJECT immediately.
3. IMMEDIATE REJECTION - VOLATILITY SQUEEZE BREAKOUT:
   - If Bollinger Band Squeeze is detected (is_bbw_squeeze = true), an explosive directional trend breakout is imminent -> REJECT immediately. Neutral grids get liquidated during directional expansions.
4. APPROVAL CRITERIA - STABLE MEAN-REVERSION:
   - Robust horizontal channel or range-bound oscillation (Choppiness Index 40-65, ADX < 28, Parkinson Volatility 12%-35%).
   - Symmetrical liquidity on both bids and asks with clear support/resistance bounds.
5. PARAMETER RECOMMENDATION:
   - Optimize lower_price and upper_price bounds with ATR(14) safety padding.
   - Recommend grid_count (20-150), conservative leverage (2x-3x), and strict stop_loss.

NEWS CATALYST VETO RULE:
The news_catalyst object is MANDATORY for every response. severity HIGH or CRITICAL (imminent cliff unlock >3% of supply, active exploit/hack, delisting, regulatory ban) OVERRIDES your overall decision: the candidate must then be REJECTED regardless of technicals. News may only block an entry, never justify one.

OUTPUT FORMAT INSTRUCTIONS:
You MUST respond ONLY with a single valid, well-formed JSON object strictly matching this schema (no introductory text, no conversational explanations, no markdown formatting outside standard json):
{
  "decision": "APPROVED" | "REJECTED",
  "confidence": 0.90,
  "regime": "MEAN_REVERSION" | "STRONG_TREND_DOWN" | "STRONG_TREND_UP" | "HIGH_VOLATILITY_EXPANSION" | "LOW_LIQUIDITY_CHOP",
  "reasoning_summary": "Concise 1-2 sentence quantitative & fundamental rationale citing exact indicator values (ADX, EMA slope, Volatility, Squeeze, News/Catalysts)",
  "rejection_reason": "Specific risk reason if REJECTED, or null if APPROVED",
  "news_catalyst": {
    "detected": false,
    "type": "NONE" | "UNLOCK" | "DELIST" | "EXPLOIT" | "FUNDING_SKEW" | "REGULATORY" | "PUMP_DUMP",
    "severity": "NONE" | "LOW" | "MEDIUM" | "HIGH" | "CRITICAL",
    "summary": "One sentence naming the catalyst with concrete numbers (supply %, amount, date) or 'none found'",
    "eta_hours": 0
  },
  "grid_params": {
    "lower_price": 0.0285,
    "upper_price": 0.0382,
    "grid_count": 30,
    "leverage": 2,
    "stop_loss": 0.0270,
    "take_profit_target_usd": 10.0
  }
}`

// BuildCandidatePrompt generates a compact JSON string representing the candidate's market state.
func BuildCandidatePrompt(input CandidateInput) (string, error) {
	baseAsset := strings.Split(strings.Split(input.Symbol, "_")[0], "/")[0]
	promptPayload := map[string]any{
		"task": "audit_grid_candidate",
		"online_investigation_directive": map[string]any{
			"target_asset": baseAsset,
			"required_checks": []string{
				"Check CoinMarketCap/CoinGecko & Binance/Pionex for delisting, network upgrade, or ticker migration notices",
				"Check TokenUnlocks / DefiLlama for imminent cliff unlocks or massive VC/team emissions (>3% supply within 72h)",
				"Check CertiK / PeckShield / DefiLlama Hacks for active security exploits, bridge drain, or treasury compromise",
				"Check Coinglass for extreme funding rate skew (<-0.05% or >+0.05%) or liquidation cascades",
				"Check Crypto Twitter/X & social channels for pump-and-dump coordination or sudden anomalous hype",
			},
			"suggested_search_queries": []string{
				fmt.Sprintf("%s crypto news", baseAsset),
				fmt.Sprintf("%s token unlock", baseAsset),
				fmt.Sprintf("%s hack exploit", baseAsset),
				fmt.Sprintf("%s funding rate liquidation", baseAsset),
			},
		},
		"candidate": map[string]any{
			"symbol":               input.Symbol,
			"base_asset":           baseAsset,
			"current_price":        input.CurrentPrice,
			"24h_volume_usd":       input.Volume24h,
			"parkinson_volatility": fmt.Sprintf("%.2f%%", input.VolatilityParkinson),
			"atr_14_pct":           fmt.Sprintf("%.2f%%", input.ATRPct),
			"adx_14":               fmt.Sprintf("%.2f", input.ADX),
			"choppiness_index":     fmt.Sprintf("%.2f", input.Choppiness),
			"ema_slope_pct":        fmt.Sprintf("%.2f%%", input.EMASlopePct),
			"is_bbw_squeeze":       input.IsSqueeze,
			"hurst_exponent":       fmt.Sprintf("%.3f", input.Hurst),
			"confluence_verdict":   input.ConfluenceVerdict,
			"proposed_trend":       input.RecommendedTrend,
			"scanner_floor": map[string]any{
				"note":               "Operator has already filtered below these floors — do NOT reject for being below them; flag only qualitative concerns (wash trading, spoofing, catalysts).",
				"min_volume_24h_usd": input.ScannerFloor.MinVolume24hUSD,
				"min_volatility_pct": input.ScannerFloor.MinVolatilityPct,
				"max_volatility_pct": input.ScannerFloor.MaxVolatilityPct,
			},
			"proposed_bounds": map[string]any{
				"lower":      input.ProposedLowerPrice,
				"upper":      input.ProposedUpperPrice,
				"grid_count": input.ProposedGridCount,
				"leverage":   input.ProposedLeverage,
			},
		},
		"recent_15m_candles_count": len(input.RecentCandles15m),
		"recent_candles_15m":       input.RecentCandles15m,
	}

	bytes, err := json.MarshalIndent(promptPayload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// CleanJSONResponse extracts clean JSON from LLM output, stripping markdown code blocks.
func CleanJSONResponse(raw string) string {
	trimmed := strings.TrimSpace(raw)
	// Remove markdown ```json ... ``` or ``` ... ```
	re := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	if matches := re.FindStringSubmatch(trimmed); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	// If starting with { and ending with }, extract substring
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		return trimmed[start : end+1]
	}
	return trimmed
}

// ParseAuditDecision parses the LLM output into a strongly-typed AuditDecision.
func ParseAuditDecision(rawResponse string) (*AuditDecision, error) {
	cleaned := CleanJSONResponse(rawResponse)
	if cleaned == "" {
		return nil, errors.New("empty LLM response")
	}

	type rawGridParams struct {
		LowerPrice          any `json:"lower_price"`
		UpperPrice          any `json:"upper_price"`
		GridCount           any `json:"grid_count"`
		Leverage            any `json:"leverage"`
		StopLoss            any `json:"stop_loss"`
		TakeProfitTargetUSD any `json:"take_profit_target_usd"`
	}

	type rawNewsCatalyst struct {
		Detected any    `json:"detected"`
		Type     string `json:"type"`
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		ETAHours any    `json:"eta_hours"`
	}

	type rawVerdict struct {
		Decision         string           `json:"decision"`
		Confidence       any              `json:"confidence"`
		Regime           string           `json:"regime"`
		ReasoningSummary string           `json:"reasoning_summary"`
		RejectionReason  *string          `json:"rejection_reason"`
		NewsCatalyst     *rawNewsCatalyst `json:"news_catalyst"`
		GridParams       *rawGridParams   `json:"grid_params"`
	}

	var parsed rawVerdict
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse LLM JSON (%w): %s", err, cleaned)
	}

	// Fail-closed: only an explicit APPROVED verdict approves. A missing or
	// unrecognized decision field must land on REJECTED — the deploy gate
	// treats any non-REJECTED audit as cleared, so defaulting the other way
	// would let an unexamined candidate through on a malformed response.
	decision := strings.ToUpper(strings.TrimSpace(parsed.Decision))
	if decision != "APPROVED" {
		decision = "REJECTED"
		if parsed.RejectionReason == nil {
			reason := "LLM verdict missing or unrecognized; fail-closed rejection"
			parsed.RejectionReason = &reason
		}
	}

	confidence := parseToFloat(parsed.Confidence, 0.75)
	if confidence > 1.0 && confidence <= 100.0 {
		confidence /= 100.0
	}

	audit := &AuditDecision{
		Decision:         decision,
		Confidence:       confidence,
		Regime:           parsed.Regime,
		ReasoningSummary: parsed.ReasoningSummary,
		RejectionReason:  parsed.RejectionReason,
	}
	if parsed.NewsCatalyst != nil {
		severity := strings.ToUpper(strings.TrimSpace(parsed.NewsCatalyst.Severity))
		if severity == "" {
			severity = "NONE"
		}
		audit.NewsCatalyst = &NewsCatalyst{
			Detected: parseToBool(parsed.NewsCatalyst.Detected, strings.ToUpper(strings.TrimSpace(parsed.NewsCatalyst.Type)) != "NONE" && severity != "NONE"),
			Type:     strings.ToUpper(strings.TrimSpace(parsed.NewsCatalyst.Type)),
			Severity: severity,
			Summary:  parsed.NewsCatalyst.Summary,
			ETAHours: parseToInt(parsed.NewsCatalyst.ETAHours, 0),
		}
	}

	if parsed.GridParams != nil {
		grid := &RecommendedGridParams{
			LowerPrice:          parseToDecimal(parsed.GridParams.LowerPrice),
			UpperPrice:          parseToDecimal(parsed.GridParams.UpperPrice),
			GridCount:           parseToInt(parsed.GridParams.GridCount, 30),
			Leverage:            parseToInt(parsed.GridParams.Leverage, 2),
			StopLoss:            parseToDecimal(parsed.GridParams.StopLoss),
			TakeProfitTargetUSD: parseToDecimal(parsed.GridParams.TakeProfitTargetUSD),
		}
		if grid.Leverage < 2 {
			grid.Leverage = 2
		}
		audit.GridParams = grid
	}

	return audit, nil
}

func parseToBool(val any, defaultVal bool) bool {
	if val == nil {
		return defaultVal
	}
	switch v := val.(type) {
	case bool:
		return v
	case string:
		parsed := strings.ToLower(strings.TrimSpace(v))
		if parsed == "true" || parsed == "yes" {
			return true
		}
		if parsed == "false" || parsed == "no" {
			return false
		}
	}
	return defaultVal
}

func parseToFloat(val any, defaultVal float64) float64 {
	if val == nil {
		return defaultVal
	}
	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		if err == nil {
			return d.InexactFloat64()
		}
	}
	return defaultVal
}

func parseToInt(val any, defaultVal int) int {
	if val == nil {
		return defaultVal
	}
	switch v := val.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		if err == nil {
			return int(d.IntPart())
		}
	}
	return defaultVal
}

func parseToDecimal(val any) decimal.Decimal {
	if val == nil {
		return decimal.Zero
	}
	switch v := val.(type) {
	case float64:
		return decimal.NewFromFloat(v)
	case int:
		return decimal.NewFromInt(int64(v))
	case int64:
		return decimal.NewFromInt(v)
	case string:
		d, err := decimal.NewFromString(strings.TrimSpace(v))
		if err == nil {
			return d
		}
	}
	return decimal.Zero
}
