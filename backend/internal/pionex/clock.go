package pionex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Clock maintains server time offset synchronization.
type Clock struct {
	offsetMS int64 // ServerTime - LocalTime
}

// NewClock creates a Clock instance.
func NewClock() *Clock {
	return &Clock{}
}

// SetOffset manually sets the millisecond offset.
func (c *Clock) SetOffset(offsetMS int64) {
	atomic.StoreInt64(&c.offsetMS, offsetMS)
}

// NowMilli returns the estimated Pionex server timestamp in milliseconds.
func (c *Clock) NowMilli() int64 {
	offset := atomic.LoadInt64(&c.offsetMS)
	return time.Now().UnixMilli() + offset
}

// SyncWithServer fetches server time and updates local offset.
func (c *Clock) SyncWithServer(ctx context.Context, baseURL string) error {
	if baseURL == "" {
		baseURL = DefaultHost
	}
	url := fmt.Sprintf("%s/api/v1/common/timestamp", baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch server timestamp: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Result bool `json:"result"`
		Data   struct {
			Timestamp int64 `json:"timestamp"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode timestamp: %w", err)
	}

	if result.Data.Timestamp > 0 {
		localNow := time.Now().UnixMilli()
		c.SetOffset(result.Data.Timestamp - localNow)
	}
	return nil
}
