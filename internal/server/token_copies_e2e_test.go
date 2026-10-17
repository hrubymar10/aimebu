package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// End-to-end scenarios for the token-only credential model, through the real
// switcher Manager, credtarget Registry/Writer/FanOut/Reconciler and usages
// TargetRefresher, wired as server.go and startTokenCopies wire them, with a
// fake CLI in place of docker. Two invariants are checked after every step: no
// refresh token is ever handed to the CLI twice, and a live file never holds a
// refresh token once its tool is migrated and a pass has run.
//
// Tests named for a finding (NEW-1, F3, C1, ...) are regression cases for that
// open finding and fail until it is fixed. Credentials are fake and live under
// t.TempDir(); HOME is pointed there first.

var (
	claudeRefreshInFile = regexp.MustCompile(`"refreshToken"\s*:\s*"([^"]+)"`)
	codexRefreshInFile  = regexp.MustCompile(`"refresh_token"\s*:\s*"([^"]+)"`)
	codexAccessInFile   = regexp.MustCompile(`"access_token"\s*:\s*"([^"]+)"`)
)

func fakeJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesig"
}

func fullCodexCreds(tag, refresh string, expires, lastRefresh time.Time) string {
	access := fakeJWT(map[string]any{"email": "codex@example.invalid", "exp": expires.Unix(), "jti": tag})
	return fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":%q},"last_refresh":%q}`,
		access, refresh, access, lastRefresh.UTC().Format(time.RFC3339))
}

// fakeCLI stands in for `claude -p` / `codex` run against a throwaway credential
// directory: it records which refresh token it was handed, then writes the
// rotated credential the real CLI would.
type fakeCLI struct {
	mu     sync.Mutex
	spent  map[string]int
	serial int
	// onSpend runs after the call is counted; it may call runtime.Goexit to model
	// the process dying with the token in flight, or break the commit target.
	onSpend func(token string)
}

func (f *fakeCLI) Refresh(_ context.Context, dir, _ string) error {
	codex := false
	path := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		codex = true
		path = filepath.Join(dir, "auth.json")
		if data, err = os.ReadFile(path); err != nil {
			return err
		}
	}
	re := claudeRefreshInFile
	if codex {
		re = codexRefreshInFile
	}
	m := re.FindSubmatch(data)
	if m == nil {
		return fmt.Errorf("fake cli: no refresh token in the temp copy")
	}
	token := string(m[1])
	f.mu.Lock()
	if f.spent == nil {
		f.spent = map[string]int{}
	}
	f.spent[token]++
	f.serial++
	n := f.serial
	hook := f.onSpend
	f.mu.Unlock()
	if hook != nil {
		hook(token)
	}
	var rotated string
	if codex {
		rotated = fullCodexCreds(fmt.Sprintf("acc-r%d", n), fmt.Sprintf("ref-r%d", n), time.Now().Add(240*time.Hour), time.Now())
	} else {
		rotated = fullClaudeCreds(fmt.Sprintf("acc-r%d", n), fmt.Sprintf("ref-r%d", n), time.Now().Add(8*time.Hour))
	}
	return os.WriteFile(path, []byte(rotated), 0o600)
}

func (f *fakeCLI) assertNothingSpentTwice(t *testing.T, step string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for token, n := range f.spent {
		if n > 1 {
			t.Errorf("%s: refresh token %s was spent %d times", step, token, n)
		}
	}
}

func (f *fakeCLI) spentCount(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spent[token]
}

func (f *fakeCLI) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.spent {
		total += n
	}
	return total
}

func (f *fakeCLI) setOnSpend(fn func(token string)) {
	f.mu.Lock()
	f.onSpend = fn
	f.mu.Unlock()
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type e2eEnv struct {
	t          *testing.T
	m          *switcher.Manager
	home       string
	swRoot     string
	liveClaude string
	liveCodex  string
	um         *usages.Manager
	usageStore *usages.Store
	fan        *credtarget.FanOut
	rec        *credtarget.Reconciler
	writer     *credtarget.Writer
	cli        *fakeCLI
	tr         *usages.TargetRefresher
	// dieOnCommit makes the next rotated-credential commit end the calling
	// goroutine before it takes the switcher lock: death after the persist-first
	// rescue file and the "rotated" journal entry, before the store is written.
	dieOnCommit atomic.Bool

	mu     sync.Mutex
	alerts []string
}

// newE2EEnv builds the wiring of startTokenCopies over a temp HOME. The install
// starts unmigrated; the refresher and reconciler are gated on the marker.
func newE2EEnv(t *testing.T) *e2eEnv {
	m, home := newSwitcherTestEnv(t)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(home, ".aimebu"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	e := &e2eEnv{t: t, m: m, home: home, swRoot: m.Root(), cli: &fakeCLI{},
		liveClaude: filepath.Join(home, ".claude", ".credentials.json"),
		liveCodex:  filepath.Join(home, ".codex", "auth.json")}
	for _, p := range []string{e.liveClaude, e.liveCodex} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.usageStore = usages.NewStoreAt(t.TempDir())
	e.um = usages.NewManager(e.usageStore, usages.EmptyRegistry())
	sr := switcherRoutes{sm: m}
	e.um.SetProfileLister(func(tool string) ([]usages.ProfileInfo, error) {
		profiles, err := m.List()
		if err != nil {
			return nil, err
		}
		var out []usages.ProfileInfo
		for _, p := range profiles {
			if p.Tool != tool {
				continue
			}
			out = append(out, usages.ProfileInfo{Tool: p.Tool, Name: p.Name, Active: p.Active, CredPath: sr.profileCredPath(p), Email: p.Email, HasCredentials: p.HasCreds, ExpiresAt: p.ExpiresAt, SwitchedAwayAt: p.SwitchedAwayAt})
		}
		return out, nil
	})
	e.um.EnableClaudeAutoRefresh(func(fn func() error) error {
		if e.dieOnCommit.CompareAndSwap(true, false) {
			runtime.Goexit()
		}
		return m.WithLock(fn)
	}, e.swRoot)
	reg := credtarget.NewRegistry(t.TempDir(), home)
	reg.LivePath = m.LiveCredPath
	writer := &credtarget.Writer{}
	e.fan = &credtarget.FanOut{Registry: reg, Writer: writer, Active: m.Active, WithLock: m.WithLock}
	e.um.SetTargetFanOut(e.fan)
	e.tr = e.newRefresher()
	e.um.SetTargetRefresher(e.tr)
	e.writer = writer
	m.SetTokenCopyHooks(e.hooks(e.tr))
	e.rec = &credtarget.Reconciler{Registry: reg, Writer: writer, FanOut: e.fan, Store: switcher.NewCredStore(m), Home: home, Ready: m.TokenCopies, ReadyTool: m.TokenCopiesFor, ImportSettle: -1}
	e.tr.SetReconcile(e.rec.Pass)
	return e
}

// newRefresher builds a TargetRefresher over the same root, as a restarted server
// would. The first one is the live one; callers rewire what they need.
func (e *e2eEnv) newRefresher() *usages.TargetRefresher {
	tr := usages.NewTargetRefresher(e.um)
	tr.SetExecutor("claude", e.cli)
	tr.SetExecutor("codex", e.cli)
	tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	tr.SetEnabled(e.m.TokenCopies)
	tr.SetToolEnabled(e.m.TokenCopiesFor)
	tr.SetPendingLogin(func(tool string) bool { // as startTokenCopies wires it
		path, err := e.m.LiveCredPath(tool)
		if err != nil {
			return true
		}
		data, err := os.ReadFile(path)
		return (err != nil && !os.IsNotExist(err)) || (err == nil && credtarget.HasRefreshToken(tool, data))
	})
	tr.SetAlertHook(func(kind, detail string) {
		e.mu.Lock()
		e.alerts = append(e.alerts, kind+": "+detail)
		e.mu.Unlock()
	})
	return tr
}

// restart swaps in a fresh refresher (empty in-memory state), as a restarted
// server would have.
func (e *e2eEnv) restart() *usages.TargetRefresher {
	tr := e.newRefresher()
	e.um.SetTargetRefresher(tr)
	tr.SetReconcile(e.rec.Pass)
	e.m.SetTokenCopyHooks(e.hooks(tr))
	e.tr = tr
	return tr
}

// hooks are the switcher hooks the server installs (startTokenCopies): fan-out,
// replace, clear, the refresher's incoming-profile refresh and the full-credential
// restore the un-migration uses.
func (e *e2eEnv) hooks(tr *usages.TargetRefresher) switcher.TokenCopyHooks {
	return switcher.TokenCopyHooks{
		FanOutActive:    e.fan.FanOutActive,
		ReplaceActive:   e.fan.FanOutActiveReplacing,
		ClearActive:     e.fan.ClearActive,
		RefreshIncoming: tr.RefreshIncoming,
		RestoreFull: func(tool string, full []byte) error {
			path, err := e.m.LiveCredPath(tool)
			if err != nil {
				return err
			}
			return e.writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: path}, full)
		},
	}
}

func (e *e2eEnv) setLive(tool, content string) {
	e.t.Helper()
	path := e.liveClaude
	if tool == switcher.ToolCodex {
		path = e.liveCodex
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2eEnv) migrate(tools ...string) {
	e.t.Helper()
	opts := switcher.MigrateOptions{Tools: tools, RunningCodex: func() ([]string, string, error) { return nil, "no codex running (test)", nil }}
	if _, err := e.m.MigrateToTokenCopies(opts); err != nil {
		e.t.Fatalf("migrate %v: %v", tools, err)
	}
}

func (e *e2eEnv) stored(tool, name string) string {
	e.t.Helper()
	return readFileString(e.t, e.m.StoredCredPath(tool, name))
}

func (e *e2eEnv) writeStored(tool, name, content string) {
	e.t.Helper()
	if err := os.WriteFile(e.m.StoredCredPath(tool, name), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2eEnv) assertLiveTokenOnly(step string) {
	e.t.Helper()
	if data, err := os.ReadFile(e.liveClaude); err == nil && strings.Contains(string(data), "refreshToken") {
		e.t.Errorf("%s: the live claude file holds a refresh token: %s", step, data)
	}
	if data, err := os.ReadFile(e.liveCodex); err == nil && codexRefreshInFile.Match(data) {
		e.t.Errorf("%s: the live codex file holds a refresh token: %s", step, data)
	}
}

func (e *e2eEnv) step(name string) {
	e.t.Helper()
	e.cli.assertNothingSpentTwice(e.t, name)
}

// ageMarker moves every timestamp in the migration marker back by d, as if the
// migration had run that long ago (the Codex quiet period is measured from it).
func ageMarker(t *testing.T, swRoot string, d time.Duration) {
	t.Helper()
	ageTimestamps(t, filepath.Join(swRoot, "token-copies.json"), d, true)
}

func ageTimestamps(t *testing.T, path string, d time.Duration, required bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var shift func(v any) any
	shift = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				x[k] = shift(val)
			}
		case string:
			if at, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return at.Add(-d).UTC().Format(time.RFC3339Nano)
			}
		}
		return v
	}
	out, err := json.Marshal(shift(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func stateFile(e *e2eEnv) string {
	return readFileString(e.t, filepath.Join(e.swRoot, "refresh-state.json"))
}

func TestTokenCopiesEndToEnd(t *testing.T) {
	e := newE2EEnv(t)
	m, cli, ctx := e.m, e.cli, context.Background()
	soon := time.Now().Add(30 * time.Minute) // inside the 60 min claude lead: due
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B0", "ref-B0", time.Now().Add(5*time.Hour)))

	// 1. Unmigrated: the refresher is off and nothing is spent, whatever is due.
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("unmigrated RefreshDue: %v", err)
	}
	if n := cli.totalCalls(); n != 0 {
		t.Fatalf("unmigrated install: the CLI ran %d times", n)
	}
	if !strings.Contains(readFileString(t, e.liveClaude), "ref-A0") {
		t.Fatal("unmigrated install: the live file was touched")
	}

	// 2. migrate-copies: profiles become regular files, live goes token-only.
	e.migrate(switcher.ToolClaude)
	e.assertLiveTokenOnly("after migrate")
	if !strings.Contains(e.stored(switcher.ToolClaude, "A"), "ref-A0") {
		t.Fatal("after migrate: profile A lost its full credential")
	}
	e.step("migrate")

	// 3. A is due: refreshed once, committed, fanned out token-only.
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if n := cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("ref-A0 spent %d times, want 1", n)
	}
	storedA := e.stored(switcher.ToolClaude, "A")
	if strings.Contains(storedA, "ref-A0") || !strings.Contains(storedA, "ref-r1") {
		t.Fatalf("A's stored credential was not rotated: %s", storedA)
	}
	if got := readFileString(t, e.liveClaude); !strings.Contains(got, "acc-r1") {
		t.Fatalf("the live file did not follow the refresh: %s", got)
	}
	e.assertLiveTokenOnly("after refresh")
	if err := e.tr.RefreshDue(ctx); err != nil || cli.totalCalls() != 1 {
		t.Fatalf("a second pass spent again: %v, calls=%d", err, cli.totalCalls())
	}
	e.step("refresh A")

	// 4. A /login lands in the live file while A is due again: the refresh must not
	// fan out over it (a login holds a refresh token), the reconciler then files
	// and strips it.
	e.writeStored(switcher.ToolClaude, "A", fullClaudeCreds("acc-A2", "ref-A2", soon))
	loginCred := fullClaudeCreds("acc-L1", "ref-L1", time.Now().Add(8*time.Hour))
	e.setLive(switcher.ToolClaude, loginCred)
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue with a login waiting: %v", err)
	}
	// With a login in the live file the refresher stands down entirely (the exposed
	// refresh token is not its to spend), whatever is due.
	if n := cli.spentCount("ref-A2"); n != 0 {
		t.Fatalf("due A was refreshed %d times while a login waited in the live file, want 0", n)
	}
	if got := readFileString(t, e.liveClaude); got != loginCred {
		t.Fatalf("a refresh fan-out overwrote the login waiting in the live file: %s", got)
	}
	if err := e.rec.Pass(ctx); err != nil {
		t.Fatalf("reconciler pass: %v", err)
	}
	e.assertLiveTokenOnly("after login import")
	if !strings.Contains(readFileString(t, e.liveClaude), "acc-L1") {
		t.Fatalf("the stripped live file lost the login's access token: %s", readFileString(t, e.liveClaude))
	}
	loginProfile, err := m.Active(switcher.ToolClaude)
	if err != nil || loginProfile == "A" || loginProfile == "" {
		t.Fatalf("active after /login = %q (%v), want the new login profile", loginProfile, err)
	}
	if got := e.stored(switcher.ToolClaude, loginProfile); !strings.Contains(got, "ref-L1") {
		t.Fatalf("the login profile does not hold the full login: %s", got)
	}
	e.step("login import")

	// 5. Switch to B, which is due: B is refreshed BEFORE it goes live.
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B1", "ref-B1", soon))
	if _, _, err := m.Switch(switcher.ToolClaude, "B"); err != nil {
		t.Fatalf("Switch to a due B: %v", err)
	}
	if n := cli.spentCount("ref-B1"); n != 1 {
		t.Fatalf("incoming B was refreshed %d times, want 1", n)
	}
	e.assertLiveTokenOnly("after switch")
	if got := readFileString(t, e.liveClaude); strings.Contains(got, "acc-B1") || strings.Contains(got, "acc-L1") {
		t.Fatalf("the live file holds a pre-refresh or the previous account's access token: %s", got)
	}
	if active, _ := m.Active(switcher.ToolClaude); active != "B" {
		t.Fatalf("active = %q, want B", active)
	}
	e.step("switch")

	// 6. Process death mid-refresh: B (active) is due; the CLI is handed its token
	// and the process dies before anything is committed.
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B2", "ref-B2", soon))
	cli.setOnSpend(func(token string) {
		if token == "ref-B2" {
			runtime.Goexit()
		}
	})
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_ = e.tr.RefreshDue(ctx)
	}()
	select {
	case <-dead:
	case <-time.After(30 * time.Second):
		t.Fatal("the interrupted refresh never ended")
	}
	if n := cli.spentCount("ref-B2"); n != 1 {
		t.Fatalf("ref-B2 handed to the CLI %d times, want 1", n)
	}
	// Any in-flight phase counts ("spending", or "started" once the CLI exists).
	if !strings.Contains(stateFile(e), `"phase"`) {
		t.Fatalf("no journal entry survived the death: %s", stateFile(e))
	}
	e.assertLiveTokenOnly("after the kill")

	// 7. Restart: a new refresher over the same root settles the journal and never
	// spends ref-B2 again.
	cli.setOnSpend(nil)
	tr2 := e.restart()
	e.runStartup(tr2, `"phase"`)
	if n := cli.spentCount("ref-B2"); n != 1 {
		t.Fatalf("ref-B2 spent %d times across the restart, want 1", n)
	}
	e.assertLiveTokenOnly("after restart")
	e.step("restart")
}

// runStartup runs tr.Run (recover, reconcile, due pass) until the journal no
// longer contains marker, then stops it.
func (e *e2eEnv) runStartup(tr *usages.TargetRefresher, marker string) {
	e.t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { defer close(ran); tr.Run(runCtx) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && strings.Contains(stateFile(e), marker) {
		time.Sleep(20 * time.Millisecond)
	}
	// One more beat for the due pass that follows recovery.
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-ran
	if strings.Contains(stateFile(e), marker) {
		e.t.Fatalf("the restarted refresher did not settle %s in the journal", marker)
	}
}

// Codex through the same model: the quiet period after migration is honoured,
// then the profile is refreshed exactly once and the live file stays token-only.
func TestTokenCopiesCodexQuietPeriodThenRefresh(t *testing.T) {
	e := newE2EEnv(t)
	ctx := context.Background()
	due := time.Now().Add(2 * time.Hour) // inside the 24 h codex lead
	e.setLive(switcher.ToolCodex, fullCodexCreds("cx0", "ref-cx0", due, time.Now().Add(-time.Hour)))
	if err := e.m.Import(switcher.ToolCodex, "cx"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolCodex)
	e.assertLiveTokenOnly("after codex migrate")
	if !strings.Contains(e.stored(switcher.ToolCodex, "cx"), "ref-cx0") {
		t.Fatal("codex profile lost its full credential")
	}

	// Just migrated: the due pass leaves codex alone.
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.cli.totalCalls(); n != 0 {
		t.Fatalf("codex refreshed %d times inside the quiet period", n)
	}

	// Past the quiet period it is refreshed once.
	ageMarker(t, e.swRoot, 11*time.Minute)
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if n := e.cli.spentCount("ref-cx0"); n != 1 {
		t.Fatalf("ref-cx0 spent %d times, want 1", n)
	}
	stored := e.stored(switcher.ToolCodex, "cx")
	if !strings.Contains(stored, "ref-r1") {
		t.Fatalf("the codex profile was not rotated: %s", stored)
	}
	access := codexAccessInFile.FindStringSubmatch(stored)
	if access == nil || !strings.Contains(readFileString(t, e.liveCodex), access[1]) {
		t.Fatalf("the live codex file did not follow the refresh: %s", readFileString(t, e.liveCodex))
	}
	e.assertLiveTokenOnly("after codex refresh")
	if err := e.tr.RefreshDue(ctx); err != nil || e.cli.totalCalls() != 1 {
		t.Fatalf("a second pass spent again: %v, calls=%d", err, e.cli.totalCalls())
	}
	e.step("codex")
}

// A process death after the CLI rotated and the new credential was saved to its
// rescue file, but before the store was written: recovery commits the rescued
// credential and fans it out, and the spent token is never handed to the CLI again.
func TestTokenCopiesRotatedPhaseRecoveryCommitsRescue(t *testing.T) {
	e := newE2EEnv(t)
	soon := time.Now().Add(30 * time.Minute)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)

	e.dieOnCommit.Store(true)
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_ = e.tr.RefreshDue(context.Background())
	}()
	select {
	case <-dead:
	case <-time.After(30 * time.Second):
		t.Fatal("the interrupted commit never ended")
	}
	if e.dieOnCommit.Load() {
		t.Fatal("fixture: the commit never ran")
	}
	if !strings.Contains(stateFile(e), `"rotated"`) {
		t.Fatalf("no rotated journal entry survived: %s", stateFile(e))
	}
	if !strings.Contains(e.stored(switcher.ToolClaude, "A"), "ref-A0") {
		t.Fatal("fixture: the store was written before the death")
	}
	rescues, _ := filepath.Glob(filepath.Join(e.swRoot, "backups", "claude", "*"))
	if len(rescues) == 0 {
		t.Fatal("the rotated credential was not persisted before the commit")
	}

	tr2 := e.restart()
	e.runStartup(tr2, `"rotated"`)
	stored := e.stored(switcher.ToolClaude, "A")
	if strings.Contains(stored, "ref-A0") || !strings.Contains(stored, "ref-r1") {
		t.Fatalf("recovery did not commit the rescued credential: %s", stored)
	}
	if n := e.cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("ref-A0 spent %d times across the restart, want 1", n)
	}
	if got := readFileString(t, e.liveClaude); !strings.Contains(got, "acc-r1") {
		t.Fatalf("the live file did not follow the recovered credential: %s", got)
	}
	e.assertLiveTokenOnly("after recovery")
	e.step("rotated recovery")
}

// NEW-2: removing the ACTIVE profile of a migrated install must tombstone the
// refresh token held by the profile store (the live file is token-only, so there
// is nothing to read there), or a late rotation of that account could resurrect it.
func TestTokenCopiesRemoveActiveProfileTombstonesStoredToken(t *testing.T) {
	e := newE2EEnv(t)
	expiry := time.Now().Add(6 * time.Hour)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", expiry))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B0", "ref-B0", expiry))
	e.migrate(switcher.ToolClaude)
	if err := e.m.Remove(switcher.ToolClaude, "A", true); err != nil {
		t.Fatalf("Remove active A: %v", err)
	}
	removed, err := usages.WasRefreshTokenRemoved(e.swRoot, switcher.ToolClaude, "ref-A0")
	if err != nil || !removed {
		t.Fatalf("NEW-2: the removed active profile's stored refresh token was not tombstoned (removed=%v, err=%v)", removed, err)
	}
	if removed, _ := usages.WasRefreshTokenRemoved(e.swRoot, switcher.ToolClaude, "ref-B0"); removed {
		t.Fatal("the tombstone covers a profile that was not removed")
	}
}

// C1: the CLI spent the token and the commit to the store failed (here: the
// profile's directory stops accepting writes). The stored token is now spent, so
// no later tick, and no restart, may hand it to the CLI again; the rotated
// credential must still be on disk in a rescue file.
func TestTokenCopiesFailedStoreCommitNeverSpendsTwice(t *testing.T) {
	e := newE2EEnv(t)
	ctx := context.Background()
	soon := time.Now().Add(30 * time.Minute)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)

	dir := filepath.Dir(e.m.StoredCredPath(switcher.ToolClaude, "A"))
	e.cli.setOnSpend(func(token string) {
		if token == "ref-A0" {
			_ = os.Chmod(dir, 0o500) // the atomic write of the store file now fails
		}
	})
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_ = e.tr.RefreshDue(ctx)
	_ = os.Chmod(dir, 0o700)
	e.cli.setOnSpend(nil)
	if n := e.cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("fixture: ref-A0 handed to the CLI %d times, want 1", n)
	}
	if !strings.Contains(e.stored(switcher.ToolClaude, "A"), "ref-A0") {
		t.Fatal("fixture: the failed commit still wrote the store")
	}
	rescues, _ := filepath.Glob(filepath.Join(e.swRoot, "backups", "claude", "*"))
	rescued := false
	for _, path := range rescues {
		if strings.Contains(readFileString(t, path), "ref-r1") {
			rescued = true
		}
	}
	if !rescued {
		t.Fatal("C1: the rotated credential was not kept in a rescue file")
	}

	_ = e.tr.RefreshDue(ctx)
	if n := e.cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("C1: the spent ref-A0 was handed to the CLI again on the next tick (%d calls)", n)
	}
	tr2 := e.restart()
	_ = tr2.RefreshDue(ctx)
	if n := e.cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("C1: the spent ref-A0 was handed to the CLI again after a restart (%d calls)", n)
	}
	e.assertLiveTokenOnly("after failed commit")
}

// NEW-1: a migration that died after the profile files became regular and the live
// file went token-only, but before the marker was written. The server's link loop
// must not link the store file to the token-only live file: the profile would then
// hold no refresh token and could never refresh.
func TestTokenCopiesInterruptedMigrationIsNotRelinked(t *testing.T) {
	m, home := newSwitcherTestEnv(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	full := fullClaudeCreds("acc-live", "ref-live", time.Now().Add(6*time.Hour))
	if err := os.WriteFile(live, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	store := m.StoredCredPath(switcher.ToolClaude, "main")
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenOnly := strings.Replace(full, `"refreshToken":"ref-live",`, "", 1)
	if tokenOnly == full {
		t.Fatal("fixture: the refresh token was not stripped")
	}
	if err := os.WriteFile(live, []byte(tokenOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.TokenCopies() {
		t.Fatal("fixture: no marker expected")
	}
	_ = m.EnsureActiveLinks()
	info, err := os.Lstat(store)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("NEW-1: EnsureActiveLinks linked the store file to the token-only live file")
	}
	if !strings.Contains(readFileString(t, store), "ref-live") {
		t.Fatal("NEW-1: the profile lost its refresh token")
	}
}

// F3: `switcher import` on a migrated install adopts a fresh login without ever
// linking the stored file to the live one, and leaves the live file token-only.
func TestTokenCopiesImportAfterMigrationStoresARegularFile(t *testing.T) {
	m, home := newSwitcherTestEnv(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(6 * time.Hour)
	if err := os.WriteFile(live, []byte(fullClaudeCreds("acc-A", "ref-A", expiry)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	reg := credtarget.NewRegistry(t.TempDir(), home)
	reg.LivePath = m.LiveCredPath
	fan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: m.Active, WithLock: m.WithLock}
	m.SetTokenCopyHooks(switcher.TokenCopyHooks{FanOutActive: fan.FanOutActive, ReplaceActive: fan.FanOutActiveReplacing})
	if _, err := m.MigrateToTokenCopies(switcher.MigrateOptions{Tools: []string{switcher.ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := os.WriteFile(live, []byte(fullClaudeCreds("acc-N", "ref-N", expiry)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "N"); err != nil {
		t.Fatalf("F3: Import after migration: %v", err)
	}
	store := m.StoredCredPath(switcher.ToolClaude, "N")
	if info, err := os.Lstat(store); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("F3: the imported profile is not a regular file (err=%v)", err)
	}
	if !strings.Contains(readFileString(t, store), "ref-N") {
		t.Fatal("F3: the imported profile lacks the full login")
	}
	if strings.Contains(readFileString(t, live), "refreshToken") {
		t.Fatal("F3: the live file still holds a refresh token after the import")
	}
}

// R-LOOP: a refresh lead longer than a token's life (the acceptance run's 9h knob
// against an 8h token) leaves a freshly rotated token due again at once. Several
// ticks must rotate the profile exactly once.
func TestTokenCopiesLeadLongerThanTokenLifeRotatesOnce(t *testing.T) {
	t.Setenv(usages.TargetLeadClaudeEnv, "9h")
	e := newE2EEnv(t)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(2*time.Hour)))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)
	for i := 0; i < 4; i++ {
		_ = e.tr.RefreshDue(context.Background())
	}
	if n := e.cli.totalCalls(); n != 1 {
		t.Fatalf("R-LOOP: %d rotations over 4 ticks, want 1", n)
	}
	e.assertLiveTokenOnly("after the ticks")
}

// A login imported right after a rotation carries a different refresh token than
// the one the rotation produced, so the cooldown after that rotation does not
// apply to it: when it is inside the lead it is refreshed on the next pass.
func TestTokenCopiesImportedLoginRightAfterRotationIsRefreshed(t *testing.T) {
	e := newE2EEnv(t)
	ctx := context.Background()
	soon := time.Now().Add(30 * time.Minute)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.cli.spentCount("ref-A0"); n != 1 {
		t.Fatalf("fixture: ref-A0 spent %d times, want 1", n)
	}
	// The user logs in again as the same profile a few seconds later; the new
	// token is already inside the lead.
	e.writeStored(switcher.ToolClaude, "A", fullClaudeCreds("acc-A9", "ref-A9", soon))
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue: %v", err)
	}
	if n := e.cli.spentCount("ref-A9"); n != 1 {
		t.Fatalf("an imported login right after a rotation was refreshed %d times, want 1 (held back by the previous rotation's cooldown)", n)
	}
	e.assertLiveTokenOnly("after the imported login")
	e.step("imported login")
}

// The Settings toggle owns the whole conversion. MCP OAuth logins belong to
// the live Claude document, so every rewrite and the final full restore must
// preserve their exact JSON bytes while the refresh token is spent at most once.
func TestTokenSettingCyclePreservesMCPOAuthAndSpendsOnce(t *testing.T) {
	e := newE2EEnv(t)
	clock := &tokenSettingClock{now: time.Now()}
	e.um.SetClock(clock)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "harness-docker-ctrl"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	mcp := []byte(`{"plane|incident": {"accessToken": "mcp-plane", "nested": [1, 2]}, "other": {"refreshToken": "mcp-other"}}`)
	full := fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(10*time.Minute))
	e.setLive(switcher.ToolClaude, strings.TrimSuffix(full, "}")+`,"mcpOAuth":`+string(mcp)+`}`)
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B0", "ref-B0", time.Now().Add(6*time.Hour)))
	s, _ := setupTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	tr := s.startTokenCopies(ctx, &wg, e.m, e.um, t.TempDir(), func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", e.cli)
		tr.SetExecutor("codex", e.cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	e.um.Start(ctx, &wg)
	defer func() { cancel(); wg.Wait() }()
	state := func() usages.TokenAutoRefreshToolState {
		resp, err := e.um.Snapshot(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		return resp.Settings.TokenAutoRefreshState[switcher.ToolClaude]
	}
	wait := func(want string) usages.TokenAutoRefreshToolState {
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			if got := state(); got.Status == want {
				return got
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("state = %+v, want %s", state(), want)
		return usages.TokenAutoRefreshToolState{}
	}
	assertMCP := func(step string) {
		data, err := os.ReadFile(e.liveClaude)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(doc["mcpOAuth"], mcp) {
			t.Fatalf("%s: MCP OAuth changed: %s", step, doc["mcpOAuth"])
		}
		e.step(step)
	}
	assertTokenOnly := func(step string) {
		data, err := os.ReadFile(e.liveClaude)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := credtarget.ParseTarget(switcher.ToolClaude, data); err != nil {
			t.Fatalf("%s: live login is not token-only: %v", step, err)
		}
	}
	set := func(on bool) {
		if _, err := e.um.UpdateSettings(ctx, 0, "", nil, false, &on, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertMCP("before migration")
	set(true)
	if got := wait("waiting"); !strings.Contains(got.Reason, "15m") {
		t.Fatalf("refusal = %+v", got)
	}
	if e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("migration bypassed the 15-minute guard")
	}
	assertMCP("refused migration")
	full = fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(6*time.Hour))
	e.setLive(switcher.ToolClaude, strings.TrimSuffix(full, "}")+`,"mcpOAuth":`+string(mcp)+`}`)
	clock.Advance(time.Minute)
	wait("migrated")
	assertTokenOnly("first migration")
	assertMCP("first migration")
	if _, _, err := e.m.Switch(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	assertMCP("switch A to B")
	if _, _, err := e.m.Switch(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	assertMCP("switch B to A")
	e.writeStored(switcher.ToolClaude, "A", fullClaudeCreds("acc-A1", "ref-A0", time.Now().Add(20*time.Minute)))
	if err := tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.cli.spentCount("ref-A0"); got != 1 {
		t.Fatalf("refresh token spent %d times, want 1", got)
	}
	assertMCP("refresh fan-out")
	set(false)
	wait("off")
	if e.m.TokenCopiesFor(switcher.ToolClaude) || !credtarget.HasRefreshToken(switcher.ToolClaude, []byte(readFileString(t, e.liveClaude))) {
		t.Fatal("OFF did not restore full live credentials")
	}
	if info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "A")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("OFF did not relink store: %v %v", info, err)
	}
	assertMCP("unmigration")
	set(true)
	wait("migrated")
	assertTokenOnly("second migration")
	assertMCP("second migration")
	if got := e.cli.spentCount("ref-A0"); got != 1 {
		t.Fatalf("old token spent again after round trip: %d", got)
	}
}

// A process can die after the full live credential is restored but before the
// store is relinked and the marker cleared. The next attempt must recognize
// the same refresh token as a resumable restore, not discard or overwrite it.
func TestUnmigrateResumesAfterFullLiveWriteCrash(t *testing.T) {
	e := newE2EEnv(t)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("access", "refresh", time.Now().Add(6*time.Hour)))
	if err := e.m.Import(switcher.ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)
	writer := &credtarget.Writer{}
	hooks := switcher.TokenCopyHooks{
		FanOutActive:    e.fan.FanOutActive,
		ReplaceActive:   e.fan.FanOutActiveReplacing,
		RefreshIncoming: e.tr.RefreshIncoming,
		RestoreFull: func(tool string, full []byte) error {
			if err := writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: e.liveClaude}, full); err != nil {
				return err
			}
			runtime.Goexit() // process death immediately after the atomic live write
			return nil
		},
	}
	e.m.SetTokenCopyHooks(hooks)
	done := make(chan struct{})
	go func() { defer close(done); _ = e.m.UnmigrateTokenCopies(switcher.ToolClaude) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted restore did not release locks")
	}
	if !e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("crash cleared marker before relinking")
	}
	live := []byte(readFileString(t, e.liveClaude))
	if !credtarget.HasRefreshToken(switcher.ToolClaude, live) {
		t.Fatal("no usable full credential after crash")
	}
	if !credtarget.HasRefreshToken(switcher.ToolClaude, []byte(e.stored(switcher.ToolClaude, "main"))) {
		t.Fatal("recovery store lost full credential")
	}
	hooks.RestoreFull = func(tool string, full []byte) error {
		return writer.WriteFullTarget(credtarget.Target{Tool: tool, Path: e.liveClaude}, full)
	}
	e.m.SetTokenCopyHooks(hooks)
	if err := e.m.UnmigrateTokenCopies(switcher.ToolClaude); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("resume kept marker")
	}
	if info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "main")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("resume did not relink: %v %v", info, err)
	}
	if !credtarget.HasRefreshToken(switcher.ToolClaude, []byte(readFileString(t, e.liveClaude))) {
		t.Fatal("resume lost full live credential")
	}
	e.step("unmigrate crash recovery")
}

// The marker gate on its own: with no marker nothing is spent, in either profile
// layout. B is added as a plain regular file (the layout a migrated install
// has), A is imported (the link layout), both are inside the lead, and neither
// the due pass, the incoming-profile hook nor a real Switch may hand a token to
// the CLI.
func TestTokenCopiesNoMarkerNeverSpendsAnyLayout(t *testing.T) {
	e := newE2EEnv(t)
	ctx := context.Background()
	soon := time.Now().Add(30 * time.Minute)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	e.writeStored(switcher.ToolClaude, "B", fullClaudeCreds("acc-B0", "ref-B0", soon))
	info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "B"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("fixture: B is not a regular file (err=%v)", err)
	}
	if e.m.TokenCopies() {
		t.Fatal("fixture: no marker expected")
	}
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatalf("RefreshDue without a marker: %v", err)
	}
	if err := e.tr.RefreshIncoming(ctx, switcher.ToolClaude, "B"); err != nil {
		t.Fatalf("RefreshIncoming without a marker: %v", err)
	}
	if _, _, err := e.m.Switch(switcher.ToolClaude, "B"); err != nil {
		t.Fatalf("Switch without a marker: %v", err)
	}
	if n := e.cli.totalCalls(); n != 0 {
		t.Fatalf("no marker: the CLI ran %d times", n)
	}
	if got := e.stored(switcher.ToolClaude, "B"); !strings.Contains(got, "ref-B0") {
		t.Fatalf("no marker: B's stored credential changed: %s", got)
	}
}

// Un-migration persists the merged document to the store BEFORE the live file
// gets the refresh token, so a failure at any step leaves the two consistent:
// no second profile, the active pointer unchanged, the MCP login intact, and the
// retry completes. Each window is made to fail through a read-only directory.
func TestTokenSettingUnmigrateFailureWindowsResume(t *testing.T) {
	const mcp = `{"plane":{"accessToken":"plane-mcp"}}`
	live := func(e *e2eEnv) string { return readFileString(e.t, e.liveClaude) }
	windows := []struct {
		name string
		dir  func(e *e2eEnv) string
		// liveFull: the failing step is after the live write, so the full
		// credential is already exposed in the live file.
		liveFull bool
	}{
		{"store write", func(e *e2eEnv) string { return filepath.Dir(e.m.StoredCredPath(switcher.ToolClaude, "A")) }, false},
		{"live write", func(e *e2eEnv) string { return filepath.Dir(e.liveClaude) }, false},
		{"marker removal", func(e *e2eEnv) string { return e.m.Root() }, true},
	}
	for _, w := range windows {
		t.Run(w.name, func(t *testing.T) {
			e := newE2EEnv(t)
			ctx := context.Background()
			e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(6*time.Hour)))
			if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
				t.Fatal(err)
			}
			e.migrate(switcher.ToolClaude)
			e.setLive(switcher.ToolClaude, strings.TrimSuffix(live(e), "}")+`,"mcpOAuth":`+mcp+`}`)
			hooks := switcher.TokenCopyHooks{
				FanOutActive:    e.fan.FanOutActive,
				ReplaceActive:   e.fan.FanOutActiveReplacing,
				RefreshIncoming: e.tr.RefreshIncoming,
				RestoreFull: func(tool string, full []byte) error {
					return (&credtarget.Writer{}).WriteFullTarget(credtarget.Target{Tool: tool, Path: e.liveClaude}, full)
				},
			}
			e.m.SetTokenCopyHooks(hooks)

			dir := w.dir(e)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			err := e.m.UnmigrateTokenCopies(switcher.ToolClaude)
			_ = os.Chmod(dir, 0o700)
			if err == nil {
				t.Skip("the directory mode did not stop the write (running as a user that ignores it?)")
			}
			if !e.m.TokenCopiesFor(switcher.ToolClaude) {
				t.Fatal("the marker is gone, nothing to resume")
			}
			if !w.liveFull && strings.Contains(live(e), `"refreshToken"`) {
				t.Fatalf("the refresh token reached the live file before the store caught up: %s", live(e))
			}
			if err := e.rec.Pass(ctx); err != nil {
				t.Fatalf("reconcile in the window: %v", err)
			}
			profiles, err := e.m.List()
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range profiles {
				if p.Tool == switcher.ToolClaude && p.Name != "A" {
					t.Errorf("the reconciler filed a new profile %q", p.Name)
				}
			}
			if active, _ := e.m.Active(switcher.ToolClaude); active != "A" {
				t.Errorf("the active profile moved to %q", active)
			}
			if err := e.m.UnmigrateTokenCopies(switcher.ToolClaude); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if e.m.TokenCopiesFor(switcher.ToolClaude) {
				t.Fatal("the marker survived the retry")
			}
			got := live(e)
			if !strings.Contains(got, `"refreshToken":"ref-A0"`) || !strings.Contains(got, `"mcpOAuth":`+mcp) {
				t.Fatalf("the retry lost the credential or the MCP login: %s", got)
			}
			if !strings.Contains(e.stored(switcher.ToolClaude, "A"), "plane-mcp") {
				t.Fatal("the store lacks the MCP login after the retry")
			}
			e.step("unmigrate failure window " + w.name)
		})
	}
}

// addLiveMCP adds an MCP OAuth login to the live claude file, as `claude mcp`
// does while the server owns the refresh token: only the live file learns of it.
func (e *e2eEnv) addLiveMCP(login string) {
	e.t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFileString(e.t, e.liveClaude)), &doc); err != nil {
		e.t.Fatal(err)
	}
	doc["mcpOAuth"] = json.RawMessage(login)
	out, err := json.Marshal(doc)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.liveClaude, out, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *e2eEnv) liveMCP() string {
	e.t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFileString(e.t, e.liveClaude)), &doc); err != nil {
		e.t.Fatal(err)
	}
	return string(doc["mcpOAuth"])
}

// ON -> refresh -> OFF -> ON: an MCP login added to the live file while the
// install is migrated (the stored credential never sees it) survives the
// un-migration, the refresher rotates once per token across both migrations, and
// the live file is token-only again after the second migration.
func TestTokenCopiesUnmigrateKeepsMCPLoginAndMigrateAgainSpendsOnce(t *testing.T) {
	e := newE2EEnv(t)
	ctx := context.Background()
	const mcp = `{"plane":{"accessToken":"plane-mcp"}}`
	soon := time.Now().Add(30 * time.Minute)
	e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", soon))
	if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	e.migrate(switcher.ToolClaude)
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	e.addLiveMCP(mcp)
	e.step("migrated and refreshed")

	if err := e.m.UnmigrateTokenCopies(switcher.ToolClaude); err != nil {
		t.Fatalf("un-migrate: %v", err)
	}
	if e.m.TokenCopiesFor(switcher.ToolClaude) {
		t.Fatal("the marker survived the un-migration")
	}
	live := readFileString(t, e.liveClaude)
	if !strings.Contains(live, `"refreshToken":"ref-r1"`) {
		t.Fatalf("the live file lacks the rotated full credential: %s", live)
	}
	if got := e.liveMCP(); got != mcp {
		t.Fatalf("the MCP login was lost by the un-migration: %s", got)
	}
	if info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "A")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the active profile was not re-linked (err=%v)", err)
	}
	// Off means the refresher is out of the picture again.
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.cli.totalCalls(); n != 1 {
		t.Fatalf("the CLI ran %d times after OFF, want 1 (the pre-OFF rotation)", n)
	}

	// ON again: the migration strips the live file and keeps the MCP login.
	e.migrate(switcher.ToolClaude)
	e.assertLiveTokenOnly("after re-migrating")
	if got := e.liveMCP(); got != mcp {
		t.Fatalf("the MCP login was lost by re-migrating: %s", got)
	}
	if !strings.Contains(e.stored(switcher.ToolClaude, "A"), "ref-r1") {
		t.Fatalf("the profile lost its credential across OFF/ON: %s", e.stored(switcher.ToolClaude, "A"))
	}
	if err := e.tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.cli.spentCount("ref-r1"); n > 1 {
		t.Fatalf("ref-r1 spent %d times across OFF/ON", n)
	}
	e.step("OFF/ON")
}

// A failure inside the un-migration, at each of its writes: the profile store
// (the merged credential goes there first), the live file, and the marker
// removal. The server stays up and the failure is retried, so in every window the
// refresher must spend nothing, the reconciler must not file the credential as a
// second profile or move the active pointer, MCP logins must survive, and the
// retry must complete.
func TestTokenCopiesUnmigrateFailureWindowsResume(t *testing.T) {
	const mcp = `{"plane":{"accessToken":"plane-mcp"}}`
	for _, window := range []struct {
		name string
		dir  func(e *e2eEnv) string // the directory whose writes fail
		due  bool                   // make the profile due once the failure happened
	}{
		{"store-write", func(e *e2eEnv) string { return filepath.Dir(e.m.StoredCredPath(switcher.ToolClaude, "A")) }, false},
		{"live-write", func(e *e2eEnv) string { return filepath.Dir(e.liveClaude) }, false},
		{"unmark", func(e *e2eEnv) string { return e.swRoot }, true},
	} {
		t.Run(window.name, func(t *testing.T) {
			e := newE2EEnv(t)
			ctx := context.Background()
			// Not due yet: a due incoming profile would be refreshed before the
			// un-migration reaches any of its writes, which is a different window.
			e.setLive(switcher.ToolClaude, fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(6*time.Hour)))
			if err := e.m.Import(switcher.ToolClaude, "A"); err != nil {
				t.Fatal(err)
			}
			e.migrate(switcher.ToolClaude)
			e.addLiveMCP(mcp)
			e.step("migrated")

			dir := window.dir(e)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			err := e.m.UnmigrateTokenCopies(switcher.ToolClaude)
			_ = os.Chmod(dir, 0o700)
			if err == nil {
				t.Skip("fixture: the write still succeeded (running as a user that ignores directory modes?)")
			}
			if !e.m.TokenCopiesFor(switcher.ToolClaude) {
				t.Fatal("fixture: the marker is gone, nothing to resume")
			}
			if window.due {
				// The window is now long enough for the profile to fall due. Store and
				// live keep the same refresh token; only the expiry moves.
				dueFull := fullClaudeCreds("acc-A0", "ref-A0", time.Now().Add(30*time.Minute))
				e.writeStored(switcher.ToolClaude, "A", dueFull)
				if strings.Contains(readFileString(t, e.liveClaude), "refreshToken") {
					e.setLive(switcher.ToolClaude, strings.TrimSuffix(dueFull, "}")+`,"mcpOAuth":`+mcp+`}`)
				}
			}
			if err := e.tr.RefreshDue(ctx); err != nil {
				t.Fatalf("RefreshDue in the window: %v", err)
			}
			if err := e.rec.Pass(ctx); err != nil {
				t.Fatalf("reconcile in the window: %v", err)
			}
			if window.due {
				// A full credential is exposed: the refresher stands down. (For the
				// other windows live is still token-only and a due profile may rotate.)
				if n := e.cli.totalCalls(); n != 0 {
					t.Fatalf("the CLI ran %d times while a full credential was exposed", n)
				}
			}
			profiles, err := e.m.List()
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range profiles {
				if p.Tool == switcher.ToolClaude && p.Name != "A" {
					t.Errorf("the reconciler filed the credential as a new profile %q", p.Name)
				}
			}
			if active, _ := e.m.Active(switcher.ToolClaude); active != "A" {
				t.Errorf("the active profile moved to %q", active)
			}
			if got := e.liveMCP(); got != mcp {
				t.Errorf("the MCP login did not survive the window: %s", got)
			}

			// The retry finishes.
			if err := e.m.UnmigrateTokenCopies(switcher.ToolClaude); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if e.m.TokenCopiesFor(switcher.ToolClaude) {
				t.Fatal("the marker survived the retry")
			}
			storedRefresh := claudeRefreshInFile.FindStringSubmatch(e.stored(switcher.ToolClaude, "A"))
			if storedRefresh == nil || !strings.Contains(readFileString(t, e.liveClaude), `"refreshToken":"`+storedRefresh[1]+`"`) || e.liveMCP() != mcp {
				t.Fatalf("the retry lost the credential or the MCP login: %s", readFileString(t, e.liveClaude))
			}
			if info, err := os.Lstat(e.m.StoredCredPath(switcher.ToolClaude, "A")); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the retry did not re-link the profile (err=%v)", err)
			}
			e.step("unmigrate " + window.name)
		})
	}
}
