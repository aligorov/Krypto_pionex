package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf16"
)

// APIError excludes the credential-bearing URL from every user/log response.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram %d: %s", e.Code, e.Description) }

func callTelegram(ctx context.Context, client *http.Client, token, method string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/"+method, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("Telegram request: %s", strings.ReplaceAll(err.Error(), token, "[redacted]"))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram transport: %s", strings.ReplaceAll(err.Error(), token, "[redacted]"))
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&envelope); err != nil {
		return &APIError{Code: resp.StatusCode, Description: "invalid Bot API response"}
	}
	if resp.StatusCode != http.StatusOK || !envelope.OK {
		code := envelope.Code
		if code == 0 {
			code = resp.StatusCode
		}
		return &APIError{Code: code, Description: strings.ReplaceAll(envelope.Description, token, "[redacted]"), RetryAfter: envelope.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

var htmlTokens = regexp.MustCompile(`(?s)<[^>]+>|&(?:#[0-9]+|#x[0-9a-fA-F]+|[a-zA-Z]+);|.`)
var htmlTags = regexp.MustCompile(`<[^>]+>`)

func plainText(text string) string { return html.UnescapeString(htmlTags.ReplaceAllString(text, "")) }

// Preserve tags and entities across pieces. The conservative 3000-character
// budget also leaves space for closing/reopening formatting around a piece.
func splitHTML(text string) []string {
	var parts []string
	var b strings.Builder
	var stack []string
	count := 0
	closeTags := func() string {
		var s strings.Builder
		for i := len(stack) - 1; i >= 0; i-- {
			fields := strings.Fields(strings.Trim(stack[i], "<>"))
			if len(fields) > 0 {
				s.WriteString("</" + fields[0] + ">")
			}
		}
		return s.String()
	}
	flush := func() {
		if count == 0 {
			return
		}
		parts = append(parts, b.String()+closeTags())
		b.Reset()
		for _, tag := range stack {
			b.WriteString(tag)
		}
		count = 0
	}
	for _, unit := range htmlTokens.FindAllString(text, -1) {
		if strings.HasPrefix(unit, "<") {
			if strings.HasPrefix(unit, "</") {
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			} else if !strings.HasSuffix(unit, "/>") {
				stack = append(stack, unit)
			}
			b.WriteString(unit)
			continue
		}
		if count >= 3000 {
			flush()
		}
		b.WriteString(unit)
		for _, r := range html.UnescapeString(unit) {
			count += utf16.RuneLen(r)
		}
	}
	flush()
	return parts
}

func (d *OutboxDispatcher) sendPart(ctx context.Context, token, chatID, topicID, text string, kb *InlineKeyboardMarkup) (int64, error) {
	body := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"}
	if topicID != "" {
		topic, err := parseTopic(topicID)
		if err != nil {
			return 0, err
		}
		body["message_thread_id"] = topic
	}
	if kb != nil {
		body["reply_markup"] = kb
	}
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	err := callTelegram(ctx, d.httpClient, token, "sendMessage", body, &result)
	// An HTML parse failure must not suppress an emergency or operator reply.
	if apiErr, ok := err.(*APIError); ok && apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "parse") {
		body["text"] = plainText(text)
		delete(body, "parse_mode")
		err = callTelegram(ctx, d.httpClient, token, "sendMessage", body, &result)
	}
	if err == nil && result.MessageID <= 0 {
		err = fmt.Errorf("Telegram sendMessage returned no message_id")
	}
	return result.MessageID, err
}
