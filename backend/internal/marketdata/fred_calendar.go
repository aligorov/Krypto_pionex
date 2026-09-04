package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FRED economic calendar (v2.0.86). ForexFactory 429'd twice in a row with
// 12h backoffs, the weekly feed stopped populating economic_events and the
// deploy economic gate went blind ("эконом-гейт слеп" alarm). The official
// FRED releases calendar takes over as the primary USD calendar:
//
//	GET /fred/releases/dates?realtime_start&realtime_end — release DATES
//	GET /fred/releases                          — release id → name
//
// ForexFactory stays wired as a fallback collector; the 429 backoff is kept
// untouched. Both sources write economic_events, disambiguated by .source.
//
// FRED publishes only the DAY of a release, never the minute. Time-of-day is
// therefore an explicit heuristic:
//
//	13:30 UTC — default for HIGH/MEDIUM statistical releases. BLS/BEA/Census
//	            publish at 8:30 ET; 8:30 EST = 13:30 UTC exactly, and during
//	            EDT the true print (12:30 UTC) still falls INSIDE the gate
//            window [event−2h, event+1h] — the fixed time can over-block by
//	            up to 1h but never misses the print.
//	18:00 UTC — FOMC-class releases (14:00 ET statement; 18:00 UTC in EDT,
//	            19:00 UTC in EST — the same over-block-never-miss argument
//	            applies, and fomc_meetings carries the exact decision times
//	            from migration 0032 as the authoritative FOMC gate).

const (
	// Source tags persisted in economic_events.source (migration 0043).
	EconomicEventSourceFRED = "FRED"
	EconomicEventSourceFF   = "forexfactory"

	fredReleasesPath     = "/releases"
	fredReleaseDatesPath = "/releases/dates"

	// fredCalendarInterval — release schedules move at most monthly; a 6h
	// refresh bounds the repair latency of a wrong/missing date to a third
	// of a day without stressing the shared fred rate limiter.
	fredCalendarInterval = 6 * time.Hour

	// fredCalendarWindow — releases fetched (and re-replaced) for ±14 days
	// around now: long enough to cover the gate's 2h lookahead and the
	// health check's 7d liveness window with margin.
	fredCalendarWindow = 14 * 24 * time.Hour

	// fredPageLimit/fredMaxPages — both endpoints page at 1000 rows max;
	// ~90 releases and <400 release-dates per ±14d window fit in one page,
	// the loop is a correctness backstop, not an expectation.
	fredPageLimit = 1000
	fredMaxPages  = 10
)

// fredRelease is one element of GET /fred/releases.
type fredRelease struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// fredReleaseDate is one element of GET /fred/releases/dates: release_id
// published its data on date (YYYY-MM-DD, day precision only).
type fredReleaseDate struct {
	ReleaseID int    `json:"release_id"`
	Date      string `json:"date"`
}

type fredReleasesResponse struct {
	Releases []fredRelease `json:"releases"`
}

type fredReleaseDatesResponse struct {
	ReleaseDates []fredReleaseDate `json:"release_dates"`
}

// Keyword tables classify a FRED release name into gate impact. Order
// matters: FOMC first (it also pins a different time-of-day), then HIGH
// statistical prints, then MEDIUM context releases. Everything else is
// ignored — FRED carries ~90 releases including regional/academic noise and
// daily "Effective Rate" style series whose release dates fire every
// business day and must never reach the gate.
var (
	// fredFOMCKeywords — FOMC meetings/minutes/press material. Deliberately
	// excludes "Federal Funds Effective Rate" (a DAILY release: matching it
	// would arm a HIGH gate event every business day).
	fredFOMCKeywords = []string{"fomc", "federal open market committee"}

	fredHighKeywords = []string{
		"employment situation", // BLS payrolls/unemployment (NFP)
		"consumer price index", // CPI
		"producer price index", // PPI
		"gross domestic product",
		"personal income and outlays", // PCE lives in this release
		"personal consumption expenditures",
		// FRED names the advance retail report "Advanced Monthly Sales for
		// Retail and Food Services" — match both Advance/Advanced spellings.
		"sales for retail",
	}

	fredMediumKeywords = []string{
		"unemployment insurance weekly claims",
		"initial claims",
		"institute for supply management",
		"ism report",
		"surveys of consumers", // University of Michigan sentiment
		"consumer sentiment",
		"consumer confidence",
		"durable goods",
		"new residential construction", // housing starts
		"housing starts",
		"new home sales",
		"existing home sales",
		"employment cost index",
		"job openings", // JOLTS
		"industrial production",
		"international trade", // trade balance
	}
)

// fredReleaseClass is the classification of one FRED release name.
type fredReleaseClass struct {
	impact     string // "High" | "Medium" — same vocabulary the gate reads
	hourUTC    int
	minuteUTC  int
	classified bool
}

// classifyFREDRelease maps a release name to the economic_events impact
// vocabulary plus the time-of-day heuristic (see the file header).
func classifyFREDRelease(name string) fredReleaseClass {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return fredReleaseClass{}
	}
	if containsAnyKeyword(lower, fredFOMCKeywords) {
		return fredReleaseClass{impact: "High", hourUTC: 18, minuteUTC: 0, classified: true}
	}
	if containsAnyKeyword(lower, fredHighKeywords) {
		return fredReleaseClass{impact: "High", hourUTC: 13, minuteUTC: 30, classified: true}
	}
	if containsAnyKeyword(lower, fredMediumKeywords) {
		return fredReleaseClass{impact: "Medium", hourUTC: 13, minuteUTC: 30, classified: true}
	}
	return fredReleaseClass{}
}

func containsAnyKeyword(lowerName string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(lowerName, keyword) {
			return true
		}
	}
	return false
}

// fredCalendarEvent is one calendar row ready to persist.
type fredCalendarEvent struct {
	Title     string
	EventTime time.Time
	Impact    string
	Country   string
	Source    string
}

// fredCalendarWindowFor returns the [start, end) UTC window releases are
// fetched and replaced for: midnights around now ± fredCalendarWindow.
func fredCalendarWindowFor(now time.Time) (time.Time, time.Time) {
	start := now.Add(-fredCalendarWindow).UTC().Truncate(24 * time.Hour)
	end := now.Add(fredCalendarWindow).UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return start, end
}

// buildFREDCalendarEvents joins release dates with release names, drops
// unclassified/out-of-window/unparseable entries and applies the
// time-of-day heuristic. now pins the ±14d window.
func buildFREDCalendarEvents(
	dates []fredReleaseDate,
	names map[int]string,
	now time.Time,
) []fredCalendarEvent {
	windowStart, windowEnd := fredCalendarWindowFor(now)
	events := make([]fredCalendarEvent, 0, len(dates))
	for _, entry := range dates {
		day, err := time.ParseInLocation("2006-01-02", entry.Date, time.UTC)
		if err != nil {
			continue // malformed date — never reaches the gate
		}
		if day.Before(windowStart) || !day.Before(windowEnd) {
			continue
		}
		name := strings.TrimSpace(names[entry.ReleaseID])
		if name == "" {
			continue // release id without a known name cannot be classified
		}
		class := classifyFREDRelease(name)
		if !class.classified {
			continue // LOW / regional / daily noise — not calendar material
		}
		eventTime := time.Date(
			day.Year(), day.Month(), day.Day(),
			class.hourUTC, class.minuteUTC, 0, 0, time.UTC,
		)
		events = append(events, fredCalendarEvent{
			Title:     name,
			EventTime: eventTime,
			Impact:    class.impact,
			Country:   "USD",
			Source:    EconomicEventSourceFRED,
		})
	}
	return events
}

// collectFREDCalendar is the 6h job: read the FRED key (macro_sources,
// Zero-ENV policy), fetch the ±14d release calendar and replace the FRED
// slice of economic_events transactionally. Every failure is logged — this
// collector is the primary calendar and silent death is what v2.0.86 fixes.
func (c *Collector) collectFREDCalendar(ctx context.Context) {
	var apiKey string
	if err := c.db.QueryRow(ctx,
		`SELECT fred_api_key FROM macro_sources WHERE id = 1`).Scan(&apiKey); err != nil || strings.TrimSpace(apiKey) == "" {
		return // no key configured — ForexFactory stays the only calendar source
	}
	apiKey = strings.TrimSpace(apiKey)

	now := time.Now().UTC()
	events, err := c.fetchFREDCalendar(ctx, apiKey, now)
	if err != nil {
		slog.Warn("fred calendar: fetch failed", "error", err)
		return
	}
	windowStart, windowEnd := fredCalendarWindowFor(now)
	if err := c.replaceFREDCalendarEvents(ctx, events, windowStart, windowEnd); err != nil {
		slog.Warn("fred calendar: persist failed", "error", err)
		return
	}
	slog.Info("fred calendar refreshed",
		"events", len(events), "high", countImpact(events, "High"),
		"medium", countImpact(events, "Medium"),
		"window_from", windowStart.Format(time.DateOnly),
		"window_to", windowEnd.Format(time.DateOnly))
}

func countImpact(events []fredCalendarEvent, impact string) int {
	n := 0
	for _, event := range events {
		if event.Impact == impact {
			n++
		}
	}
	return n
}

// fetchFREDCalendar pulls release names and the realtime-windowed release
// dates, then classifies them into calendar events.
func (c *Collector) fetchFREDCalendar(ctx context.Context, apiKey string, now time.Time) ([]fredCalendarEvent, error) {
	windowStart, windowEnd := fredCalendarWindowFor(now)
	names, err := c.fetchFREDReleaseNames(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("release names: %w", err)
	}
	dates, err := c.fetchFREDReleaseDates(ctx, apiKey, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("release dates: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("release names: FRED returned no releases")
	}
	return buildFREDCalendarEvents(dates, names, now), nil
}

// fetchFREDReleaseNames pages through GET /fred/releases (id → name). The
// limit is the documented maximum; ~90 releases fit on one page.
func (c *Collector) fetchFREDReleaseNames(ctx context.Context, apiKey string) (map[int]string, error) {
	names := make(map[int]string)
	for page := 0; page < fredMaxPages; page++ {
		query := url.Values{
			"api_key":   []string{apiKey},
			"file_type": []string{"json"},
			"limit":     []string{fmt.Sprintf("%d", fredPageLimit)},
			"offset":    []string{fmt.Sprintf("%d", page*fredPageLimit)},
		}
		var resp fredReleasesResponse
		if err := c.getJSON(ctx, limiterKeyFred, c.cfg.FREDBaseURL+fredReleasesPath+"?"+query.Encode(), &resp); err != nil {
			return nil, err
		}
		for _, release := range resp.Releases {
			names[release.ID] = release.Name
		}
		if len(resp.Releases) < fredPageLimit {
			return names, nil
		}
	}
	return nil, fmt.Errorf("more than %d release pages", fredMaxPages)
}

// fetchFREDReleaseDates pages through GET /fred/releases/dates restricted to
// the realtime window [start, end) — only releases published inside it are
// returned, which is exactly the calendar slice the collector replaces.
func (c *Collector) fetchFREDReleaseDates(
	ctx context.Context, apiKey string, start, end time.Time,
) ([]fredReleaseDate, error) {
	var dates []fredReleaseDate
	for page := 0; page < fredMaxPages; page++ {
		query := url.Values{
			"api_key":        []string{apiKey},
			"file_type":      []string{"json"},
			"realtime_start": []string{start.Format("2006-01-02")},
			"realtime_end":   []string{end.Add(-1 * time.Nanosecond).Format("2006-01-02")},
			"limit":          []string{fmt.Sprintf("%d", fredPageLimit)},
			"offset":         []string{fmt.Sprintf("%d", page*fredPageLimit)},
		}
		var resp fredReleaseDatesResponse
		if err := c.getJSON(ctx, limiterKeyFred, c.cfg.FREDBaseURL+fredReleaseDatesPath+"?"+query.Encode(), &resp); err != nil {
			return nil, err
		}
		dates = append(dates, resp.ReleaseDates...)
		if len(resp.ReleaseDates) < fredPageLimit {
			return dates, nil
		}
	}
	return nil, fmt.Errorf("more than %d release-date pages", fredMaxPages)
}

// replaceFREDCalendarEvents swaps the source='FRED' slice of economic_events
// for [windowStart, windowEnd) in ONE transaction: the deploy gate reads
// this table live and must never observe an emptied calendar mid-refresh.
// DELETE+INSERT is the idempotency mechanism — economic_events has no
// UNIQUE(title, event_time) and legacy rows forbid adding one (migration
// 0043); re-running the job leaves the row set byte-identical. Rows from
// other sources (forexfactory) are never touched.
func (c *Collector) replaceFREDCalendarEvents(
	ctx context.Context, events []fredCalendarEvent, windowStart, windowEnd time.Time,
) error {
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		DELETE FROM economic_events
		WHERE source = $1 AND event_time >= $2 AND event_time < $3
	`, EconomicEventSourceFRED, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("delete stale FRED window: %w", err)
	}
	deleted := tag.RowsAffected()

	batch := &pgx.Batch{}
	for _, event := range events {
		batch.Queue(`
			INSERT INTO economic_events (title, event_time, impact, country, source)
			VALUES ($1, $2, $3, $4, $5)
		`, event.Title, event.EventTime, event.Impact, event.Country, event.Source)
	}
	if len(batch.QueuedQueries) > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("insert %d FRED events: %w", len(events), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if deleted > int64(len(events)) {
		slog.Debug("fred calendar window shrank", "deleted", deleted, "inserted", len(events))
	}
	return nil
}
