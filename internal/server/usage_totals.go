package server

import (
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

// usageRange reads ?range= (today, 7d or all; today when absent) and answers 400 for anything else.
func usageRange(w http.ResponseWriter, r *http.Request) (string, bool) {
	rng := "today"
	if r.URL.Query().Has("range") {
		rng = r.URL.Query().Get("range")
	}
	if !store.UsageRanges[rng] {
		apiError(w, 400, "invalid_request", "range must be today, 7d or all")
		return "", false
	}
	return rng, true
}

// GET /api/rooms/{room}/usage?range=today|7d|all: what the room's turns spent, in total and by agent and model. Whoever may read the room
// may read it.
func (s *Server) roomUsage(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	rng, ok := usageRange(w, r)
	if !ok {
		return
	}
	totals, err := s.Store.UsageTotalsFor(r.Context(), "room", room.ID, rng)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, totals)
}

// GET /api/projects/{project}/usage?range=today|7d|all: the same for every room of a project. Whoever belongs to the project may read it.
func (s *Server) projectUsage(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.projectAccess(w, r, project) {
		return
	}
	rng, ok := usageRange(w, r)
	if !ok {
		return
	}
	totals, err := s.Store.UsageTotalsFor(r.Context(), "project", project, rng)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, totals)
}
