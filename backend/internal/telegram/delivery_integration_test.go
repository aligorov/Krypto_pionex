package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Every test uses a disposable PostgreSQL and an in-memory Telegram transport.
// No real messages or exchange operations are sent.
func telegramTestDispatcher(t *testing.T) *OutboxDispatcher {
	t.Helper()
	dsn := os.Getenv("PIONEX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM notification_outbox`, `DELETE FROM telegram_poll_state`,
		`UPDATE telegram_settings SET enabled=true,bot_token='test-token',chat_id='42',topic_id='7',notify_digest=true,notify_emergency=true,last_digest_at=NULL,digest_interval_minutes=60 WHERE id=1`,
		`UPDATE risk_settings SET kill_switch_enabled=false WHERE id=1`,
		`UPDATE app_config SET value='false'::jsonb WHERE key='telegram_write_commands_enabled'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	d := NewOutboxDispatcher(pool, "", "")
	d.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	d.SetBuildInfo("test-version", "test-commit", "test-build")
	d.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mockResponse(200, `{"ok":true,"result":{"message_id":77}}`), nil
	})}
	return d
}

func enqueueTest(t *testing.T, d *OutboxDispatcher, payload string) {
	t.Helper()
	if _, err := d.db.Exec(context.Background(), `INSERT INTO notification_outbox(event_type,payload) VALUES ('EMERGENCY',$1::jsonb)`, payload); err != nil {
		t.Fatal(err)
	}
}
func deliveryState(t *testing.T, d *OutboxDispatcher) (string, int) {
	t.Helper()
	var state string
	var parts int
	if err := d.db.QueryRow(context.Background(), `SELECT status,sent_parts FROM notification_outbox ORDER BY created_at LIMIT 1`).Scan(&state, &parts); err != nil {
		t.Fatal(err)
	}
	return state, parts
}

func TestDeliveryRetriesRateLimitAndPreservesTopic(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	calls := 0
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["message_thread_id"] != float64(7) {
			t.Errorf("topic lost: %+v", body)
		}
		if calls == 1 {
			return mockResponse(429, `{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":5}}`), nil
		}
		return mockResponse(200, `{"ok":true,"result":{"message_id":77}}`), nil
	})
	enqueueTest(t, d, `{"message":"Feed alert"}`)
	if err := d.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _ := deliveryState(t, d); state != "PENDING" {
		t.Fatal("rate-limit permanently lost the alert")
	}
	if err := d.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("retry_after ignored")
	}
	if _, err := d.db.Exec(ctx, `UPDATE telegram_poll_state SET send_paused_until=NOW();UPDATE notification_outbox SET scheduled_at=NOW()`); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchPending(ctx); err != nil {
		t.Fatal(err)
	}
	if state, parts := deliveryState(t, d); state != "SENT" || parts != 1 {
		t.Fatalf("delivery: %s %d", state, parts)
	}
}

func TestDeliveryDoesNotMarkOKFalseAsSent(t *testing.T) {
	d := telegramTestDispatcher(t)
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mockResponse(200, `{"ok":false,"error_code":403,"description":"blocked"}`), nil
	})
	enqueueTest(t, d, `{"text":"alert"}`)
	if err := d.DispatchPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, parts := deliveryState(t, d); state != "FAILED" || parts != 0 {
		t.Fatalf("false delivery: %s %d", state, parts)
	}
}

func TestLongDeliveryResumesAfterFailedPart(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	var texts []string
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		texts = append(texts, body["text"].(string))
		if len(texts) == 2 {
			return mockResponse(503, `{"ok":false,"error_code":503,"description":"temporary"}`), nil
		}
		return mockResponse(200, `{"ok":true,"result":{"message_id":77}}`), nil
	})
	raw, _ := json.Marshal(map[string]string{"text": "<pre>" + strings.Repeat("A", 3000) + strings.Repeat("B", 3000) + "C</pre>"})
	enqueueTest(t, d, string(raw))
	for i := 0; i < 4; i++ {
		if _, err := d.db.Exec(ctx, `UPDATE notification_outbox SET scheduled_at=NOW()`); err != nil {
			t.Fatal(err)
		}
		if err := d.DispatchPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if state, parts := deliveryState(t, d); state != "SENT" || parts != 3 {
		t.Fatalf("unfinished report: %s %d", state, parts)
	}
	if len(texts) != 4 || texts[1] != texts[2] || texts[0] == texts[2] {
		t.Fatal("delivered part was replayed instead of failed part")
	}
}

func TestConcurrentDispatchClaimsOnce(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	calls := 0
	var mu sync.Mutex
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return mockResponse(200, `{"ok":true,"result":{"message_id":77}}`), nil
	})
	enqueueTest(t, d, `{"text":"one"}`)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.DispatchPending(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("duplicate delivery: %d", calls)
	}
}

func TestInboundCursorSurvivesRestartAndDuplicateUpdate(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	offsets := []float64{}
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "setMyCommands") {
			return mockResponse(200, `{"ok":true,"result":true}`), nil
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		offsets = append(offsets, body["offset"].(float64))
		return mockResponse(200, `{"ok":true,"result":[{"update_id":10,"message":{"from":{"id":42},"chat":{"id":42,"type":"private"},"message_thread_id":7,"text":"/start"}}]}`), nil
	})
	d.pollUpdates(ctx)
	restarted := NewOutboxDispatcher(d.db, "", "")
	restarted.httpClient = d.httpClient
	restarted.logger = d.logger
	restarted.pollUpdates(ctx)
	var count int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox WHERE event_type='TG_CONSOLE'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(offsets) != 2 || offsets[1] != 11 {
		t.Fatalf("replayed: count=%d offsets=%v", count, offsets)
	}
}

func TestOtherPollerConflictDoesNotDeleteWebhook(t *testing.T) {
	d := telegramTestDispatcher(t)
	deletes := 0
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "deleteWebhook") {
			deletes++
		}
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			return mockResponse(409, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`), nil
		}
		return mockResponse(200, `{"ok":true,"result":true}`), nil
	})
	d.pollUpdates(context.Background())
	if deletes != 0 {
		t.Fatal("unrelated webhook was deleted")
	}
}

func TestConcurrentPollingUsesSharedDatabaseLock(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return mockResponse(200, `{"ok":true,"result":[]}`), nil
		}
		return mockResponse(200, `{"ok":true,"result":true}`), nil
	})
	go func() { d.pollUpdates(ctx); close(done) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first poll did not start")
	}
	other := NewOutboxDispatcher(d.db, "", "")
	other.httpClient = d.httpClient
	other.logger = d.logger
	other.pollUpdates(ctx)
	mu.Lock()
	count := calls
	mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("poll did not finish")
	}
	if count != 1 {
		t.Fatalf("concurrent getUpdates calls: %d", count)
	}
}

func TestUnifiedNotificationPolicyHonorsFlags(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	if _, err := d.db.Exec(ctx, `UPDATE telegram_settings SET notify_emergency=false,notify_range_adjust=false WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"WS_RATE_LIMIT", "FUNDING_STALE", "EMERGENCY", "ADJUST_RANGE"} {
		if err := d.service.EnqueueNotification(ctx, event, map[string]any{"message": "test"}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("notification flag bypassed: %d %v", count, err)
	}
}

func TestDigestCadenceAndDisabledCredentials(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := d.QueueDigestIfDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox WHERE event_type='DIGEST'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("digest cadence ignored: %d", count)
	}
	if _, err := d.db.Exec(ctx, `UPDATE telegram_settings SET enabled=false,last_digest_at=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueueDigestIfDue(ctx); err != nil {
		t.Fatal(err)
	}
	fallback := NewOutboxDispatcher(d.db, "fallback-token", "fallback-chat")
	if _, _, _, enabled := fallback.getActiveCredentials(ctx); enabled {
		t.Fatal("disabled database setting was bypassed")
	}
}

func TestUnauthorizedCallbackIsAcknowledgedWithoutExecution(t *testing.T) {
	d := telegramTestDispatcher(t)
	ctx := context.Background()
	acknowledged := false
	d.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getUpdates") {
			return mockResponse(200, `{"ok":true,"result":[{"update_id":12,"callback_query":{"id":"blocked","from":{"id":43},"data":"cmd_kill_switch","message":{"chat":{"id":42,"type":"private"},"message_thread_id":7}}}]}`), nil
		}
		if strings.HasSuffix(r.URL.Path, "answerCallbackQuery") {
			acknowledged = true
		}
		return mockResponse(200, `{"ok":true,"result":true}`), nil
	})
	d.pollUpdates(ctx)
	var kill bool
	if err := d.db.QueryRow(ctx, `SELECT kill_switch_enabled FROM risk_settings WHERE id=1`).Scan(&kill); err != nil {
		t.Fatal(err)
	}
	if !acknowledged || kill {
		t.Fatalf("denial not acknowledged or executed: ack=%v kill=%v", acknowledged, kill)
	}
}
