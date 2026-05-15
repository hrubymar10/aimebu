package server

import (
	"os"
	"path/filepath"

	"github.com/goccy/go-json"
)

// Settings stores user preferences persisted to settings.json.
// Pointer fields use *T so nil (absent from JSON) is distinguishable from an
// explicit false/zero — nil → server-side default is applied in getSettings().
type Settings struct {
	AgentIDDefault      string `json:"agent_id_default,omitempty"`
	Theme               string `json:"theme,omitempty"` // "" | "dark" | "light" | "red-dark" | "red-light" | "blue-dark" | "blue-light" | "green-dark" | "green-light" | "high-contrast-dark" | "high-contrast-light"
	ShowSystemEvents    *bool  `json:"show_system_events,omitempty"`
	DebugButtonEnabled  *bool  `json:"debug_button_enabled,omitempty"`
	NotificationEnabled *bool  `json:"notification_enabled,omitempty"`
	NotificationSound   string `json:"notification_sound,omitempty"`  // "builtin:<name>" or "user:<uuid>"
	NotificationVolume  *int   `json:"notification_volume,omitempty"` // 0–100
}

var validThemes = map[string]bool{
	"":                    true,
	"dark":                true,
	"light":               true,
	"red-dark":            true,
	"red-light":           true,
	"blue-dark":           true,
	"blue-light":          true,
	"green-dark":          true,
	"green-light":         true,
	"high-contrast-dark":  true,
	"high-contrast-light": true,
}

func (s *store) loadSettings() {
	data, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	_ = json.Unmarshal(data, &s.settings)
	// Legacy theme migration: prior "red" maps to "red-dark".
	if s.settings.Theme == "red" {
		s.settings.Theme = "red-dark"
	}
	if s.settings.Theme == "high-contrast-dark-cyan" {
		s.settings.Theme = "high-contrast-dark"
	}
}

func (s *store) getSettings() Settings {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	set := s.settings
	if set.Theme == "" {
		set.Theme = "dark"
	}
	if set.ShowSystemEvents == nil {
		t := true
		set.ShowSystemEvents = &t
	}
	if set.DebugButtonEnabled == nil {
		f := false
		set.DebugButtonEnabled = &f
	}
	if set.NotificationEnabled == nil {
		t := true
		set.NotificationEnabled = &t
	}
	if set.NotificationSound == "" {
		set.NotificationSound = "builtin:chime"
	}
	if set.NotificationVolume == nil {
		v := 70
		set.NotificationVolume = &v
	}
	return set
}

func (s *store) putSettings(set Settings) {
	s.settingsMu.Lock()
	s.settings = set
	data, _ := json.MarshalIndent(set, "", "  ")
	s.settingsMu.Unlock()
	atomicWrite(filepath.Join(s.dir, "settings.json"), data)
}

func (s *store) clearSettings() {
	s.settingsMu.Lock()
	s.settings = Settings{}
	s.settingsMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "settings.json"))
}
