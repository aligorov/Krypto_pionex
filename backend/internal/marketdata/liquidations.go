package marketdata

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBinanceLiquidationWS is Binance's public all-market forced-order
// stream. No authentication is required; this is a read-only market-data
// reference (Pionex remains the sole trading venue).
const DefaultBinanceLiquidationWS = "wss://fstream.binance.com/ws/!forceOrders@arr"

// liquidationFlushInterval balances two needs: the gate reads the trailing
// HOUR (GetLiquidationSummary), and rows carry the flush timestamp as
// captured_at — flushing every 10 minutes keeps the hourly sum accurate
// while bounding insert volume.
const liquidationFlushInterval = 10 * time.Minute

// LiquidationListener streams public Binance forced liquidations into the
// liquidation_events table, backing GetLiquidationSummary and the autogrid
// liquidation-cascade gate. Until v2.0.3 nothing wrote that table, so the
// gate could never fire and the UI widget always read zero.
type LiquidationListener struct {
	db  *pgxpool.Pool
	url string

	mu     sync.Mutex
	buffer []liquidationRecord
}

type liquidationRecord struct {
	Symbol   string // exchange-native form (BTCUSDT); queries only aggregate USD
	Side     string // "long" (forced SELL) | "short" (forced BUY)
	ValueUSD float64
}

// binanceForceOrderEvent is one element of the !forceOrders@arr array.
type binanceForceOrderEvent struct {
	Event string `json:"e"`
	Order struct {
		Symbol string `json:"s"`
		Side   string `json:"S"` // SELL = long liquidated, BUY = short liquidated
		Qty    string `json:"q"`
		AvgPx  string `json:"ap"`
	} `json:"o"`
}

// NewLiquidationListener creates the listener; empty url falls back to the
// public Binance stream.
func NewLiquidationListener(db *pgxpool.Pool, url string) *LiquidationListener {
	if url == "" {
		url = DefaultBinanceLiquidationWS
	}
	return &LiquidationListener{db: db, url: url}
}

// Run connects, streams and reconnects until ctx is cancelled. A dead
// stream never crashes the process: errors are logged and backed off.
func (l *LiquidationListener) Run(ctx context.Context) {
	go l.flushLoop(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		if err := l.stream(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("liquidation listener: stream closed, reconnecting", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (l *LiquidationListener) stream(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, l.url, nil)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	slog.Info("liquidation listener connected", "url", l.url)
	// Binance pings every ~3 minutes; the read deadline covers a full cycle.
	conn.SetReadDeadline(time.Now().Add(4 * time.Minute))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(4 * time.Minute))
		l.ingest(payload)
	}
}

// ingest parses one !forceOrders@arr message — an array of forceOrder events.
func (l *LiquidationListener) ingest(payload []byte) {
	var events []binanceForceOrderEvent
	if err := json.Unmarshal(payload, &events); err != nil {
		return // keep-alive frames and malformed payloads are skipped silently
	}
	l.mu.Lock()
	for _, e := range events {
		if e.Event != "forceOrder" || e.Order.Symbol == "" {
			continue
		}
		qty, err1 := strconv.ParseFloat(e.Order.Qty, 64)
		avgPx, err2 := strconv.ParseFloat(e.Order.AvgPx, 64)
		if err1 != nil || err2 != nil || qty <= 0 || avgPx <= 0 {
			continue
		}
		side := "long"
		if e.Order.Side == "BUY" {
			side = "short"
		}
		l.buffer = append(l.buffer, liquidationRecord{
			Symbol:   e.Order.Symbol,
			Side:     side,
			ValueUSD: qty * avgPx,
		})
	}
	l.mu.Unlock()
}

// flushLoop persists buffered events every 10 minutes, aggregated per
// (symbol, side) to keep row volume bounded.
func (l *LiquidationListener) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(liquidationFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.flush(ctx)
		}
	}
}

func (l *LiquidationListener) flush(ctx context.Context) {
	l.mu.Lock()
	pending := l.buffer
	l.buffer = nil
	l.mu.Unlock()
	if len(pending) == 0 || l.db == nil {
		return
	}
	aggregated := make(map[liquidationRecord]float64, len(pending))
	for _, r := range pending {
		key := liquidationRecord{Symbol: r.Symbol, Side: r.Side}
		aggregated[key] += r.ValueUSD
	}
	// Transactional flush: a partial batch failure must not leave half the
	// rows inserted — the retry would re-insert them and inflate the cascade
	// sum. Rollback puts the whole batch back into the buffer.
	tx, err := l.db.Begin(ctx)
	if err != nil {
		l.rebuffer(pending)
		return
	}
	batch := &pgx.Batch{}
	for key, usd := range aggregated {
		batch.Queue(
			`INSERT INTO liquidation_events (symbol, side, value_usd) VALUES ($1, $2, $3)`,
			key.Symbol, key.Side, usd,
		)
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		_ = tx.Rollback(ctx)
		l.rebuffer(pending)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		l.rebuffer(pending)
		return
	}
}

// rebuffer puts un-flushed events back ahead of anything ingested meanwhile.
func (l *LiquidationListener) rebuffer(pending []liquidationRecord) {
	l.mu.Lock()
	l.buffer = append(pending, l.buffer...)
	l.mu.Unlock()
}
