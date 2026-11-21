package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (s *Server) roomWriteAccess(w http.ResponseWriter, r *http.Request, room store.Room) bool {
	if store.IsAgentDM(room) {
		allowed, err := s.Store.AgentDMWriteAllowed(r.Context(), userID(r), room.ID)
		if err != nil {
			fail(w, err)
			return false
		}
		if !allowed {
			apiError(w, 403, "forbidden", "owner or operator of one of the agents' projects required")
		}
		return allowed
	}
	if room.Kind == "dm" && strings.HasPrefix(room.DMKey, "users:") {
		allowed, err := s.Store.PeopleDMWriteAllowed(r.Context(), room.ProjectID, room.ID)
		if err != nil {
			fail(w, err)
			return false
		}
		if allowed {
			return true
		}
	}
	return s.projectOperator(w, r, room.ProjectID)
}

func (s *Server) dmPeers(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project")
	peers, err := s.Store.DMPeers(r.Context(), projectID, userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, peers)
}

func (s *Server) openUserDM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	room, err := s.Store.OpenUserDM(r.Context(), r.PathValue("project"), userID(r), req.UserID)
	if errors.Is(err, store.ErrInvalidTransition) {
		apiError(w, http.StatusBadRequest, "invalid_request", "choose another person")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if room, err = s.Store.RoomForViewer(r.Context(), room, userID(r)); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}
