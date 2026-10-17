package usages

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// All credentials here are fake and live under t.TempDir(); TestMain points
// HOME, CLAUDE_CONFIG_DIR and CODEX_HOME at a throwaway root.

// scriptedRunner is a refreshRunner whose behaviour each test scripts per call.
type scriptedRunner struct {
	mu      sync.Mutex
	calls   int
	fn      func(call int, dir string) (refreshRunResult, error)
	entered chan struct{}
	release chan struct{}
}

func (s *scriptedRunner) Refresh(ctx context.Context, dir, flag string) error {
	_, err := s.Run(ctx, dir, flag)
	return err
}

// RunStarted makes the scripted runner a refreshRunner, so the refresher sees
// Started and Output exactly as it does from the docker executors. The start
// notification fires before the scripted behaviour runs.
func (s *scriptedRunner) RunStarted(ctx context.Context, dir, flag string, onStart func()) (refreshRunResult, error) {
	if onStart != nil {
		onStart()
	}
	return s.Run(ctx, dir, flag)
}

func (s *scriptedRunner) Run(ctx context.Context, dir, flag string) (refreshRunResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	return s.fn(call, dir)
}

func (s *scriptedRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type fanOutCall struct {
	tool, profile string
	full          []byte
}

type recordingFanOut struct {
	mu    sync.Mutex
	calls []fanOutCall
	err   error
}

func (f *recordingFanOut) FanOut(tool, profile string, full []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fanOutCall{tool, profile, append([]byte(nil), full...)})
	return f.err
}

func (f *recordingFanOut) snapshot() []fanOutCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fanOutCall(nil), f.calls...)
}

type refreshHarness struct {
	t        *testing.T
	root     string
	clock    *fakeClock
	mgr      *Manager
	r        *TargetRefresher
	fan      *recordingFanOut
	runner   *scriptedRunner
	mu       sync.Mutex
	profiles map[string][]ProfileInfo // tool -> profiles
	alerts   []string
}

func newRefreshHarness(t *testing.T, runner *scriptedRunner) *refreshHarness {
	t.Helper()
	h := &refreshHarness{t: t, root: t.TempDir(), clock: &fakeClock{now: time.Unix(1_800_000_000, 0)}, runner: runner, fan: &recordingFanOut{}, profiles: map[string][]ProfileInfo{}}
	h.mgr = NewManager(NewStoreAt(t.TempDir()), EmptyRegistry())
	h.mgr.SetClock(h.clock)
	h.mgr.maintRoot = h.root
	h.mgr.maintGuard = newMaintenanceGuard()
	h.mgr.maintGuard.root = h.root
	h.mgr.SetTargetFanOut(h.fan)
	h.mgr.SetProfileLister(func(tool string) ([]ProfileInfo, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := append([]ProfileInfo(nil), h.profiles[tool]...)
		for i := range out {
			out[i].HasCredentials = true
		}
		return out, nil
	})
	h.newRefresher()
	return h
}

// newRefresher builds a fresh TargetRefresher over the same root, as a restarted
// server would.
func (h *refreshHarness) newRefresher() *TargetRefresher {
	r := NewTargetRefresher(h.mgr)
	r.executors = map[string]refreshExecutor{"claude": h.runner, "codex": h.runner}
	r.available = func() bool { return true }
	r.reachable = func(context.Context) bool { return true }
	r.SetEnabled(func() bool { return true })
	r.SetToolEnabled(func(string) bool { return true })
	r.SetAlertHook(func(kind, detail string) {
		h.mu.Lock()
		h.alerts = append(h.alerts, kind+" "+detail)
		h.mu.Unlock()
	})
	h.r = r
	h.mgr.SetTargetRefresher(r)
	return r
}

func (h *refreshHarness) alertKinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var kinds []string
	for _, a := range h.alerts {
		kinds = append(kinds, strings.SplitN(a, " ", 2)[0])
	}
	return kinds
}

// stateAlertKinds is alertKinds without the time-based expiry edges, for tests
// about refresh outcomes.
func (h *refreshHarness) stateAlertKinds() []string {
	var kinds []string
	for _, k := range h.alertKinds() {
		if k != TargetLevelExpiring && k != TargetLevelExpired {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

func claudeFull(access, refresh string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference"],"subscriptionType":"max"}}`, access, refresh, expires.UnixMilli())
}

func fakeJWT(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.Itoa(int(exp.Unix())) + `}`))
	return "h." + payload + ".s"
}

func codexFull(exp time.Time, access, refresh string, last time.Time) string {
	return fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"account_id":"acct-1","id_token":"h.e30.s"},"last_refresh":%q}`, access+"."+fakeJWT(exp)[2:], refresh, last.UTC().Format(time.RFC3339Nano))
}

func (h *refreshHarness) addProfile(tool, name string, active bool, body string) ProfileInfo {
	h.t.Helper()
	dir := filepath.Join(h.root, "profiles", tool, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	file := ".credentials.json"
	if tool == "codex" {
		file = "auth.json"
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	p := ProfileInfo{Tool: tool, Name: name, Active: active, CredPath: path, HasCredentials: true}
	h.mu.Lock()
	h.profiles[tool] = append(h.profiles[tool], p)
	h.mu.Unlock()
	return p
}

func (h *refreshHarness) stored(p ProfileInfo) string {
	h.t.Helper()
	data, err := os.ReadFile(p.CredPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *refreshHarness) rescueFiles(tool, name string) []string {
	entries, _ := os.ReadDir(filepath.Join(h.root, "backups", tool))
	var out []string
	for _, e := range entries {
		if _, ok := RotatedRescueTimestamp(e.Name(), name); ok {
			out = append(out, filepath.Join(h.root, "backups", tool, e.Name()))
		}
	}
	return out
}

// rotateRunner writes a newer credential into the temp copy, like claude does.
func rotateClaudeRunner(newAccess, newRefresh string, newExpiry time.Time) func(int, string) (refreshRunResult, error) {
	return func(call int, dir string) (refreshRunResult, error) {
		body := claudeFull(newAccess, newRefresh, newExpiry)
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
	}
}

// ── due pass, commit, fan-out ───────────────────────────────────────────

func TestTokenRefresherRefreshesEveryDueProfileIncludingTheActiveOne(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("new-access", "new-refresh", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	active := h.addProfile("claude", "main", true, claudeFull("old-a", "refresh-a", now.Add(30*time.Minute)))
	idle := h.addProfile("claude", "backup", false, claudeFull("old-b", "refresh-b", now.Add(-time.Minute)))
	fresh := h.addProfile("claude", "fresh", false, claudeFull("old-c", "refresh-c", now.Add(5*time.Hour)))

	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := runner.callCount(); got != 2 {
		t.Fatalf("executor calls = %d, want 2 (active and idle are due, fresh is not)", got)
	}
	for _, p := range []ProfileInfo{active, idle} {
		if !strings.Contains(h.stored(p), "new-refresh") {
			t.Fatalf("%s was not committed: %s", p.Name, h.stored(p))
		}
		if files := h.rescueFiles("claude", p.Name); len(files) != 0 {
			t.Fatalf("%s: persist-first rescue file left behind after the commit: %v", p.Name, files)
		}
	}
	if !strings.Contains(h.stored(fresh), "refresh-c") {
		t.Fatal("a profile outside the lead was touched")
	}
	calls := h.fan.snapshot()
	if len(calls) != 2 {
		t.Fatalf("fan-out calls = %d, want 2", len(calls))
	}
	for _, c := range calls {
		if c.tool != "claude" || !strings.Contains(string(c.full), "new-refresh") {
			t.Fatalf("fan-out got %+v", c)
		}
	}
	if len(h.r.stateStore().journal()) != 0 {
		t.Fatal("journal not cleared after success")
	}
}

func TestTokenRefresherLeavesLinkedAndFreshProfilesAlone(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "n", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	live := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(live, []byte(claudeFull("a", "r", now.Add(time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := h.addProfile("claude", "linked", true, claudeFull("a", "r", now.Add(time.Minute)))
	if err := os.Remove(linked.CredPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(live, linked.CredPath); err != nil {
		t.Fatal(err)
	}
	recent := h.addProfile("claude", "recent", false, claudeFull("a", "r2", now.Add(time.Minute)))
	h.mu.Lock()
	switched := now.Add(-30 * time.Second)
	h.profiles["claude"][1].SwitchedAwayAt = &switched
	h.mu.Unlock()
	_ = recent
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("executor ran %d times for a linked profile and a profile switched away 30 s ago", runner.callCount())
	}
}

func TestTokenRefresherCodexUsesItsOwnLead(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		body := codexFull(now.Add(240*time.Hour), "new", "refresh-new", now)
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600)
	}}
	h := newRefreshHarness(t, runner)
	due := h.addProfile("codex", "work", true, codexFull(now.Add(10*time.Hour), "old", "refresh-old", now.Add(-time.Hour)))
	far := h.addProfile("codex", "spare", false, codexFull(now.Add(100*time.Hour), "old", "refresh-spare", now.Add(-time.Hour)))
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d, want 1 (only the profile inside the 24 h lead)", runner.callCount())
	}
	if !strings.Contains(h.stored(due), "refresh-new") || !strings.Contains(h.stored(far), "refresh-spare") {
		t.Fatal("wrong codex profile committed")
	}
}

// F2: a tool still on the link model is left to the legacy coordinators.
func TestTokenRefresherLeavesAnUnmigratedToolAlone(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		body := codexFull(now.Add(240*time.Hour), "new", "refresh-new", now)
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600)
	}}
	h := newRefreshHarness(t, runner)
	held := h.addProfile("codex", "work", false, codexFull(now.Add(10*time.Hour), "old", "refresh-old", now.Add(-time.Hour)))
	h.r.SetToolEnabled(func(tool string) bool { return tool == "claude" })
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.r.RefreshIncoming(context.Background(), "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 || !strings.Contains(h.stored(held), "refresh-old") {
		t.Fatalf("an unmigrated tool was refreshed (calls=%d)", runner.callCount())
	}
	for _, s := range h.r.Statuses() {
		if s.Tool == "codex" {
			t.Fatalf("an unmigrated tool is reported: %+v", s)
		}
	}
	if h.r.m.targetRefresher == nil {
		h.r.m.SetTargetRefresher(h.r)
	}
	if h.r.m.targetRefresherOwnsTool("codex") || !h.r.m.targetRefresherOwnsTool("claude") {
		t.Fatal("legacy coordinators must keep codex and hand over claude")
	}
	h.r.SetToolEnabled(func(string) bool { return true })
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d once the tool is migrated, want 1", runner.callCount())
	}
}

// ── M2: spend once ──────────────────────────────────────────────────────

func TestTokenRefresherNotSentRetriesWithBackoffAndTouchesNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var started bool
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		if call == 1 {
			return refreshRunResult{Started: false}, errors.New("docker: daemon error")
		}
		if call == 2 {
			return refreshRunResult{Started: true, Output: "Unable to connect to API (ENOTFOUND)"}, errors.New("exit status 1")
		}
		started = true
		return rotateClaudeRunner("n", "n2", now.Add(10*time.Hour))(call, dir)
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	before := h.stored(p)
	ctx := context.Background()

	if err := h.r.RefreshDue(ctx); err == nil {
		t.Fatal("a failed refresh must be reported")
	}
	if h.stored(p) != before {
		t.Fatal("store changed after a failed refresh")
	}
	// Inside the backoff window nothing runs.
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 1 {
		t.Fatalf("within backoff: err=%v calls=%d", err, runner.callCount())
	}
	h.clock.now = h.clock.now.Add(6 * time.Minute)
	if err := h.r.RefreshDue(ctx); err == nil || runner.callCount() != 2 {
		t.Fatalf("second try: err=%v calls=%d (a DNS failure proves nothing was sent, so it retries)", err, runner.callCount())
	}
	if h.stored(p) != before || len(h.r.stateStore().journal()) != 0 {
		t.Fatal("not-sent failures must leave the store untouched and the journal empty")
	}
	// Backoff doubled: 6 minutes later is still too early, 11 is not.
	h.clock.now = h.clock.now.Add(6 * time.Minute)
	_ = h.r.RefreshDue(ctx)
	if runner.callCount() != 2 {
		t.Fatalf("calls = %d, want 2 (backoff must double to 10 minutes)", runner.callCount())
	}
	h.clock.now = h.clock.now.Add(5 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil || !started {
		t.Fatalf("third try: err=%v started=%v", err, started)
	}
	if !strings.Contains(h.stored(p), "n2") {
		t.Fatalf("recovered refresh not committed: %s", h.stored(p))
	}
	if h.r.AccountStates()["claude/main"] != "" {
		t.Fatalf("a recovered account must not carry a state: %v", h.r.AccountStates())
	}
}

func TestTokenRefresherUnknownOutcomeNeverRespendsEvenAfterARestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		// The CLI started, the request may have gone out, nothing came back.
		return refreshRunResult{Started: true, Output: "timeout of 30000ms exceeded"}, errors.New("signal: killed")
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	before := h.stored(p)
	ctx := context.Background()

	err := h.r.RefreshDue(ctx)
	_ = err
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", runner.callCount())
	}
	if h.stored(p) != before {
		t.Fatal("the stored credential must not change on an unknown outcome")
	}
	if got := h.r.AccountStates()["claude/main"]; got != AccountNeedsReloginSoon {
		t.Fatalf("state = %q, want %q", got, AccountNeedsReloginSoon)
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != AccountNeedsReloginSoon {
		t.Fatalf("alerts = %v", kinds)
	}
	// Hours later, and in a brand new process, the same token is never spent again.
	for i := 0; i < 3; i++ {
		h.clock.now = h.clock.now.Add(2 * time.Hour)
		if i == 1 {
			h.newRefresher()
		}
		if err := h.r.RefreshDue(ctx); err != nil {
			t.Fatalf("pass %d: a terminal account is skipped quietly in the due pass, got %v", i, err)
		}
	}
	if runner.callCount() != 1 {
		t.Fatalf("the refresh token was spent %d times", runner.callCount())
	}
	// RefreshIncoming (a switch to this profile) must refuse instead.
	if err := h.r.RefreshIncoming(ctx, "claude", "main"); !errors.Is(err, ErrAccountNeedsLogin) {
		t.Fatalf("RefreshIncoming err = %v, want ErrAccountNeedsLogin", err)
	}
	// A human logs in again: a new refresh token is a new account identity.
	if err := os.WriteFile(p.CredPath, []byte(claudeFull("b", "r-new", h.clock.now.Add(5*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.fn = rotateClaudeRunner("c", "r-after", h.clock.now.Add(8*time.Hour))
	if err := h.r.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(p), "r-after") {
		t.Fatalf("a re-login must clear the terminal state: %s", h.stored(p))
	}
}

func TestTokenRefresherClaudeInvalidGrantIsTerminalAndNeverWipes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		// Claude marks the refresh token dead in its own file on invalid_grant.
		body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"","expiresAt":1}}`)
		_ = os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
		return refreshRunResult{Started: true, Output: "OAuth error"}, errors.New("exit status 1")
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	before := h.stored(p)

	_ = h.r.RefreshDue(context.Background())
	if h.stored(p) != before {
		t.Fatalf("invalid_grant must not wipe or rewrite the stored credential: %s", h.stored(p))
	}
	if h.r.AccountStates()["claude/main"] != AccountReauthRequired {
		t.Fatalf("states = %v", h.r.AccountStates())
	}
	_ = h.r.RefreshDue(context.Background())
	if runner.callCount() != 1 {
		t.Fatalf("a rejected refresh token was tried %d times", runner.callCount())
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != AccountReauthRequired {
		t.Fatalf("alerts = %v", kinds)
	}
	if len(h.fan.snapshot()) != 0 {
		t.Fatal("nothing may be fanned out after a failed refresh")
	}
}

func TestTokenRefresherCodexRejectionMessagesAreTerminal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, msg := range []string{
		"Your access token could not be refreshed because your refresh token was already used.",
		"Your access token could not be refreshed because your refresh token was revoked.",
		"Your access token could not be refreshed because your refresh token has expired.",
		"Your access token could not be refreshed because you have since logged out or signed in to another account.",
	} {
		runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
			return refreshRunResult{Started: true, Output: msg}, errors.New("exit status 1")
		}}
		h := newRefreshHarness(t, runner)
		p := h.addProfile("codex", "work", true, codexFull(now.Add(time.Hour), "old", "refresh-old", now.Add(-time.Hour)))
		before := h.stored(p)
		_ = h.r.RefreshDue(context.Background())
		if h.r.AccountStates()["codex/work"] != AccountReauthRequired || h.stored(p) != before {
			t.Fatalf("%q: states=%v changed=%v", msg, h.r.AccountStates(), h.stored(p) != before)
		}
	}
}

func TestTokenRefresherCleanExitWithoutRotationIsQuietlyRetried(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{Started: true}, nil // exits 0, rotated nothing
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	ctx := context.Background()
	for i := 0; i < targetNoopLimit; i++ {
		_ = h.r.RefreshDue(ctx)
		h.clock.now = h.clock.now.Add(31 * time.Minute)
		// The token is long expired by now; keep it due and valid-looking.
		_ = os.WriteFile(p.CredPath, []byte(claudeFull("a", "r1", h.clock.now.Add(time.Minute))), 0o600)
		if i < targetNoopLimit-1 && len(h.stateAlertKinds()) != 0 {
			t.Fatalf("pass %d alerted too early: %v", i, h.stateAlertKinds())
		}
	}
	if runner.callCount() != targetNoopLimit {
		t.Fatalf("calls = %d, want %d", runner.callCount(), targetNoopLimit)
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != AccountRefreshFailed {
		t.Fatalf("after %d no-op runs the account is reported as refresh_failed, alerts=%v", targetNoopLimit, kinds)
	}
	if h.r.AccountStates()["claude/main"] != AccountRefreshFailed {
		t.Fatalf("states = %v", h.r.AccountStates())
	}
}

// ── single flight ───────────────────────────────────────────────────────

func TestTokenRefresherIsSingleFlightPerAccount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{
		fn:      rotateClaudeRunner("n", "r2", now.Add(8*time.Hour)),
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = h.r.RefreshDue(context.Background()) }()
	<-runner.entered
	// A second pass while the first is in flight must not start another run.
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A holder of the cross-process lock (a switch) also keeps the refresher out.
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d while one run is in flight", runner.callCount())
	}
	close(runner.release)
	wg.Wait()
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", runner.callCount())
	}
}

func TestTokenRefresherSkipsAnAccountWhoseLockIsHeldByASwitch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	release, err := HoldMaintenance(h.root, "claude", p.Name, p.CredPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("the refresher must not run while a switch holds the account lock")
	}
	release()
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 1 {
		t.Fatalf("after the switch released the lock: err=%v calls=%d", err, runner.callCount())
	}
}

// ── persist-first and restart recovery (T7) ────────────────────────────

func journalKeyed(h *refreshHarness, tool, name, phase, spentToken, rescue, tempDir string) {
	h.t.Helper()
	if err := h.r.stateStore().setJournal(refreshJournalEntry{Tool: tool, Name: name, Phase: phase, SpentHash: tokenHash(tool, spentToken), Rescue: rescue, TempDir: tempDir, StartedAt: h.clock.now.UTC()}); err != nil {
		h.t.Fatal(err)
	}
}

func TestTokenRefresherRecoversARotatedRescueFileAfterACrash(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "spent-refresh", now.Add(10*time.Minute)))
	// The server died after the CLI rotated the token and the rescue file was
	// written, before the commit: the store still holds the spent token.
	rescue := filepath.Join(h.root, "backups", "claude", RotatedRescueName("main", now))
	if err := os.MkdirAll(filepath.Dir(rescue), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rescue, []byte(claudeFull("fresh", "fresh-refresh", now.Add(8*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	journalKeyed(h, "claude", "main", journalRotated, "spent-refresh", rescue, "")

	r2 := h.newRefresher() // the restarted server
	r2.recover(context.Background())
	if !strings.Contains(h.stored(p), "fresh-refresh") {
		t.Fatalf("the rescued credentials were not committed: %s", h.stored(p))
	}
	if _, err := os.Stat(rescue); !os.IsNotExist(err) {
		t.Fatalf("rescue file still present after the recovery commit: %v", err)
	}
	if len(r2.stateStore().journal()) != 0 {
		t.Fatal("journal not cleared")
	}
	if got := h.fan.snapshot(); len(got) != 1 || !strings.Contains(string(got[0].full), "fresh-refresh") {
		t.Fatalf("recovery must fan the committed credential out: %+v", got)
	}
	if runner.callCount() != 0 {
		t.Fatal("recovery must never run the CLI")
	}
}

func TestTokenRefresherRecoversATempCopyThatRotatedBeforeTheCrash(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "spent-refresh", now.Add(10*time.Minute)))
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".credentials.json"), []byte(claudeFull("fresh", "fresh-refresh", now.Add(8*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	journalKeyed(h, "claude", "main", journalSpending, "spent-refresh", "", tmp)

	h.newRefresher().recover(context.Background())
	if !strings.Contains(h.stored(p), "fresh-refresh") {
		t.Fatalf("the temp copy's rotation was not recovered: %s", h.stored(p))
	}
}

func TestTokenRefresherAnInterruptedSpendWithNothingToRecoverIsNeverRepeated(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "spent-refresh", now.Add(10*time.Minute)))
	// Killed mid-run (the CLI had started); the temp dir is gone (reboot), so
	// whether the provider processed the refresh is unknowable.
	journalKeyed(h, "claude", "main", journalStarted, "spent-refresh", "", filepath.Join(t.TempDir(), "gone"))
	before := h.stored(p)

	r2 := h.newRefresher()
	r2.recover(context.Background())
	_ = r2.RefreshDue(context.Background())
	if runner.callCount() != 0 {
		t.Fatal("a token that may already be spent was spent again after a restart")
	}
	if h.stored(p) != before {
		t.Fatal("the store must not change")
	}
	if got := r2.AccountStates()["claude/main"]; got != AccountNeedsReloginSoon {
		t.Fatalf("state = %q", got)
	}
	if kinds := h.stateAlertKinds(); len(kinds) == 0 || kinds[len(kinds)-1] != AccountNeedsReloginSoon {
		t.Fatalf("alerts = %v", kinds)
	}
	if len(r2.stateStore().journal()) != 0 {
		t.Fatal("journal must be settled")
	}
}

func TestTokenRefresherJournalEntryForAReplacedTokenIsDropped(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	// The profile already holds a newer token (a commit or a login got there first).
	h.addProfile("claude", "main", true, claudeFull("a", "newer-refresh", now.Add(5*time.Hour)))
	journalKeyed(h, "claude", "main", journalSpending, "spent-refresh", "", "")
	h.newRefresher().recover(context.Background())
	if len(h.r.stateStore().journal()) != 0 {
		t.Fatal("journal entry for a token no profile holds must be dropped")
	}
	if len(h.r.AccountStates()) != 0 {
		t.Fatalf("no account state expected: %v", h.r.AccountStates())
	}
}

func TestTokenRefresherPersistsFirstThenCommitsAndSurvivesACommitAbort(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var h *refreshHarness
	var p ProfileInfo
	runner := &scriptedRunner{}
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		// Between the spend and the commit someone logs the profile in again, so
		// the CAS must refuse to overwrite it and keep the rotated tokens.
		body := claudeFull("n", "r-rotated", now.Add(8*time.Hour))
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
			return refreshRunResult{Started: true}, err
		}
		if err := os.WriteFile(p.CredPath, []byte(claudeFull("login", "r-login", now.Add(8*time.Hour))), 0o600); err != nil {
			return refreshRunResult{Started: true}, err
		}
		return refreshRunResult{Started: true}, nil
	}
	h = newRefreshHarness(t, runner)
	p = h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))

	err := h.r.RefreshDue(context.Background())
	if err == nil {
		t.Fatal("an aborted commit must be reported")
	}
	if !strings.Contains(h.stored(p), "r-login") {
		t.Fatalf("the CAS overwrote a newer login: %s", h.stored(p))
	}
	files := h.rescueFiles("claude", "main")
	if len(files) == 0 {
		t.Fatal("the rotated credentials must survive in a rescue file")
	}
	found := false
	for _, f := range files {
		data, _ := os.ReadFile(f)
		if strings.Contains(string(data), "r-rotated") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rescue file holds the rotated token: %v", files)
	}
	if len(h.fan.snapshot()) != 0 {
		t.Fatal("an aborted commit must not fan out")
	}
	if len(h.r.stateStore().journal()) != 0 {
		t.Fatal("journal must be cleared once the rescue file holds the result")
	}
}

func TestTokenRefresherFanOutFailureIsAlertedNotFatal(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	h.fan.err = errors.New("target busy")
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatalf("a fan-out failure is healed by the reconciler and must not fail the refresh: %v", err)
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatal("the store commit must stand")
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != "fanout_failed" {
		t.Fatalf("alerts = %v", kinds)
	}
}

// ── RefreshIncoming and plumbing ────────────────────────────────────────

func TestTokenRefresherRefreshIncomingOnlyWhenDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "fresh", false, claudeFull("a", "r-fresh", now.Add(6*time.Hour)))
	due := h.addProfile("claude", "stale", false, claudeFull("a", "r-stale", now.Add(10*time.Minute)))
	ctx := context.Background()
	if err := h.r.RefreshIncoming(ctx, "claude", "fresh"); err != nil || runner.callCount() != 0 {
		t.Fatalf("not due: err=%v calls=%d", err, runner.callCount())
	}
	if err := h.r.RefreshIncoming(ctx, "claude", "stale"); err != nil || runner.callCount() != 1 {
		t.Fatalf("due: err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(due), "r2") {
		t.Fatal("the incoming profile was not committed")
	}
	if err := h.r.RefreshIncoming(ctx, "claude", "ghost"); err == nil {
		t.Fatal("an unknown profile must be an error")
	}
	// A failing refresh must surface so the switch can answer 409.
	runner.fn = func(int, string) (refreshRunResult, error) {
		return refreshRunResult{Started: false}, errors.New("no docker")
	}
	h.addProfile("claude", "stale2", false, claudeFull("a", "r-stale2", now.Add(10*time.Minute)))
	if err := h.r.RefreshIncoming(ctx, "claude", "stale2"); err == nil {
		t.Fatal("RefreshIncoming must report a failed refresh")
	}
}

func TestTokenRefresherDisabledDoesNothingAndLegacyStandsDown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	enabled := false
	h.r.SetEnabled(func() bool { return enabled })
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 0 {
		t.Fatalf("disabled: err=%v calls=%d", err, runner.callCount())
	}
	if h.mgr.targetRefresherOwnsRefresh() {
		t.Fatal("a disabled refresher must not claim ownership")
	}
	enabled = true
	if !h.mgr.targetRefresherOwnsRefresh() {
		t.Fatal("an enabled refresher owns refresh")
	}
}

func TestTokenRefresherNeverTouchesTheRealCredentialFiles(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, "/tmp/") {
		t.Fatalf("test HOME is not isolated: %s", home)
	}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if v := os.Getenv(key); !strings.HasPrefix(v, "/tmp/") {
			t.Fatalf("%s is not isolated: %q", key, v)
		}
	}
}

func TestClassifyRefreshRunTable(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		run      refreshRunResult
		err      error
		rotated  string
		advanced bool
		start    string
		want     RefreshOutcome
		noop     bool
	}{
		{"rotated despite error", "claude", refreshRunResult{Started: true}, errors.New("x"), `{"a":1}`, true, "r", OutcomeRefreshed, false},
		{"not started", "claude", refreshRunResult{Started: false}, errors.New("x"), "", false, "r", OutcomeNotSent, false},
		{"claude dead marker", "claude", refreshRunResult{Started: true}, errors.New("x"), `{"claudeAiOauth":{"refreshToken":""}}`, false, "r", OutcomeInvalidGrant, false},
		{"claude invalid_grant output", "claude", refreshRunResult{Started: true, Output: `{"error":"invalid_grant"}`}, errors.New("x"), "", false, "r", OutcomeInvalidGrant, false},
		{"codex revoked", "codex", refreshRunResult{Started: true, Output: "your refresh token was revoked."}, errors.New("x"), "", false, "r", OutcomeInvalidGrant, false},
		{"clean exit no rotation", "claude", refreshRunResult{Started: true}, nil, `{"claudeAiOauth":{"refreshToken":"r"}}`, false, "r", OutcomeNotSent, true},
		{"dns failure", "claude", refreshRunResult{Started: true, Output: "getaddrinfo ENOTFOUND platform.claude.com"}, errors.New("x"), "", false, "r", OutcomeNotSent, false},
		{"timeout after start", "claude", refreshRunResult{Started: true, Output: "timeout"}, errors.New("x"), "", false, "r", OutcomeUnknown, false},
		{"generic failure after start", "codex", refreshRunResult{Started: true, Output: "Failed to refresh token: 500"}, errors.New("x"), "", false, "r", OutcomeUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRefreshRun(tc.tool, tc.run, tc.err, []byte(tc.rotated), tc.advanced, tc.start)
			if got.Outcome != tc.want || got.Noop != tc.noop {
				t.Fatalf("got outcome=%d noop=%v (%s), want outcome=%d noop=%v", got.Outcome, got.Noop, got.Detail, tc.want, tc.noop)
			}
		})
	}
}

func TestTargetBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := targetBackoff(i + 1); got != w {
			t.Fatalf("failures=%d: %v, want %v", i+1, got, w)
		}
	}
}

func TestRefreshStateSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	s := newRefreshStateStore(root)
	s.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountReauthRequired, SpentHash: "abc", Since: time.Unix(1, 0)})
	if err := s.setJournal(refreshJournalEntry{Tool: "codex", Name: "work", Phase: journalSpending, SpentHash: "def"}); err != nil {
		t.Fatal(err)
	}
	again := newRefreshStateStore(root)
	if e, ok := again.account("claude", "abc"); !ok || e.State != AccountReauthRequired {
		t.Fatalf("account state lost: %+v %v", e, ok)
	}
	if j := again.journal(); len(j) != 1 || j[0].Phase != journalSpending {
		t.Fatalf("journal lost: %+v", j)
	}
	info, err := os.Stat(filepath.Join(root, refreshStateFileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, err = %v; want 0600", info.Mode().Perm(), err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, refreshStateFileName))
	var parsed map[string]any
	if json.Unmarshal(raw, &parsed) != nil {
		t.Fatal("state file is not JSON")
	}
	for _, secret := range []string{"refreshToken", "accessToken", "access_token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("the state file must never hold credentials: found %q", secret)
		}
	}
}

// ── interplay rules (reconciler / switch / startup) ─────────────────────

// A refresher that was only enabled, with no explicit tool set, owns no tool:
// the per-tool gate fails closed so nothing is spent by accident.
func TestTokenRefresherWithoutToolSetSpendsNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	fresh := NewTargetRefresher(h.mgr)
	fresh.executors = map[string]refreshExecutor{"claude": runner, "codex": runner}
	fresh.available = func() bool { return true }
	fresh.reachable = func(context.Context) bool { return true }
	fresh.SetEnabled(func() bool { return true })
	h.mgr.SetTargetRefresher(fresh)
	if fresh.ownsTool("claude") || fresh.ownsTool("codex") {
		t.Fatal("a refresher without a tool set must own no tool")
	}
	if err := fresh.RefreshDue(context.Background()); err != nil || runner.callCount() != 0 {
		t.Fatalf("refresher without a tool set spent: err=%v calls=%d", err, runner.callCount())
	}
}

func TestTokenRefresherIsOffByDefault(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	fresh := NewTargetRefresher(h.mgr)
	fresh.executors = map[string]refreshExecutor{"claude": runner, "codex": runner}
	fresh.available = func() bool { return true }
	fresh.reachable = func(context.Context) bool { return true }
	h.mgr.SetTargetRefresher(fresh)
	if h.mgr.targetRefresherOwnsRefresh() {
		t.Fatal("a new refresher must not own refresh until it is enabled")
	}
	if err := fresh.RefreshDue(context.Background()); err != nil || runner.callCount() != 0 {
		t.Fatalf("a disabled refresher spent a token: err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r1") {
		t.Fatal("store touched")
	}
}

func TestTokenRefresherAbandonsATickWhoseRefreshTokenChangedBeforeTheLock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r-from-old", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-old", now.Add(10*time.Minute)))
	// A login is imported between the decision and the lock.
	h.r.afterLock = func() {
		if err := os.WriteFile(p.CredPath, []byte(claudeFull("login", "r-login", now.Add(10*time.Minute))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 0 {
		t.Fatal("the replaced (older) refresh token was spent")
	}
	if !strings.Contains(h.stored(p), "r-login") || len(h.r.stateStore().journal()) != 0 || len(h.r.AccountStates()) != 0 {
		t.Fatalf("the abandoned tick must leave no trace: stored=%s journal=%v states=%v", h.stored(p), h.r.stateStore().journal(), h.r.AccountStates())
	}
	// The next tick starts from the new credential.
	h.r.afterLock = nil
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 1 {
		t.Fatalf("next tick: err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r-from-old") {
		t.Fatalf("stored = %s", h.stored(p))
	}
}

func TestTokenRefresherTerminalStateSurvivesARenameButNotALogin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) {
		return refreshRunResult{Started: true, Output: "timeout"}, errors.New("killed")
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-main", now.Add(10*time.Minute)))
	_ = h.r.RefreshDue(context.Background())
	if runner.callCount() != 1 || h.r.AccountStates()["claude/main"] != AccountNeedsReloginSoon {
		t.Fatalf("setup: calls=%d states=%v", runner.callCount(), h.r.AccountStates())
	}
	// A rename keeps the credential, so it must not hand the spent token back to
	// the refresher under a new name.
	h.mu.Lock()
	h.profiles["claude"][0].Name = "renamed"
	h.mu.Unlock()
	_ = h.r.RefreshDue(context.Background())
	if runner.callCount() != 1 {
		t.Fatalf("a rename re-spent the token: %d calls", runner.callCount())
	}
	if h.r.AccountStates()["claude/renamed"] != AccountNeedsReloginSoon {
		t.Fatalf("the state must follow the credential: %v", h.r.AccountStates())
	}
	// An imported login is a new refresh token: the state is gone.
	if err := os.WriteFile(p.CredPath, []byte(claudeFull("b", "r-imported", now.Add(10*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(h.r.AccountStates()) != 0 {
		t.Fatalf("an imported login must clear the state: %v", h.r.AccountStates())
	}
	runner.fn = rotateClaudeRunner("c", "r-after", now.Add(8*time.Hour))
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 2 {
		t.Fatalf("after the import: err=%v calls=%d", err, runner.callCount())
	}
}

func TestTokenRefresherLoginPendingInATargetIsNeverRetriedOrForced(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	h.fan.err = fmt.Errorf("live/claude: %w", ErrTargetHoldsLogin)
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatalf("a login waiting in a target is not a refresh failure: %v", err)
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatal("the store commit must stand")
	}
	if got := len(h.fan.snapshot()); got != 1 {
		t.Fatalf("fan-out was attempted %d times; it must never retry", got)
	}
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != "login_pending" {
		t.Fatalf("alerts = %v, want one login_pending and no fanout_failed", kinds)
	}
	// A later pass must not touch the target either (nothing is due).
	_ = h.r.RefreshDue(context.Background())
	if got := len(h.fan.snapshot()); got != 1 {
		t.Fatalf("fan-out calls = %d after another pass", got)
	}
}

func TestTokenRefresherStartupRunsTheReconcilePassBeforeTheDuePass(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	var order []string
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		mu.Lock()
		order = append(order, "refresh")
		mu.Unlock()
		return rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))(call, dir)
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	h.r.SetReconcile(func(context.Context) error {
		mu.Lock()
		order = append(order, "reconcile")
		mu.Unlock()
		return nil
	})
	h.r.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.r.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for runner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "reconcile" || order[1] != "refresh" {
		t.Fatalf("startup order = %v, want reconcile before refresh", order)
	}
}

func TestTokenRefresherRefreshIncomingKeepsTheCallersAccountLockAcrossTheFanOut(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "next", false, claudeFull("a", "r1", now.Add(10*time.Minute)))
	// Switch holds the incoming account's lock for the whole switch.
	release, err := HoldMaintenance(h.root, "claude", p.Name, p.CredPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	identity := maintenanceIdentity("claude", p.Name, p.CredPath) // the identity Switch locked
	heldDuringFanOut := false
	h.mgr.SetTargetFanOut(fanOutFunc(func(tool, profile string, full []byte) error {
		other, err := tryMaintenanceLock(h.root, identity)
		heldDuringFanOut = err == nil && other == nil
		if other != nil {
			other()
		}
		return nil
	}))
	if err := h.r.RefreshIncoming(context.Background(), "claude", "next"); err != nil {
		t.Fatal(err)
	}
	if !heldDuringFanOut {
		t.Fatal("the caller's account lock was not held while the credential was fanned out")
	}
	// And it is still the caller's afterwards: RefreshIncoming neither took nor dropped it.
	other, err := tryMaintenanceLock(h.root, identity)
	if err != nil || other != nil {
		if other != nil {
			other()
		}
		t.Fatalf("RefreshIncoming released the caller's lock (err=%v)", err)
	}
	release()
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatal("the incoming profile was not committed")
	}
}

type fanOutFunc func(tool, profile string, full []byte) error

func (f fanOutFunc) FanOut(tool, profile string, full []byte) error { return f(tool, profile, full) }

func TestTokenRefresherIncomingIgnoresTheSwitchAwayGrace(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", false, claudeFull("a", "r1", now.Add(10*time.Minute)))
	// A was active a moment ago (A -> B -> A): inside the grace.
	h.mu.Lock()
	away := now.Add(-30 * time.Second)
	h.profiles["claude"][0].SwitchedAwayAt = &away
	h.mu.Unlock()
	ctx := context.Background()
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 0 {
		t.Fatalf("the due pass must leave a just-switched-away profile alone: err=%v calls=%d", err, runner.callCount())
	}
	if err := h.r.RefreshIncoming(ctx, "claude", "main"); err != nil || runner.callCount() != 1 {
		t.Fatalf("an incoming profile must be refreshed if due: err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatalf("stored = %s", h.stored(p))
	}
}

func TestTokenRefresherAlertsAreRearmedWhenTheAccountIsHealthyAgain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fail := true
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		if fail {
			return refreshRunResult{Started: false}, errors.New("docker is down")
		}
		return rotateClaudeRunner("n", fmt.Sprintf("r-%d", call), now.Add(48*time.Hour))(call, dir)
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	ctx := context.Background()
	_ = h.r.RefreshDue(ctx)
	if kinds := h.stateAlertKinds(); len(kinds) != 1 || kinds[0] != AccountRefreshFailed {
		t.Fatalf("first failure alerts = %v", kinds)
	}
	// Still failing later: the same condition stays quiet.
	h.clock.now = h.clock.now.Add(6 * time.Minute)
	_ = h.r.RefreshDue(ctx)
	if kinds := h.stateAlertKinds(); len(kinds) != 1 {
		t.Fatalf("a repeat of the same failure must not alert again: %v", kinds)
	}
	// It recovers...
	fail = false
	h.clock.now = h.clock.now.Add(11 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil || !strings.Contains(h.stored(p), "r-") {
		t.Fatalf("recovery: err=%v stored=%s", err, h.stored(p))
	}
	// ...and breaks again much later: that is announced again.
	fail = true
	if err := os.WriteFile(p.CredPath, []byte(claudeFull("a", "r-late", h.clock.now.Add(10*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	h.clock.now = h.clock.now.Add(refreshMinGap + time.Minute) // past the minimum gap after the rotation
	if err := os.WriteFile(p.CredPath, []byte(claudeFull("a", "r-later", h.clock.now.Add(10*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = h.r.RefreshDue(ctx)
	kinds := h.stateAlertKinds()
	if len(kinds) != 2 || kinds[1] != AccountRefreshFailed {
		t.Fatalf("a failure after a recovery must alert again, alerts = %v", kinds)
	}
}

func TestTokenRefresherLeavesCodexAloneRightAfterMigration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{}
	h := newRefreshHarness(t, runner)
	codex := h.addProfile("codex", "work", true, codexFull(now.Add(time.Hour), "old", "refresh-old", now.Add(-time.Hour)))
	claude := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	marker := fmt.Sprintf(`{"version":1,"migrated_at":%q}`, now.Add(-3*time.Minute).UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(h.root, "token-copies.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		if _, err := os.Stat(filepath.Join(dir, "auth.json")); err == nil {
			return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(codexFull(now.Add(400*time.Hour), "new", "refresh-new", h.clock.now)), 0o600)
		}
		return rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))(call, dir)
	}
	ctx := context.Background()
	if err := h.r.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(claude), "r2") {
		t.Fatal("claude has no quiet period and must be refreshed")
	}
	if !strings.Contains(h.stored(codex), "refresh-old") {
		t.Fatal("a codex profile was refreshed 3 minutes after migration; a pre-migration session may still hold its cached token")
	}
	// Past the quiet period codex is refreshed like any other profile.
	h.clock.now = now.Add(8 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(codex), "refresh-new") {
		t.Fatalf("codex was not refreshed after the quiet period: %s", h.stored(codex))
	}
}

func TestTargetLeadsFromEnv(t *testing.T) {
	t.Setenv(TargetLeadClaudeEnv, "")
	t.Setenv(TargetLeadCodexEnv, "")
	if got := targetLeadsFromEnv(); got["claude"] != TargetLeadClaude || got["codex"] != TargetLeadCodex {
		t.Fatalf("defaults = %v", got)
	}
	t.Setenv(TargetLeadClaudeEnv, "9h")
	t.Setenv(TargetLeadCodexEnv, "36h")
	if got := targetLeadsFromEnv(); got["claude"] != 9*time.Hour || got["codex"] != 36*time.Hour {
		t.Fatalf("overrides = %v", got)
	}
	for _, bad := range []string{"soon", "5m", "-1h", "400h", "0"} {
		t.Setenv(TargetLeadClaudeEnv, bad)
		if got := targetLeadsFromEnv(); got["claude"] != TargetLeadClaude {
			t.Fatalf("%q must be ignored, got %v", bad, got["claude"])
		}
	}
}

func TestTokenRefresherLongLeadMakesAFreshProfileDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(16*time.Hour))}
	t.Setenv(TargetLeadClaudeEnv, "9h")
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(8*time.Hour)))
	if err := h.r.RefreshDue(context.Background()); err != nil || runner.callCount() != 1 {
		t.Fatalf("err=%v calls=%d: a lead longer than the token's life must make it due at once", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatal("not committed")
	}
}

func TestTokenRefresherIncomingCodexIsRefreshedEvenInTheMigrationQuietPeriod(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, "auth.json"), []byte(codexFull(now.Add(400*time.Hour), "new", "refresh-new", now)), 0o600)
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("codex", "work", false, codexFull(now.Add(time.Hour), "old", "refresh-old", now.Add(-time.Hour)))
	marker := fmt.Sprintf(`{"version":1,"migrated_at":%q}`, now.Add(-2*time.Minute).UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(h.root, "token-copies.json"), []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 0 {
		t.Fatalf("the due pass must hold codex back: err=%v calls=%d", err, runner.callCount())
	}
	if err := h.r.RefreshIncoming(ctx, "codex", "work"); err != nil || runner.callCount() != 1 {
		t.Fatalf("a switch must not be blocked by the quiet period: err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "refresh-new") {
		t.Fatalf("stored = %s", h.stored(p))
	}
}

func TestTokenRefresherRunIsIdempotent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	var mu sync.Mutex
	starts := 0
	h.r.SetReconcile(func(context.Context) error {
		mu.Lock()
		starts++
		mu.Unlock()
		return nil
	})
	h.r.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.r.Run(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for runner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 {
		t.Fatalf("startup (journal recovery + reconcile + first pass) ran %d times for 4 Run calls, want 1", starts)
	}
	if runner.callCount() != 1 {
		t.Fatalf("refresh ran %d times, want 1", runner.callCount())
	}
	if len(h.r.AccountStates()) != 0 || len(h.r.stateStore().journal()) != 0 {
		t.Fatalf("a duplicate loop left state behind: states=%v journal=%v", h.r.AccountStates(), h.r.stateStore().journal())
	}
}

func TestUsageManagerStartDoesNotStartTheTokenRefresher(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	var mu sync.Mutex
	starts := 0
	h.r.SetReconcile(func(context.Context) error {
		mu.Lock()
		starts++
		mu.Unlock()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	h.mgr.Start(ctx, &wg)
	// The wiring owns the refresher: it starts Run once, after SetReconcile.
	wg.Add(1)
	go func() { defer wg.Done(); h.r.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for runner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 || runner.callCount() != 1 {
		t.Fatalf("reconcile starts=%d refreshes=%d, want exactly one of each", starts, runner.callCount())
	}
}

func TestMigrationTimeReadsThePerToolMarkerAndFallsBackToTheLegacyField(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(h.root, "token-copies.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := h.r.migrationTime("codex"); ok {
		t.Fatal("no marker must impose no wait")
	}
	write(fmt.Sprintf(`{"version":1,"migrated_at":%q}`, now.Format(time.RFC3339)))
	if at, ok := h.r.migrationTime("codex"); !ok || !at.Equal(now) {
		t.Fatalf("legacy marker: %v %v", at, ok)
	}
	claudeAt, codexAt := now.Add(-time.Hour), now.Add(-time.Minute)
	write(fmt.Sprintf(`{"version":2,"migrated":{"claude":%q,"codex":%q}}`, claudeAt.Format(time.RFC3339), codexAt.Format(time.RFC3339)))
	if at, ok := h.r.migrationTime("codex"); !ok || !at.Equal(codexAt) {
		t.Fatalf("per-tool codex time: %v %v", at, ok)
	}
	if at, ok := h.r.migrationTime("claude"); !ok || !at.Equal(claudeAt) {
		t.Fatalf("per-tool claude time: %v %v", at, ok)
	}
	// Only claude migrated: codex is still linked and must impose no wait.
	write(fmt.Sprintf(`{"version":2,"migrated":{"claude":%q}}`, claudeAt.Format(time.RFC3339)))
	if _, ok := h.r.migrationTime("codex"); ok {
		t.Fatal("a tool that has not been migrated must impose no wait")
	}
}

// ── review round: commit retry, shutdown, rotation judgement, holders ───

// failingCopyBack makes the compare-and-swap commit fail the way a real I/O or
// lock error does, while the store still holds the spent token.
func failCommitsWith(h *refreshHarness, err error, times *int) {
	h.mgr.maintWithLock = func(fn func() error) error {
		if *times > 0 {
			*times--
			return err
		}
		return fn()
	}
}

func TestTokenRefresherACommitThatFailsWithTheSpentTokenStillStoredIsRetriedFromTheRescueNeverSpentAgain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r-new", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-spent", now.Add(10*time.Minute)))
	failures := 2
	failCommitsWith(h, errors.New("disk full"), &failures)
	ctx := context.Background()

	if err := h.r.RefreshDue(ctx); err == nil {
		t.Fatal("a failed commit must be reported")
	}
	if runner.callCount() != 1 || !strings.Contains(h.stored(p), "r-spent") {
		t.Fatalf("setup: calls=%d stored=%s", runner.callCount(), h.stored(p))
	}
	e, ok := h.r.journalFor("claude", "main", tokenHash("claude", "r-spent"))
	if !ok || e.Phase != journalRotated || e.Rescue == "" {
		t.Fatalf("the journal must stay at rotated with the rescue path, got %+v ok=%v", e, ok)
	}
	if _, err := os.Stat(e.Rescue); err != nil {
		t.Fatalf("the rescue file must stay: %v", err)
	}
	// Inside the backoff nothing runs; after it the commit is retried (and fails
	// once more), still without spending.
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 1 {
		t.Fatalf("within backoff: err=%v calls=%d", err, runner.callCount())
	}
	h.clock.now = h.clock.now.Add(6 * time.Minute)
	if err := h.r.RefreshDue(ctx); err == nil || runner.callCount() != 1 {
		t.Fatalf("second commit attempt: err=%v calls=%d (it fails once more and must not spend)", err, runner.callCount())
	}
	// The third attempt succeeds, from the rescue copy.
	h.clock.now = h.clock.now.Add(11 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("the executor ran %d times; a failed commit must be retried, never respent", runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r-new") {
		t.Fatalf("the rescued credentials were not committed: %s", h.stored(p))
	}
	if _, ok := h.r.journalFor("claude", "main", tokenHash("claude", "r-spent")); ok {
		t.Fatal("journal not cleared after the retried commit")
	}
	if _, err := os.Stat(e.Rescue); !os.IsNotExist(err) {
		t.Fatalf("rescue file must go once committed: %v", err)
	}
	if got := h.fan.snapshot(); len(got) != 1 || !strings.Contains(string(got[0].full), "r-new") {
		t.Fatalf("fan-out after the retried commit: %+v", got)
	}
}

func TestTokenRefresherACommitFailureWithoutARescueFileUsesTheInMemoryCopy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r-new", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-spent", now.Add(10*time.Minute)))
	failures := 1
	failCommitsWith(h, errors.New("lock unavailable"), &failures)
	// The rescue cannot be written: the backups path is a file.
	if err := os.WriteFile(filepath.Join(h.root, "backups"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = h.r.RefreshDue(ctx)
	h.clock.now = h.clock.now.Add(6 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 || !strings.Contains(h.stored(p), "r-new") {
		t.Fatalf("calls=%d stored=%s: the retry must use the in-memory copy", runner.callCount(), h.stored(p))
	}
}

func TestTokenRefresherACommitReachesEveryProfileHoldingTheSpentToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r-new", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	a := h.addProfile("claude", "main", true, claudeFull("a", "r-shared", now.Add(10*time.Minute)))
	b := h.addProfile("claude", "copy", false, claudeFull("a", "r-shared", now.Add(10*time.Minute)))
	other := h.addProfile("claude", "other", false, claudeFull("a", "r-other", now.Add(6*time.Hour)))
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("the shared token was spent %d times", runner.callCount())
	}
	for _, p := range []ProfileInfo{a, b} {
		if !strings.Contains(h.stored(p), "r-new") {
			t.Fatalf("%s still holds the spent token: %s", p.Name, h.stored(p))
		}
	}
	if !strings.Contains(h.stored(other), "r-other") {
		t.Fatal("an unrelated profile was touched")
	}
	if got := len(h.fan.snapshot()); got != 2 {
		t.Fatalf("fan-out calls = %d, want one per committed profile", got)
	}
}

func TestTokenRefresherAProviderThatKeepsTheRefreshTokenStillCommitsTheNewAccessToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	// Same refresh token, later expiry and a new access token.
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) {
		body := claudeFull("fresh-access", "r-same", now.Add(8*time.Hour))
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
	}}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("old-access", "r-same", now.Add(10*time.Minute)))
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(p), "fresh-access") {
		t.Fatalf("a rotation that kept the refresh token was not committed: %s", h.stored(p))
	}
}

func TestTokenRefresherRecoveryJudgesATempCopyByItsAccessTokenNotItsRefreshToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("old-access", "r-same", now.Add(10*time.Minute)))
	tmp := t.TempDir()
	// The CLI rotated the access token and kept the refresh token; the server died
	// before the rescue file was written.
	if err := os.WriteFile(filepath.Join(tmp, ".credentials.json"), []byte(claudeFull("fresh-access", "r-same", now.Add(8*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	journalKeyed(h, "claude", "main", journalStarted, "r-same", "", tmp)
	h.newRefresher().recover(context.Background())
	if !strings.Contains(h.stored(p), "fresh-access") {
		t.Fatalf("a good rotation was discarded because the refresh token was unchanged: %s", h.stored(p))
	}
	if got := h.r.AccountStates()["claude/main"]; got != "" {
		t.Fatalf("an unspent account must not be marked: %q", got)
	}
}

func TestTokenRefresherACrashBeforeTheCLIStartedIsNotAnUnknownSpend(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-spent", now.Add(10*time.Minute)))
	// Journaled "spending" and then the server died before launching anything: the
	// temp dir still holds the unrotated copy.
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, ".credentials.json"), []byte(claudeFull("a", "r-spent", now.Add(10*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	journalKeyed(h, "claude", "main", journalSpending, "r-spent", "", tmp)
	r2 := h.newRefresher()
	r2.recover(context.Background())
	if got := r2.AccountStates()["claude/main"]; got != "" {
		t.Fatalf("a refresh that never reached the CLI must not demand a login: %q", got)
	}
	if len(r2.stateStore().journal()) != 0 {
		t.Fatal("journal must be settled")
	}
	// The token was never spent, so the due pass refreshes it normally.
	if err := r2.RefreshDue(context.Background()); err != nil || runner.callCount() != 1 {
		t.Fatalf("err=%v calls=%d", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatal("not refreshed")
	}
}

func TestTokenRefresherJournalsStartedOnceTheCLIExists(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var h *refreshHarness
	phases := []string{}
	runner := &scriptedRunner{}
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		if e, ok := h.r.journalFor("claude", "main", tokenHash("claude", "r1")); ok {
			phases = append(phases, e.Phase)
		}
		return rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))(call, dir)
	}
	h = newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0] != journalStarted {
		t.Fatalf("journal phase while the CLI ran = %v, want [%s]", phases, journalStarted)
	}
}

func TestTokenRefresherAServerShutdownDoesNotKillARunningCLIAndStillCommits(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cancelled := make(chan struct{})
	var ctxErrDuringRun error
	runner := &scriptedRunner{fn: func(call int, dir string) (refreshRunResult, error) { return refreshRunResult{}, nil }}
	runner.entered = make(chan struct{}, 1)
	runner.release = make(chan struct{})
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		<-cancelled // the shutdown happens while the CLI is running
		return rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))(call, dir)
	}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	// Capture the context the executor actually runs under.
	h.r.executors["claude"] = ctxProbe{inner: runner, seen: func(err error) { ctxErrDuringRun = err }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.RefreshDue(ctx) }()
	<-runner.entered
	runner.release <- struct{}{}
	cancel()         // server shutdown
	close(cancelled) // let the CLI finish after the cancel
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if ctxErrDuringRun != nil {
		t.Fatalf("the CLI's context was cancelled by the shutdown: %v", ctxErrDuringRun)
	}
	if !strings.Contains(h.stored(p), "r2") {
		t.Fatalf("the refresh that was running at shutdown was not committed: %s", h.stored(p))
	}
	if got := h.r.AccountStates()["claude/main"]; got != "" {
		t.Fatalf("a graceful shutdown must not demand a login: %q", got)
	}
}

// ctxProbe wraps an executor and reports the state of the context it runs under
// at the moment the run finishes.
type ctxProbe struct {
	inner *scriptedRunner
	seen  func(error)
}

func (c ctxProbe) Refresh(ctx context.Context, dir, flag string) error {
	_, err := c.RunStarted(ctx, dir, flag, nil)
	return err
}

func (c ctxProbe) RunStarted(ctx context.Context, dir, flag string, onStart func()) (refreshRunResult, error) {
	if onStart != nil {
		onStart()
	}
	res, err := c.inner.Run(ctx, dir, flag)
	c.seen(ctx.Err())
	return res, err
}

func TestTokenRefresherBootstrapRunsWhenTheRefresherIsEnabledLater(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r2", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(10*time.Minute)))
	var mu sync.Mutex
	var order []string
	h.r.SetReconcile(func(context.Context) error {
		mu.Lock()
		order = append(order, "reconcile")
		mu.Unlock()
		return nil
	})
	var enabled atomic.Bool
	h.r.SetEnabled(enabled.Load)
	h.r.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.r.Run(ctx); close(done) }()
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	early := len(order)
	mu.Unlock()
	if early != 0 || runner.callCount() != 0 {
		t.Fatalf("a disabled refresher did startup work: reconciles=%d refreshes=%d", early, runner.callCount())
	}
	enabled.Store(true) // migrate-copies ran against the live server
	deadline := time.Now().Add(10 * time.Second)
	for runner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 {
		t.Fatalf("startup reconcile ran %d times after enabling, want 1", len(order))
	}
	if runner.callCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", runner.callCount())
	}
}

func TestTokenRefresherALeadLongerThanTheTokenLifeRotatesOnlyOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	t.Setenv(TargetLeadClaudeEnv, "9h")
	runner := &scriptedRunner{}
	var h *refreshHarness
	// Every rotation yields an 8-hour token, measured from the current clock.
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		body := claudeFull(fmt.Sprintf("n-%d", call), fmt.Sprintf("r-%d", call), h.clock.now.Add(8*time.Hour))
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
	}
	h = newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-0", now.Add(7*time.Hour)))
	ctx := context.Background()

	// The 9 h knob makes the fresh 8 h token due at once: one forced rotation.
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 1 {
		t.Fatalf("first pass: err=%v calls=%d", err, runner.callCount())
	}
	// Every later tick would find the new token due again under the raw lead.
	for i := 0; i < 8; i++ {
		h.clock.now = h.clock.now.Add(30 * time.Second)
		if err := h.r.RefreshDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if runner.callCount() != 1 {
		t.Fatalf("rotations over 8 ticks = %d, want exactly 1: a lead longer than the token life must not loop", runner.callCount())
	}
	// A restarted refresher reads the same record and still does not loop.
	h.newRefresher()
	h.clock.now = h.clock.now.Add(5 * time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 1 {
		t.Fatalf("after a restart: err=%v calls=%d", err, runner.callCount())
	}
	// Past the minimum gap the lead is capped at life minus the gap (7h30m for an
	// 8 h token), so the token is due again once it is 30 minutes old: the rate
	// is bounded to one rotation per gap instead of one per tick.
	h.clock.now = now.Add(refreshMinGap + time.Minute)
	if err := h.r.RefreshDue(ctx); err != nil || runner.callCount() != 2 {
		t.Fatalf("after the gap: err=%v calls=%d, want the second rotation", err, runner.callCount())
	}
	if !strings.Contains(h.stored(p), "r-2") {
		t.Fatalf("stored = %s", h.stored(p))
	}
}

func TestTokenRefresherNeverRotatesTheSameProfileTwiceInsideTheMinimumGap(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var h *refreshHarness
	runner := &scriptedRunner{}
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		// A pathological provider: the new token is already inside the lead.
		body := claudeFull(fmt.Sprintf("n-%d", call), fmt.Sprintf("r-%d", call), h.clock.now.Add(11*time.Minute))
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
	}
	h = newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r-0", now.Add(10*time.Minute)))
	ctx := context.Background()
	_ = h.r.RefreshDue(ctx)
	for i := 0; i < 20; i++ {
		h.clock.now = h.clock.now.Add(time.Minute)
		_ = h.r.RefreshDue(ctx)
	}
	if runner.callCount() != 1 {
		t.Fatalf("rotations inside %v = %d, want 1", refreshMinGap, runner.callCount())
	}
	h.clock.now = h.clock.now.Add(refreshMinGap)
	_ = h.r.RefreshDue(ctx)
	if runner.callCount() != 2 {
		t.Fatalf("after the gap the profile is eligible again: calls=%d", runner.callCount())
	}
}

func TestTokenRefresherCommitsWithoutARescueFileAndLeavesNoTempDir(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: rotateClaudeRunner("n", "r-new", now.Add(8*time.Hour))}
	h := newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-old", now.Add(10*time.Minute)))
	// A regular file where the rescue directory should be: the persist-first
	// write fails, so the commit relies on the in-memory copy and the temp dir.
	if err := os.MkdirAll(filepath.Join(h.root, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "backups", "claude"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.r.RefreshDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.stored(p), "r-new") {
		t.Fatalf("stored = %s", h.stored(p))
	}
	entries, _ := os.ReadDir(filepath.Join(h.root, "refresh-tmp"))
	if len(entries) != 0 {
		t.Fatalf("temp dirs left after a settled commit: %v", entries)
	}
}

func TestTokenRefresherNeverLeaksCLIOutputIntoStateLogsStatusOrAlerts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyLTEyMzQ1NiJ9.c2lnbmF0dXJlc2lnbmF0dXJl"
	secret := "rt-SECRETVALUE0123456789abcdef"
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) {
		out := `fatal: boom {"refresh_token":"` + secret + `"} Authorization: Bearer ` + jwt
		return refreshRunResult{Started: true, Output: out}, errors.New(out)
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "main", true, claudeFull("a", "r-old", now.Add(10*time.Minute)))

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	_ = h.r.RefreshDue(context.Background())

	state, _ := os.ReadFile(filepath.Join(h.root, "refresh-state.json"))
	rows, _ := json.Marshal(h.r.Statuses())
	alerts, _ := json.Marshal(h.r.Alerts())
	h.mu.Lock()
	hook := strings.Join(h.alerts, "\n")
	h.mu.Unlock()
	for name, text := range map[string]string{"state": string(state), "log": logs.String(), "statuses": string(rows), "alerts": string(alerts), "hook": hook} {
		for _, s := range []string{jwt, secret, "eyJhbGciOiJSUzI1NiJ9"} {
			if strings.Contains(text, s) {
				t.Errorf("%s leaks %q: %s", name, s, text)
			}
		}
	}
	if hook == "" {
		t.Fatal("the failed refresh raised no alert, so the test proved nothing")
	}
}

func TestCleanDetailTruncatesOnARuneBoundary(t *testing.T) {
	got := cleanDetail(strings.Repeat("é", 400))
	if !utf8.ValidString(got) {
		t.Fatalf("cleanDetail split a rune: %q", got)
	}
}

func TestTokenRefresherFloorDoesNotHoldBackAnImportedLogin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var h *refreshHarness
	runner := &scriptedRunner{}
	runner.fn = func(call int, dir string) (refreshRunResult, error) {
		body := claudeFull(fmt.Sprintf("n-%d", call), fmt.Sprintf("r-%d", call), h.clock.now.Add(11*time.Minute))
		return refreshRunResult{Started: true}, os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600)
	}
	h = newRefreshHarness(t, runner)
	p := h.addProfile("claude", "main", true, claudeFull("a", "r-0", now.Add(10*time.Minute)))
	ctx := context.Background()
	_ = h.r.RefreshDue(ctx)
	if runner.callCount() != 1 {
		t.Fatalf("first rotation: calls=%d", runner.callCount())
	}
	// Still the rotated token, inside the gap: held back.
	h.clock.now = h.clock.now.Add(2 * time.Minute)
	_ = h.r.RefreshDue(ctx)
	if runner.callCount() != 1 {
		t.Fatalf("the rotated token was refreshed again inside the gap: calls=%d", runner.callCount())
	}
	// A different login lands in the store, near expiry: not held back.
	if err := os.WriteFile(p.CredPath, []byte(claudeFull("b", "r-imported", h.clock.now.Add(5*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	h.clock.now = h.clock.now.Add(time.Minute)
	_ = h.r.RefreshDue(ctx)
	if runner.callCount() != 2 || !strings.Contains(h.stored(p), "r-2") {
		t.Fatalf("an imported login inside the gap must refresh: calls=%d stored=%s", runner.callCount(), h.stored(p))
	}
}
