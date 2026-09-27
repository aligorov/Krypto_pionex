package marketdata

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// MicrostructureRegime classifies the order book and order flow state.
type MicrostructureRegime string

const (
	RegimeWarmingUp     MicrostructureRegime = "WARMING_UP"
	RegimeDesync        MicrostructureRegime = "DESYNC"
	RegimeStale         MicrostructureRegime = "STALE"
	RegimeThinBook      MicrostructureRegime = "THIN_BOOK"
	RegimeSpreadBlown   MicrostructureRegime = "SPREAD_BLOWN"
	RegimeNeutral       MicrostructureRegime = "NEUTRAL"
	RegimePumpPressure  MicrostructureRegime = "PUMP_PRESSURE"
	RegimeDumpPressure  MicrostructureRegime = "DUMP_PRESSURE"
	RegimeConfirmedPump MicrostructureRegime = "CONFIRMED_PUMP"
	RegimeConfirmedDump MicrostructureRegime = "CONFIRMED_DUMP"
	RegimeSpoofWarning  MicrostructureRegime = "SPOOF_WARNING"
)

// OFIEngineConfig tunes the dynamic multi-window OFI engine.
type OFIEngineConfig struct {
	WindowDuration          time.Duration // Duration of each rolling micro-window (e.g. 5s - 10s)
	MinWindows              int           // Minimum consecutive windows to establish a regime (e.g. 3)
	MaxSpreadBps            float64       // Maximum permissible spread in basis points (e.g. 30.0 bps)
	MinTakerVolumeUSDT      float64       // Minimum taker volume required for flow confirmation (default $5,000)
	BreakoutDisplacementPct float64       // Price displacement threshold for triple confluence (default 0.15%)
	MinDepthNotionalUSDT    float64       // Minimum notional on bid & ask within 2% to not be thin (default $500)
	MaxHistoryWindows       int           // Retained window history count (default 10)
	MaxStaleness            time.Duration // Maximum age of updates before being declared STALE (default 15s)
}

func DefaultOFIEngineConfig() OFIEngineConfig {
	return OFIEngineConfig{
		WindowDuration:          5 * time.Second,
		MinWindows:              3,
		MaxSpreadBps:            30.0,
		MinTakerVolumeUSDT:      5000.0,
		BreakoutDisplacementPct: 0.15,
		MinDepthNotionalUSDT:    500.0,
		MaxHistoryWindows:       10,
		MaxStaleness:            15 * time.Second,
	}
}

// OFIWindow represents an aggregated time window of order book events and taker trades.
type OFIWindow struct {
	StartTime       time.Time
	EndTime         time.Time
	FirstMidPrice   decimal.Decimal
	LastMidPrice    decimal.Decimal
	LastMicroPrice  decimal.Decimal
	OFI             float64
	TakerBuyUSDT    float64
	TakerSellUSDT   float64
	TakerDeltaUSDT  float64
	TakerBuyRatio   float64
	LastSpreadBps   float64
	BidVolume2Pct   float64
	AskVolume2Pct   float64
	QueueImbalance  float64 // (BidVol - AskVol) / (BidVol + AskVol)
	TradeCount      int
	L2Count         int
	IsThin          bool
}

// MicrostructureAnalysis is the output of the OFI and order flow evaluation.
type MicrostructureAnalysis struct {
	Symbol                 string               `json:"symbol"`
	Regime                 MicrostructureRegime `json:"regime"`
	ConsecutiveBullWindows int                  `json:"consecutiveBullWindows"`
	ConsecutiveBearWindows int                  `json:"consecutiveBearWindows"`
	CurrentOFI             float64              `json:"currentOfi"`
	CurrentMicroPrice      decimal.Decimal      `json:"currentMicroPrice"`
	CurrentMidPrice        decimal.Decimal      `json:"currentMidPrice"`
	MicroPriceBiasBps      float64              `json:"microPriceBiasBps"`
	CurrentSpreadBps       float64              `json:"currentSpreadBps"`
	TakerDeltaUSDT         float64              `json:"takerDeltaUsdt"`
	TakerBuyRatio          float64              `json:"takerBuyRatio"`
	IsSpoofRisk            bool                 `json:"isSpoofRisk"`
	IsThinBook             bool                 `json:"isThinBook"`
	IsSpreadIntact         bool                 `json:"isSpreadIntact"`
	IsFresh                bool                 `json:"isFresh"`
	Reason                 string               `json:"reason"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

// IsActionable returns true only if the signal is fresh, continuous, synchronized, and actionable.
// Stale, desynced, warming-up, thin-book, and spread-blown states are non-actionable fail-safes.
func (a MicrostructureAnalysis) IsActionable() bool {
	if !a.IsFresh {
		return false
	}
	switch a.Regime {
	case RegimePumpPressure, RegimeConfirmedPump, RegimeDumpPressure, RegimeConfirmedDump, RegimeSpoofWarning:
		return true
	default:
		return false
	}
}

// CanEnter checks whether a bot entry (LONG, SHORT, or NEUTRAL) is allowed
// given the current microstructure regime.
func (a MicrostructureAnalysis) CanEnter(trend string) (allowed bool, reason string) {
	if a.Regime == RegimeDesync {
		return false, "order book sequence desynchronized — awaiting fresh snapshot"
	}
	if !a.IsActionable() {
		// Non-directional regimes (WARMING_UP, STALE, NEUTRAL, THIN_BOOK, SPREAD_BLOWN)
		// do not veto entry here (thin book and spread checks are governed by their dedicated risk filters).
		return true, ""
	}

	t := strings.ToUpper(strings.TrimSpace(trend))
	if t == "SHORT" {
		if a.Regime == RegimePumpPressure || a.Regime == RegimeConfirmedPump {
			return false, fmt.Sprintf("pump pressure veto: order flow imbalance and microprice persistently bullish across %d windows (OFI: +$%.0f, taker buy: %.0f%%)",
				a.ConsecutiveBullWindows, a.CurrentOFI, a.TakerBuyRatio*100)
		}
	}
	if t == "LONG" {
		if a.Regime == RegimeDumpPressure || a.Regime == RegimeConfirmedDump {
			return false, fmt.Sprintf("dump pressure veto: order flow imbalance and microprice persistently bearish across %d windows (OFI: -$%.0f, taker sell: %.0f%%) — knife guard",
				a.ConsecutiveBearWindows, math.Abs(a.CurrentOFI), (1.0-a.TakerBuyRatio)*100)
		}
	}
	if t == "NEUTRAL" || t == "NO_TREND" || t == "" {
		if a.Regime == RegimeConfirmedPump {
			return false, fmt.Sprintf("confirmed pump breakout (OFI + taker buy %.0f%% + price displacement) — neutral grid would load one-sided short inventory",
				a.TakerBuyRatio*100)
		}
		if a.Regime == RegimeConfirmedDump {
			return false, fmt.Sprintf("confirmed dump breakout (OFI + taker sell %.0f%% + price displacement) — neutral grid would load one-sided long inventory",
				(1.0-a.TakerBuyRatio)*100)
		}
		if a.Regime == RegimePumpPressure {
			return false, fmt.Sprintf("pump pressure veto: persistent order book and taker flow accumulation across %d windows — neutral grid risks one-sided upper runaway",
				a.ConsecutiveBullWindows)
		}
		if a.Regime == RegimeDumpPressure {
			return false, fmt.Sprintf("dump pressure veto: persistent order book and taker flow selling across %d windows — neutral grid risks falling knife",
				a.ConsecutiveBearWindows)
		}
	}
	return true, ""
}

// CalculateMicroPrice computes the volume-weighted microprice from top of book.
// MicroPrice = (Q_bid * P_ask + Q_ask * P_bid) / (Q_bid + Q_ask)
func CalculateMicroPrice(bids, asks []pionex.DepthLevel) decimal.Decimal {
	if len(bids) == 0 && len(asks) == 0 {
		return decimal.Zero
	}
	if len(bids) == 0 {
		return asks[0].Price
	}
	if len(asks) == 0 {
		return bids[0].Price
	}

	bestBid := bids[0]
	bestAsk := asks[0]

	qBid := bestBid.Amount
	qAsk := bestAsk.Amount
	totalQ := qBid.Add(qAsk)
	if !totalQ.IsPositive() {
		return bestBid.Price.Add(bestAsk.Price).Div(decimal.NewFromInt(2))
	}

	// (qBid * pAsk + qAsk * pBid) / totalQ
	term1 := qBid.Mul(bestAsk.Price)
	term2 := qAsk.Mul(bestBid.Price)
	return term1.Add(term2).Div(totalQ)
}

// CalculateLevel1OFI calculates discrete event Order Flow Imbalance (Cont, Kukanov & Stoikov 2014)
// between two consecutive snapshots of the best bid and best ask.
func CalculateLevel1OFI(prevBids, prevAsks, nextBids, nextAsks []pionex.DepthLevel) float64 {
	if len(prevBids) == 0 || len(prevAsks) == 0 || len(nextBids) == 0 || len(nextAsks) == 0 {
		return 0
	}

	prevB, prevA := prevBids[0], prevAsks[0]
	nextB, nextA := nextBids[0], nextAsks[0]

	pBPrev, _ := prevB.Price.Float64()
	qBPrev, _ := prevB.Amount.Float64()
	pBNext, _ := nextB.Price.Float64()
	qBNext, _ := nextB.Amount.Float64()

	pAPrev, _ := prevA.Price.Float64()
	qAPrev, _ := prevA.Amount.Float64()
	pANext, _ := nextA.Price.Float64()
	qANext, _ := nextA.Amount.Float64()

	var deltaBid float64
	if pBNext > pBPrev {
		deltaBid = qBNext * pBNext
	} else if pBNext == pBPrev {
		deltaBid = (qBNext - qBPrev) * pBNext
	} else {
		deltaBid = -qBPrev * pBPrev
	}

	var deltaAsk float64
	if pANext < pAPrev {
		deltaAsk = qANext * pANext
	} else if pANext == pAPrev {
		deltaAsk = (qANext - qAPrev) * pANext
	} else {
		deltaAsk = -qAPrev * pAPrev
	}

	return deltaBid - deltaAsk
}

// SymbolMicrostate stores the rolling microstructure, local order book, and OFI history for a single symbol.
type SymbolMicrostate struct {
	symbol          string
	config          OFIEngineConfig
	mu              sync.RWMutex
	book            *LocalOrderBook
	lastBookUpdate  time.Time
	lastTradeUpdate time.Time
	prevBestBid     pionex.DepthLevel
	prevBestAsk     pionex.DepthLevel
	hasPrevL1       bool
	currentWindow   OFIWindow
	history         []OFIWindow
	latestAnalysis  MicrostructureAnalysis
	recentTrades    map[string]time.Time // tradeId -> seenAt for deduplication
	resyncInFlight  bool
	lastResyncAt    time.Time
	resyncAttempts  int
}

func newSymbolMicrostate(symbol string, cfg OFIEngineConfig) *SymbolMicrostate {
	return &SymbolMicrostate{
		symbol:       symbol,
		config:       cfg,
		book:         NewLocalOrderBook(symbol),
		history:      make([]OFIWindow, 0, cfg.MaxHistoryWindows),
		recentTrades: make(map[string]time.Time, 1024),
		latestAnalysis: MicrostructureAnalysis{
			Symbol:  symbol,
			Regime:  RegimeWarmingUp,
			Reason:  "initializing market data stream",
			IsFresh: false,
		},
	}
}

// OFIEngine manages symbol microstates and processes real-time order book and trade events.
type OFIEngine struct {
	config   OFIEngineConfig
	mu       sync.RWMutex
	symbols  map[string]*SymbolMicrostate
	onDesync func(symbol string)
}

// NewOFIEngine initializes a new OFIEngine.
func NewOFIEngine(cfg OFIEngineConfig) *OFIEngine {
	if cfg.WindowDuration <= 0 {
		cfg.WindowDuration = 5 * time.Second
	}
	if cfg.MinWindows <= 0 {
		cfg.MinWindows = 3
	}
	if cfg.MaxSpreadBps <= 0 {
		cfg.MaxSpreadBps = 30.0
	}
	if cfg.MinTakerVolumeUSDT <= 0 {
		cfg.MinTakerVolumeUSDT = 5000.0
	}
	if cfg.BreakoutDisplacementPct <= 0 {
		cfg.BreakoutDisplacementPct = 0.15
	}
	if cfg.MinDepthNotionalUSDT <= 0 {
		cfg.MinDepthNotionalUSDT = 500.0
	}
	if cfg.MaxHistoryWindows <= 0 {
		cfg.MaxHistoryWindows = 10
	}
	if cfg.MaxStaleness <= 0 {
		cfg.MaxStaleness = 15 * time.Second
	}

	return &OFIEngine{
		config:  cfg,
		symbols: make(map[string]*SymbolMicrostate),
	}
}

// SetDesyncHandler registers a callback invoked when a symbol's order book loses sequence continuity.
func (e *OFIEngine) SetDesyncHandler(fn func(symbol string)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onDesync = fn
}

// UpdateConfig dynamically updates engine parameters (e.g. from database risk settings).
func (e *OFIEngine) UpdateConfig(cfg OFIEngineConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cfg.MinTakerVolumeUSDT > 0 {
		e.config.MinTakerVolumeUSDT = cfg.MinTakerVolumeUSDT
	}
	if cfg.MaxSpreadBps > 0 {
		e.config.MaxSpreadBps = cfg.MaxSpreadBps
	}
	if cfg.MinDepthNotionalUSDT > 0 {
		e.config.MinDepthNotionalUSDT = cfg.MinDepthNotionalUSDT
	}
	if cfg.MaxStaleness > 0 {
		e.config.MaxStaleness = cfg.MaxStaleness
	}
	for _, st := range e.symbols {
		st.mu.Lock()
		st.config = e.config
		st.mu.Unlock()
	}
}

func (e *OFIEngine) getOrCreate(symbol string) *SymbolMicrostate {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.symbols[sym]
	if !ok {
		st = newSymbolMicrostate(sym, e.config)
		e.symbols[sym] = st
	}
	return st
}

// IngestOrderbookUpdate handles official Pionex WebSocket ORDERBOOK frames.
// It synchronizes the local 100-level L2 book (SNAPSHOT vs UPDATE), enforces sequence continuity,
// updates mid/micro prices, computes L1 OFI, and maintains 2% depth profiles.
func (e *OFIEngine) IngestOrderbookUpdate(update pionex.OrderbookUpdate) {
	st := e.getOrCreate(update.Symbol)
	st.mu.Lock()
	defer st.mu.Unlock()

	ts := update.ReceivedAt
	if ts.IsZero() {
		ts = time.Now()
	}

	if st.currentWindow.StartTime.IsZero() {
		st.currentWindow.StartTime = ts
	} else if ts.Sub(st.currentWindow.StartTime) >= st.config.WindowDuration {
		st.finalizeWindowLocked(ts)
		st.currentWindow.StartTime = ts
	}

	// 1. Synchronize local order book
	if update.IsSnapshot {
		wasDesynced := !st.book.IsSynced()
		st.book.ApplySnapshot(update.Bids, update.Asks, update.Number, ts)
		st.resyncInFlight = false
		st.resyncAttempts = 0
		st.hasPrevL1 = false
		st.prevBestBid = pionex.DepthLevel{}
		st.prevBestAsk = pionex.DepthLevel{}
		if wasDesynced || st.lastBookUpdate.IsZero() || ts.Sub(st.lastBookUpdate) > st.config.WindowDuration {
			// Sequence breach occurred previously or connection gap: purge contaminated history and restart clean warmup!
			st.history = st.history[:0]
			st.currentWindow = OFIWindow{StartTime: ts}
		}
		st.lastBookUpdate = ts
	} else {
		if err := st.book.ApplyDelta(update.Bids, update.Asks, update.Number, update.PrevNumber, ts); err != nil {
			st.latestAnalysis.Regime = RegimeDesync
			st.latestAnalysis.IsFresh = false
			st.latestAnalysis.Reason = fmt.Sprintf("order book desync: %v", err)
			st.latestAnalysis.UpdatedAt = ts
			now := time.Now()
			shouldResync := !st.resyncInFlight || now.Sub(st.lastResyncAt) > 10*time.Second
			if shouldResync {
				st.resyncInFlight = true
				st.lastResyncAt = now
				st.resyncAttempts++
				e.mu.RLock()
				handler := e.onDesync
				e.mu.RUnlock()
				if handler != nil {
					go handler(update.Symbol)
				}
			}
			return
		}
		st.lastBookUpdate = ts
	}

	if !st.book.IsSynced() {
		st.latestAnalysis.Regime = RegimeDesync
		st.latestAnalysis.IsFresh = false
		st.latestAnalysis.Reason = "order book waiting for snapshot"
		st.latestAnalysis.UpdatedAt = ts
		return
	}

	bestBid, bestAsk, ok := st.book.BestBidAsk()
	if !ok {
		return
	}

	mid, micro, _ := st.book.MidAndMicroPrice()

	if st.currentWindow.FirstMidPrice.IsZero() {
		st.currentWindow.FirstMidPrice = mid
	}
	st.currentWindow.LastMidPrice = mid
	st.currentWindow.LastMicroPrice = micro
	st.currentWindow.L2Count++

	if mid.IsPositive() {
		biasBps, _ := micro.Sub(mid).Div(mid).Mul(decimal.NewFromInt(10000)).Float64()
		st.latestAnalysis.CurrentMidPrice = mid
		st.latestAnalysis.CurrentMicroPrice = micro
		st.latestAnalysis.MicroPriceBiasBps = biasBps
		st.latestAnalysis.UpdatedAt = ts
	}

	// 2. Calculate discrete Level 1 OFI against true previous top of book
	if st.hasPrevL1 {
		ofiStep := CalculateLevel1OFI(
			[]pionex.DepthLevel{st.prevBestBid}, []pionex.DepthLevel{st.prevBestAsk},
			[]pionex.DepthLevel{bestBid}, []pionex.DepthLevel{bestAsk},
		)
		st.currentWindow.OFI += ofiStep
	}
	st.prevBestBid = bestBid
	st.prevBestAsk = bestAsk
	st.hasPrevL1 = true

	// 3. Calculate 2% Depth and Spread from full synchronized book
	spreadBps, _ := st.book.SpreadBps()
	bidVol, askVol := st.book.DepthVolume2Pct(mid)

	st.currentWindow.BidVolume2Pct = bidVol
	st.currentWindow.AskVolume2Pct = askVol
	totalVol := bidVol + askVol
	if totalVol > 0 {
		st.currentWindow.QueueImbalance = (bidVol - askVol) / totalVol
	}
	st.currentWindow.LastSpreadBps = spreadBps
	st.currentWindow.IsThin = bidVol < st.config.MinDepthNotionalUSDT || askVol < st.config.MinDepthNotionalUSDT
}

// IngestL2 processes an L2 order book snapshot (e.g. from REST GetDepth).
// Treats incoming bids/asks as a full snapshot ONLY if the symbol is not actively streaming via WebSocket.
// This prevents REST candidate checks from corrupting continuous WebSocket sequence numbers.
func (e *OFIEngine) IngestL2(symbol string, bids, asks []pionex.DepthLevel, ts time.Time) {
	st := e.getOrCreate(symbol)
	st.mu.RLock()
	// REST snapshots must never overwrite an active, synchronized WebSocket book with positive sequence numbers
	isWSStreamActive := st.book.IsSynced() && st.book.LastNumber() > 0 && !st.lastBookUpdate.IsZero() && time.Since(st.lastBookUpdate) < st.config.MaxStaleness
	st.mu.RUnlock()

	if isWSStreamActive {
		return
	}

	e.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     symbol,
		Action:     "SNAPSHOT",
		Bids:       bids,
		Asks:       asks,
		IsSnapshot: true,
		ReceivedAt: ts,
	})
}

// IngestTrade processes an aggressive taker trade event.
// Enforces tradeId deduplication and strict window temporal isolation.
func (e *OFIEngine) IngestTrade(symbol string, trade pionex.Trade) {
	st := e.getOrCreate(symbol)
	st.mu.Lock()
	defer st.mu.Unlock()

	// Trade deduplication
	if trade.TradeID != "" {
		if _, seen := st.recentTrades[trade.TradeID]; seen {
			return
		}
		st.recentTrades[trade.TradeID] = time.Now()
		if len(st.recentTrades) > 5000 {
			cutoff := time.Now().Add(-2 * time.Minute)
			for tid, tSeen := range st.recentTrades {
				if tSeen.Before(cutoff) {
					delete(st.recentTrades, tid)
				}
			}
		}
	}

	ts := time.UnixMilli(trade.Time)
	if trade.Time <= 0 {
		ts = time.Now()
	}

	// 1. Ignore trades older than 2 minutes to prevent backfill/replay pollution
	if time.Since(ts) > 2*time.Minute {
		return
	}

	// 2. Strict window temporal isolation:
	// If the trade timestamp is earlier than the current window start time,
	// do NOT add past volume into the current forward-looking micro-window!
	if !st.currentWindow.StartTime.IsZero() && ts.UnixMilli() < st.currentWindow.StartTime.UnixMilli() {
		return
	}

	if st.currentWindow.StartTime.IsZero() {
		st.currentWindow.StartTime = ts
	} else if ts.Sub(st.currentWindow.StartTime) >= st.config.WindowDuration {
		st.finalizeWindowLocked(ts)
		st.currentWindow.StartTime = ts
	}

	pF, _ := trade.Price.Float64()
	sF, _ := trade.Size.Float64()
	notional := pF * sF
	side := strings.ToUpper(trade.Side)

	if side == "BUY" {
		st.currentWindow.TakerBuyUSDT += notional
		st.currentWindow.TakerDeltaUSDT += notional
	} else if side == "SELL" {
		st.currentWindow.TakerSellUSDT += notional
		st.currentWindow.TakerDeltaUSDT -= notional
	}

	tot := st.currentWindow.TakerBuyUSDT + st.currentWindow.TakerSellUSDT
	if tot > 0 {
		st.currentWindow.TakerBuyRatio = st.currentWindow.TakerBuyUSDT / tot
	}
	st.currentWindow.TradeCount++
	st.lastTradeUpdate = ts
	// Only update latestAnalysis.UpdatedAt if order book is active
	if !st.lastBookUpdate.IsZero() {
		st.latestAnalysis.UpdatedAt = ts
	}
}

// IngestTradeBatch sorts trades chronologically and ingests each trade, rolling micro-windows.
func (e *OFIEngine) IngestTradeBatch(symbol string, trades []pionex.Trade) {
	if len(trades) == 0 {
		return
	}
	sorted := make([]pionex.Trade, len(trades))
	copy(sorted, trades)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Time < sorted[j].Time
	})
	for _, tr := range sorted {
		e.IngestTrade(symbol, tr)
	}
}

// IngestSnapshot ingests an L2 orderbook snapshot and recent trades together,
// chronologically rolling windows and finalizing the current state for immediate analysis.
func (e *OFIEngine) IngestSnapshot(symbol string, bids, asks []pionex.DepthLevel, trades []pionex.Trade, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	e.IngestL2(symbol, bids, asks, now)
	e.IngestTradeBatch(symbol, trades)
	e.FinalizeWindow(symbol, now)
}

// FinalizeWindow closes the current window explicitly (e.g. at timer tick or test step).
func (e *OFIEngine) FinalizeWindow(symbol string, now time.Time) {
	st := e.getOrCreate(symbol)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.finalizeWindowLocked(now)
}

func (st *SymbolMicrostate) finalizeWindowLocked(now time.Time) {
	w := st.currentWindow
	w.EndTime = now
	if w.StartTime.IsZero() {
		w.StartTime = now.Add(-st.config.WindowDuration)
	}

	totTaker := w.TakerBuyUSDT + w.TakerSellUSDT
	if totTaker > 0 {
		w.TakerBuyRatio = w.TakerBuyUSDT / totTaker
	} else {
		w.TakerBuyRatio = 0.5
	}

	st.history = append(st.history, w)
	if len(st.history) > st.config.MaxHistoryWindows {
		st.history = st.history[len(st.history)-st.config.MaxHistoryWindows:]
	}

	// Reset current window
	st.currentWindow = OFIWindow{
		StartTime:      now,
		FirstMidPrice:  w.LastMidPrice,
		LastMidPrice:   w.LastMidPrice,
		LastMicroPrice: w.LastMicroPrice,
		BidVolume2Pct:  w.BidVolume2Pct,
		AskVolume2Pct:  w.AskVolume2Pct,
		QueueImbalance: w.QueueImbalance,
		LastSpreadBps:  w.LastSpreadBps,
		IsThin:         w.IsThin,
	}

	st.evaluateLocked(now)
}

func (st *SymbolMicrostate) evaluateLocked(now time.Time) {
	analysis := MicrostructureAnalysis{
		Symbol:    st.symbol,
		Regime:    RegimeNeutral,
		UpdatedAt: now,
	}

	if len(st.history) == 0 {
		analysis.Regime = RegimeWarmingUp
		analysis.IsFresh = false
		analysis.Reason = "warming up (no completed history windows)"
		st.latestAnalysis = analysis
		return
	}

	lastW := st.history[len(st.history)-1]
	analysis.CurrentOFI = lastW.OFI
	analysis.CurrentMidPrice = lastW.LastMidPrice
	analysis.CurrentMicroPrice = lastW.LastMicroPrice
	analysis.CurrentSpreadBps = lastW.LastSpreadBps
	analysis.TakerDeltaUSDT = lastW.TakerDeltaUSDT
	analysis.TakerBuyRatio = lastW.TakerBuyRatio
	analysis.IsThinBook = lastW.IsThin
	analysis.IsSpreadIntact = lastW.LastSpreadBps <= st.config.MaxSpreadBps && lastW.LastSpreadBps > 0

	midF, _ := lastW.LastMidPrice.Float64()
	microF, _ := lastW.LastMicroPrice.Float64()
	if midF > 0 {
		analysis.MicroPriceBiasBps = ((microF - midF) / midF) * 10000.0
	}

	// 1. Order book synchronization check
	if !st.book.IsSynced() {
		analysis.Regime = RegimeDesync
		analysis.IsFresh = false
		analysis.Reason = "order book sequence desynchronized"
		st.latestAnalysis = analysis
		return
	}

	// 2. Spread degraded check
	if !analysis.IsSpreadIntact {
		analysis.Regime = RegimeSpreadBlown
		analysis.IsFresh = false
		analysis.Reason = fmt.Sprintf("spread degraded: %.1f bps > limit %.1f bps", lastW.LastSpreadBps, st.config.MaxSpreadBps)
		st.latestAnalysis = analysis
		return
	}

	// 3. Thin book liquidity check
	if analysis.IsThinBook {
		analysis.Regime = RegimeThinBook
		analysis.IsFresh = false
		analysis.Reason = "thin book liquidity in 2% range"
		st.latestAnalysis = analysis
		return
	}

	// 4. Warmup check: require at least MinWindows completed windows before declaring directional/spoof regimes
	if len(st.history) < st.config.MinWindows {
		analysis.Regime = RegimeWarmingUp
		analysis.IsFresh = false
		analysis.Reason = fmt.Sprintf("warming up (%d/%d windows)", len(st.history), st.config.MinWindows)
		st.latestAnalysis = analysis
		return
	}

	// 5. Spoofing Detection: Severe queue imbalance (> 4:1) with zero or contrary taker volume
	hasHugeWall := math.Abs(lastW.QueueImbalance) >= 0.60
	totalTaker := lastW.TakerBuyUSDT + lastW.TakerSellUSDT
	if hasHugeWall && totalTaker < 1000.0 {
		analysis.IsSpoofRisk = true
		analysis.Regime = RegimeSpoofWarning
		analysis.IsFresh = true
		analysis.Reason = "unconfirmed book wall with zero taker flow confirmation (spoof risk)"
		st.latestAnalysis = analysis
		return
	}

	// 6. Count consecutive windows meeting Bullish / Bearish persistence
	bullStreak := 0
	bearStreak := 0

	for i := len(st.history) - 1; i >= 0; i-- {
		w := st.history[i]
		if w.LastSpreadBps > st.config.MaxSpreadBps || w.IsThin {
			break
		}

		wMidF, _ := w.LastMidPrice.Float64()
		wMicroF, _ := w.LastMicroPrice.Float64()
		isMicroBull := wMicroF >= wMidF
		isMicroBear := wMicroF <= wMidF

		// Check window taker volume satisfies MinTakerVolumeUSDT
		wTakerTotal := w.TakerBuyUSDT + w.TakerSellUSDT
		hasTakerVolume := wTakerTotal >= st.config.MinTakerVolumeUSDT

		// Bullish window criteria:
		// Positive OFI or bid-heavy queue AND microprice >= midprice AND taker buy ratio >= 60% AND taker volume threshold met
		isBull := (w.OFI > 0 || w.QueueImbalance >= 0.15) && isMicroBull && w.TakerBuyRatio >= 0.60 && hasTakerVolume
		// Bearish window criteria:
		// Negative OFI or ask-heavy queue AND microprice <= midprice AND taker sell ratio >= 60% (buy ratio <= 40%) AND taker volume threshold met
		isBear := (w.OFI < 0 || w.QueueImbalance <= -0.15) && isMicroBear && w.TakerBuyRatio <= 0.40 && hasTakerVolume

		if isBull && bearStreak == 0 {
			bullStreak++
		} else if isBear && bullStreak == 0 {
			bearStreak++
		} else {
			break
		}
	}

	analysis.ConsecutiveBullWindows = bullStreak
	analysis.ConsecutiveBearWindows = bearStreak

	// 7. Evaluate Regime
	if bullStreak >= st.config.MinWindows {
		firstIdx := len(st.history) - bullStreak
		spanFirstMid := st.history[firstIdx].FirstMidPrice
		spanLastMid := st.history[len(st.history)-1].LastMidPrice

		var spanTakerBuy float64
		var spanTakerSell float64
		for k := firstIdx; k < len(st.history); k++ {
			spanTakerBuy += st.history[k].TakerBuyUSDT
			spanTakerSell += st.history[k].TakerSellUSDT
		}
		spanTakerTotal := spanTakerBuy + spanTakerSell
		spanBuyRatio := 0.5
		if spanTakerTotal > 0 {
			spanBuyRatio = spanTakerBuy / spanTakerTotal
		}

		pDisp := 0.0
		spanFirstF, _ := spanFirstMid.Float64()
		spanLastF, _ := spanLastMid.Float64()
		if spanFirstF > 0 {
			pDisp = ((spanLastF - spanFirstF) / spanFirstF) * 100.0
		}

		if spanTakerBuy >= 15000.0 && spanBuyRatio >= 0.75 && pDisp >= st.config.BreakoutDisplacementPct {
			analysis.Regime = RegimeConfirmedPump
			analysis.Reason = fmt.Sprintf("triple confluence confirmed pump: %d windows OFI, $%.0f taker buy (%.0f%%), displacement +%.2f%%",
				bullStreak, spanTakerBuy, spanBuyRatio*100, pDisp)
		} else {
			analysis.Regime = RegimePumpPressure
			analysis.Reason = fmt.Sprintf("pump pressure: %d consecutive bullish windows (OFI +$%.0f, micro bias +%.1f bps)",
				bullStreak, analysis.CurrentOFI, analysis.MicroPriceBiasBps)
		}
		analysis.IsFresh = true
	} else if bearStreak >= st.config.MinWindows {
		firstIdx := len(st.history) - bearStreak
		spanFirstMid := st.history[firstIdx].FirstMidPrice
		spanLastMid := st.history[len(st.history)-1].LastMidPrice

		var spanTakerBuy float64
		var spanTakerSell float64
		for k := firstIdx; k < len(st.history); k++ {
			spanTakerBuy += st.history[k].TakerBuyUSDT
			spanTakerSell += st.history[k].TakerSellUSDT
		}
		spanTakerTotal := spanTakerBuy + spanTakerSell
		spanSellRatio := 0.5
		if spanTakerTotal > 0 {
			spanSellRatio = spanTakerSell / spanTakerTotal
		}

		pDisp := 0.0
		spanFirstF, _ := spanFirstMid.Float64()
		spanLastF, _ := spanLastMid.Float64()
		if spanFirstF > 0 {
			pDisp = ((spanLastF - spanFirstF) / spanFirstF) * 100.0
		}

		if spanTakerSell >= 15000.0 && spanSellRatio >= 0.75 && pDisp <= -st.config.BreakoutDisplacementPct {
			analysis.Regime = RegimeConfirmedDump
			analysis.Reason = fmt.Sprintf("triple confluence confirmed dump: %d windows OFI, $%.0f taker sell (%.0f%%), displacement %.2f%%",
				bearStreak, spanTakerSell, spanSellRatio*100, pDisp)
		} else {
			analysis.Regime = RegimeDumpPressure
			analysis.Reason = fmt.Sprintf("dump pressure: %d consecutive bearish windows (OFI -$%.0f, micro bias %.1f bps) — knife guard",
				bearStreak, math.Abs(analysis.CurrentOFI), analysis.MicroPriceBiasBps)
		}
		analysis.IsFresh = true
	} else {
		analysis.Regime = RegimeNeutral
		analysis.Reason = "order book and taker flow in balanced/neutral regime"
		analysis.IsFresh = true
	}

	st.latestAnalysis = analysis
}

// Analyze returns the current microstructure analysis for a symbol.
// Enforces strict independent staleness checking:
// If the order book data has not been updated within MaxStaleness (15s),
// the analysis is immediately declared STALE and non-actionable,
// guaranteeing that active taker trades can NEVER mask a stalled order book!
func (e *OFIEngine) Analyze(symbol string) MicrostructureAnalysis {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	e.mu.RLock()
	st, ok := e.symbols[sym]
	e.mu.RUnlock()

	if !ok {
		return MicrostructureAnalysis{
			Symbol:  sym,
			Regime:  RegimeWarmingUp,
			Reason:  "no market data stream",
			IsFresh: false,
		}
	}

	st.mu.RLock()
	defer st.mu.RUnlock()

	analysis := st.latestAnalysis

	// Strict Order Book Freshness Check:
	// Trades must never mask a stale or stuck order book!
	if st.lastBookUpdate.IsZero() || time.Since(st.lastBookUpdate) > st.config.MaxStaleness {
		analysis.Regime = RegimeStale
		analysis.IsFresh = false
		if st.lastBookUpdate.IsZero() {
			analysis.Reason = "order book waiting for initial depth data"
		} else {
			analysis.Reason = fmt.Sprintf("order book data stale: last depth update was %s ago (>%s)",
				time.Since(st.lastBookUpdate).Round(time.Second), st.config.MaxStaleness)
		}
		return analysis
	}

	return analysis
}

// ResetSymbol resets order book sync, tracking flags, and window history for a symbol (e.g. after socket reconnect).
func (e *OFIEngine) ResetSymbol(symbol string) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	e.mu.RLock()
	st, ok := e.symbols[sym]
	e.mu.RUnlock()
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.book.Reset()
	st.history = st.history[:0]
	st.currentWindow = OFIWindow{}
	st.hasPrevL1 = false
	st.prevBestBid = pionex.DepthLevel{}
	st.prevBestAsk = pionex.DepthLevel{}
	st.resyncInFlight = false
	st.resyncAttempts = 0
	st.latestAnalysis = MicrostructureAnalysis{
		Symbol:    sym,
		Regime:    RegimeWarmingUp,
		Reason:    "stream reset (reconnecting)",
		IsFresh:   false,
		UpdatedAt: time.Now(),
	}
}

// ResetAll resets all tracked symbol microstates (e.g. on WebSocket reconnect).
func (e *OFIEngine) ResetAll() {
	e.mu.RLock()
	syms := make([]string, 0, len(e.symbols))
	for sym := range e.symbols {
		syms = append(syms, sym)
	}
	e.mu.RUnlock()
	for _, sym := range syms {
		e.ResetSymbol(sym)
	}
}
