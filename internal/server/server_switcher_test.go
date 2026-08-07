package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// newSwitcherTestEnv creates a Manager with temp directories, pre-wired so no
// real ~/.claude or ~/.codex is touched.
func newSwitcherTestEnv(t *testing.T) (*switcher.Manager, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	swRoot := filepath.Join(base, "config", "switcher")
	m := switcherTestManager(t, swRoot, home)
	return m, home
}

// switcherTestManager builds a Manager with an injected home directory so no
// real ~/.claude or ~/.codex is touched during tests.
func switcherTestManager(t *testing.T, root, home string) *switcher.Manager {
	t.Helper()
	// Injecting a home is not enough: claude's paths follow CLAUDE_CONFIG_DIR
	// when set, and a value inherited from the developer's environment would
	// point the lookup at their real credentials instead of the fixture.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	m, err := switcher.New(root)
	if err != nil {
		t.Fatalf("switcher.New: %v", err)
	}
	m.SetHomeDir(home)
	return m
}

func newSwitcherMux(t *testing.T, sm *switcher.Manager) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	switcherRoutes{sm: sm, um: nil}.mount(mux)
	return mux
}

func httpDo(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	var req *http.Request
	if bodyReader != nil {
		req = httptest.NewRequest(method, path, bodyReader)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// writeLiveSwitcherCred writes a credential file to the fake home dir.
func writeLiveSwitcherCred(t *testing.T, home, tool, content string) {
	t.Helper()
	var path string
	switch tool {
	case switcher.ToolClaude:
		path = usages.ClaudeLiveCredPath(home)
	case switcher.ToolCodex:
		path = filepath.Join(home, ".codex", "auth.json")
	default:
		t.Fatalf("unknown tool %q", tool)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func futureCladeCred(token string) string {
	ms := time.Now().Add(24 * time.Hour).UnixMilli()
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"reftok","expiresAt":%d}}`, token, ms)
}

// ── GET /api/usages/switcher ──────────────────────────────────────────────────

func TestSwitcherGetDisabled(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "GET", "/api/usages/switcher", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("expected enabled=false; body = %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"profiles":[]`) {
		t.Fatalf("expected empty profiles; body = %s", rr.Body.String())
	}
}

func TestSwitcherGetWithProfiles(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	// Enable + import a profile.
	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok-main"))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	rr := httpDo(t, mux, "GET", "/api/usages/switcher", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("expected enabled=true; body = %s", body)
	}
	if !strings.Contains(body, `"name":"main"`) {
		t.Fatalf("expected profile name=main; body = %s", body)
	}
	if !strings.Contains(body, `"active":true`) {
		t.Fatalf("expected active=true; body = %s", body)
	}
}

// TestSwitcherGetNoCredentialMaterial verifies that credential tokens never
// appear in the GET response body.
func TestSwitcherGetNoCredentialMaterial(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	secretToken := "SUPER_SECRET_ACCESS_TOKEN_XYZ"
	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred(secretToken))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	rr := httpDo(t, mux, "GET", "/api/usages/switcher", "")
	if strings.Contains(rr.Body.String(), secretToken) {
		t.Fatalf("credential token leaked into GET response: %s", rr.Body.String())
	}
}

// ── POST /api/usages/switcher/settings ───────────────────────────────────────

func TestSwitcherSettings(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/settings", `{"enabled":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"enabled":true`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
	enabled, err := sm.Enabled()
	if err != nil || !enabled {
		t.Fatalf("Enabled() = %v, %v", enabled, err)
	}
}

func TestSwitcherSettingsInvalidJSON(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/settings", `not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

// ── POST /api/usages/switcher/switch ─────────────────────────────────────────

func TestSwitcherSwitchHappyPath(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok-main"))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := sm.Add(switcher.ToolClaude, "work"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/switch", `{"tool":"claude","profile":"work"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"active":"work"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestSwitcherSwitchDisabled(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok"))
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		// Import requires enabled is not enforced, but Switch does.
		_ = sm.SetEnabled(true)
		_ = sm.Import(switcher.ToolClaude, "main")
		_ = sm.SetEnabled(false)
	}
	if err := sm.Add(switcher.ToolClaude, "work"); err != nil {
		_ = sm.SetEnabled(true)
		_ = sm.Add(switcher.ToolClaude, "work")
		_ = sm.SetEnabled(false)
	}

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/switch", `{"tool":"claude","profile":"work"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

func TestSwitcherSwitchNotFound(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok"))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/switch", `{"tool":"claude","profile":"nonexistent"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

func TestSwitcherSwitchNoCredentialMaterial(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	secret := "VERY_SECRET_REFRESH_TOKEN_ABC"
	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred(secret))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := sm.Add(switcher.ToolClaude, "work"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/switch", `{"tool":"claude","profile":"work"}`)
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("credential token leaked into switch response: %s", rr.Body.String())
	}
}

// ── POST /api/usages/switcher/profiles ───────────────────────────────────────

func TestSwitcherProfileAdd(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"dev","mode":"add"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"mode":"add"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestSwitcherProfileImport(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok"))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"personal","mode":"import"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

func TestSwitcherProfileInvalidMode(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"x","mode":"invent"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestSwitcherProfileExists(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "POST", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"dup","mode":"add"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("first add status = %d", rr.Code)
	}
	rr = httpDo(t, mux, "POST", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"dup","mode":"add"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate add status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

// ── DELETE /api/usages/switcher/profiles ─────────────────────────────────────

func TestSwitcherProfileDelete(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	if err := sm.Add(switcher.ToolClaude, "tmp"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rr := httpDo(t, mux, "DELETE", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"tmp"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"deleted":"tmp"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestSwitcherProfileDeleteActiveRequiresForce(t *testing.T) {
	sm, home := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	writeLiveSwitcherCred(t, home, switcher.ToolClaude, futureCladeCred("tok"))
	if err := sm.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if err := sm.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Without force: should get 409 so the UI can confirm.
	rr := httpDo(t, mux, "DELETE", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"main","force":false}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("no-force status = %d; body = %s", rr.Code, rr.Body.String())
	}

	// With force: allowed.
	rr = httpDo(t, mux, "DELETE", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"main","force":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("force status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

func TestSwitcherProfileDeleteNotFound(t *testing.T) {
	sm, _ := newSwitcherTestEnv(t)
	mux := newSwitcherMux(t, sm)

	rr := httpDo(t, mux, "DELETE", "/api/usages/switcher/profiles", `{"tool":"claude","profile":"ghost"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
	}
}

// ── Per-profile reading ───────────────────────────────────────────────────────

func TestProfileCredPath(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	swRoot := filepath.Join(base, "config", "switcher")
	sm, _ := switcher.New(swRoot)
	sm.SetHomeDir(home)
	routes := switcherRoutes{sm: sm, um: nil}

	// 1. Active Claude with CLAUDE_CONFIG_DIR set
	t.Run("ClaudeActiveWithEnv", func(t *testing.T) {
		customCfg := filepath.Join(base, "custom_cfg")
		t.Setenv("CLAUDE_CONFIG_DIR", customCfg)
		p := switcher.Profile{Tool: switcher.ToolClaude, Name: "main", Active: true}
		want := filepath.Join(customCfg, ".credentials.json")
		if got := routes.profileCredPath(p); got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	})

	// 2. Active Claude with CLAUDE_CONFIG_DIR unset
	t.Run("ClaudeActiveNoEnv", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		p := switcher.Profile{Tool: switcher.ToolClaude, Name: "main", Active: true}
		want := filepath.Join(home, ".claude", ".credentials.json")
		if got := routes.profileCredPath(p); got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	})

	// 3. Active Codex with CODEX_HOME set
	t.Run("CodexActiveWithEnv", func(t *testing.T) {
		customHome := filepath.Join(base, "custom_codex")
		t.Setenv("CODEX_HOME", customHome)
		p := switcher.Profile{Tool: switcher.ToolCodex, Name: "main", Active: true}
		want := filepath.Join(customHome, "auth.json")
		if got := routes.profileCredPath(p); got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	})

	// 4. Active Codex with CODEX_HOME unset
	t.Run("CodexActiveNoEnv", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "")
		p := switcher.Profile{Tool: switcher.ToolCodex, Name: "main", Active: true}
		want := filepath.Join(home, ".codex", "auth.json")
		if got := routes.profileCredPath(p); got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	})

	// 5. Inactive profile reads from stored copy
	t.Run("InactiveProfile", func(t *testing.T) {
		p := switcher.Profile{Tool: switcher.ToolClaude, Name: "backup", Active: false}
		want := filepath.Join(swRoot, "profiles", "claude", "backup", ".credentials.json")
		if got := routes.profileCredPath(p); got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	})
}

// TestProfileReaderUsePersistFalse verifies that fetching a non-active profile
// snapshot never writes back to the stored credential file (persist=false).
// The profile's stored codex auth.json records a specific last_refresh; after
// FetchProfileSnapshot the file must be unchanged.
func TestProfileReaderUsePersistFalse(t *testing.T) {
	store := usages.NewStoreAt(t.TempDir())
	m := usages.NewManager(store, usages.EmptyRegistry())

	dir := t.TempDir()
	// Write a codex auth.json that looks very old (last_refresh=epoch+1) so a
	// persist=true path would attempt a token refresh and rewrite the file.
	oldContent := `{"tokens":{"access_token":"acc","refresh_token":"ref","id_token":"x.e30K.sig"},"last_refresh":"1970-01-01T00:00:01Z"}`
	credPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(credPath, []byte(oldContent), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := func() time.Time {
		fi, err := os.Stat(credPath)
		if err != nil {
			t.Fatal(err)
		}
		return fi.ModTime()
	}
	before := mtime()

	// FetchProfileSnapshot with a codex path — it will fail with auth_missing
	// since there's no network, but it must not have written the file.
	_, _ = m.FetchProfileSnapshot(t.Context(), "codex", "old-profile", credPath)

	after := mtime()
	if !after.Equal(before) {
		t.Fatalf("stored credential was modified: before=%v after=%v (persist=true leaked in)", before, after)
	}
}

// TestProfileReaderActiveFromLive verifies that the active profile's snapshot
// is fetched from the live harness directory path, not the stored copy. We
// pass the live path directly (as switcherRoutes does) and confirm the fetch
// call sees the current live credentials.
func TestProfileReaderActiveFromLive(t *testing.T) {
	store := usages.NewStoreAt(t.TempDir())
	m := usages.NewManager(store, usages.EmptyRegistry())

	liveDir := t.TempDir()
	liveCred := filepath.Join(liveDir, ".credentials.json")
	ms := time.Now().Add(24 * time.Hour).UnixMilli()
	liveContent := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"live-tok","refreshToken":"ref","expiresAt":%d}}`, ms)
	if err := os.WriteFile(liveCred, []byte(liveContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// A path that cannot be read short-circuits to auth_missing with no error,
	// before any network call is attempted. That is the signature of reading
	// the wrong (empty, stored) path, and it is what this test must rule out.
	missing, missingErr := m.FetchProfileSnapshot(t.Context(), "claude", "gone",
		filepath.Join(t.TempDir(), "nope.json"))
	if missingErr != nil || missing.Status != usages.StatusAuthMissing {
		t.Fatalf("unreadable path should yield auth_missing with no error; got status=%q err=%v",
			missing.Status, missingErr)
	}

	// The live path parses, so the fetch gets past credential loading. What the
	// network then says is irrelevant — a rejected token and an unreadable file
	// BOTH surface as auth_missing, so the status alone cannot tell them apart.
	// The distinguishing signal is the message: only a file that could not be
	// read reports "credentials not found".
	//
	// Short deadline because the network result is not part of the assertion;
	// without it this test spends ten seconds waiting for a timeout it ignores.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	snap, _ := m.FetchProfileSnapshot(ctx, "claude", "main", liveCred)
	if strings.Contains(snap.Error, "credentials not found") {
		t.Fatalf("live path was not read: %q", snap.Error)
	}
}
