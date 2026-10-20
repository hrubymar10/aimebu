package server

import (
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

// usageLimit is GET/PUT /api/projects/{project}/usage-limit: the project's daily limits. They apply to the whole project, all its rooms
// together. PUT takes a mode per field (inherit the global default, custom with a value, or none); a body without modes (the older
// shape) means a value is custom and null is none.
func (s *Server) usageLimit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	if !s.projectAccess(w, r, id) {
		return
	}
	if r.Method == http.MethodGet {
		limit, err := s.Store.UsageLimit(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, limit)
		return
	}
	var req struct {
		CostMode     string   `json:"cost_mode"`
		TurnMode     string   `json:"turn_mode"`
		DailyCostCap *float64 `json:"daily_cost_cap"`
		DailyTurnCap *int64   `json:"daily_turn_cap"`
		Rev          int64    `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	in := store.UsageLimitInput{CostMode: req.CostMode, Cost: req.DailyCostCap, TurnMode: req.TurnMode, Turns: req.DailyTurnCap}
	if in.CostMode == "" {
		in.CostMode = "none"
		if in.Cost != nil {
			in.CostMode = "custom"
		}
	}
	if in.TurnMode == "" {
		in.TurnMode = "none"
		if in.Turns != nil {
			in.TurnMode = "custom"
		}
	}
	var limit store.UsageLimit
	err := s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		var err error
		limit, err = tx.UpdateUsageLimit(id, userID(r), in, req.Rev)
		return err
	})
	if err != nil {
		spawnError(w, err)
		return
	}
	writeJSON(w, 200, limit)
}

// usageLimitDefault is GET/PUT /api/usage-limit-default: the global default that projects inherit. Anyone signed in may read it (the
// project settings show "Inherit (global: X)"); only the owner may change it. A null field is no limit.
func (s *Server) usageLimitDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		out, err := s.Store.UsageLimitDefault(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		canEdit, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"daily_cost_cap": out.DailyCostCap, "daily_turn_cap": out.DailyTurnCap, "can_edit": canEdit})
		return
	}
	if !s.ownerMachine(w, r) {
		return
	}
	var req struct {
		DailyCostCap *float64 `json:"daily_cost_cap"`
		DailyTurnCap *int64   `json:"daily_turn_cap"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if (req.DailyCostCap != nil && !(*req.DailyCostCap > 0)) || (req.DailyTurnCap != nil && *req.DailyTurnCap <= 0) {
		apiError(w, 400, "invalid_request", "a limit must be a positive number, or null for no limit")
		return
	}
	out, err := s.Store.SetUsageLimitDefault(r.Context(), userID(r), req.DailyCostCap, req.DailyTurnCap)
	if err != nil {
		spawnError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
