package marketdata

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

var (
	ErrNotSynced   = errors.New("orderbook: not synced, waiting for snapshot")
	ErrSequenceGap = errors.New("orderbook: sequence gap detected")
	ErrCrossedBook = errors.New("orderbook: crossed book (bestBid >= bestAsk)")
)

const MaxOrderBookLevels = 100

// LocalOrderBook maintains a full synchronized L2 order book (up to 100 levels each side)
// adhering strictly to the official Pionex WebSocket incremental protocol.
type LocalOrderBook struct {
	mu         sync.RWMutex
	symbol     string
	bids       []pionex.DepthLevel // sorted descending by price (bids[0] is best bid)
	asks       []pionex.DepthLevel // sorted ascending by price (asks[0] is best ask)
	lastNumber int64
	synced     bool
	lastUpdate time.Time
}

// NewLocalOrderBook initializes an empty local order book for a symbol.
func NewLocalOrderBook(symbol string) *LocalOrderBook {
	return &LocalOrderBook{
		symbol: symbol,
		bids:   make([]pionex.DepthLevel, 0, MaxOrderBookLevels),
		asks:   make([]pionex.DepthLevel, 0, MaxOrderBookLevels),
	}
}

// ApplySnapshot resets the local book with a full 100-level snapshot from Pionex.
func (b *LocalOrderBook) ApplySnapshot(bids, asks []pionex.DepthLevel, number int64, ts time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ts.IsZero() {
		ts = time.Now()
	}

	cleanBids := make([]pionex.DepthLevel, 0, len(bids))
	for _, l := range bids {
		if l.Amount.IsPositive() && l.Price.IsPositive() {
			cleanBids = append(cleanBids, l)
		}
	}
	sort.Slice(cleanBids, func(i, j int) bool {
		return cleanBids[i].Price.GreaterThan(cleanBids[j].Price)
	})
	if len(cleanBids) > MaxOrderBookLevels {
		cleanBids = cleanBids[:MaxOrderBookLevels]
	}

	cleanAsks := make([]pionex.DepthLevel, 0, len(asks))
	for _, l := range asks {
		if l.Amount.IsPositive() && l.Price.IsPositive() {
			cleanAsks = append(cleanAsks, l)
		}
	}
	sort.Slice(cleanAsks, func(i, j int) bool {
		return cleanAsks[i].Price.LessThan(cleanAsks[j].Price)
	})
	if len(cleanAsks) > MaxOrderBookLevels {
		cleanAsks = cleanAsks[:MaxOrderBookLevels]
	}

	b.bids = cleanBids
	b.asks = cleanAsks
	b.lastNumber = number
	b.synced = true
	b.lastUpdate = ts
}

// ApplyDelta applies incremental changes (action: "UPDATE") according to Pionex rules:
// - prevNumber must match lastNumber for sequence continuity.
// - size == 0 removes the price level.
// - size > 0 updates or inserts the price level.
func (b *LocalOrderBook) ApplyDelta(bids, asks []pionex.DepthLevel, number, prevNumber int64, ts time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.synced {
		return ErrNotSynced
	}

	// Pionex sequence validation:
	// prevNumber of current update must match lastNumber of previous update.
	if prevNumber != 0 && b.lastNumber != 0 && prevNumber != b.lastNumber {
		b.synced = false
		return fmt.Errorf("%w: expected prevNumber %d, got %d (lastNumber %d)",
			ErrSequenceGap, b.lastNumber, prevNumber, b.lastNumber)
	}

	if ts.IsZero() {
		ts = time.Now()
	}

	for _, l := range bids {
		b.updateBidLocked(l.Price, l.Amount)
	}
	for _, l := range asks {
		b.updateAskLocked(l.Price, l.Amount)
	}

	b.lastNumber = number
	b.lastUpdate = ts

	// Validate crossed book
	if len(b.bids) > 0 && len(b.asks) > 0 {
		if b.bids[0].Price.GreaterThanOrEqual(b.asks[0].Price) {
			b.synced = false
			return fmt.Errorf("%w: bestBid %s >= bestAsk %s",
				ErrCrossedBook, b.bids[0].Price.String(), b.asks[0].Price.String())
		}
	}

	return nil
}

func (b *LocalOrderBook) updateBidLocked(price, amount decimal.Decimal) {
	if !price.IsPositive() {
		return
	}
	// Bids are sorted descending
	idx := sort.Search(len(b.bids), func(i int) bool {
		return b.bids[i].Price.LessThanOrEqual(price)
	})
	if idx < len(b.bids) && b.bids[idx].Price.Equal(price) {
		if !amount.IsPositive() {
			// Level removed
			b.bids = append(b.bids[:idx], b.bids[idx+1:]...)
		} else {
			// Level updated
			b.bids[idx].Amount = amount
		}
		return
	}
	// Level not in book
	if amount.IsPositive() {
		b.bids = append(b.bids, pionex.DepthLevel{})
		copy(b.bids[idx+1:], b.bids[idx:])
		b.bids[idx] = pionex.DepthLevel{Price: price, Amount: amount}
		if len(b.bids) > MaxOrderBookLevels {
			b.bids = b.bids[:MaxOrderBookLevels]
		}
	}
}

func (b *LocalOrderBook) updateAskLocked(price, amount decimal.Decimal) {
	if !price.IsPositive() {
		return
	}
	// Asks are sorted ascending
	idx := sort.Search(len(b.asks), func(i int) bool {
		return b.asks[i].Price.GreaterThanOrEqual(price)
	})
	if idx < len(b.asks) && b.asks[idx].Price.Equal(price) {
		if !amount.IsPositive() {
			// Level removed
			b.asks = append(b.asks[:idx], b.asks[idx+1:]...)
		} else {
			// Level updated
			b.asks[idx].Amount = amount
		}
		return
	}
	// Level not in book
	if amount.IsPositive() {
		b.asks = append(b.asks, pionex.DepthLevel{})
		copy(b.asks[idx+1:], b.asks[idx:])
		b.asks[idx] = pionex.DepthLevel{Price: price, Amount: amount}
		if len(b.asks) > MaxOrderBookLevels {
			b.asks = b.asks[:MaxOrderBookLevels]
		}
	}
}

// SetDesync flags the local book as out-of-sync, requiring a subsequent full snapshot.
func (b *LocalOrderBook) SetDesync() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.synced = false
}

// Reset clears all bids and asks and marks book as unsynced.
func (b *LocalOrderBook) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bids = b.bids[:0]
	b.asks = b.asks[:0]
	b.synced = false
	b.lastNumber = 0
}

// IsSynced returns whether the local book is currently continuous and valid.
func (b *LocalOrderBook) IsSynced() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.synced && len(b.bids) > 0 && len(b.asks) > 0
}

// LastUpdate returns the timestamp of the last applied snapshot or delta.
func (b *LocalOrderBook) LastUpdate() time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastUpdate
}

// LastNumber returns the last sequence number applied.
func (b *LocalOrderBook) LastNumber() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastNumber
}

// BestBidAsk returns the top-of-book best bid and best ask.
func (b *LocalOrderBook) BestBidAsk() (bestBid, bestAsk pionex.DepthLevel, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.bids) == 0 || len(b.asks) == 0 || !b.synced {
		return pionex.DepthLevel{}, pionex.DepthLevel{}, false
	}
	return b.bids[0], b.asks[0], true
}

// MidAndMicroPrice computes the mid price and volume-weighted microprice from the synchronized book.
func (b *LocalOrderBook) MidAndMicroPrice() (mid, micro decimal.Decimal, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.bids) == 0 || len(b.asks) == 0 || !b.synced {
		return decimal.Zero, decimal.Zero, false
	}
	bestBid := b.bids[0]
	bestAsk := b.asks[0]
	mid = bestBid.Price.Add(bestAsk.Price).Div(decimal.NewFromInt(2))
	micro = CalculateMicroPrice(b.bids, b.asks)
	return mid, micro, true
}

// SpreadBps returns the spread in basis points: ((bestAsk - bestBid) / mid) * 10,000.
func (b *LocalOrderBook) SpreadBps() (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.bids) == 0 || len(b.asks) == 0 || !b.synced {
		return 0, false
	}
	bestBid := b.bids[0]
	bestAsk := b.asks[0]
	mid := bestBid.Price.Add(bestAsk.Price).Div(decimal.NewFromInt(2))
	if !mid.IsPositive() {
		return 0, false
	}
	bps, _ := bestAsk.Price.Sub(bestBid.Price).Div(mid).Mul(decimal.NewFromInt(10000)).Float64()
	return bps, true
}

// DepthVolume2Pct calculates total notional depth on bid and ask sides within 2% of mid.
func (b *LocalOrderBook) DepthVolume2Pct(mid decimal.Decimal) (bidVol, askVol float64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	midF, _ := mid.Float64()
	if midF <= 0 {
		return 0, 0
	}
	bidLow := midF * 0.98
	askHigh := midF * 1.02

	for _, b := range b.bids {
		pF, _ := b.Price.Float64()
		aF, _ := b.Amount.Float64()
		if pF >= bidLow && pF <= midF {
			bidVol += pF * aF
		}
	}
	for _, a := range b.asks {
		pF, _ := a.Price.Float64()
		aF, _ := a.Amount.Float64()
		if pF <= askHigh && pF >= midF {
			askVol += pF * aF
		}
	}
	return bidVol, askVol
}

// GetBids returns a copy of top N bids (sorted descending).
func (b *LocalOrderBook) GetBids(limit int) []pionex.DepthLevel {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.bids) {
		limit = len(b.bids)
	}
	out := make([]pionex.DepthLevel, limit)
	copy(out, b.bids[:limit])
	return out
}

// GetAsks returns a copy of top N asks (sorted ascending).
func (b *LocalOrderBook) GetAsks(limit int) []pionex.DepthLevel {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit <= 0 || limit > len(b.asks) {
		limit = len(b.asks)
	}
	out := make([]pionex.DepthLevel, limit)
	copy(out, b.asks[:limit])
	return out
}
