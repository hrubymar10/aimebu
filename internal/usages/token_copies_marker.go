package usages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// TokenCopiesMarkerFile is the marker `aimebu switcher migrate-copies` writes in
// the switcher root. Its presence switches a tool to the token-copy model.
const TokenCopiesMarkerFile = "token-copies.json"

// TokenCopiesMarker is the marker's one definition. The switcher writes it; the
// switcher, the refresher and doctor read it through ReadTokenCopiesMarker, so
// the three can never disagree about the shape.
//
// Version 1 was a single global flag (both tools migrated at MigratedAt).
// Version 2 records a time per migrated tool in Migrated; a tool absent from it
// is still on the link model.
type TokenCopiesMarker struct {
	Version    int                  `json:"version"`
	MigratedAt time.Time            `json:"migrated_at"`
	Migrated   map[string]time.Time `json:"migrated,omitempty"`
}

// ReadTokenCopiesMarker reads the marker under switcherRoot. ok is false when it
// is missing, unreadable, corrupt or of an unknown version: no tool is migrated.
func ReadTokenCopiesMarker(switcherRoot string) (TokenCopiesMarker, bool) {
	data, err := os.ReadFile(filepath.Join(switcherRoot, TokenCopiesMarkerFile))
	if err != nil {
		return TokenCopiesMarker{}, false
	}
	var marker TokenCopiesMarker
	if json.Unmarshal(data, &marker) != nil || marker.Version < 1 {
		return TokenCopiesMarker{}, false
	}
	return marker, true
}

// Since is when tool was migrated; false when it was not. A zero time (a marker
// that records the tool without a time) still counts as migrated.
func (m TokenCopiesMarker) Since(tool string) (time.Time, bool) {
	if m.Version < 2 {
		return m.MigratedAt, true
	}
	at, ok := m.Migrated[tool]
	return at, ok
}

// Any reports whether at least one tool is migrated.
func (m TokenCopiesMarker) Any() bool {
	return m.Version < 2 || len(m.Migrated) > 0
}
