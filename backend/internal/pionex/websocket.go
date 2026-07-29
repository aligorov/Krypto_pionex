package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultWSURL = "wss://ws.pionex.com/wsUA"
	PingInterval = 20 * time.Second
)

// FuturesWSClient manages WebSocket streaming connection to Pionex Futures Private Stream.
type FuturesWSClient struct {
	wsURL       string
	signer      *Signer
	conn        *websocket.Conn
	mu          sync.Mutex
	isConnected bool
}

// NewFuturesWSClient initializes a Futures WebSocket client using wss://ws.pionex.com/wsUA.
func NewFuturesWSClient(wsURL string, apiKey, apiSecret string) *FuturesWSClient {
	if wsURL == "" {
		wsURL = DefaultWSURL
	}
	return &FuturesWSClient{
		wsURL:  wsURL,
		signer: NewSigner(apiKey, apiSecret),
	}
}

// Connect establishes the WebSocket connection and starts heartbeat.
func (ws *FuturesWSClient) Connect(ctx context.Context) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, ws.wsURL, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	ws.conn = conn
	ws.isConnected = true
	slog.Info("Pionex Futures Private WebSocket connected successfully to wsUA")

	go ws.heartbeatLoop(ctx)
	return nil
}

func (ws *FuturesWSClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.mu.Lock()
			if ws.conn != nil && ws.isConnected {
				pingMsg := map[string]string{"op": "ping"}
				bytes, _ := json.Marshal(pingMsg)
				_ = ws.conn.WriteMessage(websocket.TextMessage, bytes)
			}
			ws.mu.Unlock()
		}
	}
}

// Subscribe Topics ("ORDER", "FILL", "POSITION", "BALANCE")
func (ws *FuturesWSClient) Subscribe(ctx context.Context, topics []string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if !ws.isConnected || ws.conn == nil {
		return fmt.Errorf("websocket is not connected")
	}

	subReq := map[string]interface{}{
		"op":     "subscribe",
		"topics": topics,
	}

	return ws.conn.WriteJSON(subReq)
}
