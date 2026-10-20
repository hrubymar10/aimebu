package server

import (
	"net/http"
	"strings"
)

// archiveSearch finds text in this project's closed rooms for one member,
// keeping other members' private chats out like the room lists do.
func (s *Server) archiveSearch(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if !s.projectAccess(w, r, project) {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		apiError(w, 400, "invalid_request", "q required")
		return
	}
	hits, err := s.Store.ArchiveSearch(r.Context(), project, userID(r), query, 50)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, hits)
}
