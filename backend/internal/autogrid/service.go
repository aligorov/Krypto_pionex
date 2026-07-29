package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/aligorov/pionex-bot/backend/internal/risk"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

const DefaultScope = "default"

type Settings struct {
	ID                      string          `json:"id"`
	AccountID               *string         `json:"accountId"`
	Status                  string          `json:"status"`
	ExecutionMode           string          `json:"executionMode"`
	BudgetUSDT              decimal.Decimal `json:"budgetUsdt"`
	MaxActiveBots           int             `json:"maxActiveBots"`
	Leverage                int             `json:"leverage"`
	MinSharpe               decimal.Decimal `json:"minSharpe"`
	MinEVPct                decimal.Decimal `json:"minEvPct"`
	StopLossMode            string          `json:"stopLossMode"`
	SmartPNLEnabled         bool            `json:"smartPnlEnabled"`
	AdaptiveLeverageEnabled bool            `json:"adaptiveLeverageEnabled"`
	DensityGridEnabled      bool            `json:"densityGridEnabled"`
	CandleInterval          string          `json:"candleInterval"`
	LookbackCandles         int             `json:"lookbackCandles"`
	MaxSymbolsPerScan       int             `json:"maxSymbolsPerScan"`
	ScanIntervalSeconds     int             `json:"scanIntervalSeconds"`
	MinVolume24h            decimal.Decimal `json:"minVolume24h"`
	MinVolatilityPct        decimal.Decimal `json:"minVolatilityPct"`
	MaxVolatilityPct        decimal.Decimal `json:"maxVolatilityPct"`
	MaxDrawdownPct          decimal.Decimal `json:"maxDrawdownPct"`
	MinProfitFactor         decimal.Decimal `json:"minProfitFactor"`
	FeeBps                  decimal.Decimal `json:"feeBps"`
	SlippageBps             decimal.Decimal `json:"slippageBps"`
	LastError               *string         `json:"lastError"`
	LastStartedAt           *time.Time      `json:"lastStartedAt"`
	LastStoppedAt           *time.Time      `json:"lastStoppedAt"`
	CreatedAt               time.Time       `json:"createdAt"`
	UpdatedAt               time.Time       `json:"updatedAt"`
}

type UpdateSettingsInput struct {
	AccountID               *string         `json:"accountId"`
	ExecutionMode           string          `json:"executionMode"`
	BudgetUSDT              decimal.Decimal `json:"budgetUsdt"`
	MaxActiveBots           int             `json:"maxActiveBots"`
	Leverage                int             `json:"leverage"`
	MinSharpe               decimal.Decimal `json:"minSharpe"`
	MinEVPct                decimal.Decimal `json:"minEvPct"`
	StopLossMode            string          `json:"stopLossMode"`
	SmartPNLEnabled         bool            `json:"smartPnlEnabled"`
	AdaptiveLeverageEnabled bool            `json:"adaptiveLeverageEnabled"`
	DensityGridEnabled      bool            `json:"densityGridEnabled"`
	CandleInterval          string          `json:"candleInterval"`
	LookbackCandles         int             `json:"lookbackCandles"`
	MaxSymbolsPerScan       int             `json:"maxSymbolsPerScan"`
	ScanIntervalSeconds     int             `json:"scanIntervalSeconds"`
	MinVolume24h            decimal.Decimal `json:"minVolume24h"`
	MinVolatilityPct        decimal.Decimal `json:"minVolatilityPct"`
	MaxVolatilityPct        decimal.Decimal `json:"maxVolatilityPct"`
	MaxDrawdownPct          decimal.Decimal `json:"maxDrawdownPct"`
	MinProfitFactor         decimal.Decimal `json:"minProfitFactor"`
	FeeBps                  decimal.Decimal `json:"feeBps"`
	SlippageBps             decimal.Decimal `json:"slippageBps"`
}

type ScanRun struct {
	ID              string     `json:"id"`
	Status          string     `json:"status"`
	CandidatesFound int        `json:"candidatesFound"`
	ErrorMessage    *string    `json:"errorMessage"`
	StartedAt       time.Time  `json:"startedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
}

type Candidate struct {
	ID                  string           `json:"id"`
	Symbol              string           `json:"symbol"`
	CurrentPrice        decimal.Decimal  `json:"currentPrice"`
	VolatilityPct       decimal.Decimal  `json:"volatilityPct"`
	Volume24h           decimal.Decimal  `json:"volume24h"`
	FundingRate         *decimal.Decimal `json:"fundingRate"`
	ExpectedValuePct    decimal.Decimal  `json:"expectedValuePct"`
	Sharpe              decimal.Decimal  `json:"sharpe"`
	Sortino             decimal.Decimal  `json:"sortino"`
	MaxDrawdownPct      decimal.Decimal  `json:"maxDrawdownPct"`
	WinRatePct          decimal.Decimal  `json:"winRatePct"`
	ProfitFactor        decimal.Decimal  `json:"profitFactor"`
	TurnoverProxy       decimal.Decimal  `json:"turnoverProxy"`
	Score               decimal.Decimal  `json:"score"`
	Decision            string           `json:"decision"`
	RejectionReason     *string          `json:"rejectionReason"`
	LowerPrice          decimal.Decimal  `json:"lowerPrice"`
	UpperPrice          decimal.Decimal  `json:"upperPrice"`
	GridNum             int              `json:"gridNum"`
	RecommendedLeverage int              `json:"recommendedLeverage"`
	RecommendedTrend    string           `json:"recommendedTrend"`
	ModelAssumptions    map[string]any   `json:"modelAssumptions"`
	CreatedAt           time.Time        `json:"createdAt"`
}

type ActiveBot struct {
	ID                  string           `json:"id"`
	Source              string           `json:"source"`
	AccountID           *string          `json:"accountId"`
	BUOrderID           *string          `json:"buOrderId"`
	Symbol              string           `json:"symbol"`
	Status              string           `json:"status"`
	Direction           string           `json:"direction"`
	GridType            string           `json:"gridType"`
	LowerPrice          decimal.Decimal  `json:"lowerPrice"`
	UpperPrice          decimal.Decimal  `json:"upperPrice"`
	GridNum             int              `json:"gridNum"`
	Leverage            int              `json:"leverage"`
	QuoteInvestment     decimal.Decimal  `json:"quoteInvestment"`
	RealizedPNLUSDT     *decimal.Decimal `json:"realizedPnlUsdt"`
	UnrealizedPNLUSDT   *decimal.Decimal `json:"unrealizedPnlUsdt"`
	ReconciliationState string           `json:"reconciliationState"`
	UpdatedAt           time.Time        `json:"updatedAt"`
}

type State struct {
	Settings            Settings          `json:"settings"`
	LastScan            *ScanRun          `json:"lastScan"`
	Candidates          []Candidate       `json:"candidates"`
	ActiveBots          []ActiveBot       `json:"activeBots"`
	MetricDefinitions   map[string]string `json:"metricDefinitions"`
	FeatureAvailability map[string]string `json:"featureAvailability"`
}

type Service struct {
	db   *pgxpool.Pool
	risk *risk.Engine
}

func NewService(db *pgxpool.Pool, riskEngine *risk.Engine) *Service {
	return &Service{db: db, risk: riskEngine}
}

func (s *Service) GetSettings(ctx context.Context) (*Settings, error) {
	var item Settings
	err := s.db.QueryRow(ctx, `
		SELECT id, account_id, status, execution_mode, budget_usdt,
		       max_active_bots, leverage, min_sharpe, min_ev_pct,
		       stop_loss_mode, smart_pnl_enabled, adaptive_leverage_enabled,
		       density_grid_enabled, candle_interval, lookback_candles,
		       max_symbols_per_scan, scan_interval_seconds, min_volume_24h,
		       min_volatility_pct, max_volatility_pct, max_drawdown_pct,
		       min_profit_factor, fee_bps, slippage_bps, last_error,
		       last_started_at, last_stopped_at, created_at, updated_at
		FROM autogrid_settings WHERE scope_key = $1
	`, DefaultScope).Scan(settingsScanTargets(&item)...)
	if err != nil {
		return nil, fmt.Errorf("load AutoGrid settings: %w", err)
	}
	return &item, nil
}

func (s *Service) UpdateSettings(
	ctx context.Context,
	input UpdateSettingsInput,
) (*Settings, error) {
	current, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if current.Status != "STOPPED" && current.Status != "EMERGENCY_STOPPED" {
		return nil, errors.New("stop AutoGrid before changing execution settings")
	}
	if err := s.validateSettings(ctx, input); err != nil {
		return nil, err
	}
	accountID := input.AccountID
	if accountID != nil && strings.TrimSpace(*accountID) == "" {
		accountID = nil
	}
	_, err = s.db.Exec(ctx, `
		UPDATE autogrid_settings
		SET account_id = $2, execution_mode = $3, budget_usdt = $4,
		    max_active_bots = $5, leverage = $6, min_sharpe = $7,
		    min_ev_pct = $8, stop_loss_mode = $9, smart_pnl_enabled = $10,
		    adaptive_leverage_enabled = $11, density_grid_enabled = $12,
		    candle_interval = $13, lookback_candles = $14,
		    max_symbols_per_scan = $15, scan_interval_seconds = $16,
		    min_volume_24h = $17, min_volatility_pct = $18,
		    max_volatility_pct = $19, max_drawdown_pct = $20,
		    min_profit_factor = $21, fee_bps = $22, slippage_bps = $23,
		    last_error = NULL, updated_at = NOW()
		WHERE scope_key = $1
	`, DefaultScope, accountID, input.ExecutionMode, input.BudgetUSDT,
		input.MaxActiveBots, input.Leverage, input.MinSharpe, input.MinEVPct,
		input.StopLossMode, input.SmartPNLEnabled,
		input.AdaptiveLeverageEnabled, input.DensityGridEnabled,
		input.CandleInterval, input.LookbackCandles, input.MaxSymbolsPerScan,
		input.ScanIntervalSeconds, input.MinVolume24h, input.MinVolatilityPct,
		input.MaxVolatilityPct, input.MaxDrawdownPct, input.MinProfitFactor,
		input.FeeBps, input.SlippageBps)
	if err != nil {
		return nil, fmt.Errorf("update AutoGrid settings: %w", err)
	}
	return s.GetSettings(ctx)
}

func (s *Service) State(ctx context.Context) (*State, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	state := &State{
		Settings:   *settings,
		Candidates: make([]Candidate, 0),
		ActiveBots: make([]ActiveBot, 0),
		MetricDefinitions: map[string]string{
			"expectedValuePct": "Средний модельный net-return на свечу после заданных fee/slippage.",
			"maxDrawdownPct":   "Максимальная просадка модельной equity curve.",
			"sharpe":           "Годовой Sharpe модельных доходностей без безрисковой ставки.",
			"sortino":          "Годовой Sortino по отрицательным модельным доходностям.",
			"winRatePct":       "Доля положительных модельных интервалов.",
			"profitFactor":     "Сумма прибылей / абсолютная сумма убытков.",
			"turnoverProxy":    "Оценка числа исполнений на интервал; не биржевой turnover.",
		},
		FeatureAvailability: map[string]string{
			"adaptiveStopLoss": "NATIVE_CREATE_PRICE_STOP",
			"smartPnl":         "NATIVE_CREATE_TAKE_PROFIT",
			"adaptiveLeverage": "SCANNER_RISK_CAPPED",
			"densityGrid":      "PIONEX_GEOMETRIC_GRID",
			"fundingRate":      "NOT_INCLUDED_NO_OFFICIAL_SCANNER_CALL",
		},
	}
	var scan ScanRun
	err = s.db.QueryRow(ctx, `
		SELECT id, status, candidates_found, error_message, started_at, completed_at
		FROM autogrid_scan_runs
		WHERE settings_id = $1
		ORDER BY started_at DESC LIMIT 1
	`, settings.ID).Scan(
		&scan.ID, &scan.Status, &scan.CandidatesFound, &scan.ErrorMessage,
		&scan.StartedAt, &scan.CompletedAt,
	)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load AutoGrid last scan: %w", err)
	}
	if err == nil {
		state.LastScan = &scan
		state.Candidates, err = s.listCandidates(ctx, scan.ID)
		if err != nil {
			return nil, err
		}
	}
	state.ActiveBots, err = s.listActiveBots(ctx, settings.ID)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (s *Service) BeginScan(
	ctx context.Context,
	settingsID, requestedBy string,
) (string, error) {
	var scanID string
	err := s.db.QueryRow(ctx, `
		INSERT INTO autogrid_scan_runs (settings_id, requested_by, status)
		VALUES ($1, NULLIF($2, '')::UUID, 'RUNNING')
		RETURNING id
	`, settingsID, requestedBy).Scan(&scanID)
	if err != nil {
		return "", fmt.Errorf("begin AutoGrid scan: %w", err)
	}
	return scanID, nil
}

func (s *Service) CompleteScan(
	ctx context.Context,
	scanID string,
	items []marketdata.ScannerCandidate,
) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin scan persistence: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, item := range items {
		var reason *string
		if item.RejectionReason != "" {
			value := item.RejectionReason
			reason = &value
		}
		assumptions, err := json.Marshal(item.ModelAssumptions)
		if err != nil {
			return fmt.Errorf("marshal scanner model assumptions: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO autogrid_candidates (
				scan_id, symbol, volatility, volume_24h, funding_rate,
				ev_pct, sharpe, decision, rejection_reason, current_price,
				score, lower_price, upper_price, grid_num, recommended_leverage,
				recommended_trend, max_drawdown_pct, sortino, win_rate_pct, profit_factor,
				turnover_proxy, model_assumptions
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22
			)
		`, scanID, item.Symbol, item.VolatilityPct, item.Volume24h,
			item.FundingRate, item.ExpectedValuePct, item.Sharpe,
			item.Decision, reason, item.Price, item.Score, item.LowerPrice,
			item.UpperPrice, item.GridNum, item.RecommendedLeverage,
			item.RecommendedTrend, item.MaxDrawdownPct, item.Sortino, item.WinRatePct,
			item.ProfitFactor, item.TurnoverProxy, assumptions)
		if err != nil {
			return fmt.Errorf("persist AutoGrid candidate %s: %w", item.Symbol, err)
		}
	}
	_, err = tx.Exec(ctx, `
		UPDATE autogrid_scan_runs
		SET status = 'SUCCEEDED', candidates_found = $2,
		    completed_at = NOW(), error_message = NULL
		WHERE id = $1
	`, scanID, len(items))
	if err != nil {
		return fmt.Errorf("complete AutoGrid scan: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Service) FailScan(ctx context.Context, scanID string, scanErr error) {
	_, _ = s.db.Exec(ctx, `
		UPDATE autogrid_scan_runs
		SET status = 'FAILED', error_message = $2, completed_at = NOW()
		WHERE id = $1
	`, scanID, scanErr.Error())
}

func (s *Service) SetStatus(
	ctx context.Context,
	status string,
	statusErr error,
) error {
	var message *string
	if statusErr != nil {
		value := statusErr.Error()
		message = &value
	}
	_, err := s.db.Exec(ctx, `
		UPDATE autogrid_settings
		SET status = $2::VARCHAR, last_error = $3::TEXT,
		    last_started_at = CASE WHEN $2::TEXT = 'RUNNING' THEN NOW() ELSE last_started_at END,
		    last_stopped_at = CASE WHEN $2::TEXT IN ('STOPPED', 'EMERGENCY_STOPPED') THEN NOW() ELSE last_stopped_at END,
		    updated_at = NOW()
		WHERE scope_key = $1
	`, DefaultScope, status, message)
	if err != nil {
		return fmt.Errorf("set AutoGrid status: %w", err)
	}
	return nil
}

func (s *Service) scannerConfig(settings Settings) marketdata.ScanConfig {
	return marketdata.ScanConfig{
		Interval:            settings.CandleInterval,
		LookbackCandles:     settings.LookbackCandles,
		MaxSymbols:          settings.MaxSymbolsPerScan,
		MinVolume24h:        settings.MinVolume24h,
		MinVolatilityPct:    decimalFloat(settings.MinVolatilityPct),
		MaxVolatilityPct:    decimalFloat(settings.MaxVolatilityPct),
		MinExpectedValuePct: decimalFloat(settings.MinEVPct),
		MinSharpe:           decimalFloat(settings.MinSharpe),
		MaxDrawdownPct:      decimalFloat(settings.MaxDrawdownPct),
		MinProfitFactor:     decimalFloat(settings.MinProfitFactor),
		FeeBps:              decimalFloat(settings.FeeBps),
		SlippageBps:         decimalFloat(settings.SlippageBps),
		BaseLeverage:        settings.Leverage,
		AdaptiveLeverage:    settings.AdaptiveLeverageEnabled,
		GridType:            mapGridType(settings.DensityGridEnabled),
	}
}

func (s *Service) validateSettings(
	ctx context.Context,
	input UpdateSettingsInput,
) error {
	if input.ExecutionMode != "PAPER" && input.ExecutionMode != "REAL" {
		return errors.New("execution mode must be PAPER or REAL")
	}
	if input.ExecutionMode == "REAL" && (input.AccountID == nil || strings.TrimSpace(*input.AccountID) == "") {
		return errors.New("a verified Pionex account is required for REAL mode")
	}
	if input.BudgetUSDT.LessThanOrEqual(decimal.Zero) {
		return errors.New("budget per bot must be greater than zero")
	}
	if input.MaxActiveBots < 1 || input.MaxActiveBots > 20 {
		return errors.New("max active bots must be between 1 and 20")
	}
	if input.Leverage < 1 || input.Leverage > 100 {
		return errors.New("leverage must be between 1 and 100")
	}
	if input.StopLossMode != "NONE" && input.StopLossMode != "ADAPTIVE_ATR" {
		return errors.New("unsupported stop-loss mode")
	}
	switch input.CandleInterval {
	case "1M", "5M", "15M", "30M", "60M", "4H", "8H", "12H", "1D":
	default:
		return errors.New("unsupported Pionex candle interval")
	}
	if input.LookbackCandles < 30 || input.LookbackCandles > 500 {
		return errors.New("lookback candles must be between 30 and 500")
	}
	if input.MaxSymbolsPerScan < 1 || input.MaxSymbolsPerScan > 50 {
		return errors.New("max symbols per scan must be between 1 and 50")
	}
	if input.ScanIntervalSeconds < 60 || input.ScanIntervalSeconds > 86400 {
		return errors.New("scan interval must be between 60 and 86400 seconds")
	}
	if input.MinVolume24h.IsNegative() || input.MinVolatilityPct.IsNegative() ||
		input.MaxVolatilityPct.LessThanOrEqual(input.MinVolatilityPct) ||
		input.MaxDrawdownPct.LessThanOrEqual(decimal.Zero) ||
		input.MinProfitFactor.IsNegative() || input.FeeBps.IsNegative() ||
		input.SlippageBps.IsNegative() {
		return errors.New("invalid scanner risk thresholds")
	}
	riskSettings, err := s.risk.LoadSettings(ctx)
	if err != nil {
		return err
	}
	if input.Leverage > riskSettings.MaxLeverage {
		return fmt.Errorf("leverage %d exceeds durable risk limit %d", input.Leverage, riskSettings.MaxLeverage)
	}
	if input.MaxActiveBots > riskSettings.MaxActiveGridBots {
		return fmt.Errorf(
			"max active bots %d exceeds durable risk limit %d",
			input.MaxActiveBots, riskSettings.MaxActiveGridBots,
		)
	}
	if input.BudgetUSDT.GreaterThan(riskSettings.MaxSymbolExposureUSD) {
		return fmt.Errorf(
			"budget per bot %s exceeds per-symbol exposure %s",
			input.BudgetUSDT, riskSettings.MaxSymbolExposureUSD,
		)
	}
	total := input.BudgetUSDT.Mul(decimal.NewFromInt(int64(input.MaxActiveBots)))
	if total.GreaterThan(riskSettings.MaxAccountExposureUSD) {
		return fmt.Errorf(
			"total AutoGrid budget %s exceeds account exposure %s",
			total, riskSettings.MaxAccountExposureUSD,
		)
	}
	if input.ExecutionMode == "REAL" {
		var enabled, readPermission, futuresPermission, botPermission bool
		err := s.db.QueryRow(ctx, `
			SELECT is_enabled, has_read_permission,
			       has_futures_permission, has_bot_permission
			FROM pionex_accounts WHERE id = $1
		`, *input.AccountID).Scan(
			&enabled, &readPermission, &futuresPermission, &botPermission,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("selected Pionex account does not exist")
		}
		if err != nil {
			return fmt.Errorf("validate Pionex account: %w", err)
		}
		if !enabled || !readPermission || !futuresPermission || !botPermission {
			return errors.New("selected Pionex account is not enabled and verified for declared Futures/Bot permissions")
		}
	}
	return nil
}

func (s *Service) listCandidates(ctx context.Context, scanID string) ([]Candidate, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, symbol, COALESCE(current_price, 0), COALESCE(volatility, 0),
		       COALESCE(volume_24h, 0), funding_rate, COALESCE(ev_pct, 0),
		       COALESCE(sharpe, 0), COALESCE(sortino, 0),
		       COALESCE(max_drawdown_pct, 0), COALESCE(win_rate_pct, 0),
		       COALESCE(profit_factor, 0), COALESCE(turnover_proxy, 0),
		       COALESCE(score, 0), decision, rejection_reason,
		       COALESCE(lower_price, 0), COALESCE(upper_price, 0),
		       COALESCE(grid_num, 0), COALESCE(recommended_leverage, 1),
		       COALESCE(recommended_trend, 'no_trend'), model_assumptions, created_at
		FROM autogrid_candidates
		WHERE scan_id = $1
		ORDER BY (decision = 'ACCEPTED') DESC, score DESC NULLS LAST, symbol
	`, scanID)
	if err != nil {
		return nil, fmt.Errorf("list AutoGrid candidates: %w", err)
	}
	defer rows.Close()
	items := make([]Candidate, 0)
	for rows.Next() {
		var item Candidate
		if err := rows.Scan(
			&item.ID, &item.Symbol, &item.CurrentPrice, &item.VolatilityPct,
			&item.Volume24h, &item.FundingRate, &item.ExpectedValuePct,
			&item.Sharpe, &item.Sortino, &item.MaxDrawdownPct,
			&item.WinRatePct, &item.ProfitFactor, &item.TurnoverProxy,
			&item.Score, &item.Decision, &item.RejectionReason,
			&item.LowerPrice, &item.UpperPrice, &item.GridNum,
			&item.RecommendedLeverage, &item.RecommendedTrend,
			&item.ModelAssumptions, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan AutoGrid candidate: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Service) listActiveBots(ctx context.Context, settingsID string) ([]ActiveBot, error) {
	items := make([]ActiveBot, 0)
	rows, err := s.db.Query(ctx, `
		SELECT id, account_id, bu_order_id, symbol, status, direction, grid_type,
		       lower_price, upper_price, grid_num, leverage, quote_investment,
		       reconciliation_state, updated_at
		FROM grid_bots
		WHERE autogrid_settings_id = $1
		  AND status NOT IN ('STOPPED', 'CANCELLED', 'COMPLETED', 'LIQUIDATED', 'FAILED')
		ORDER BY created_at DESC
	`, settingsID)
	if err != nil {
		return nil, fmt.Errorf("list real AutoGrid bots: %w", err)
	}
	for rows.Next() {
		var item ActiveBot
		item.Source = "REAL"
		if err := rows.Scan(
			&item.ID, &item.AccountID, &item.BUOrderID, &item.Symbol,
			&item.Status, &item.Direction, &item.GridType, &item.LowerPrice,
			&item.UpperPrice, &item.GridNum, &item.Leverage,
			&item.QuoteInvestment, &item.ReconciliationState, &item.UpdatedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan real AutoGrid bot: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = s.db.Query(ctx, `
		SELECT id, symbol, status, direction, grid_type, lower_price,
		       upper_price, grid_num, leverage, quote_investment,
		       realized_pnl_usdt, unrealized_pnl_usdt, updated_at
		FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
		ORDER BY opened_at DESC
	`, settingsID)
	if err != nil {
		return nil, fmt.Errorf("list paper AutoGrid bots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item ActiveBot
		item.Source = "PAPER"
		item.ReconciliationState = "SIMULATION"
		if err := rows.Scan(
			&item.ID, &item.Symbol, &item.Status, &item.Direction,
			&item.GridType, &item.LowerPrice, &item.UpperPrice,
			&item.GridNum, &item.Leverage, &item.QuoteInvestment,
			&item.RealizedPNLUSDT, &item.UnrealizedPNLUSDT, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan paper AutoGrid bot: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func settingsScanTargets(item *Settings) []any {
	return []any{
		&item.ID, &item.AccountID, &item.Status, &item.ExecutionMode,
		&item.BudgetUSDT, &item.MaxActiveBots, &item.Leverage,
		&item.MinSharpe, &item.MinEVPct, &item.StopLossMode,
		&item.SmartPNLEnabled, &item.AdaptiveLeverageEnabled,
		&item.DensityGridEnabled, &item.CandleInterval,
		&item.LookbackCandles, &item.MaxSymbolsPerScan,
		&item.ScanIntervalSeconds, &item.MinVolume24h,
		&item.MinVolatilityPct, &item.MaxVolatilityPct,
		&item.MaxDrawdownPct, &item.MinProfitFactor, &item.FeeBps,
		&item.SlippageBps, &item.LastError, &item.LastStartedAt,
		&item.LastStoppedAt, &item.CreatedAt, &item.UpdatedAt,
	}
}

func decimalFloat(value decimal.Decimal) float64 {
	result, _ := value.Float64()
	return result
}

func mapGridType(density bool) string {
	if density {
		return "geometric"
	}
	return "arithmetic"
}
