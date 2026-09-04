package marketdata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newFREDCalendarTestCollector builds a collector pointed at a mock FRED API
// (rate limiting off, no database — only fetch/classify paths are exercised).
func newFREDCalendarTestCollector(t *testing.T, baseURL string) *Collector {
	t.Helper()
	cfg := DefaultCollectorConfig([]string{"BTC_USDT_PERP"})
	cfg.FREDBaseURL = baseURL
	cfg.HTTPTimeout = 5 * time.Second
	cfg.MinRequestInterval = 0
	return NewCollectorWithConfig(nil, cfg)
}

// TestClassifyFREDRelease pins the release-name → (impact, time-of-day)
// mapping. Names are the ones api.stlouisfed.org/fred/releases actually
// returns. The "Federal Funds Effective Rate" case is the daily-release
// guard: matching it would arm a HIGH gate event every business day.
func TestClassifyFREDRelease(t *testing.T) {
	cases := []struct {
		name       string
		wantImpact string
		wantHour   int
		wantMinute int
	}{
		{"Employment Situation", "High", 13, 30},                                // BLS payrolls (NFP)
		{"Consumer Price Index", "High", 13, 30},                                // CPI
		{"Producer Price Index", "High", 13, 30},                                // PPI
		{"Gross Domestic Product", "High", 13, 30},                              // GDP
		{"Personal Income and Outlays", "High", 13, 30},                         // PCE
		{"Advanced Monthly Sales for Retail and Food Services", "High", 13, 30}, // retail sales
		{"Advance Monthly Sales for Retail and Food Services", "High", 13, 30},  // Advance/Advanced spelling
		{"FOMC Board Meeting Minutes", "High", 18, 0},                           // 14:00 ET statement hour
		{"Federal Open Market Committee Press Conference", "High", 18, 0},
		{"Unemployment Insurance Weekly Claims", "Medium", 13, 30}, // jobless claims
		{"Initial Claims", "Medium", 13, 30},
		{"Manufacturing ISM Report On Business", "Medium", 13, 30},
		{"Nonmanufacturing ISM Report On Business", "Medium", 13, 30},
		{"Surveys of Consumers", "Medium", 13, 30},         // UoM sentiment
		{"New Residential Construction", "Medium", 13, 30}, // housing starts
		{"G.17 Industrial Production and Capacity Utilization", "Medium", 13, 30},
		{"U.S. International Trade in Goods and Services", "Medium", 13, 30}, // trade balance
		{"Manufacturing and Trade Inventories and Sales", "", 0, 0},          // LOW/ignored
		{"Federal Funds Effective Rate", "", 0, 0},                           // DAILY — must never map
		{"Dallas Fed Manufacturing Survey", "", 0, 0},                        // regional noise
		{"NBER-based Recession Indicators", "", 0, 0},
		{"H.6 Money Stock Measures", "", 0, 0},
		{"  ", "", 0, 0},
	}
	for _, tc := range cases {
		got := classifyFREDRelease(tc.name)
		if tc.wantImpact == "" {
			if got.classified {
				t.Errorf("classifyFREDRelease(%q) = %+v, want not classified", tc.name, got)
			}
			continue
		}
		if !got.classified || got.impact != tc.wantImpact ||
			got.hourUTC != tc.wantHour || got.minuteUTC != tc.wantMinute {
			t.Errorf("classifyFREDRelease(%q) = %+v, want impact=%s %02d:%02d UTC",
				tc.name, got, tc.wantImpact, tc.wantHour, tc.wantMinute)
		}
	}
}

// TestBuildFREDCalendarEvents verifies the ±14d window, the time-of-day
// heuristics (13:30 UTC default, 18:00 UTC FOMC) and the skip rules.
func TestBuildFREDCalendarEvents(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	names := map[int]string{
		10:  "Consumer Price Index",
		50:  "Employment Situation",
		336: "Federal Funds Effective Rate", // daily — must be dropped
		78:  "Unemployment Insurance Weekly Claims",
		99:  "FOMC Board Meeting Minutes",
	}
	dates := []fredReleaseDate{
		{ReleaseID: 10, Date: "2026-09-11"},  // in window, HIGH 13:30Z
		{ReleaseID: 99, Date: "2026-09-16"},  // in window, FOMC 18:00Z
		{ReleaseID: 78, Date: "2026-09-10"},  // in window, MEDIUM 13:30Z
		{ReleaseID: 50, Date: "2026-08-20"},  // before window start (Aug 21)
		{ReleaseID: 50, Date: "2026-09-19"},  // window end is exclusive (Sep 19 00:00Z)
		{ReleaseID: 50, Date: "not-a-date"},  // malformed
		{ReleaseID: 42, Date: "2026-09-12"},  // unknown release id
		{ReleaseID: 336, Date: "2026-09-08"}, // daily release — unclassified
	}
	events := buildFREDCalendarEvents(dates, names, now)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	byTitle := make(map[string]fredCalendarEvent, len(events))
	for _, event := range events {
		byTitle[event.Title] = event
		if event.Country != "USD" || event.Source != EconomicEventSourceFRED {
			t.Errorf("%q: country/source = %q/%q, want USD/%s",
				event.Title, event.Country, event.Source, EconomicEventSourceFRED)
		}
	}
	cpi := byTitle["Consumer Price Index"]
	if cpi.Impact != "High" || !cpi.EventTime.Equal(time.Date(2026, 9, 11, 13, 30, 0, 0, time.UTC)) {
		t.Errorf("CPI event = %+v, want High at 2026-09-11T13:30:00Z", cpi)
	}
	fomc := byTitle["FOMC Board Meeting Minutes"]
	if fomc.Impact != "High" || !fomc.EventTime.Equal(time.Date(2026, 9, 16, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("FOMC event = %+v, want High at 2026-09-16T18:00:00Z", fomc)
	}
	claims := byTitle["Unemployment Insurance Weekly Claims"]
	if claims.Impact != "Medium" || !claims.EventTime.Equal(time.Date(2026, 9, 10, 13, 30, 0, 0, time.UTC)) {
		t.Errorf("claims event = %+v, want Medium at 2026-09-10T13:30:00Z", claims)
	}
}

// TestFetchFREDCalendarMock drives the full fetch path (releases +
// releases/dates) against an httptest FRED, asserting the official query
// contract: api_key, file_type=json and the ±14d realtime window.
func TestFetchFREDCalendarMock(t *testing.T) {
	const apiKey = "0123456789abcdef0123456789abcdef"
	var gotRealtimeStart, gotRealtimeEnd string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("api_key") != apiKey || query.Get("file_type") != "json" {
			http.Error(w, "bad auth params", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case fredReleasesPath:
			fmt.Fprint(w, `{"releases":[
				{"id":10,"name":"Consumer Price Index"},
				{"id":50,"name":"Employment Situation"},
				{"id":78,"name":"Unemployment Insurance Weekly Claims"},
				{"id":99,"name":"FOMC Board Meeting Minutes"},
				{"id":336,"name":"Federal Funds Effective Rate"}
			]}`)
		case fredReleaseDatesPath:
			gotRealtimeStart = query.Get("realtime_start")
			gotRealtimeEnd = query.Get("realtime_end")
			fmt.Fprint(w, `{"release_dates":[
				{"release_id":10,"date":"2026-09-11"},
				{"release_id":50,"date":"2026-09-04"},
				{"release_id":78,"date":"2026-09-10"},
				{"release_id":99,"date":"2026-09-16"},
				{"release_id":336,"date":"2026-09-08"}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	collector := newFREDCalendarTestCollector(t, server.URL)
	events, err := collector.fetchFREDCalendar(context.Background(), apiKey, now)
	if err != nil {
		t.Fatalf("fetchFREDCalendar: %v", err)
	}
	// 336 (daily) is dropped; 4 events survive.
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if gotRealtimeStart != "2026-08-21" || gotRealtimeEnd != "2026-09-18" {
		t.Errorf("realtime window = %q..%q, want 2026-08-21..2026-09-18",
			gotRealtimeStart, gotRealtimeEnd)
	}
	byTitle := make(map[string]fredCalendarEvent, len(events))
	for _, event := range events {
		byTitle[event.Title] = event
	}
	if fomc := byTitle["FOMC Board Meeting Minutes"]; fomc.Impact != "High" ||
		!fomc.EventTime.Equal(time.Date(2026, 9, 16, 18, 0, 0, 0, time.UTC)) {
		t.Errorf("FOMC = %+v, want High at 18:00Z", fomc)
	}
	if cpi := byTitle["Consumer Price Index"]; cpi.Impact != "High" ||
		!cpi.EventTime.Equal(time.Date(2026, 9, 11, 13, 30, 0, 0, time.UTC)) {
		t.Errorf("CPI = %+v, want High at 13:30Z", cpi)
	}
	if nfp := byTitle["Employment Situation"]; nfp.Impact != "High" {
		t.Errorf("NFP = %+v, want High", nfp)
	}
	if claims := byTitle["Unemployment Insurance Weekly Claims"]; claims.Impact != "Medium" {
		t.Errorf("claims = %+v, want Medium", claims)
	}
}
