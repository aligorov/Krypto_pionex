package marketdata

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Macro collectors (v2.0.59) — free regime-context sources found by the
// 2026-09-01 data-gap audit. All strictly read-only references; Pionex
// stays the sole execution venue.
//
//	FRED   : DGS2/DGS10/DTWEXBGS/VIXCLS/STLFSI4/T10YIE daily series.
//	         Keyless legs of the stack never depend on the key being set.
//	Yahoo  : ^VIX + DX-Y.NYB intraday via the public chart endpoint
//	         (query1, query2 fallback — unofficial, UA required).
//	GNews  : Google News RSS "Fed OR CPI OR FOMC when:6h" — zero quota.

const (
	fredBaseURL      = "https://api.stlouisfed.org/fred/series/observations"
	yahooChartURL    = "https://query1.finance.yahoo.com/v8/finance/chart/"
	yahooChartURLAlt = "https://query2.finance.yahoo.com/v8/finance/chart/"
	gnewsRSSURL      = "https://news.google.com/rss/search?q=Fed+OR+CPI+OR+FOMC+OR+%22rate+decision%22+when:6h&hl=en-US&gl=US&ceid=US:en"

	macroRetention = 30 * 24 * time.Hour
	newsRetention  = 14 * 24 * time.Hour

	macroCollectorUA = "pionex-autogrid-collector/2.0 (+macro)"
)

var fredSeries = []string{"DGS2", "DGS10", "DTWEXBGS", "VIXCLS", "STLFSI4", "T10YIE"}

// collectFRED persists the latest observation of each tracked series.
func (c *Collector) collectFRED(ctx context.Context) {
	var apiKey string
	if err := c.db.QueryRow(ctx,
		`SELECT fred_api_key FROM macro_sources WHERE id = 1`).Scan(&apiKey); err != nil || strings.TrimSpace(apiKey) == "" {
		return // no key configured — keyless legs keep the stack alive
	}
	for _, series := range fredSeries {
		if ctx.Err() != nil {
			return
		}
		query := url.Values{
			"series_id":  []string{series},
			"api_key":    []string{apiKey},
			"file_type":  []string{"json"},
			"sort_order": []string{"desc"},
			"limit":      []string{"2"},
		}
		var resp struct {
			Observations []struct {
				Value string `json:"value"`
			} `json:"observations"`
		}
		if err := c.getJSON(ctx, limiterKeyFred, fredBaseURL+"?"+query.Encode(), &resp); err != nil {
			continue // per-series isolation: one bad series never wedges the rest
		}
		for _, obs := range resp.Observations {
			value, err := decimal.NewFromString(strings.TrimSpace(obs.Value))
			if err != nil {
				continue // "." = FRED's missing-value marker
			}
			if _, err := c.db.Exec(ctx, `
				INSERT INTO macro_snapshots (source, metric, value) VALUES ('fred', $1, $2)
			`, series, value); err == nil {
				break // newest valid observation only
			}
		}
	}
}

// yahooChartResponse is the subset of the public chart payload we need.
type yahooChartResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
			} `json:"meta"`
		} `json:"result"`
	} `json:"chart"`
}

// collectYahoo persists intraday VIX and the dollar index. The endpoint is
// unofficial: UA header + query2 fallback keep it dependable, and an outage
// only thins the LLM context, never gates anything.
func (c *Collector) collectYahoo(ctx context.Context) {
	for _, ticker := range []struct{ symbol, metric string }{
		{"%5EVIX", "VIX"},
		{"DX-Y.NYB", "DXY"},
	} {
		if ctx.Err() != nil {
			return
		}
		var resp yahooChartResponse
		path := yahooChartURL + ticker.symbol + "?interval=15m&range=1d"
		if err := c.fetchUA(ctx, limiterKeyYahoo, path, &resp); err != nil {
			path = yahooChartURLAlt + ticker.symbol + "?interval=15m&range=1d"
			if err := c.fetchUA(ctx, limiterKeyYahoo, path, &resp); err != nil {
				continue
			}
		}
		if len(resp.Chart.Result) == 0 || resp.Chart.Result[0].Meta.RegularMarketPrice <= 0 {
			continue
		}
		_, _ = c.db.Exec(ctx, `
			INSERT INTO macro_snapshots (source, metric, value) VALUES ('yahoo', $1, $2)
		`, ticker.metric, decimal.NewFromFloat(resp.Chart.Result[0].Meta.RegularMarketPrice))
	}
}

// fetchUA is getJSON with a browser-ish UA — Yahoo rejects bare clients.
func (c *Collector) fetchUA(ctx context.Context, limiterKey, endpoint string, out any) error {
	limiter := c.limiters[limiterKey]
	if limiter == nil {
		return fmt.Errorf("unknown rate limiter %q", limiterKey)
	}
	if err := limiter.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s returned HTTP %d", limiterKey, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out)
}

// gnewsRSS is the subset of the RSS envelope Google News serves.
type gnewsRSS struct {
	Channel struct {
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			PublishedAt string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

// collectGNews stores the last 6h of macro headlines for the LLM context.
func (c *Collector) collectGNews(ctx context.Context) {
	limiter := c.limiters[limiterKeyGNews]
	if limiter == nil {
		return
	}
	if err := limiter.wait(ctx); err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gnewsRSSURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", macroCollectorUA)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var feed gnewsRSS
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&feed); err != nil {
		return
	}
	stored := 0
	for _, item := range feed.Channel.Items {
		if stored >= 20 || ctx.Err() != nil {
			break
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		var published *time.Time
		if t, err := time.Parse(time.RFC1123Z, item.PublishedAt); err == nil {
			published = &t
		}
		tag, err := c.db.Exec(ctx, `
			INSERT INTO news_headlines (source, title, url, published_at)
			VALUES ('gnews', $1, $2, $3)
			ON CONFLICT (source, title) DO NOTHING
		`, title, item.Link, published)
		if err == nil && tag.RowsAffected() > 0 {
			stored++
		}
	}
	// Macro retention piggybacks the news loop.
	c.retentionDelete(ctx, "macro_snapshots", macroRetention)
	c.retentionDelete(ctx, "news_headlines", newsRetention)
}

// ---------------------------------------------------------------------------
// Operator-facing macro settings (v2.0.60): the FRED key is managed from the
// web UI exactly like the LLM/Telegram keys — stored in PostgreSQL, never in
// env, never returned to the client.
// ---------------------------------------------------------------------------

// MacroSeriesPoint is the latest snapshot of one FRED/Yahoo metric.
type MacroSeriesPoint struct {
	Metric     string    `json:"metric"`
	Value      float64   `json:"value"`
	CapturedAt time.Time `json:"capturedAt"`
}

// MacroSourcesStatus is the GET /api/macro/sources payload: whether a key is
// configured (length only — the key itself never leaves the server) and what
// the collectors have actually persisted, so the operator sees the feed
// working without waiting an hour.
type MacroSourcesStatus struct {
	HasKey     bool               `json:"hasKey"`
	KeyLength  int                `json:"keyLength"`
	UpdatedAt  *time.Time         `json:"updatedAt,omitempty"`
	Series     []MacroSeriesPoint `json:"series"`
}

func (s *Service) GetMacroSources(ctx context.Context) (MacroSourcesStatus, error) {
	var status MacroSourcesStatus
	var key string
	if err := s.db.QueryRow(ctx,
		`SELECT fred_api_key, updated_at FROM macro_sources WHERE id = 1`,
	).Scan(&key, &status.UpdatedAt); err != nil {
		return status, err
	}
	status.KeyLength = len(strings.TrimSpace(key))
	status.HasKey = status.KeyLength > 0
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (metric) metric, value::FLOAT8, captured_at
		FROM macro_snapshots ORDER BY metric, captured_at DESC
	`)
	if err == nil {
		for rows.Next() {
			var p MacroSeriesPoint
			if rows.Scan(&p.Metric, &p.Value, &p.CapturedAt) == nil {
				status.Series = append(status.Series, p)
			}
		}
		rows.Close()
	}
	return status, nil
}

// UpdateFREDKey stores (or, with an empty value, removes) the FRED api key.
func (s *Service) UpdateFREDKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key != "" && len(key) != 32 {
		return fmt.Errorf("FRED-ключ — 32 символа, получено %d", len(key))
	}
	_, err := s.db.Exec(ctx, `
		UPDATE macro_sources SET fred_api_key = $1, updated_at = NOW() WHERE id = 1
	`, key)
	return err
}

// FREDKey returns the stored key (empty when unset).
func (s *Service) FREDKey(ctx context.Context) (string, error) {
	var key string
	err := s.db.QueryRow(ctx,
		`SELECT fred_api_key FROM macro_sources WHERE id = 1`).Scan(&key)
	return strings.TrimSpace(key), err
}

// TestFREDKey validates a key with one lightweight call (DGS10, limit 1).
func (s *Service) TestFREDKey(ctx context.Context, key string) (int64, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return 0, fmt.Errorf("ключ не задан")
	}
	start := time.Now()
	endpoint := fredBaseURL + "?" + url.Values{
		"series_id": []string{"DGS10"}, "api_key": []string{key},
		"file_type": []string{"json"}, "sort_order": []string{"desc"}, "limit": []string{"1"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", macroCollectorUA)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start).Milliseconds(), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Since(start).Milliseconds(), fmt.Errorf("FRED вернул HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Observations []struct {
			Value string `json:"value"`
		} `json:"observations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return time.Since(start).Milliseconds(), err
	}
	if len(payload.Observations) == 0 {
		return time.Since(start).Milliseconds(), fmt.Errorf("FRED не вернул наблюдений")
	}
	return time.Since(start).Milliseconds(), nil
}
