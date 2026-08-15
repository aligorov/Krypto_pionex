package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client executes completions across multiple LLM providers.
type Client struct {
	httpClient *http.Client
}

// NewClient initializes a new LLM HTTP client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

// ExecutePrompt sends the system instructions and user prompt to the configured LLM provider.
func (c *Client) ExecutePrompt(
	ctx context.Context,
	settings Settings,
	systemPrompt, userPrompt string,
) (string, int, error) {
	if strings.TrimSpace(settings.APIKey) == "" {
		return "", 0, errors.New("LLM API key is not configured")
	}

	start := time.Now()
	var rawResponse string
	var err error

	switch settings.Provider {
	case ProviderGemini:
		rawResponse, err = c.callGemini(ctx, settings, systemPrompt, userPrompt)
	case ProviderAnthropic:
		rawResponse, err = c.callAnthropic(ctx, settings, systemPrompt, userPrompt)
	case ProviderOpenRouter, ProviderCustom:
		rawResponse, err = c.callOpenAICompatible(ctx, settings, systemPrompt, userPrompt)
	default:
		return "", 0, fmt.Errorf("unsupported LLM provider: %s", settings.Provider)
	}

	latencyMs := int(time.Since(start).Milliseconds())
	if err != nil {
		return "", latencyMs, err
	}
	return rawResponse, latencyMs, nil
}

// callGemini calls the official Google Gemini API (v1beta REST).
func (c *Client) callGemini(
	ctx context.Context,
	settings Settings,
	systemPrompt, userPrompt string,
) (string, error) {
	model := settings.Model
	if model == "" {
		model = "gemini-2.0-flash"
	}
	baseURL := settings.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	endpoint := fmt.Sprintf("%s/models/%s:generateContent?key=%s", baseURL, model, settings.APIKey)

	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	type generationConfig struct {
		Temperature      float64        `json:"temperature"`
		ResponseMimeType string         `json:"responseMimeType,omitempty"`
		ThinkingConfig   map[string]any `json:"thinkingConfig,omitempty"`
	}
	type geminiReq struct {
		SystemInstruction *content          `json:"system_instruction,omitempty"`
		Contents          []content         `json:"contents"`
		GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	}

	reqBody := geminiReq{
		Contents: []content{
			{
				Role:  "user",
				Parts: []part{{Text: userPrompt}},
			},
		},
		GenerationConfig: &generationConfig{
			Temperature:      settings.Temperature,
			ResponseMimeType: "application/json",
		},
	}
	if systemPrompt != "" {
		reqBody.SystemInstruction = &content{
			Parts: []part{{Text: systemPrompt}},
		}
	}
	if settings.ThinkingEnabled && strings.Contains(strings.ToLower(model), "thinking") {
		reqBody.GenerationConfig.ThinkingConfig = map[string]any{
			"thinkingBudget": 1024,
		}
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read gemini response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini API error [%d]: %s", resp.StatusCode, string(respBytes))
	}

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
		return "", fmt.Errorf("parse gemini response: %w", err)
	}

	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("gemini returned no content candidate")
	}

	return geminiResp.Candidates[0].Content.Parts[0].Text, nil
}

// callAnthropic calls the Anthropic Messages API.
func (c *Client) callAnthropic(
	ctx context.Context,
	settings Settings,
	systemPrompt, userPrompt string,
) (string, error) {
	model := settings.Model
	if model == "" {
		model = "claude-3-7-sonnet-20250219"
	}
	endpoint := "https://api.anthropic.com/v1/messages"
	if settings.BaseURL != "" {
		endpoint = settings.BaseURL
	}

	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type anthropicReq struct {
		Model       string         `json:"model"`
		MaxTokens   int            `json:"max_tokens"`
		System      string         `json:"system,omitempty"`
		Messages    []msg          `json:"messages"`
		Temperature *float64       `json:"temperature,omitempty"`
		Thinking    map[string]any `json:"thinking,omitempty"`
	}

	reqBody := anthropicReq{
		Model:     model,
		MaxTokens: 2048,
		System:    systemPrompt,
		Messages:  []msg{{Role: "user", Content: userPrompt}},
	}
	if settings.ThinkingEnabled && strings.Contains(model, "3-7") {
		reqBody.Thinking = map[string]any{
			"type":          "enabled",
			"budget_tokens": 1024,
		}
	} else {
		temp := settings.Temperature
		reqBody.Temperature = &temp
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", settings.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("anthropic API error [%d]: %s", resp.StatusCode, string(respBytes))
	}

	var anthropicResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(respBytes, &anthropicResp); err != nil {
		return "", fmt.Errorf("parse anthropic response: %w", err)
	}

	for _, block := range anthropicResp.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", errors.New("anthropic returned no text block")
}

// callOpenAICompatible handles OpenRouter and standard OpenAI chat completion endpoints.
func (c *Client) callOpenAICompatible(
	ctx context.Context,
	settings Settings,
	systemPrompt, userPrompt string,
) (string, error) {
	model := settings.Model
	if model == "" {
		model = "anthropic/claude-3.7-sonnet"
	}
	endpoint := settings.BaseURL
	if endpoint == "" {
		if settings.Provider == ProviderOpenRouter {
			endpoint = "https://openrouter.ai/api/v1/chat/completions"
		} else {
			endpoint = "https://api.openai.com/v1/chat/completions"
		}
	}

	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type openAIReq struct {
		Model          string  `json:"model"`
		Messages       []msg   `json:"messages"`
		Temperature    float64 `json:"temperature"`
		ResponseFormat map[string]string `json:"response_format,omitempty"`
	}

	messages := make([]msg, 0, 2)
	if systemPrompt != "" {
		messages = append(messages, msg{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, msg{Role: "user", Content: userPrompt})

	reqBody := openAIReq{
		Model:          model,
		Messages:       messages,
		Temperature:    settings.Temperature,
		ResponseFormat: map[string]string{"type": "json_object"},
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+settings.APIKey)
	if settings.Provider == ProviderOpenRouter {
		req.Header.Set("HTTP-Referer", "https://pcrypto.ligam.org")
		req.Header.Set("X-Title", "Pionex AutoGrid Intelligence")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm completion network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("llm API error [%d]: %s", resp.StatusCode, string(respBytes))
	}

	var openAIResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBytes, &openAIResp); err != nil {
		return "", fmt.Errorf("parse completion response: %w", err)
	}

	if len(openAIResp.Choices) == 0 {
		return "", errors.New("llm returned no choices")
	}

	return openAIResp.Choices[0].Message.Content, nil
}
