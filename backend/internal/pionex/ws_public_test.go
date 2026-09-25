package pionex

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
)

// wsTestServer hosts a minimal INDEX stream: it records subscribe frames and
// lets tests push envelopes exactly as the exchange would.
type wsTestServer struct {
	server     *httptest.Server
	url        string
	mu         sync.Mutex
	subscribed []string
	frames     []string
	conns      []*websocket.Conn
	push       chan []byte
}

func newWSTestServer(t *testing.T) *wsTestServer {
	t.Helper()
	s := &wsTestServer{push: make(chan []byte, 16)}
	upgrader := websocket.Upgrader{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		defer conn.Close()
		go func() {
			for frame := range s.push {
				_ = conn.WriteMessage(websocket.TextMessage, frame)
			}
		}()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.frames = append(s.frames, string(raw))
			var env struct {
				Op     string `json:"op"`
				Symbol string `json:"symbol"`
			}
			_ = json.Unmarshal(raw, &env)
			if env.Op == "SUBSCRIBE" {
				s.subscribed = append(s.subscribed, env.Symbol)
			}
			s.mu.Unlock()
		}
	}))
	s.url = "ws" + strings.TrimPrefix(s.server.URL, "http")
	t.Cleanup(func() {
		close(s.push)
		s.server.Close()
	})
	return s
}

func (s *wsTestServer) subscribedSymbols() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.subscribed...)
}

func (s *wsTestServer) sentFrames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.frames...)
}

func (s *wsTestServer) pushIndex(symbol, mark string) {
	payload := map[string]any{
		"topic":     "INDEX",
		"symbol":    symbol,
		"timestamp": time.Now().UnixMilli(),
		"data": []map[string]any{{
			"symbol":          symbol,
			"indexPrice":      mark,
			"markPrice":       mark,
			"nextFundingRate": "0.0001",
			"nextFundingTime": 1790000000000,
			"updateTime":      time.Now().UnixMilli(),
		}},
	}
	raw, _ := json.Marshal(payload)
	s.push <- raw
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within 2s")
}

func TestPublicStreamSubscribesAndStoresMarks(t *testing.T) {
	srv := newWSTestServer(t)
	stream := NewPublicStream(srv.url, testLogger(t))
	go stream.Run(t.Context())

	stream.SetSymbols([]string{"BTC_USDT_PERP", " xmr_usdt_perp "})
	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 2 })

	srv.pushIndex("BTC_USDT_PERP", "571.25")
	waitFor(t, func() bool {
		m, ok := stream.Mark("BTC_USDT_PERP")
		return ok && m.MarkPrice.Equal(decimal.RequireFromString("571.25"))
	})

	mark, ok := stream.Mark("XMR_USDT_PERP")
	if ok {
		// Only BTC got pushed; the normalized XMR subscription exists but
		// has no mark yet. Any stored value must still be fresh-consistent.
		if !mark.Fresh(time.Minute) && !mark.ReceivedAt.IsZero() {
			t.Fatalf("stale mark reported fresh")
		}
	}
	if !stream.Connected() {
		t.Fatalf("stream should report connected")
	}
}

func TestPublicStreamPingGetsPong(t *testing.T) {
	srv := newWSTestServer(t)
	stream := NewPublicStream(srv.url, testLogger(t))
	go stream.Run(t.Context())
	stream.SetSymbols([]string{"BTC_USDT_PERP"})
	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 1 })

	// Server-initiated PING per docs; the client must echo with PONG and
	// the same timestamp.
	srv.push <- []byte(`{"op":"PING","timestamp":1790000000123}`)
	waitFor(t, func() bool {
		for _, frame := range srv.sentFrames() {
			if strings.Contains(frame, `"PONG"`) && strings.Contains(frame, "1790000000123") {
				return true
			}
		}
		return false
	})
}

func TestPublicStreamDiffSubscriptions(t *testing.T) {
	srv := newWSTestServer(t)
	stream := NewPublicStream(srv.url, testLogger(t))
	go stream.Run(t.Context())

	stream.SetSymbols([]string{"BTC_USDT_PERP"})
	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 1 })
	stream.SetSymbols([]string{"BTC_USDT_PERP", "ETH_USDT_PERP"})
	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 2 })

	// A no-op sync must not hit the wire.
	stream.SetSymbols([]string{"ETH_USDT_PERP", "BTC_USDT_PERP"})
	time.Sleep(100 * time.Millisecond)
	if got := len(srv.subscribedSymbols()); got != 2 {
		t.Fatalf("no-op diff produced frames: %d subscribe frames", got)
	}
}

func TestPublicStreamReconnectResubscribes(t *testing.T) {
	srv := newWSTestServer(t)
	stream := NewPublicStream(srv.url, testLogger(t))
	go stream.Run(t.Context())
	stream.SetSymbols([]string{"BTC_USDT_PERP"})
	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 1 })

	// Kill the socket server-side; the lane must reconnect and resubscribe.
	srv.mu.Lock()
	for _, conn := range srv.conns {
		_ = conn.Close()
	}
	srv.conns = nil
	srv.mu.Unlock()
	srv.mu.Lock()
	srv.subscribed = nil
	srv.mu.Unlock()

	waitFor(t, func() bool { return len(srv.subscribedSymbols()) == 1 })
}

func TestIndexPayloadParsingShapes(t *testing.T) {
	stream := NewPublicStream("ws://unused", testLogger(t))

	// Documented shape: data is an array.
	stream.handleFrame([]byte(`{"topic":"INDEX","symbol":"BTC_USDT_PERP","timestamp":1,
		"data":[{"symbol":"BTC_USDT_PERP","indexPrice":"570","markPrice":"571.5","nextFundingRate":"0.0001","nextFundingTime":1790000000000,"updateTime":1790000000500}]}`))
	mark, ok := stream.Mark("BTC_USDT_PERP")
	if !ok || !mark.MarkPrice.Equal(decimal.RequireFromString("571.5")) {
		t.Fatalf("array payload not stored: %+v ok=%v", mark, ok)
	}
	if mark.NextFundingRate.String() != "0.0001" {
		t.Fatalf("funding rate lost: %s", mark.NextFundingRate)
	}

	// Defensive shape: single object instead of an array.
	stream.handleFrame([]byte(`{"topic":"INDEX","symbol":"XMR_USDT_PERP","timestamp":2,
		"data":{"symbol":"XMR_USDT_PERP","indexPrice":"550","markPrice":"552","updateTime":0}}`))
	mark, ok = stream.Mark("XMR_USDT_PERP")
	if !ok || !mark.MarkPrice.Equal(decimal.RequireFromString("552")) {
		t.Fatalf("object payload not stored: %+v ok=%v", mark, ok)
	}

	// A zero mark must not poison the store.
	stream.handleFrame([]byte(`{"topic":"INDEX","symbol":"BAD_USDT_PERP","timestamp":3,
		"data":[{"symbol":"BAD_USDT_PERP","markPrice":"0","updateTime":0}]}`))
	if _, ok := stream.Mark("BAD_USDT_PERP"); ok {
		t.Fatalf("zero mark stored")
	}
}

func TestMarkFreshness(t *testing.T) {
	m := MarkUpdate{ReceivedAt: time.Now().Add(-2 * time.Minute)}
	if m.Fresh(time.Minute) {
		t.Fatalf("2-minute-old mark must not be fresh")
	}
	m.ReceivedAt = time.Now()
	if !m.Fresh(time.Minute) {
		t.Fatalf("fresh mark rejected")
	}
	if (MarkUpdate{}).Fresh(time.Minute) {
		t.Fatalf("zero mark must not be fresh")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
