package server

import (
	"os"
	"path/filepath"

	"github.com/goccy/go-json"
)

// Settings stores user preferences persisted to settings.json.
// ShowSystemEvents uses *bool so nil (absent from JSON) is distinguishable
// from an explicit false — nil → server default of true.
type Settings struct {
	AgentIDDefault   string `json:"agent_id_default,omitempty"`
	Theme            string `json:"theme,omitempty"` // "" | "dark" | "light"
	ShowSystemEvents *bool  `json:"show_system_events,omitempty"`
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
