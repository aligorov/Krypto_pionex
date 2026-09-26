package autogrid

import (
	"testing"
	"time"
)

// Storm mode must arm only on a CORRELATED burst: three fleet symbols
// tripping inside the window, each trip refreshing the rolling clock.
func TestStormArmsOnCorrelatedBurst(t *testing.T) {
	w := &Worker{}
	if w.stormActive() {
		t.Fatalf("storm must start disarmed")
	}
	w.noteStormTrigger("DOT_USDT_PERP")
	w.noteStormTrigger("ORDI_USDT_PERP")
	if w.stormActive() {
		t.Fatalf("two symbols must not arm the storm")
	}
	w.noteStormTrigger("ICP_USDT_PERP")
	if !w.stormActive() {
		t.Fatalf("three symbols within the window must arm the storm")
	}

	// The window is rolling: a fresh trip extends it, a quiet stretch ends it.
	w.stormMu.Lock()
	w.stormUntil = time.Now().Add(-time.Second)
	w.stormMu.Unlock()
	if w.stormActive() {
		t.Fatalf("expired storm must disarm")
	}

	// Stale entries do not count toward a new arm.
	w.stormMu.Lock()
	for sym := range w.stormTriggers {
		w.stormTriggers[sym] = time.Now().Add(-2 * stormSymbolWindow)
	}
	w.stormMu.Unlock()
	w.noteStormTrigger("NEW_USDT_PERP")
	if w.stormActive() {
		t.Fatalf("stale trips must not arm the storm")
	}
}

// The maybeLogStormState lock-nest deadlock (agent_3217d33f finding 1):
// an armed storm with a nil DB must return in bounded time, not hang the
// supervision loop. The test fails by timeout if the regression returns.
func TestMaybeLogStormStateNoDeadlock(t *testing.T) {
	w := &Worker{}
	w.stormMu.Lock()
	w.stormUntil = time.Now().Add(stormDuration)
	w.stormTriggers = map[string]time.Time{
		"A_USDT_PERP": time.Now(), "B_USDT_PERP": time.Now(), "C_USDT_PERP": time.Now(),
	}
	w.stormMu.Unlock()
	done := make(chan struct{})
	go func() { w.maybeLogStormState(nil); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("maybeLogStormState deadlocked (Lock→RLock nest regression)")
	}
}
