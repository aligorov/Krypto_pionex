package autogrid

import "testing"

// Regression test for audit M3: transitional remote states must stay under
// supervision; only final states finalize the bot.
func TestTerminalRemoteGridStatus(t *testing.T) {
	terminal := []string{
		"finished", "FINISHED", " canceled ", "cancelled", "closed",
		"stopped", "stop_by_user", "stopped_by_user", "liquidated",
		"expired", "inactive", "terminated", "completed", "failed",
	}
	for _, status := range terminal {
		if !terminalRemoteGridStatus(status) {
			t.Errorf("expected %q to be terminal", status)
		}
	}
	transitional := []string{
		"running", "stopping", "canceling", "cancelling", "closing",
		"creating", "adjusting", "", "unknown_status_token",
	}
	for _, status := range transitional {
		if terminalRemoteGridStatus(status) {
			t.Errorf("expected %q to stay under supervision (not terminal)", status)
		}
	}
}
