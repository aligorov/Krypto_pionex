package pionex

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RateLimiter manages weighted API request limits.
type RateLimiter struct {
	mu            sync.Mutex
	capacity      int
	tokens        int
	refillRate    int // tokens per second
	lastRefill    time.Time
	cooldownUntil time.Time
}

// NewRateLimiter creates a RateLimiter with default headroom (9 weight/sec, max capacity 10).
func NewRateLimiter(capacity int, refillRate int) *RateLimiter {
	if capacity <= 0 {
		capacity = 9
	}
	if refillRate <= 0 {
		refillRate = 9
	}
	return &RateLimiter{
		capacity:   capacity,
		tokens:     capacity,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// Wait blocks until the required weight tokens are available. Tokens are
// reserved exclusively under the mutex: every awakened waiter re-checks the
// balance, so concurrent callers cannot over-admit beyond the refill rate.
func (rl *RateLimiter) Wait(ctx context.Context, weight int) error {
	if weight > rl.capacity {
		weight = rl.capacity
	}
	for {
		rl.mu.Lock()
		now := time.Now()
		if now.Before(rl.cooldownUntil) {
			until := rl.cooldownUntil
			rl.mu.Unlock()
			return fmt.Errorf("rate limiter in HTTP 429 cooldown until %s", until.Format(time.RFC3339))
		}

		// Refill tokens
		elapsed := now.Sub(rl.lastRefill).Seconds()
		if elapsed > 0 {
			newTokens := int(elapsed * float64(rl.refillRate))
			if newTokens > 0 {
				rl.tokens += newTokens
				if rl.tokens > rl.capacity {
					rl.tokens = rl.capacity
				}
				rl.lastRefill = now
			}
		}

		if rl.tokens >= weight {
			rl.tokens -= weight
			rl.mu.Unlock()
			return nil
		}

		needed := weight - rl.tokens
		waitDuration := time.Duration(float64(needed) / float64(rl.refillRate) * float64(time.Second))
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDuration):
			// Loop: re-reserve under the mutex instead of assuming the
			// tokens are still there.
		}
	}
}

// TriggerCooldown initiates a rate limit backoff (e.g. on HTTP 429).
func (rl *RateLimiter) TriggerCooldown(duration time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cooldownUntil = time.Now().Add(duration)
}
