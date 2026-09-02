package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/autogrid"
	"github.com/aligorov/pionex-bot/backend/internal/controlplane"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shopspring/decimal"
)

type contextKey string

const principalContextKey contextKey = "mcp_principal"

type Services struct {
	Auth     *auth.Service
	Control  *controlplane.Service
	AutoGrid *autogrid.Service
	Accounts *accounts.Service
	DB       *pgxpool.Pool
	Version  string
}

type EmptyInput struct{}

type LimitInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"Maximum number of rows, from 1 to 500."`
}

type DataOutput struct {
	Data any `json:"data"`
}

type UserIDInput struct {
	UserID string `json:"userId,omitempty" jsonschema:"User UUID. Omit to use the token owner."`
}

type UpdateSettingsInput struct {
	UserID           string         `json:"userId,omitempty"`
	Language         string         `json:"language"`
	Timezone         string         `json:"timezone"`
	Theme            string         `json:"theme"`
	DefaultAccountID *string        `json:"defaultAccountId,omitempty"`
	Preferences      map[string]any `json:"preferences,omitempty"`
}

type UpdateUserInput struct {
	UserID      string  `json:"userId"`
	DisplayName *string `json:"displayName,omitempty"`
	Email       *string `json:"email,omitempty"`
	Role        *string `json:"role,omitempty"`
	IsActive    *bool   `json:"isActive,omitempty"`
}

type ResetPasswordInput struct {
	UserID             string `json:"userId"`
	Password           string `json:"password"`
	MustChangePassword bool   `json:"mustChangePassword"`
}

type ConfigSetInput struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

type FeatureFlagSetInput struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type RiskUpdateInput struct {
	KillSwitchEnabled     bool   `json:"killSwitchEnabled"`
	MaxAccountExposureUSD string `json:"maxAccountExposureUsd"`
	MaxSymbolExposureUSD  string `json:"maxSymbolExposureUsd"`
	MaxDailyLossUSD       string `json:"maxDailyLossUsd"`
	// BreakerMode is optional: AUTO returns the daily-loss breaker to fleet
	// design derivation; MANUAL keeps the maxDailyLossUsd pin. Empty
	// preserves the stored mode, except that supplying maxDailyLossUsd pins
	// MANUAL (an explicit operator override the derivation must respect).
	BreakerMode       string `json:"breakerMode,omitempty" jsonschema:"Optional breaker ownership: AUTO re-derives maxDailyLossUsd from the fleet design, MANUAL pins the operator value."`
	MaxLeverage       int    `json:"maxLeverage"`
	MaxActiveGridBots int    `json:"maxActiveGridBots"`
	MaxOpenPositions  int    `json:"maxOpenPositions"`
}

type LogListInput struct {
	Level     string `json:"level,omitempty"`
	Component string `json:"component,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Search    string `json:"search,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type AuditListInput struct {
	Action       string `json:"action,omitempty"`
	ActorID      string `json:"actorId,omitempty"`
	ResourceType string `json:"resourceType,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type ConfirmCommandInput struct {
	CommandID        string `json:"commandId"`
	ConfirmationCode string `json:"confirmationCode"`
}

type CreateTokenInput struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	ExpiresInH int      `json:"expiresInHours,omitempty"`
}

type RevokeTokenInput struct {
	TokenID string `json:"tokenId"`
}

func NewServer(services Services, principal auth.Principal) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "pionex-control-plane", Title: "Standalone Pionex Control Plane",
		Version: services.Version,
	}, nil)

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	writeHint := &mcp.ToolAnnotations{IdempotentHint: true}

	mcp.AddTool(server, &mcp.Tool{
		Name: "system_status", Description: "Read database, kill switch, executor and MCP status.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.Dashboard(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "users_list", Description: "List application users. Requires mcp:admin.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.ListUsers(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "users_create", Description: "Create a DB-backed application user. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input auth.CreateUserInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.CreateUser(ctx, input)
		if err == nil {
			recordMCP(ctx, services, principal, "user.create", "user", data.ID, map[string]any{
				"username": data.Username, "role": data.Role,
			})
		}
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "users_update", Description: "Update a user's display name, email, role or active state. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input UpdateUserInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.UpdateUser(ctx, input.UserID, auth.UpdateUserInput{
			DisplayName: input.DisplayName, Email: input.Email, Role: input.Role, IsActive: input.IsActive,
		})
		if err == nil {
			recordMCP(ctx, services, principal, "user.update", "user", input.UserID, map[string]any{
				"role": data.Role, "isActive": data.IsActive,
			})
		}
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "users_password_reset", Description: "Reset a user's password and revoke all sessions. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ResetPasswordInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		err := services.Auth.ChangePassword(
			ctx, input.UserID, input.Password, input.MustChangePassword,
		)
		if err == nil {
			recordMCP(ctx, services, principal, "user.password.reset", "user", input.UserID, nil)
		}
		return nil, DataOutput{Data: map[string]bool{"ok": err == nil}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "user_settings_get", Description: "Read personal settings. mcp:admin is required to read another user.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input UserIDInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		userID, err := resolveUserID(principal, input.UserID)
		if err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.GetUserSettings(ctx, userID)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "user_settings_update", Description: "Update language, timezone, theme and preferences.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input UpdateSettingsInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:write"); err != nil {
			return nil, DataOutput{}, err
		}
		userID, err := resolveUserID(principal, input.UserID)
		if err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.UpdateUserSettings(ctx, auth.UserSettings{
			UserID: userID, Language: input.Language, Timezone: input.Timezone,
			Theme: input.Theme, DefaultAccountID: input.DefaultAccountID,
			Preferences: input.Preferences,
		})
		if err == nil {
			recordMCP(ctx, services, principal, "user.settings.update", "user", userID, nil)
		}
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "config_list", Description: "List runtime configuration stored in PostgreSQL.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListConfig(ctx, principal.HasScope("mcp:admin"))
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "config_set", Description: "Update an existing DB runtime configuration key. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ConfigSetInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.SetConfig(ctx, principal, input.Key, input.Value)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "feature_flags_list", Description: "List durable feature flags.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListFeatureFlags(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "feature_flag_set", Description: "Enable or disable an existing feature flag. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input FeatureFlagSetInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.SetFeatureFlag(ctx, principal, input.Name, input.Enabled)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "risk_get", Description: "Read durable trading risk limits and kill switch state.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.RiskSettings(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "risk_update",
		Description: "Atomically replace risk limits. Requires mcp:admin. Passing maxDailyLossUsd pins " +
			"breakerMode=MANUAL (the autopilot's derivation will not touch it); pass breakerMode=AUTO to " +
			"return the breaker to fleet-design derivation.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input RiskUpdateInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		current, err := services.Control.RiskSettings(ctx)
		if err != nil {
			return nil, DataOutput{}, err
		}
		current.KillSwitchEnabled = input.KillSwitchEnabled
		current.MaxLeverage = input.MaxLeverage
		current.MaxActiveGridBots = input.MaxActiveGridBots
		current.MaxOpenPositions = input.MaxOpenPositions
		if err := current.MaxAccountExposureUSD.UnmarshalText([]byte(input.MaxAccountExposureUSD)); err != nil {
			return nil, DataOutput{}, errors.New("invalid maxAccountExposureUsd")
		}
		if err := current.MaxSymbolExposureUSD.UnmarshalText([]byte(input.MaxSymbolExposureUSD)); err != nil {
			return nil, DataOutput{}, errors.New("invalid maxSymbolExposureUsd")
		}
		maxDailyLossPinned := false
		if strings.TrimSpace(input.MaxDailyLossUSD) != "" {
			if err := current.MaxDailyLossUSD.UnmarshalText([]byte(input.MaxDailyLossUSD)); err != nil {
				return nil, DataOutput{}, errors.New("invalid maxDailyLossUsd")
			}
			maxDailyLossPinned = true
		}
		// Breaker ownership: an explicit breakerMode wins; otherwise an
		// explicit maxDailyLossUsd pins MANUAL (operator override), otherwise
		// the stored mode rides along untouched.
		switch strings.ToUpper(strings.TrimSpace(input.BreakerMode)) {
		case risk.BreakerModeAuto, risk.BreakerModeManual:
			current.BreakerMode = strings.ToUpper(strings.TrimSpace(input.BreakerMode))
		case "":
			if maxDailyLossPinned {
				current.BreakerMode = risk.BreakerModeManual
			}
		default:
			return nil, DataOutput{}, fmt.Errorf(
				"invalid breakerMode %q: must be AUTO or MANUAL", input.BreakerMode)
		}
		data, err := services.Control.UpdateRiskSettings(ctx, principal, *current)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "accounts_list", Description: "List configured Pionex accounts without exposing credentials.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListAccounts(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "grids_list", Description: "List durable native Futures Grid bot lifecycle records.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input LimitInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListGridBots(ctx, input.Limit)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "orders_list", Description: "List durable ordinary Futures order lifecycle records.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input LimitInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListPatternOrders(ctx, input.Limit)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "logs_list", Description: "Query structured application logs with redacted sensitive fields.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input LogListInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.Logs().List(ctx, observability.LogFilter{
			Level: input.Level, Component: input.Component, RequestID: input.RequestID,
			Search: input.Search, Limit: input.Limit,
		})
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "audit_list", Description: "Query the immutable operator and MCP audit trail.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AuditListInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.Audit().List(ctx, audit.Filter{
			Action: input.Action, ActorID: input.ActorID, ResourceType: input.ResourceType,
			Outcome: input.Outcome, Limit: input.Limit,
		})
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "commands_list", Description: "List prepared, confirmed, queued and completed control commands.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input LimitInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ListCommands(ctx, input.Limit)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "command_prepare",
		Description: "Prepare a kill-switch, account, scanner, grid, pattern or emergency command. Dangerous commands return a one-time confirmation code. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input controlplane.PrepareCommandInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.PrepareCommand(ctx, principal, input)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "command_confirm",
		Description: "Confirm and execute a previously prepared dangerous command within five minutes. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ConfirmCommandInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.ConfirmCommand(
			ctx, principal, input.CommandID, input.ConfirmationCode,
		)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "api_tokens_list", Description: "List MCP tokens without revealing their secrets.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Auth.ListAPITokens(ctx, principal.UserID, principal.HasScope("mcp:admin"))
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "api_token_create", Description: "Create a new MCP token. Requires mcp:admin; secret is returned once.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input CreateTokenInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		var expiresAt *time.Time
		if input.ExpiresInH > 0 {
			value := time.Now().Add(time.Duration(input.ExpiresInH) * time.Hour)
			expiresAt = &value
		}
		token, secret, err := services.Auth.CreateAPIToken(
			ctx, principal.UserID, input.Name, input.Scopes, expiresAt,
		)
		if err == nil {
			recordMCP(ctx, services, principal, "mcp_token.create", "api_token", token.ID, map[string]any{
				"name": token.Name, "scopes": token.Scopes,
			})
		}
		return nil, DataOutput{Data: map[string]any{"token": token, "secret": secret}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "api_token_revoke", Description: "Revoke an MCP token. Requires mcp:admin.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input RevokeTokenInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:admin"); err != nil {
			return nil, DataOutput{}, err
		}
		err := services.Auth.RevokeAPIToken(ctx, input.TokenID, principal.UserID, true)
		if err == nil {
			recordMCP(ctx, services, principal, "mcp_token.revoke", "api_token", input.TokenID, nil)
		}
		return nil, DataOutput{Data: map[string]bool{"ok": err == nil}}, err
	})

	registerAutoGridTools(server, services, principal, readOnly, writeHint)

	registerSQLTool(server, services, principal)

	return server
}

type AutoGridActionInput struct {
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// AutoGridSettingsUpdateInput mirrors autogrid.UpdateSettingsInput with all
// decimals as strings: MCP JSON schemas cannot express shopspring decimal
// structs, and string parsing keeps the exchange-grade precision.
type AutoGridSettingsUpdateInput struct {
	AccountID               string `json:"accountId,omitempty"`
	ExecutionMode           string `json:"executionMode"`
	BudgetUSDT              string `json:"budgetUsdt"`
	MaxActiveBots           int    `json:"maxActiveBots"`
	Leverage                int    `json:"leverage"`
	MinSharpe               string `json:"minSharpe"`
	MinEVPct                string `json:"minEvPct"`
	StopLossMode            string `json:"stopLossMode"`
	SmartPNLEnabled         bool   `json:"smartPnlEnabled"`
	AdaptiveLeverageEnabled bool   `json:"adaptiveLeverageEnabled"`
	DensityGridEnabled      bool   `json:"densityGridEnabled"`
	CandleInterval          string `json:"candleInterval"`
	LookbackCandles         int    `json:"lookbackCandles"`
	MaxSymbolsPerScan       int    `json:"maxSymbolsPerScan"`
	ScanIntervalSeconds     int    `json:"scanIntervalSeconds"`
	MinVolume24h            string `json:"minVolume24h"`
	MinVolatilityPct        string `json:"minVolatilityPct"`
	MaxVolatilityPct        string `json:"maxVolatilityPct"`
	MaxDrawdownPct          string `json:"maxDrawdownPct"`
	MinProfitFactor         string `json:"minProfitFactor"`
	FeeBps                  string `json:"feeBps"`
	SlippageBps             string `json:"slippageBps"`
	PnLTargetMode           string `json:"pnlTargetMode,omitempty"` // DYNAMIC or FIXED
	PnLTargetUSDT           string `json:"pnlTargetUsdt"`
	MaxLossUSDT             string `json:"maxLossUsdt"`
	ManageIntervalSeconds   int    `json:"manageIntervalSeconds"`
	RangeBreakBufferPct     string `json:"rangeBreakBufferPct"`
	MaxAdjustmentsPerBot    int    `json:"maxAdjustmentsPerBot"`
	AIKitEnabled            bool   `json:"aiKitEnabled"`
}

type AutoGridBotIDInput struct {
	BotID string `json:"botId"`
}

type AutoGridAIKitInput struct {
	Symbol string `json:"symbol"`
}

type AutoGridPresetApplyInput struct {
	PresetID string `json:"presetId"`
}

type AutoGridManualDeployInput struct {
	Symbol      string `json:"symbol"`
	Mode        string `json:"mode,omitempty"` // PAPER, REAL or empty (= autopilot mode)
	Direction   string `json:"direction,omitempty"`
	Leverage    int    `json:"leverage,omitempty"`
	Lower       string `json:"lower,omitempty"`
	Upper       string `json:"upper,omitempty"`
	Row         int    `json:"row,omitempty"`
	RangeSource string `json:"rangeSource,omitempty"`
}

type AutoGridBotAdjustInput struct {
	BotID           string `json:"botId"`
	Mode            string `json:"mode"`
	QuoteInvestment string `json:"quoteInvestment,omitempty"`
	Lower           string `json:"lower,omitempty"`
	Upper           string `json:"upper,omitempty"`
	Row             int    `json:"row,omitempty"`
}

// registerAutoGridTools exposes the full AutoGrid management surface: state
// with live PnL, settings, lifecycle actions through the durable command
// queue, per-bot close/adjust and the native Pionex AI Kit advisory.
func registerAutoGridTools(
	server *mcp.Server,
	services Services,
	principal auth.Principal,
	readOnly, writeHint *mcp.ToolAnnotations,
) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_state",
		Description: "Full AutoGrid state: settings, last scan candidates with regime metrics, " +
			"active bots with realized/unrealized PnL, closed bots with outcomes and the PnL summary.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.AutoGrid.State(ctx)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_settings_update",
		Description: "Update AutoGrid execution settings (stopped autopilot only): mode, budget, " +
			"per-bot PnL target and max loss, manage interval, scanner thresholds. All decimals are " +
			"strings. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridSettingsUpdateInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		update := autogrid.UpdateSettingsInput{
			ExecutionMode:           input.ExecutionMode,
			MaxActiveBots:           input.MaxActiveBots,
			Leverage:                input.Leverage,
			StopLossMode:            input.StopLossMode,
			SmartPNLEnabled:         input.SmartPNLEnabled,
			AdaptiveLeverageEnabled: input.AdaptiveLeverageEnabled,
			DensityGridEnabled:      input.DensityGridEnabled,
			CandleInterval:          input.CandleInterval,
			LookbackCandles:         input.LookbackCandles,
			MaxSymbolsPerScan:       input.MaxSymbolsPerScan,
			ScanIntervalSeconds:     input.ScanIntervalSeconds,
			ManageIntervalSeconds:   input.ManageIntervalSeconds,
			MaxAdjustmentsPerBot:    input.MaxAdjustmentsPerBot,
			AIKitEnabled:            input.AIKitEnabled,
		}
		if input.AccountID != "" {
			accountID := input.AccountID
			update.AccountID = &accountID
		}
		if input.PnLTargetMode != "" {
			mode := strings.ToUpper(strings.TrimSpace(input.PnLTargetMode))
			if mode != "DYNAMIC" && mode != "FIXED" {
				return nil, DataOutput{}, fmt.Errorf("invalid pnlTargetMode %q: must be DYNAMIC or FIXED", input.PnLTargetMode)
			}
			update.PnLTargetMode = mode
		}
		decimalFields := []struct {
			raw  string
			dest *decimal.Decimal
			name string
		}{
			{input.BudgetUSDT, &update.BudgetUSDT, "budgetUsdt"},
			{input.MinSharpe, &update.MinSharpe, "minSharpe"},
			{input.MinEVPct, &update.MinEVPct, "minEvPct"},
			{input.MinVolume24h, &update.MinVolume24h, "minVolume24h"},
			{input.MinVolatilityPct, &update.MinVolatilityPct, "minVolatilityPct"},
			{input.MaxVolatilityPct, &update.MaxVolatilityPct, "maxVolatilityPct"},
			{input.MaxDrawdownPct, &update.MaxDrawdownPct, "maxDrawdownPct"},
			{input.MinProfitFactor, &update.MinProfitFactor, "minProfitFactor"},
			{input.FeeBps, &update.FeeBps, "feeBps"},
			{input.SlippageBps, &update.SlippageBps, "slippageBps"},
			{input.PnLTargetUSDT, &update.PnLTargetUSDT, "pnlTargetUsdt"},
			{input.MaxLossUSDT, &update.MaxLossUSDT, "maxLossUsdt"},
			{input.RangeBreakBufferPct, &update.RangeBreakBufferPct, "rangeBreakBufferPct"},
		}
		for _, field := range decimalFields {
			if err := field.dest.UnmarshalText([]byte(field.raw)); err != nil {
				return nil, DataOutput{}, fmt.Errorf("invalid %s decimal %q", field.name, field.raw)
			}
		}
		data, err := services.AutoGrid.UpdateSettings(ctx, update)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.settings.update", "autogrid", data.ID, map[string]any{
				"executionMode": data.ExecutionMode, "pnlTargetUsdt": data.PnLTargetUSDT,
				"maxLossUsdt": data.MaxLossUSDT,
			})
		}
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_action",
		Description: "Run an AutoGrid lifecycle action: start, scan, stop or emergency-stop. " +
			"Goes through the durable command queue; dangerous actions return a one-time confirmation " +
			"code that must be confirmed with command_confirm. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridActionInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		commandType := map[string]string{
			"start": "autogrid.start", "scan": "autogrid.scan",
			"stop": "autogrid.stop", "emergency-stop": "autogrid.emergency_stop",
		}[input.Action]
		if commandType == "" {
			return nil, DataOutput{}, fmt.Errorf(
				"unknown action %q; expected start, scan, stop or emergency-stop", input.Action)
		}
		settings, err := services.AutoGrid.GetSettings(ctx)
		if err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.Control.PrepareCommand(ctx, principal, controlplane.PrepareCommandInput{
			CommandType:    commandType,
			ResourceType:   "autogrid",
			ResourceID:     settings.ID,
			Arguments:      map[string]any{"real": settings.ExecutionMode == "REAL"},
			IdempotencyKey: fmt.Sprintf("mcp-%s-%s-%d", commandType, settings.ID, time.Now().UnixNano()),
		})
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_bot_close",
		Description: "Close one AutoGrid bot by id. Real bots receive a durable stop intent; the " +
			"reconcile loop submits the native Pionex cancel and verifies the terminal state remotely. " +
			"Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridBotIDInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		settings, err := services.AutoGrid.GetSettings(ctx)
		if err != nil {
			return nil, DataOutput{}, err
		}
		source, err := services.AutoGrid.RequestBotClose(
			ctx, settings.ID, input.BotID, "MCP_MANUAL_CLOSE",
		)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.bot.close", "grid_bot", input.BotID, map[string]any{
				"source": source,
			})
		}
		return nil, DataOutput{Data: map[string]any{
			"ok": err == nil, "source": source,
			"message": "close requested; native cancel is submitted and verified by the reconcile loop",
		}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_bot_adjust",
		Description: "Manage a running bot through the native Pionex adjustParams endpoint: " +
			"mode=invest_in adds quoteInvestment USDT; mode=adjust_params moves the grid range " +
			"(lower, upper, optional row). Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridBotAdjustInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		adjust := autogrid.AdjustBotInput{Mode: input.Mode, Row: input.Row}
		if input.QuoteInvestment != "" {
			if err := adjust.QuoteInvestment.UnmarshalText([]byte(input.QuoteInvestment)); err != nil {
				return nil, DataOutput{}, errors.New("invalid quoteInvestment decimal")
			}
		}
		if input.Lower != "" {
			if err := adjust.Lower.UnmarshalText([]byte(input.Lower)); err != nil {
				return nil, DataOutput{}, errors.New("invalid lower decimal")
			}
		}
		if input.Upper != "" {
			if err := adjust.Upper.UnmarshalText([]byte(input.Upper)); err != nil {
				return nil, DataOutput{}, errors.New("invalid upper decimal")
			}
		}
		settings, err := services.AutoGrid.GetSettings(ctx)
		if err != nil {
			return nil, DataOutput{}, err
		}
		source, err := services.AutoGrid.AdjustBot(
			ctx, services.Accounts, settings.ID, input.BotID, adjust,
		)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.bot.adjust", "grid_bot", input.BotID, map[string]any{
				"source": source, "mode": adjust.Mode,
			})
		}
		return nil, DataOutput{Data: map[string]any{
			"ok": err == nil, "source": source, "mode": adjust.Mode,
		}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "autogrid_mode_set",
		Description: "Switch the autopilot execution mode PAPER/REAL at any time (affects newly opened bots; running bots keep their nature). REAL requires the durable gates and a verified account. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridPresetApplyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		if input.PresetID == "" {
			return nil, DataOutput{}, errors.New("presetId must carry the mode PAPER or REAL here")
		}
		settings, err := services.AutoGrid.SetExecutionMode(ctx, input.PresetID)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.mode.set", "autogrid", settings.ID, map[string]any{
				"mode": settings.ExecutionMode,
			})
		}
		return nil, DataOutput{Data: settings}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_settings_ai_fill",
		Description: "Sample the native Pionex AI Kit across the most liquid PERP pairs and derive " +
			"autopilot setting proposals (volatility band, drawdown cap, leverage, DYNAMIC targets). " +
			"Advisory only: apply them with autogrid_settings_update.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		data, err := services.AutoGrid.AIKitSettingsFill(ctx, services.Accounts)
		return nil, DataOutput{Data: data}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_presets_list",
		Description: "List the researched market-phase presets (flat range, uptrend, downtrend, " +
			"turbulence, strict sandbox) with full parameter patches and usage guidance.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		return nil, DataOutput{Data: autogrid.MarketPhasePresets()}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "autogrid_preset_apply",
		Description: "Apply a market-phase preset (stopped autopilot only). Execution mode, account and budget stay under manual operator control. Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridPresetApplyInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		settings, preset, err := services.AutoGrid.ApplyPreset(ctx, input.PresetID)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.preset.apply", "autogrid", settings.ID, map[string]any{
				"preset": preset.ID, "title": preset.Title,
			})
		}
		return nil, DataOutput{Data: map[string]any{"settings": settings, "preset": preset}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_bot_deploy",
		Description: "Open one bot with operator-confirmed parameters. Empty fields fall back to the " +
			"latest scanner recommendation. Use autogrid_ai_strategy first to get an AI-adapted range " +
			"proposal (width from the Spot AI Kit, centered on the PERP price). Requires mcp:trade.",
		Annotations: writeHint,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridManualDeployInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireWrite(ctx, services, principal, "mcp:trade"); err != nil {
			return nil, DataOutput{}, err
		}
		deploy := autogrid.ManualDeployInput{
			Symbol: input.Symbol, Mode: input.Mode, Direction: input.Direction,
			Leverage: input.Leverage, Row: input.Row, RangeSource: input.RangeSource,
		}
		if input.Lower != "" {
			if err := deploy.Lower.UnmarshalText([]byte(input.Lower)); err != nil {
				return nil, DataOutput{}, errors.New("invalid lower decimal")
			}
		}
		if input.Upper != "" {
			if err := deploy.Upper.UnmarshalText([]byte(input.Upper)); err != nil {
				return nil, DataOutput{}, errors.New("invalid upper decimal")
			}
		}
		bot, source, err := services.AutoGrid.DeployManualBot(ctx, services.Accounts, deploy)
		if err == nil {
			recordMCP(ctx, services, principal, "autogrid.bot.deploy.manual", "grid_bot", bot.ID, map[string]any{
				"source": source, "symbol": bot.Symbol, "direction": bot.Direction,
				"leverage": bot.Leverage, "rangeSource": input.RangeSource,
			})
		}
		return nil, DataOutput{Data: map[string]any{"bot": bot, "source": source}}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "autogrid_ai_strategy",
		Description: "Fetch the native Pionex AI Kit recommendation (annualized, volatility, " +
			"maxDrawDown, spot grid levels) for a PERP symbol like BTC_USDT_PERP. Advisory only: " +
			"Spot AI parameters are never applied to Futures Grid bots.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AutoGridAIKitInput) (*mcp.CallToolResult, DataOutput, error) {
		if err := requireScope(principal, "mcp:read"); err != nil {
			return nil, DataOutput{}, err
		}
		strategy, err := services.AutoGrid.AIKitStrategy(ctx, services.Accounts, input.Symbol)
		if err != nil {
			return nil, DataOutput{}, err
		}
		return nil, DataOutput{Data: map[string]any{
			"symbol": input.Symbol,
			"advisory": map[string]any{
				"boundary": "Pionex AI Kit parameters are Spot-only and are never applied to Futures Grid bots",
			},
			"strategy": strategy,
		}}, nil
	})
}

func NewHTTPHandler(services Services) http.Handler {
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		principal, ok := r.Context().Value(principalContextKey).(auth.Principal)
		if !ok {
			return nil
		}
		return NewServer(services, principal)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			expectedHTTP := "http://" + r.Host
			expectedHTTPS := "https://" + r.Host
			if origin != expectedHTTP && origin != expectedHTTPS {
				http.Error(w, "cross-origin MCP requests are not allowed", http.StatusForbidden)
				return
			}
		}
		authorization := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(authorization, "Bearer ") {
			http.Error(w, "MCP bearer token required", http.StatusUnauthorized)
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
		principal, err := services.Auth.ValidateAPIToken(r.Context(), raw)
		if err != nil {
			http.Error(w, "invalid MCP bearer token", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey, *principal)
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	return protected
}

func requireScope(principal auth.Principal, scope string) error {
	if !principal.HasScope(scope) {
		return auth.ErrForbidden
	}
	return nil
}

func requireWrite(
	ctx context.Context,
	services Services,
	principal auth.Principal,
	scope string,
) error {
	if err := requireScope(principal, scope); err != nil {
		return err
	}
	enabled, err := services.Control.MCPWritesEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return errors.New("MCP writes are disabled in app_config")
	}
	return nil
}

func resolveUserID(principal auth.Principal, requested string) (string, error) {
	if requested == "" || requested == principal.UserID {
		return principal.UserID, nil
	}
	if !principal.HasScope("mcp:admin") {
		return "", auth.ErrForbidden
	}
	return requested, nil
}

func recordMCP(
	ctx context.Context,
	services Services,
	principal auth.Principal,
	action, resourceType, resourceID string,
	details map[string]any,
) {
	_ = services.Control.Audit().Record(ctx, audit.EventFromPrincipal(
		principal, action, resourceType, resourceID, "SUCCESS",
		observability.RedactFields(details),
	))
}
