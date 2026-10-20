package server

import (
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

// Macros are a shared composer resource. Signed-in users can read them;
// only the instance owner can replace the map.
func (s *Server) macros(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		macros, err := s.Store.ListMacros(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, macros)
		return
	}
	if !s.adminMachine(w, r) {
		return
	}
	var macros map[string]string
	// A valid 256-entry map can carry 4 MiB of text before JSON escaping.
	if !readJSONLimit(w, r, &macros, 32<<20) {
		return
	}
	if macros == nil {
		apiError(w, 400, "invalid_request", "macro map is required")
		return
	}
	if err := store.ValidateMacros(macros); err != nil {
		apiError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := s.Store.ReplaceMacros(r.Context(), macros); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, macros)
}

func (s *Server) agentToolMacros(w http.ResponseWriter, r *http.Request) {
	macros, err := s.Store.ListMacros(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, macros)
}
