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
	Theme               string `json:"theme,omitempty"` // "" | "dark" | "light"
	ShowSystemEvents    *bool  `json:"show_system_events,omitempty"`
	NotificationEnabled *bool  `json:"notification_enabled,omitempty"`
	NotificationSound   string `json:"notification_sound,omitempty"` // "builtin:<name>" or "user:<uuid>"
	NotificationVolume  *int   `json:"notification_volume,omitempty"` // 0–100
}

var validThemes = map[string]bool{"": true, "dark": true, "light": true}

func (s *store) loadSettings() {
	data, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	_ = json.Unmarshal(data, &s.settings)
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
