package autogrid

import "testing"

func TestAdoptDirectionAndLeverage(t *testing.T) {
	if got := adoptDirection("long"); got != "LONG" {
		t.Fatalf("long: %s", got)
	}
	if got := adoptDirection(" SHORT "); got != "SHORT" {
		t.Fatalf("short: %s", got)
	}
	if got := adoptDirection("no_trend"); got != "NEUTRAL" {
		t.Fatalf("no_trend: %s", got)
	}
	if got := adoptDirection(""); got != "NEUTRAL" {
		t.Fatalf("empty: %s", got)
	}
	if got := adoptLeverage(0); got != 1 {
		t.Fatalf("zero leverage must default to 1: %d", got)
	}
	if got := adoptLeverage(4); got != 4 {
		t.Fatalf("leverage passthrough broken: %d", got)
	}
}
