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

func buildFleetTable(fleet []fleetRow) string {
	if len(fleet) == 0 {
		return "флот пуст"
	}
	var b strings.Builder
	b.WriteString("<pre>  #  символ    гир  кап   итог    к капе  возр  сдв\n")
	var realized, floating, capital decimal.Decimal
	for _, r := range fleet {
		total := r.Realized.Add(r.Floating)
		realized = realized.Add(r.Realized)
		floating = floating.Add(r.Floating)
		capital = capital.Add(r.Investment)
		useOfCap := "—"
		if r.MaxLoss.IsPositive() {
			pct := total.Div(r.MaxLoss).Mul(decimal.NewFromInt(100)).Round(0)
			useOfCap = fmt.Sprintf("%s%%", pct.String())
		}
		age := time.Since(r.CreatedAt).Hours()
		fmt.Fprintf(&b, "%4d %-9s %dx $%-4d %8s %7s %4.1fh %d\n",
			r.BotNumber, sym(r.Symbol), r.Leverage, r.Investment.Round(0).IntPart(),
			moneySigned(total), useOfCap, age, r.Shifts)
	}
	b.WriteString("</pre>")
	b.WriteString(fmt.Sprintf("\nΣ капитал <b>$%s</b> · realized <b>%s</b> · плавающий <b>%s</b> · нетто <b>%s</b>",
		capital.Round(0).String(), moneySigned(realized), moneySigned(floating), moneySigned(realized.Add(floating))))
	return b.String()
}

func buildClosedTable(rows []closedRow, withHeld bool) string {
	if len(rows) == 0 {
		return "закрытий нет"
	}
	var b strings.Builder
	if withHeld {
		b.WriteString("<pre>  #  символ     финал   держал\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "%4d %-10s %8s %5.1fh\n", r.BotNumber, sym(r.Symbol), moneySigned(r.Final), r.HeldHours)
		}
	} else {
		b.WriteString("<pre>  #  символ     финал   причина\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "%4d %-10s %8s %s\n", r.BotNumber, sym(r.Symbol), moneySigned(r.Final), shortReason(r.Reason))
		}
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
