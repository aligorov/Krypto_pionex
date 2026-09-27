package marketdata

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal {
	v, _ := decimal.NewFromString(s)
	return v
}

func TestOFIEngine_MicroPrice(t *testing.T) {
	// Case 1: Balanced book -> microprice == midprice
	bids1 := []pionex.DepthLevel{{Price: d("100.0"), Amount: d("10.0")}}
	asks1 := []pionex.DepthLevel{{Price: d("102.0"), Amount: d("10.0")}}
	mid1 := d("101.0")
	micro1 := CalculateMicroPrice(bids1, asks1)
	if !micro1.Equal(mid1) {
		t.Fatalf("expected microprice %s to equal midprice %s on balanced book", micro1, mid1)
	}

	// Case 2: Heavy bid -> microprice > midprice
	bids2 := []pionex.DepthLevel{{Price: d("100.0"), Amount: d("30.0")}}
	asks2 := []pionex.DepthLevel{{Price: d("102.0"), Amount: d("10.0")}}
	// micro = (30*102 + 10*100) / (30 + 10) = (3060 + 1000) / 40 = 4060 / 40 = 101.5 > 101.0
	micro2 := CalculateMicroPrice(bids2, asks2)
	expectedMicro2 := d("101.5")
	if !micro2.Equal(expectedMicro2) {
		t.Fatalf("expected microprice %s, got %s", expectedMicro2, micro2)
	}
	if !micro2.GreaterThan(mid1) {
		t.Fatalf("microprice %s should be strictly greater than midprice %s", micro2, mid1)
	}

	// Case 3: Heavy ask -> microprice < midprice
	bids3 := []pionex.DepthLevel{{Price: d("100.0"), Amount: d("10.0")}}
	asks3 := []pionex.DepthLevel{{Price: d("102.0"), Amount: d("30.0")}}
	// micro = (10*102 + 30*100) / 40 = (1020 + 3000) / 40 = 4020 / 40 = 100.5 < 101.0
	micro3 := CalculateMicroPrice(bids3, asks3)
	expectedMicro3 := d("100.5")
	if !micro3.Equal(expectedMicro3) {
		t.Fatalf("expected microprice %s, got %s", expectedMicro3, micro3)
	}
	if !micro3.LessThan(mid1) {
		t.Fatalf("microprice %s should be strictly less than midprice %s", micro3, mid1)
	}
}

func TestOFIEngine_Level1OFI(t *testing.T) {
	// Prev state: bid 100 @ 10, ask 102 @ 10
	prevBids := []pionex.DepthLevel{{Price: d("100.0"), Amount: d("10.0")}}
	prevAsks := []pionex.DepthLevel{{Price: d("102.0"), Amount: d("10.0")}}

	// Next state A: bid price jumps to 101 @ 5, ask remains 102 @ 10
	// deltaBid = +5 * 101 = +505 (notional)
	// deltaAsk = 0
	// ofi should be positive
	nextBidsA := []pionex.DepthLevel{{Price: d("101.0"), Amount: d("5.0")}}
	ofiA := CalculateLevel1OFI(prevBids, prevAsks, nextBidsA, prevAsks)
	if ofiA <= 0 {
		t.Fatalf("expected positive OFI when bid price moves up, got %f", ofiA)
	}

	// Next state B: bid price drops to 99 @ 10 (previous 100 bid wiped or cancelled)
	// deltaBid = -10 * 100 = -1000
	nextBidsB := []pionex.DepthLevel{{Price: d("99.0"), Amount: d("10.0")}}
	ofiB := CalculateLevel1OFI(prevBids, prevAsks, nextBidsB, prevAsks)
	if ofiB >= 0 {
		t.Fatalf("expected negative OFI when bid price drops, got %f", ofiB)
	}

	// Next state C: ask price moves up to 103 @ 10 (ask at 102 was eaten or pulled)
	// deltaAsk = -10 * 102 = -1020
	// OFI = deltaBid - deltaAsk = 0 - (-1020) = +1020
	nextAsksC := []pionex.DepthLevel{{Price: d("103.0"), Amount: d("10.0")}}
	ofiC := CalculateLevel1OFI(prevBids, prevAsks, prevBids, nextAsksC)
	if ofiC <= 0 {
		t.Fatalf("expected positive OFI when ask price moves up (supply retreated), got %f", ofiC)
	}
}

func TestOFIEngine_SpoofDetection(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     3,
		MaxSpreadBps:   20.0,
	})

	sym := "TEST_USDT_PERP"
	now := time.Now()

	// A huge bid wall of 1000 units ($100,000) appears, but NO trades happen (0 taker volume)
	// across 3 windows
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i*5) * time.Second)
		bids := []pionex.DepthLevel{{Price: d("100.0"), Amount: d("1000.0")}}
		asks := []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10.0")}}
		engine.IngestL2(sym, bids, asks, ts)
		// No trades ingested!
		engine.FinalizeWindow(sym, ts.Add(5*time.Second))
	}

	analysis := engine.Analyze(sym)
	if !analysis.IsSpoofRisk {
		t.Fatalf("expected spoof risk true due to massive wall with zero taker confirmation")
	}
	if analysis.Regime == RegimePumpPressure || analysis.Regime == RegimeConfirmedPump {
		t.Fatalf("spoofing wall must NOT trigger pump pressure, got regime %s", analysis.Regime)
	}
}

func TestOFIEngine_PumpPressure_Persistence(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration:     5 * time.Second,
		MinWindows:         3,
		MaxSpreadBps:       30.0,
		MinTakerVolumeUSDT: 1000.0,
	})

	sym := "PUMP_USDT_PERP"
	now := time.Now()

	// Window 1: Bullish flow (OFI > 0, micro > mid, taker buys dominate)
	t1 := now
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("10.0"), Amount: d("500.0")}},
		[]pionex.DepthLevel{{Price: d("10.01"), Amount: d("100.0")}},
		t1,
	)
	engine.IngestTrade(sym, pionex.Trade{
		Symbol: sym, Side: "BUY", Price: d("10.01"), Size: d("200.0"), Time: t1.UnixMilli(),
	})
	engine.FinalizeWindow(sym, t1.Add(5*time.Second))

	// After 1 window: should not be pump pressure yet (needs 3 consecutive windows)
	a1 := engine.Analyze(sym)
	if a1.Regime == RegimePumpPressure || a1.Regime == RegimeConfirmedPump || a1.IsActionable() {
		t.Fatalf("expected non-actionable warmup after only 1 window, got regime %s (actionable=%v)", a1.Regime, a1.IsActionable())
	}

	// Window 2: Bullish flow continues
	t2 := now.Add(5 * time.Second)
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("10.01"), Amount: d("600.0")}},
		[]pionex.DepthLevel{{Price: d("10.02"), Amount: d("100.0")}},
		t2,
	)
	engine.IngestTrade(sym, pionex.Trade{
		Symbol: sym, Side: "BUY", Price: d("10.02"), Size: d("200.0"), Time: t2.UnixMilli(),
	})
	engine.FinalizeWindow(sym, t2.Add(5*time.Second))

	// After 2 windows: should still not be actionable pump pressure
	a2 := engine.Analyze(sym)
	if a2.Regime == RegimePumpPressure || a2.Regime == RegimeConfirmedPump || a2.IsActionable() {
		t.Fatalf("expected non-actionable warmup after only 2 windows, got regime %s (actionable=%v)", a2.Regime, a2.IsActionable())
	}

	// Window 3: Bullish flow continues for 3rd window
	t3 := now.Add(10 * time.Second)
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("10.02"), Amount: d("700.0")}},
		[]pionex.DepthLevel{{Price: d("10.03"), Amount: d("100.0")}},
		t3,
	)
	engine.IngestTrade(sym, pionex.Trade{
		Symbol: sym, Side: "BUY", Price: d("10.03"), Size: d("300.0"), Time: t3.UnixMilli(),
	})
	engine.FinalizeWindow(sym, t3.Add(5*time.Second))

	// After 3 windows: now we have sustained pump pressure!
	a3 := engine.Analyze(sym)
	if a3.Regime != RegimePumpPressure && a3.Regime != RegimeConfirmedPump {
		t.Fatalf("expected PumpPressure or ConfirmedPump after 3 consecutive windows, got %s", a3.Regime)
	}

	// Grid bot risk check: SHORT entries must be vetoed!
	allowedShort, reasonShort := a3.CanEnter("SHORT")
	if allowedShort {
		t.Fatalf("expected SHORT entry to be vetoed under pump pressure")
	}
	if reasonShort == "" {
		t.Fatalf("expected clear rejection reason for SHORT under pump pressure")
	}

	// Window 4: Spread explodes (liquidity vacuum / broken spread)
	t4 := now.Add(15 * time.Second)
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("10.00"), Amount: d("50.0")}},
		[]pionex.DepthLevel{{Price: d("10.50"), Amount: d("50.0")}}, // 5% spread!
		t4,
	)
	engine.FinalizeWindow(sym, t4.Add(5*time.Second))

	a4 := engine.Analyze(sym)
	if a4.Regime == RegimePumpPressure || a4.Regime == RegimeConfirmedPump {
		t.Fatalf("blown out spread must immediately suppress pump pressure signal, got %s", a4.Regime)
	}
}

func TestOFIEngine_DumpPressure_Persistence(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     3,
		MaxSpreadBps:   30.0,
	})

	sym := "DUMP_USDT_PERP"
	now := time.Now()

	// Feed 3 consecutive windows of heavy selling and ask pressure
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i*5) * time.Second)
		price := d("100.0").Sub(decimal.NewFromFloat(float64(i) * 0.1))
		engine.IngestL2(sym,
			[]pionex.DepthLevel{{Price: price.Sub(d("0.02")), Amount: d("50.0")}},
			[]pionex.DepthLevel{{Price: price, Amount: d("500.0")}},
			ts,
		)
		engine.IngestTrade(sym, pionex.Trade{
			Symbol: sym, Side: "SELL", Price: price.Sub(d("0.02")), Size: d("300.0"), Time: ts.UnixMilli(),
		})
		engine.FinalizeWindow(sym, ts.Add(5*time.Second))
	}

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDumpPressure && analysis.Regime != RegimeConfirmedDump {
		t.Fatalf("expected DumpPressure after 3 windows of dump, got %s", analysis.Regime)
	}

	// Grid bot risk check: LONG entries must be vetoed to avoid knife-catching!
	allowedLong, reasonLong := analysis.CanEnter("LONG")
	if allowedLong {
		t.Fatalf("expected LONG entry to be vetoed under dump pressure")
	}
	if reasonLong == "" {
		t.Fatalf("expected clear rejection reason for LONG under dump pressure")
	}
}

func TestOFIEngine_TripleConfluence_Breakout(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     3,
		MaxSpreadBps:   30.0,
	})

	sym := "BREAKOUT_USDT_PERP"
	now := time.Now()

	// Ingest 3 windows of strong accumulation where price also clearly displaces upwards
	// Starting at 10.0 and moving to 10.15 (>1.5% displacement) with $50,000 taker buy volume
	basePrice := 10.0
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i*5) * time.Second)
		p := basePrice + float64(i)*0.08
		pDec := decimal.NewFromFloat(p)
		engine.IngestL2(sym,
			[]pionex.DepthLevel{{Price: pDec, Amount: d("2000.0")}},
			[]pionex.DepthLevel{{Price: pDec.Add(d("0.01")), Amount: d("200.0")}},
			ts,
		)
		// $20,000 taker buy each window
		engine.IngestTrade(sym, pionex.Trade{
			Symbol: sym, Side: "BUY", Price: pDec.Add(d("0.01")), Size: d("2000.0"), Time: ts.UnixMilli(),
		})
		engine.FinalizeWindow(sym, ts.Add(5*time.Second))
	}

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeConfirmedPump {
		t.Fatalf("expected RegimeConfirmedPump with triple confluence (OFI + taker + displacement), got %s", analysis.Regime)
	}

	// In confirmed directional breakout, neutral grid is unsafe (inventory blowout)
	allowedNeutral, _ := analysis.CanEnter("NEUTRAL")
	if allowedNeutral {
		t.Fatalf("expected NEUTRAL grid entry to be vetoed under confirmed breakout")
	}
	// Directional LONG is allowed and confirmed!
	allowedLong, _ := analysis.CanEnter("LONG")
	if !allowedLong {
		t.Fatalf("expected LONG entry to be allowed under confirmed pump")
	}
}

func TestOFIEngine_IngestTradeBatch(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "BATCH_DUMP_PERP"
	baseTime := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	// Feed L2 depth initially
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("50.0"), Amount: d("100.0")}},
		[]pionex.DepthLevel{{Price: d("50.05"), Amount: d("1000.0")}},
		baseTime,
	)

	// Simulate 15 trades arriving out of chronological order over 20 seconds,
	// with heavy sell pressure (85% sells).
	trades := make([]pionex.Trade, 0, 15)
	for i := 0; i < 15; i++ {
		secOffset := (i * 2) % 20 // mixed times
		tradeTime := baseTime.Add(time.Duration(secOffset) * time.Second).UnixMilli()
		side := "SELL"
		if i == 3 || i == 7 {
			side = "BUY"
		}
		trades = append(trades, pionex.Trade{
			Symbol:  sym,
			TradeID: fmt.Sprintf("%d", i+1),
			Price:   d("50.0"),
			Size:    d("40.0"), // $2,000 per trade
			Side:    side,
			Time:    tradeTime,
		})
	}

	engine.IngestTradeBatch(sym, trades)
	analysis := engine.Analyze(sym)

	// Taker sell ratio should be heavy sell (buy ratio <= 35%)
	if analysis.TakerBuyRatio > 0.35 {
		t.Fatalf("expected taker buy ratio <= 0.35 (heavy sell), got %.2f", analysis.TakerBuyRatio)
	}
	allowedLong, reason := analysis.CanEnter("LONG")
	if allowedLong && analysis.Regime == RegimeDumpPressure {
		t.Fatalf("expected LONG entry to be vetoed under dump pressure, got allowed with reason: %s", reason)
	}
}

func TestOFIEngine_Staleness(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     3,
		MaxStaleness:   15 * time.Second,
	})

	sym := "STALE_USDT_PERP"
	oldTime := time.Now().Add(-30 * time.Second)

	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		[]pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		oldTime,
	)
	engine.FinalizeWindow(sym, oldTime.Add(5*time.Second))

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeStale {
		t.Fatalf("expected RegimeStale for 30s old data, got %s", analysis.Regime)
	}
	if analysis.IsActionable() {
		t.Fatalf("stale analysis must NOT be actionable")
	}
	if allowed, _ := analysis.CanEnter("LONG"); !allowed {
		t.Fatalf("stale data must fail-safe to allowed (not freeze or veto entries spuriously)")
	}
}

func TestOFIEngine_TradeDeduplication(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "DEDUP_USDT_PERP"
	now := time.Now()

	tr := pionex.Trade{
		Symbol:  sym,
		TradeID: "dup_trade_001",
		Price:   d("100"),
		Size:    d("10"), // $1,000
		Side:    "BUY",
		Time:    now.UnixMilli(),
	}

	// Ingest same trade twice
	engine.IngestTrade(sym, tr)
	engine.IngestTrade(sym, tr)

	st := engine.getOrCreate(sym)
	st.mu.RLock()
	takerBuy := st.currentWindow.TakerBuyUSDT
	tradeCount := st.currentWindow.TradeCount
	st.mu.RUnlock()

	if takerBuy != 1000.0 {
		t.Fatalf("expected taker volume 1000.0 (deduplicated), got %.2f", takerBuy)
	}
	if tradeCount != 1 {
		t.Fatalf("expected tradeCount=1, got %d", tradeCount)
	}
}

func TestOFIEngine_DesyncRecovery(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "SYNC_USDT_PERP"
	now := time.Now()

	// 1. Initial snapshot
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: now,
	})

	// 2. Incremental delta with gap (prevNumber 5 instead of 1)
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "UPDATE",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("15")}},
		Number:     6,
		PrevNumber: 5,
		IsSnapshot: false,
		ReceivedAt: now.Add(50 * time.Millisecond),
	})

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDesync {
		t.Fatalf("expected RegimeDesync on sequence gap, got %s", analysis.Regime)
	}
	if analysis.IsActionable() {
		t.Fatalf("desynced book must not be actionable")
	}

	// 3. New snapshot recovers sync
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("20")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("20")}},
		Number:     10,
		IsSnapshot: true,
		ReceivedAt: now.Add(100 * time.Millisecond),
	})

	st := engine.getOrCreate(sym)
	if !st.book.IsSynced() {
		t.Fatalf("expected book to be synced after fresh snapshot")
	}
}

func TestOFIEngine_StaleOrderbook_TradesDoNotMask(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     1,
		MaxStaleness:   100 * time.Millisecond,
	})

	sym := "STALE_BOOK_PERP"
	past := time.Now().Add(-500 * time.Millisecond)

	// Order book arrives in the past
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		[]pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		past,
	)

	// Trades arrive in real time (now)
	engine.IngestTrade(sym, pionex.Trade{
		Symbol: sym, Side: "BUY", Price: d("100.1"), Size: d("5.0"), Time: time.Now().UnixMilli(),
	})

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeStale {
		t.Fatalf("expected RegimeStale when orderbook is past staleness threshold, got %s", analysis.Regime)
	}
	if analysis.IsFresh {
		t.Fatalf("stale orderbook must have IsFresh = false")
	}
	if analysis.IsActionable() {
		t.Fatalf("stale orderbook must be non-actionable")
	}
}

func TestOFIEngine_MinTakerVolume_Gate(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration:     5 * time.Second,
		MinWindows:         3,
		MaxSpreadBps:       30.0,
		MinTakerVolumeUSDT: 5000.0, // High taker volume threshold
	})

	sym := "LOW_VOL_PERP"
	now := time.Now()

	// Ingest 3 windows with bullish orderbook imbalance, but low taker volume ($500 < $5000)
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i*5) * time.Second)
		engine.IngestL2(sym,
			[]pionex.DepthLevel{{Price: d("10.0"), Amount: d("1000.0")}},
			[]pionex.DepthLevel{{Price: d("10.01"), Amount: d("100.0")}},
			ts,
		)
		// Small taker trade: 50 * 10 = $500 USDT (well below $5000 threshold)
		engine.IngestTrade(sym, pionex.Trade{
			Symbol: sym, Side: "BUY", Price: d("10.01"), Size: d("50.0"), Time: ts.UnixMilli(),
		})
		engine.FinalizeWindow(sym, ts.Add(5*time.Second))
	}

	analysis := engine.Analyze(sym)
	if analysis.Regime == RegimePumpPressure || analysis.Regime == RegimeConfirmedPump {
		t.Fatalf("expected low taker volume to block pump pressure, got %s", analysis.Regime)
	}
	if analysis.ConsecutiveBullWindows != 0 {
		t.Fatalf("expected 0 consecutive bull windows due to volume threshold, got %d", analysis.ConsecutiveBullWindows)
	}
}

func TestOFIEngine_DesyncHandler_AndHistoryPurge(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     2,
	})

	var desyncTriggered sync.WaitGroup
	desyncTriggered.Add(1)
	var reportedSymbol string

	engine.SetDesyncHandler(func(sym string) {
		reportedSymbol = sym
		desyncTriggered.Done()
	})

	sym := "DESYNC_PURGE_PERP"
	now := time.Now()

	// 1. Initial snapshot
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: now,
	})
	engine.FinalizeWindow(sym, now.Add(5*time.Second))

	// Verify history has 1 window
	st := engine.getOrCreate(sym)
	if len(st.history) != 1 {
		t.Fatalf("expected 1 window of history, got %d", len(st.history))
	}

	// 2. Trigger desync gap (prevNumber 5 instead of 1)
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "UPDATE",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("15")}},
		Number:     6,
		PrevNumber: 5,
		IsSnapshot: false,
		ReceivedAt: now.Add(6 * time.Second),
	})

	// Wait for async desync handler
	desyncTriggered.Wait()
	if reportedSymbol != sym {
		t.Fatalf("expected desync handler for %s, got %s", sym, reportedSymbol)
	}

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDesync {
		t.Fatalf("expected RegimeDesync, got %s", analysis.Regime)
	}

	// 3. Fresh snapshot arrives to recover
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("20")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("20")}},
		Number:     10,
		IsSnapshot: true,
		ReceivedAt: now.Add(7 * time.Second),
	})

	// Verify contaminated history was purged!
	st.mu.RLock()
	histLen := len(st.history)
	st.mu.RUnlock()
	if histLen != 0 {
		t.Fatalf("expected history to be purged to 0 upon desync recovery snapshot, got %d", histLen)
	}
}

func TestOFIEngine_TradeTemporalIsolation(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     1,
	})

	sym := "TEMPORAL_PERP"
	now := time.Now()

	// Initialize window starting at now
	engine.IngestL2(sym,
		[]pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		[]pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		now,
	)

	// Attempt to ingest an old trade from 10 seconds ago
	oldTradeTime := now.Add(-10 * time.Second)
	engine.IngestTrade(sym, pionex.Trade{
		Symbol: sym, Side: "BUY", Price: d("100.1"), Size: d("50.0"), Time: oldTradeTime.UnixMilli(),
	})

	st := engine.getOrCreate(sym)
	st.mu.RLock()
	defer st.mu.RUnlock()

	if st.currentWindow.TradeCount != 0 {
		t.Fatalf("expected 0 trades in window after ingesting out-of-window trade, got %d", st.currentWindow.TradeCount)
	}
	if st.currentWindow.TakerBuyUSDT != 0 {
		t.Fatalf("expected 0 volume in window after ingesting out-of-window trade, got %f", st.currentWindow.TakerBuyUSDT)
	}
}

func TestOFIEngine_SingleInFlightResync(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "THROTTLE_RESYNC_PERP"

	var desyncCalls int
	var mu sync.Mutex
	engine.SetDesyncHandler(func(s string) {
		mu.Lock()
		desyncCalls++
		mu.Unlock()
	})

	now := time.Now()
	// Initial snapshot
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: now,
	})

	// Send 5 broken deltas (gap in sequence) in rapid succession
	for i := 0; i < 5; i++ {
		engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
			Symbol:     sym,
			Action:     "UPDATE",
			Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("12")}},
			Number:     10 + int64(i),
			PrevNumber: 9 + int64(i), // expected PrevNumber was 1
			IsSnapshot: false,
			ReceivedAt: now.Add(time.Duration(i*10) * time.Millisecond),
		})
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	calls := desyncCalls
	mu.Unlock()

	if calls != 1 {
		t.Fatalf("expected exactly 1 resync trigger due to 10s in-flight cooldown, got %d", calls)
	}

	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDesync {
		t.Fatalf("expected RegimeDesync after broken delta, got %s", analysis.Regime)
	}
	if allowed, _ := analysis.CanEnter("LONG"); allowed {
		t.Fatalf("expected CanEnter to reject entry during RegimeDesync")
	}

	// Now snapshot arrives to heal the desync
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("15")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("15")}},
		Number:     20,
		IsSnapshot: true,
		ReceivedAt: now.Add(200 * time.Millisecond),
	})

	st := engine.getOrCreate(sym)
	st.mu.RLock()
	inFlight := st.resyncInFlight
	st.mu.RUnlock()
	if inFlight {
		t.Fatalf("snapshot should clear resyncInFlight")
	}
}

func TestOFIEngine_SnapshotL1BaselineReset(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "L1_RESET_PERP"
	now := time.Now()

	// 1. Initial snapshot at price 100
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: now,
	})

	// 2. Normal delta moving bid to 100.05
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "UPDATE",
		Bids:       []pionex.DepthLevel{{Price: d("100.05"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     2,
		PrevNumber: 1,
		IsSnapshot: false,
		ReceivedAt: now.Add(time.Second),
	})

	st := engine.getOrCreate(sym)
	st.mu.RLock()
	ofi1 := st.currentWindow.OFI
	st.mu.RUnlock()
	if ofi1 <= 0 {
		t.Fatalf("expected positive OFI after bid price increase, got %f", ofi1)
	}

	// 3. New snapshot arrives at completely different price (e.g. after reconnect or resync)
	// Price jumps to 150. This snapshot MUST NOT compute a discrete OFI step against 100.05!
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("150"), Amount: d("50")}},
		Asks:       []pionex.DepthLevel{{Price: d("150.1"), Amount: d("50")}},
		Number:     100,
		IsSnapshot: true,
		ReceivedAt: now.Add(2 * time.Second),
	})

	st.mu.RLock()
	ofi2 := st.currentWindow.OFI
	st.mu.RUnlock()

	// OFI in window must be identical to ofi1 (i.e. zero added OFI from snapshot jump), NOT blown up by price delta 150-100!
	if ofi2 != ofi1 {
		t.Fatalf("snapshot should not add discrete OFI step: expected ofi2 == ofi1 (%f), got %f", ofi1, ofi2)
	}
}

func TestOFIEngine_ResetAll(t *testing.T) {
	engine := NewOFIEngine(DefaultOFIEngineConfig())
	sym := "RESET_TEST_PERP"

	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("10"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("10.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: time.Now(),
	})

	st := engine.getOrCreate(sym)
	if !st.book.IsSynced() {
		t.Fatalf("expected book to be synced")
	}

	engine.ResetAll()

	if st.book.IsSynced() {
		t.Fatalf("ResetAll should desynchronize order book until fresh snapshot")
	}
	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDesync && analysis.Regime != RegimeWarmingUp && analysis.Regime != RegimeStale {
		t.Fatalf("expected Desync, WarmingUp or Stale after ResetAll, got %s", analysis.Regime)
	}
}

func TestOFIEngine_DesyncPersistsAcrossStalenessAndWarmup(t *testing.T) {
	engine := NewOFIEngine(OFIEngineConfig{
		WindowDuration: 5 * time.Second,
		MinWindows:     3,
		MaxStaleness:   15 * time.Second,
	})
	sym := "DESYNC_VETO_PERP"
	now := time.Now()

	// 1. Initial snapshot + normal delta -> synced
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     1,
		IsSnapshot: true,
		ReceivedAt: now.Add(-60 * time.Second),
	})

	// 2. Broken delta (sequence gap: expected 1, got 10)
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "UPDATE",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("12")}},
		Number:     11,
		PrevNumber: 10,
		IsSnapshot: false,
		ReceivedAt: now.Add(-50 * time.Second),
	})

	// Microstate was updated 50s ago (> 15s MaxStaleness).
	// Crucial invariant: DESYNC must NOT be overwritten by RegimeStale!
	analysis := engine.Analyze(sym)
	if analysis.Regime != RegimeDesync {
		t.Fatalf("expected RegimeDesync to persist across staleness, got %s", analysis.Regime)
	}
	if analysis.IsSynced {
		t.Fatalf("expected IsSynced == false during desync")
	}
	if allowed, reason := analysis.CanEnter("LONG"); allowed {
		t.Fatalf("CanEnter must reject during desync even when book is stale")
	} else if !strings.Contains(reason, "desynchronized") {
		t.Fatalf("expected desynchronized in rejection reason, got %s", reason)
	}

	// 3. Heal with fresh snapshot -> enters recoveringFromDesync
	engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
		Symbol:     sym,
		Action:     "SNAPSHOT",
		Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
		Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
		Number:     100,
		IsSnapshot: true,
		ReceivedAt: now.Add(-30 * time.Second),
	})

	// Check immediate recovery state (0/3 windows completed, timestamp 30s ago > 15s staleness)
	// Crucial invariant: recoveringFromDesync must still veto entry!
	analysisRecovering := engine.Analyze(sym)
	if analysisRecovering.Regime != RegimeWarmingUp {
		t.Fatalf("expected RegimeWarmingUp while recovering from desync, got %s", analysisRecovering.Regime)
	}
	if analysisRecovering.IsSynced {
		t.Fatalf("expected IsSynced == false while recovering from desync")
	}
	if allowed, reason := analysisRecovering.CanEnter("LONG"); allowed {
		t.Fatalf("CanEnter must reject while recovering from desync")
	} else if !strings.Contains(reason, "recovering") {
		t.Fatalf("expected recovering in rejection reason, got %s", reason)
	}

	// 4. Complete 3 clean windows with fresh data -> recovery completes
	for i := 0; i < 3; i++ {
		ts := now.Add(time.Duration(i*5) * time.Second)
		engine.IngestOrderbookUpdate(pionex.OrderbookUpdate{
			Symbol:     sym,
			Action:     "UPDATE",
			Bids:       []pionex.DepthLevel{{Price: d("100"), Amount: d("10")}},
			Asks:       []pionex.DepthLevel{{Price: d("100.1"), Amount: d("10")}},
			Number:     101 + int64(i),
			PrevNumber: 100 + int64(i),
			IsSnapshot: false,
			ReceivedAt: ts,
		})
		engine.FinalizeWindow(sym, ts.Add(5*time.Second))
	}

	analysisClean := engine.Analyze(sym)
	if !analysisClean.IsSynced {
		t.Fatalf("expected IsSynced == true after completed warmup windows")
	}
	if allowed, _ := analysisClean.CanEnter("LONG"); !allowed {
		t.Fatalf("CanEnter should allow entry after warmup completes in neutral regime")
	}
}


