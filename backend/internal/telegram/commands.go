package telegram

import (
	"context"
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// v2.0.109: the operator-facing command layer. The inbound listener existed
// since the original controller but was never started in main and its only
// report (/status) still spoke the paper epoch. Everything here is READ-ONLY
// against the same tables the web UI reads; the only write path remains the
// pre-existing /kill switch. Auth stays the dispatcher's chat_id check.

// fleetRow is one RUNNING REAL bot for /bots and /status.
type fleetRow struct {
	BotNumber  int
	Symbol     string
	Leverage   int
	Investment decimal.Decimal
	Realized   decimal.Decimal
	Floating   decimal.Decimal
	MaxLoss    decimal.Decimal
	Shifts     int
	CreatedAt  time.Time
}

// closedRow is one terminal REAL bot for /closed, /day and /stats.
type closedRow struct {
	BotNumber int
	Symbol    string
	Final     decimal.Decimal
	Capital   decimal.Decimal
	Reason    string
	ClosedAt  time.Time
	HeldHours float64
}

type dayStat struct {
	Day     string
	Closes  int
	Net     decimal.Decimal
	Wins    int
	Losses  int
	AvgLoss decimal.Decimal
}

// ── formatting helpers ────────────────────────────────────────────────────

func sym(s string) string {
	return html.EscapeString(strings.TrimSuffix(strings.TrimSuffix(s, "_USDT_PERP"), "_PERP"))
}

func moneySigned(d decimal.Decimal) string {
	if d.IsPositive() {
		return "+" + d.Round(2).StringFixed(2)
	}
	return d.Round(2).StringFixed(2)
}

// reasonGlyph is the one-character close-class mark for the narrow tables.
func reasonGlyph(r string) string {
	switch {
	case strings.HasPrefix(r, "STOP_LOSS"), r == "LOSS_STOP":
		return "✖стоп"
	case strings.HasPrefix(r, "RADAR_AUTOCLOSE"):
		return "⚠радар"
	case r == "TAKE_PROFIT":
		return "✔тейк"
	case r == "GRID_AGED_HALF_LIFE":
		return "↻рот"
	default:
		return strings.ToLower(strings.ReplaceAll(r, "_", " "))
	}
}

func shortReason(r string) string {
	switch {
	case strings.HasPrefix(r, "STOP_LOSS"):
		return "стоп"
	case strings.HasPrefix(r, "RADAR_AUTOCLOSE"):
		return "радар"
	case r == "GRID_AGED_HALF_LIFE":
		return "ротация"
	case r == "TAKE_PROFIT":
		return "тейк"
	default:
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(r, "_", " "), " ", " "))
	}
}

// ── data loaders (read-only) ──────────────────────────────────────────────

func loadFleet(ctx context.Context, db *pgxpool.Pool) ([]fleetRow, error) {
	rows, err := db.Query(ctx, `
		SELECT COALESCE(bot_number,0), symbol, leverage, quote_investment,
		       COALESCE(realized_pnl_usdt,0), COALESCE(unrealized_pnl_usdt,0),
		       COALESCE(max_loss_usdt,0), adjustments_count, created_at
		FROM grid_bots
		WHERE status IN ('RUNNING','STOP_REQUESTED','STOPPING') AND bu_order_id IS NOT NULL
		ORDER BY bot_number
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]fleetRow, 0, 24)
	for rows.Next() {
		var r fleetRow
		if err := rows.Scan(&r.BotNumber, &r.Symbol, &r.Leverage, &r.Investment,
			&r.Realized, &r.Floating, &r.MaxLoss, &r.Shifts, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func loadClosed(ctx context.Context, db *pgxpool.Pool, limit int, sinceDays int) ([]closedRow, error) {
	rows, err := db.Query(ctx, `
		SELECT COALESCE(bot_number,0), symbol, realized_pnl_usdt,
		       COALESCE(quote_investment,0), COALESCE(closed_reason,''), closed_at,
		       EXTRACT(EPOCH FROM (closed_at - created_at))/3600
		FROM grid_bots
		WHERE closed_at IS NOT NULL AND bu_order_id IS NOT NULL
		  AND realized_pnl_usdt IS NOT NULL
		  AND ($2::INT = 0 OR closed_at > NOW() - ($2::INT * INTERVAL '1 day'))
		ORDER BY closed_at DESC
		LIMIT $1
	`, limit, sinceDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]closedRow, 0, limit)
	for rows.Next() {
		var r closedRow
		if err := rows.Scan(&r.BotNumber, &r.Symbol, &r.Final, &r.Capital, &r.Reason, &r.ClosedAt, &r.HeldHours); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func dayStatsFrom(rows []closedRow) []dayStat {
	byDay := map[string]*dayStat{}
	for _, r := range rows {
		key := r.ClosedAt.UTC().Format("02.01")
		st, ok := byDay[key]
		if !ok {
			st = &dayStat{Day: key}
			byDay[key] = st
		}
		st.Closes++
		st.Net = st.Net.Add(r.Final)
		if r.Final.IsPositive() {
			st.Wins++
		} else {
			st.Losses++
			st.AvgLoss = st.AvgLoss.Add(r.Final)
		}
	}
	out := make([]dayStat, 0, len(byDay))
	for _, st := range byDay {
		if st.Losses > 0 {
			st.AvgLoss = st.AvgLoss.Div(decimal.NewFromInt(int64(st.Losses)))
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

// ── report builders (pure — unit-tested) ─────────────────────────────────

// sparkline renders a price series as 8 unicode blocks (narrow-screen
// friendly). Series shorter than 2 points yields an em-dash.
func sparkline(prices []float64) string {
	if len(prices) < 2 {
		return "—"
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	const cells = 8
	lo, hi := prices[0], prices[0]
	for _, p := range prices {
		if p < lo {
			lo = p
		}
		if p > hi {
			hi = p
		}
	}
	if hi <= lo {
		return strings.Repeat(string(blocks[len(blocks)-1]), cells)
	}
	// Bucket the series by time order so the shape reads left→right.
	out := make([]byte, 0, cells*3)
	bucket := make([]float64, cells)
	counts := make([]int, cells)
	for i, p := range prices {
		idx := i * cells / len(prices)
		bucket[idx] += p
		counts[idx]++
	}
	for i := 0; i < cells; i++ {
		if counts[i] == 0 {
			out = append(out, []byte("·")...)
			continue
		}
		v := (bucket[i]/float64(counts[i]) - lo) / (hi - lo)
		lvl := int(v * float64(len(blocks)-1))
		out = append(out, []byte(string(blocks[lvl]))...)
	}
	return string(out)
}

// capBar renders cap usage as ▓▓▓░░░░░ (8 cells), sign-aware.
func capBar(total, maxLoss decimal.Decimal) string {
	if !maxLoss.IsPositive() {
		return "········"
	}
	use := total.Abs().Div(maxLoss)
	pct, _ := use.Mul(decimal.NewFromInt(100)).Float64()
	cells := int(pct / 100 * 8)
	if cells < 0 {
		cells = 0
	}
	if cells > 8 {
		cells = 8
	}
	return strings.Repeat("▓", cells) + strings.Repeat("░", 8-cells)
}

// buildFleetTable renders the compact mobile-first fleet view: one line per
// bot ≈30 chars wide — #sym lever+cap, total, cap bar+%, 24h spark.
func buildFleetTable(fleet []fleetRow, sparks map[int]string) string {
	if len(fleet) == 0 {
		return "флот пуст"
	}
	var b strings.Builder
	b.WriteString("<pre>#    симв   гир  итог   капа   24ч\n")
	var realized, floating, capital decimal.Decimal
	for _, r := range fleet {
		total := r.Realized.Add(r.Floating)
		realized = realized.Add(r.Realized)
		floating = floating.Add(r.Floating)
		capital = capital.Add(r.Investment)
		pct := "   —"
		if r.MaxLoss.IsPositive() {
			p := total.Div(r.MaxLoss).Mul(decimal.NewFromInt(100)).Round(0)
			pct = fmt.Sprintf("%4s%%", p.String())
		}
		sp := sparks[r.BotNumber]
		if sp == "" {
			sp = "—"
		}
		fmt.Fprintf(&b, "%-4d %-6s %d%-3d %6s %s %s\n",
			r.BotNumber%10000, sym(r.Symbol), r.Leverage, r.Investment.Round(0).IntPart(),
			moneySigned(total), pct, sp)
	}
	b.WriteString("</pre>")
	b.WriteString(fmt.Sprintf("Σ $%s · realized %s · плав. %s · нетто <b>%s</b>",
		capital.Round(0).String(), moneySigned(realized), moneySigned(floating), moneySigned(realized.Add(floating))))
	return b.String()
}

// loadSparks pulls 24h price series per RUNNING bot and renders sparklines.
func loadSparks(ctx context.Context, db *pgxpool.Pool, botNumbers []int) (map[int]string, error) {
	if len(botNumbers) == 0 {
		return map[int]string{}, nil
	}
	rows, err := db.Query(ctx, `
		SELECT bot_number, price::FLOAT8, captured_at
		FROM bot_telemetry
		WHERE bot_number = ANY($1::INT[]) AND captured_at > NOW() - INTERVAL '24 hours'
		ORDER BY captured_at
	`, botNumbers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	series := map[int][]float64{}
	for rows.Next() {
		var n int
		var p float64
		var at time.Time
		if err := rows.Scan(&n, &p, &at); err != nil {
			return nil, err
		}
		series[n] = append(series[n], p)
	}
	out := make(map[int]string, len(series))
	for n, ps := range series {
		out[n] = sparkline(ps)
	}
	return out, rows.Err()
}

func buildClosedTable(rows []closedRow) string {
	if len(rows) == 0 {
		return "закрытий нет"
	}
	var b strings.Builder
	b.WriteString("<pre>#    симв    финал держал прич\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%-4d %-7s %7s %5.1fh %s\n", r.BotNumber%10000, sym(r.Symbol), moneySigned(r.Final), r.HeldHours, reasonGlyph(r.Reason))
	}
	b.WriteString("</pre>")
	return b.String()
}

func buildDayStats(days []dayStat) string {
	if len(days) == 0 {
		return "статистики нет"
	}
	var b strings.Builder
	var total decimal.Decimal
	closes, wins := 0, 0
	b.WriteString("<pre>день     закрытий  W/L    нетто    ср.минус\n")
	for _, st := range days {
		avgLoss := "—"
		if st.Losses > 0 {
			avgLoss = st.AvgLoss.Round(2).StringFixed(2)
		}
		fmt.Fprintf(&b, "%s %6d   %2d/%-2d %8s %8s\n", st.Day, st.Closes, st.Wins, st.Losses, moneySigned(st.Net), avgLoss)
		total = total.Add(st.Net)
		closes += st.Closes
		wins += st.Wins
	}
	b.WriteString("</pre>")
	wr := 0
	if closes > 0 {
		wr = wins * 100 / closes
	}
	fmt.Fprintf(&b, "\nΣ закрыто <b>%s</b> на %d закрытиях · винрейт <b>%d%%</b> · среднее <b>%s</b>/закрытие",
		moneySigned(total), closes, wr, total.Div(decimal.NewFromInt(int64(closes))).Round(2).StringFixed(2))
	return b.String()
}

// forecastLine projects the observed daily net onto month/year at the
// CURRENT deployed capital — a straight read of the run rate, no compounding,
// labelled as such.
func forecastLine(days []dayStat, capital decimal.Decimal) string {
	if len(days) == 0 || !capital.IsPositive() {
		return ""
	}
	var net decimal.Decimal
	for _, st := range days {
		net = net.Add(st.Net)
	}
	perDay := net.Div(decimal.NewFromInt(int64(len(days))))
	perMonth := perDay.Mul(decimal.NewFromInt(30))
	pctDay := perDay.Div(capital).Mul(decimal.NewFromInt(100))
	return fmt.Sprintf("\n📉 Темп: <b>%s/день</b> (~%s%%/день капитала) → месяц <b>≈%s</b> (без реинвеста)",
		perDay.Round(2).StringFixed(2), pctDay.Round(2).StringFixed(2), perMonth.Round(0).StringFixed(0))
}
