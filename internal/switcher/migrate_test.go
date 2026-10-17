package switcher

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// linkedInstall builds a link-based install: profile "main" active for claude with
// the live file the freshest full credential and the store file a symlink to it,
// plus an inactive profile.
func linkedInstall(t *testing.T, expiry time.Time) (m *Manager, livePath string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	livePath = writeLive(t, home, ToolClaude, fullClaude("tok-live", "ref-live", expiry))
	must(t, m.Import(ToolClaude, "main"))
	must(t, m.Add(ToolClaude, "other"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "other"), []byte(fullClaude("tok-other", "ref-other", expiry)), 0o600))
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture: the active profile should be linked: %v %v", info, err)
	}
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(tool string, full []byte) error {
		stripped := strings.Replace(string(full), `"refreshToken":"ref-live",`, "", 1)
		return os.WriteFile(livePath, []byte(stripped), 0o600)
	}})
	return m, livePath
}

func TestMigrateMovesTheLiveCredentialIntoTheStoreAndStripsLive(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	original := readFile(t, live)
	res, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if err != nil || res.AlreadyMigrated || len(res.Tools) != 1 || res.Tools[0].Status != "migrated" {
		t.Fatalf("Migrate = %+v %v", res, err)
	}
	store := m.profileCredPath(ToolClaude, "main")
	info, err := os.Lstat(store)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("store file is not a regular 0600 file: %v %v", info, err)
	}
	if got := readFile(t, store); got != original {
		t.Fatalf("store lost the full credential: %s", got)
	}
	if got := readFile(t, live); strings.Contains(got, "refreshToken") || !strings.Contains(got, "tok-live") {
		t.Fatalf("live file was not rewritten token-only: %s", got)
	}
	if !m.TokenCopies() {
		t.Fatal("marker not written")
	}
	// The original live credential is in a backup (persist-first).
	found := false
	backups, _ := m.profileBackups(ToolClaude, "main")
	for _, b := range backups {
		if data, rerr := os.ReadFile(b.path); rerr == nil && string(data) == original {
			found = true
		}
	}
	if !found {
		t.Fatal("no backup holds the original live credential")
	}
	// A second run is a no-op.
	again, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if err != nil || !again.AlreadyMigrated {
		t.Fatalf("second run = %+v %v", again, err)
	}
	// And Switch now works in the token-copy model.
	if _, _, err := m.Switch(ToolClaude, "other"); err != nil {
		t.Fatalf("Switch after migration: %v", err)
	}
}

func TestUnmigrateTokenCopiesRestoresLiveAndLink(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatal(err)
	}
	// A worker can add MCP OAuth state to the token-only live file after
	// migration. The restore must carry it into the full live credential.
	current := strings.TrimSuffix(readFile(t, live), "}") + `,"mcpOAuth":{"keep":"new-login"}}`
	must(t, os.WriteFile(live, []byte(current), 0o600))
	writer := &credtarget.Writer{}
	m.SetTokenCopyHooks(TokenCopyHooks{RestoreFull: func(tool string, full []byte) error {
		return writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: live}, full)
	}})
	if err := m.UnmigrateTokenCopies(ToolClaude); err != nil {
		t.Fatal(err)
	}
	if m.TokenCopiesFor(ToolClaude) {
		t.Fatal("marker retained")
	}
	if got := readFile(t, live); !strings.Contains(got, `"refreshToken":"ref-live"`) {
		t.Fatalf("live is not full: %s", got)
	}
	if got := readFile(t, live); !strings.Contains(got, `"mcpOAuth":{"keep":"new-login"}`) {
		t.Fatalf("live lost MCP OAuth: %s", got)
	}
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("store is not linked: %v %v", info, err)
	}
	if err := m.UnmigrateTokenCopies(ToolClaude); err != nil {
		t.Fatalf("idempotence: %v", err)
	}
	if got := readFile(t, m.profileCredPath(ToolClaude, "main")); !strings.Contains(got, `"mcpOAuth":{"keep":"new-login"}`) {
		t.Fatalf("stored link lost MCP OAuth: %s", got)
	}
}

func TestUnmigrateRefusesPendingLogin(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatal(err)
	}
	login := fullClaude("login-access", "login-refresh", time.Now().Add(7*time.Hour))
	must(t, os.WriteFile(live, []byte(login), 0o600))
	m.SetTokenCopyHooks(TokenCopyHooks{RestoreFull: func(string, []byte) error { t.Fatal("restore overwrote login"); return nil }})
	if err := m.UnmigrateTokenCopies(ToolClaude); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("unmigrate = %v", err)
	}
	if !m.TokenCopiesFor(ToolClaude) || readFile(t, live) != login {
		t.Fatal("pending login changed")
	}
}

func TestUnmigrateCodexKeepsOtherFields(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(48 * time.Hour)
	jwt := makeTestJWT("fake@example.com", expiry.Unix())
	live := writeLive(t, home, ToolCodex, `{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"`+jwt+`","refresh_token":"codex-refresh","account_id":"acct"},"OPENAI_API_KEY":"keep"}`)
	must(t, m.Import(ToolCodex, "main"))
	writer := &credtarget.Writer{}
	m.SetTokenCopyHooks(TokenCopyHooks{
		FanOutActive: func(tool string, full []byte) error {
			return writer.WriteTarget(credtarget.Target{Tool: tool, Path: live}, full)
		},
		ReplaceActive: func(tool string, full []byte) error {
			return writer.WriteImportedLogin(credtarget.Target{Tool: tool, Path: live}, full)
		},
		RestoreFull: func(tool string, full []byte) error {
			return writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: live}, full)
		},
	})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolCodex}, RunningCodex: func() ([]string, string, error) { return nil, "test", nil }}); err != nil {
		t.Fatal(err)
	}
	if err := m.UnmigrateTokenCopies(ToolCodex); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, live)
	if !strings.Contains(data, `"refresh_token":"codex-refresh"`) || !strings.Contains(data, `"OPENAI_API_KEY":"keep"`) {
		t.Fatalf("Codex live credential not restored: %s", data)
	}
	if m.TokenCopiesFor(ToolCodex) {
		t.Fatal("Codex marker retained")
	}
	if info, err := os.Lstat(m.profileCredPath(ToolCodex, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Codex store is not linked: %v %v", info, err)
	}
}

// C2: inside the last 15 minutes a CLI mid-refresh could spend the very token
// being moved, so the migration refuses and changes nothing.
func TestMigrateRefusesInsideTheFifteenMinuteWindow(t *testing.T) {
	now := time.Now()
	m, live := linkedInstall(t, now.Add(10*time.Minute))
	liveBefore := readFile(t, live)
	storeBefore, _ := os.Readlink(m.profileCredPath(ToolClaude, "main"))
	_, err := m.MigrateToTokenCopies(MigrateOptions{Now: func() time.Time { return now }})
	if !errors.Is(err, credtarget.ErrMigrateTokenExpiring) {
		t.Fatalf("Migrate = %v, want ErrMigrateTokenExpiring", err)
	}
	if !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("message = %v", err)
	}
	if readFile(t, live) != liveBefore {
		t.Fatal("live file changed on refusal")
	}
	if target, _ := os.Readlink(m.profileCredPath(ToolClaude, "main")); target != storeBefore || target == "" {
		t.Fatalf("store link changed on refusal: %q", target)
	}
	if m.TokenCopies() {
		t.Fatal("marker written on refusal")
	}
	// Exactly at the boundary the guard still refuses; just outside it passes.
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Now: func() time.Time { return now.Add(-6 * time.Minute) }}); err != nil {
		t.Fatalf("outside the window: %v", err)
	}
}

// One tool's refusal blocks the whole migration: nothing is half-converted.
func TestMigrateIsAllOrNothingAcrossTools(t *testing.T) {
	now := time.Now()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	claudeLive := writeLive(t, home, ToolClaude, fullClaude("tok", "ref-live", now.Add(8*time.Hour)))
	must(t, m.Import(ToolClaude, "main"))
	soon := makeTestJWT("soon@example.com", now.Add(5*time.Minute).Unix())
	codexLive := writeLive(t, home, ToolCodex, `{"tokens":{"access_token":"`+soon+`","refresh_token":"refresh","account_id":"acct"}}`)
	must(t, m.Import(ToolCodex, "cx"))
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(string, []byte) error { t.Fatal("fan-out ran"); return nil }})
	claudeBefore, codexBefore := readFile(t, claudeLive), readFile(t, codexLive)
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Now: func() time.Time { return now }}); !errors.Is(err, credtarget.ErrMigrateTokenExpiring) {
		t.Fatalf("Migrate = %v", err)
	}
	if readFile(t, claudeLive) != claudeBefore || readFile(t, codexLive) != codexBefore || m.TokenCopies() {
		t.Fatal("a refused migration changed something")
	}
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("claude store link was replaced although codex refused")
	}
}

func TestMigrateResumesAnInterruptedRunAndHandlesAbsentLive(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	// Interrupted run: the store is a regular full file, the live file already token-only.
	full := readFile(t, live)
	store := m.profileCredPath(ToolClaude, "main")
	must(t, os.Remove(store))
	must(t, os.WriteFile(store, []byte(full), 0o600))
	must(t, os.WriteFile(live, []byte(strings.Replace(full, `"refreshToken":"ref-live",`, "", 1)), 0o600))
	res, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if err != nil || res.Tools[0].Status != "migrated" || !m.TokenCopies() {
		t.Fatalf("resume = %+v %v", res, err)
	}
	if readFile(t, store) != full {
		t.Fatal("the store credential changed")
	}

	// A token-only live file with no full credential in the store cannot be migrated.
	m2, live2 := linkedInstall(t, time.Now().Add(6*time.Hour))
	must(t, os.Remove(m2.profileCredPath(ToolClaude, "main")))
	must(t, os.WriteFile(m2.profileCredPath(ToolClaude, "main"), []byte(tokenOnlyClaude("tok", time.Now().Add(6*time.Hour))), 0o600))
	must(t, os.WriteFile(live2, []byte(tokenOnlyClaude("tok", time.Now().Add(6*time.Hour))), 0o600))
	if _, err := m2.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err == nil || !strings.Contains(err.Error(), "not a full credential") {
		t.Fatalf("token-only everywhere = %v", err)
	}

	// No live file: skipped, a dangling store link is dropped, and the marker is written.
	m3, live3 := linkedInstall(t, time.Now().Add(6*time.Hour))
	must(t, os.Remove(live3))
	res, err = m3.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if err != nil || !strings.HasPrefix(res.Tools[0].Status, "skipped") {
		t.Fatalf("absent live = %+v %v", res, err)
	}
	if _, err := os.Lstat(m3.profileCredPath(ToolClaude, "main")); !os.IsNotExist(err) {
		t.Fatalf("dangling store link kept: %v", err)
	}
}

func TestMigrateRefusesWhenDisabledUnwiredOrCorrupt(t *testing.T) {
	m, home := newTestManager(t)
	if _, err := m.MigrateToTokenCopies(MigrateOptions{}); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("unwired = %v", err)
	}
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(string, []byte) error { return nil }})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{}); !errors.Is(err, ErrMigrateNotEnabled) {
		t.Fatalf("disabled = %v", err)
	}
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, fullClaude("tok", "ref", time.Now().Add(6*time.Hour)))
	must(t, m.Import(ToolClaude, "main"))
	live := writeLive(t, home, ToolClaude, "{corrupt")
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err == nil {
		t.Fatalf("a corrupt live file was migrated (%s)", live)
	}
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{"pi"}}); err == nil {
		t.Fatal("an unknown tool was accepted")
	}
}

// A refused migration promises that nothing changed: the dangling link of a tool
// with no live file is only removed once every tool has passed its guard.
func TestMigrateRefusalKeepsADanglingLinkOfAnotherTool(t *testing.T) {
	now := time.Now()
	m, claudeLive := linkedInstall(t, now.Add(6*time.Hour))
	home, err := m.ResolveHome()
	must(t, err)
	soon := makeTestJWT("soon@example.com", now.Add(5*time.Minute).Unix())
	writeLive(t, home, ToolCodex, `{"tokens":{"access_token":"`+soon+`","refresh_token":"refresh","account_id":"acct"}}`)
	must(t, m.Import(ToolCodex, "cx"))
	// claude has no live file any more: its active profile is a dangling link.
	must(t, os.Remove(claudeLive))
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(string, []byte) error { t.Fatal("fan-out ran"); return nil }})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Now: func() time.Time { return now }}); !errors.Is(err, credtarget.ErrMigrateTokenExpiring) {
		t.Fatalf("Migrate = %v", err)
	}
	if _, err := os.Lstat(m.profileCredPath(ToolClaude, "main")); err != nil {
		t.Fatalf("the dangling claude link was removed although codex refused: %v", err)
	}
}

func codexLinkedInstall(t *testing.T) *Manager {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	good := makeTestJWT("cx@example.com", time.Now().Add(6*time.Hour).Unix())
	writeLive(t, home, ToolCodex, `{"tokens":{"access_token":"`+good+`","refresh_token":"refresh","account_id":"acct"}}`)
	must(t, m.Import(ToolCodex, "cx"))
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(string, []byte) error { return nil }})
	return m
}

// A running codex caches its refresh token: migrating under it would let the
// session and the server spend the same token.
func TestMigrateRefusesWhileCodexRuns(t *testing.T) {
	m := codexLinkedInstall(t)
	running := func() ([]string, string, error) {
		return []string{"pid 4242", "container abc: 7"}, "host processes; 2 running docker container(s)", nil
	}
	_, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolCodex}, RunningCodex: running})
	want := "codex is running (pid 4242, container abc: 7): a running codex caches its refresh token and would spend it again; end those sessions and re-run, or pass --allow-running-codex; checked: host processes; 2 running docker container(s); nothing was changed"
	var runningErr *CodexRunningError
	if !errors.Is(err, ErrCodexRunning) || !errors.As(err, &runningErr) || len(runningErr.Found) != 2 || err.Error() != want {
		t.Fatalf("Migrate = %v, want ErrCodexRunning naming the pids", err)
	}
	if m.TokenCopies() {
		t.Fatal("a refused migration wrote the marker")
	}
	if info, lerr := os.Lstat(m.profileCredPath(ToolCodex, "cx")); lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the store link was replaced although the migration was refused")
	}
	// The override migrates anyway; claude alone never consults the process table.
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolCodex}, RunningCodex: running, AllowRunningCodex: true}); err != nil {
		t.Fatalf("Migrate with the override: %v", err)
	}
}

func TestMigrateClaudeOnlyIgnoresRunningCodex(t *testing.T) {
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	boom := func() ([]string, string, error) { t.Fatal("process table consulted for claude"); return nil, "", nil }
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}, RunningCodex: boom}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
}

func TestParseCodexPIDsMatchesTheExecutableNameOnly(t *testing.T) {
	out := "  101 /usr/local/bin/codex\n  102 codex\n  103 /usr/bin/vim\n  104 codexish\n  200 codex\n   x bad\n"
	got := parseCodexPIDs(out, 200)
	if len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Fatalf("pids = %v, want [101 102] (self and look-alikes excluded)", got)
	}
}

// F2: migrating one tool must not mark the other one migrated.
func TestMigrateIsPerTool(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	live := writeLive(t, home, ToolClaude, fullClaude("tok-live", "ref-live", time.Now().Add(6*time.Hour)))
	must(t, m.Import(ToolClaude, "main"))
	good := makeTestJWT("cx@example.com", time.Now().Add(6*time.Hour).Unix())
	writeLive(t, home, ToolCodex, `{"tokens":{"access_token":"`+good+`","refresh_token":"refresh","account_id":"acct"}}`)
	must(t, m.Import(ToolCodex, "cx"))
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(tool string, full []byte) error {
		if tool != ToolClaude {
			t.Fatalf("fan-out for %s", tool)
		}
		return os.WriteFile(live, []byte(strings.Replace(string(full), `"refreshToken":"ref-live",`, "", 1)), 0o600)
	}})

	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("claude migration: %v", err)
	}
	if !m.TokenCopiesFor(ToolClaude) || m.TokenCopiesFor(ToolCodex) || !m.TokenCopies() {
		t.Fatalf("marker after claude only: claude=%v codex=%v any=%v", m.TokenCopiesFor(ToolClaude), m.TokenCopiesFor(ToolCodex), m.TokenCopies())
	}
	if info, err := os.Lstat(m.profileCredPath(ToolCodex, "cx")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("codex lost its link although only claude was migrated")
	}
	// The unmigrated tool keeps the link model: its links are not torn down and
	// EnsureActiveLinks leaves the migrated one alone.
	must(t, m.EnsureActiveLinks())
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("EnsureActiveLinks relinked the migrated claude profile")
	}
	again, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if err != nil || !again.AlreadyMigrated {
		t.Fatalf("second claude run = %+v %v", again, err)
	}

	// Codex can still be migrated afterwards (this used to return AlreadyMigrated).
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(string, []byte) error { return nil }})
	res, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude, ToolCodex}})
	if err != nil || res.AlreadyMigrated {
		t.Fatalf("codex migration = %+v %v", res, err)
	}
	if !m.TokenCopiesFor(ToolCodex) {
		t.Fatal("codex not marked after its own migration")
	}
	if info, err := os.Lstat(m.profileCredPath(ToolCodex, "cx")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("codex store file is still a link")
	}
	if _, ok := m.TokenCopiesSince(ToolClaude); !ok {
		t.Fatal("claude lost its migration time")
	}
}

// A version-1 marker (written before the marker became per tool) means both.
func TestTokenCopiesMarkerVersionOneCoversBothTools(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, os.MkdirAll(m.root, 0o700))
	must(t, os.WriteFile(m.tokenCopiesPath(), []byte(`{"version":1,"migrated_at":"2026-10-16T10:00:00Z"}`), 0o600))
	if !m.TokenCopiesFor(ToolClaude) || !m.TokenCopiesFor(ToolCodex) || !m.TokenCopies() {
		t.Fatal("a v1 marker must cover both tools")
	}
	if at, _ := m.TokenCopiesSince(ToolCodex); at.Hour() != 10 {
		t.Fatalf("since = %v", at)
	}
}

// Migration takes the account lock BEFORE the switcher flock, the order Switch and
// the refresher use. While another holder has the account, migrate waits without
// holding the flock, so a Switch (flock taker) is never blocked behind it and the
// two cannot deadlock.
func TestMigrateWaitsForTheAccountLockWithoutHoldingTheSwitcherFlock(t *testing.T) {
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	release, err := usages.HoldMaintenance(m.root, ToolClaude, "main", m.profileCredPath(ToolClaude, "main"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, merr := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
		done <- merr
	}()
	time.Sleep(300 * time.Millisecond) // migrate is now waiting on the account lock
	lock, lerr := m.acquireLock()
	if lerr != nil {
		t.Fatalf("flock unavailable while migrate waits for the account: %v", lerr)
	}
	_ = lock.unlock()
	release()
	select {
	case merr := <-done:
		if merr != nil {
			t.Fatalf("Migrate = %v", merr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("migrate did not finish after the account lock was released (deadlock)")
	}
	if !m.TokenCopiesFor(ToolClaude) {
		t.Fatal("marker not written")
	}
}

// A Switch racing a migrate must not deadlock; both end consistent.
func TestMigrateAndSwitchDoNotDeadlock(t *testing.T) {
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	done := make(chan string, 2)
	go func() {
		_, merr := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
		done <- fmt.Sprint("migrate:", merr)
	}()
	go func() {
		_, _, serr := m.Switch(ToolClaude, "other")
		done <- fmt.Sprint("switch:", serr)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("migrate and Switch deadlocked")
		}
	}
}
