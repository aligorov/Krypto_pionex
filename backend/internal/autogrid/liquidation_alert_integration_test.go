package autogrid

import (
	"context"
	"testing"
)

// The disposable database is also used to prove that a recent liquidation
// cannot hide a dead configured transport, and quiet markets never alarm.
func TestDataHealthCheckLiquidationMatchesAdmission(t *testing.T) {
	pool, ctx := connectEconomicPool(t)
	const source = "alert-test"
	var previousSource string
	if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT value::text FROM app_config WHERE key='liquidation_source'), 'null')`).Scan(&previousSource); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_config(key,value) VALUES ('liquidation_source','"alert-test"')
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM liquidation_feed_health WHERE source IN ('alert-test','other-alert-test')`)
		_, _ = pool.Exec(ctx, `DELETE FROM liquidation_events WHERE symbol='ALERT_TEST_USDT_PERP'`)
		if previousSource == "null" {
			_, _ = pool.Exec(ctx, `DELETE FROM app_config WHERE key='liquidation_source'`)
		} else {
			_, _ = pool.Exec(ctx, `UPDATE app_config SET value=$1::jsonb WHERE key='liquidation_source'`, previousSource)
		}
	})
	// Only the configured source may authorize risk, even if another feed is fresh.
	if _, err := pool.Exec(ctx, `INSERT INTO liquidation_feed_health(source,connected,last_message_at)
		VALUES ('other-alert-test',true,NOW())`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		connected bool
		age       string
		missing   bool
		quiet     bool
		wantAlarm bool
	}{
		{"quiet-empty", true, "0 seconds", false, true, false},
		{"stale-despite-recent-event", true, "16 minutes", false, false, true},
		{"disconnected-despite-recent-event", false, "0 seconds", false, false, true},
		{"future-heartbeat", true, "-1 hour", false, false, true},
		{"missing-configured-source", false, "0 seconds", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM liquidation_events`); err != nil {
				t.Fatal(err)
			}
			if !tc.quiet {
				if _, err := pool.Exec(ctx, `INSERT INTO liquidation_events(symbol,side,value_usd)
					VALUES ('ALERT_TEST_USDT_PERP','long',1)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `DELETE FROM liquidation_feed_health WHERE source=$1`, source); err != nil {
				t.Fatal(err)
			}
			if !tc.missing {
				if _, err := pool.Exec(ctx, `INSERT INTO liquidation_feed_health(source,connected,last_message_at)
					VALUES ($1,$2,NOW()-$3::interval)`, source, tc.connected, tc.age); err != nil {
					t.Fatal(err)
				}
			}
			worker := newEconomicTestWorker(pool)
			worker.dataHealthCheck(ctx)
			if _, alarmed := worker.dataAlarmAt["liquidation_events"]; alarmed != tc.wantAlarm {
				t.Fatalf("alarmed=%v, want %v", alarmed, tc.wantAlarm)
			}
			// Recovery clears the episode; the same worker must not retain a stale alarm.
			if _, err := pool.Exec(ctx, `INSERT INTO liquidation_feed_health(source,connected,last_message_at)
				VALUES ($1,true,NOW()) ON CONFLICT(source) DO UPDATE
				SET connected=true,last_message_at=NOW()`, source); err != nil {
				t.Fatal(err)
			}
			worker.dataHealthCheck(ctx)
			if _, alarmed := worker.dataAlarmAt["liquidation_events"]; alarmed {
				t.Fatal("recovered transport retained the alarm")
			}
		})
	}
}
