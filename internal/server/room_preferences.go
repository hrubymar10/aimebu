package server

import "net/http"

func (s *Server) roomPreferences(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	var body struct {
		Hidden *bool `json:"hidden"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Hidden == nil {
		apiError(w, 400, "invalid_request", "hidden is required")
		return
	}
	preference, err := s.Store.SetRoomViewPreference(r.Context(), userID(r), room.ID, body.Hidden)
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, 200, preference)
}

// roomRead advances the caller's server-side read cursor for one room.
func (s *Server) roomRead(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	var body struct {
		Seq int64 `json:"seq"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &body) {
		return
	}
	if body.Seq < 0 {
		apiError(w, 400, "invalid_request", "seq must not be negative")
		return
	}
	state, err := s.Store.MarkRoomRead(r.Context(), userID(r), room.ID, body.Seq)
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, 200, state)
}

// roomReadReceipts reports where each member's read position lands.
func (s *Server) roomReadReceipts(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	receipts, err := s.Store.RoomReadReceipts(r.Context(), room.ID)
	if err != nil {
		userError(w, err)
		return
	}
	writeJSON(w, 200, receipts)
}
