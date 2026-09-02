package marketdata

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBinanceLiquidationWS is Binance's public all-market forced-order
// stream. No authentication is required; this is a read-only market-data
// reference (Pionex remains the sole trading venue). The topic name is
// !forceOrder@arr — SINGULAR "forceOrder". The v1.3.x constant said
// "forceOrders@arr", which Binance accepts as a connection but never pushes
// to: the table stayed at zero rows for the system's entire history and the
// cascade gate never fired once (found by the 2026-09-01 data-gap audit).
const DefaultBinanceLiquidationWS = "wss://fstream.binance.com/ws/!forceOrder@arr"

// DefaultBybitLiquidationWS is Bybit v5's public linear stream. The
// allLiquidation topic (keyless, 500ms batches) pushes EVERY liquidation
// event exchange-wide — it replaced the Binance leg as the default after
// Binance's WS went data-blocked for both networks this system runs on
// (2026-09-01/02: REST answered, stream pushed zero bytes forever).
const DefaultBybitLiquidationWS = "wss://stream.bybit.com/v5/public/linear"

// bybitToPionexSymbol normalizes Bybit's concatenated linear tickers
// (BTCUSDT) into Pionex's native form (BTC_USDT_PERP) whenever the split is
// derivable. The cascade gate only aggregates USD, but future per-symbol
// consumers should not face mixed naming conventions in one table.
func bybitToPionexSymbol(bybit string) string {
	base := strings.TrimSuffix(bybit, "USDT")
	if base == "" || base == bybit {
		return bybit // non-USDT-linear or unparseable: keep exchange-native
	}
	return base + "_USDT_PERP"
}

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
// The source is read from app_config.liquidation_source ("bybit" default,
// "binance" fallback) — switching is a config flip + restart, no release.
func (l *LiquidationListener) Run(ctx context.Context) {
	go l.flushLoop(ctx)
	source := "bybit"
	if l.db != nil {
		var val string
		if err := l.db.QueryRow(ctx, `
			SELECT COALESCE(value#>>'{}', '') FROM app_config WHERE key = 'liquidation_source'
		`).Scan(&val); err == nil && (val == "bybit" || val == "binance") {
			source = val
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		var err error
		if source == "binance" {
			err = l.stream(ctx)
		} else {
			err = l.bybitStream(ctx)
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("liquidation listener: stream closed, reconnecting",
				"source", source, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// bybitStream subscribes to the allLiquidation topic and feeds the shared
// buffer. Bybit requires an application-level ping every ~20s of silence.
func (l *LiquidationListener) bybitStream(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, DefaultBybitLiquidationWS, nil)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	sub, _ := json.Marshal(map[string]any{"op": "subscribe", "args": []string{"allLiquidation"}})
	if err := conn.WriteControl(websocket.TextMessage, sub, time.Now().Add(5*time.Second)); err != nil {
		return err
	}
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				ping, _ := json.Marshal(map[string]string{"op": "ping"})
				_ = conn.WriteControl(websocket.TextMessage, ping, time.Now().Add(5*time.Second))
			}
		}
	}()

	slog.Info("liquidation listener connected", "url", DefaultBybitLiquidationWS, "source", "bybit")
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		l.ingestBybit(payload)
	}
}

// bybitLiquidationEvent is one element of an allLiquidation push.
type bybitLiquidationEvent struct {
	Symbol string `json:"s"`
	Side   string `json:"S"` // Sell = long liquidated, Buy = short liquidated
	Size   string `json:"v"`
	Price  string `json:"p"`
}

// ingestBybit parses one allLiquidation envelope; data arrives as a single
// object or an array depending on burst shape — both are accepted.
func (l *LiquidationListener) ingestBybit(payload []byte) {
	var envelope struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Topic != "allLiquidation" {
		return
	}
	events := make([]bybitLiquidationEvent, 0, 2)
	if len(envelope.Data) > 0 && envelope.Data[0] == '[' {
		_ = json.Unmarshal(envelope.Data, &events)
	} else {
		var one bybitLiquidationEvent
		if json.Unmarshal(envelope.Data, &one) == nil {
			events = append(events, one)
		}
	}
	l.mu.Lock()
	for _, e := range events {
		if e.Symbol == "" {
			continue
		}
		qty, err1 := strconv.ParseFloat(e.Size, 64)
		px, err2 := strconv.ParseFloat(e.Price, 64)
		if err1 != nil || err2 != nil || qty <= 0 || px <= 0 {
			continue
		}
		side := "long"
		if e.Side == "Buy" {
			side = "short"
		}
		l.buffer = append(l.buffer, liquidationRecord{
			Symbol:   bybitToPionexSymbol(e.Symbol),
			Side:     side,
			ValueUSD: qty * px,
		})
	}
	l.mu.Unlock()
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
