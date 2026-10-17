package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/switcher"
)

func writeCred(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeDoc(access, refresh string, expires time.Time) string {
	r := ""
	if refresh != "" {
		r = fmt.Sprintf(`"refreshToken":%q,`, refresh)
	}
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,%s"expiresAt":%d}}`, access, r, expires.UnixMilli())
}

func stubLookPath(t *testing.T, found bool) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) {
		if found {
			return "/usr/bin/harness-docker-ctrl", nil
		}
		return "", fmt.Errorf("not found")
	}
	t.Cleanup(func() { lookPath = old })
}

func TestCredentialTargetsIsSilentWithoutTheTokenOnlyMarker(t *testing.T) {
	root := t.TempDir()
	writeCred(t, filepath.Join(root, "switcher", "profiles", "claude", "main", ".credentials.json"), claudeDoc("a", "r", time.Now().Add(-time.Hour)))
	if got := credentialTargetResults(root, time.Now()); got != nil {
		t.Fatalf("a link-based install must produce no credential results, got %+v", got)
	}
}

func TestCredentialTargetsReportsExpiryAndStatesWithTheServerStopped(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	live := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	t.Setenv("CODEX_HOME", filepath.Join(live, "codex"))
	sw := filepath.Join(root, "switcher")
	writeCred(t, filepath.Join(sw, "token-copies.json"), `{"version":1}`)
	writeCred(t, filepath.Join(sw, "state.json"), `{"version":1,"enabled":true,"active":{"claude":"main"}}`)
	writeCred(t, filepath.Join(sw, "profiles", "claude", "main", ".credentials.json"), claudeDoc("a", "r-main", now.Add(5*time.Hour)))
	writeCred(t, filepath.Join(sw, "profiles", "claude", "spare", ".credentials.json"), claudeDoc("a", "r-spare", now.Add(-5*time.Minute)))
	writeCred(t, filepath.Join(live, ".credentials.json"), claudeDoc("live", "", now.Add(9*time.Minute)))

	results := credentialTargetResults(root, now)
	byName := map[string]Result{}
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName["credential claude/spare"]; r.Status != StatusFail || !strings.Contains(r.Message, "expired") {
		t.Fatalf("spare = %+v", r)
	}
	if r := byName["credential claude/live"]; r.Status != StatusWarn || !strings.Contains(r.Message, "15 minutes") {
		t.Fatalf("live = %+v (a token-only file with 9 minutes left must warn)", r)
	}
	if r := byName["credential targets"]; r.Status != StatusOK || !strings.Contains(r.Message, "1 credential(s) healthy") {
		t.Fatalf("summary = %+v", r)
	}
	if _, exists := byName["credential claude/main"]; exists {
		t.Fatal("a healthy credential must be folded into the summary")
	}
}

func TestClaudeAccountFileResultsWarnOnDisagreement(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := claudeAccountFileResults(root, home); got != nil {
		t.Fatalf("a link-based install must say nothing, got %v", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "switcher"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "switcher", tokenCopiesMarker), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, email string) {
		body := `{"oauthAccount":{"emailAddress":"` + email + `"}}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", ".claude.json"), "alice@example.com")
	write(filepath.Join(home, ".claude.json"), "alice@example.com")
	if got := claudeAccountFileResults(root, home); got != nil {
		t.Fatalf("agreeing files must be silent, got %v", got)
	}
	write(filepath.Join(home, ".claude.json"), "carol@example.com")
	got := claudeAccountFileResults(root, home)
	if len(got) != 1 || got[0].Status != StatusWarn {
		t.Fatalf("results = %v, want one warning", got)
	}
}

func TestCredentialTargetsFollowTheMarkerPerTool(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	live := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	t.Setenv("CODEX_HOME", filepath.Join(live, "codex"))
	seed := func(marker string) string {
		root := t.TempDir()
		sw := filepath.Join(root, "switcher")
		writeCred(t, filepath.Join(sw, "token-copies.json"), marker)
		writeCred(t, filepath.Join(sw, "state.json"), `{"version":1,"enabled":true,"active":{"claude":"main"}}`)
		writeCred(t, filepath.Join(sw, "profiles", "claude", "gone", ".credentials.json"), claudeDoc("a", "r", now.Add(-5*time.Minute)))
		writeCred(t, filepath.Join(sw, "profiles", "codex", "old", "auth.json"), `{"tokens":{"access_token":"a","refresh_token":"r"}}`)
		return root
	}
	stamp := now.UTC().Format(time.RFC3339)

	if got := credentialTargetResults(seed(`{"version":2,"migrated":{"codex":"`+stamp+`"}}`), now); hasCred(got, "claude") {
		t.Fatalf("claude is not migrated, doctor must not report it: %+v", got)
	}
	if got := credentialTargetResults(seed(`{"version":2,"migrated":{"claude":"`+stamp+`"}}`), now); !hasCred(got, "claude") {
		t.Fatalf("claude is migrated, doctor must report it: %+v", got)
	}
	if got := credentialTargetResults(seed(`{"version":2,"migrated":{}}`), now); got != nil {
		t.Fatalf("no migrated tool must be silent, got %+v", got)
	}
	if got := credentialTargetResults(seed(`{"version":1}`), now); !hasCred(got, "claude") {
		t.Fatalf("a version 1 marker means both tools: %+v", got)
	}
}

func hasCred(results []Result, tool string) bool {
	for _, r := range results {
		if strings.HasPrefix(r.Name, "credential "+tool+"/") {
			return true
		}
	}
	return false
}

func TestCredentialTargetsWarnWhenTheRefresherBinaryIsMissing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	live := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", live)
	t.Setenv("CODEX_HOME", filepath.Join(live, "codex"))
	root := t.TempDir()
	sw := filepath.Join(root, "switcher")
	writeCred(t, filepath.Join(sw, "token-copies.json"), `{"version":1}`)
	writeCred(t, filepath.Join(sw, "state.json"), `{"version":1,"enabled":true,"active":{"claude":"main"}}`)
	writeCred(t, filepath.Join(sw, "profiles", "claude", "main", ".credentials.json"), claudeDoc("a", "r", now.Add(5*time.Hour)))
	find := func() *Result {
		for _, r := range credentialTargetResults(root, now) {
			if r.Name == "credential refresher" {
				r := r
				return &r
			}
		}
		return nil
	}
	stubLookPath(t, false)
	if r := find(); r == nil || r.Status != StatusWarn || !strings.Contains(r.Message, "harness-docker-ctrl") {
		t.Fatalf("a migrated install without the binary must warn, got %+v", r)
	}
	stubLookPath(t, true)
	if r := find(); r != nil {
		t.Fatalf("binary present: no refresher warning, got %+v", r)
	}
	// not migrated at all: silent regardless of the binary
	stubLookPath(t, false)
	if got := credentialTargetResults(t.TempDir(), now); got != nil {
		t.Fatalf("unmigrated install must stay silent, got %+v", got)
	}
}

// The marker the real switcher writes must be what doctor reads (a hand-built
// JSON in a test cannot catch a key rename).
func TestDoctorReadsTheMarkerTheSwitcherWrites(t *testing.T) {
	root := t.TempDir()
	m, err := switcher.New(filepath.Join(root, "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetTokenCopies(true); err != nil {
		t.Fatal(err)
	}
	got := migratedTools(filepath.Join(root, "switcher"))
	if !got["claude"] || !got["codex"] {
		t.Fatalf("doctor sees %v after SetTokenCopies(true), want both tools", got)
	}
}
