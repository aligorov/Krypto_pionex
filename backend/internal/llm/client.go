package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

// ValidateBaseURL enforces that a custom base URL points at the official API
// host of the provider. Without this gate, a request could point the server
// at an attacker-controlled host and exfiltrate the stored API key that is
// attached to every provider call (audit SEC-005).
func ValidateBaseURL(provider, baseURL string) error {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("base URL must be a valid https:// URL")
	}
	host := strings.ToLower(parsed.Hostname())
	var official string
	switch provider {
	case ProviderGemini:
		official = "generativelanguage.googleapis.com"
	case ProviderAnthropic:
		official = "api.anthropic.com"
	case ProviderOpenRouter:
		official = "openrouter.ai"
	case ProviderCustom:
		// Custom providers may use any public https host, but never an
		// internal address the stored key must not be sent to.
		if ip := net.ParseIP(host); ip != nil &&
			(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
			return errors.New("custom base URL must not point at internal addresses")
		}
		if host == "localhost" || strings.HasSuffix(host, ".local") ||
			strings.HasSuffix(host, ".internal") {
			return errors.New("custom base URL must not point at internal addresses")
		}
		return nil
	default:
		return fmt.Errorf("unsupported LLM provider: %s", provider)
	}
	if host != official {
		return fmt.Errorf("base URL host must be %s for provider %s", official, provider)
	}
	return nil
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
	if err := ValidateBaseURL(settings.Provider, settings.BaseURL); err != nil {
		return "", 0, fmt.Errorf("LLM base URL rejected: %w", err)
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
		model = "gemini-3.7-flash"
	}
	baseURL := settings.BaseURL
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	// The API key travels in the x-goog-api-key header only: URLs are logged
	// by proxies and log pipelines, so it must never be a query parameter.
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", baseURL, url.PathEscape(model))

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
	type geminiTool struct {
		GoogleSearch map[string]any `json:"google_search,omitempty"`
	}
	type geminiReq struct {
		SystemInstruction *content          `json:"system_instruction,omitempty"`
		Contents          []content         `json:"contents"`
		GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
		Tools             []geminiTool      `json:"tools,omitempty"`
	}

	reqBody := geminiReq{
		Contents: []content{
			{
				Role:  "user",
				Parts: []part{{Text: userPrompt}},
			},
		},
		GenerationConfig: &generationConfig{
			Temperature: settings.Temperature,
		},
	}
	// google_search grounding turns the catalyst investigation from
	// memory-recall into a live web lookup. JSON response mode is mutually
	// exclusive with tools on this API, so with grounding enabled the mime
	// type is dropped and the strict prompt + CleanJSONResponse take over.
	grounded := settings.GroundingEnabled
	if grounded {
		reqBody.Tools = []geminiTool{{GoogleSearch: map[string]any{}}}
	} else {
		reqBody.GenerationConfig.ResponseMimeType = "application/json"
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

	sendGemini := func(body geminiReq) ([]byte, int, error) {
		jsonBytes, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, fmt.Errorf("marshal gemini request: %w", marshalErr)
		}
		// Free-tier rate limit is ~15 RPM; grounded requests hit it fast.
		// Retry 429s with exponential backoff (1s, 3s, 7s).
		for attempt := 0; attempt < 3; attempt++ {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(jsonBytes))
			if reqErr != nil {
				return nil, 0, reqErr
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-goog-api-key", strings.TrimSpace(settings.APIKey))
			resp, doErr := c.httpClient.Do(req)
			if doErr != nil {
				return nil, 0, fmt.Errorf("gemini network error: %w", doErr)
			}
			respBytes, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				resp.Body.Close()
				return nil, resp.StatusCode, fmt.Errorf("read gemini response: %w", readErr)
			}
			if resp.StatusCode == 429 && attempt < 2 {
				resp.Body.Close()
				backoff := time.Duration(1<<attempt) * 3 * time.Second / 2 // 1.5s, 3s
				select {
				case <-ctx.Done():
					return nil, 429, ctx.Err()
				case <-time.After(backoff):
				}
				continue
			}
			resp.Body.Close()
			return respBytes, resp.StatusCode, nil
		}
		return nil, 0, fmt.Errorf("gemini rate limit: exhausted retries")
	}

	respBytes, statusCode, err := sendGemini(reqBody)
	if err != nil {
		return "", err
	}
	// Some API revisions reject the google_search tool combination: retry
	// once without grounding (JSON mode restored) before surfacing errors.
	if grounded && statusCode >= 400 {
		fallbackBody := reqBody
		fallbackBody.Tools = nil
		fallbackBody.GenerationConfig.ResponseMimeType = "application/json"
		if retryBytes, retryStatus, retryErr := sendGemini(fallbackBody); retryErr == nil && retryStatus < 400 {
			respBytes, statusCode = retryBytes, retryStatus
		}
	}

	if statusCode >= 400 {
		return "", fmt.Errorf("gemini API error [%d]: %s", statusCode, string(respBytes))
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
		Model          string            `json:"model"`
		Messages       []msg             `json:"messages"`
		Temperature    float64           `json:"temperature"`
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

// ListAvailableModels queries the provider's API for the list of available models.
func (c *Client) ListAvailableModels(ctx context.Context, settings Settings) ([]string, error) {
	if err := ValidateBaseURL(settings.Provider, settings.BaseURL); err != nil {
		return nil, fmt.Errorf("LLM base URL rejected: %w", err)
	}
	switch settings.Provider {
	case ProviderGemini:
		baseURL := settings.BaseURL
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com/v1beta"
		}
		endpoint := fmt.Sprintf("%s/models", baseURL)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", strings.TrimSpace(settings.APIKey))
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gemini models network error: %w", err)
		}
		defer resp.Body.Close()
		respBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("gemini API error [%d]: %s", resp.StatusCode, string(respBytes))
		}
		var listResp struct {
			Models []struct {
				Name                       string   `json:"name"`
				DisplayName                string   `json:"displayName"`
				SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
			} `json:"models"`
		}
		if err := json.Unmarshal(respBytes, &listResp); err != nil {
			return nil, fmt.Errorf("parse gemini models: %w", err)
		}
		models := make([]string, 0, len(listResp.Models))
		for _, m := range listResp.Models {
			canGenerate := false
			for _, method := range m.SupportedGenerationMethods {
				if method == "generateContent" {
					canGenerate = true
					break
				}
			}
			if canGenerate {
				cleanName := strings.TrimPrefix(m.Name, "models/")
				models = append(models, cleanName)
			}
		}
		return models, nil

	case ProviderOpenRouter:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+settings.APIKey)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("openrouter models network error: %w", err)
		}
		defer resp.Body.Close()
		respBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("openrouter API error [%d]: %s", resp.StatusCode, string(respBytes))
		}
		var orResp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBytes, &orResp); err != nil {
			return nil, fmt.Errorf("parse openrouter models: %w", err)
		}
		models := make([]string, 0, len(orResp.Data))
		for _, d := range orResp.Data {
			models = append(models, d.ID)
		}
		return models, nil

	case ProviderAnthropic:
		return []string{
			"claude-3-7-sonnet-20250219",
			"claude-3-5-sonnet-20241022",
			"claude-3-5-haiku-20241022",
			"claude-3-opus-20240229",
		}, nil

	default:
		return []string{"default"}, nil
	}
}
