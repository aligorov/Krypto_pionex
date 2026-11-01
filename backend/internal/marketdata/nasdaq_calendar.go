package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EconomicEventSourceNasdaq tag stored in economic_events.source.
const EconomicEventSourceNasdaq = "nasdaq"

const (
	nasdaqCalendarURL    = "https://api.nasdaq.com/api/calendar/economicevents"
	nasdaqDefaultTimeout = 10 * time.Second
)

// nasdaqResponse represents the JSON payload from api.nasdaq.com/api/calendar/economicevents.
type nasdaqResponse struct {
	Data struct {
		Rows []struct {
			GMT         string `json:"gmt"`
			Country     string `json:"country"`
			EventName   string `json:"eventName"`
			Actual      string `json:"actual"`
			Consensus   string `json:"consensus"`
			Previous    string `json:"previous"`
			Description string `json:"description"`
		} `json:"rows"`
	} `json:"data"`
}

// NasdaqEvent represents one parsed and classified economic event.
type NasdaqEvent struct {
	Title     string
	EventTime time.Time
	Impact    string // "High" | "Medium"
	Country   string
	Actual    string
	Consensus string
	Previous  string
}

var (
	nasdaqHighKeywords = []string{
		"fomc", "federal open market committee", "interest rate decision",
		"cpi", "consumer price index",
		"ppi", "producer price index",
		"pce", "personal consumption expenditures", "personal income",
		"gdp", "gross domestic product",
		"employment situation", "nonfarm payrolls", "unemployment rate",
		"retail sales",
	}

	nasdaqMediumKeywords = []string{
		"jobless claims", "initial claims", "continuing claims",
		"ism", "institute for supply management", "pmi",
		"consumer sentiment", "consumer confidence", "michigan",
		"durable goods", "housing starts", "home sales",
		"jolts", "job openings",
	}
)

// classifyNasdaqEvent determines event impact based on market-moving keyword tables.
func classifyNasdaqEvent(name string) (impact string, ok bool) {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return "", false
	}

	for _, kw := range nasdaqHighKeywords {
		if strings.Contains(lower, kw) {
			return "High", true
		}
	}
	for _, kw := range nasdaqMediumKeywords {
		if strings.Contains(lower, kw) {
			return "Medium", true
		}
	}

	return "", false
}

// parseNasdaqTime converts a date (YYYY-MM-DD) and GMT/ET time string (HH:MM) to UTC.
// Nasdaq's time column is served in Eastern Time (America/New_York).
func parseNasdaqTime(dateStr, timeStr string, loc *time.Location) (time.Time, error) {
	trimmedTime := strings.TrimSpace(timeStr)
	if trimmedTime == "" || trimmedTime == "N/A" {
		trimmedTime = "08:30" // default morning statistical release anchor
	}
	parts := strings.Split(trimmedTime, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("invalid time format %q", timeStr)
	}

	layout := "2006-01-02 15:04"
	fullStr := fmt.Sprintf("%s %02s:%02s", dateStr, parts[0], parts[1])
	t, err := time.ParseInLocation(layout, fullStr, loc)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// FetchNasdaqCalendar fetches and parses economic events for a given target date.
func FetchNasdaqCalendar(ctx context.Context, targetDate time.Time) ([]NasdaqEvent, error) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("EDT", -4*3600)
	}

	dateParam := targetDate.Format(time.DateOnly)
	reqURL := fmt.Sprintf("%s?date=%s", nasdaqCalendarURL, dateParam)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: nasdaqDefaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nasdaq request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nasdaq returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var parsed nasdaqResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal nasdaq: %w", err)
	}

	var events []NasdaqEvent
	for _, row := range parsed.Data.Rows {
		if !strings.EqualFold(row.Country, "United States") {
			continue
		}
		impact, ok := classifyNasdaqEvent(row.EventName)
		if !ok {
			continue
		}

		eventTime, err := parseNasdaqTime(dateParam, row.GMT, loc)
		if err != nil {
			continue
		}

		events = append(events, NasdaqEvent{
			Title:     strings.TrimSpace(row.EventName),
			EventTime: eventTime,
			Impact:    impact,
			Country:   "USD",
			Actual:    strings.TrimSpace(row.Actual),
			Consensus: strings.TrimSpace(row.Consensus),
			Previous:  strings.TrimSpace(row.Previous),
		})
	}

	return events, nil
}

// ReplaceNasdaqEvents persists fetched Nasdaq events into economic_events table.
func ReplaceNasdaqEvents(ctx context.Context, db *pgxpool.Pool, events []NasdaqEvent, windowStart, windowEnd time.Time) error {
	if db == nil {
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		DELETE FROM economic_events
		WHERE source = $1 AND event_time >= $2 AND event_time < $3
	`, EconomicEventSourceNasdaq, windowStart, windowEnd)
	if err != nil {
		return fmt.Errorf("delete stale nasdaq events: %w", err)
	}

	batch := &pgx.Batch{}
	for _, ev := range events {
		batch.Queue(`
			INSERT INTO economic_events (title, event_time, impact, country, source)
			VALUES ($1, $2, $3, $4, $5)
		`, ev.Title, ev.EventTime, ev.Impact, ev.Country, EconomicEventSourceNasdaq)
	}

	if len(batch.QueuedQueries) > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("insert nasdaq events batch: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit nasdaq events: %w", err)
	}

	slog.Info("nasdaq economic calendar refreshed",
		"events", len(events),
		"window_from", windowStart.Format(time.DateOnly),
		"window_to", windowEnd.Format(time.DateOnly),
	)
	return nil
}
