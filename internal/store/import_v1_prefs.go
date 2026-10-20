package store

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/goccy/go-json"
)

// v1's personal interface settings (theme, notification volume and sound, system events, the debug button) are the owner's, so they
// are written to the owner's user preferences (user_preference), where v2 keeps them, instead of being listed as left behind. An
// existing v2 choice always wins. A value v2 has no equivalent for (a theme it lacks, a custom sound) is listed under skipped settings.

var v1PersonalKeys = map[string]bool{"theme": true, "notification_sound": true, "notification_volume": true, "show_system_events": true, "debug_button_enabled": true}

var (
	v2Themes        = map[string]bool{"dark": true, "light": true, "blue-dark": true, "blue-light": true, "green-dark": true, "green-light": true, "red-dark": true, "red-light": true, "high-contrast-dark": true, "high-contrast-light": true}
	v2BuiltinSounds = map[string]bool{"beep": true, "chime": true, "ding": true, "knock": true}
)

// v1PersonalPreferences maps the v1 settings to v2 preference names and values, and lists what could not be carried.
func v1PersonalPreferences(settings map[string]json.RawMessage) (map[string]string, []V1SkippedSetting) {
	prefs := map[string]string{}
	var skipped []V1SkippedSetting
	boolean := func(key, name string) {
		raw, ok := settings[key]
		if !ok || string(raw) == "null" {
			return
		}
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			skipped = append(skipped, V1SkippedSetting{key, "invalid v1 boolean"})
			return
		}
		prefs[name] = map[bool]string{true: "1", false: "0"}[value]
	}
	boolean("show_system_events", "system_events")
	boolean("debug_button_enabled", "debug_inspector")
	if raw, ok := settings["theme"]; ok && string(raw) != "null" {
		var theme string
		if json.Unmarshal(raw, &theme) != nil || !v2Themes[theme] {
			skipped = append(skipped, V1SkippedSetting{"theme", "v2 has no theme with this name; pick one in Settings → General"})
		} else {
			prefs["theme"] = theme
		}
	}
	if raw, ok := settings["notification_volume"]; ok && string(raw) != "null" {
		var volume float64
		if json.Unmarshal(raw, &volume) != nil || volume < 0 || volume > 100 || volume != float64(int(volume)) {
			skipped = append(skipped, V1SkippedSetting{"notification_volume", "invalid v1 value"})
		} else {
			prefs["inbox_volume"] = strconv.Itoa(int(volume))
		}
	}
	if raw, ok := settings["notification_sound"]; ok && string(raw) != "null" {
		var sound string
		if json.Unmarshal(raw, &sound) == nil && len(sound) > 8 && sound[:8] == "builtin:" && v2BuiltinSounds[sound[8:]] {
			prefs["inbox_sound"] = sound
		} else if sound == "" {
			// v1's default: nothing chosen, nothing to carry.
		} else {
			skipped = append(skipped, V1SkippedSetting{"notification_sound", "pick the sound again in Settings → Notifications; v2 keeps its sounds under other names"})
		}
	}
	return prefs, skipped
}

// importV1Preferences writes the owner's carried preferences, never replacing one the owner already has.
func importV1Preferences(ctx context.Context, tx *sql.Tx, data *v1ImportData, ownerID string, report *V1ImportReport) error {
	prefs, _ := v1PersonalPreferences(data.Settings)
	for _, name := range sortedImportKeys(prefs) {
		result, err := tx.ExecContext(ctx, `INSERT INTO user_preference(user_id,key,value,updated_at) VALUES (?,?,?,?) ON CONFLICT(user_id,key) DO NOTHING`, ownerID, name, prefs[name], timestamp())
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			report.Imported["preferences"]++
		} else {
			report.AlreadyPresent["preferences"]++
		}
	}
	return nil
}
