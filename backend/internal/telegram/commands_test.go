package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func d(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	v, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("bad decimal %q", s)
	}
	return v
}

func TestBuildFleetTable(t *testing.T) {
	fleet := []fleetRow{
		{BotNumber: 1395, Symbol: "DOT_USDT_PERP", Leverage: 4, Investment: d(t, "50"),
			Realized: d(t, "0.03"), Floating: d(t, "-0.25"), MaxLoss: d(t, "4"), Shifts: 0,
			CreatedAt: time.Now().Add(-90 * time.Minute)},
		{BotNumber: 1381, Symbol: "ICP_USDT_PERP", Leverage: 4, Investment: d(t, "100"),
			Realized: d(t, "0.20"), Floating: d(t, "-1.47"), MaxLoss: d(t, "8"), Shifts: 2,
			CreatedAt: time.Now().Add(-5 * time.Hour)},
	}
	out := buildFleetTable(fleet, map[int]string{1395: "▂▄▆█▆▄▂▁", 1381: "▁▂▃▅▇█▇▅"})
	for _, want := range []string{"1395", "DOT", "ICP", "-1.27", " -16%", "▂▄▆█▆▄▂▁", "4100"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fleet table missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "нетто <b>-1.49</b>") {
		t.Fatalf("fleet totals line wrong:\n%s", out)
	}
}

func TestBuildClosedTableAndDayStats(t *testing.T) {
	base := time.Now().UTC()
	rows := []closedRow{
		{BotNumber: 1284, Symbol: "SUI_USDT_PERP", Final: d(t, "-4.84"), Capital: d(t, "50"), Reason: "STOP_LOSS", ClosedAt: base.Add(-26 * time.Hour), HeldHours: 2.4},
		{BotNumber: 1289, Symbol: "XRP_USDT_PERP", Final: d(t, "3.01"), Capital: d(t, "100"), Reason: "GRID_AGED_HALF_LIFE", ClosedAt: base.Add(-2 * time.Hour), HeldHours: 27.1},
		{BotNumber: 1387, Symbol: "MARSCOIN_USDT_PERP", Final: d(t, "-0.41"), Capital: d(t, "50"), Reason: "RADAR_AUTOCLOSE_STRICT", ClosedAt: base.Add(-1 * time.Hour), HeldHours: 3.3},
	}
	closed := buildClosedTable(rows)
	for _, want := range []string{"✖стоп", "↻рот", "⚠радар", "-4.84", "+3.01"} {
		if !strings.Contains(closed, want) {
			t.Fatalf("closed table missing %q:\n%s", want, closed)
		}
	}

	days := dayStatsFrom(rows)
	if len(days) != 2 {
		t.Fatalf("expected 2 day buckets, got %d", len(days))
	}
	stats := buildDayStats(days)
	if !strings.Contains(stats, "Σ закрыто <b>-2.24</b>") || !strings.Contains(stats, "33%") {
		t.Fatalf("day stats totals wrong:\n%s", stats)
	}
}

func TestForecastLine(t *testing.T) {
	days := []dayStat{
		{Day: "25.09", Closes: 8, Net: d(t, "5.41"), Wins: 6, Losses: 2},
		{Day: "26.09", Closes: 25, Net: d(t, "5.13"), Wins: 18, Losses: 7},
	}
	out := forecastLine(days, d(t, "800"))
	if !strings.Contains(out, "5.27/день") || !strings.Contains(out, "0.66%") || !strings.Contains(out, "≈158") {
		t.Fatalf("forecast line wrong:\n%s", out)
	}
	if forecastLine(nil, d(t, "800")) != "" {
		t.Fatalf("empty days must yield no forecast")
	}
}

func TestSparkline(t *testing.T) {
	rising := sparkline([]float64{1, 2, 3, 4, 5, 6, 7, 8})
	if !strings.HasPrefix(rising, "▁") || !strings.HasSuffix(rising, "█") {
		t.Fatalf("rising series must go low→high: %s", rising)
	}
	flat := sparkline([]float64{5, 5, 5})
	if flat != "████████" {
		t.Fatalf("flat series must be full blocks: %s", flat)
	}
	if got := sparkline([]float64{1}); got != "—" {
		t.Fatalf("single point must yield dash: %s", got)
	}
}

func TestCapBar(t *testing.T) {
	if got := capBar(decimal.RequireFromString("-2"), decimal.RequireFromString("8")); got != "▓▓░░░░░░" {
		t.Fatalf("25%% usage bar wrong: %s", got)
	}
	if got := capBar(decimal.RequireFromString("1.5"), decimal.RequireFromString("2")); got != "▓▓▓▓▓▓░░" {
		t.Fatalf("75%% usage bar wrong: %s", got)
	}
	if got := capBar(decimal.Zero, decimal.Zero); got != "········" {
		t.Fatalf("no-cap bar wrong: %s", got)
	}
}
