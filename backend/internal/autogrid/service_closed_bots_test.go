package autogrid

import (
	"testing"
	"time"
)

// TestSortClosedBotsMergesSources pins the merged closed-bots timeline: the
// state endpoint concatenates the REAL block and the PAPER block (each
// newest-first), and the UI table must still read as one chronology. Rows
// without a close timestamp sink to the end instead of jumping the queue.
func TestSortClosedBotsMergesSources(t *testing.T) {
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time {
		value := base.Add(time.Duration(minutes) * time.Minute)
		return &value
	}
	items := []ClosedBot{
		{ID: "real-old", Source: "REAL", ClosedAt: at(-60)},
		{ID: "real-new", Source: "REAL", ClosedAt: at(-5)},
		{ID: "paper-old", Source: "PAPER", ClosedAt: at(-30)},
		{ID: "paper-new", Source: "PAPER", ClosedAt: at(-1)},
		{ID: "paper-unknown", Source: "PAPER", ClosedAt: nil},
	}

	sortClosedBots(items)

	wantOrder := []string{"paper-new", "real-new", "paper-old", "real-old", "paper-unknown"}
	for index, want := range wantOrder {
		if items[index].ID != want {
			t.Fatalf("position %d: want %s, got %s (full order: %v)", index, want, items[index].ID, ids(items))
		}
	}
}

func ids(items []ClosedBot) []string {
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, item.ID)
	}
	return result
}
