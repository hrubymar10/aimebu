package switcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// stripOnlyWriter stands in for the target writer: it writes the credential
// token-only, like the real one, so the reconciler's strip can be observed.
type stripOnlyWriter struct{ calls int }

func (w *stripOnlyWriter) WriteTarget(t credtarget.Target, full []byte) error {
	return w.write(t, full)
}

// WriteImportedLogin is the per-call way to overwrite a target holding a login.
func (w *stripOnlyWriter) WriteImportedLogin(t credtarget.Target, full []byte) error {
	return w.write(t, full)
}

func (w *stripOnlyWriter) write(t credtarget.Target, full []byte) error {
	w.calls++
	var doc map[string]any
	if err := json.Unmarshal(full, &doc); err != nil {
		return err
	}
	switch t.Tool {
	case ToolClaude:
		if oauth, ok := doc["claudeAiOauth"].(map[string]any); ok {
			delete(oauth, "refreshToken")
		}
	case ToolCodex:
		if tokens, ok := doc["tokens"].(map[string]any); ok {
			tokens["refresh_token"] = ""
		}
	}
	out, _ := json.Marshal(doc)
	return os.WriteFile(t.Path, out, 0o600)
}

type reconcilerHarness struct {
	m      *Manager
	home   string
	rec    *credtarget.Reconciler
	writer *stripOnlyWriter
	alerts []string
	target string
}

func newReconcilerHarness(t *testing.T) *reconcilerHarness {
	t.Helper()
	m, home := enabledManager(t)
	h := &reconcilerHarness{m: m, home: home, writer: &stripOnlyWriter{}}
	registry := credtarget.NewRegistry(filepath.Join(home, "server"), home)
	h.rec = &credtarget.Reconciler{
		Registry: registry,
		Writer:   h.writer,
		Store:    NewCredStore(m),
		Home:     home,
		Ready:    m.TokenCopies,
		// Fixtures are written a moment before the pass; the settle delay has
		// its own test in credtarget.
		ImportSettle: -1,
		Now:          func() time.Time { return time.Date(2026, 10, 16, 21, 30, 5, 0, time.UTC) },
		Alert:        func(kind, detail string) { h.alerts = append(h.alerts, kind+": "+detail) },
	}
	h.rec.AccountLockWait = time.Second
	h.target = filepath.Join(home, ".claude", ".credentials.json")
	return h
}

func (h *reconcilerHarness) claudeAccount(t *testing.T, email string) {
	t.Helper()
	path := usages.ClaudeAccountFilePath(h.home)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600))
}

func (h *reconcilerHarness) writeTarget(t *testing.T, data string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(h.target), 0o700))
	must(t, os.WriteFile(h.target, []byte(data), 0o600))
}

func (h *reconcilerHarness) pass() error { return h.rec.Pass(context.Background()) }

func dirNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestReconcilerDoesNothingBeforeTheMigration(t *testing.T) {
	h := newReconcilerHarness(t)
	login := futureClaude("login-token")
	h.writeTarget(t, login)
	must(t, h.m.Add(ToolClaude, "main"))
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.target); got != login || h.writer.calls != 0 {
		t.Fatalf("an unmigrated install must be left alone: %q calls=%d", got, h.writer.calls)
	}
}

func TestReconcilerSameAccountLoginKeepsABackupAndReleasesTheLock(t *testing.T) {
	h := newReconcilerHarness(t)
	must(t, h.m.SetTokenCopies(true))
	old := futureClaude("old-token")
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(old), Email: "alice@example.test", SetActive: true}))
	login := futureClaude("new-token")
	h.writeTarget(t, login)
	h.claudeAccount(t, "alice@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.m.StoredCredPath(ToolClaude, "main")); got != login {
		t.Fatalf("the profile must hold the new login: %q", got)
	}
	info, err := os.Lstat(h.m.StoredCredPath(ToolClaude, "main"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored credential must be a regular 0600 file: %v %v", info, err)
	}
	var kept bool
	for _, name := range dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)) {
		if strings.HasPrefix(name, "main-") {
			kept = kept || readFile(t, filepath.Join(h.m.Root(), "backups", ToolClaude, name)) == old
		}
	}
	if !kept {
		t.Fatal("the replaced credential is not in the backups")
	}
	if left := dirNames(filepath.Join(h.m.Root(), ".maintenance")); len(left) != 0 {
		t.Fatalf("the account lock was left behind: %v", left)
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, h.target))) {
		t.Fatal("the target still holds a refresh token")
	}
	if len(h.alerts) != 0 {
		t.Fatalf("a same-account login is routine: %v", h.alerts)
	}
}

func TestReconcilerUnknownAccountLoginBecomesAProfileAndMovesActive(t *testing.T) {
	h := newReconcilerHarness(t)
	must(t, h.m.SetTokenCopies(true))
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("main-token")), Email: "alice@example.test", SetActive: true}))
	login := futureClaude("carol-token")
	h.writeTarget(t, login)
	h.claudeAccount(t, "carol@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	const created = "login-20261016T213005"
	if got := readFile(t, h.m.StoredCredPath(ToolClaude, created)); got != login {
		t.Fatalf("the login must be a new profile: %q", got)
	}
	if !strings.Contains(readFile(t, h.m.StoredCredPath(ToolClaude, "main")), "main-token") {
		t.Fatal("an existing profile was touched")
	}
	if active, _ := h.m.Active(ToolClaude); active != created {
		t.Fatalf("active = %q", active)
	}
	if got := h.m.ProfileEmail(ToolClaude, created); got != "carol@example.test" {
		t.Fatalf("email = %q", got)
	}
	var rescued bool
	for _, name := range dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)) {
		rescued = rescued || strings.HasPrefix(name, "_login-")
	}
	if !rescued {
		t.Fatal("no login rescue was kept")
	}
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "the active profile changed from main to "+created) {
		t.Fatalf("alerts = %v", h.alerts)
	}
	before, _ := h.m.ProfileNames(ToolClaude)
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if after, _ := h.m.ProfileNames(ToolClaude); len(after) != len(before) {
		t.Fatalf("a second pass created profiles: %v -> %v", before, after)
	}
}

func TestReconcilerEmptyActiveProfileTakesTheDocumentedFirstLogin(t *testing.T) {
	h := newReconcilerHarness(t)
	must(t, h.m.SetTokenCopies(true))
	// `switcher add main`, `switcher use main`, then `claude login`.
	must(t, h.m.Add(ToolClaude, "main"))
	state, _ := h.m.loadState()
	state.Active[ToolClaude] = "main"
	must(t, h.m.saveState(state))
	login := futureClaude("first-login")
	h.writeTarget(t, login)
	h.claudeAccount(t, "alice@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.m.StoredCredPath(ToolClaude, "main")); got != login {
		t.Fatalf("the login belongs in the active profile: %q", got)
	}
	if names, _ := h.m.ProfileNames(ToolClaude); len(names) != 1 {
		t.Fatalf("no extra profile may be created: %v", names)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func TestReconcilerRefusesWhileAProfileIsStillLinkedToTheLiveFile(t *testing.T) {
	h := newReconcilerHarness(t)
	must(t, h.m.SetTokenCopies(true)) // even a wrongly set marker must not allow stripping a linked file
	live := futureClaude("live-token")
	h.writeTarget(t, live)
	must(t, h.m.Add(ToolClaude, "main"))
	must(t, os.Symlink(h.target, h.m.StoredCredPath(ToolClaude, "main")))
	h.claudeAccount(t, "someone@example.test")

	err := h.pass()
	if !errors.Is(err, credtarget.ErrNotMigrated) {
		t.Fatalf("pass = %v, want ErrNotMigrated", err)
	}
	if got := readFile(t, h.target); !bytes.Equal([]byte(got), []byte(live)) {
		t.Fatalf("the live file, the only holder of the refresh token, was changed: %s", got)
	}
	if names, _ := h.m.ProfileNames(ToolClaude); len(names) != 1 || h.writer.calls != 0 {
		t.Fatalf("nothing may be created or written: %v calls=%d", names, h.writer.calls)
	}
	if len(h.alerts) != 1 || !strings.Contains(h.alerts[0], "migration") {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

// A rewrite of a lagging target must wait for a Switch in flight: the Switch
// holds the switcher lock while it moves the pointer and rewrites the live
// file, and a heal that raced it could put the previous account's token back.
func TestReconcilerHealWaitsForTheSwitcherLock(t *testing.T) {
	h := newReconcilerHarness(t)
	must(t, h.m.SetTokenCopies(true))
	fresh := futureClaude("fresh-token")
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(fresh), Email: "alice@example.test", SetActive: true}))
	stale := `{"claudeAiOauth":{"accessToken":"stale-token","expiresAt":1}}`
	h.writeTarget(t, stale)

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = h.m.WithLock(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	done := make(chan error, 1)
	go func() { done <- h.pass() }()
	select {
	case err := <-done:
		t.Fatalf("the heal ran while the switcher lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if got := readFile(t, h.target); got != stale {
		t.Fatalf("the target changed under a held lock: %q", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if h.writer.calls != 1 {
		t.Fatalf("the heal should write once after the lock is free: %d", h.writer.calls)
	}
}

// The strip must work with the real writer, whose ordinary write refuses a
// target that still holds a refresh token. A fake writer cannot show that.
func TestReconcilerStripsARealLoginWithTheRealWriter(t *testing.T) {
	h := newReconcilerHarness(t)
	h.rec.Writer = &credtarget.Writer{}
	must(t, h.m.SetTokenCopies(true))
	must(t, h.m.Add(ToolClaude, "main"))
	state, _ := h.m.loadState()
	state.Active[ToolClaude] = "main"
	must(t, h.m.saveState(state))
	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	login := `{"claudeAiOauth":{"accessToken":"first-access","refreshToken":"first-refresh-SECRET","expiresAt":` + itoa(expires) + `,"scopes":["user:inference"]},"mcpOAuth":{"plane":{"accessToken":"mcp-token","refreshToken":"mcp-refresh"}}}`
	h.writeTarget(t, login)
	h.claudeAccount(t, "alice@example.test")

	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, h.target)
	if strings.Contains(got, "first-refresh-SECRET") || credtarget.HasRefreshToken(ToolClaude, []byte(got)) {
		t.Fatalf("the target still holds the refresh token: %s", got)
	}
	if !strings.Contains(got, `"mcpOAuth":{"plane":{"accessToken":"mcp-token","refreshToken":"mcp-refresh"}}`) || !strings.Contains(got, "first-access") {
		t.Fatalf("the rewrite must keep the worker's MCP logins and the access token: %s", got)
	}
	if info, _ := os.Stat(h.target); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	stored := readFile(t, h.m.StoredCredPath(ToolClaude, "main"))
	if !strings.Contains(stored, "first-refresh-SECRET") {
		t.Fatalf("the profile store must hold the full login: %s", stored)
	}
	var rescued bool
	for _, name := range dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)) {
		rescued = rescued || strings.HasPrefix(name, "_login-")
	}
	if !rescued {
		t.Fatal("no rescue copy was kept")
	}
	if left := dirNames(filepath.Join(h.home, ".claude")); contains(left, ".storage-write.lock") {
		t.Fatalf("the write lock was left behind: %v", left)
	}
	if len(h.alerts) != 0 {
		t.Fatalf("alerts = %v", h.alerts)
	}
}

func itoa(n int64) string { return fmt.Sprint(n) }

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ── Import under the token-copy model (review F3) ────────────────────────────

type importHarness struct {
	*reconcilerHarness
	live       string
	replaced   int
	replaceErr error
}

func newImportHarness(t *testing.T) *importHarness {
	t.Helper()
	h := &importHarness{reconcilerHarness: newReconcilerHarness(t)}
	h.live = h.target
	must(t, h.m.SetTokenCopies(true))
	h.m.SetTokenCopyHooks(TokenCopyHooks{
		FanOutActive: func(tool string, full []byte) error { return nil },
		ReplaceActive: func(tool string, full []byte) error {
			h.replaced++
			if h.replaceErr != nil {
				return h.replaceErr
			}
			return h.writer.WriteImportedLogin(credtarget.Target{ID: "live/" + tool, Tool: tool, Path: h.live, Role: credtarget.RoleLive}, full)
		},
	})
	return h
}

func TestImportOnAMigratedInstallStoresARegularFileAndStripsTheLiveFile(t *testing.T) {
	h := newImportHarness(t)
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "old", Create: true, Full: []byte(futureClaude("old-token")), Email: "old@example.test", SetActive: true}))
	login := futureClaude("fresh-login-token")
	h.writeTarget(t, login)
	h.claudeAccount(t, "alice@example.test")

	if err := h.m.Import(ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	path := h.m.StoredCredPath(ToolClaude, "main")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the stored credential must be a regular 0600 file, never a link: %v %v", info, err)
	}
	if got := readFile(t, path); got != login {
		t.Fatalf("the profile must hold the full login: %q", got)
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, h.live))) {
		t.Fatal("the live file still holds the refresh token the profile store holds")
	}
	if info, _ := os.Lstat(h.live); info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the live file became a link")
	}
	if active, _ := h.m.Active(ToolClaude); active != "main" {
		t.Fatalf("active = %q", active)
	}
	if got := h.m.ProfileEmail(ToolClaude, "main"); got != "alice@example.test" {
		t.Fatalf("email = %q", got)
	}
	if _, ok := h.m.loadSwitchedAway()[switchedAwayKey(ToolClaude, "old")]; !ok {
		t.Fatal("the profile that stopped being active must be stamped as switched away")
	}
	var rescued, backedUp bool
	for _, name := range dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)) {
		rescued = rescued || strings.HasPrefix(name, "_login-")
		backedUp = backedUp || strings.HasPrefix(name, "main-")
	}
	if !rescued || !backedUp {
		t.Fatalf("a rescue copy and a backup must be kept: rescued=%v backedUp=%v", rescued, backedUp)
	}
	// The old profile is a regular file too: nothing was linked or detached.
	if info, err := os.Lstat(h.m.StoredCredPath(ToolClaude, "old")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the previous profile must stay a regular file: %v %v", info, err)
	}
	if h.replaced != 1 {
		t.Fatalf("the live file is rewritten exactly once: %d", h.replaced)
	}
}

func TestImportOnAMigratedInstallRefusesWithoutTheFanOutHook(t *testing.T) {
	h := newImportHarness(t)
	h.m.SetTokenCopyHooks(TokenCopyHooks{})
	login := futureClaude("login-token")
	h.writeTarget(t, login)
	err := h.m.Import(ToolClaude, "main")
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("import = %v", err)
	}
	if names, _ := h.m.ProfileNames(ToolClaude); len(names) != 0 {
		t.Fatalf("nothing may be created: %v", names)
	}
	if got := readFile(t, h.live); got != login {
		t.Fatal("the live file must be untouched")
	}
}

func TestImportOnAMigratedInstallKeepsAConsistentStateWhenTheStripFails(t *testing.T) {
	h := newImportHarness(t)
	h.replaceErr = errors.New("disk full")
	login := futureClaude("login-token")
	h.writeTarget(t, login)
	if err := h.m.Import(ToolClaude, "main"); err != nil {
		t.Fatalf("the import itself succeeded and the reconciler finishes the strip: %v", err)
	}
	if got := readFile(t, h.m.StoredCredPath(ToolClaude, "main")); got != login {
		t.Fatal("the profile must hold the login")
	}
	if active, _ := h.m.Active(ToolClaude); active != "main" {
		t.Fatalf("active = %q", active)
	}
	// The reconciler's pass then strips it, because the stored bytes equal the live ones.
	h.replaceErr = nil
	h.rec.Writer = &credtarget.Writer{}
	if err := h.pass(); err != nil {
		t.Fatal(err)
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, h.live))) {
		t.Fatal("the reconciler must finish the strip")
	}
	if names, _ := h.m.ProfileNames(ToolClaude); len(names) != 1 {
		t.Fatalf("the reconciler must not mint a second profile: %v", names)
	}
}

func TestImportOnAMigratedInstallRefusesAnExistingName(t *testing.T) {
	h := newImportHarness(t)
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("kept")), SetActive: true}))
	login := futureClaude("login-token")
	h.writeTarget(t, login)
	if err := h.m.Import(ToolClaude, "main"); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("import = %v", err)
	}
	if !strings.Contains(readFile(t, h.m.StoredCredPath(ToolClaude, "main")), "kept") || readFile(t, h.live) != login || h.replaced != 0 {
		t.Fatal("a refused import must change nothing")
	}
}

func TestImportOnAMigratedInstallStillRefusesATokenOnlyLiveFile(t *testing.T) {
	h := newImportHarness(t)
	h.writeTarget(t, `{"claudeAiOauth":{"accessToken":"a","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`)
	err := h.m.Import(ToolClaude, "main")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a token-only live file is not a login: %v", err)
	}
	if names, _ := h.m.ProfileNames(ToolClaude); len(names) != 0 {
		t.Fatalf("nothing may be created: %v", names)
	}
}

func TestImportOnAMigratedInstallWithTheRealFanOutKeepsTheWorkersMCPLogins(t *testing.T) {
	h := newImportHarness(t)
	reg := credtarget.NewRegistry(filepath.Join(h.home, "server"), h.home)
	reg.LivePath = h.m.LiveCredPath
	fan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: h.m.Active, WithLock: h.m.WithLock}
	h.m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: fan.FanOutActive, ReplaceActive: fan.FanOutActiveReplacing})
	login := `{"claudeAiOauth":{"accessToken":"first-access","refreshToken":"first-refresh-SECRET","expiresAt":` + itoa(time.Now().Add(24*time.Hour).UnixMilli()) + `},"mcpOAuth":{"plane":{"accessToken":"mcp","refreshToken":"mcp-refresh"}}}`
	h.writeTarget(t, login)
	if err := h.m.Import(ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, h.live)
	if strings.Contains(got, "first-refresh-SECRET") || !strings.Contains(got, `"mcpOAuth":{"plane":{"accessToken":"mcp","refreshToken":"mcp-refresh"}}`) || !strings.Contains(got, "first-access") {
		t.Fatalf("the live file must be token-only and keep the MCP logins: %s", got)
	}
	if !strings.Contains(readFile(t, h.m.StoredCredPath(ToolClaude, "main")), "first-refresh-SECRET") {
		t.Fatal("the store must hold the full login")
	}
}

// Without the marker the link-based Import is unchanged.
func TestImportWithoutTheMarkerStillLinksTheLiveFile(t *testing.T) {
	m, home := enabledManager(t)
	live := writeLive(t, home, ToolClaude, futureClaude("live-token"))
	must(t, m.Import(ToolClaude, "main"))
	if info, err := os.Lstat(m.StoredCredPath(ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the legacy import links the stored file to live: %v %v", info, err)
	}
	if !strings.Contains(readFile(t, live), "live-token") {
		t.Fatal("the live file must be untouched")
	}
}

// The marker is per tool: a claude-only migration leaves codex on the
// link-based layout, and its import must keep linking.
func TestImportFollowsTheMarkerOfItsOwnTool(t *testing.T) {
	m, home := enabledManager(t)
	must(t, m.markTokenCopies(ToolClaude))
	live := writeLive(t, home, ToolCodex, futureCodex("kim@example.test"))
	must(t, m.Import(ToolCodex, "work"))
	if info, err := os.Lstat(m.StoredCredPath(ToolCodex, "work")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("codex is not migrated, so its import still links: %v %v", info, err)
	}
	if !strings.Contains(readFile(t, live), "refresh_token\":\"refresh\"") {
		t.Fatal("the codex live file must be untouched")
	}
}

// ── Remove and Rename under the token-copy model (NEW-2, L-1) ───────────────

func TestRemovingTheActiveProfileTombstonesTheStoredRefreshTokenUnderTheMarker(t *testing.T) {
	h := newImportHarness(t)
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("main-token")), Email: "a@example.test", SetActive: true}))
	// Under the model the live file is token-only: the refresh token lives only
	// in the store.
	h.writeTarget(t, `{"claudeAiOauth":{"accessToken":"live-access","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`)
	if err := h.m.Remove(ToolClaude, "main", true); err != nil {
		t.Fatal(err)
	}
	removed, err := usages.WasRefreshTokenRemoved(h.m.Root(), ToolClaude, "refresh-main-token")
	if err != nil || !removed {
		t.Fatalf("the removed profile's refresh token must be tombstoned (so a refresh in flight is recognised): removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(h.live); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removing the active profile clears the live file")
	}
}

func TestRemovingAnInactiveProfileStillTombstonesItsToken(t *testing.T) {
	h := newImportHarness(t)
	full := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"inactive-refresh","expiresAt":` + itoa(time.Now().Add(time.Hour).UnixMilli()) + `}}`
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "other", Create: true, Full: []byte(full)}))
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("main-token")), SetActive: true}))
	must(t, h.m.Remove(ToolClaude, "other", false))
	if removed, err := usages.WasRefreshTokenRemoved(h.m.Root(), ToolClaude, "inactive-refresh"); err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
}

func TestRemovingAProfileDeletesTheRescuedLoginThatCreatedIt(t *testing.T) {
	h := newImportHarness(t)
	login := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"login-refresh-SECRET","expiresAt":` + itoa(time.Now().Add(time.Hour).UnixMilli()) + `}}`
	other := `{"claudeAiOauth":{"accessToken":"b","refreshToken":"other-refresh","expiresAt":` + itoa(time.Now().Add(time.Hour).UnixMilli()) + `}}`
	h.writeTarget(t, login)
	must(t, h.m.Import(ToolClaude, "main"))
	// An unrelated login is rescued too (a different account).
	_, err := h.m.SaveLoginRescue(ToolClaude, []byte(other), time.Now().Add(time.Second))
	must(t, err)
	must(t, h.m.Remove(ToolClaude, "main", true))
	var left []string
	for _, name := range dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)) {
		data, _ := os.ReadFile(filepath.Join(h.m.Root(), "backups", ToolClaude, name))
		if strings.Contains(string(data), "login-refresh-SECRET") {
			t.Fatalf("a removed profile's refresh token survives in %s", name)
		}
		left = append(left, name)
	}
	var keptOther bool
	for _, name := range left {
		data, _ := os.ReadFile(filepath.Join(h.m.Root(), "backups", ToolClaude, name))
		keptOther = keptOther || strings.Contains(string(data), "other-refresh")
	}
	if !keptOther {
		t.Fatalf("an unrelated rescued login must be kept: %v", left)
	}
}

func TestOldRescuedLoginsAreDeletedWhenANewOneIsSaved(t *testing.T) {
	m, _ := enabledManager(t)
	dir := filepath.Join(m.Root(), "backups", ToolClaude)
	old, err := m.SaveLoginRescue(ToolClaude, []byte("old"), time.Now().Add(-48*time.Hour))
	must(t, err)
	past := time.Now().Add(-48 * time.Hour)
	must(t, os.Chtimes(old, past, past))
	if _, err := m.SaveLoginRescue(ToolClaude, []byte("new"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rescue older than a day must be pruned: %v", dirNames(dir))
	}
}

func TestRenamingTheActiveProfileMakesNoUselessBackupOfATokenOnlyLiveFileUnderTheMarker(t *testing.T) {
	h := newImportHarness(t)
	must(t, h.m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("main-token")), SetActive: true}))
	h.writeTarget(t, `{"claudeAiOauth":{"accessToken":"live-access","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`)
	before := len(dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude)))
	must(t, h.m.Rename(ToolClaude, "main", "primary"))
	if after := len(dirNames(filepath.Join(h.m.Root(), "backups", ToolClaude))); after != before {
		t.Fatalf("a token-only live file is not worth a backup: %d -> %d", before, after)
	}
	if active, _ := h.m.Active(ToolClaude); active != "primary" {
		t.Fatalf("active = %q", active)
	}
	if got := readFile(t, h.m.StoredCredPath(ToolClaude, "primary")); !strings.Contains(got, "main-token") {
		t.Fatal("the store file must move with the profile")
	}
}
