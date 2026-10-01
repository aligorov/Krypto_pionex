package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/telegram"
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

	mu              sync.Mutex
	buffer          []liquidationRecord
	lastHealthWrite time.Time
	// lastEventAt is the ingest time of the most recent liquidation event
	// (v2.0.167 auto-failover clock); failoverEpisode guards one page per
	// silence episode.
	lastEventAt     *time.Time
	failoverEpisode bool
	// ws1008Streak counts consecutive stream errors containing "1008"
	// (rate-limit); three in a row pages the operator (v2.0.171 WS alert).
	ws1008Streak    int
	lastWS1008Alert time.Time
}

// resetEpisode clears the failover latch after a switch (or a fresh event).
func (l *LiquidationListener) resetEpisode() {
	l.failoverEpisode = false
}

func (l *LiquidationListener) lastWS1008Alarm() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastWS1008Alert
}

func (l *LiquidationListener) setLastWS1008Alarm(t time.Time) {
	l.mu.Lock()
	l.lastWS1008Alert = t
	l.mu.Unlock()
}

// pageWS1008Alert writes a WS_RATE_LIMIT telegram event (v2.0.171).
func pageWS1008Alert(ctx context.Context, db *pgxpool.Pool, source string, streak int) {
	if db == nil {
		return
	}
	_ = telegram.NewService(db, nil).EnqueueNotification(ctx, "WS_RATE_LIMIT", map[string]any{
		"message": fmt.Sprintf("⚠️ <b>WebSocket rate-limit</b>: %d подряд отключений 1008 от %s — сокет троттлится, каскад-гейт может закрыть входы. Проверьте подписки.", streak, source),
	})
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
		l.recordTransport(ctx, source, false)
		var err error
		if source == "binance" {
			err = l.stream(ctx)
		} else {
			err = l.bybitStream(ctx)
		}
		l.recordTransport(ctx, source, false)
		if err != nil && ctx.Err() == nil {
			slog.Warn("liquidation listener: stream closed, reconnecting",
				"source", source, "error", err.Error())

			// v2.0.171 WS-1008 alert (consensus-2.0): three consecutive
			// rate-limit disconnects = the socket is being throttled — the
			// operator needs to know before the cascade gate fail-closes.
			errStr := err.Error()
			if strings.Contains(errStr, "1008") || strings.Contains(errStr, "rate limit") {
				l.mu.Lock()
				l.ws1008Streak++
				streak := l.ws1008Streak
				l.mu.Unlock()
				if streak >= 3 && time.Since(l.lastWS1008Alarm()) >= 15*time.Minute {
					l.setLastWS1008Alarm(time.Now())
					slog.Error("liquidation listener: 3+ consecutive rate-limit disconnects",
						"source", source, "streak", streak)
					pageWS1008Alert(ctx, l.db, source, streak)
				}
			} else {
				l.mu.Lock()
				l.ws1008Streak = 0
				l.mu.Unlock()
			}
		}
		// v2.0.167 auto-failover (week-audit P1-2): the entry gate fail-closes
		// LONG/NEUTRAL at 15m of event silence. Instead of freezing the fleet
		// on a dead primary until a human flips app_config, the listener
		// itself switches to the other source after 15m of silence and pages
		// once per episode. The DB config stays untouched — a restart returns
		// to the operator's chosen primary.
		if l.lastEventAt != nil && time.Since(*l.lastEventAt) > 15*time.Minute && ctx.Err() == nil {
			previous := source
			if source == "bybit" {
				source = "binance"
			} else {
				source = "bybit"
			}
			l.resetEpisode()
			slog.Error("liquidation listener: 15m of silence — auto-failover",
				"from", previous, "to", source)
			pageFailover(ctx, l.db, previous, source)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// pageFailover writes the LIQ_FEED_FAILOVER telegram event directly (the
// marketdata package cannot import autogrid; the queue table is the contract).
func pageFailover(ctx context.Context, db *pgxpool.Pool, from, to string) {
	if db == nil {
		return
	}
	_ = telegram.NewService(db, nil).EnqueueNotification(ctx, "LIQ_FEED_FAILOVER", map[string]any{
		"message": "⚠️ <b>Ликвидации: 15м тишины</b> — переключение источника " + from + " → " + to + ". Здоровье нового потока ещё должно подтвердиться heartbeat.",
	})
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

	// The docs topic is PER-SYMBOL: allLiquidation.BTCUSDT — a bare
	// "allLiquidation" arg is rejected with handler-not-found (verified live
	// 2026-09-02). Subscribe across the top liquid linear symbols by 24h
	// turnover; cascades concentrate in the majors, the long tail adds noise
	// without moving the hourly USD sums the gate reads.
	symbols := bybitTopLiquidSymbols(ctx)
	args := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		args = append(args, "allLiquidation."+sym)
	}
	// Bybit ops are JSON DATA frames — gorilla's WriteControl only accepts
	// control frames (Close/Ping/Pong) and rejects Text with "bad write
	// message type". WriteMessage is fine here: the ping goroutine below is
	// the only other writer and it starts after this call returns.
	sub, _ := json.Marshal(map[string]any{"op": "subscribe", "args": args})
	if err := conn.WriteMessage(websocket.TextMessage, sub); err != nil {
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
				// Bybit expects an application-level JSON ping (a data
				// frame), not a WS protocol ping.
				ping, _ := json.Marshal(map[string]string{"op": "ping"})
				_ = conn.WriteMessage(websocket.TextMessage, ping)
			}
		}
	}()

	slog.Info("liquidation listener connected", "url", DefaultBybitLiquidationWS, "source", "bybit")
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	subscribed := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		// A subscription acknowledgement or a topic event proves that the
		// subscribed stream works. Pongs refresh liveness only after that.
		var frame struct {
			Op      string `json:"op"`
			Success bool   `json:"success"`
			Topic   string `json:"topic"`
		}
		if json.Unmarshal(payload, &frame) == nil {
			if frame.Op == "subscribe" && !frame.Success {
				return fmt.Errorf("liquidation subscription rejected")
			}
			if (frame.Op == "subscribe" && frame.Success) || strings.HasPrefix(frame.Topic, "allLiquidation.") {
				subscribed = true
			}
			if subscribed && (frame.Op == "ping" || frame.Op == "pong" || frame.Op == "subscribe" || strings.HasPrefix(frame.Topic, "allLiquidation.")) {
				l.recordTransport(ctx, "bybit", true)
			}
		}
		l.ingestBybit(payload)
	}
}

// bybitTopLiquidSymbols returns the top USDT-linear tickers by 24h turnover
// (public REST, same host the funding/OI collectors already use) with a
// hardcoded major-set fallback when the catalog fetch fails.
var bybitLiquidFallback = []string{
	"BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT", "DOGEUSDT", "BNBUSDT",
	"LTCUSDT", "AVAXUSDT", "LINKUSDT", "SUIUSDT", "ADAUSDT", "PEPEUSDT",
}

func bybitTopLiquidSymbols(ctx context.Context) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.bybit.com/v5/market/tickers?category=linear", nil)
	if err != nil {
		return bybitLiquidFallback
	}
	req.Header.Set("User-Agent", "pionex-autogrid-collector/2.0 (+liquidations)")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return bybitLiquidFallback
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bybitLiquidFallback
	}
	var payload struct {
		Result struct {
			List []struct {
				Symbol      string `json:"symbol"`
				Turnover24h string `json:"turnover24h"`
			} `json:"list"`
		} `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) != nil {
		return bybitLiquidFallback
	}
	type row struct {
		symbol string
		turn   float64
	}
	rows := make([]row, 0, len(payload.Result.List))
	for _, item := range payload.Result.List {
		if !strings.HasSuffix(item.Symbol, "USDT") {
			continue
		}
		turn, _ := strconv.ParseFloat(item.Turnover24h, 64)
		rows = append(rows, row{item.Symbol, turn})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].turn > rows[j].turn })
	if len(rows) > 40 {
		rows = rows[:40]
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.symbol)
	}
	if len(out) == 0 {
		return bybitLiquidFallback
	}
	return out
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
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		!strings.HasPrefix(envelope.Topic, "allLiquidation.") {
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
		// Bybit reports the POSITION side: a Buy update means a LONG was
		// liquidated (the closing order itself is a sell) — the inverse of
		// Binance's order-side encoding.
		side := "short"
		if e.Side == "Buy" {
			side = "long"
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
	// Server pings are received even when there are no liquidations.
	defaultPing := conn.PingHandler()
	conn.SetPingHandler(func(message string) error {
		conn.SetReadDeadline(time.Now().Add(4 * time.Minute))
		l.recordTransport(ctx, "binance", true)
		return defaultPing(message)
	})
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
		l.recordTransport(ctx, "binance", true)
		l.ingest(payload)
	}
}

// recordTransport only records the existing connection; it never opens a
// second market source. Missing/failed writes age out and block fresh risk.
func (l *LiquidationListener) recordTransport(ctx context.Context, source string, connected bool) {
	if l.db == nil {
		return
	}
	// A burst may contain thousands of events. Persist transport evidence at
	// most once per ten seconds; disconnects are always written immediately.
	if connected && time.Since(l.lastHealthWrite) < 10*time.Second {
		return
	}
	if !connected {
		l.lastHealthWrite = time.Time{}
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := l.db.Exec(writeCtx, `
		INSERT INTO liquidation_feed_health (source, connected, last_message_at)
		VALUES ($1, $2, CASE WHEN $2 THEN NOW() ELSE NULL END)
		ON CONFLICT (source) DO UPDATE SET connected = EXCLUDED.connected,
		last_message_at = CASE WHEN EXCLUDED.connected THEN NOW() ELSE liquidation_feed_health.last_message_at END,
		updated_at = NOW()`, source, connected)
	if err != nil && ctx.Err() == nil {
		slog.Warn("liquidation transport health persist failed", "source", source, "error", err)
	}
	if err == nil && connected {
		l.lastHealthWrite = time.Now()
	}
}

// ingest parses one !forceOrders@arr message — an array of forceOrder events.
func (l *LiquidationListener) ingest(payload []byte) {
	now := time.Now().UTC()
	l.mu.Lock()
	l.lastEventAt = &now
	l.mu.Unlock()
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
