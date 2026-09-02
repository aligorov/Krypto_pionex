package mcpserver

// autogrid_sql (v2.0.55): read-only SQL against the bot database, scoped to
// the analytics tables. This closes the 2026-08-31 gap — the crypto-bot
// MCP (run_sql) has been 502-dead, so every ledger/telemetry question had
// to make a round trip through a manual psql paste on the VPS.
//
// Defense in depth:
//  1. statement must be a single SELECT or WITH...SELECT;
//  2. write/DDL/DML keywords rejected anywhere in the statement
//     (SELECT INTO, FOR UPDATE, pg_sleep, dblink, ... included);
//  3. every FROM/JOIN table must be whitelisted — credential_keyring,
//     pionex_accounts, api_tokens, app_users, user_sessions, telegram/llm
//     settings and anything pg_*/information_schema are NOT reachable;
//  4. 15s query timeout + hard row cap.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SQLInput struct {
	SQL   string `json:"sql" jsonschema:"Read-only SELECT or WITH ... SELECT statement. Only whitelisted trading/telemetry tables are reachable."`
	Limit int    `json:"limit,omitempty" jsonschema:"Max rows to return, 1..2000 (default 500)."`
}

var sqlAllowedTables = map[string]bool{
	"paper_grid_bots": true, "grid_bots": true, "pattern_orders": true,
	"autogrid_candidates": true, "autogrid_scan_runs": true, "autogrid_settings": true,
	"autogrid_radar_watchlist": true,
	"bot_execution_events":     true, "bot_risk_snapshots": true, "bot_telemetry": true,
	"shadow_candidates": true, "macro_snapshots": true, "news_headlines": true, "fomc_meetings": true,
	"coingecko_snapshots": true, "funding_snapshots": true, "oi_history": true,
	"liquidation_events": true, "sentiment_snapshots": true, "economic_events": true,
	"market_derivatives_metrics": true, "market_symbols": true, "backtest_jobs": true,
	"risk_settings": true, "control_commands": true, "audit_events": true,
	"application_logs": true, "system_incidents": true,
}

var (
	sqlForbiddenRe = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|create|drop|alter|truncate|grant|revoke|copy|vacuum|analyze|reindex|call|do|set|into|nowait|pg_sleep|pg_read_file|pg_ls_dir|dblink|commit|rollback|savepoint)\b`)
	sqlTablesRe    = regexp.MustCompile(`(?i)\b(?:from|join)\s+([a-zA-Z_][a-zA-Z_0-9]*)`)
	sqlLeadingRe   = regexp.MustCompile(`(?i)^\s*(select|with)\b`)
)

func validateAnalyticsSQL(raw string) (string, error) {
	q := strings.TrimSpace(raw)
	q = strings.TrimSuffix(q, ";")
	q = strings.TrimSpace(q)
	if q == "" {
		return "", fmt.Errorf("empty query")
	}
	if !sqlLeadingRe.MatchString(q) {
		return "", fmt.Errorf("only SELECT or WITH ... SELECT statements are allowed")
	}
	if strings.Contains(q, ";") {
		return "", fmt.Errorf("multi-statement queries are not allowed")
	}
	if sqlForbiddenRe.MatchString(q) {
		return "", fmt.Errorf("statement contains a forbidden keyword (write/DDL/utility)")
	}
	// CTE names (WITH x AS (...), y AS (...)) are query-local refs, not
	// tables — collect them so the whitelist check does not false-positive.
	cteNames := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?i)(?:\bwith|,)\s*([a-zA-Z_][a-zA-Z_0-9]*)\s+as\b`).FindAllStringSubmatch(q, -1) {
		cteNames[strings.ToLower(m[1])] = true
	}
	matches := sqlTablesRe.FindAllStringSubmatch(q, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("no FROM/JOIN table found")
	}
	for _, m := range matches {
		table := strings.ToLower(m[1])
		if cteNames[table] {
			continue
		}
		if !sqlAllowedTables[table] {
			return "", fmt.Errorf("table %q is not whitelisted for analytics SQL", table)
		}
	}
	return q, nil
}

func jsonableSQLValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case driver.Valuer:
		if out, err := val.Value(); err == nil {
			return jsonableSQLValue(out)
		}
	case time.Time:
		return val.UTC().Format(time.RFC3339Nano)
	case []byte:
		return string(val)
	}
	return v
}

func runAnalyticsSQL(ctx context.Context, db *pgxpool.Pool, input SQLInput) (map[string]any, error) {
	q, err := validateAnalyticsSQL(input.SQL)
	if err != nil {
		return nil, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 2000 {
		limit = 2000
	}

	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := db.Query(qctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, f := range fields {
		columns[i] = f.Name
	}

	out := make([][]any, 0, limit)
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = jsonableSQLValue(v)
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{
		"columns": columns, "rows": out, "rowCount": len(out),
	}, nil
}

func registerSQLTool(server *mcp.Server, services Services, principal auth.Principal) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_sql",
		Description: "Run a read-only SQL query (SELECT or WITH ... SELECT only) against the bot database. " +
			"Only whitelisted trading/telemetry tables are reachable (ledger, telemetry, candidates, snapshots, events); " +
			"credentials, accounts, users, tokens and messenger/LLM settings are excluded. Rows capped, 15s timeout.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input SQLInput) (*mcp.CallToolResult, DataOutput, error) {
		data, err := runAnalyticsSQL(ctx, services.DB, input)
		if err == nil {
			recordMCP(ctx, services, principal, "sql.query", "database", "", map[string]any{
				"rowCount": data["rowCount"],
			})
		}
		return nil, DataOutput{Data: data}, err
	})
}
