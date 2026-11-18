package server

import (
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

func (s *Server) agentClearContext(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rev int64 `json:"rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.Store.AuthorizeAgentControl(r.Context(), id, userID(r)); err != nil {
		fail(w, err)
		return
	}
	err := s.Store.Tx(r.Context(), func(tx *store.Tx) error {
		_, err := tx.RequestAgentContextClear(id, userID(r), req.Rev)
		return err
	})
	if err != nil {
		spawnError(w, err)
		return
	}
	card, err := s.Store.AgentCardByID(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, card)
}
