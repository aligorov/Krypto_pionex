package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Gate value aggregation (v2.0.138 package D). The shadow portfolio
// (migration 0031) already replays top-scored REJECTED candidates through
// the paper-model core and persists the counterfactual outcome
// (shadow_candidates.outcome_pnl_usdt) — but the data was never aggregated,
// so "did each gate prevent loss or block profit" was unanswerable. This
// module reads the last 7 days of simulated shadow outcomes joined to the
// rejected candidates' rejection_reason, attributes each outcome to the
// gate(s) that rejected it, and snapshots per-gate value into
// gate_value_snapshots (migration 0053).
//
// Sign conventions:
//   - prevented_loss_usdt = −Σ min(0, pnl)  — the positive amount of loss
//     the gate kept out of the fleet;
//   - missed_profit_usdt  =  Σ max(0, pnl)  — the positive outcome it
//     blocked (the gate's cost);
//   - net_value_usdt      = prevented_loss − missed_profit — positive means
//     the gate pays for itself over the window;
//   - win_rate_blocked    = profitable outcomes / attributed samples.
//
// Attribution double-count (intentional): rejection_reason strings are
// compound ("; "-joined multi-gate rejections from the scanner), and a row
// whose reason mentions several gates attributes to EVERY matched gate —
// each gate gets the sample because each one alone would have blocked the
// entry. Per-gate sample counts therefore overlap: Σ samples across gates
// exceeds the shadow-row count whenever compound reasons exist. Reasons
// matching no matcher (AI rejections, fleet-level pauses, new gates) fall
// to the OTHER bucket so nothing is silently dropped.

// gateKeyOther is the fallback bucket for rejection reasons no matcher
// recognizes.
const gateKeyOther = "OTHER"

// gateValueWindow is the shadow-outcome lookback aggregated per report.
const gateValueWindow = "7d"

// gateMatchers maps rejection_reason substrings to stable gate keys. The
// reason is lowercased before matching (case-insensitive, Cyrillic
// included); a matcher fires on substring containment — all listed parts
// for multi-part matchers. The list is ordered to mirror the entry chain
// (scanner core gates first), though attribution is match-ALL, not
// first-match: a compound reason credits every gate it mentions (see the
// double-count note above). Substrings come verbatim from the persisted
// texts in marketdata/scanner.go, marketdata/targets.go (fee-gate),
// autogrid/worker.go (Стакан/Knife Pause), autogrid/entry_chain.go
// (каскад/макро) and autogrid/macro_gate.go (macro beta/alt-drain).
var gateMatchers = []struct {
	key   string
	match func(lowered string) bool
}{
	{"OU_HALF_LIFE", gateSubstr("ou half-life")},
	{"ANTI_FOMO", gateSubstr("anti-fomo")},
	{"MACRO", gateAny("macro", "макро")},
	{"CONFLUENCE_HURST", gateSubstr("confluence veto", "hurst")},
	{"SEMI_TREND_DEAD_ZONE", gateSubstr("полутренд")},
	{"TREND_TOO_STRONG", gateSubstr("trend too strong")},
	{"VOLATILITY_CAP", gateSubstr("volatility above")},
	{"KNIFE_PAUSE", gateSubstr("knife pause")},
	{"DEPTH_CUSHION", gateSubstr("стакан")},
	{"LIQ_CASCADE", gateSubstr("каскад")},
	{"MACRO_ALT_DRAIN", gateSubstr("alt-drain")},
	{"SQUEEZE", gateSubstr("squeeze")},
	// Extras below are the remaining persisted scanner/worker gate texts —
	// each is a distinct gate in autogrid_settings or a dedicated veto.
	{"CONFLUENCE_FLOW", gateSubstr("flow supports")},
	{"VOLATILITY_FLOOR", gateSubstr("volatility below")},
	{"FEES", gateSubstr("fee-gate")},
	{"KAUFMAN_ER", gateSubstr("kaufman er")},
	{"FLASH_SPIKE", gateSubstr("flash candle spike")},
	{"NON_ASCII_SYMBOL", gateSubstr("не-ascii")},
	{"VOLUME_FLOOR", gateSubstr("turnover below")},
	{"MODEL_EV", gateSubstr("model ev")},
	{"MODEL_SHARPE", gateSubstr("model sharpe")},
	{"MODEL_DRAWDOWN", gateSubstr("model max drawdown")},
	{"MODEL_PROFIT_FACTOR", gateSubstr("model profit factor")},
}

// gateSubstr builds a matcher requiring every listed substring (AND) of the
// lowercased rejection reason. Single-part matchers are plain containment.
func gateSubstr(parts ...string) func(string) bool {
	return func(lowered string) bool {
		for _, part := range parts {
			if !strings.Contains(lowered, part) {
				return false
			}
		}
		return true
	}
}

// gateAny builds a matcher firing on ANY listed substring (OR) — for gates
// whose persisted texts mix English and Russian spellings (macro).
func gateAny(parts ...string) func(string) bool {
	return func(lowered string) bool {
		for _, part := range parts {
			if strings.Contains(lowered, part) {
				return true
			}
		}
		return false
	}
}

// attributeGates returns every gate key the rejection reason matches, or
// [OTHER] when nothing matches (including empty reasons — the row must
// still be counted somewhere).
func attributeGates(reason string) []string {
	lowered := strings.ToLower(reason)
	var keys []string
	for _, m := range gateMatchers {
		if m.match(lowered) {
			keys = append(keys, m.key)
		}
	}
	if len(keys) == 0 {
		return []string{gateKeyOther}
	}
	return keys
}

// gateValueSample is one simulated shadow outcome attributed by the
// rejecting candidate's persisted rejection_reason.
type gateValueSample struct {
	reason string
	pnl    float64
}

// gateValueStat is the per-gate aggregation over the report window.
type gateValueStat struct {
	Gate          string
	Samples       int
	Wins          int // outcomes strictly above zero
	SumPnL        float64
	PreventedLoss float64 // −Σ min(0, pnl) ≥ 0
	MissedProfit  float64 // Σ max(0, pnl) ≥ 0
}

func (s *gateValueStat) add(pnl float64) {
	s.Samples++
	s.SumPnL += pnl
	if pnl < 0 {
		s.PreventedLoss += -pnl
	} else {
		s.MissedProfit += pnl
		if pnl > 0 {
			s.Wins++
		}
	}
}

// NetValue is prevented loss minus missed profit: positive = the gate paid
// for itself over the window.
func (s *gateValueStat) NetValue() float64 {
	return s.PreventedLoss - s.MissedProfit
}

// AvgOutcome is the mean counterfactual PnL of the outcomes the gate
// blocked. Samples are always ≥ 1 for stats that exist.
func (s *gateValueStat) AvgOutcome() float64 {
	return s.SumPnL / float64(s.Samples)
}

// WinRateBlocked is the share of blocked outcomes that were profitable.
func (s *gateValueStat) WinRateBlocked() float64 {
	return float64(s.Wins) / float64(s.Samples)
}

// aggregateGateValues attributes every sample to its matched gates and
// returns the stats sorted by gate key for deterministic inserts. Compound
// reasons credit each matched gate (documented double-count).
func aggregateGateValues(samples []gateValueSample) []gateValueStat {
	byGate := make(map[string]*gateValueStat)
	for _, sample := range samples {
		for _, key := range attributeGates(sample.reason) {
			stat, ok := byGate[key]
			if !ok {
				stat = &gateValueStat{Gate: key}
				byGate[key] = stat
			}
			stat.add(sample.pnl)
		}
	}
	stats := make([]gateValueStat, 0, len(byGate))
	for _, stat := range byGate {
		stats = append(stats, *stat)
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Gate < stats[j].Gate })
	return stats
}

// gateValueRound keeps the snapshot columns clean of float64 artifacts
// (1.2000000000000002) without losing cent-level precision.
func gateValueRound(x float64) float64 {
	return math.Round(x*1e6) / 1e6
}

// buildGateValueReport aggregates the last 7 days of simulated shadow
// outcomes per rejecting gate and snapshots them into
// gate_value_snapshots. Failure-tolerant: SQL errors are logged and the
// call returns — the report is telemetry and must never disturb the
// reconcile loop that invokes it (the caller owns the 24h throttle).
func (worker *Worker) buildGateValueReport(ctx context.Context) {
	// Unit workers are constructed without a pool; there is nothing to
	// aggregate.
	if worker == nil || worker.db == nil {
		return
	}
	rows, err := worker.db.Query(ctx, `
		SELECT c.rejection_reason, s.outcome_pnl_usdt
		FROM shadow_candidates s
		JOIN autogrid_candidates c ON c.id = s.candidate_id
		WHERE s.simulated_at > NOW() - INTERVAL '7 days'
		  AND s.outcome_pnl_usdt IS NOT NULL
	`)
	if err != nil {
		worker.logGateValueFailure("gate value report: load shadow outcomes failed", err)
		return
	}
	defer rows.Close()

	var samples []gateValueSample
	for rows.Next() {
		var reason *string // rejection_reason is nullable
		var pnl float64
		if err := rows.Scan(&reason, &pnl); err != nil {
			worker.logGateValueFailure("gate value report: scan shadow outcome failed", err)
			return
		}
		sample := gateValueSample{reason: "", pnl: pnl}
		if reason != nil {
			sample.reason = *reason
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		worker.logGateValueFailure("gate value report: iterate shadow outcomes failed", err)
		return
	}

	stats := aggregateGateValues(samples)
	details, err := json.Marshal(map[string]string{"window": gateValueWindow})
	if err != nil {
		worker.logGateValueFailure("gate value report: encode details failed", err)
		return
	}
	for _, stat := range stats {
		if _, err := worker.db.Exec(ctx, `
			INSERT INTO gate_value_snapshots (
				gate, samples, avg_outcome_pnl_usdt,
				prevented_loss_usdt, missed_profit_usdt, net_value_usdt,
				win_rate_blocked, details
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		`, stat.Gate, stat.Samples,
			gateValueRound(stat.AvgOutcome()),
			gateValueRound(stat.PreventedLoss),
			gateValueRound(stat.MissedProfit),
			gateValueRound(stat.NetValue()),
			gateValueRound(stat.WinRateBlocked()),
			string(details)); err != nil {
			worker.logGateValueFailure("gate value report: snapshot gate failed", err)
			return
		}
	}

	// Top-5 by net value — the one-line answer to "which gates pay for
	// themselves". Sorted descending; ties broken by gate key for stable
	// logs.
	top := append([]gateValueStat(nil), stats...)
	sort.SliceStable(top, func(i, j int) bool {
		if top[i].NetValue() == top[j].NetValue() {
			return top[i].Gate < top[j].Gate
		}
		return top[i].NetValue() > top[j].NetValue()
	})
	if len(top) > 5 {
		top = top[:5]
	}
	ranked := make([]string, 0, len(top))
	for _, stat := range top {
		ranked = append(ranked, fmt.Sprintf("%s:%+.2f(n=%d)", stat.Gate, stat.NetValue(), stat.Samples))
	}
	if worker.logger != nil {
		worker.logger.Info("gate value report (7d shadow counterfactual)",
			"component", "autogrid_worker",
			"shadow_rows", len(samples),
			"gates", len(stats),
			"top_by_net_value", strings.Join(ranked, ", "))
	}
}

// logGateValueFailure keeps the report failure-tolerant: a nil logger
// (bare unit workers) must not turn telemetry into a panic.
func (worker *Worker) logGateValueFailure(message string, err error) {
	if worker.logger != nil {
		worker.logger.Warn(message, "component", "autogrid_worker", "error", err)
	}
}
