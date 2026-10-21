package server

import (
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/internal/store"
)

// systemEvents lists the newest operational events the signed-in user may
// see, newest first. The live stream carries later ones; this is the history
// the system-events panel opens with.
func (s *Server) systemEvents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			apiError(w, 400, "invalid_request", "limit must be between 1 and 500")
			return
		}
		limit = value
	}
	events, err := s.Store.RecentSystemEvents(r.Context(), userID(r), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": events, "kinds": store.SystemEventKinds()})
}

// messageDebug returns the debug inspector's view of one message: the stored
// row, addressing, per-agent wake notes with their turns and receipts, and
// the related events. It exposes nothing a room member cannot already read
// plus delivery bookkeeping, so it needs the same room access as reading.
func (s *Server) messageDebug(w http.ResponseWriter, r *http.Request) {
	message, err := s.Store.MessageByID(r.Context(), r.PathValue("message"))
	if err != nil {
		fail(w, err)
		return
	}
	if _, ok := s.roomAccess(w, r, message.RoomID); !ok {
		return
	}
	debug, err := s.Store.MessageDebug(r.Context(), message.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, debug)
}

// GET /api/message-numbers/{no}: where the message numbered #no lives, for a "#N" link that may point into another room. The caller must
// have access to that room, like any other read of it.
func (s *Server) messageByNo(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.ParseInt(r.PathValue("no"), 10, 64)
	if err != nil || no < 1 {
		apiError(w, 400, "invalid_request", "message number must be a positive integer")
		return
	}
	message, err := s.Store.MessageByNo(r.Context(), no)
	if err != nil {
		fail(w, err)
		return
	}
	if _, ok := s.roomAccess(w, r, message.RoomID); !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"id": message.ID, "no": message.No, "seq": message.Seq, "room_id": message.RoomID})
}
