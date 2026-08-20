package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Walk-forward backtest gate (release 3): a REAL deployment requires a fresh
// OOS verdict for the symbol on the TRADED timeframe, and treats neighbor
// timeframes as a fragility check. Multi-TF results feed a "potential"
// ranking signal — deliberately NOT a best-TF picker: selecting the best
// backtested timeframe would be in-sample selection, the exact trap the
// red-team review flagged.
const (
	backtestGateFlag    = "backtest_gate"
	backtestFreshWindow = 12 * time.Hour
	// Traded-TF hard ceilings: the parameters we are about to deploy must
	// have earned near-breakeven OOS with bounded drawdown and no stop-storm.
	backtestMaxDrawdown = 0.15
	// backtestMinOOSPct is the traded-TF OOS floor. A neutral grid's
	// walk-forward OOS includes trend segments the strategy deliberately
	// does not trade, so a small negative OOS with bounded drawdown and no
	// stop-storm is still a harvestable chopper — 0.0 rejected SNDKX/EDEN-
	// class flat choppers on a single trend fold (2026-08-20 external
	// audit §3). Drawdown, stop-storm and neighbor-fragility vetoes stand.
	backtestMinOOSPct = -1.5
	// Neighbor fragility ceiling: a healthy traded TF with a catastrophic
	// neighbor (MUBARAK-style 71% DD one TF away) means the symbol's range
	// behavior is fragile — regime shifts surface on other TFs first.
	backtestNeighborMaxDrawdown = 0.40
	backtestNeighborMinOOSPct   = -10.0
)

var backtestTFLadder = []string{"15M", "30M", "60M", "4H", "1D"}

// BacktestJobSummary is one timeframe's walk-forward verdict from the queue.
type BacktestJobSummary struct {
	Interval   string  `json:"interval"`
	State      string  `json:"state"` // done | pending | missing
	Folds      int     `json:"folds"`
	OOSPct     float64 `json:"oosPct"`
	MaxDD      float64 `json:"maxDd"`
	RoundTrips int     `json:"roundTrips"`
	StopHits   int     `json:"stopHits"`
}

// BacktestGateVerdict is the deploy decision context for one candidate.
type BacktestGateVerdict struct {
	Allowed   bool                 `json:"allowed"`
	Pending   bool                 `json:"pending"`
	Reason    string               `json:"reason"`
	Traded    BacktestJobSummary   `json:"traded"`
	Neighbors []BacktestJobSummary `json:"neighbors"`
	// PotentialPct is the average OOS across the timeframes with results —
	// a ranking signal for candidate priority, never a gate.
	PotentialPct float64 `json:"potentialPct"`
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
		return "15M"
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

// evaluateBacktestGate is the pure decision core: traded TF must pass, no
// done neighbor may be catastrophic, potential is the OOS average.
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
	if traded.OOSPct < backtestMinOOSPct {
		verdict.Reason = fmt.Sprintf("backtest gate: OOS %.2f%% < %.1f%% on traded TF %s",
			traded.OOSPct, backtestMinOOSPct, traded.Interval)
		return verdict
	}
	if traded.MaxDD > backtestMaxDrawdown {
		verdict.Reason = fmt.Sprintf("backtest gate: drawdown %.1f%% > %.0f%% on traded TF %s",
			traded.MaxDD*100, backtestMaxDrawdown*100, traded.Interval)
		return verdict
	}
	if traded.StopHits*2 > traded.Folds {
		verdict.Reason = fmt.Sprintf("backtest gate: %d stop hits in %d folds on traded TF %s",
			traded.StopHits, traded.Folds, traded.Interval)
		return verdict
	}
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
	verdict.Reason = fmt.Sprintf("backtest OK: traded %s OOS %+.2f%% DD %.1f%%",
		traded.Interval, traded.OOSPct, traded.MaxDD*100)
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

// loadBacktestSummary returns the latest queue verdict for symbol+TF,
// auto-enqueuing a fresh job when nothing usable exists.
func (worker *Worker) loadBacktestSummary(ctx context.Context, symbol, interval string) BacktestJobSummary {
	summary := BacktestJobSummary{Interval: interval}
	var status string
	var resultBytes []byte
	var finishedAt *time.Time
	err := worker.db.QueryRow(ctx, `
		SELECT status, result, finished_at
		FROM backtest_jobs
		WHERE symbol = $1 AND interval = $2
		ORDER BY created_at DESC LIMIT 1
	`, symbol, interval).Scan(&status, &resultBytes, &finishedAt)
	switch {
	case err == nil && status == "DONE" && finishedAt != nil &&
		time.Since(*finishedAt) <= backtestFreshWindow:
		var result struct {
			Folds      int     `json:"folds"`
			OOSPct     float64 `json:"oos_return_pct"`
			MaxDD      float64 `json:"oos_max_drawdown"`
			RoundTrips int     `json:"round_trips"`
			StopHits   int     `json:"stop_hits"`
		}
		if json.Unmarshal(resultBytes, &result) == nil && result.Folds > 0 {
			summary.State = "done"
			summary.Folds = result.Folds
			summary.OOSPct = result.OOSPct
			summary.MaxDD = result.MaxDD
			summary.RoundTrips = result.RoundTrips
			summary.StopHits = result.StopHits
			return summary
		}
		fallthrough
	case err == nil && (status == "QUEUED" || status == "RUNNING"):
		summary.State = "pending"
		return summary
	default:
		// Nothing usable: enqueue a fresh job, then WAIT for the quant
		// worker to finish it (typically ~30s) instead of punting the
		// candidate to the next scan — a 5-minute gap kills entries.
		_, _ = worker.db.Exec(ctx, `
			INSERT INTO backtest_jobs (symbol, interval, params)
			VALUES ($1, $2, $3::jsonb)
		`, symbol, interval, `{"train_bars":240,"test_bars":60,"purge_bars":6,"stop_loss_pct":8}`)
		return worker.waitForBacktest(ctx, symbol, interval, 75*time.Second)
	}
}

// waitForBacktest polls the job queue until the result lands or the
// deadline passes. The quant worker completes jobs in ~30s.
func (worker *Worker) waitForBacktest(ctx context.Context, symbol, interval string, timeout time.Duration) BacktestJobSummary {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return BacktestJobSummary{Interval: interval, State: "pending"}
		case <-time.After(5 * time.Second):
		}
		var status string
		var resultBytes []byte
		err := worker.db.QueryRow(ctx, `
			SELECT status, result FROM backtest_jobs
			WHERE symbol = $1 AND interval = $2
			ORDER BY created_at DESC LIMIT 1
		`, symbol, interval).Scan(&status, &resultBytes)
		if err != nil || status != "DONE" {
			continue
		}
		var result struct {
			Folds      int     `json:"folds"`
			OOSPct     float64 `json:"oos_return_pct"`
			MaxDD      float64 `json:"oos_max_drawdown"`
			RoundTrips int     `json:"round_trips"`
			StopHits   int     `json:"stop_hits"`
		}
		if json.Unmarshal(resultBytes, &result) == nil && result.Folds > 0 {
			return BacktestJobSummary{
				Interval: interval, State: "done",
				Folds: result.Folds, OOSPct: result.OOSPct,
				MaxDD: result.MaxDD, RoundTrips: result.RoundTrips,
				StopHits: result.StopHits,
			}
		}
	}
	return BacktestJobSummary{Interval: interval, State: "pending"}
}

// backtestGate runs the full multi-TF evaluation for a candidate.
func (worker *Worker) backtestGate(ctx context.Context, settings Settings, symbol string) BacktestGateVerdict {
	tradedTF := normalizeBacktestTF(settings.CandleInterval)
	traded := worker.loadBacktestSummary(ctx, symbol, tradedTF)
	neighbors := make([]BacktestJobSummary, 0, 2)
	for _, tf := range neighborBacktestTFs(tradedTF) {
		neighbors = append(neighbors, worker.loadBacktestSummary(ctx, symbol, tf))
	}
	return evaluateBacktestGate(traded, neighbors)
}
