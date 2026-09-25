package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/gorilla/websocket"
)

// v2.0.98 real-time lane: the supervision stack previously sampled prices
// only through REST snapshots taken at the start of each manage pass, so a
// mark shown to the operator (and fed to the radar velocity trail) was up to
// manageIntervalSeconds + pass-duration old against the exchange app. The
// public futures WebSocket pushes the INDEX topic — markPrice, indexPrice,
// nextFundingRate — per symbol, which is the exact PnL reference the manage
// loop already prefers via GetIndexes (v2.0.58 F8).
//
// Docs (futures-websocket): endpoint wss://ws.pionex.com/wsPub, subscribe
// {"op":"SUBSCRIBE","topic":"INDEX","symbol":"BTC_USDT_PERP"}, server sends
// {"op":"PING","timestamp":ms} every 15s and the client MUST answer
// {"op":"PONG","timestamp":ms} (3 missed PONGs → disconnect), limits 5
// messages/second per connection and 10 connections per IP. The push
// envelope is {topic, symbol, timestamp, data:[...]}.
//
// The lane is advisory only: it never gates a REST truth read. When it is
// down or stale, every consumer silently falls back to the REST snapshot —
// a WebSocket outage must not wedge supervision (same rule as the GetTickers
// fallback in priceMap).
const (
	DefaultPublicWSURL = "wss://ws.pionex.com/wsPub"
	// Server pings every 15s; three missed pings disconnect, so a silent
	// 45s window means the connection is already dead from the server's
	// perspective — drop it slightly earlier and reconnect ourselves.
	wsReadDeadline = 40 * time.Second
	// Reconnect backoff: 1s doubling capped at 30s, reset after a frame
	// arrives on a healthy connection.
	wsBackoffMax = 30 * time.Second
	// Hard cap on concurrent INDEX subscriptions. The fleet runs 20 slots,
	// so 60 leaves headroom while staying far below the 5 msg/s budget for
	// subscribe/unsubscribe bursts.
	MaxStreamSymbols = 60
)

// MarkUpdate is one INDEX push for one symbol.
type MarkUpdate struct {
	Symbol          string
	MarkPrice       decimal.Decimal
	IndexPrice      decimal.Decimal
	NextFundingRate decimal.Decimal
	NextFundingTime int64
	ReceivedAt      time.Time
}

// Fresh reports whether the mark is younger than maxAge.
func (m MarkUpdate) Fresh(maxAge time.Duration) bool {
	return !m.ReceivedAt.IsZero() && time.Since(m.ReceivedAt) <= maxAge
}

type PublicStream struct {
	url    string
	logger *slog.Logger

	// writeMu serializes frames onto the socket (subscribe diffs from the
	// manage goroutine, PONG replies from the read pump).
	writeMu sync.Mutex
	connMu  sync.Mutex
	conn    *websocket.Conn

	// stateMu guards the subscription wish and the mark store; reads come
	// from manage-loop consumers, writes from the read pump and SetSymbols.
	stateMu     sync.RWMutex
	want        map[string]struct{}
	subscribed  map[string]struct{}
	marks       map[string]MarkUpdate
	connected   bool
	lastFrameAt time.Time
	backoff     time.Duration
	// firstPayloadLogged keys symbols whose first live INDEX push was logged
	// (observability rule: every new endpoint gets a raw-payload snippet
	// until trusted against the docs).
	firstPayloadLogged map[string]struct{}
}

// NewPublicStream builds a lane client for the public futures stream.
func NewPublicStream(url string, logger *slog.Logger) *PublicStream {
	if url == "" {
		url = DefaultPublicWSURL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PublicStream{
		url:                url,
		logger:             logger,
		want:               make(map[string]struct{}),
		subscribed:         make(map[string]struct{}),
		marks:              make(map[string]MarkUpdate),
		firstPayloadLogged: make(map[string]struct{}),
	}
}

// SetSymbols diffs the subscription wish. Only the delta hits the wire, so a
// steady fleet costs zero messages and the 5 msg/s limit survives bursts.
func (s *PublicStream) SetSymbols(symbols []string) {
	next := make(map[string]struct{}, len(symbols))
	for _, sym := range symbols {
		sym = normalizeStreamSymbol(sym)
		if sym == "" {
			continue
		}
		next[sym] = struct{}{}
	}

	s.stateMu.Lock()
	added, removed := make([]string, 0, len(next)), make([]string, 0)
	for sym := range next {
		if _, ok := s.want[sym]; !ok {
			added = append(added, sym)
		}
	}
	for sym := range s.want {
		if _, ok := next[sym]; !ok {
			removed = append(removed, sym)
		}
	}
	s.want = next
	connected := s.connected
	s.stateMu.Unlock()

	if !connected {
		return
	}
	for _, sym := range added {
		if err := s.sendStreamOp("SUBSCRIBE", sym); err != nil {
			s.logger.Warn("ws lane subscribe failed", "component", "pionex_ws", "symbol", sym, "error", err)
		}
	}
	for _, sym := range removed {
		if err := s.sendStreamOp("UNSUBSCRIBE", sym); err != nil {
			s.logger.Debug("ws lane unsubscribe failed", "component", "pionex_ws", "symbol", sym, "error", err)
		}
	}
}

// Mark returns the last INDEX push for a symbol.
func (s *PublicStream) Mark(symbol string) (MarkUpdate, bool) {
	sym := normalizeStreamSymbol(symbol)
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	m, ok := s.marks[sym]
	return m, ok
}

// Connected reports whether the socket is currently up.
func (s *PublicStream) Connected() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.connected
}

// LastFrameAt returns the time of the last received frame (any kind).
func (s *PublicStream) LastFrameAt() time.Time {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.lastFrameAt
}

// Run maintains the connection for the life of ctx: connect, pump reads,
// reconnect with backoff. It never returns an error to the caller — the lane
// is advisory, so every failure degrades to the REST fallback instead.
func (s *PublicStream) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := s.dial(ctx); err != nil {
			s.logger.Warn("ws lane dial failed", "component", "pionex_ws", "url", s.url, "error", err)
		} else if err := s.readPump(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("ws lane read pump ended", "component", "pionex_ws", "error", err)
		}
		s.markDisconnected()

		backoff := s.currentBackoff()
		s.logger.Info("ws lane reconnect scheduled", "component", "pionex_ws", "backoff_seconds", backoff.Seconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func (s *PublicStream) currentBackoff() time.Duration {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.backoff == 0 {
		s.backoff = time.Second
	} else {
		s.backoff *= 2
		if s.backoff > wsBackoffMax {
			s.backoff = wsBackoffMax
		}
	}
	return s.backoff
}

func (s *PublicStream) resetBackoff() {
	s.stateMu.Lock()
	s.backoff = 0
	s.stateMu.Unlock()
}

func (s *PublicStream) dial(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, s.url, nil)
	if err != nil {
		return err
	}
	s.connMu.Lock()
	s.conn = conn
	s.connMu.Unlock()

	s.stateMu.Lock()
	s.connected = true
	s.lastFrameAt = time.Now()
	// Resubscribe the full wish on a fresh socket: the server has no memory
	// of the previous connection's topics.
	s.subscribed = make(map[string]struct{}, len(s.want))
	wish := make([]string, 0, len(s.want))
	for sym := range s.want {
		wish = append(wish, sym)
	}
	s.stateMu.Unlock()

	for _, sym := range wish {
		if err := s.sendStreamOp("SUBSCRIBE", sym); err != nil {
			s.logger.Warn("ws lane resubscribe failed", "component", "pionex_ws", "symbol", sym, "error", err)
		}
	}
	s.logger.Info("ws lane connected", "component", "pionex_ws", "url", s.url, "symbols", len(wish))
	return nil
}

func (s *PublicStream) markDisconnected() {
	s.connMu.Lock()
	conn := s.conn
	s.conn = nil
	s.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	s.stateMu.Lock()
	s.connected = false
	s.subscribed = make(map[string]struct{})
	s.stateMu.Unlock()
}

func (s *PublicStream) sendStreamOp(op, symbol string) error {
	s.connMu.Lock()
	conn := s.conn
	s.connMu.Unlock()
	if conn == nil {
		return fmt.Errorf("websocket not connected")
	}
	frame, err := json.Marshal(map[string]string{"op": op, "topic": "INDEX", "symbol": symbol})
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, frame)
}

func (s *PublicStream) replyPong(timestamp int64) error {
	s.connMu.Lock()
	conn := s.conn
	s.connMu.Unlock()
	if conn == nil {
		return fmt.Errorf("websocket not connected")
	}
	frame, err := json.Marshal(map[string]any{"op": "PONG", "timestamp": timestamp})
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, frame)
}

// wsEnvelope mirrors the documented push envelope. Data stays raw because
// INDEX pushes an array while control frames carry scalars.
type wsEnvelope struct {
	Op        string          `json:"op"`
	Topic     string          `json:"topic"`
	Symbol    string          `json:"symbol"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// wsIndexEntry is one element of an INDEX push's data array.
type wsIndexEntry struct {
	Symbol          string          `json:"symbol"`
	IndexPrice      decimal.Decimal `json:"indexPrice"`
	MarkPrice       decimal.Decimal `json:"markPrice"`
	NextFundingRate decimal.Decimal `json:"nextFundingRate"`
	NextFundingTime int64           `json:"nextFundingTime"`
	UpdateTime      int64           `json:"updateTime"`
}

func (s *PublicStream) readPump(ctx context.Context) error {
	s.connMu.Lock()
	conn := s.conn
	s.connMu.Unlock()
	if conn == nil {
		return fmt.Errorf("websocket not connected")
	}
	_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		s.stateMu.Lock()
		s.lastFrameAt = time.Now()
		s.stateMu.Unlock()
		_ = conn.SetReadDeadline(time.Now().Add(wsReadDeadline))
		s.handleFrame(raw)
	}
}

func (s *PublicStream) handleFrame(raw []byte) {
	var env wsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		s.logger.Debug("ws lane undecodable frame", "component", "pionex_ws", "payload", truncateForLog(string(raw)))
		return
	}
	switch strings.ToUpper(env.Op) {
	case "PING":
		// Docs: server pings every 15s, client must echo the timestamp.
		// Handle the lowercase variant too — the live API deviates from
		// the docs regularly, and a missed PONG family disconnects us.
		_ = s.replyPong(env.Timestamp)
	case "PONG", "SUBSCRIBE", "UNSUBSCRIBE", "ERROR":
		s.logger.Debug("ws lane control frame", "component", "pionex_ws", "op", env.Op)
	default:
		s.logger.Debug("ws lane unhandled op", "component", "pionex_ws", "op", env.Op)
	}
	if strings.EqualFold(env.Topic, "INDEX") && strings.ToUpper(env.Op) != "PING" {
		s.ingestIndex(env)
	}
}

func (s *PublicStream) ingestIndex(env wsEnvelope) {
	var entries []wsIndexEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		var single wsIndexEntry
		if err2 := json.Unmarshal(env.Data, &single); err2 != nil || single.Symbol == "" {
			s.logger.Debug("ws lane INDEX payload undecodable", "component", "pionex_ws",
				"payload", truncateForLog(string(env.Data)))
			return
		}
		entries = []wsIndexEntry{single}
	}
	for _, entry := range entries {
		sym := normalizeStreamSymbol(entry.Symbol)
		if sym == "" {
			sym = normalizeStreamSymbol(env.Symbol)
		}
		if sym == "" {
			continue
		}
		mark := entry.MarkPrice
		if !mark.IsPositive() {
			mark = entry.IndexPrice
		}
		if !mark.IsPositive() {
			continue
		}
		received := time.Now()
		if entry.UpdateTime > 0 {
			if server := time.UnixMilli(entry.UpdateTime); server.After(received.Add(-time.Minute)) && server.Before(received.Add(time.Minute)) {
				received = server
			}
		}
		update := MarkUpdate{
			Symbol:          sym,
			MarkPrice:       mark,
			IndexPrice:      entry.IndexPrice,
			NextFundingRate: entry.NextFundingRate,
			NextFundingTime: entry.NextFundingTime,
			ReceivedAt:      received,
		}
		s.stateMu.Lock()
		s.marks[sym] = update
		_, logged := s.firstPayloadLogged[sym]
		if !logged {
			s.firstPayloadLogged[sym] = struct{}{}
		}
		s.stateMu.Unlock()
		if !logged {
			// Observability snippet: first live payload per symbol until the
			// field shapes are trusted against the docs.
			s.logger.Info("ws lane first INDEX payload", "component", "pionex_ws",
				"symbol", sym, "mark_price", update.MarkPrice.String(),
				"index_price", update.IndexPrice.String(),
				"next_funding_rate", update.NextFundingRate.String(),
				"next_funding_time", update.NextFundingTime,
				"raw", truncateForLog(string(env.Data)))
		}
	}
	s.resetBackoff()
}

func normalizeStreamSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}

// InjectForTest seeds the mark store from unit tests without a socket.
func (s *PublicStream) InjectForTest(update MarkUpdate) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.marks[normalizeStreamSymbol(update.Symbol)] = update
	s.firstPayloadLogged[update.Symbol] = struct{}{}
}
