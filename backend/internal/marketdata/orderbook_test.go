package marketdata

import (
	"testing"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func TestLocalOrderBook_SnapshotAndDelta(t *testing.T) {
	book := NewLocalOrderBook("BTC_USDT_PERP")
	now := time.Now()

	// 1. Initial snapshot
	initialBids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(50000), Amount: decimal.NewFromFloat(1.5)},
		{Price: decimal.NewFromFloat(49990), Amount: decimal.NewFromFloat(2.0)},
		{Price: decimal.NewFromFloat(49980), Amount: decimal.NewFromFloat(3.0)},
	}
	initialAsks := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(50010), Amount: decimal.NewFromFloat(1.2)},
		{Price: decimal.NewFromFloat(50020), Amount: decimal.NewFromFloat(2.5)},
		{Price: decimal.NewFromFloat(50030), Amount: decimal.NewFromFloat(4.0)},
	}

	book.ApplySnapshot(initialBids, initialAsks, 100, now)

	if !book.IsSynced() {
		t.Fatalf("expected book to be synced after snapshot")
	}
	if book.LastNumber() != 100 {
		t.Fatalf("expected lastNumber=100, got %d", book.LastNumber())
	}

	bestBid, bestAsk, ok := book.BestBidAsk()
	if !ok || !bestBid.Price.Equal(decimal.NewFromFloat(50000)) || !bestAsk.Price.Equal(decimal.NewFromFloat(50010)) {
		t.Fatalf("unexpected best bid/ask: bid=%s, ask=%s", bestBid.Price, bestAsk.Price)
	}

	// 2. Incremental delta:
	// - New best bid at 50005 (size 0.8)
	// - Remove ask at 50010 (size 0)
	// - Update ask at 50020 (size changed from 2.5 to 3.5)
	deltaBids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(50005), Amount: decimal.NewFromFloat(0.8)},
	}
	deltaAsks := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(50010), Amount: decimal.Zero},
		{Price: decimal.NewFromFloat(50020), Amount: decimal.NewFromFloat(3.5)},
	}

	err := book.ApplyDelta(deltaBids, deltaAsks, 101, 100, now.Add(50*time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error applying delta: %v", err)
	}

	bestBid, bestAsk, ok = book.BestBidAsk()
	if !ok {
		t.Fatalf("expected book to remain valid after delta")
	}
	if !bestBid.Price.Equal(decimal.NewFromFloat(50005)) {
		t.Errorf("expected best bid 50005, got %s", bestBid.Price)
	}
	if !bestAsk.Price.Equal(decimal.NewFromFloat(50020)) {
		t.Errorf("expected best ask 50020 (50010 removed), got %s", bestAsk.Price)
	}
	if !bestAsk.Amount.Equal(decimal.NewFromFloat(3.5)) {
		t.Errorf("expected best ask size 3.5, got %s", bestAsk.Amount)
	}

	// 3. Sequence gap detection
	gapBids := []pionex.DepthLevel{
		{Price: decimal.NewFromFloat(50000), Amount: decimal.NewFromFloat(1.0)},
	}
	err = book.ApplyDelta(gapBids, nil, 105, 103, now.Add(100*time.Millisecond))
	if err == nil {
		t.Fatalf("expected error on sequence gap, got nil")
	}
	if book.IsSynced() {
		t.Fatalf("expected book to be marked desynced on sequence gap")
	}
}

func TestLocalOrderBook_CrossedBookDetection(t *testing.T) {
	book := NewLocalOrderBook("ETH_USDT_PERP")
	now := time.Now()

	bids := []pionex.DepthLevel{{Price: decimal.NewFromFloat(2000), Amount: decimal.NewFromFloat(1)}}
	asks := []pionex.DepthLevel{{Price: decimal.NewFromFloat(2010), Amount: decimal.NewFromFloat(1)}}
	book.ApplySnapshot(bids, asks, 1, now)

	// Invert book with a delta bid higher than best ask
	crossedBids := []pionex.DepthLevel{{Price: decimal.NewFromFloat(2020), Amount: decimal.NewFromFloat(1)}}
	err := book.ApplyDelta(crossedBids, nil, 2, 1, now.Add(time.Second))
	if err == nil {
		t.Fatalf("expected crossed book error, got nil")
	}
	if book.IsSynced() {
		t.Fatalf("expected book to be un-synced after crossed book error")
	}
}
