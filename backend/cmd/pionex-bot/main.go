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
	Version   = "1.0.0"
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
		dbURL = "postgres://pionex:pionex_password@localhost:5432/pionex_bot?sslmode=disable"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

		// Initialize services & worker
		accountService := accounts.NewService(dbPool)
		riskEngine := risk.NewEngine(dbPool)
		autoService := autogrid.NewService(dbPool, riskEngine)
		llmService := llm.NewService(dbPool, logger)

		autoWorker := autogrid.NewWorker(dbPool, autoService, accountService, riskEngine, llmService, logger)
		go autoWorker.Run(ctx)

		// Start Telegram Outbox Dispatcher Loop
		tgToken := os.Getenv("TELEGRAM_BOT_TOKEN")
		tgChat := os.Getenv("TELEGRAM_CHAT_ID")
		dispatcher := telegram.NewOutboxDispatcher(dbPool, tgToken, tgChat)
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
		accountService := accounts.NewService(dbPool)
		riskEngine := risk.NewEngine(dbPool)
		autoService := autogrid.NewService(dbPool, riskEngine)
		llmService := llm.NewService(dbPool, logger)
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
