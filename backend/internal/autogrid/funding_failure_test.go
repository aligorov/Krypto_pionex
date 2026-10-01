package autogrid

import (
	"errors"
	"fmt"
	"testing"
)

func TestBotFundingFailurePreservesWrappedStaleCause(t *testing.T) {
	code, reason := botFundingFailure(fmt.Errorf("funding check: %w", ErrBotFundingSnapshotStale))
	if code != "FUNDING_STALE" || reason == "" {
		t.Fatalf("wrapped stale snapshot must defer as FUNDING_STALE: %s %q", code, reason)
	}
	code, _ = botFundingFailure(errors.New("read Spot funding snapshot: connection unavailable"))
	if code != "FUNDING_UNAVAILABLE" {
		t.Fatalf("unreadable funding must not be treated as a shortfall: %s", code)
	}
}
