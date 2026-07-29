package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/autogrid"
	"github.com/aligorov/pionex-bot/backend/internal/controlplane"
	"github.com/aligorov/pionex-bot/backend/internal/database"
	"github.com/aligorov/pionex-bot/backend/internal/httpapi"
	"github.com/aligorov/pionex-bot/backend/internal/mcpserver"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	Version   = "1.1.0"
	GitCommit = "dev"
	BuildTime = "unknown"
)

const defaultDatabaseURL = "postgres://pionex:pionex_password@localhost:5432/pionex_bot?sslmode=disable"

func main() {
	baseHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	bootstrapLogger := slog.New(baseHandler)
	slog.SetDefault(bootstrapLogger)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = defaultDatabaseURL
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startupCancel()

	db, err := pgxpool.New(startupCtx, databaseURL)
	if err != nil {
		bootstrapLogger.Error("create PostgreSQL pool", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := db.Ping(startupCtx); err != nil {
		bootstrapLogger.Error("PostgreSQL is unavailable", "error", err)
		os.Exit(1)
	}
	if err := database.Migrate(startupCtx, db, migrationsDirectory()); err != nil {
		bootstrapLogger.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	logStore := observability.NewStore(db)
	dbHandler := observability.NewDBHandler(baseHandler, logStore)
	logger := slog.New(dbHandler)
	slog.SetDefault(logger)

	authService := auth.NewService(db)
	accountService := accounts.NewService(db)
	auditStore := audit.NewStore(db)
	riskEngine := risk.NewEngine(db)
	autoGridService := autogrid.NewService(db, riskEngine)
	controlService := controlplane.NewService(
		db, riskEngine, auditStore, logStore, Version, GitCommit, BuildTime,
	)
	mcpServices := mcpserver.Services{
		Auth: authService, Control: controlService, Version: Version,
	}
	api := httpapi.NewServer(
		authService, accountService, autoGridService, controlService,
		Version, GitCommit, BuildTime,
		mcpserver.NewHTTPHandler(mcpServices), frontendDirectory(), logger,
	)
	runtimeCtx, runtimeCancel := context.WithCancel(context.Background())
	defer runtimeCancel()
	autoGridWorker := autogrid.NewWorker(
		db, autoGridService, accountService, riskEngine, logger,
	)
	go autoGridWorker.Run(runtimeCtx)

	server := &http.Server{
		Addr:              ":8080",
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	logger.Info("Standalone Pionex control plane started",
		"component", "startup", "version", Version, "commit", GitCommit,
		"build_time", BuildTime, "address", server.Addr,
	)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "component", "http", "error", err)
			os.Exit(1)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	runtimeCancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "component", "shutdown", "error", err)
	}
	logger.Info("Standalone Pionex control plane stopped",
		"component", "shutdown", "dropped_db_logs", dbHandler.Dropped(),
	)
}

func migrationsDirectory() string {
	if _, err := os.Stat("/app/migrations"); err == nil {
		return "/app/migrations"
	}
	return "./migrations"
}

func frontendDirectory() string {
	if _, err := os.Stat("/app/frontend/index.html"); err == nil {
		return "/app/frontend"
	}
	return "./frontend/dist"
}
