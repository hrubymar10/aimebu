package server

import (
	"net/http"
	"strconv"
)

// userPrefNames are the personal interface preferences the server keeps per user. Layout state that belongs to one device
// (panel widths, collapsed rooms) stays in the browser.
var userPrefNames = map[string]bool{"theme": true, "inbox_muted": true, "inbox_sound": true, "inbox_volume": true,
	"system_events": true, "debug_inspector": true, "markdown_mode": true, "composer_formatting": true, "sidebar_sort": true, "sidebar_project_order": true,
	"harness_setup_skipped": true, "notification_banner_dismissed": true,
	"notification_text": true, "sidebar_preview": true}

// retiredPreferences are names an older page may still send (a cached tab, the bulk upload of a sign-in). They are accepted and
// dropped, so one stale name never makes the server refuse the other preferences sent with it. A value stored under one earlier is
// left alone and still read back.
var retiredPreferences = map[string]bool{"harness_layout": true, "thread_order": true, "message_layout": true, "sidebar_status_variant": true}

// userPrefMax is how long a value may be; most are a word or a number, the manual project order is a list of ids.
const userPrefMaxDefault = 256

var userPrefMax = map[string]int{"sidebar_project_order": 4096}

func userPrefLimit(name string) int {
	if limit, ok := userPrefMax[name]; ok {
		return limit
	}
	return userPrefMaxDefault
}

// GET /api/me/preferences: the signed-in user's stored preferences.
func (s *Server) preferencesGet(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.Store.UserPreferences(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"preferences": prefs})
}

// PUT /api/me/preferences {"preferences": {name: value}}: stores the given preferences and returns all of them. Unknown names and
// values over their limit (256 bytes; 4096 for the manual project order) are refused as a whole; retired names are dropped.
func (s *Server) preferencesPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preferences map[string]string `json:"preferences"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	retired := false
	for name := range req.Preferences {
		if retiredPreferences[name] {
			delete(req.Preferences, name)
			retired = true
		}
	}
	if retired && len(req.Preferences) == 0 {
		s.preferencesGet(w, r)
		return
	}
	if len(req.Preferences) == 0 || len(req.Preferences) > len(userPrefNames) {
		apiError(w, 400, "invalid_preferences", "send between one and "+strconv.Itoa(len(userPrefNames))+" preferences")
		return
	}
	for name, value := range req.Preferences {
		if !userPrefNames[name] {
			apiError(w, 400, "invalid_preferences", "not a known preference: "+name)
			return
		}
		if len(value) > userPrefLimit(name) {
			apiError(w, 400, "invalid_preferences", "the value of "+name+" is too long")
			return
		}
	}
	if err := s.Store.SetUserPreferences(r.Context(), userID(r), req.Preferences); err != nil {
		fail(w, err)
		return
	}
	s.preferencesGet(w, r)
}
