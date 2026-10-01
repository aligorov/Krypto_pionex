package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestBotAPIValidatesEnvelopeAndRedactsToken(t *testing.T) {
	const token = "123:secret-token"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mockResponse(200, `{"ok":false,"error_code":429,"description":"flood","parameters":{"retry_after":9}}`), nil
	})}
	err := callTelegram(context.Background(), client, token, "sendMessage", nil, nil)
	api, ok := err.(*APIError)
	if !ok || api.Code != 429 || api.RetryAfter != 9 {
		t.Fatalf("lost API error: %v", err)
	}
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("failed %s", r.URL.String()) })
	err = callTelegram(context.Background(), client, token, "sendMessage", nil, nil)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("token exposed: %v", err)
	}
}

func TestNotificationPayloadAndEscaping(t *testing.T) {
	n, err := decodeNotification(`{"message":"⚠️ Feed alert"}`, "WS_RATE_LIMIT")
	if err != nil || n.Text != "⚠️ Feed alert" || n.Keyboard == nil {
		t.Fatalf("legacy message lost: %+v %v", n, err)
	}
	if _, err = decodeNotification(`{"reason":"silent"}`, "EMERGENCY"); err == nil {
		t.Fatal("empty payload accepted")
	}
	text := RenderTemplate(`<b>{{symbol}}</b>: {{error}}`, map[string]any{"symbol": "A&B", "error": "price <5"})
	if text != `<b>A&amp;B</b>: price &lt;5` {
		t.Fatal(text)
	}
}

func TestLongHTMLKeepsFormattingAndText(t *testing.T) {
	text := "<b>Сводка</b>\n<pre>" + strings.Repeat("Строка &lt; 5\n", 1000) + "</pre>"
	parts := splitHTML(text)
	if len(parts) < 2 {
		t.Fatal("long report not split")
	}
	var joined strings.Builder
	for _, part := range parts {
		if utf8.RuneCountInString(plainText(part)) > 4096 {
			t.Fatal("Telegram character limit exceeded")
		}
		if strings.Count(part, "<pre>") != strings.Count(part, "</pre>") {
			t.Fatal("formatting split mid-tag")
		}
		joined.WriteString(plainText(part))
	}
	if joined.String() != plainText(text) {
		t.Fatal("report text changed during splitting")
	}
}

func TestPrivateAuthorizationAndTopics(t *testing.T) {
	private := telegramChat{ID: 42, Type: "private"}
	if !authorizedPrivate(private, telegramUser{ID: 42}, "42") {
		t.Fatal("operator refused")
	}
	for _, tc := range []struct {
		chat telegramChat
		from telegramUser
	}{
		{telegramChat{ID: -42, Type: "group"}, telegramUser{ID: 42}},
		{private, telegramUser{ID: 43}}, {private, telegramUser{ID: 42, IsBot: true}},
	} {
		if authorizedPrivate(tc.chat, tc.from, "42") {
			t.Fatal("unauthorized update accepted")
		}
	}
	if !authorizedTopic(7, "7") || authorizedTopic(8, "7") {
		t.Fatal("topic authorization failed")
	}
}

func TestDayOrderingCrossesMonthAndYear(t *testing.T) {
	var rows []closedRow
	for _, day := range []string{"2025-12-31T23:59:00Z", "2026-01-01T00:01:00Z"} {
		var r closedRow
		if err := json.Unmarshal([]byte(`{"ClosedAt":"`+day+`"}`), &r); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	days := dayStatsFrom(rows)
	if len(days) != 2 || days[0].Day != "2026-01-01" || days[1].Losses != 0 {
		t.Fatalf("bad day buckets: %+v", days)
	}
}
