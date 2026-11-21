package server

import (
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// retentionBody is the message-expiry setting as the API shows it. Expiry hides old
// messages from what an AI is given; it never deletes a message.
type retentionBody struct {
	MessagesConsideredExpiredAfterSeconds int `json:"messages_considered_expired_after_seconds"`
	DefaultSeconds                        int `json:"default_seconds"`
	MinSeconds                            int `json:"min_seconds"`
	MaxSeconds                            int `json:"max_seconds"`
	// CanEdit says whether the caller may change the window (the global owner).
	CanEdit bool `json:"can_edit"`
}

func retentionOf(seconds int) retentionBody {
	return retentionBody{MessagesConsideredExpiredAfterSeconds: seconds, DefaultSeconds: store.DefaultMessageExpirySeconds,
		MinSeconds: store.MinMessageExpirySeconds, MaxSeconds: store.MaxMessageExpirySeconds}
}

// GET /api/retention (any signed-in user: the chat marks expired messages from it)
// and PUT /api/retention (the owner changes the window).
func (s *Server) retention(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		if !s.adminMachine(w, r) {
			return
		}
		var body struct {
			Seconds *int `json:"messages_considered_expired_after_seconds"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Seconds == nil {
			apiError(w, 400, "invalid_request", "messages_considered_expired_after_seconds is required")
			return
		}
		if err := s.Store.SetMessageExpirySeconds(r.Context(), *body.Seconds); err != nil {
			if errors.Is(err, store.ErrInvalidMessageExpiry) {
				apiError(w, 400, "invalid_request", err.Error())
				return
			}
			fail(w, err)
			return
		}
	}
	seconds, err := s.Store.MessageExpirySeconds(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	canEdit, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	body := retentionOf(seconds)
	body.CanEdit = canEdit
	writeJSON(w, 200, body)
}
