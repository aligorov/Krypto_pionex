package autogrid

import "testing"

func TestCooldownHoursEscalation(t *testing.T) {
	cases := map[int]int{0: 2, 1: 2, 2: 4, 3: 8, 4: 16, 5: 24, 6: 24, 9: 24}
	for closes, want := range cases {
		if got := cooldownHours(closes); got != want {
			t.Fatalf("cooldownHours(%d) = %d, want %d", closes, got, want)
		}
	}
}
