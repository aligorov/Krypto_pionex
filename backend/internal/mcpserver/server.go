package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/controlplane"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type contextKey string

const principalContextKey contextKey = "mcp_principal"

type Services struct {
	Auth    *auth.Service
	Control *controlplane.Service
	Version string
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
	MaxLeverage           int    `json:"maxLeverage"`
	MaxActiveGridBots     int    `json:"maxActiveGridBots"`
	MaxOpenPositions      int    `json:"maxOpenPositions"`
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
		Name: "risk_update", Description: "Atomically replace risk limits. Requires mcp:admin.",
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
		if err := current.MaxDailyLossUSD.UnmarshalText([]byte(input.MaxDailyLossUSD)); err != nil {
			return nil, DataOutput{}, errors.New("invalid maxDailyLossUsd")
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

	return server
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
