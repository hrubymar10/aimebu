package usages

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMarker(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, TokenCopiesMarkerFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The marker's on-disk key names are a contract between the switcher (writer),
// the refresher and doctor (readers): this pins them.
func TestTokenCopiesMarkerShape(t *testing.T) {
	m, ok := ReadTokenCopiesMarker(writeMarker(t, `{"version":2,"migrated_at":"2026-10-16T10:00:00Z","migrated":{"claude":"2026-10-16T10:00:00Z"}}`))
	if !ok || !m.Any() {
		t.Fatal("a version 2 marker with a tool must be readable")
	}
	if at, ok := m.Since("claude"); !ok || at.Hour() != 10 {
		t.Fatalf("claude since = %v %v", at, ok)
	}
	if _, ok := m.Since("codex"); ok {
		t.Fatal("a tool absent from `migrated` is not migrated")
	}
}

func TestTokenCopiesMarkerVersionOneAndUnreadable(t *testing.T) {
	m, ok := ReadTokenCopiesMarker(writeMarker(t, `{"version":1,"migrated_at":"2026-10-16T10:00:00Z"}`))
	if !ok || !m.Any() {
		t.Fatal("version 1 must be readable")
	}
	for _, tool := range []string{"claude", "codex"} {
		if at, ok := m.Since(tool); !ok || !at.Equal(time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)) {
			t.Fatalf("version 1 means both tools, %s = %v %v", tool, at, ok)
		}
	}
	for name, body := range map[string]string{"empty tools": `{"version":2,"migrated":{}}`} {
		m, ok := ReadTokenCopiesMarker(writeMarker(t, body))
		if !ok || m.Any() {
			t.Fatalf("%s: readable but no tool migrated, got any=%v ok=%v", name, m.Any(), ok)
		}
	}
	for name, body := range map[string]string{"corrupt": `{not json`, "unknown version": `{"version":0}`} {
		if _, ok := ReadTokenCopiesMarker(writeMarker(t, body)); ok {
			t.Fatalf("%s must not count as a marker", name)
		}
	}
	if _, ok := ReadTokenCopiesMarker(t.TempDir()); ok {
		t.Fatal("a missing marker is not a marker")
	}
}
