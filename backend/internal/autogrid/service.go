package autogrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/accounts"
	"github.com/aligorov/pionex-bot/backend/internal/grid"
	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/aligorov/pionex-bot/backend/internal/pionex"
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
	PnLTargetMode           string          `json:"pnlTargetMode"` // DYNAMIC or FIXED
	PnLTargetUSDT           decimal.Decimal `json:"pnlTargetUsdt"` // FIXED mode amounts
	MaxLossUSDT             decimal.Decimal `json:"maxLossUsdt"`
	ManageIntervalSeconds   int             `json:"manageIntervalSeconds"`
	RangeBreakBufferPct     decimal.Decimal `json:"rangeBreakBufferPct"`
	MaxAdjustmentsPerBot    int             `json:"maxAdjustmentsPerBot"`
	AIKitEnabled            bool            `json:"aiKitEnabled"`
	AIAutotuneEnabled       bool            `json:"aiAutotuneEnabled"`
	AIAutotuneInterval      int             `json:"aiAutotuneIntervalSeconds"`
	LastAutotuneAt          *time.Time      `json:"lastAutotuneAt"`
	LastAutotuneNotes       *string         `json:"lastAutotuneNotes"`
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
	PnLTargetMode           string          `json:"pnlTargetMode"` // DYNAMIC or FIXED
	PnLTargetUSDT           decimal.Decimal `json:"pnlTargetUsdt"` // FIXED mode amounts
	MaxLossUSDT             decimal.Decimal `json:"maxLossUsdt"`
	ManageIntervalSeconds   int             `json:"manageIntervalSeconds"`
	RangeBreakBufferPct     decimal.Decimal `json:"rangeBreakBufferPct"`
	MaxAdjustmentsPerBot    int             `json:"maxAdjustmentsPerBot"`
	AIKitEnabled            bool            `json:"aiKitEnabled"`
	AIAutotuneEnabled       bool            `json:"aiAutotuneEnabled"`
	AIAutotuneInterval      int             `json:"aiAutotuneIntervalSeconds"`
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
	AdjustmentsCount    int              `json:"adjustmentsCount"`
	PnLTargetUSDT       *decimal.Decimal `json:"pnlTargetUsdt"`
	MaxLossUSDT         *decimal.Decimal `json:"maxLossUsdt"`
	LeverageReason      string           `json:"leverageReason,omitempty"`
	LeverageMode        string           `json:"leverageMode,omitempty"`
	BaseLeverage        int              `json:"baseLeverage,omitempty"`
	UpdatedAt           time.Time        `json:"updatedAt"`
}

type ClosedBot struct {
	ID              string           `json:"id"`
	Source          string           `json:"source"`
	Symbol          string           `json:"symbol"`
	Direction       string           `json:"direction"`
	QuoteInvestment decimal.Decimal  `json:"quoteInvestment"`
	RealizedPNLUSDT *decimal.Decimal `json:"realizedPnlUsdt"`
	ClosedReason    *string          `json:"closedReason"`
	Status          string           `json:"status"`
	ClosedAt        *time.Time       `json:"closedAt"`
}

type State struct {
	Settings            Settings          `json:"settings"`
	LastScan            *ScanRun          `json:"lastScan"`
	Candidates          []Candidate       `json:"candidates"`
	ActiveBots          []ActiveBot       `json:"activeBots"`
	ClosedBots          []ClosedBot       `json:"closedBots"`
	PnL                 PnLBreakdown      `json:"pnl"`
	Exchange            *ExchangeSnapshot `json:"exchange,omitempty"`
	MetricDefinitions   map[string]string `json:"metricDefinitions"`
	FeatureAvailability map[string]string `json:"featureAvailability"`
}

// PnLBreakdown keeps simulated and real money strictly separated: PAPER
// numbers never mix into REAL totals on any screen.
type PnLBreakdown struct {
	Paper PnLSummary `json:"paper"`
	Real  PnLSummary `json:"real"`
}

// ExchangeSnapshot is the live wallet state from the native Pionex balance
// endpoints (spot trading account + futures account), cached briefly so UI
// polling stays polite.
type ExchangeSnapshot struct {
	Connected     bool                     `json:"connected"`
	AccountName   string                   `json:"accountName,omitempty"`
	Error         string                   `json:"error,omitempty"`
	Coins         []pionex.FuturesBalance  `json:"coins"`
	USDTFree      decimal.Decimal          `json:"usdtFree"`
	USDTFrozen    decimal.Decimal          `json:"usdtFrozen"`
	USDTDebts     decimal.Decimal          `json:"usdtDebts"`
	SpotCoins     []pionex.SpotBalance     `json:"spotCoins"`
	SpotUSDTFree  decimal.Decimal          `json:"spotUsdtFree"`
	SpotUSDTFrozen decimal.Decimal         `json:"spotUsdtFrozen"`
	TotalUSDT     decimal.Decimal          `json:"totalUsdt"`
	UpdatedAt     time.Time                `json:"updatedAt"`
}

type PnLSummary struct {
	RealizedUSDT   decimal.Decimal `json:"realizedUsdt"`
	UnrealizedUSDT decimal.Decimal `json:"unrealizedUsdt"`
	TotalUSDT      decimal.Decimal `json:"totalUsdt"`
	ClosedBots     int             `json:"closedBots"`
	Profitable     int             `json:"profitable"`
}

type clientCacheEntry struct {
	fingerprint string
	client      *pionex.Client
}

type Service struct {
	db   *pgxpool.Pool
	risk *risk.Engine

	balanceMu      sync.Mutex
	balanceCached  *ExchangeSnapshot
	clientMu       sync.Mutex
	clientCache    map[string]*clientCacheEntry
	publicAPI      *pionex.Client
}

func NewService(db *pgxpool.Pool, riskEngine *risk.Engine) *Service {
	return &Service{
		db: db, risk: riskEngine,
		clientCache: make(map[string]*clientCacheEntry),
		publicAPI:   pionex.NewClient("", "", ""),
	}
}

// PublicAPI returns the shared unauthenticated market-data client so every
// caller works inside one rate budget instead of private ones.
func (s *Service) PublicAPI() *pionex.Client { return s.publicAPI }

// PrivateClient returns a cached authenticated client for the account. The
// cache is keyed by account id and invalidated automatically when the key
// fingerprint rotates, so the keyring is not decrypted on every call.
func (s *Service) PrivateClient(
	ctx context.Context,
	accountService *accounts.Service,
	accountID string,
) (*pionex.Client, error) {
	var fingerprint string
	_ = s.db.QueryRow(ctx, `
		SELECT COALESCE(key_fingerprint, '') FROM pionex_accounts WHERE id = $1
	`, accountID).Scan(&fingerprint)

	s.clientMu.Lock()
	if entry, ok := s.clientCache[accountID]; ok && entry.fingerprint == fingerprint {
		client := entry.client
		s.clientMu.Unlock()
		return client, nil
	}
	s.clientMu.Unlock()

	credentials, err := accountService.Credentials(ctx, accountID, true)
	if err != nil {
		return nil, err
	}
	client := pionex.NewClient("", credentials.APIKey, credentials.APISecret)
	s.clientMu.Lock()
	s.clientCache[accountID] = &clientCacheEntry{fingerprint: fingerprint, client: client}
	s.clientMu.Unlock()
	return client, nil
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
		       min_profit_factor, fee_bps, slippage_bps,
		       pnl_target_mode, pnl_target_usdt, max_loss_usdt, manage_interval_seconds,
		       range_break_buffer_pct, max_adjustments_per_bot, ai_kit_enabled,
		       ai_autotune_enabled, ai_autotune_interval_seconds,
		       last_autotune_at, last_autotune_notes,
		       last_error,
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
	// Settings may change at any time: they govern future deployments and
	// the supervision loop, while every running bot keeps the parameters
	// captured at its own open (target, stop, range).
	if input.AIAutotuneInterval == 0 {
		input.AIAutotuneInterval = current.AIAutotuneInterval
		if input.AIAutotuneInterval == 0 {
			input.AIAutotuneInterval = 3600
		}
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
		    pnl_target_mode = $24, pnl_target_usdt = $25, max_loss_usdt = $26,
		    manage_interval_seconds = $27, range_break_buffer_pct = $28,
		    max_adjustments_per_bot = $29, ai_kit_enabled = $30,
		    ai_autotune_enabled = $31, ai_autotune_interval_seconds = $32,
		    last_error = NULL, updated_at = NOW()
		WHERE scope_key = $1
	`, DefaultScope, accountID, input.ExecutionMode, input.BudgetUSDT,
		input.MaxActiveBots, input.Leverage, input.MinSharpe, input.MinEVPct,
		input.StopLossMode, input.SmartPNLEnabled,
		input.AdaptiveLeverageEnabled, input.DensityGridEnabled,
		input.CandleInterval, input.LookbackCandles, input.MaxSymbolsPerScan,
		input.ScanIntervalSeconds, input.MinVolume24h, input.MinVolatilityPct,
		input.MaxVolatilityPct, input.MaxDrawdownPct, input.MinProfitFactor,
		input.FeeBps, input.SlippageBps, input.PnLTargetMode,
		input.PnLTargetUSDT, input.MaxLossUSDT, input.ManageIntervalSeconds,
		input.RangeBreakBufferPct, input.MaxAdjustmentsPerBot, input.AIKitEnabled,
		input.AIAutotuneEnabled, input.AIAutotuneInterval)
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
		WHERE settings_id = $1 AND status = 'SUCCEEDED'
		ORDER BY started_at DESC LIMIT 1
	`, settings.ID).Scan(
		&scan.ID, &scan.Status, &scan.CandidatesFound, &scan.ErrorMessage,
		&scan.StartedAt, &scan.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.db.QueryRow(ctx, `
			SELECT id, status, candidates_found, error_message, started_at, completed_at
			FROM autogrid_scan_runs
			WHERE settings_id = $1
			ORDER BY started_at DESC LIMIT 1
		`, settings.ID).Scan(
			&scan.ID, &scan.Status, &scan.CandidatesFound, &scan.ErrorMessage,
			&scan.StartedAt, &scan.CompletedAt,
		)
	}
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
	state.ClosedBots, err = s.listClosedBots(ctx, settings.ID)
	if err != nil {
		return nil, err
	}
	state.PnL = breakdownPnL(state.ActiveBots, state.ClosedBots)
	return state, nil
}

// ExchangeSnapshotWith fetches the live exchange wallet through the resolved
// account (30s cache); without an account it reports connected=false instead
// of failing the whole state.
func (s *Service) ExchangeSnapshotWith(
	ctx context.Context,
	accountService *accounts.Service,
) *ExchangeSnapshot {
	s.balanceMu.Lock()
	if s.balanceCached != nil && time.Since(s.balanceCached.UpdatedAt) < 30*time.Second {
		cached := *s.balanceCached
		s.balanceMu.Unlock()
		return &cached
	}
	s.balanceMu.Unlock()

	// Non-nil slices so the JSON never carries null arrays the UI would
	// have to defend against.
	snapshot := &ExchangeSnapshot{
		UpdatedAt: time.Now(),
		Coins:     []pionex.FuturesBalance{},
		SpotCoins: []pionex.SpotBalance{},
	}
	accountID, err := s.resolveAccount(ctx)
	if err != nil {
		snapshot.Error = err.Error()
	} else if accountID == nil {
		snapshot.Error = "no Pionex account configured"
	} else {
		var name string
		_ = s.db.QueryRow(ctx, `SELECT name FROM pionex_accounts WHERE id = $1`, *accountID).Scan(&name)
		snapshot.AccountName = name
		credentials, credErr := accountService.Credentials(ctx, *accountID, true)
		if credErr != nil {
			snapshot.Error = credErr.Error()
		} else {
			client := pionex.NewClient("", credentials.APIKey, credentials.APISecret)
			balCtx, balCancel := context.WithTimeout(ctx, 3*time.Second)
			defer balCancel()
			// Futures wallet: the margin grids trade with.
			if balances, balanceErr := client.GetFuturesBalances(balCtx); balanceErr != nil {
				snapshot.Error = balanceErr.Error()
			} else {
				snapshot.Connected = true
				snapshot.Coins = balances
				for _, balance := range balances {
					if strings.EqualFold(balance.Coin, "USDT") {
						snapshot.USDTFree = balance.Free
						snapshot.USDTFrozen = balance.Frozen
						snapshot.USDTDebts = balance.Debts
					}
				}
			}
			// Spot trading account: where deposited funds actually sit until
			// they are transferred to futures.
			if spot, spotErr := client.GetSpotBalances(balCtx); spotErr != nil {
				if snapshot.Error == "" {
					snapshot.Error = "spot: " + spotErr.Error()
				}
			} else {
				snapshot.SpotCoins = spot
				for _, balance := range spot {
					if strings.EqualFold(balance.Coin, "USDT") {
						snapshot.SpotUSDTFree = balance.Free
						snapshot.SpotUSDTFrozen = balance.Frozen
					}
				}
			}
			snapshot.TotalUSDT = snapshot.SpotUSDTFree.Add(snapshot.USDTFree)
		}
	}
	s.balanceMu.Lock()
	s.balanceCached = snapshot
	s.balanceMu.Unlock()
	return snapshot
}

func breakdownPnL(active []ActiveBot, closed []ClosedBot) PnLBreakdown {
	paperActive, realActive := splitActiveBySource(active)
	paperClosed, realClosed := splitClosedBySource(closed)
	return PnLBreakdown{
		Paper: summarizePnL(paperActive, paperClosed),
		Real:  summarizePnL(realActive, realClosed),
	}
}

func splitActiveBySource(bots []ActiveBot) ([]ActiveBot, []ActiveBot) {
	paper, real := make([]ActiveBot, 0), make([]ActiveBot, 0)
	for _, bot := range bots {
		if bot.Source == "REAL" {
			real = append(real, bot)
		} else {
			paper = append(paper, bot)
		}
	}
	return paper, real
}

func splitClosedBySource(bots []ClosedBot) ([]ClosedBot, []ClosedBot) {
	paper, real := make([]ClosedBot, 0), make([]ClosedBot, 0)
	for _, bot := range bots {
		if bot.Source == "REAL" {
			real = append(real, bot)
		} else {
			paper = append(paper, bot)
		}
	}
	return paper, real
}

func summarizePnL(active []ActiveBot, closed []ClosedBot) PnLSummary {
	summary := PnLSummary{}
	for _, bot := range active {
		if bot.RealizedPNLUSDT != nil {
			summary.RealizedUSDT = summary.RealizedUSDT.Add(*bot.RealizedPNLUSDT)
		}
		if bot.UnrealizedPNLUSDT != nil {
			summary.UnrealizedUSDT = summary.UnrealizedUSDT.Add(*bot.UnrealizedPNLUSDT)
		}
	}
	for _, bot := range closed {
		summary.ClosedBots++
		if bot.RealizedPNLUSDT != nil {
			summary.RealizedUSDT = summary.RealizedUSDT.Add(*bot.RealizedPNLUSDT)
			if bot.RealizedPNLUSDT.GreaterThan(decimal.Zero) {
				summary.Profitable++
			}
		}
	}
	summary.TotalUSDT = summary.RealizedUSDT.Add(summary.UnrealizedUSDT)
	return summary
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
	if status == "STOPPED" || status == "EMERGENCY_STOPPED" {
		_ = s.CloseAllActiveBots(ctx, status)
	}
	return nil
}

// CloseAllActiveBots immediately closes all running paper bots and queues stop for real bots.
func (s *Service) CloseAllActiveBots(ctx context.Context, reason string) error {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "AUTOGRID_STOP"
	}
	// 1. Close all running paper bots immediately
	_, err = s.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET status = 'COMPLETED',
		    closed_reason = $2,
		    closed_at = NOW(),
		    updated_at = NOW()
		WHERE settings_id = $1 AND status = 'RUNNING'
	`, settings.ID, reason)
	if err != nil {
		return fmt.Errorf("close all paper bots: %w", err)
	}

	// 2. Request stop for all real bots
	_, err = s.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED',
		    closed_reason = COALESCE(closed_reason, $2),
		    updated_at = NOW()
		WHERE autogrid_settings_id = $1
		  AND status IN ('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING')
	`, settings.ID, reason)
	if err != nil {
		return fmt.Errorf("request stop for real bots: %w", err)
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
	if input.MaxActiveBots < 1 || input.MaxActiveBots > 50 {
		return errors.New("max active bots must be between 1 and 50")
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
	if input.MaxSymbolsPerScan < 1 || input.MaxSymbolsPerScan > 500 {
		return errors.New("max symbols per scan must be between 1 and 500")
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
	if input.PnLTargetMode == "" {
		input.PnLTargetMode = "DYNAMIC"
	}
	if input.PnLTargetMode != "FIXED" && input.PnLTargetMode != "DYNAMIC" {
		return errors.New("PnL target mode must be DYNAMIC or FIXED")
	}
	if input.PnLTargetUSDT.IsNegative() || input.MaxLossUSDT.IsNegative() {
		return errors.New("PnL target and max loss cannot be negative")
	}
	if input.PnLTargetMode == "FIXED" &&
		(input.PnLTargetUSDT.IsZero() || input.MaxLossUSDT.IsZero()) {
		return errors.New("FIXED PnL mode requires non-zero target and max loss")
	}
	if input.ManageIntervalSeconds < 15 || input.ManageIntervalSeconds > 86400 {
		return errors.New("manage interval must be between 15 and 86400 seconds")
	}
	if input.RangeBreakBufferPct.LessThanOrEqual(decimal.Zero) ||
		input.RangeBreakBufferPct.GreaterThan(decimal.NewFromInt(20)) {
		return errors.New("range break buffer must be between 0 and 20 percent")
	}
	if input.MaxAdjustmentsPerBot < 0 || input.MaxAdjustmentsPerBot > 10 {
		return errors.New("max adjustments per bot must be between 0 and 10")
	}
	if input.AIAutotuneInterval < 300 || input.AIAutotuneInterval > 86400 {
		return errors.New("AI autotune interval must be between 300 and 86400 seconds")
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
		       reconciliation_state, adjustments_count,
		       pnl_target_usdt, max_loss_usdt,
		       realized_pnl_usdt, unrealized_pnl_usdt, updated_at
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
			&item.QuoteInvestment, &item.ReconciliationState,
			&item.AdjustmentsCount, &item.PnLTargetUSDT, &item.MaxLossUSDT,
			&item.RealizedPNLUSDT, &item.UnrealizedPNLUSDT, &item.UpdatedAt,
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
		       realized_pnl_usdt, unrealized_pnl_usdt,
		       pnl_target_usdt, max_loss_usdt, updated_at,
		       model_state
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
		var rawModelState any
		if err := rows.Scan(
			&item.ID, &item.Symbol, &item.Status, &item.Direction,
			&item.GridType, &item.LowerPrice, &item.UpperPrice,
			&item.GridNum, &item.Leverage, &item.QuoteInvestment,
			&item.RealizedPNLUSDT, &item.UnrealizedPNLUSDT,
			&item.PnLTargetUSDT, &item.MaxLossUSDT, &item.UpdatedAt,
			&rawModelState,
		); err != nil {
			return nil, fmt.Errorf("scan paper AutoGrid bot: %w", err)
		}
		if ms, ok := rawModelState.(map[string]any); ok {
			if reason, ok := ms["leverageReason"].(string); ok {
				item.LeverageReason = reason
			}
			if mode, ok := ms["leverageMode"].(string); ok {
				item.LeverageMode = mode
			}
			if base, ok := ms["baseLeverage"].(float64); ok {
				item.BaseLeverage = int(base)
			}
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
		&item.SlippageBps, &item.PnLTargetMode, &item.PnLTargetUSDT, &item.MaxLossUSDT,
		&item.ManageIntervalSeconds, &item.RangeBreakBufferPct,
		&item.MaxAdjustmentsPerBot, &item.AIKitEnabled,
		&item.AIAutotuneEnabled, &item.AIAutotuneInterval,
		&item.LastAutotuneAt, &item.LastAutotuneNotes,
		&item.LastError, &item.LastStartedAt,
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

func (s *Service) listClosedBots(ctx context.Context, settingsID string) ([]ClosedBot, error) {
	items := make([]ClosedBot, 0)
	rows, err := s.db.Query(ctx, `
		SELECT id, symbol, direction, quote_investment,
		       realized_pnl_usdt, closed_reason, status, COALESCE(closed_at, updated_at)
		FROM grid_bots
		WHERE (autogrid_settings_id = $1 OR autogrid_settings_id IS NOT NULL)
		  AND status IN ('STOPPED', 'CANCELLED', 'COMPLETED', 'LIQUIDATED', 'FAILED')
		ORDER BY COALESCE(closed_at, updated_at) DESC
		LIMIT 100
	`, settingsID)
	if err != nil {
		return nil, fmt.Errorf("list closed real AutoGrid bots: %w", err)
	}
	for rows.Next() {
		var item ClosedBot
		item.Source = "REAL"
		if err := rows.Scan(
			&item.ID, &item.Symbol, &item.Direction, &item.QuoteInvestment,
			&item.RealizedPNLUSDT, &item.ClosedReason, &item.Status, &item.ClosedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan closed real AutoGrid bot: %w", err)
		}
		items = append(items, item)
	}
	rows.Close()

	rows, err = s.db.Query(ctx, `
		SELECT id, symbol, direction, quote_investment,
		       realized_pnl_usdt, closed_reason, status, COALESCE(closed_at, updated_at)
		FROM paper_grid_bots
		WHERE (settings_id = $1 OR settings_id IS NOT NULL)
		  AND status IN ('STOPPED', 'COMPLETED', 'EMERGENCY_STOPPED')
		ORDER BY COALESCE(closed_at, updated_at) DESC
		LIMIT 100
	`, settingsID)
	if err != nil {
		return nil, fmt.Errorf("list closed paper AutoGrid bots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item ClosedBot
		item.Source = "PAPER"
		if err := rows.Scan(
			&item.ID, &item.Symbol, &item.Direction, &item.QuoteInvestment,
			&item.RealizedPNLUSDT, &item.ClosedReason, &item.Status, &item.ClosedAt,
		); err != nil {
			return nil, fmt.Errorf("scan closed paper AutoGrid bot: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// RequestBotClose durably records a close intent. Real bots transition to
// STOP_REQUESTED and the reconcile loop submits the native Pionex cancel;
// paper bots close immediately with a final PnL mark.
func (s *Service) RequestBotClose(ctx context.Context, settingsID, botID, reason string) (string, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE grid_bots
		SET status = 'STOP_REQUESTED', closed_reason = $3, updated_at = NOW()
		WHERE id = $1 AND autogrid_settings_id = $2
		  AND status IN ('PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'RUNNING')
	`, botID, settingsID, reason)
	if err != nil {
		return "", fmt.Errorf("request real bot close: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return "REAL", nil
	}
	tag, err = s.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET status = 'COMPLETED', closed_reason = $3,
		    closed_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND settings_id = $2 AND status = 'RUNNING'
	`, botID, settingsID, reason)
	if err != nil {
		return "", fmt.Errorf("close paper bot: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return "PAPER", nil
	}
	return "", errors.New("bot not found or already terminal")
}

// SetExecutionMode switches the autopilot execution mode at any time (it
// governs newly opened bots only; running bots keep their nature). REAL is
// admitted only through the durable gates and a verified account.
func (s *Service) SetExecutionMode(ctx context.Context, mode string) (*Settings, error) {
	if mode != "PAPER" && mode != "REAL" {
		return nil, errors.New("mode must be PAPER or REAL")
	}
	if mode == "REAL" {
		if err := s.realExecutionGates(ctx); err != nil {
			return nil, err
		}
		if _, err := s.resolveAccount(ctx); err != nil {
			return nil, err
		}
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE autogrid_settings
		SET execution_mode = $2, updated_at = NOW()
		WHERE scope_key = $1
	`, DefaultScope, mode); err != nil {
		return nil, fmt.Errorf("set execution mode: %w", err)
	}
	return s.GetSettings(ctx)
}

// realExecutionGates mirrors the AutoGrid worker gates: real money requires
// the explicit app_config switch, the durable feature flag and the kill
// switch to be off.
func (s *Service) realExecutionGates(ctx context.Context) error {
	riskSettings, err := s.risk.LoadSettings(ctx)
	if err != nil {
		return err
	}
	if riskSettings.KillSwitchEnabled {
		return errors.New("kill switch is enabled: real bot creation is blocked")
	}
	var configEnabled, featureEnabled bool
	if err := s.db.QueryRow(ctx, `
		SELECT
			COALESCE((SELECT (value #>> '{}')::BOOLEAN
			          FROM app_config WHERE key = 'real_grid_execution_enabled'), false),
			COALESCE((SELECT enabled FROM feature_flags WHERE name = 'real_native_grid'), false)
	`).Scan(&configEnabled, &featureEnabled); err != nil {
		return fmt.Errorf("load real execution gates: %w", err)
	}
	if !configEnabled || !featureEnabled {
		return errors.New("REAL bots are blocked by real_grid_execution_enabled or real_native_grid")
	}
	return nil
}

// resolveAccount returns the AutoGrid account: the explicitly selected one,
// or — when none is selected — the single enabled verified account, so that
// adding an API key is enough for AI Kit and manual deploys to work.
func (s *Service) resolveAccount(ctx context.Context) (*string, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings.AccountID != nil {
		return settings.AccountID, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id FROM pionex_accounts
		WHERE is_enabled AND last_verified_at IS NOT NULL
		  AND has_read_permission AND has_bot_permission
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 1:
		return &ids[0], nil
	case 0:
		return nil, errors.New("no verified Pionex account: add an API key on the Pionex API screen")
	default:
		return nil, errors.New("multiple Pionex accounts exist: select one in the AutoGrid settings")
	}
}

// AIKitStrategy fetches the native Pionex AI Kit recommendation for a PERP
// symbol through the configured AutoGrid account. Spot AI parameters are
// returned as advisory market intelligence only (AGENTS.md rule 3).
func (s *Service) AIKitStrategy(
	ctx context.Context,
	accountService *accounts.Service,
	symbol string,
) (*pionex.SpotGridAIStrategy, error) {
	base, quote, err := SplitPionexPerp(symbol)
	if err != nil {
		return nil, err
	}
	accountID, err := s.resolveAccount(ctx)
	if err != nil {
		return nil, err
	}
	client, err := s.PrivateClient(ctx, accountService, *accountID)
	if err != nil {
		return nil, err
	}
	return client.GetSpotGridAIStrategy(ctx, base, quote)
}

type AdjustBotInput struct {
	Mode            string          `json:"mode"` // invest_in or adjust_params
	QuoteInvestment decimal.Decimal `json:"quoteInvestment,omitempty"`
	Lower           decimal.Decimal `json:"lower,omitempty"`
	Upper           decimal.Decimal `json:"upper,omitempty"`
	Row             int             `json:"row,omitempty"`
}

// AdjustBot manages a single running bot through the native Pionex
// adjustParams endpoint (invest_in adds capital, adjust_params moves the
// grid range). Paper bots only move their simulated range.
func (s *Service) AdjustBot(
	ctx context.Context,
	accountService *accounts.Service,
	settingsID, botID string,
	input AdjustBotInput,
) (string, error) {
	if input.Mode != "invest_in" && input.Mode != "adjust_params" {
		return "", errors.New("mode must be invest_in or adjust_params")
	}
	var buOrderID *string
	var accountID *string
	var currentLower, currentUpper decimal.Decimal
	var currentRow int
	if err := s.db.QueryRow(ctx, `
		SELECT bu_order_id, account_id, lower_price, upper_price, grid_num
		FROM grid_bots
		WHERE id = $1 AND autogrid_settings_id = $2 AND status = 'RUNNING'
	`, botID, settingsID).Scan(&buOrderID, &accountID, &currentLower, &currentUpper, &currentRow); err == nil {
		if buOrderID == nil || *buOrderID == "" {
			return "", errors.New("real bot has no remote buOrderId yet")
		}
		client, err := s.PrivateClient(ctx, accountService, *accountID)
		if err != nil {
			return "", err
		}
		params := pionex.AdjustFuturesGridParams{
			BUOrderID: *buOrderID, Type: input.Mode,
		}
		if input.Mode == "invest_in" {
			if !input.QuoteInvestment.GreaterThan(decimal.Zero) {
				return "", errors.New("invest_in requires a positive quoteInvestment")
			}
			params.QuoteInvestment = input.QuoteInvestment
		} else {
			params.Bottom = input.Lower
			params.Top = input.Upper
			params.Row = input.Row
			if input.Row <= 0 {
				params.Row = currentRow
			}
			if !input.Upper.GreaterThan(input.Lower) || !input.Lower.GreaterThan(decimal.Zero) {
				return "", errors.New("adjust_params requires upper > lower > 0")
			}
		}
		if err := client.AdjustFuturesGridBot(ctx, params); err != nil {
			return "", fmt.Errorf("native adjust failed: %w", err)
		}
		if input.Mode == "adjust_params" {
			_, err = s.db.Exec(ctx, `
				UPDATE grid_bots
				SET lower_price = $2, upper_price = $3, grid_num = $4,
				    adjustments_count = adjustments_count + 1, updated_at = NOW()
				WHERE id = $1
			`, botID, input.Lower, input.Upper, params.Row)
		} else {
			_, err = s.db.Exec(ctx, `
				UPDATE grid_bots
				SET quote_investment = quote_investment + $2,
				    adjustments_count = adjustments_count + 1, updated_at = NOW()
				WHERE id = $1
			`, botID, input.QuoteInvestment)
		}
		if err != nil {
			return "", fmt.Errorf("persist adjustment: %w", err)
		}
		return "REAL", nil
	}
	if input.Mode == "adjust_params" {
		if !input.Upper.GreaterThan(input.Lower) || !input.Lower.GreaterThan(decimal.Zero) {
			return "", errors.New("adjust_params requires upper > lower > 0")
		}
		row := input.Row
		if row <= 0 {
			return "", errors.New("adjust_params requires a positive row")
		}
		tag, err := s.db.Exec(ctx, `
			UPDATE paper_grid_bots
			SET lower_price = $2, upper_price = $3, grid_num = $4, updated_at = NOW()
			WHERE id = $1 AND settings_id = $5 AND status = 'RUNNING'
		`, botID, input.Lower, input.Upper, row, settingsID)
		if err != nil {
			return "", fmt.Errorf("adjust paper bot: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return "PAPER", nil
		}
		return "", errors.New("bot not found or not RUNNING")
	}
	if !input.QuoteInvestment.GreaterThan(decimal.Zero) {
		return "", errors.New("invest_in requires a positive quoteInvestment")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE paper_grid_bots
		SET quote_investment = quote_investment + $2, updated_at = NOW()
		WHERE id = $1 AND settings_id = $3 AND status = 'RUNNING'
	`, botID, input.QuoteInvestment, settingsID)
	if err != nil {
		return "", fmt.Errorf("invest into paper bot: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return "PAPER", nil
	}
	return "", errors.New("bot not found or not RUNNING")
}

// AIAdaptedRange converts a Spot AI Kit high/low recommendation into a
// futures-safe grid range: the AI width is kept, the center follows the PERP
// price (spot/perp basis), and the width is clamped to a sane band. The
// operator can always override the proposal manually.
func AIAdaptedRange(spotHigh, spotLow, perpPrice decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	if !spotHigh.GreaterThan(spotLow) || !perpPrice.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero
	}
	mid := spotHigh.Add(spotLow).Div(decimal.NewFromInt(2))
	if !mid.GreaterThan(decimal.Zero) {
		return decimal.Zero, decimal.Zero
	}
	lower := perpPrice.Mul(spotLow.Div(mid))
	upper := perpPrice.Mul(spotHigh.Div(mid))
	maxHalf := perpPrice.Mul(decimal.NewFromFloat(0.125)) // ±12.5% safety clamp
	minHalf := perpPrice.Mul(decimal.NewFromFloat(0.01))  // ±1% minimum width
	lower = clampDecimal(lower, perpPrice.Sub(maxHalf), perpPrice.Sub(minHalf))
	upper = clampDecimal(upper, perpPrice.Add(minHalf), perpPrice.Add(maxHalf))
	return lower, upper
}

func clampDecimal(value, minimum, maximum decimal.Decimal) decimal.Decimal {
	if value.LessThan(minimum) {
		return minimum
	}
	if value.GreaterThan(maximum) {
		return maximum
	}
	return value
}

// AIKitProposal returns the native AI Kit strategy plus an adapted futures
// range proposal centered on the live PERP price. If Pionex AI Kit returns
// BOT_INTERNAL_ERROR (common on altcoins/perp-only tokens), it falls back
// to our quantitative ATR + Support/Resistance grid model.
func (s *Service) AIKitProposal(
	ctx context.Context,
	accountService *accounts.Service,
	symbol string,
) (*pionex.SpotGridAIStrategy, decimal.Decimal, decimal.Decimal, error) {
	strategy, err := s.AIKitStrategy(ctx, accountService, symbol)
	price, priceErr := s.perpPrice(ctx, symbol)
	if priceErr != nil {
		return nil, decimal.Zero, decimal.Zero, priceErr
	}

	if err != nil {
		// Fallback: calculate quantitative ATR & Support/Resistance proposal
		candles, kErr := s.publicAPI.GetKlines(ctx, symbol, "15M", 192)
		if kErr != nil || len(candles) < 20 {
			return nil, decimal.Zero, decimal.Zero, fmt.Errorf("AI Kit недоступен на Pionex для %s (токен без спотовой AI-модели)", symbol)
		}
		regime := marketdata.DetectRegime(candles)
		windowLow, windowHigh := math.MaxFloat64, 0.0
		for _, c := range candles {
			l, _ := c.Low.Float64()
			h, _ := c.High.Float64()
			windowLow = math.Min(windowLow, l)
			windowHigh = math.Max(windowHigh, h)
		}
		curPrice, _ := price.Float64()
		halfBand := math.Max(regime.ParkinsonVolatility, 2.0) / 200
		volLower := curPrice * (1 - halfBand)
		volUpper := curPrice * (1 + halfBand)
		atrBuffer := curPrice * (math.Max(regime.ATRPct, 0.5) / 100) * 0.5
		lowBound := math.Max(windowLow-atrBuffer, volLower)
		highBound := math.Min(windowHigh+atrBuffer, volUpper)
		if !(lowBound < curPrice && curPrice < highBound && lowBound > 0) {
			lowBound, highBound = volLower, volUpper
		}
		gridNum := int(math.Round((highBound - lowBound) / (curPrice * (math.Max(regime.ATRPct, 0.3) / 100))))
		if gridNum < 15 {
			gridNum = 20
		} else if gridNum > 150 {
			gridNum = 150
		}
		fallbackStrategy := &pionex.SpotGridAIStrategy{
			StrategyID:  "local-sr-atr-model",
			High:        decimal.NewFromFloat(highBound),
			Low:         decimal.NewFromFloat(lowBound),
			GridCount:   gridNum,
			Annualized:  decimal.NewFromFloat(regime.ParkinsonVolatility * 2.5),
			MaxDrawDown: decimal.NewFromFloat(regime.ATRPct * 0.5),
		}
		return fallbackStrategy, fallbackStrategy.Low, fallbackStrategy.High, nil
	}

	lower, upper := AIAdaptedRange(strategy.High, strategy.Low, price)
	return strategy, lower, upper, nil
}

func (s *Service) perpPrice(ctx context.Context, symbol string) (decimal.Decimal, error) {
	tickers, err := s.publicAPI.GetTickers(ctx, symbol, "")
	if err != nil || len(tickers) == 0 {
		return decimal.Zero, fmt.Errorf("fetch PERP price for %s: %w", symbol, err)
	}
	if !tickers[0].Close.GreaterThan(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("empty PERP price for %s", symbol)
	}
	return tickers[0].Close, nil
}

// computeManualTargets derives dynamic targets for a manually deployed bot
// from live klines (sigma + ATR blend) when no scanner candidate exists.
func (s *Service) computeManualTargets(
	ctx context.Context,
	settings Settings,
	symbol string,
) (*decimal.Decimal, *decimal.Decimal) {
	if settings.PnLTargetMode != "DYNAMIC" {
		if settings.PnLTargetUSDT.IsZero() || settings.MaxLossUSDT.IsZero() {
			return nil, nil
		}
		target, loss := settings.PnLTargetUSDT, settings.MaxLossUSDT
		return &target, &loss
	}
	candles, err := s.publicAPI.GetKlines(ctx, symbol, "60M", 60)
	if err != nil || len(candles) < 30 {
		return nil, nil
	}
	regime := marketdata.DetectRegime(candles)
	vol := sigmaDailyFromCandles(candles)
	if vol <= 0 {
		vol = regime.ATRPct
	}
	drawdown := vol * 2
	if drawdown < 5 {
		drawdown = 5
	}
	targets := marketdata.ComputeDynamicTargets(marketdata.DynamicTargetsInput{
		Budget:               settings.BudgetUSDT.InexactFloat64(),
		ScannerVolatilityPct: vol,
		ScannerATRPct:        regime.ATRPct,
		ScannerDrawdownPct:   drawdown,
	})
	target := decimal.NewFromFloat(targets.TargetUSDT)
	loss := decimal.NewFromFloat(targets.MaxLossUSDT)
	return &target, &loss
}

// sigmaDailyFromCandles annualizes nothing: it returns the daily sigma in
// percent from hourly candle closes.
func sigmaDailyFromCandles(candles []pionex.KlineCandle) float64 {
	returns := make([]float64, 0, len(candles))
	for index := 1; index < len(candles); index++ {
		previous, _ := candles[index-1].Close.Float64()
		current, _ := candles[index].Close.Float64()
		if previous > 0 && current > 0 {
			returns = append(returns, current/previous-1)
		}
	}
	if len(returns) < 10 {
		return 0
	}
	meanValue := 0.0
	for _, value := range returns {
		meanValue += value
	}
	meanValue /= float64(len(returns))
	sum := 0.0
	for _, value := range returns {
		sum += (value - meanValue) * (value - meanValue)
	}
	std := math.Sqrt(sum / float64(len(returns)-1))
	return std * math.Sqrt(24) * 100
}

// lastATRPct reads the recent ATR% for a symbol from live klines.
func (s *Service) lastATRPct(ctx context.Context, symbol string) float64 {
	candles, err := s.publicAPI.GetKlines(ctx, symbol, "60M", 60)
	if err != nil || len(candles) < 30 {
		return 0
	}
	return marketdata.DetectRegime(candles).ATRPct
}

// ManualDeployInput opens a bot with operator-confirmed parameters. Empty
// fields fall back to the latest scanner recommendation for the symbol.
type ManualDeployInput struct {
	Symbol      string          `json:"symbol"`
	Mode        string          `json:"mode"`        // PAPER, REAL or empty (= autopilot mode)
	Direction   string          `json:"direction"`   // LONG, SHORT, NEUTRAL
	Leverage    int             `json:"leverage"`
	Lower       decimal.Decimal `json:"lower"`
	Upper       decimal.Decimal `json:"upper"`
	Row         int             `json:"row"`
	RangeSource string          `json:"rangeSource"` // SCANNER or AI_KIT (recorded)
}

// DeployManualBot opens one bot with explicit operator parameters, gated by
// the durable risk engine and (REAL) the native Pionex checkParams endpoint.
func (s *Service) DeployManualBot(
	ctx context.Context,
	accountService *accounts.Service,
	input ManualDeployInput,
) (*ActiveBot, string, error) {
	if _, _, err := SplitPionexPerp(input.Symbol); err != nil {
		return nil, "", err
	}
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, "", err
	}
	mode := input.Mode
	if mode == "" {
		mode = settings.ExecutionMode
	}
	if mode != "PAPER" && mode != "REAL" {
		return nil, "", errors.New("mode must be PAPER or REAL")
	}
	trend := "no_trend"
	switch input.Direction {
	case "LONG":
		trend = "long"
	case "SHORT":
		trend = "short"
	case "NEUTRAL", "":
	default:
		return nil, "", errors.New("direction must be LONG, SHORT or NEUTRAL")
	}
	leverage := input.Leverage
	if leverage <= 0 {
		leverage = settings.Leverage
	}
	lower, upper := input.Lower, input.Upper
	row := input.Row
	if row <= 0 && upper.GreaterThan(lower) && lower.GreaterThan(decimal.Zero) {
		// ATR-derived level count for the operator's range (AI Kit count is
		// applied automatically when the prefill was taken from the proposal).
		mid := upper.Add(lower).Div(decimal.NewFromInt(2))
		rangePct := upper.Sub(lower).Div(mid).InexactFloat64() * 100
		row = marketdata.GridLevelsForRange(rangePct, s.lastATRPct(ctx, input.Symbol))
	}
	// Fall back to the latest scanner recommendation for missing fields.
	if !upper.GreaterThan(lower) {
		var candidate struct{ lower, upper decimal.Decimal; trend string; leverage, row int }
		if err := s.db.QueryRow(ctx, `
			SELECT COALESCE(lower_price, 0), COALESCE(upper_price, 0),
			       COALESCE(recommended_trend, 'no_trend'),
			       COALESCE(recommended_leverage, 0), COALESCE(grid_num, 0)
			FROM autogrid_candidates
			WHERE symbol = $1 AND decision = 'ACCEPTED'
			ORDER BY created_at DESC LIMIT 1
		`, input.Symbol).Scan(&candidate.lower, &candidate.upper, &candidate.trend,
			&candidate.leverage, &candidate.row); err == nil && candidate.upper.GreaterThan(candidate.lower) {
			lower, upper = candidate.lower, candidate.upper
			if input.Leverage <= 0 && candidate.leverage > 0 {
				leverage = candidate.leverage
			}
			if input.Row <= 0 && candidate.row > 0 {
				row = candidate.row
			}
			if input.Direction == "" {
				trend = candidate.trend
			}
		}
	}
	if !upper.GreaterThan(lower) || !lower.GreaterThan(decimal.Zero) {
		return nil, "", errors.New("a grid range is required: pass lower/upper or run a scan first")
	}
	if row < 2 || row > 500 {
		return nil, "", errors.New("grid row must be between 2 and 500")
	}
	// A grid that does not bracket the live price cannot trade and would be
	// closed by the management loop as a range break on its first cycle.
	if price, priceErr := s.perpPrice(ctx, input.Symbol); priceErr == nil &&
		(price.LessThanOrEqual(lower) || price.GreaterThanOrEqual(upper)) {
		return nil, "", fmt.Errorf(
			"range %s–%s does not bracket the live PERP price %s",
			lower.Round(8), upper.Round(8), price.Round(8))
	}

	if mode == "PAPER" {
		botTarget, botMaxLoss := s.computeManualTargets(ctx, *settings, input.Symbol)
		var id string
		if err := s.db.QueryRow(ctx, `
			INSERT INTO paper_grid_bots (
				settings_id, symbol, status, direction, grid_type,
				lower_price, upper_price, grid_num, leverage, quote_investment,
				entry_price, mark_price, model_state,
				pnl_target_usdt, max_loss_usdt
			) VALUES (
				$1, $2, 'RUNNING', $3, $4, $5, $6, $7, $8, $9, $10, $10,
				jsonb_build_object('model', 'manual_deploy', 'rangeSource', $11::TEXT,
					'pnlTargetSource', $12::TEXT,
					'warning', 'paper PnL is not a native Pionex grid backtest'),
				$13, $14
			)
			ON CONFLICT (settings_id, symbol) WHERE status = 'RUNNING'
			DO NOTHING
			RETURNING id
		`, settings.ID, input.Symbol, dbDirection(trend), "ARITHMETIC",
			lower, upper, row, leverage, settings.BudgetUSDT,
			decimal.Zero, input.RangeSource, settings.PnLTargetMode,
			botTarget, botMaxLoss,
		).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, "", fmt.Errorf(
					"a %s bot is already RUNNING — close it first on the bots screen", input.Symbol)
			}
			return nil, "", fmt.Errorf("deploy manual paper bot: %w", err)
		}
		if id == "" {
			return nil, "", errors.New("a paper bot for this symbol is already RUNNING")
		}
		price, priceErr := s.perpPrice(ctx, input.Symbol)
		if priceErr == nil {
			_, _ = s.db.Exec(ctx, `UPDATE paper_grid_bots SET entry_price = $2, mark_price = $2 WHERE id = $1`, id, price)
		}
		return &ActiveBot{ID: id, Source: "PAPER", Symbol: input.Symbol,
			Status: "RUNNING", Direction: dbDirection(trend),
			LowerPrice: lower, UpperPrice: upper, GridNum: row,
			Leverage: leverage, QuoteInvestment: settings.BudgetUSDT}, "PAPER", nil
	}

	// REAL mode: durable execution gates, risk gate, native checkParams,
	// idempotent create.
	if err := s.realExecutionGates(ctx); err != nil {
		return nil, "", err
	}
	accountID, err := s.resolveAccount(ctx)
	if err != nil {
		return nil, "", err
	}
	if err := s.risk.ValidateNewGrid(ctx, *accountID, input.Symbol, leverage, settings.BudgetUSDT); err != nil {
		return nil, "", err
	}
	client, err := s.PrivateClient(ctx, accountService, *accountID)
	if err != nil {
		return nil, "", err
	}
	base, quote, _ := SplitPionexPerp(input.Symbol)
	params := pionex.NativeFuturesGridCreateParams{
		Base: base, Quote: quote,
		BUOrderData: pionex.BUOrderData{
			Top: upper, Bottom: lower, Row: row,
			GridType: "arithmetic", Trend: trend,
			Leverage: leverage, QuoteInvestment: settings.BudgetUSDT,
			InvestCoin: "USDT", InvestmentFrom: "USER",
		},
	}
	botTarget, botMaxLoss := s.computeManualTargets(ctx, *settings, input.Symbol)
	if botTarget != nil && botTarget.GreaterThan(decimal.Zero) {
		params.BUOrderData.ProfitStopType = "profit_amount"
		params.BUOrderData.ProfitStop = botTarget
	}
	check, checkErr := client.CheckFuturesGridParams(ctx, params)
	if checkErr != nil {
		return nil, "", fmt.Errorf("native checkParams rejected the proposal: %w", checkErr)
	}
	if check.MinInvestment.GreaterThan(decimal.Zero) && settings.BudgetUSDT.LessThan(check.MinInvestment) {
		return nil, "", fmt.Errorf(
			"budget %s is below the Pionex minimum investment %s",
			settings.BudgetUSDT, check.MinInvestment)
	}
	manager := grid.NewLifecycleManager(s.db, client)
	gridID, createErr := manager.CreateGridBot(ctx, grid.CreateInput{
		AccountID:          *accountID,
		AutoGridSettingsID: &settings.ID,
		IdempotencyKey:     fmt.Sprintf("manual:%s:%d", input.Symbol, time.Now().UnixNano()),
		Params:             params,
		PnLTargetUSDT:      botTarget,
		MaxLossUSDT:        botMaxLoss,
	})
	if createErr != nil {
		return nil, "", createErr
	}
	return &ActiveBot{ID: gridID, Source: "REAL", Symbol: input.Symbol,
		Status: "RUNNING", Direction: dbDirection(trend),
		LowerPrice: lower, UpperPrice: upper, GridNum: row,
		Leverage: leverage, QuoteInvestment: settings.BudgetUSDT}, "REAL", nil
}

func dbDirection(trend string) string {
	switch trend {
	case "long":
		return "LONG"
	case "short":
		return "SHORT"
	default:
		return "NEUTRAL"
	}
}

// AISample is one pair's native AI Kit reading used for settings inference.
type AISample struct {
	Symbol      string  `json:"symbol"`
	Volatility  float64 `json:"volatilityPct"`
	MaxDrawDown float64 `json:"maxDrawDownPct"`
	Annualized  float64 `json:"annualizedPct"`
	GridCount   int     `json:"gridCount"`
}

// AIFillSuggestion proposes autopilot settings derived from the live AI Kit
// distribution across the most liquid pairs. Nothing is applied
// automatically — the operator reviews and saves.
type AIFillSuggestion struct {
	Sampled   []AISample      `json:"sampled"`
	Suggested map[string]any  `json:"suggested"`
	Notes     []string        `json:"notes"`
}

// deriveAISettings maps the sampled AI Kit distribution onto autopilot
// fields: the volatility band follows the P25–P90 of pair readings, the
// drawdown cap the P75, and leverage steps down as median volatility rises.
func deriveAISettings(samples []AISample) (map[string]any, []string) {
	vols, draws := make([]float64, 0, len(samples)), make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.Volatility > 0 {
			vols = append(vols, sample.Volatility)
		}
		if sample.MaxDrawDown > 0 {
			draws = append(draws, sample.MaxDrawDown)
		}
	}
	suggested := map[string]any{
		"pnlTargetMode": "DYNAMIC",
		"aiKitEnabled":  true,
	}
	notes := []string{
		"цели PnL: DYNAMIC — считаются per-bot из волатильности AI Kit",
	}
	if len(vols) >= 3 {
		minVol := clampPercentile(vols, 0.25, 0.5, 10)
		maxVol := clampPercentile(vols, 0.90, math.Max(minVol*1.5, 5), 40)
		suggested["minVolatilityPct"] = minVol
		suggested["maxVolatilityPct"] = maxVol
		median := percentile(vols, 0.5)
		leverage := 3.0
		switch {
		case median > 8:
			leverage = 1
		case median > 5:
			leverage = 2
		}
		suggested["leverage"] = int(leverage)
		notes = append(notes, fmt.Sprintf(
			"волатильность AI Kit по %d парам: медиана %.1f%% → полоса %.1f–%.1f%%, плечо %dx",
			len(vols), median, minVol, maxVol, int(leverage)))
	}
	if len(draws) >= 3 {
		maxDrawdown := clampPercentile(draws, 0.75, 8, 30)
		suggested["maxDrawdownPct"] = maxDrawdown
		notes = append(notes, fmt.Sprintf(
			"просадка AI Kit P75: %.1f%% → порог maxDrawdownPct", maxDrawdown))
	}
	return suggested, notes
}

func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Round(fraction * float64(len(sorted)-1)))
	return sorted[index]
}

func clampPercentile(values []float64, fraction, minimum, maximum float64) float64 {
	value := percentile(values, fraction)
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

// ApplyAutotune merges AI-derived suggestions into the RUNNING autopilot.
// Only the scanner whitelist moves (volatility band, drawdown cap, leverage)
// and every field is clamped to a bounded step per tuning round so the
// market steers parameters, not flips them.
func (s *Service) ApplyAutotune(ctx context.Context, suggested map[string]any) (*Settings, []string, error) {
	current, err := s.GetSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	changes := make([]string, 0)
	update := mergePreset(*current, Preset{})

	if value, ok := suggested["minVolatilityPct"].(float64); ok {
		next := clampDecimalStep(current.MinVolatilityPct, decimal.NewFromFloat(value),
			decimal.NewFromFloat(0.3), decimal.NewFromFloat(0.25), decimal.NewFromFloat(15))
		if !next.Equal(current.MinVolatilityPct) {
			update.MinVolatilityPct = next
			changes = append(changes, fmt.Sprintf("minVolatility %s→%s", current.MinVolatilityPct, next))
		}
	}
	if value, ok := suggested["maxVolatilityPct"].(float64); ok {
		next := clampDecimalStep(current.MaxVolatilityPct, decimal.NewFromFloat(value),
			decimal.NewFromFloat(0.3), decimal.NewFromFloat(5), decimal.NewFromFloat(40))
		if !next.Equal(current.MaxVolatilityPct) {
			update.MaxVolatilityPct = next
			changes = append(changes, fmt.Sprintf("maxVolatility %s→%s", current.MaxVolatilityPct, next))
		}
	}
	if value, ok := suggested["maxDrawdownPct"].(float64); ok {
		next := clampDecimalStep(current.MaxDrawdownPct, decimal.NewFromFloat(value),
			decimal.NewFromFloat(0.3), decimal.NewFromFloat(8), decimal.NewFromFloat(30))
		if !next.Equal(current.MaxDrawdownPct) {
			update.MaxDrawdownPct = next
			changes = append(changes, fmt.Sprintf("maxDrawdown %s→%s", current.MaxDrawdownPct, next))
		}
	}
	if leverage, ok := suggested["leverage"].(int); ok {
		next := current.Leverage
		if leverage > next {
			next = current.Leverage + 1
		} else if leverage < next {
			next = current.Leverage - 1
		}
		if next < 1 {
			next = 1
		}
		riskSettings, riskErr := s.risk.LoadSettings(ctx)
		if riskErr == nil && next > riskSettings.MaxLeverage {
			next = riskSettings.MaxLeverage
		}
		if next != current.Leverage {
			update.Leverage = next
			changes = append(changes, fmt.Sprintf("leverage %d→%d", current.Leverage, next))
		}
	}
	if update.MaxVolatilityPct.LessThanOrEqual(update.MinVolatilityPct) {
		update.MinVolatilityPct = current.MinVolatilityPct
		update.MaxVolatilityPct = current.MaxVolatilityPct
		changes = nil
	}
	notes := strings.Join(changes, "; ")
	if notes == "" {
		notes = "без изменений: рынок в пределах текущих параметров"
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE autogrid_settings
		SET min_volatility_pct = $2, max_volatility_pct = $3,
		    max_drawdown_pct = $4, leverage = $5,
		    last_autotune_at = NOW(), last_autotune_notes = $6,
		    updated_at = NOW()
		WHERE scope_key = $1
	`, DefaultScope, update.MinVolatilityPct, update.MaxVolatilityPct,
		update.MaxDrawdownPct, update.Leverage, notes); err != nil {
		return nil, nil, fmt.Errorf("apply autotune: %w", err)
	}
	updated, err := s.GetSettings(ctx)
	if err != nil {
		return nil, changes, err
	}
	return updated, changes, nil
}

// clampDecimalStep moves current toward the proposal by at most
// maxRelativeChange per round, inside [minimum, maximum].
func clampDecimalStep(current, proposal, maxRelativeChange, minimum, maximum decimal.Decimal) decimal.Decimal {
	target := proposal
	if target.LessThan(minimum) {
		target = minimum
	}
	if target.GreaterThan(maximum) {
		target = maximum
	}
	bound := current.Mul(maxRelativeChange)
	upper := current.Add(bound)
	lower := current.Sub(bound)
	switch {
	case target.GreaterThan(upper):
		target = upper
	case target.LessThan(lower):
		target = lower
	}
	if target.LessThan(minimum) {
		target = minimum
	}
	if target.GreaterThan(maximum) {
		target = maximum
	}
	return target
}

// AIKitSettingsFill samples the native AI Kit across the most liquid PERP
// pairs and derives autopilot setting proposals from the distribution.
func (s *Service) AIKitSettingsFill(
	ctx context.Context,
	accountService *accounts.Service,
) (*AIFillSuggestion, error) {
	accountID, err := s.resolveAccount(ctx)
	if err != nil {
		return nil, err
	}
	client, err := s.PrivateClient(ctx, accountService, *accountID)
	if err != nil {
		return nil, err
	}

	tickers, err := client.GetTickers(ctx, "", "PERP")
	if err != nil {
		return nil, err
	}
	type volumePair struct {
		symbol string
		amount float64
	}
	ranked := make([]volumePair, 0, len(tickers))
	for _, ticker := range tickers {
		if !ticker.Close.GreaterThan(decimal.Zero) {
			continue
		}
		amount := ticker.Amount.InexactFloat64()
		if amount <= 0 {
			amount = ticker.Volume.Mul(ticker.Close).InexactFloat64()
		}
		if amount > 0 {
			ranked = append(ranked, volumePair{ticker.Symbol, amount})
		}
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].amount > ranked[j].amount })
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}

	suggestion := &AIFillSuggestion{
		Sampled: make([]AISample, 0, len(ranked)),
		Notes:   nil,
	}
	for _, pair := range ranked {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		base, quote, splitErr := SplitPionexPerp(pair.symbol)
		if splitErr != nil {
			continue
		}
		strategy, aiErr := client.GetSpotGridAIStrategy(ctx, base, quote)
		if aiErr != nil {
			continue
		}
		vol, _ := strategy.Volatility.Float64()
		drawdown, _ := strategy.MaxDrawDown.Float64()
		annualized, _ := strategy.Annualized.Float64()
		suggestion.Sampled = append(suggestion.Sampled, AISample{
			Symbol:      pair.symbol,
			Volatility:  normalizePercent(vol),
			MaxDrawDown: normalizePercent(drawdown),
			Annualized:  normalizePercent(annualized),
			GridCount:   strategy.GridCount,
		})
	}
	if len(suggestion.Sampled) < 3 {
		return nil, fmt.Errorf(
			"AI Kit вернул недостаточно данных (%d пар) — попробуйте позже", len(suggestion.Sampled))
	}
	suggestion.Suggested, suggestion.Notes = deriveAISettings(suggestion.Sampled)
	return suggestion, nil
}

// normalizePercent converts AI Kit ratios (0.05) to percent (5).
func normalizePercent(value float64) float64 {
	if value > 0 && value < 1 {
		return value * 100
	}
	if value >= 1 && value <= 500 {
		return value
	}
	return 0
}

// ClearPaperHistory removes simulated paper bots history from PostgreSQL.
func (s *Service) ClearPaperHistory(ctx context.Context, includeRunning bool) (int64, error) {
	query := `DELETE FROM paper_grid_bots WHERE status = 'COMPLETED'`
	if includeRunning {
		query = `DELETE FROM paper_grid_bots`
	}
	tag, err := s.db.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("clear paper history: %w", err)
	}
	return tag.RowsAffected(), nil
}
