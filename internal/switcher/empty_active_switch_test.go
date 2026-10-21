package switcher

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// A dead or logged-out active login must never trap the user on it.
// Every shape of the live credential file is tried on a migrated (token-only)
// install, where the switch takes a different path from the link model.
//
// Switching away from a logged-out live file must also capture nothing: the
// outgoing profile's stored full credential stays byte-identical, no backup of
// the empty state is written, and the refresher's journal is not touched.
func TestTokenCopySwitchAwayFromADeadActiveLogin(t *testing.T) {
	type shape struct {
		name     string
		live     *string // nil: the live file is missing
		wantFail bool
	}
	expiredClaude := tokenOnlyClaude("tok-a", time.Now().Add(-time.Hour))
	expiredCodex := tokenOnlyCodex(time.Now().Add(-time.Hour))
	tools := []struct {
		tool   string
		shapes []shape
	}{
		{ToolClaude, []shape{
			{"missing", nil, false},
			{"empty-object", ptr(`{}`), false},
			{"null-oauth", ptr(`{"claudeAiOauth":null}`), false},
			{"empty-tokens", ptr(`{"claudeAiOauth":{"accessToken":"","refreshToken":""}}`), false},
			{"metadata-only", ptr(`{"claudeAiOauth":{"expiresAt":123,"scopes":[]}}`), false},
			{"expired", &expiredClaude, false},
			{"malformed", ptr(`not-json`), true},
			{"half-written", ptr(`{"claudeAiOauth":{"accessToken":"tok`), true},
			{"partial-token", ptr(`{"claudeAiOauth":{"accessToken":"partial"}}`), true},
		}},
		{ToolCodex, []shape{
			{"missing", nil, false},
			{"empty-object", ptr(`{}`), false},
			{"null-tokens", ptr(`{"tokens":null}`), false},
			{"auth-mode-without-tokens", ptr(`{"auth_mode":"chatgpt","last_refresh":"2026-01-01T00:00:00Z"}`), false},
			{"empty-tokens", ptr(`{"tokens":{"access_token":"","refresh_token":""},"last_refresh":"2026-01-01T00:00:00Z"}`), false},
			{"identity-only", ptr(`{"tokens":{"id_token":"old-identity"}}`), false},
			{"expired", &expiredCodex, false},
			{"malformed", ptr(`not-json`), true},
			{"partial-token", ptr(`{"tokens":{"access_token":"partial"}}`), true},
			{"api-key", ptr(`{"OPENAI_API_KEY":"still-a-credential"}`), true},
		}},
	}
	for _, group := range tools {
		for _, tc := range group.shapes {
			t.Run(group.tool+"/"+tc.name, func(t *testing.T) {
				m, live, _ := migratedManager(t, group.tool)
				outgoing := m.profileCredPath(group.tool, "a")
				storedBefore := readFile(t, outgoing)
				backupsBefore := backupNames(t, m, group.tool)
				if tc.live == nil {
					must(t, os.Remove(live))
				} else {
					must(t, os.WriteFile(live, []byte(*tc.live), 0o600))
				}
				liveBefore := ""
				if tc.live != nil {
					liveBefore = *tc.live
				}

				active, switched, err := m.Switch(group.tool, "b")

				if got := readFile(t, outgoing); got != storedBefore {
					t.Fatalf("outgoing stored credential changed:\nbefore %s\nafter  %s", storedBefore, got)
				}
				if after := backupNames(t, m, group.tool); !reflect.DeepEqual(after, backupsBefore) {
					t.Fatalf("a backup of the live state was written: %v -> %v", backupsBefore, after)
				}
				if _, err := os.Stat(filepath.Join(m.root, "refresh-state.json")); !os.IsNotExist(err) {
					t.Fatalf("the refresher journal was touched by a switch: %v", err)
				}

				if tc.wantFail {
					if err == nil || switched {
						t.Fatalf("Switch = (%q, %v, %v), want a refusal", active, switched, err)
					}
					if a, _ := m.Active(group.tool); a != "a" {
						t.Fatalf("refused switch moved active to %q", a)
					}
					if tc.live != nil {
						if got := readFile(t, live); got != liveBefore {
							t.Fatal("refused switch changed the live file")
						}
					}
					return
				}
				if err != nil || !switched || active != "b" {
					t.Fatalf("Switch = (%q, %v, %v), want (b, true, nil)", active, switched, err)
				}
				if a, _ := m.Active(group.tool); a != "b" {
					t.Fatalf("active = %q", a)
				}
			})
		}
	}
}

// migratedManager builds a migrated install for tool: profiles a (active) and b
// as regular full-credential files, a marker, and a fan-out that writes the
// incoming profile's bytes to the live file.
func migratedManager(t *testing.T, tool string) (m *Manager, live, home string) {
	t.Helper()
	if tool == ToolClaude {
		m, live, _ = tokenCopyManager(t)
		return m, live, filepath.Dir(m.root)
	}
	m, home = newTestManager(t)
	must(t, m.SetEnabled(true))
	live = writeLive(t, home, ToolCodex, futureCodex("a@example.test"))
	must(t, m.Import(ToolCodex, "a"))
	must(t, m.Add(ToolCodex, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolCodex, "b"), []byte(futureCodex("b@example.test")), 0o600))
	stored := m.profileCredPath(ToolCodex, "a")
	must(t, os.Remove(stored)) // Import linked it to live: store a regular file
	must(t, os.WriteFile(stored, []byte(futureCodex("a@example.test")), 0o600))
	must(t, os.WriteFile(live, []byte(tokenOnlyCodex(time.Now().Add(6*time.Hour))), 0o600))
	must(t, m.SetTokenCopies(true))
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(tool string, full []byte) error {
		return os.WriteFile(live, full, 0o600)
	}})
	return m, live, home
}

func tokenOnlyCodex(expires time.Time) string {
	return `{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"live-access","refresh_token":"","id_token":` +
		`"` + makeTestJWT("a@example.test", expires.Unix()) + `"}}`
}

func ptr(s string) *string { return &s }

func backupNames(t *testing.T, m *Manager, tool string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.backupsRoot(), tool))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
