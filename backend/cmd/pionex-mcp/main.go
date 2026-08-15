package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/autogrid"
	"github.com/aligorov/pionex-bot/backend/internal/controlplane"
	"github.com/aligorov/pionex-bot/backend/internal/database"
	"github.com/aligorov/pionex-bot/backend/internal/mcpserver"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var Version = "1.1.0"

const defaultDatabaseURL = "postgres://pionex:pionex_password@localhost:5432/pionex_bot?sslmode=disable"

func main() {
	tokenFile := flag.String("token-file", "", "path to a file containing one MCP API token")
	flag.Parse()
	if *tokenFile == "" {
		exitError(errors.New("--token-file is required"))
	}
	rawToken, err := os.ReadFile(*tokenFile)
	if err != nil {
		exitError(fmt.Errorf("read token file: %w", err))
	}
	token := strings.TrimSpace(string(rawToken))

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}
	db, err := pgxpool.New(startupCtx, databaseURL)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	if err := db.Ping(startupCtx); err != nil {
		exitError(err)
	}
	directory := "./migrations"
	if _, statErr := os.Stat("/app/migrations"); statErr == nil {
		directory = "/app/migrations"
	}
	if err := database.Migrate(startupCtx, db, directory); err != nil {
		exitError(err)
	}

	authService := auth.NewService(db)
	principal, err := authService.ValidateAPIToken(startupCtx, token)
	if err != nil {
		exitError(errors.New("invalid MCP token"))
	}
	logStore := observability.NewStore(db)
	riskEngine := risk.NewEngine(db)
	controlService := controlplane.NewService(
		db, riskEngine, audit.NewStore(db), logStore, Version, "stdio", "runtime",
	)
	server := mcpserver.NewServer(mcpserver.Services{
		Auth:     authService,
		Control:  controlService,
		AutoGrid: autogrid.NewService(db, riskEngine),
		Accounts: accounts.NewService(db),
		Version:  Version,
	}, *principal)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		exitError(err)
	}
}

func exitError(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
