package autogrid

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Decision intelligence core (v2.0.184, docs/decision-analysis-plan-2026-10-01.md,
// migration 0062). The system had no memory of its own decisions: every
// gate/threshold dispute was settled by a release onto live money. This file
// is the write path that makes each admission evaluation observable —
// episode linkage, per-gate trace, data passport, config snapshot — so the
// replay engine and the per-regime quality reports can later re-judge the
// verdict from stored facts instead of re-running it on live capital.
//
// DIRECTION OF THE WRITE: observation, never participation. Every function
// here is best-effort by construction — a bounded context (2s), ONE attempt,
// no retries, and a lost-observation counter instead of an error return. An
// analytics failure must degrade the COVERAGE number, never the trading
// path: RecordDecision cannot block, delay or fail a deploy even when the
// database is gone. The flip side is deliberate and recorded: a row that
// failed to write is gone (write-only journal, no backfill) — its absence is
// exactly what lostObservations counts.
//
// pgx discipline (prod-incident lessons): continuous $N numbering inside
// every statement, explicit ::uuid/::int/::numeric/::jsonb casts on
// placeholder positions, no error is swallowed silently (rate-limited WARN
// at minimum), and no SQL idiom is copied between tables without checking
// the target column types (entry_episodes has NO unique constraint on
// (symbol, direction) — episodes repeat over time — so the episode upsert
// is a guarded CTE, not an ON CONFLICT).

const (
	// decisionWriteTimeout bounds the whole observation write. The scan
	// cycle is ~150s and an evaluation is ~4min apart per symbol; 2 seconds
	// is an eternity for two indexed statements and cheap enough that a
	// wedged DB cannot pile up latency inside a deploy pass.
	decisionWriteTimeout = 2 * time.Second
	// episodeOpenWindow is the episode-collapsing policy from the plan: a
	// candidate re-rejected every 4 minutes must not inflate "missed
	// profit" — attempts of the same symbol+direction inside this window
	// collapse into ONE opportunity.
	episodeOpenWindow = 2 * time.Hour
	// decisionLostWarnEvery rate-limits the lost-observation WARN: ~67k
	// evaluations/day against a dead DB would otherwise flood the log with
	// one line per attempt; one line per 5 minutes is unmissable and cheap.
	decisionLostWarnEvery = 5 * time.Minute
)

// Episode dispositions (entry_episodes.disposition). ALLOWED is set here on
// the ALLOW verdict; REJECTED_STILL is finalized ONLY by the background
// retention sweep (a still-open episode may yet flip to an allowed attempt
// inside the window) — this file never writes it.
const (
	episodeDispositionOpen    = "OPEN"
	episodeDispositionAllowed = "ALLOWED"
)

// Row-level data quality vocabulary (entry_decisions.data_quality, VARCHAR(16)).
// RV=0 on a klines error is NEVER "no volatility" — the passport separates
// "measured OK" from "could not measure".
const (
	dataQualityOK      = "OK"
	dataQualityMissing = "MISSING"
	dataQualityStale   = "STALE"
	dataQualityDesync  = "DESYNC"
	dataQualityError   = "ERROR"
)

// dataQualityRank orders the passport values worst-first for the row-level
// rollup. ERROR (a computation failed, values are actively wrong) outranks
// DESYNC (indicators measured at different times), which outranks STALE
// (old but present), which outranks MISSING (absent — the honest state of
// an early rejection's unevaluated gates). MISSING is the mildest defect:
// a NOT_EVALUATED gate is the normal shape of a short-circuited chain.
var dataQualityRank = map[string]int{
	dataQualityError:   4,
	dataQualityDesync:  3,
	dataQualityStale:   2,
	dataQualityMissing: 1,
	dataQualityOK:      0,
}

// Gate trace verdict vocabulary (gate_trace[].verdict). The trace lives in
// jsonb, so unknown strings survive verbatim instead of being rewritten —
// an out-of-vocabulary verdict is a caller bug that must stay visible, not
// be silently mapped to UNKNOWN. Only the empty value defaults.
const traceVerdictUnknown = "UNKNOWN"

// Geometry stages (entry_decisions.stage, VARCHAR(16)). An early rejection
// legitimately has no final geometry — it stays unknown instead of being
// simulated (plan §4.1).
const (
	decisionStageScanner      = "SCANNER"
	decisionStageIntermediate = "INTERMEDIATE"
	decisionStageFinal        = "FINAL"
)

// decisionStages is the whitelist normalizeDecisionStage enforces; anything
// unrecognized degrades to SCANNER, the column's own default and the least
// claim-making stage (a garbage stage must not assert FINAL geometry).
var decisionStages = map[string]bool{
	decisionStageScanner:      true,
	decisionStageIntermediate: true,
	decisionStageFinal:        true,
}

// lostObservations counts observation writes that never reached the
// database (RecordDecision / LogShadowCoverage failures, nil-DB shutdown
// races). Monotonic, never reset — state and coverage reports divide by the
// successful writes and show this counter as the observer's own outage.
var lostObservations atomic.Uint64

// decisionLostWarnAt is the rate-limit anchor for the shared lost-observation
// WARN (one line per decisionLostWarnEvery, CAS-guarded for concurrency).
var decisionLostWarnAt atomic.Int64

// LostObservations exposes the lost-observation counter for state and
// coverage reports (the "observer outage" line in screen 3).
func LostObservations() uint64 {
	return lostObservations.Load()
}

// GateTraceEntry is one gate's verdict inside the ordered gate_trace array.
// Built from the checks ALREADY executed — recording must never trigger an
// extra exchange call after a rejection (plan invariant §4.1).
type GateTraceEntry struct {
	Gate       string         `json:"gate"`                // RV | ANTI_FOMO | HURST | STRESS | KNIFE | LIQ | ORDERBOOK | BACKTEST | SCANNER_* | ...
	Verdict    string         `json:"verdict"`             // PASS | REJECT | WAIT | EXEMPT | NOT_EVALUATED | UNKNOWN
	Threshold  float64        `json:"threshold,omitempty"` // действовавший порог
	Exemption  string         `json:"exemption,omitempty"` // например confirmed_directional_trend
	Inputs     map[string]any `json:"inputs,omitempty"`    // численные входы: {"ratio":1.62}
	ObservedAt *time.Time     `json:"observed_at,omitempty"`
	Quality    string         `json:"quality,omitempty"` // OK | MISSING | STALE | DESYNC | ERROR
}

// DecisionRecord is one admission evaluation on ANY path that can open or
// add risk. RefID is the candidate id on scanner paths (bot/intent id on
// the others), exactly like the 0052 journal's ref_id.
type DecisionRecord struct {
	Path, Fleet, Symbol, RefID      string // path: SCANNER_PAPER|SCANNER_REAL|..., fleet: PAPER|REAL, RefID = candidate id
	Outcome, Code, Reason           string // outcome: ALLOW|WAIT|REJECT, code = решающий гейт
	Stage                           string // SCANNER | INTERMEDIATE | FINAL
	DirectionBefore, DirectionAfter string
	PriceAtDecision                 *decimal.Decimal
	PriceSource                     string
	DataQuality                     string // OK|MISSING|STALE|DESYNC|ERROR
	Trace                           []GateTraceEntry
	Features                        map[string]any
	ConfigSnapshot                  map[string]any
	CandidateID, ScanID             string
}

// ── normalization / serialization helpers ───────────────────────────────────

// normalizeDataQuality resolves the row-level data passport: an explicit
// valid value wins; otherwise the trace's per-gate qualities roll up
// worst-first; an empty trace with no explicit value is OK (nothing was
// measured wrong because nothing claims to have been measured). An
// out-of-vocabulary explicit value (a caller bug) falls through to the
// trace-derived value — the column is VARCHAR(16) with an enum contract,
// and a garbage string must neither error the insert nor impersonate a
// passport value.
func normalizeDataQuality(explicit string, trace []GateTraceEntry) string {
	if rank, ok := dataQualityRank[explicit]; ok && rank >= 0 {
		return explicit
	}
	worst := dataQualityRank[dataQualityOK]
	for _, e := range trace {
		if rank, ok := dataQualityRank[e.Quality]; ok && rank > worst {
			worst = rank
		}
	}
	for quality, rank := range dataQualityRank {
		if rank == worst {
			return quality
		}
	}
	return dataQualityOK
}

// normalizeDecisionStage whitelists the stage vocabulary; the default (and
// the degradation for garbage) is SCANNER — the earliest stage, the one
// making the fewest geometry claims.
func normalizeDecisionStage(stage string) string {
	if decisionStages[stage] {
		return stage
	}
	return decisionStageScanner
}

// normalizeTraceVerdict defaults an empty verdict to UNKNOWN (the gate ran
// but no verdict was recorded — distinct from NOT_EVALUATED, which the
// caller sets explicitly for short-circuited gates). Non-empty values pass
// through verbatim: the trace is jsonb, an out-of-vocabulary verdict stays
// visible instead of being silently rewritten.
func normalizeTraceVerdict(verdict string) string {
	if strings.TrimSpace(verdict) == "" {
		return traceVerdictUnknown
	}
	return verdict
}

// marshalTraceEntry serializes one trace entry, degrading instead of
// failing: Inputs that json.Marshal cannot encode (NaN/Inf floats from a
// failed indicator computation, channels, funcs) are dropped and the entry
// is stamped Quality=ERROR — an input we cannot serialize is a data-quality
// defect of the observation, and one bad input must not cost the whole
// trace (the row is the denominator of every coverage report).
func marshalTraceEntry(e GateTraceEntry) []byte {
	e.Verdict = normalizeTraceVerdict(e.Verdict)
	buf, err := json.Marshal(e)
	if err == nil {
		return buf
	}
	e.Inputs = nil
	if strings.TrimSpace(e.Quality) == "" {
		e.Quality = dataQualityError
	}
	if buf, err = json.Marshal(e); err == nil {
		return buf
	}
	// Cannot happen — only string/number/time fields remain — but a trace
	// entry must never be able to break the row write.
	return []byte(fmt.Sprintf(`{"gate":%q,"verdict":%q,"quality":%q}`,
		e.Gate, traceVerdictUnknown, dataQualityError))
}

// marshalGateTrace serializes the ordered trace; nil/empty → [] (the
// column's own default shape).
func marshalGateTrace(trace []GateTraceEntry) []byte {
	if len(trace) == 0 {
		return []byte("[]")
	}
	out := make([]byte, 0, len(trace)*96+2)
	out = append(out, '[')
	for i, e := range trace {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, marshalTraceEntry(e)...)
	}
	return append(out, ']')
}

// jsonSafeMap copies m dropping values json.Marshal cannot encode, and
// records the dropped keys under "_unserializable" — a feature vector that
// carries a NaN must not void the features column, and the drop must not
// be silent either (the key list IS the visibility).
func jsonSafeMap(m map[string]any) map[string]any {
	safe := make(map[string]any, len(m)+1)
	var dropped []string
	for k, v := range m {
		if _, err := json.Marshal(v); err != nil {
			dropped = append(dropped, k)
			continue
		}
		safe[k] = v
	}
	if len(dropped) > 0 {
		safe["_unserializable"] = dropped
	}
	return safe
}

// marshalJSONMap is the column-facing serialize for features /
// config_snapshot: sanitized, never nil-marshaling (nil → the scalar jsonb
// `null`, which broke `||` merges in prod — see rejectCandidate), never
// failing.
func marshalJSONMap(m map[string]any) []byte {
	buf, err := json.Marshal(jsonSafeMap(m))
	if err != nil || len(buf) == 0 {
		return []byte("{}")
	}
	return buf
}

// warnDecisionLost emits the rate-limited lost-observation WARN (at most
// one per decisionLostWarnEvery, CAS-guarded): the counter is the durable
// signal, the log line is the human one, and neither may ever become a
// return value the trading path would have to handle.
func warnDecisionLost(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := decisionLostWarnAt.Load()
	if now-last < int64(decisionLostWarnEvery) {
		return
	}
	if !decisionLostWarnAt.CompareAndSwap(last, now) {
		return
	}
	slog.Default().Warn(msg, append([]any{"component", "autogrid_worker", "lost_total", lostObservations.Load()}, args...)...)
}

// ── episodes ────────────────────────────────────────────────────────────────

// ResolveEpisode implements the episode-collapsing policy: the open episode
// is the same symbol+direction with last_seen inside episodeOpenWindow and
// disposition='OPEN'. Found → attempts++ (returning the new attempt number);
// not found → a new episode is created and (id, 1) returned.
//
// Atomicity: the bump-or-create is ONE guarded CTE (UPDATE-RETURNING with a
// conditional INSERT leg — entry_episodes has NO unique constraint to lean
// an ON CONFLICT on, because episodes of the same pair repeat over time),
// wrapped in a transaction holding a per-(symbol,direction) advisory lock.
// The lock closes the one race a bare CTE leaves open: two lanes evaluating
// the same NEW pair concurrently would both take the INSERT leg and fork
// two episodes for one opportunity. Errors are RETURNED, not swallowed —
// callers degrade episode linkage and keep writing the decision row.
func ResolveEpisode(ctx context.Context, db *pgxpool.Pool, symbol, direction, regime string) (episodeID uuid.UUID, attemptNo int, err error) {
	if db == nil {
		return uuid.Nil, 0, fmt.Errorf("resolveEpisode: nil pool")
	}
	direction = entryDirectionFromTrend(direction) // "" / any vocabulary → NEUTRAL
	if strings.TrimSpace(regime) == "" {
		regime = "UNKNOWN"
	}
	lockKey := symbol + "|" + direction

	tx, err := db.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Per-pair advisory lock (transaction-scoped, released at commit). The
	// bump-or-create itself is one statement; the lock only serializes the
	// INSERT leg across concurrent lanes.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return uuid.Nil, 0, err
	}

	var id string
	if err = tx.QueryRow(ctx, `
		WITH existing AS (
			SELECT id FROM entry_episodes
			WHERE symbol = $1 AND direction = $2
			  AND disposition = '`+episodeDispositionOpen+`'
			  AND last_seen > NOW() - $3::interval
			ORDER BY last_seen DESC
			LIMIT 1
		), bumped AS (
			UPDATE entry_episodes e
			SET attempts = e.attempts + 1, last_seen = NOW()
			FROM existing
			WHERE e.id = existing.id
			RETURNING e.id::TEXT, e.attempts
		), created AS (
			INSERT INTO entry_episodes (symbol, direction, regime)
			SELECT $4, $5, $6
			WHERE NOT EXISTS (SELECT 1 FROM bumped)
			RETURNING id::TEXT, attempts
		)
		SELECT id, attempts FROM bumped
		UNION ALL
		SELECT id, attempts FROM created
	`, symbol, direction, fmt.Sprintf("%d seconds", int(episodeOpenWindow.Seconds())),
		symbol, direction, regime).Scan(&id, &attemptNo); err != nil {
		return uuid.Nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, 0, err
	}
	return parsed, attemptNo, nil
}

// ── the decision write ──────────────────────────────────────────────────────

// RecordDecision persists one admission evaluation into entry_decisions
// with the full 0062 column set: episode linkage (ResolveEpisode first),
// decision_at=NOW(), data passport, ordered gate trace, geometry stage,
// config snapshot, direction before/after the smart override, and the price
// with its source. On ALLOW the episode's disposition moves to ALLOWED
// (guarded to OPEN episodes only); REJECT/WAIT leave it OPEN — the
// REJECTED_STILL finalization belongs to the background sweep, never to
// the hot path (an episode may still flip to an allowed attempt inside the
// window).
//
// Best-effort contract: the context is clamped to decisionWriteTimeout,
// there is exactly ONE attempt per statement, no error ever leaves this
// function, and every lost row increments lostObservations with a
// rate-limited WARN. An episode-resolution failure degrades the linkage
// (NULL episode_id, attempt_no=1) but NOT the row — the rejection itself is
// the denominator of coverage and must land even when episodes misbehave.
func RecordDecision(ctx context.Context, db *pgxpool.Pool, rec DecisionRecord) {
	if db == nil {
		// Shutdown race or wiring bug: no write is even possible. Counted,
		// not silently dropped — the coverage report shows the hole.
		lostObservations.Add(1)
		warnDecisionLost("decision-intelligence: nil pool — observation lost",
			"path", rec.Path, "symbol", rec.Symbol, "outcome", rec.Outcome)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, decisionWriteTimeout)
	defer cancel()

	// Episode linkage. The episode follows the direction the gates judged —
	// the post-override direction when the smart override ran, the original
	// one otherwise; the regime rides in Features (the same key the shared
	// entry-chain journal uses), defaulting to UNKNOWN.
	episodeDir := rec.DirectionAfter
	if strings.TrimSpace(episodeDir) == "" {
		episodeDir = rec.DirectionBefore
	}
	regime, _ := rec.Features["regime"].(string)
	episodeID, attemptNo, epErr := ResolveEpisode(ctx, db, rec.Symbol, episodeDir, regime)
	if epErr != nil {
		// Linkage lost, observation not: WARN (visible, rate-limited on the
		// shared lost channel is wrong here — this is a different defect —
		// so a plain WARN) and write the row unlinked.
		slog.Default().Warn("decision-intelligence: episode resolution failed; row written unlinked",
			"component", "autogrid_worker", "symbol", rec.Symbol, "error", epErr)
		episodeID, attemptNo = uuid.Nil, 1
	}

	// features: never nil-marshaled (jsonb `null` breaks `||` merges — the
	// rejectCandidate lesson), candidate/scan ids preserved for replay
	// joins when the caller did not embed them itself.
	features := make(map[string]any, len(rec.Features)+2)
	for k, v := range rec.Features {
		features[k] = v
	}
	if _, ok := features["candidate_id"]; !ok && rec.CandidateID != "" {
		features["candidate_id"] = rec.CandidateID
	}
	if _, ok := features["scan_id"]; !ok && rec.ScanID != "" {
		features["scan_id"] = rec.ScanID
	}

	// Nullable-when-empty text columns: analytics read NULL as "absent",
	// '' as "present but empty" — an unset override direction is absent.
	dirBefore, dirAfter := nullableText(rec.DirectionBefore), nullableText(rec.DirectionAfter)
	priceSource := nullableText(clampText(rec.PriceSource, 16))
	var episodeParam *string
	if episodeID != uuid.Nil {
		s := episodeID.String()
		episodeParam = &s
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO entry_decisions (
			path, fleet, symbol, outcome, code, reason, features, config_version, ref_id,
			episode_id, attempt_no, decision_at, data_quality, gate_trace, stage,
			config_snapshot, direction_before, direction_after, price_at_decision, price_source
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9,
			$10::uuid, $11::int, NOW(), $12, $13::jsonb, $14,
			$15::jsonb, $16, $17, $18::numeric, $19
		)
	`,
		rec.Path, rec.Fleet, rec.Symbol,
		strings.ToUpper(strings.TrimSpace(rec.Outcome)),
		rec.Code, rec.Reason,
		string(marshalJSONMap(features)),
		// Settings are not threaded through DecisionRecord; the
		// default-scope settings row fingerprints the config that decided
		// (cached for a minute inside ConfigVersion).
		ConfigVersion(ctx, db, defaultSettingsID(ctx, db)),
		rec.RefID,
		episodeParam, attemptNo,
		normalizeDataQuality(rec.DataQuality, rec.Trace),
		string(marshalGateTrace(rec.Trace)),
		normalizeDecisionStage(rec.Stage),
		string(marshalJSONMap(rec.ConfigSnapshot)),
		dirBefore, dirAfter,
		rec.PriceAtDecision, priceSource,
	); err != nil {
		lostObservations.Add(1)
		warnDecisionLost("decision-intelligence: entry_decisions write failed — observation lost",
			"path", rec.Path, "symbol", rec.Symbol, "outcome", rec.Outcome, "error", err)
		return
	}

	// ALLOW closes the episode's question ("did refusing ever get
	// overruled?"); the guard keeps a finalized episode from being
	// resurrected by a late racing write.
	if strings.EqualFold(strings.TrimSpace(rec.Outcome), entryOutcomeAllow) && episodeID != uuid.Nil {
		if _, err := db.Exec(ctx, `
			UPDATE entry_episodes
			SET disposition = $2
			WHERE id = $1::uuid AND disposition = $3
		`, episodeID.String(), episodeDispositionAllowed, episodeDispositionOpen); err != nil {
			// The decision row landed; only the disposition lags. The
			// background sweep reconciles — WARN, not a lost observation.
			slog.Default().Warn("decision-intelligence: episode ALLOWED update failed",
				"component", "autogrid_worker", "symbol", rec.Symbol, "error", err)
		}
	}
}

// ── coverage ledger ─────────────────────────────────────────────────────────

// LogShadowCoverage is the coverage denominator: EVERY rejection attempt
// logs either its shadow observation or the reason none exists
// (CAP_REACHED | DUPLICATE_EPISODE | INVALID_GEOMETRY | DATA_QUALITY).
// The same best-effort policy as RecordDecision — a ledger row lost to a
// DB blip is a lost observation (counted + rate-limited WARN), never a
// blocker. scanID/episodeID that are empty or malformed degrade to NULL
// with a WARN rather than failing the insert: the denominator line itself
// is the artifact that must land.
func LogShadowCoverage(ctx context.Context, db *pgxpool.Pool, decisionID, episodeID uuid.UUID, symbol, scanID string, shadowCreated bool, skipReason string) {
	if db == nil {
		lostObservations.Add(1)
		warnDecisionLost("decision-intelligence: nil pool — coverage ledger row lost", "symbol", symbol)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, decisionWriteTimeout)
	defer cancel()

	scanParam := uuidTextParam(scanID, "scan_id", symbol)
	var episodeParam *string
	if episodeID != uuid.Nil {
		s := episodeID.String()
		episodeParam = &s
	}

	if _, err := db.Exec(ctx, `
		INSERT INTO shadow_coverage_log (
			decision_id, episode_id, symbol, scan_id, shadow_created, skip_reason
		) VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6)
	`,
		decisionID.String(), episodeParam, symbol, scanParam,
		shadowCreated, clampText(skipReason, 24)); err != nil {
		lostObservations.Add(1)
		warnDecisionLost("decision-intelligence: shadow coverage ledger write failed — denominator row lost",
			"symbol", symbol, "shadow_created", shadowCreated, "error", err)
	}
}

// uuidTextParam validates a caller-supplied UUID string and returns it as a
// nullable parameter: empty → NULL, malformed → NULL plus a WARN (visible,
// not silent — but the surrounding write must survive a bad id).
func uuidTextParam(raw, field, symbol string) *string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if _, err := uuid.Parse(s); err != nil {
		slog.Default().Warn("decision-intelligence: malformed uuid — stored as NULL",
			"component", "autogrid_worker", "field", field, "symbol", symbol, "value", raw)
		return nil
	}
	return &s
}

// ── config snapshot ─────────────────────────────────────────────────────────

// BuildConfigSnapshot captures the VALUES that decided (a config hash
// identifies, it does not restore — plan §4.1): the capital-shape settings
// plus the feature flags steering the gate chain. Decimals are stored as
// their exact string form: jsonb numbers would silently pass through
// float64 and a 1.5-bps threshold must still read 1.5 in six months.
// Flags default to TRUE when their row is absent — the same
// COALESCE-convention every runtime flag reader in this package uses; a
// dead flags read records JSON null per flag (unknown, never faked).
func BuildConfigSnapshot(ctx context.Context, db *pgxpool.Pool, settings Settings) map[string]any {
	snapshot := map[string]any{
		"budget":               settings.BudgetUSDT.String(),
		"leverage":             settings.Leverage,
		"feeBps":               settings.FeeBps.String(),
		"slippageBps":          settings.SlippageBps.String(),
		"trancheDeployEnabled": settings.TrancheDeployEnabled,
		"marginReservePct":     settings.MarginReservePct.String(),
		"stopLossMode":         settings.StopLossMode,
		"pnlTargetMode":        settings.PnLTargetMode,
		"minRiskReward":        settings.MinRiskReward.String(),
	}
	flags := map[string]any{
		"backtest_gate":         nil,
		"shadow_portfolio":      nil,
		"decision_intelligence": nil,
		"replay_experiments":    nil,
	}
	if db != nil {
		var backtest, shadow, decision, replay *bool
		if err := db.QueryRow(ctx, `
			SELECT (SELECT enabled FROM feature_flags WHERE name = 'backtest_gate'),
			       (SELECT enabled FROM feature_flags WHERE name = 'shadow_portfolio'),
			       (SELECT enabled FROM feature_flags WHERE name = 'decision_intelligence'),
			       (SELECT enabled FROM feature_flags WHERE name = 'replay_experiments')
		`).Scan(&backtest, &shadow, &decision, &replay); err != nil {
			// Unknown stays unknown: null per flag, WARN — a snapshot that
			// faked the flags would poison every replay it anchors.
			slog.Default().Warn("decision-intelligence: feature flags read failed — snapshot carries nulls",
				"component", "autogrid_worker", "error", err)
		} else {
			flags["backtest_gate"] = flagOrDefault(backtest)
			flags["shadow_portfolio"] = flagOrDefault(shadow)
			flags["decision_intelligence"] = flagOrDefault(decision)
			flags["replay_experiments"] = flagOrDefault(replay)
		}
	}
	for name, value := range flags {
		snapshot[name] = value
	}
	if db != nil {
		snapshot["config_version"] = ConfigVersion(ctx, db, settings.ID)
	}
	return snapshot
}

// flagOrDefault applies the package-wide missing-row convention (absent =
// enabled), matching shadowPortfolioEnabled's COALESCE.
func flagOrDefault(v *bool) any {
	if v == nil {
		return true
	}
	return *v
}

// ── small text helpers ──────────────────────────────────────────────────────

// nullableText maps empty/blank to NULL for the nullable VARCHAR columns;
// analytics treat NULL as absent.
func nullableText(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

// clampText hard-bounds a value against its VARCHAR width so a runaway
// caller string cannot error the whole insert (the columns' vocabularies
// are far shorter than their widths; this is a seatbelt, not a shaper).
func clampText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max])
}
