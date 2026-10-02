package httpapi

// v2.0.184 decision intelligence endpoints (docs/decision-analysis-plan-2026-10-01.md §7).
// Everything here is read-only over migration 0062 artifacts except
// POST /api/autogrid/replay/run, which only enqueues an immutable experiment
// row — replay never trades, never opens bots and never changes live policy.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/autogrid"
)

// listDecisionHistory serves GET /api/autogrid/decisions?symbol=XXX&limit=50:
// the newest entry_decisions with per-row episode/shadow fate.
func (s *Server) listDecisionHistory(w http.ResponseWriter, r *http.Request) {
	symbol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("symbol")))
	limit := queryLimit(r, 50)
	if limit > 500 {
		limit = 500
	}
	items, err := s.autogrid.ListDecisionHistory(r.Context(), symbol, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

// getDecisionDetail serves GET /api/autogrid/decisions/{id}: one journal row
// with the full gate trace and the linked shadow episode.
func (s *Server) getDecisionDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decision id required"})
		return
	}
	item, err := s.autogrid.GetDecisionDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, autogrid.ErrDecisionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": item})
}

// replayRunInput is the POST /api/autogrid/replay/run body: either a gate
// experiment {from, to, gate, params:{threshold:1.8}} or the zero-override
// model validation {from, to, validation:true}.
type replayRunInput struct {
	From       string         `json:"from"`
	To         string         `json:"to"`
	Gate       string         `json:"gate"`
	Params     map[string]any `json:"params"`
	Validation bool           `json:"validation"`
}

// parseReplayWindow accepts RFC3339, minute and date-only timestamps — the
// datetime-local inputs the panel sends.
func parseReplayWindow(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// runReplayExperiment serves POST /api/autogrid/replay/run (operator-only:
// the run row is an experiment, but enqueueing remains a mutation).
func (s *Server) runReplayExperiment(w http.ResponseWriter, r *http.Request) {
	var input replayRunInput
	if !decodeJSON(w, r, &input) {
		return
	}
	from, ok := parseReplayWindow(input.From)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный период: поле from обязательно (RFC3339 или YYYY-MM-DD)"})
		return
	}
	to, ok := parseReplayWindow(input.To)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный период: поле to обязательно (RFC3339 или YYYY-MM-DD)"})
		return
	}
	if !from.Before(to) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "период некорректен: from должен быть раньше to"})
		return
	}
	if to.After(time.Now().UTC().Add(time.Minute)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "период некорректен: to в будущем — replay идёт только по записанной ленте"})
		return
	}
	gate := strings.ToUpper(strings.TrimSpace(input.Gate))
	if input.Validation && gate != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "валидация модели — нулевой override: укажи либо validation, либо gate с params"})
		return
	}
	if !input.Validation && gate == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "укажи гейт или включи режим валидации модели"})
		return
	}
	var overrides map[string]any
	if input.Validation {
		// Zero override: stored verdicts must reproduce, or the run reports
		// NOT_REPLAYABLE per decision — that mismatch is the calibration signal.
		overrides = map[string]any{}
	} else {
		params := input.Params
		if params == nil {
			params = map[string]any{}
		}
		// v2.0.187: the engine reads overrides["params"]["threshold"]
		// (replay_engine.go contract) — the old "overrides" key made every
		// operator run collapse to NOT_REPLAYABLE(threshold_not_stored).
		overrides = map[string]any{"gate": gate, "params": params}
	}
	runID, err := s.autogrid.EnqueueReplayRunOperator(r.Context(), from, to, overrides)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.auditMutation(r, "autogrid.replay.run", "replay_run", runID, map[string]any{
		"from":        input.From,
		"to":          input.To,
		"gate":        gate,
		"validation":  input.Validation,
		"tradingRisk": "none",
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": runID})
}

// listReplayRuns serves GET /api/autogrid/replay/runs?limit=20.
func (s *Server) listReplayRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryLimit(r, 20)
	if limit > 100 {
		limit = 100
	}
	items, err := s.autogrid.ListReplayRuns(r.Context(), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

// getReplayRunDetail serves GET /api/autogrid/replay/runs/{id}: the run plus
// its per-decision verdicts (N/K/M/L summary lives in run.stats).
func (s *Server) getReplayRunDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run id required"})
		return
	}
	run, items, err := s.autogrid.GetReplayRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, autogrid.ErrReplayRunNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "items": items})
}

// gateQuality serves GET /api/autogrid/gate-quality?window=24H (24H | 7D):
// the stored per-regime aggregates — episodes, coverage, model ±$ of blocked
// entries and proof strength.
func (s *Server) gateQuality(w http.ResponseWriter, r *http.Request) {
	window := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("window")))
	if window == "" {
		window = "24H"
	}
	if window != "24H" && window != "7D" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "window must be 24H or 7D"})
		return
	}
	items, err := s.autogrid.ListGateQuality(r.Context(), window)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": window, "data": items})
}
