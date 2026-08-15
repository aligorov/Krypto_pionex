package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/audit"
	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/autogrid"
	"github.com/aligorov/pionex-bot/backend/internal/controlplane"
	"github.com/aligorov/pionex-bot/backend/internal/llm"
	"github.com/aligorov/pionex-bot/backend/internal/observability"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
)

type contextKey string

const (
	principalContextKey contextKey = "principal"
	requestIDContextKey contextKey = "request_id"
	maxRequestBody                 = 1 << 20
)

type Server struct {
	auth         *auth.Service
	accounts     *accounts.Service
	autogrid     *autogrid.Service
	control      *controlplane.Service
	llm          *llm.Service
	version      string
	commit       string
	buildTime    string
	mcpHandler   http.Handler
	frontendDist string
	logger       *slog.Logger
}

func NewServer(
	authService *auth.Service,
	accountService *accounts.Service,
	autoGridService *autogrid.Service,
	controlService *controlplane.Service,
	llmService *llm.Service,
	version, commit, buildTime string,
	mcpHandler http.Handler,
	frontendDist string,
	logger *slog.Logger,
) *Server {
	return &Server{
		auth: authService, accounts: accountService,
		autogrid: autoGridService, control: controlService,
		llm: llmService,
		version: version, commit: commit, buildTime: buildTime,
		mcpHandler: mcpHandler, frontendDist: frontendDist, logger: logger,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("GET /api/version", s.apiVersion)
	mux.HandleFunc("GET /api/bootstrap/status", s.bootstrapStatus)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.Handle("POST /api/auth/logout", s.withSession(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/auth/me", s.withSession(http.HandlerFunc(s.me)))
	mux.Handle("GET /api/auth/2fa/setup", s.withSession(http.HandlerFunc(s.setup2FA)))
	mux.Handle("POST /api/auth/2fa/enable", s.withSession(http.HandlerFunc(s.enable2FA)))
	mux.Handle("POST /api/auth/2fa/disable", s.withSession(http.HandlerFunc(s.disable2FA)))

	mux.Handle("GET /api/security/bans", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.listIPBans)))
	mux.Handle("DELETE /api/security/bans/{ip}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.unbanIP)))
	mux.Handle("GET /api/security/whitelist", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.listWhitelist)))
	mux.Handle("POST /api/security/whitelist", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.addWhitelist)))
	mux.Handle("DELETE /api/security/whitelist/{id}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.removeWhitelist)))
	mux.Handle("GET /api/security/my-ip", s.withSession(http.HandlerFunc(s.myIP)))

	mux.Handle("GET /api/llm/settings", s.withSession(http.HandlerFunc(s.getLLMSettings)))
	mux.Handle("PUT /api/llm/settings", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.updateLLMSettings)))
	mux.Handle("POST /api/llm/test", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.testLLMConnection)))
	mux.Handle("POST /api/llm/models", s.withSession(http.HandlerFunc(s.listLLMModels)))
	mux.Handle("GET /api/llm/audits", s.withSession(http.HandlerFunc(s.listLLMAudits)))

	mux.Handle("GET /api/dashboard", s.withSession(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /api/users", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.listUsers)))
	mux.Handle("POST /api/users", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.createUser)))
	mux.Handle("PATCH /api/users/{id}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.updateUser)))
	mux.Handle("PUT /api/users/{id}/password", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.resetPassword)))

	mux.Handle("GET /api/me/settings", s.withSession(http.HandlerFunc(s.getMySettings)))
	mux.Handle("PUT /api/me/settings", s.withSession(http.HandlerFunc(s.updateMySettings)))
	mux.Handle("PUT /api/me/password", s.withSession(http.HandlerFunc(s.changeMyPassword)))

	mux.Handle("GET /api/config", s.withSession(http.HandlerFunc(s.listConfig)))
	mux.Handle("PUT /api/config/{key}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.setConfig)))
	mux.Handle("GET /api/feature-flags", s.withSession(http.HandlerFunc(s.listFeatureFlags)))
	mux.Handle("PUT /api/feature-flags/{name}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.setFeatureFlag)))
	mux.Handle("GET /api/risk/settings", s.withSession(http.HandlerFunc(s.getRiskSettings)))
	mux.Handle("PUT /api/risk/settings", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.updateRiskSettings)))

	mux.Handle("GET /api/accounts", s.withSession(http.HandlerFunc(s.listAccounts)))
	mux.Handle("POST /api/accounts", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.createAccount)))
	mux.Handle("PATCH /api/accounts/{id}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.updateAccount)))
	mux.Handle("DELETE /api/accounts/{id}", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.deleteAccount)))
	mux.Handle("POST /api/accounts/{id}/verify", s.withRole(auth.RoleAdmin, http.HandlerFunc(s.verifyAccount)))
	mux.Handle("GET /api/autogrid", s.withSession(http.HandlerFunc(s.autoGridState)))
	mux.Handle("PUT /api/autogrid/settings", s.withRole(auth.RoleOperator, http.HandlerFunc(s.updateAutoGridSettings)))
	mux.Handle("POST /api/autogrid/actions/{action}", s.withRole(auth.RoleOperator, http.HandlerFunc(s.autoGridAction)))
	mux.Handle("POST /api/autogrid/bots", s.withRole(auth.RoleOperator, http.HandlerFunc(s.deployManualAutoGridBot)))
	mux.Handle("PUT /api/autogrid/mode", s.withRole(auth.RoleOperator, http.HandlerFunc(s.setAutoGridMode)))
	mux.Handle("POST /api/autogrid/bots/{id}/close", s.withRole(auth.RoleOperator, http.HandlerFunc(s.closeAutoGridBot)))
	mux.Handle("POST /api/autogrid/bots/{id}/adjust", s.withRole(auth.RoleOperator, http.HandlerFunc(s.adjustAutoGridBot)))
	mux.Handle("GET /api/autogrid/ai-strategy", s.withSession(http.HandlerFunc(s.autoGridAIStrategy)))
	mux.Handle("GET /api/autogrid/presets", s.withSession(http.HandlerFunc(s.listAutoGridPresets)))
	mux.Handle("GET /api/autogrid/settings/ai-fill", s.withSession(http.HandlerFunc(s.autoGridAIFill)))
	mux.Handle("POST /api/autogrid/presets/{id}/apply", s.withRole(auth.RoleOperator, http.HandlerFunc(s.applyAutoGridPreset)))
	mux.Handle("POST /api/autogrid/paper/clear", s.withRole(auth.RoleOperator, http.HandlerFunc(s.clearPaperHistory)))
	mux.Handle("GET /api/grids", s.withSession(http.HandlerFunc(s.listGrids)))
	mux.Handle("GET /api/orders", s.withSession(http.HandlerFunc(s.listOrders)))
	mux.Handle("GET /api/logs", s.withSession(http.HandlerFunc(s.listLogs)))
	mux.Handle("GET /api/audit", s.withSession(http.HandlerFunc(s.listAudit)))

	mux.Handle("GET /api/mcp/tokens", s.withSession(http.HandlerFunc(s.listTokens)))
	mux.Handle("POST /api/mcp/tokens", s.withSession(http.HandlerFunc(s.createToken)))
	mux.Handle("DELETE /api/mcp/tokens/{id}", s.withSession(http.HandlerFunc(s.revokeToken)))
	mux.Handle("GET /api/control/commands", s.withSession(http.HandlerFunc(s.listCommands)))
	mux.Handle("POST /api/control/commands", s.withRole(auth.RoleOperator, http.HandlerFunc(s.prepareCommand)))
	mux.Handle("POST /api/control/commands/{id}/confirm", s.withRole(auth.RoleOperator, http.HandlerFunc(s.confirmCommand)))

	if s.mcpHandler != nil {
		mux.Handle("/mcp", s.mcpHandler)
	}
	mux.Handle("/", s.spaHandler())
	return s.requestMiddleware(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "time": time.Now().UTC(), "version": s.version,
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.control.Dashboard(r.Context())
	if err != nil || !dashboard.DatabaseHealthy {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) apiVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": s.version, "gitCommit": s.commit, "buildTime": s.buildTime,
	})
}

func (s *Server) bootstrapStatus(w http.ResponseWriter, r *http.Request) {
	count, err := s.auth.CountUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized":  count > 0,
		"setupCommand": "docker compose run --rm backend /app/pionex-admin create-user --username admin --display-name \"Administrator\" --role ADMIN",
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	clientIP := requestIP(r)
	if banned, until, _ := s.auth.Fail2Ban().CheckIP(r.Context(), clientIP); banned {
		retryAfterSec := int(time.Until(until).Seconds())
		if retryAfterSec < 1 {
			retryAfterSec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": fmt.Sprintf("Слишком много неудачных попыток. IP %s заблокирован до %s.", clientIP, until.Format("15:04:05")),
		})
		return
	}

	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code,omitempty"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	user, err := s.auth.Authenticate(r.Context(), input.Username, input.Password, input.Code)
	if err != nil {
		if errors.Is(err, auth.ErrTwoFactorRequired) {
			writeJSON(w, http.StatusOK, map[string]any{
				"requires2fa": true,
			})
			return
		}
		banned, until, _ := s.auth.Fail2Ban().RecordFailure(r.Context(), clientIP, input.Username, err.Error())
		if banned {
			retryAfterSec := int(time.Until(until).Seconds())
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSec))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": fmt.Sprintf("Превышен лимит попыток. IP %s заблокирован на 15 минут.", clientIP),
			})
			return
		}
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrAccountLocked) {
			status = http.StatusLocked
		}
		s.recordAuthEvent(r, input.Username, "auth.login", "DENIED")
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	_ = s.auth.Fail2Ban().RecordSuccess(r.Context(), clientIP)
	sessionToken, csrfToken, expiresAt, err := s.auth.CreateSession(
		r.Context(), user.ID, clientIP, r.UserAgent(),
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	secure := requestSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: sessionToken, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
		Expires: expiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name: auth.CSRFCookieName, Value: csrfToken, Path: "/",
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode,
		Expires: expiresAt,
	})
	principal := auth.Principal{
		UserID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Role: user.Role, ActorType: "USER",
	}
	event := audit.EventFromPrincipal(principal, "auth.login", "session", "", "SUCCESS", nil)
	s.enrichEvent(&event, r)
	_ = s.control.Audit().Record(r.Context(), event)
	writeJSON(w, http.StatusOK, map[string]any{
		"user": user, "csrfToken": csrfToken, "expiresAt": expiresAt,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if err := s.auth.RevokeSession(r.Context(), principal.SessionID); err != nil {
		s.fail(w, r, err)
		return
	}
	clearAuthCookies(w, requestSecure(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	user, err := s.auth.GetUser(r.Context(), principal.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	item, err := s.control.Dashboard(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	items, err := s.auth.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var input auth.CreateUserInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.auth.CreateUser(r.Context(), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.create", "user", item.ID, map[string]any{
		"username": item.Username, "role": item.Role,
	})
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var input auth.UpdateUserInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.auth.UpdateUser(r.Context(), r.PathValue("id"), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.update", "user", item.ID, map[string]any{
		"role": item.Role, "isActive": item.IsActive,
	})
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password           string `json:"password"`
		MustChangePassword bool   `json:"mustChangePassword"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.auth.ChangePassword(
		r.Context(), r.PathValue("id"), input.Password, input.MustChangePassword,
	); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.password.reset", "user", r.PathValue("id"), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getMySettings(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	settings, err := s.auth.GetUserSettings(r.Context(), principal.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) updateMySettings(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var settings auth.UserSettings
	if !decodeJSON(w, r, &settings) {
		return
	}
	settings.UserID = principal.UserID
	item, err := s.auth.UpdateUserSettings(r.Context(), settings)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.settings.update", "user", principal.UserID, nil)
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var input struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.auth.ChangePassword(r.Context(), principal.UserID, input.Password, false); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.password.change", "user", principal.UserID, nil)
	clearAuthCookies(w, requestSecure(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setup2FA(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	setup, err := s.auth.Setup2FA(r.Context(), principal.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, setup)
}

func (s *Server) enable2FA(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var input struct {
		Secret        string   `json:"secret"`
		Code          string   `json:"code"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.auth.Enable2FA(r.Context(), principal.UserID, input.Secret, input.Code, input.RecoveryCodes); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.2fa.enable", "user", principal.UserID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) disable2FA(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var input struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.auth.Disable2FA(r.Context(), principal.UserID, input.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "user.2fa.disable", "user", principal.UserID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) myIP(w http.ResponseWriter, r *http.Request) {
	ip := requestIP(r)
	ipStr := ""
	if ip != nil {
		ipStr = ip.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ip":          ipStr,
		"whitelisted": s.auth.Fail2Ban().IsWhitelisted(ip),
	})
}

func (s *Server) listIPBans(w http.ResponseWriter, r *http.Request) {
	bans, err := s.auth.Fail2Ban().ListBans(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bans)
}

func (s *Server) unbanIP(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	if err := s.auth.Fail2Ban().UnbanIP(r.Context(), ip); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "security.ip.unban", "ip_ban", ip, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listWhitelist(w http.ResponseWriter, r *http.Request) {
	list, err := s.auth.Fail2Ban().ListWhitelist(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) addWhitelist(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var input struct {
		IPOrCIDR    string `json:"ipOrCidr"`
		Description string `json:"description"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := s.auth.Fail2Ban().AddWhitelist(r.Context(), input.IPOrCIDR, input.Description, principal.Username); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "security.whitelist.add", "whitelist", input.IPOrCIDR, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) removeWhitelist(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid whitelist entry ID"})
		return
	}
	if err := s.auth.Fail2Ban().RemoveWhitelist(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "security.whitelist.remove", "whitelist", idStr, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listConfig(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	items, err := s.control.ListConfig(r.Context(), principal.HasRole(auth.RoleAdmin))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Value any `json:"value"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.control.SetConfig(
		r.Context(), principalFromContext(r.Context()), r.PathValue("key"), input.Value,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) listFeatureFlags(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.ListFeatureFlags(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) setFeatureFlag(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.control.SetFeatureFlag(
		r.Context(), principalFromContext(r.Context()), r.PathValue("name"), input.Enabled,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) getRiskSettings(w http.ResponseWriter, r *http.Request) {
	item, err := s.control.RiskSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateRiskSettings(w http.ResponseWriter, r *http.Request) {
	var input risk.RiskSettings
	if !decodeJSON(w, r, &input) {
		return
	}
	input.ID = 1
	item, err := s.control.UpdateRiskSettings(
		r.Context(), principalFromContext(r.Context()), input,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := s.accounts.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var input accounts.CreateInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.accounts.Create(r.Context(), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "pionex_account.create", "pionex_account", item.ID, map[string]any{
		"name":                 item.Name,
		"keyFingerprint":       item.KeyFingerprint,
		"isPaper":              item.IsPaper,
		"hasFuturesPermission": item.HasFuturesPermission,
		"hasBotPermission":     item.HasBotPermission,
	})
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	var input accounts.UpdateInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.accounts.Update(r.Context(), r.PathValue("id"), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "pionex_account.update", "pionex_account", item.ID, map[string]any{
		"name":           item.Name,
		"keyFingerprint": item.KeyFingerprint,
		"isEnabled":      item.IsEnabled,
	})
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) verifyAccount(w http.ResponseWriter, r *http.Request) {
	item, err := s.accounts.Verify(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "pionex_account.verify", "pionex_account", item.ID, map[string]any{
		"capabilityStatus": item.CapabilityStatus,
		"keyFingerprint":   item.KeyFingerprint,
	})
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if err := s.accounts.Delete(r.Context(), accountID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "pionex_account.delete", "pionex_account", accountID, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) autoGridState(w http.ResponseWriter, r *http.Request) {
	state, err := s.autogrid.State(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state.Exchange = s.autogrid.ExchangeSnapshotWith(r.Context(), s.accounts)
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) updateAutoGridSettings(w http.ResponseWriter, r *http.Request) {
	var input autogrid.UpdateSettingsInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.autogrid.UpdateSettings(r.Context(), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.settings.update", "autogrid", item.ID, map[string]any{
		"executionMode":           item.ExecutionMode,
		"accountId":               item.AccountID,
		"budgetUsdt":              item.BudgetUSDT,
		"maxActiveBots":           item.MaxActiveBots,
		"leverage":                item.Leverage,
		"stopLossMode":            item.StopLossMode,
		"smartPnlEnabled":         item.SmartPNLEnabled,
		"adaptiveLeverageEnabled": item.AdaptiveLeverageEnabled,
		"densityGridEnabled":      item.DensityGridEnabled,
	})
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) autoGridAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	action := r.PathValue("action")
	commandType := map[string]string{
		"start":          "autogrid.start",
		"scan":           "autogrid.scan",
		"stop":           "autogrid.stop",
		"emergency-stop": "autogrid.emergency_stop",
	}[action]
	if commandType == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown AutoGrid action"})
		return
	}
	settings, err := s.autogrid.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	prepared, err := s.control.PrepareCommand(
		r.Context(), principalFromContext(r.Context()), controlplane.PrepareCommandInput{
			CommandType:  commandType,
			ResourceType: "autogrid",
			ResourceID:   settings.ID,
			Arguments: map[string]any{
				"real": settings.ExecutionMode == "REAL",
			},
			IdempotencyKey: input.IdempotencyKey,
		},
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, prepared)
}

func (s *Server) setAutoGridMode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	settings, err := s.autogrid.SetExecutionMode(r.Context(), input.Mode)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.mode.set", "autogrid", settings.ID, map[string]any{
		"mode": settings.ExecutionMode,
	})
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) closeAutoGridBot(w http.ResponseWriter, r *http.Request) {
	settings, err := s.autogrid.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	source, err := s.autogrid.RequestBotClose(
		r.Context(), settings.ID, r.PathValue("id"), "MANUAL_CLOSE",
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.bot.close", "grid_bot", r.PathValue("id"), map[string]any{
		"source": source,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "source": source,
		"message": "close requested; native cancel is submitted and verified by the reconcile loop",
	})
}

func (s *Server) clearPaperHistory(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IncludeRunning bool `json:"includeRunning"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		if !decodeJSON(w, r, &input) {
			return
		}
	}
	count, err := s.autogrid.ClearPaperHistory(r.Context(), input.IncludeRunning)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.paper.clear", "paper_grid_bots", "all", map[string]any{
		"deletedCount":   count,
		"includeRunning": input.IncludeRunning,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"deletedCount": count,
	})
}

func (s *Server) autoGridAIStrategy(w http.ResponseWriter, r *http.Request) {
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "symbol query parameter is required"})
		return
	}
	strategy, lower, upper, err := s.autogrid.AIKitProposal(r.Context(), s.accounts, symbol)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"symbol": symbol,
		"advisory": map[string]any{
			"boundary": "Pionex AI Kit parameters are Spot-only; the futures proposal adapts the AI width to the live PERP price and stays operator-confirmable",
		},
		"strategy":     strategy,
		"futuresAdapted": map[string]any{
			"lower":     lower,
			"upper":     upper,
			"gridCount": strategy.GridCount,
			"note":      "width from AI Kit, centered on PERP price, clamped to +/-12.5%",
		},
	})
}

func (s *Server) autoGridAIFill(w http.ResponseWriter, r *http.Request) {
	suggestion, err := s.autogrid.AIKitSettingsFill(r.Context(), s.accounts)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, suggestion)
}

func (s *Server) listAutoGridPresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, autogrid.MarketPhasePresets())
}

func (s *Server) applyAutoGridPreset(w http.ResponseWriter, r *http.Request) {
	settings, preset, err := s.autogrid.ApplyPreset(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.preset.apply", "autogrid", settings.ID, map[string]any{
		"preset":      preset.ID,
		"title":       preset.Title,
		"leverage":    settings.Leverage,
		"pnlTarget":   settings.PnLTargetUSDT,
		"maxLoss":     settings.MaxLossUSDT,
	})
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "preset": preset})
}

func (s *Server) deployManualAutoGridBot(w http.ResponseWriter, r *http.Request) {
	var input autogrid.ManualDeployInput
	if !decodeJSON(w, r, &input) {
		return
	}
	bot, source, err := s.autogrid.DeployManualBot(r.Context(), s.accounts, input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.bot.deploy.manual", "grid_bot", bot.ID, map[string]any{
		"source":      source,
		"symbol":      bot.Symbol,
		"direction":   bot.Direction,
		"leverage":    bot.Leverage,
		"lower":       bot.LowerPrice,
		"upper":       bot.UpperPrice,
		"row":         bot.GridNum,
		"rangeSource": input.RangeSource,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"bot": bot, "source": source,
		"message": "bot deployed with operator-confirmed parameters",
	})
}

func (s *Server) adjustAutoGridBot(w http.ResponseWriter, r *http.Request) {
	var input autogrid.AdjustBotInput
	if !decodeJSON(w, r, &input) {
		return
	}
	settings, err := s.autogrid.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	source, err := s.autogrid.AdjustBot(
		r.Context(), s.accounts, settings.ID, r.PathValue("id"), input,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.bot.adjust", "grid_bot", r.PathValue("id"), map[string]any{
		"source": source, "mode": input.Mode,
		"quoteInvestment": input.QuoteInvestment,
		"lower":           input.Lower, "upper": input.Upper, "row": input.Row,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "source": source, "mode": input.Mode,
		"message": "native adjustParams submitted to Pionex",
	})
}

func (s *Server) listGrids(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.ListGridBots(r.Context(), queryLimit(r, 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.ListPatternOrders(r.Context(), queryLimit(r, 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.Logs().List(r.Context(), observability.LogFilter{
		Level: r.URL.Query().Get("level"), Component: r.URL.Query().Get("component"),
		RequestID: r.URL.Query().Get("requestId"), Search: r.URL.Query().Get("search"),
		Limit: queryLimit(r, 200),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.Audit().List(r.Context(), audit.Filter{
		Action: r.URL.Query().Get("action"), ActorID: r.URL.Query().Get("actorId"),
		ResourceType: r.URL.Query().Get("resourceType"), Outcome: r.URL.Query().Get("outcome"),
		Limit: queryLimit(r, 200),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	all := principal.HasRole(auth.RoleAdmin) && r.URL.Query().Get("all") == "true"
	items, err := s.auth.ListAPITokens(r.Context(), principal.UserID, all)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var input struct {
		Name      string     `json:"name"`
		Scopes    []string   `json:"scopes"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	for _, scope := range input.Scopes {
		if (scope == "mcp:admin" || scope == "mcp:trade") &&
			!principal.HasRole(auth.RoleAdmin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": auth.ErrForbidden.Error()})
			return
		}
	}
	token, raw, err := s.auth.CreateAPIToken(
		r.Context(), principal.UserID, input.Name, input.Scopes, input.ExpiresAt,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "mcp_token.create", "api_token", token.ID, map[string]any{
		"name": token.Name, "scopes": token.Scopes,
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": token, "secret": raw,
		"warning": "The token is shown once. Store it in a protected client secret store.",
	})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if err := s.auth.RevokeAPIToken(
		r.Context(), r.PathValue("id"), principal.UserID, principal.HasRole(auth.RoleAdmin),
	); err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "mcp_token.revoke", "api_token", r.PathValue("id"), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	items, err := s.control.ListCommands(r.Context(), queryLimit(r, 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) prepareCommand(w http.ResponseWriter, r *http.Request) {
	var input controlplane.PrepareCommandInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.control.PrepareCommand(
		r.Context(), principalFromContext(r.Context()), input,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, item)
}

func (s *Server) confirmCommand(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ConfirmationCode string `json:"confirmationCode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	item, err := s.control.ConfirmCommand(
		r.Context(), principalFromContext(r.Context()),
		r.PathValue("id"), input.ConfirmationCode,
	)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) withRole(role string, next http.Handler) http.Handler {
	return s.withSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principalFromContext(r.Context()).HasRole(role) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": auth.ErrForbidden.Error()})
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.ErrUnauthorized.Error()})
			return
		}
		principal, err := s.auth.ValidateSession(r.Context(), cookie.Value)
		if err != nil {
			clearAuthCookies(w, requestSecure(r))
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": auth.ErrUnauthorized.Error()})
			return
		}
		if mutatingMethod(r.Method) {
			csrf := r.Header.Get("X-CSRF-Token")
			if !s.auth.ValidateCSRF(*principal, csrf) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
				return
			}
		}
		ctx := context.WithValue(r.Context(), principalContextKey, *principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 64 {
			requestID = randomID()
		}
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
		ctx := context.WithValue(r.Context(), requestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
		s.logger.InfoContext(ctx, "http request",
			"component", "http", "request_id", requestID, "method", r.Method,
			"path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func (s *Server) spaHandler() http.Handler {
	absolute, err := filepath.Abs(s.frontendDist)
	if err != nil {
		return http.NotFoundHandler()
	}
	indexPath := filepath.Join(absolute, "index.html")
	if info, statErr := os.Stat(indexPath); statErr != nil || info.IsDir() {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.Dir(absolute))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
			http.NotFound(w, r)
			return
		}
		clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		requested := filepath.Join(absolute, clean)
		if info, statErr := os.Stat(requested); statErr == nil && !info.IsDir() {
			// Hashed build artifacts never change: cache them hard.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// The SPA entry must always revalidate so browsers never run a
		// stale bundle after a deploy.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		s.logger.ErrorContext(r.Context(), "request failed",
			"component", "http", "request_id", requestIDFromContext(r.Context()),
			"path", r.URL.Path, "error", err,
		)
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "query") ||
			strings.Contains(err.Error(), "database") ||
			strings.Contains(err.Error(), "scan ") ||
			strings.Contains(err.Error(), "load ") {
			status = http.StatusInternalServerError
		}
		message := err.Error()
		if status == http.StatusInternalServerError {
			message = "internal server error"
		}
		writeJSON(w, status, map[string]string{"error": message})
	}
}

func (s *Server) auditMutation(
	r *http.Request,
	action, resourceType, resourceID string,
	details map[string]any,
) {
	event := audit.EventFromPrincipal(
		principalFromContext(r.Context()), action, resourceType, resourceID, "SUCCESS",
		observability.RedactFields(details),
	)
	s.enrichEvent(&event, r)
	_ = s.control.Audit().Record(r.Context(), event)
}

func (s *Server) recordAuthEvent(r *http.Request, actor, action, outcome string) {
	event := audit.Event{
		Action: action, Actor: actor, ActorType: "USER", Outcome: outcome,
		ResourceType: "session",
	}
	s.enrichEvent(&event, r)
	_ = s.control.Audit().Record(r.Context(), event)
}

func (s *Server) enrichEvent(event *audit.Event, r *http.Request) {
	event.RequestID = requestIDFromContext(r.Context())
	if ip := requestIP(r); ip != nil {
		event.IPAddress = ip.String()
	}
	event.UserAgent = r.UserAgent()
}

func principalFromContext(ctx context.Context) auth.Principal {
	principal, _ := ctx.Value(principalContextKey).(auth.Principal)
	return principal
}

func requestIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey).(string)
	return value
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func mutatingMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut ||
		method == http.MethodPatch || method == http.MethodDelete
}

func clearAuthCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
			HttpOnly: name == auth.SessionCookieName, Secure: secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func requestSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func requestIP(r *http.Request) net.IP {
	return auth.ExtractClientIP(r)
}

func queryLimit(r *http.Request, fallback int) int {
	parsed, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func randomID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func (s *Server) getLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM service is not initialized"})
		return
	}
	settings, err := s.llm.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) updateLLMSettings(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM service is not initialized"})
		return
	}
	var input llm.Settings
	if !decodeJSON(w, r, &input) {
		return
	}
	settings, err := s.llm.UpdateSettings(r.Context(), input)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "llm.settings.update", "llm_settings", "1", map[string]any{
		"enabled":  settings.Enabled,
		"provider": settings.Provider,
		"model":    settings.Model,
	})
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) testLLMConnection(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM service is not initialized"})
		return
	}
	var input llm.Settings
	if r.Body != nil && r.ContentLength > 0 {
		_ = decodeJSON(w, r, &input)
	}
	resp, latencyMs, err := s.llm.TestConnection(r.Context(), input)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        false,
			"error":     err.Error(),
			"latencyMs": latencyMs,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"response":  resp,
		"latencyMs": latencyMs,
	})
}

func (s *Server) listLLMModels(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "LLM service is not initialized"})
		return
	}
	var input llm.Settings
	if r.Body != nil && r.ContentLength > 0 {
		_ = decodeJSON(w, r, &input)
	}
	models, err := s.llm.ListModels(r.Context(), input)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"models": models,
	})
}

func (s *Server) listLLMAudits(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	audits, err := s.llm.ListRecentAudits(r.Context(), 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, audits)
}
