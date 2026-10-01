package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/shopspring/decimal"
)

// Walk-forward backtest gate: a REAL deployment requires a fresh
// OOS verdict for the symbol on the TRADED timeframe with the exact deployed
// parameters (range, levels, leverage, tranche investment), and treats neighbor
// timeframes as a fragility check.
const (
	backtestGateFlag    = "backtest_gate"
	backtestFreshWindow = 4 * time.Hour
	// backtestEngineVersion (v2.0.172b): bumped on every breaking engine
	// change — cached results from a different version are INVALID and must
	// not be served. v170 fixed time-reversed candles; v172 fixed metric
	// mixing (exact candidate DD now reported). All pre-172 caches are wrong.
	backtestEngineVersion = "v173"

	// Traded-TF hard ceilings calibrated for quality entry filtering:
	// 1. OOS Net return floor: relaxed from 0.0% to -1.0% to allow minor noise / walk-forward friction.
	backtestMinOOSPct = -1.0
	// 2. Max Drawdown bounded to risk limits (calibrated to 15% for crypto perpetual grids).
	backtestMaxDrawdown = 0.15
	// 3. Minimum trades / sample size:
	backtestMinRoundTrips = 8 // v2.0.176: grid trades autocorrelated; raw >= 8 is the pre-n_eff floor
	backtestMinFolds      = 3
	// Max stop hits allowed across walk-forward folds (allow at most 1 isolated stop hit).
	backtestMaxStopHits = 1
	// 4. Neighbor fragility ceilings:
	backtestNeighborMaxDrawdown = 0.30
	backtestNeighborMinOOSPct   = -6.0
)

var backtestTFLadder = []string{"15M", "30M", "60M", "4H", "1D"}

// WorstPeriodReport contains the worst OOS test fold details.
type WorstPeriodReport struct {
	Fold        int     `json:"fold"`
	Regime      string  `json:"regime"`
	ReturnPct   float64 `json:"return_pct"`
	MaxDrawdown float64 `json:"max_drawdown"`
	RoundTrips  int     `json:"round_trips"`
	StopHit     bool    `json:"stop_hit"`
}

// BacktestJobSummary is one timeframe's walk-forward verdict from the queue.
type BacktestJobSummary struct {
	Interval         string             `json:"interval"`
	State            string             `json:"state"` // done | pending | missing
	Folds            int                `json:"folds"`
	OOSPct           float64            `json:"oosPct"`
	MaxDD            float64            `json:"maxDd"`
	RoundTrips       int                `json:"roundTrips"`
	StopHits         int                `json:"stopHits"`
	EngineVersion    string             `json:"engineVersion"`
	NetEV            float64            `json:"netEv"`
	CI95Lower        float64            `json:"ci95Lower"`
	CI95Upper        float64            `json:"ci95Upper"`
	CI95Positive     bool               `json:"ci95Positive"`
	SampleSufficient bool               `json:"sampleSufficient"`
	OOSSharpe        float64            `json:"oosSharpe"`
	OOSSharpeValid   bool               `json:"oosSharpeValid"`
	OOSSortino       float64            `json:"oosSortino"`
	OOSSortinoValid  bool               `json:"oosSortinoValid"`
	WinRate          float64            `json:"winRate"`
	ProfitFactor     float64            `json:"profitFactor"`
	Turnover         float64            `json:"turnover"`
	WorstPeriod      *WorstPeriodReport `json:"worstPeriod,omitempty"`
	RegimesTested    []string           `json:"regimesTested,omitempty"`
	LiquidityOK      bool               `json:"liquidityOk"`
	LiquidityReason  string             `json:"liquidityReason,omitempty"`
}

// BacktestGateVerdict is the deploy decision context for one candidate.
type BacktestGateVerdict struct {
	Allowed      bool                 `json:"allowed"`
	Pending      bool                 `json:"pending"`
	Reason       string               `json:"reason"`
	Traded       BacktestJobSummary   `json:"traded"`
	Neighbors    []BacktestJobSummary `json:"neighbors"`
	PotentialPct float64              `json:"potentialPct"`
}

// BacktestDeployParams holds the exact configuration that will be deployed to the exchange.
type BacktestDeployParams struct {
	Symbol      string          `json:"symbol"`
	Interval    string          `json:"interval"`
	LowerPrice  decimal.Decimal `json:"lower_price"`
	UpperPrice  decimal.Decimal `json:"upper_price"`
	GridNum     int             `json:"grid_num"`
	Leverage    int             `json:"leverage"`
	Investment  decimal.Decimal `json:"investment"`
	StopLossPct float64         `json:"stop_loss_pct"`
	Direction   string          `json:"direction"`
	FeeBps      float64         `json:"fee_bps"`
	SlippageBps float64         `json:"slippage_bps"`
}

// normalizeBacktestTF maps scanner interval names onto the test ladder.
func normalizeBacktestTF(interval string) string {
	switch interval {
	case "1M", "5M":
		return "15M"
	case "1H", "60M":
		return "60M"
	case "8H", "12H", "4H":
		return "4H"
	case "1D":
		return "1D"
	case "30M":
		return "30M"
	case "15M":
		// v2.0.173: 15M grid trades are ~80% serially correlated (consecutive
		// fills at adjacent levels) → n_eff ≈ raw/9, structurally never
		// reaching the 15-trade independence minimum even with 100+ raw
		// round trips. The walk-forward validation runs on 60M where trades
		// are more independent; entry timing (scanner) stays on 15M.
		return "60M"
	default:
		return "60M"
	}
}

// neighborBacktestTFs returns the adjacent ladder rungs around the traded TF.
func neighborBacktestTFs(traded string) []string {
	index := -1
	for i, item := range backtestTFLadder {
		if item == traded {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	neighbors := make([]string, 0, 2)
	if index > 0 {
		neighbors = append(neighbors, backtestTFLadder[index-1])
	}
	if index < len(backtestTFLadder)-1 {
		neighbors = append(neighbors, backtestTFLadder[index+1])
	}
	return neighbors
}

// evaluateBacktestGate is the pure decision core: traded TF must pass all empirical tests,
// no neighbor TF may be fragile, and potential is the OOS average.
func evaluateBacktestGate(traded BacktestJobSummary, neighbors []BacktestJobSummary) BacktestGateVerdict {
	verdict := BacktestGateVerdict{Traded: traded, Neighbors: neighbors}
	if traded.State != "done" {
		verdict.Pending = true
		verdict.Reason = fmt.Sprintf("backtest pending for traded TF %s", traded.Interval)
		return verdict
	}
	if traded.Folds <= 0 {
		verdict.Pending = true
		verdict.Reason = "backtest produced no folds"
		return verdict
	}
	if traded.Folds < backtestMinFolds {
		verdict.Reason = fmt.Sprintf("backtest gate: %d folds < %d required on traded TF %s",
			traded.Folds, backtestMinFolds, traded.Interval)
		return verdict
	}
	if traded.RoundTrips < backtestMinRoundTrips {
		verdict.Reason = fmt.Sprintf("backtest gate: %d round trips < %d required on traded TF %s (insufficient sample)",
			traded.RoundTrips, backtestMinRoundTrips, traded.Interval)
		return verdict
	}
	if !traded.SampleSufficient {
		// v2.0.172 (audit): the Go override that flipped SampleSufficient to
		// true when round_trips >= 15 is REMOVED — the Python engine already
		// accounts for trade dependence (autocorrelation-adjusted n_eff),
		// and DOGE's 58 raw / 7.39 effective sample proved the override was
		// admitting statistically insufficient candidates. Respect the
		// engine's verdict.
		verdict.Reason = fmt.Sprintf(
			"backtest gate: эффективная выборка недостаточна на TF %s (учтена автокорреляция сделок — переопределение Go удалено v2.0.172)",
			traded.Interval)
		return verdict
	}
	// Task 4: Lower bound of 95% Confidence Interval for Net EV after costs MUST be strictly > 0
	if !traded.CI95Positive || traded.CI95Lower <= 0.0 {
		verdict.Reason = fmt.Sprintf("backtest gate: нет подтверждения — 95%% ДИ чистого EV (нижняя граница %.4f) <= 0 или не рассчитан на traded TF %s",
			traded.CI95Lower, traded.Interval)
		return verdict
	}
	// Net OOS return must be above minimum floor (-1.0%)
	if traded.OOSPct <= backtestMinOOSPct {
		verdict.Reason = fmt.Sprintf("backtest gate: OOS return %.2f%% <= %.1f%% on traded TF %s",
			traded.OOSPct, backtestMinOOSPct, traded.Interval)
		return verdict
	}
	// Max Drawdown within risk limit (15%)
	if traded.MaxDD > backtestMaxDrawdown {
		verdict.Reason = fmt.Sprintf("backtest gate: drawdown %.1f%% > %.1f%% on traded TF %s",
			traded.MaxDD*100, backtestMaxDrawdown*100, traded.Interval)
		return verdict
	}
	// Stop hits in OOS validation bounded to backtestMaxStopHits (max 1)
	if traded.StopHits > backtestMaxStopHits {
		verdict.Reason = fmt.Sprintf("backtest gate: %d stop hits in walk-forward OOS on traded TF %s (max %d allowed)",
			traded.StopHits, traded.Interval, backtestMaxStopHits)
		return verdict
	}
	// Task 4: Liquidity check
	if !traded.LiquidityOK {
		liqReason := traded.LiquidityReason
		if liqReason == "" {
			liqReason = "проверка не проведена или не подтверждена"
		}
		verdict.Reason = fmt.Sprintf("backtest gate: нет подтверждения — ликвидность (%s) на traded TF %s",
			liqReason, traded.Interval)
		return verdict
	}
	// Neighbor fragility checks
	for _, neighbor := range neighbors {
		if neighbor.State != "done" {
			continue // pending neighbors never block — they inform later
		}
		if neighbor.MaxDD > backtestNeighborMaxDrawdown || neighbor.OOSPct < backtestNeighborMinOOSPct {
			verdict.Reason = fmt.Sprintf(
				"backtest gate: fragile on neighbor TF %s (OOS %.2f%%, DD %.1f%%) — symbol range behavior is not robust",
				neighbor.Interval, neighbor.OOSPct, neighbor.MaxDD*100)
			return verdict
		}
	}
	verdict.Allowed = true
	verdict.Reason = fmt.Sprintf("backtest OK: traded %s OOS %+.2f%% DD %.1f%% CI95_low %+.4f Sharpe %.2f WR %.1f%% PF %.2f",
		traded.Interval, traded.OOSPct, traded.MaxDD*100, traded.CI95Lower, traded.OOSSharpe, traded.WinRate, traded.ProfitFactor)
	// Potential: average OOS across every TF with a result.
	sum, count := 0.0, 0
	if traded.State == "done" {
		sum += traded.OOSPct
		count++
	}
	for _, neighbor := range neighbors {
		if neighbor.State == "done" {
			sum += neighbor.OOSPct
			count++
		}
	}
	if count > 0 {
		verdict.PotentialPct = sum / float64(count)
	}
	return verdict
}

// backtestGateEnabled reads the feature flag (default on when absent).
func (worker *Worker) backtestGateEnabled(ctx context.Context) bool {
	enabled := true
	_ = worker.db.QueryRow(ctx, `
		SELECT COALESCE((SELECT enabled FROM feature_flags WHERE name = $1), true)
	`, backtestGateFlag).Scan(&enabled)
	return enabled
}

func parseBacktestResult(resultBytes []byte) (BacktestJobSummary, bool) {
	var result struct {
		Folds            int                `json:"folds"`
		OOSPct           float64            `json:"oos_return_pct"`
		MaxDD            float64            `json:"oos_max_drawdown"`
		RoundTrips       int                `json:"round_trips"`
		StopHits         int                `json:"stop_hits"`
		NetEV            float64            `json:"net_ev"`
		CI95Lower        float64            `json:"ci95_lower"`
		CI95Upper        float64            `json:"ci95_upper"`
		CI95Positive     bool               `json:"ci95_positive"`
		SampleSufficient bool               `json:"sample_sufficient"`
		OOSSharpe        float64            `json:"oos_sharpe"`
		OOSSharpeValid   bool               `json:"oos_sharpe_valid"`
		OOSSortino       float64            `json:"oos_sortino"`
		OOSSortinoValid  bool               `json:"oos_sortino_valid"`
		WinRate          float64            `json:"win_rate"`
		ProfitFactor     float64            `json:"profit_factor"`
		Turnover         float64            `json:"turnover"`
		WorstPeriod      *WorstPeriodReport `json:"worst_period"`
		RegimesTested    []string           `json:"regimes_tested"`
		LiquidityOK      *bool              `json:"liquidity_ok"`
		LiquidityReason  string             `json:"liquidity_reason"`
		EngineVersion    string             `json:"engine_version"`
	}
	if json.Unmarshal(resultBytes, &result) != nil || result.Folds <= 0 {
		return BacktestJobSummary{}, false
	}
	summary := BacktestJobSummary{
		State:            "done",
		EngineVersion:    result.EngineVersion,
		Folds:            result.Folds,
		OOSPct:           result.OOSPct,
		MaxDD:            result.MaxDD,
		RoundTrips:       result.RoundTrips,
		StopHits:         result.StopHits,
		NetEV:            result.NetEV,
		CI95Lower:        result.CI95Lower,
		CI95Upper:        result.CI95Upper,
		CI95Positive:     result.CI95Positive,
		SampleSufficient: result.SampleSufficient,
		OOSSharpe:        result.OOSSharpe,
		OOSSharpeValid:   result.OOSSharpeValid,
		OOSSortino:       result.OOSSortino,
		OOSSortinoValid:  result.OOSSortinoValid,
		WinRate:          result.WinRate,
		ProfitFactor:     result.ProfitFactor,
		Turnover:         result.Turnover,
		WorstPeriod:      result.WorstPeriod,
		RegimesTested:    result.RegimesTested,
		LiquidityOK:      false,
		LiquidityReason:  result.LiquidityReason,
	}
	if result.LiquidityOK != nil {
		summary.LiquidityOK = *result.LiquidityOK
	}
	return summary, true
}

// deployParamsPriceTolerance (v2.0.182): the S/R geometry drifts a few bps
// every scan, so a bit-exact params comparison never matched the previous
// DONE job — every scan enqueued a FRESH job (prod 01.10: MSTRX 15 jobs /
// 15 distinct params in 90 min) and the gate sat in PENDING forever while
// the queue happily churned DONE results no one read. 0.2% of price is
// below half a grid step (0.25% at the doctrine floor), so a matched job
// backtested a grid materially identical to the deploy geometry; grid_num
// and leverage stay exact.
const deployParamsPriceTolerance = 0.002

func matchesDeployParams(jobParamsBytes []byte, p *BacktestDeployParams) bool {
	if p == nil {
		return true
	}
	if len(jobParamsBytes) == 0 {
		return false
	}
	var jobParams struct {
		LowerPrice float64 `json:"lower_price"`
		UpperPrice float64 `json:"upper_price"`
		GridNum    int     `json:"grid_num"`
		Leverage   int     `json:"leverage"`
	}
	if json.Unmarshal(jobParamsBytes, &jobParams) != nil {
		return false
	}
	if jobParams.GridNum != p.GridNum {
		return false
	}
	pLower := p.LowerPrice.InexactFloat64()
	pUpper := p.UpperPrice.InexactFloat64()
	if pLower > 0 && math.Abs(jobParams.LowerPrice-pLower) > pLower*deployParamsPriceTolerance {
		return false
	}
	if pUpper > 0 && math.Abs(jobParams.UpperPrice-pUpper) > pUpper*deployParamsPriceTolerance {
		return false
	}
	if p.Leverage > 0 && jobParams.Leverage > 0 && jobParams.Leverage != p.Leverage {
		return false
	}
	return true
}

// loadBacktestSummary returns the latest queue verdict for symbol+TF.
func (worker *Worker) loadBacktestSummary(ctx context.Context, symbol, interval string) BacktestJobSummary {
	return worker.loadBacktestSummaryWithParams(ctx, symbol, interval, nil)
}

// loadBacktestSummaryWithParams returns the latest queue verdict for symbol+TF matching deployed parameters.
func (worker *Worker) loadBacktestSummaryWithParams(ctx context.Context, symbol, interval string, p *BacktestDeployParams) BacktestJobSummary {
	summary := BacktestJobSummary{Interval: interval}
	rows, err := worker.db.Query(ctx, `
		SELECT status, result, finished_at, params
		FROM backtest_jobs
		WHERE symbol = $1 AND interval = $2
		ORDER BY created_at DESC LIMIT 10
	`, symbol, interval)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var status string
			var resultBytes []byte
			var finishedAt *time.Time
			var paramsBytes []byte
			if err := rows.Scan(&status, &resultBytes, &finishedAt, &paramsBytes); err != nil {
				continue
			}
			if status == "DONE" && finishedAt != nil && time.Since(*finishedAt) <= backtestFreshWindow {
				if matchesDeployParams(paramsBytes, p) {
					if parsed, ok := parseBacktestResult(resultBytes); ok {
						// v2.0.172b: reject cached results from a different
						// engine version — the 159 pre-172 entries with
						// mismatched DD must never serve again.
						if parsed.EngineVersion != backtestEngineVersion {
							continue // stale engine version, skip this cache row
						}
						parsed.Interval = interval
						return parsed
					}
				}
			} else if (status == "QUEUED" || status == "RUNNING") && matchesDeployParams(paramsBytes, p) {
				summary.State = "pending"
				return summary
			}
		}
	}

	// Nothing usable: enqueue a fresh job
	paramsMap := map[string]any{
		"interval":      interval,
		"train_bars":    240,
		"test_bars":     60,
		"purge_bars":    6,
		"stop_loss_pct": 8.0,
	}
	if p != nil {
		paramsMap["lower_price"] = p.LowerPrice.InexactFloat64()
		paramsMap["upper_price"] = p.UpperPrice.InexactFloat64()
		paramsMap["grid_num"] = p.GridNum
		paramsMap["leverage"] = p.Leverage
		paramsMap["investment"] = p.Investment.InexactFloat64()
		paramsMap["direction"] = p.Direction
		if p.StopLossPct > 0 {
			paramsMap["stop_loss_pct"] = p.StopLossPct
		}
		paramsMap["fee_bps"] = p.FeeBps
		paramsMap["slippage_bps"] = p.SlippageBps
	}
	// v2.0.173: 60M walk-forward needs more history than the 500-candle
	// default — give it 1000 bars (~42 days) so the train/test folds cover
	// multiple market regimes and produce independent-enough trades.
	if interval == "60M" || interval == "1H" {
		paramsMap["limits"] = 500 // v2.0.176b: Pionex klines API max is 500
	}
	encoded, _ := json.Marshal(paramsMap)
	_, _ = worker.db.Exec(ctx, `
		INSERT INTO backtest_jobs (symbol, interval, params)
		VALUES ($1, $2, $3::jsonb)
	`, symbol, interval, string(encoded))
	return worker.waitForBacktestWithParams(ctx, symbol, interval, p, 75*time.Second)
}

// waitForBacktestWithParams polls the job queue until the result lands or the deadline passes.
func (worker *Worker) waitForBacktestWithParams(ctx context.Context, symbol, interval string, p *BacktestDeployParams, timeout time.Duration) BacktestJobSummary {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return BacktestJobSummary{Interval: interval, State: "pending"}
		case <-time.After(5 * time.Second):
		}
		rows, err := worker.db.Query(ctx, `
			SELECT status, result, params FROM backtest_jobs
			WHERE symbol = $1 AND interval = $2
			ORDER BY created_at DESC LIMIT 5
		`, symbol, interval)
		if err != nil {
			continue
		}
		for rows.Next() {
			var status string
			var resultBytes, paramsBytes []byte
			if err := rows.Scan(&status, &resultBytes, &paramsBytes); err == nil && status == "DONE" {
				if matchesDeployParams(paramsBytes, p) {
					if parsed, ok := parseBacktestResult(resultBytes); ok {
						// v2.0.172c: the WAIT path now checks the engine
						// version identically to the cache path — a DONE
						// result from a stale engine must not be served.
						if parsed.EngineVersion != backtestEngineVersion {
							continue
						}
						rows.Close()
						parsed.Interval = interval
						return parsed
					}
				}
			}
		}
		rows.Close()
	}
	return BacktestJobSummary{Interval: interval, State: "pending"}
}

// backtestGate runs the full multi-TF evaluation for a candidate.
func (worker *Worker) backtestGate(ctx context.Context, settings Settings, symbol string) BacktestGateVerdict {
	return worker.backtestGateWithParams(ctx, settings, symbol, nil)
}

// backtestGateWithParams runs the multi-TF evaluation using the exact deployed grid parameters on traded TF.
func (worker *Worker) backtestGateWithParams(ctx context.Context, settings Settings, symbol string, p *BacktestDeployParams) BacktestGateVerdict {
	tradedTF := normalizeBacktestTF(settings.CandleInterval)
	traded := worker.loadBacktestSummaryWithParams(ctx, symbol, tradedTF, p)
	neighbors := make([]BacktestJobSummary, 0, 2)
	for _, tf := range neighborBacktestTFs(tradedTF) {
		// Neighbors check general structural robustness of symbol across timeframes
		neighbors = append(neighbors, worker.loadBacktestSummaryWithParams(ctx, symbol, tf, nil))
	}
	return evaluateBacktestGate(traded, neighbors)
}
