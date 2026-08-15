package pionex

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimiterNoOverAdmission proves concurrent waiters cannot consume
// more tokens than the bucket actually holds: with capacity 2 and a slow
// refill, at most a small trickle may be admitted inside a short window.
func TestRateLimiterNoOverAdmission(t *testing.T) {
	limiter := NewRateLimiter(2, 2) // capacity 2, refill 2 tokens/sec

	var admitted atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.Wait(context.Background(), 1); err != nil {
				t.Errorf("wait failed: %v", err)
				return
			}
			if time.Since(start) < 100*time.Millisecond {
				admitted.Add(1)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waiters deadlocked")
	}
	// Capacity 2 (+ at most one refill tick inside 100ms at 2/s).
	if admitted.Load() > 3 {
		t.Fatalf("over-admission: %d requests passed within 100ms with capacity 2", admitted.Load())
	}
}

func TestRateLimiterCooldownFailsFast(t *testing.T) {
	limiter := NewRateLimiter(2, 2)
	limiter.TriggerCooldown(50 * time.Millisecond)
	if err := limiter.Wait(context.Background(), 1); err == nil {
		t.Fatal("cooldown must fail fast")
	}
	time.Sleep(60 * time.Millisecond)
	if err := limiter.Wait(context.Background(), 1); err != nil {
		t.Fatalf("wait must succeed after cooldown: %v", err)
	}
}

func TestRateLimiterContextCancel(t *testing.T) {
	limiter := NewRateLimiter(1, 1)
	if err := limiter.Wait(context.Background(), 1); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx, 1); err == nil {
		t.Fatal("cancelled context must abort the wait")
	}
}
