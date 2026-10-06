package autogrid

// v2.0.184 decision intelligence — read side (docs/decision-analysis-plan-2026-10-01.md §7).
// Observation, not participation: every query here is a plain SELECT over
// migration 0062 artifacts (entry_decisions / entry_episodes /
// shadow_candidates / replay_runs / replay_run_items / gate_quality_daily);
// the only write is the operator-driven replay enqueue, which creates an
// experiment row and never touches the trading path (AGENTS.md rule: replay
// and shadow never open bots or submit orders).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// ErrDecisionNotFound separates "no such row" from query failures so the API
// layer can answer 404 instead of 500.
var ErrDecisionNotFound = errors.New("решение не найдено")

// ErrReplayRunNotFound marks a missing replay run row for the 404 path.
var ErrReplayRunNotFound = errors.New("replay-прогон не найден")

// DecisionHistoryItem is one row of GET /api/autogrid/decisions: the journal
// entry plus the episode/shadow fate that followed it.
type DecisionHistoryItem struct {
	ID              string           `json:"id"`
	CreatedAt       time.Time        `json:"createdAt"`
	DecisionAt      *time.Time       `json:"decisionAt"`
	Symbol          string           `json:"symbol"`
	Path            string           `json:"path"`
	Fleet           string           `json:"fleet"`
	Outcome         string           `json:"outcome"`
	Code            string           `json:"code"`
	Reason          string           `json:"reason"`
	Stage           string           `json:"stage"`
	DataQuality     string           `json:"dataQuality"`
	EpisodeID       *string          `json:"episodeId"`
	AttemptNo       int              `json:"attemptNo"`
	DirectionBefore *string          `json:"directionBefore"`
	DirectionAfter  *string          `json:"directionAfter"`
	GateTrace       []map[string]any `json:"gateTrace"`
	ConfigSnapshot  map[string]any   `json:"configSnapshot"`
	Features        map[string]any   `json:"features"`
	PriceAtDecision *decimal.Decimal `json:"priceAtDecision"`
	PriceSource     *string          `json:"priceSource"`
	Episode         *DecisionEpisode `json:"episode"`
}

// DecisionEpisode is the entry_episodes state of the linked opportunity.
type DecisionEpisode struct {
	Symbol      string           `json:"symbol"`
	Direction   string           `json:"direction"`
	Regime      string           `json:"regime"`
	Attempts    int              `json:"attempts"`
	Disposition string           `json:"disposition"`
	LastSeen    *time.Time       `json:"lastSeen"`
	Shadow      *ShadowFateState `json:"shadow"`
}

// ShadowFateState is what actually happened to the virtual observation of a
// rejected episode: the two state machines (position × calculation) plus the
// model PnL when the outcome completed.
type ShadowFateState struct {
	ID             int64            `json:"id"`
	PosState       string           `json:"posState"`
	CalcState      string           `json:"calcState"`
	OutcomePnlUsdt *decimal.Decimal `json:"outcomePnlUsdt"`
	OutcomeReason  *string          `json:"outcomeReason"`
}

// decisionHistorySelect loads journal rows with their episode and the latest
// shadow observation of that episode (LATERAL: one shadow per episode row,
// never a fan-out that would multiply the decisions list). The shadow link
// accepts both 0062 styles — decision_id and episode_id — so either linkage
// written by the recorder resolves the same fate.
const decisionHistorySelect = `
	SELECT ed.id, ed.created_at, ed.decision_at, ed.symbol, ed.path, ed.fleet,
	       ed.outcome, ed.code, ed.reason, ed.stage, ed.data_quality,
	       ed.episode_id, ed.attempt_no, ed.direction_before, ed.direction_after,
	       ed.gate_trace, ed.config_snapshot, ed.features,
	       ed.price_at_decision, ed.price_source,
	       ee.symbol, ee.direction, ee.regime, ee.attempts, ee.disposition, ee.last_seen,
	       sc.id, sc.pos_state, sc.calc_state, sc.outcome_pnl_usdt, sc.outcome_reason
	FROM entry_decisions ed
	LEFT JOIN entry_episodes ee ON ee.id = ed.episode_id
	LEFT JOIN LATERAL (
	    SELECT id, pos_state, calc_state, outcome_pnl_usdt, outcome_reason
	    FROM shadow_candidates
	    WHERE decision_id = ed.id OR episode_id = ed.episode_id
	    ORDER BY captured_at DESC
	    LIMIT 1
	) sc ON TRUE
`

// ListDecisionHistory returns the newest entry_decisions, optionally scoped
// to one symbol, each joined with its episode/shadow fate.
func (s *Service) ListDecisionHistory(ctx context.Context, symbol string, limit int) ([]DecisionHistoryItem, error) {
	rows, err := s.db.Query(ctx, decisionHistorySelect+`
		WHERE ($1 = '' OR ed.symbol = $1)
		ORDER BY ed.created_at DESC
		LIMIT $2
	`, symbol, limit)
	if err != nil {
		return nil, fmt.Errorf("list entry decisions: %w", err)
	}
	defer rows.Close()
	items := make([]DecisionHistoryItem, 0)
	for rows.Next() {
		item, scanErr := scanDecisionHistoryRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// GetDecisionDetail returns a single decision with its trace and episode fate.
func (s *Service) GetDecisionDetail(ctx context.Context, id string) (*DecisionHistoryItem, error) {
	rows, err := s.db.Query(ctx, decisionHistorySelect+`
		WHERE ed.id = $1
		LIMIT 1
	`, id)
	if err != nil {
		return nil, fmt.Errorf("load entry decision: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("load entry decision: %w", err)
		}
		return nil, ErrDecisionNotFound
	}
	return scanDecisionHistoryRow(rows)
}

func scanDecisionHistoryRow(rows pgx.Rows) (*DecisionHistoryItem, error) {
	var item DecisionHistoryItem
	var rawTrace, rawSnapshot, rawFeatures any
	var eeSymbol, eeDirection, eeRegime, eeDisposition *string
	var eeAttempts *int
	var eeLastSeen *time.Time
	var scID *int64
	var scPosState, scCalcState, scOutcomeReason *string
	var scOutcomePnl *decimal.Decimal
	if err := rows.Scan(
		&item.ID, &item.CreatedAt, &item.DecisionAt, &item.Symbol, &item.Path,
		&item.Fleet, &item.Outcome, &item.Code, &item.Reason, &item.Stage,
		&item.DataQuality, &item.EpisodeID, &item.AttemptNo,
		&item.DirectionBefore, &item.DirectionAfter,
		&rawTrace, &rawSnapshot, &rawFeatures,
		&item.PriceAtDecision, &item.PriceSource,
		&eeSymbol, &eeDirection, &eeRegime, &eeAttempts, &eeDisposition, &eeLastSeen,
		&scID, &scPosState, &scCalcState, &scOutcomePnl, &scOutcomeReason,
	); err != nil {
		return nil, fmt.Errorf("scan entry decision: %w", err)
	}
	item.GateTrace = asObjectList(rawTrace)
	item.ConfigSnapshot = asObject(rawSnapshot)
	item.Features = asObject(rawFeatures)
	if item.EpisodeID != nil {
		episode := &DecisionEpisode{Regime: "UNKNOWN", Disposition: "OPEN", LastSeen: eeLastSeen}
		if eeSymbol != nil {
			episode.Symbol = *eeSymbol
		} else {
			episode.Symbol = item.Symbol
		}
		if eeDirection != nil {
			episode.Direction = *eeDirection
		}
		if eeRegime != nil {
			episode.Regime = *eeRegime
		}
		if eeAttempts != nil {
			episode.Attempts = *eeAttempts
		}
		if eeDisposition != nil {
			episode.Disposition = *eeDisposition
		}
		if scID != nil {
			episode.Shadow = &ShadowFateState{
				ID: *scID, PosState: stringOrDefault(scPosState, "OPEN"),
				CalcState:      stringOrDefault(scCalcState, "PENDING"),
				OutcomePnlUsdt: scOutcomePnl, OutcomeReason: scOutcomeReason,
			}
		}
		item.Episode = episode
	}
	return &item, nil
}

// ReplayRunSummary is one replay_runs row. stats carries the N/K/M/L
// contract (checked / verdicts_changed / full_chain_available /
// outcomes_available) plus not_replayable causes; effect carries the
// verdict-level model effect {delta_pnl_proxy, prevented_losses, missed_profits}.
type ReplayRunSummary struct {
	ID              string         `json:"id"`
	CreatedAt       time.Time      `json:"createdAt"`
	PeriodFrom      time.Time      `json:"periodFrom"`
	PeriodTo        time.Time      `json:"periodTo"`
	Baseline        map[string]any `json:"baseline"`
	Overrides       map[string]any `json:"overrides"`
	CodeVersion     string         `json:"codeVersion"`
	Status          string         `json:"status"`
	StatusReason    string         `json:"statusReason"`
	Stats           map[string]any `json:"stats"`
	Effect          map[string]any `json:"effect"`
	ModelCalibrated *bool          `json:"modelCalibrated"`
	FinishedAt      *time.Time     `json:"finishedAt"`
}

// ReplayRunItem is one replay_run_items verdict: what the stored decision
// was, what it becomes under the override, and whether the whole chain
// after the overridden gate was even available to replay.
type ReplayRunItem struct {
	ID                 int64            `json:"id"`
	DecisionID         string           `json:"decisionId"`
	Symbol             string           `json:"symbol"`
	Verdict            string           `json:"verdict"`
	NotReplayableCause string           `json:"notReplayableCause"`
	BaseOutcome        string           `json:"baseOutcome"`
	NewOutcome         string           `json:"newOutcome"`
	ChainFollowed      bool             `json:"chainFollowed"`
	EpisodeOutcomePnl  *decimal.Decimal `json:"episodeOutcomePnl"`
	CreatedAt          time.Time        `json:"createdAt"`
}

// ListReplayRuns returns the newest replay experiments (immutable rows — a
// new run never overwrites an old one).
func (s *Service) ListReplayRuns(ctx context.Context, limit int) ([]ReplayRunSummary, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, created_at, period_from, period_to, baseline, overrides,
		       code_version, status, status_reason, stats, effect,
		       model_calibrated, finished_at
		FROM replay_runs
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list replay runs: %w", err)
	}
	defer rows.Close()
	items := make([]ReplayRunSummary, 0)
	for rows.Next() {
		var item ReplayRunSummary
		var rawBaseline, rawOverrides, rawStats, rawEffect any
		if err := rows.Scan(
			&item.ID, &item.CreatedAt, &item.PeriodFrom, &item.PeriodTo,
			&rawBaseline, &rawOverrides, &item.CodeVersion, &item.Status,
			&item.StatusReason, &rawStats, &rawEffect,
			&item.ModelCalibrated, &item.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan replay run: %w", err)
		}
		item.Baseline = asObject(rawBaseline)
		item.Overrides = asObject(rawOverrides)
		item.Stats = asObject(rawStats)
		item.Effect = asObject(rawEffect)
		items = append(items, item)
	}
	return items, rows.Err()
}

// GetReplayRun returns one run plus its per-decision verdicts.
func (s *Service) GetReplayRun(ctx context.Context, id string) (*ReplayRunSummary, []ReplayRunItem, error) {
	runRows, err := s.db.Query(ctx, `
		SELECT id, created_at, period_from, period_to, baseline, overrides,
		       code_version, status, status_reason, stats, effect,
		       model_calibrated, finished_at
		FROM replay_runs
		WHERE id = $1
	`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("load replay run: %w", err)
	}
	defer runRows.Close()
	if !runRows.Next() {
		if err := runRows.Err(); err != nil {
			return nil, nil, fmt.Errorf("load replay run: %w", err)
		}
		return nil, nil, ErrReplayRunNotFound
	}
	var run ReplayRunSummary
	var rawBaseline, rawOverrides, rawStats, rawEffect any
	if err := runRows.Scan(
		&run.ID, &run.CreatedAt, &run.PeriodFrom, &run.PeriodTo,
		&rawBaseline, &rawOverrides, &run.CodeVersion, &run.Status,
		&run.StatusReason, &rawStats, &rawEffect,
		&run.ModelCalibrated, &run.FinishedAt,
	); err != nil {
		return nil, nil, fmt.Errorf("scan replay run: %w", err)
	}
	run.Baseline = asObject(rawBaseline)
	run.Overrides = asObject(rawOverrides)
	run.Stats = asObject(rawStats)
	run.Effect = asObject(rawEffect)

	itemRows, err := s.db.Query(ctx, `
		SELECT id, decision_id, symbol, verdict, not_replayable_cause,
		       base_outcome, new_outcome, chain_followed, episode_outcome_pnl, created_at
		FROM replay_run_items
		WHERE run_id = $1
		ORDER BY created_at ASC
		LIMIT 2000
	`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("list replay run items: %w", err)
	}
	defer itemRows.Close()
	items := make([]ReplayRunItem, 0)
	for itemRows.Next() {
		var item ReplayRunItem
		if err := itemRows.Scan(
			&item.ID, &item.DecisionID, &item.Symbol, &item.Verdict,
			&item.NotReplayableCause, &item.BaseOutcome, &item.NewOutcome,
			&item.ChainFollowed, &item.EpisodeOutcomePnl, &item.CreatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scan replay run item: %w", err)
		}
		items = append(items, item)
	}
	return &run, items, itemRows.Err()
}

// GateQualityRow is one gate_quality_daily aggregate: per-gate, per-regime
// counters with shadow coverage and the model ±$ of blocked entries.
type GateQualityRow struct {
	Window            string           `json:"window"`
	WindowStart       time.Time        `json:"windowStart"`
	ComputedAt        time.Time        `json:"computedAt"`
	Gate              string           `json:"gate"`
	Regime            string           `json:"regime"`
	Episodes          int              `json:"episodes"`
	Decisions         int              `json:"decisions"`
	Coverage          *decimal.Decimal `json:"coverage"`
	SkipReasons       map[string]any   `json:"skipReasons"`
	CompletedOutcomes int              `json:"completedOutcomes"`
	OpenOutcomes      int              `json:"openOutcomes"`
	BlockedModelPnl   *decimal.Decimal `json:"blockedModelPnl"`
	BlockedPnlParts   map[string]any   `json:"blockedModelPnlParts"`
	ProofStrength     string           `json:"proofStrength"`
	Calibration       map[string]any   `json:"calibration"`
	Notes             map[string]any   `json:"notes"`
}

// ListGateQuality returns the stored per-regime aggregates for one window
// (24H | 7D). Aggregation itself is owned by the worker; this read side
// only serves what was computed.
func (s *Service) ListGateQuality(ctx context.Context, window string) ([]GateQualityRow, error) {
	rows, err := s.db.Query(ctx, `
		SELECT window_kind, window_start, computed_at, gate, regime,
		       episodes, decisions, coverage, skip_reasons,
		       completed_outcomes, open_outcomes,
		       blocked_model_pnl, blocked_model_pnl_parts,
		       proof_strength, calibration, notes
		FROM gate_quality_daily
		WHERE window_kind = $1
		  AND window_start = (
		      SELECT MAX(window_start) FROM gate_quality_daily WHERE window_kind = $1
		  )
		ORDER BY gate, regime
	`, window)
	if err != nil {
		return nil, fmt.Errorf("list gate quality: %w", err)
	}
	defer rows.Close()
	items := make([]GateQualityRow, 0)
	for rows.Next() {
		var item GateQualityRow
		var rawSkip, rawParts, rawCalibration, rawNotes any
		if err := rows.Scan(
			&item.Window, &item.WindowStart, &item.ComputedAt, &item.Gate,
			&item.Regime, &item.Episodes, &item.Decisions, &item.Coverage,
			&rawSkip, &item.CompletedOutcomes, &item.OpenOutcomes,
			&item.BlockedModelPnl, &rawParts, &item.ProofStrength,
			&rawCalibration, &rawNotes,
		); err != nil {
			return nil, fmt.Errorf("scan gate quality row: %w", err)
		}
		item.SkipReasons = asObject(rawSkip)
		item.BlockedPnlParts = asObject(rawParts)
		item.Calibration = asObject(rawCalibration)
		item.Notes = asObject(rawNotes)
		items = append(items, item)
	}
	return items, rows.Err()
}

// EnqueueReplayRunOperator is the operator-facing entrypoint of
// POST /api/autogrid/replay/run: it stamps the current config version and
// hands the experiment to the replay engine. overrides == nil/empty means
// the zero-override validation run (stored verdicts MUST reproduce).
//
// NOTE (task seam): EnqueueReplayRun is delivered by the parallel replay
// engine task with the exact signature
//
//	EnqueueReplayRun(ctx context.Context, db *pgxpool.Pool,
//		from, to time.Time, baseline map[string]any,
//		overrides map[string]any, codeVersion string) (uuid.UUID, error)
//
// and this wrapper is the single call site.
func (s *Service) EnqueueReplayRunOperator(ctx context.Context, from, to time.Time, overrides map[string]any) (string, error) {
	if overrides == nil {
		overrides = map[string]any{}
	}
	codeVersion := ConfigVersion(ctx, s.db, defaultSettingsID(ctx, s.db))
	runID, err := EnqueueReplayRun(ctx, s.db, from, to, map[string]any{}, overrides, codeVersion)
	if err != nil {
		return "", fmt.Errorf("enqueue replay run: %w", err)
	}
	return runID.String(), nil
}

// asObject coerces a scanned jsonb value into a JSON object; anything else
// (NULL included) degrades to an empty object, never a nil map.
func asObject(raw any) map[string]any {
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asObjectList coerces a scanned jsonb array (the gate trace) into a list
// of objects, dropping malformed entries instead of failing the row.
func asObjectList(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func stringOrDefault(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}
