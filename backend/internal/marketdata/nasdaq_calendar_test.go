package marketdata

import (
	"testing"
	"time"
)

func TestClassifyNasdaqEvent(t *testing.T) {
	cases := []struct {
		name       string
		wantImpact string
		wantOK     bool
	}{
		{"Consumer Price Index", "High", true},
		{"Core CPI", "High", true},
		{"Initial Jobless Claims", "Medium", true},
		{"FOMC Meeting", "High", true},
		{"Gross Domestic Product", "High", true},
		{"Random Regional Survey", "", false},
	}

	for _, c := range cases {
		impact, ok := classifyNasdaqEvent(c.name)
		if ok != c.wantOK || impact != c.wantImpact {
			t.Errorf("classifyNasdaqEvent(%q) = (%q, %v); want (%q, %v)", c.name, impact, ok, c.wantImpact, c.wantOK)
		}
	}
}

func TestParseNasdaqTime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("EDT", -4*3600)
	}

	// 08:30 ET on 2026-10-30 (EDT = UTC-4) -> 12:30 UTC
	eventTime, err := parseNasdaqTime("2026-10-30", "08:30", loc)
	if err != nil {
		t.Fatalf("parseNasdaqTime: %v", err)
	}
	if eventTime.Hour() != 12 || eventTime.Minute() != 30 {
		t.Errorf("expected 12:30 UTC for 08:30 EDT, got %s", eventTime.Format(time.RFC3339))
	}
}
