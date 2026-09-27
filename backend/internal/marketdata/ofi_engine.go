package marketdata

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// MicrostructureRegime classifies the order book and order flow state.
type MicrostructureRegime string

const (
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
	AskDepleted     float64
	AskReplenished  float64
	BidDepleted     float64
	BidReplenished  float64
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
	Reason                 string               `json:"reason"`
	UpdatedAt              time.Time            `json:"updatedAt"`
}

// CanEnter checks whether a bot entry (LONG, SHORT, or NEUTRAL) is allowed
// given the current microstructure regime.
func (a MicrostructureAnalysis) CanEnter(trend string) (allowed bool, reason string) {
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

// CalculateLevel1OFI calculates the discrete event Order Flow Imbalance (Cont, Kukanov & Stoikov 2014)
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

// SymbolMicrostate stores the rolling microstructure and OFI history for a single symbol.
type SymbolMicrostate struct {
	symbol          string
	config          OFIEngineConfig
	mu              sync.RWMutex
	prevBids        []pionex.DepthLevel
	prevAsks        []pionex.DepthLevel
	currentWindow   OFIWindow
	history         []OFIWindow
	latestAnalysis  MicrostructureAnalysis
}

func newSymbolMicrostate(symbol string, cfg OFIEngineConfig) *SymbolMicrostate {
	return &SymbolMicrostate{
		symbol:  symbol,
		config:  cfg,
		history: make([]OFIWindow, 0, cfg.MaxHistoryWindows),
		latestAnalysis: MicrostructureAnalysis{
			Symbol: symbol,
			Regime: RegimeNeutral,
		},
	}
}

// OFIEngine manages symbol microstates and processes real-time order book and trade events.
type OFIEngine struct {
	config  OFIEngineConfig
	mu      sync.RWMutex
	symbols map[string]*SymbolMicrostate
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

	return &OFIEngine{
		config:  cfg,
		symbols: make(map[string]*SymbolMicrostate),
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

// IngestL2 processes an L2 order book update.
func (e *OFIEngine) IngestL2(symbol string, bids, asks []pionex.DepthLevel, ts time.Time) {
	st := e.getOrCreate(symbol)
	st.mu.Lock()
	defer st.mu.Unlock()

	if ts.IsZero() {
		ts = time.Now()
	}

	if st.currentWindow.StartTime.IsZero() {
		st.currentWindow.StartTime = ts
	} else if ts.Sub(st.currentWindow.StartTime) >= st.config.WindowDuration {
		st.finalizeWindowLocked(ts)
		st.currentWindow.StartTime = ts
	}

	if len(bids) == 0 || len(asks) == 0 {
		return
	}

	bestBid, bestAsk := bids[0], asks[0]
	mid := bestBid.Price.Add(bestAsk.Price).Div(decimal.NewFromInt(2))
	micro := CalculateMicroPrice(bids, asks)

	if st.currentWindow.FirstMidPrice.IsZero() {
		st.currentWindow.FirstMidPrice = mid
	}
	st.currentWindow.LastMidPrice = mid
	st.currentWindow.LastMicroPrice = micro
	st.currentWindow.L2Count++

	// Calculate L1 OFI
	if len(st.prevBids) > 0 && len(st.prevAsks) > 0 {
		ofiStep := CalculateLevel1OFI(st.prevBids, st.prevAsks, bids, asks)
		st.currentWindow.OFI += ofiStep
	}

	// Calculate 2% Depth and Spread
	midF, _ := mid.Float64()
	if midF > 0 {
		bidLow := midF * 0.98
		askHigh := midF * 1.02

		var bidVol, askVol float64
		for _, b := range bids {
			pF, _ := b.Price.Float64()
			aF, _ := b.Amount.Float64()
			if pF >= bidLow && pF <= midF {
				bidVol += pF * aF
			}
		}
		for _, a := range asks {
			pF, _ := a.Price.Float64()
			aF, _ := a.Amount.Float64()
			if pF <= askHigh && pF >= midF {
				askVol += pF * aF
			}
		}

		st.currentWindow.BidVolume2Pct = bidVol
		st.currentWindow.AskVolume2Pct = askVol
		totalVol := bidVol + askVol
		if totalVol > 0 {
			st.currentWindow.QueueImbalance = (bidVol - askVol) / totalVol
		}

		pAskF, _ := bestAsk.Price.Float64()
		pBidF, _ := bestBid.Price.Float64()
		st.currentWindow.LastSpreadBps = ((pAskF - pBidF) / midF) * 10000.0
		st.currentWindow.IsThin = bidVol < st.config.MinDepthNotionalUSDT || askVol < st.config.MinDepthNotionalUSDT
	}

	st.prevBids = bids
	st.prevAsks = asks
}

// IngestTrade processes an aggressive taker trade event.
func (e *OFIEngine) IngestTrade(symbol string, trade pionex.Trade) {
	st := e.getOrCreate(symbol)
	st.mu.Lock()
	defer st.mu.Unlock()

	ts := time.UnixMilli(trade.Time)
	if trade.Time <= 0 {
		ts = time.Now()
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

	// 1. Spoofing Detection: Severe queue imbalance (> 4:1) with zero or contrary taker volume
	hasHugeWall := math.Abs(lastW.QueueImbalance) >= 0.60
	totalTaker := lastW.TakerBuyUSDT + lastW.TakerSellUSDT
	if hasHugeWall && totalTaker < 1000.0 {
		analysis.IsSpoofRisk = true
		analysis.Regime = RegimeSpoofWarning
		analysis.Reason = "unconfirmed book wall with zero taker flow confirmation (spoof risk)"
		st.latestAnalysis = analysis
		return
	}

	// If spread is blown out or book is thin, reject directional signal
	if !analysis.IsSpreadIntact {
		analysis.Regime = RegimeNeutral
		analysis.Reason = fmt.Sprintf("spread degraded: %.1f bps > limit %.1f bps", lastW.LastSpreadBps, st.config.MaxSpreadBps)
		st.latestAnalysis = analysis
		return
	}
	if analysis.IsThinBook {
		analysis.Regime = RegimeNeutral
		analysis.Reason = "thin book liquidity in 2% range"
		st.latestAnalysis = analysis
		return
	}

	// 2. Count consecutive windows meeting Bullish / Bearish persistence
	// Check from newest window backwards
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

		// Bullish window criteria:
		// Positive OFI or bid-heavy queue AND microprice >= midprice AND taker buy ratio >= 60%
		isBull := (w.OFI > 0 || w.QueueImbalance >= 0.15) && isMicroBull && w.TakerBuyRatio >= 0.60
		// Bearish window criteria:
		// Negative OFI or ask-heavy queue AND microprice <= midprice AND taker sell ratio >= 60% (buy ratio <= 40%)
		isBear := (w.OFI < 0 || w.QueueImbalance <= -0.15) && isMicroBear && w.TakerBuyRatio <= 0.40

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

	// 3. Evaluate Regime
	if bullStreak >= st.config.MinWindows {
		// Check Triple Confluence:
		// 1) Persistent OFI (confirmed by bullStreak >= MinWindows)
		// 2) Taker Flow Dominance (Total taker buy >= $15,000 across window span and ratio >= 75%)
		// 3) Price displacement (FirstMidPrice of span vs LastMidPrice >= BreakoutDisplacementPct)
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
	} else {
		analysis.Regime = RegimeNeutral
		analysis.Reason = "order book and taker flow in balanced/neutral regime"
	}

	st.latestAnalysis = analysis
}

// Analyze returns the current microstructure analysis for a symbol.
func (e *OFIEngine) Analyze(symbol string) MicrostructureAnalysis {
	st := e.getOrCreate(symbol)
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.latestAnalysis
}
