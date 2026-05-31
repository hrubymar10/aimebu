package server

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goccy/go-json"
)

// Settings stores user preferences persisted to settings.json.
// Pointer fields use *T so nil (absent from JSON) is distinguishable from an
// explicit false/zero — nil → server-side default is applied in getSettings().
type Settings struct {
	AgentIDDefault          string `json:"agent_id_default,omitempty"`
	Theme                   string `json:"theme,omitempty"` // "" | "dark" | "light" | "red-dark" | "red-light" | "blue-dark" | "blue-light" | "green-dark" | "green-light" | "high-contrast-dark" | "high-contrast-light"
	ShowSystemEvents        *bool  `json:"show_system_events,omitempty"`
	DebugButtonEnabled      *bool  `json:"debug_button_enabled,omitempty"`
	MemoryEnabled           *bool  `json:"memory_enabled,omitempty"` // nil = first-run prompt has not answered; effective disabled
	NotificationEnabled     *bool  `json:"notification_enabled,omitempty"`
	NotificationSound       string `json:"notification_sound,omitempty"`  // "builtin:<name>" or "user:<uuid>"
	NotificationVolume      *int   `json:"notification_volume,omitempty"` // 0–100
	StaleAgentWindowSeconds *int   `json:"stale_agent_window_seconds,omitempty"`
	EmptyRoomWindowSeconds  *int   `json:"empty_room_window_seconds,omitempty"`
	CleanupIntervalSeconds  *int   `json:"cleanup_interval_seconds,omitempty"`
	MessageRetentionSeconds *int   `json:"message_retention_seconds,omitempty"`
	MessageRetentionCount   *int   `json:"message_retention_count,omitempty"`
}

const (
	defaultStaleAgentWindowSeconds = 30 * 60
	defaultEmptyRoomWindowSeconds  = 60 * 60
	defaultCleanupIntervalSeconds  = 60
	defaultMessageRetentionSeconds = 0
	defaultMessageRetentionCount   = 0

	maxRetentionWindowSeconds = 30 * 24 * 60 * 60
	maxCleanupIntervalSeconds = 60 * 60
	maxMessageRetentionCount  = 1_000_000
)

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
	if set.StaleAgentWindowSeconds == nil {
		v := defaultStaleAgentWindowSeconds
		set.StaleAgentWindowSeconds = &v
	}
	if set.EmptyRoomWindowSeconds == nil {
		v := defaultEmptyRoomWindowSeconds
		set.EmptyRoomWindowSeconds = &v
	}
	if set.CleanupIntervalSeconds == nil {
		v := defaultCleanupIntervalSeconds
		set.CleanupIntervalSeconds = &v
	}
	if set.MessageRetentionSeconds == nil {
		v := defaultMessageRetentionSeconds
		set.MessageRetentionSeconds = &v
	}
	if set.MessageRetentionCount == nil {
		v := defaultMessageRetentionCount
		set.MessageRetentionCount = &v
	}
	return set
}

func (s Settings) staleAgentWindow() time.Duration {
	return time.Duration(settingIntInRange(s.StaleAgentWindowSeconds, defaultStaleAgentWindowSeconds, 60, maxRetentionWindowSeconds, false)) * time.Second
}

func (s Settings) emptyRoomWindow() time.Duration {
	return time.Duration(settingIntInRange(s.EmptyRoomWindowSeconds, defaultEmptyRoomWindowSeconds, 60, maxRetentionWindowSeconds, false)) * time.Second
}

func (s Settings) cleanupInterval() time.Duration {
	return time.Duration(settingIntInRange(s.CleanupIntervalSeconds, defaultCleanupIntervalSeconds, 10, maxCleanupIntervalSeconds, false)) * time.Second
}

func (s Settings) messageRetentionWindow() time.Duration {
	return time.Duration(settingIntInRange(s.MessageRetentionSeconds, defaultMessageRetentionSeconds, 60, maxRetentionWindowSeconds, true)) * time.Second
}

func (s Settings) messageRetentionCount() int {
	return settingIntInRange(s.MessageRetentionCount, defaultMessageRetentionCount, 1, maxMessageRetentionCount, true)
}

func settingInt(v *int, fallback int) int {
	if v == nil {
		return fallback
	}
	return *v
}

func settingIntInRange(v *int, fallback, min, max int, zeroUnlimited bool) int {
	n := settingInt(v, fallback)
	if zeroUnlimited && n == 0 {
		return n
	}
	if n < min || n > max {
		return fallback
	}
	return n
}

func validateRetentionSettings(set Settings) error {
	if err := validateSettingRange("stale_agent_window_seconds", set.StaleAgentWindowSeconds, 60, maxRetentionWindowSeconds, false); err != nil {
		return err
	}
	if err := validateSettingRange("empty_room_window_seconds", set.EmptyRoomWindowSeconds, 60, maxRetentionWindowSeconds, false); err != nil {
		return err
	}
	if err := validateSettingRange("cleanup_interval_seconds", set.CleanupIntervalSeconds, 10, maxCleanupIntervalSeconds, false); err != nil {
		return err
	}
	if err := validateSettingRange("message_retention_seconds", set.MessageRetentionSeconds, 60, maxRetentionWindowSeconds, true); err != nil {
		return err
	}
	if err := validateSettingRange("message_retention_count", set.MessageRetentionCount, 1, maxMessageRetentionCount, true); err != nil {
		return err
	}
	return nil
}

func validateSettingRange(field string, value *int, min, max int, zeroUnlimited bool) error {
	if value == nil {
		return nil
	}
	v := *value
	if zeroUnlimited && v == 0 {
		return nil
	}
	if v < min || v > max {
		if zeroUnlimited {
			return fmt.Errorf("%s must be 0 or between %d and %d", field, min, max)
		}
		return fmt.Errorf("%s must be between %d and %d", field, min, max)
	}
	return nil
}

func (s *store) putSettings(set Settings) {
	s.settingsMu.Lock()
	s.settings = set
	data, _ := json.MarshalIndent(set, "", "  ")
	s.settingsMu.Unlock()
	atomicWrite(filepath.Join(s.dir, "settings.json"), data)
}

func (s *store) staleAgentWindow() time.Duration {
	return s.getSettings().staleAgentWindow()
}

func (s *store) emptyRoomWindow() time.Duration {
	return s.getSettings().emptyRoomWindow()
}

func (s *store) cleanupInterval() time.Duration {
	return s.getSettings().cleanupInterval()
}

func (s *store) messageRetentionWindow() time.Duration {
	return s.getSettings().messageRetentionWindow()
}

func (s *store) messageRetentionCount() int {
	return s.getSettings().messageRetentionCount()
}

func (s *store) clearSettings() {
	s.settingsMu.Lock()
	s.settings = Settings{}
	s.settingsMu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, "settings.json"))
}
