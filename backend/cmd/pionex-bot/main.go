package main

import (
	"context"
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
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/mcpserver"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/aligorov/pionex-bot/backend/internal/telegram"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	Version   = "1.3.7"
	GitCommit = "dev"
	BuildTime = "unknown"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	slog.Info("Starting Standalone Pionex Trading Bot",
		"version", Version,
		"commit", GitCommit,
		"build_time", BuildTime,
	)

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		// No credentials may ship inside the binary (Zero-ENV policy,
		// audit SEC-001): refuse to start instead of falling back to a
		// known default password.
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Shared services: declared here so the worker goroutine and the HTTP
	// server use the same instances (single Pionex rate budget, M1).
	var (
		accountService *accounts.Service
		riskEngine     *risk.Engine
		autoService    *autogrid.Service
		llmService     *llm.Service
	)

	dbPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		slog.Warn("Could not connect to PostgreSQL pool immediately", "error", err)
	} else {
		defer dbPool.Close()

		// Apply pending migrations so upgrades of an existing volume never run
		// against a stale schema (pionex-admin is no longer the only path).
		migrationsDir := "./migrations"
		if _, statErr := os.Stat(migrationsDir); statErr != nil {
			migrationsDir = "/app/migrations"
		}
		if migrateErr := database.Migrate(ctx, dbPool, migrationsDir); migrateErr != nil {
			slog.Error("Database migration failed", "error", migrateErr)
			os.Exit(1)
		}
		slog.Info("PostgreSQL connection pool established")

		// Initialize services & worker. One shared AutoGrid service keeps the
		// worker, HTTP API and MCP inside a single Pionex rate budget (M1).
		accountService = accounts.NewService(dbPool)
		riskEngine = risk.NewEngine(dbPool)
		autoService = autogrid.NewService(dbPool, riskEngine)
		llmService = llm.NewService(dbPool, logger)

		autoWorker := autogrid.NewWorker(dbPool, autoService, accountService, riskEngine, llmService, logger)
		go autoWorker.Run(ctx)

		// Start Telegram Outbox Dispatcher Loop. Credentials come from the
		// telegram_settings table only — Zero-ENV policy, no fallbacks.
		dispatcher := telegram.NewOutboxDispatcher(dbPool, "", "")
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_ = dispatcher.DispatchPending(ctx)
				}
			}
		}()
	}

	// Locate frontend dist directory
	frontendDist := "/app/frontend"
	if _, err := os.Stat(frontendDist); err != nil {
		frontendDist = "./frontend/dist"
	}

	var handler http.Handler
	if dbPool != nil {
		authService := auth.NewService(dbPool)
		auditStore := audit.NewStore(dbPool)
		logStore := observability.NewStore(dbPool)

		controlService := controlplane.NewService(dbPool, riskEngine, auditStore, logStore, Version, GitCommit, BuildTime)
		telegramService := telegram.NewService(dbPool, logger)

		// The MCP streamable HTTP endpoint is mounted at /mcp and authenticated
		// with scoped Bearer API tokens (same tokens as the stdio binary).
		mcpHandler := mcpserver.NewHTTPHandler(mcpserver.Services{
			Auth:     authService,
			Control:  controlService,
			AutoGrid: autoService,
			Accounts: accountService,
			Version:  Version,
		})

		apiServer := httpapi.NewServer(
			authService,
			accountService,
			autoService,
			controlService,
			llmService,
			telegramService,
			Version,
			GitCommit,
			BuildTime,
			mcpHandler,
			frontendDist,
			logger,
		)
		handler = apiServer.Handler()
	} else {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		})
		handler = mux
	}

	server := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("Server listening on :8080", "frontend", frontendDist)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Forced shutdown error", "error", err)
	}

	slog.Info("Application stopped cleanly")
}
