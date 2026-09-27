package marketdata

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
	"time"
)

func TestLiquidationTransportHealthPersistsWithoutEvents(t *testing.T) {
	url := os.Getenv("PIONEX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PIONEX_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const source = "transport-test"
	defer pool.Exec(ctx, `DELETE FROM liquidation_feed_health WHERE source=$1`, source)
	l := &LiquidationListener{db: pool}
	l.recordTransport(ctx, source, true)
	var connected bool
	var first, last time.Time
	if err := pool.QueryRow(ctx, `SELECT connected,last_message_at FROM liquidation_feed_health WHERE source=$1`, source).Scan(&connected, &first); err != nil || !connected {
		t.Fatalf("connected: %v %v", connected, err)
	}
	l.recordTransport(ctx, source, true) // The message burst must be throttled.
	if err := pool.QueryRow(ctx, `SELECT last_message_at FROM liquidation_feed_health WHERE source=$1`, source).Scan(&last); err != nil || !last.Equal(first) {
		t.Fatalf("burst was not throttled: %v", err)
	}
	l.recordTransport(ctx, source, false) // Disconnect must bypass throttling.
	if err := pool.QueryRow(ctx, `SELECT connected,last_message_at FROM liquidation_feed_health WHERE source=$1`, source).Scan(&connected, &last); err != nil || connected || !last.Equal(first) {
		t.Fatalf("disconnect: %v %v", connected, err)
	}
	l.recordTransport(ctx, source, true) // Reconnect must immediately restore evidence.
	if err := pool.QueryRow(ctx, `SELECT connected FROM liquidation_feed_health WHERE source=$1`, source).Scan(&connected); err != nil || !connected {
		t.Fatalf("reconnect: %v %v", connected, err)
	}
}
