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

// v2.0.144: the storm sensor is pinned to the RUNNING fleet — pre-warmed
// candidate symbols (which share the WS lane) must NOT arm market-wide
// storms. Legacy behavior (count everything) survives an empty set only.
func TestStormSensorFleetPinned(t *testing.T) {
	w := &Worker{}
	w.setFleetStormSymbols([]string{"FLEET_A_USDT_PERP", "FLEET_B_USDT_PERP"})

	// Two fleet hits + one pre-warm hit: only two DISTINCT fleet symbols —
	// below stormMinSymbols, no storm.
	if w.fleetStormSymbol("PREWARM_X_USDT_PERP") {
		t.Fatal("pre-warm symbol must not count toward the storm")
	}
	w.noteStormTrigger("FLEET_A_USDT_PERP")
	w.noteStormTrigger("FLEET_B_USDT_PERP")
	// noteStormTrigger itself doesn't filter (unit callers); the onRealtimeMark
	// gate does. Simulate the gate: only fleet symbols reach it.
	w.noteStormTrigger("FLEET_A_USDT_PERP") // re-trip within window
	if w.stormActive() {
		t.Fatal("two distinct fleet symbols must not arm the storm (need 3)")
	}
	w.noteStormTrigger("FLEET_B_USDT_PERP")
	// Still 2 distinct. Third fleet symbol:
	w.setFleetStormSymbols([]string{"FLEET_A_USDT_PERP", "FLEET_B_USDT_PERP", "FLEET_C_USDT_PERP"})
	w.noteStormTrigger("FLEET_C_USDT_PERP")
	if !w.stormActive() {
		t.Fatal("three distinct fleet symbols must arm the storm")
	}
}

func TestStormSensorEmptySetCountsEverything(t *testing.T) {
	w := &Worker{}
	w.noteStormTrigger("A_USDT_PERP")
	w.noteStormTrigger("B_USDT_PERP")
	w.noteStormTrigger("C_USDT_PERP")
	if !w.stormActive() {
		t.Fatal("empty fleet set = legacy count-everything behavior (boot window)")
	}
	if !w.fleetStormSymbol("ANYTHING_USDT_PERP") {
		t.Fatal("empty set must pass every symbol")
	}
}
