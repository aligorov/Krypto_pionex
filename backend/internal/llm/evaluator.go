package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

const SystemPromptEvaluator = `Вы — ведущий квант-стратег институционального криптофонда, проводящий аудит кандидатов для автоматических сеточных ботов Pionex Futures Grid.
Ваша цель — защитить капитал от направленных импульсов, ликвидаций и проливов, отбирая только высококачественные инструменты с выраженным возвратом к среднему (mean-reversion).

ПРАВИЛА ОЦЕНКИ РИСКОВ:
1. НЕМЕДЛЕННЫЙ REJECT (Падающие ножи): Если цена пробивает локальные поддержки на повышенном объеме, а наклон EMA < -3.0%, кандидат немедленно отклоняется.
2. НЕМЕДЛЕННЫЙ REJECT (Вертикальный памп): Если актив находится в стадии параболического роста без консолидации (ADX > 40, аномальный объем), сеточные боты запрещены из-за риска резкого схлопывания.
3. НЕМЕДЛЕННЫЙ REJECT (Сжатие волатильности / Squeeze): Если зафиксирован сильный сквиз полос Боллинджера (is_bbw_squeeze = true), ожидается мощный импульсный пробой диапазона — нейтральные сетки запрещены.
4. ОДОБРЕНИЕ (APPROVED - Mean Reversion): Устойчивый флэт или умеренный канал (Choppiness Index 40-65, ADX < 28, волатильность Паркинсона 12-35%), с четкими границами поддержки и сопротивления.
5. РЕКОМЕНДАЦИЯ ПАРАМЕТРОВ: Корректируйте нижнюю и верхнюю границы диапазона (lower_price, upper_price) с учетом буфера ATR, число сеток (20-150), консервативное плечо (2x-5x) и жесткий Stop Loss.

ТРЕБОВАНИЯ К ФОРМАТУ ОТВЕТА:
Верните ИСКЛЮЧИТЕЛЬНО валидный JSON строго по следующей схеме (без лишнего текста вокруг):
{
  "decision": "APPROVED" | "REJECTED",
  "confidence": 0.88,
  "regime": "MEAN_REVERSION" | "STRONG_TREND_DOWN" | "STRONG_TREND_UP" | "HIGH_VOLATILITY_EXPANSION",
  "reasoning_summary": "Четкое обоснование на русском языке в 1-2 предложениях с конкретными цифрами (ADX, наклон EMA, волатильность)",
  "rejection_reason": "Конкретная причина риска на русском языке в случае REJECTED, либо null если APPROVED",
  "grid_params": {
    "lower_price": 0.0285,
    "upper_price": 0.0382,
    "grid_count": 28,
    "leverage": 3,
    "stop_loss": 0.0271,
    "take_profit_target_usd": 7.5
  }
}`

// BuildCandidatePrompt generates a compact JSON string representing the candidate's market state.
func BuildCandidatePrompt(input CandidateInput) (string, error) {
	promptPayload := map[string]any{
		"task": "audit_grid_candidate",
		"candidate": map[string]any{
			"symbol":                 input.Symbol,
			"current_price":          input.CurrentPrice,
			"24h_volume_usd":         input.Volume24h,
			"parkinson_volatility":   fmt.Sprintf("%.2f%%", input.VolatilityParkinson),
			"atr_14_pct":             fmt.Sprintf("%.2f%%", input.ATRPct),
			"adx_14":                 fmt.Sprintf("%.2f", input.ADX),
			"choppiness_index":       fmt.Sprintf("%.2f", input.Choppiness),
			"ema_slope_pct":          fmt.Sprintf("%.2f%%", input.EMASlopePct),
			"is_bbw_squeeze":         input.IsSqueeze,
			"proposed_trend":         input.RecommendedTrend,
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

	type rawVerdict struct {
		Decision         string         `json:"decision"`
		Confidence       any            `json:"confidence"`
		Regime           string         `json:"regime"`
		ReasoningSummary string         `json:"reasoning_summary"`
		RejectionReason  *string        `json:"rejection_reason"`
		GridParams       *rawGridParams `json:"grid_params"`
	}

	var parsed rawVerdict
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil, fmt.Errorf("failed to parse LLM JSON (%w): %s", err, cleaned)
	}

	decision := strings.ToUpper(strings.TrimSpace(parsed.Decision))
	if decision != "APPROVED" && decision != "REJECTED" {
		if parsed.RejectionReason != nil && *parsed.RejectionReason != "" {
			decision = "REJECTED"
		} else {
			decision = "APPROVED"
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
