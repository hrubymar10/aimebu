package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

var refreshTokenInFile = regexp.MustCompile(`"refreshToken"\s*:\s*"([^"]+)"`)

func startupClaudeCreds(access, refresh string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference"],"subscriptionType":"max"}}`, access, refresh, expires.UnixMilli())
}

// startupCLI stands in for `claude -p` run against a throwaway credential dir:
// it records which refresh token it was handed and writes the rotated credential
// the real CLI would. Everything is fake and lives under t.TempDir().
type startupCLI struct {
	mu    sync.Mutex
	spent map[string]int
}

func (c *startupCLI) Refresh(_ context.Context, dir, _ string) error {
	path := filepath.Join(dir, ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m := refreshTokenInFile.FindSubmatch(data)
	if m == nil {
		return fmt.Errorf("fake cli: no refresh token in the temp copy")
	}
	c.mu.Lock()
	if c.spent == nil {
		c.spent = map[string]int{}
	}
	c.spent[string(m[1])]++
	n := len(c.spent)
	c.mu.Unlock()
	return os.WriteFile(path, []byte(startupClaudeCreds(fmt.Sprintf("acc-r%d", n), fmt.Sprintf("ref-r%d", n), time.Now().Add(8*time.Hour))), 0o600)
}

func (c *startupCLI) calls() (total int, twice []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for token, n := range c.spent {
		total += n
		if n > 1 {
			twice = append(twice, token)
		}
	}
	return total, twice
}

// startupEnv is an isolated home with a switcher and one imported claude profile
// "A" holding a full credential that expires in `left`. Nothing touches the real
// ~/.claude, ~/.codex or ~/.aimebu*.
func startupEnv(t *testing.T, left time.Duration) (f *fixture, m *switcher.Manager, live string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	t.Setenv("AIMEBU2_CONFIG_DIR", filepath.Join(root, ".aimebu2"))
	f = newFixture(t)
	m, err := switcher.New(filepath.Join(root, ".aimebu2", "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(root)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	live = filepath.Join(root, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(startupClaudeCreds("acc-A0", "ref-A0", time.Now().Add(left))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	f.srv.Switcher = m
	return f, m, live
}

func migrateClaude(t *testing.T, m *switcher.Manager) {
	t.Helper()
	if _, err := m.MigrateToTokenCopies(switcher.MigrateOptions{
		Tools: []string{switcher.ToolClaude},
		Now:   func() time.Time { return time.Now().Add(-time.Hour) }, // the 15-minute guard is not under test
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// The real startup: somebody must start the refresher, and only once. A due
// profile is refreshed exactly once, a second Run and a second start are
// no-ops, and nothing keeps spending afterwards.
func TestStartTokenCopiesStartsTheRefresherExactlyOnce(t *testing.T) {
	f, m, live := startupEnv(t, 20*time.Minute) // inside the 60 min claude lead: due
	migrateClaude(t, m)

	cli := &startupCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetExecutor("codex", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	if tr == nil {
		t.Fatal("StartTokenCopies did not build a refresher")
	}
	if again := f.srv.StartTokenCopies(ctx); again != tr {
		t.Fatal("a second start must return the same refresher, not build another owner")
	}

	deadline := time.Now().Add(20 * time.Second)
	for total, _ := cli.calls(); total == 0 && time.Now().Before(deadline); total, _ = cli.calls() {
		time.Sleep(20 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { tr.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second Run must return immediately")
	}
	time.Sleep(1500 * time.Millisecond)
	if total, twice := cli.calls(); total != 1 || len(twice) != 0 {
		t.Fatalf("the due profile was handed to the CLI %d times (spent twice: %v), want exactly once", total, twice)
	}
	if data, err := os.ReadFile(live); err != nil || refreshTokenInFile.Match(data) || !strings.Contains(string(data), "acc-r1") {
		t.Fatalf("live file after the startup refresh: %v %s", err, data)
	}
	if stored, err := os.ReadFile(m.StoredCredPath(switcher.ToolClaude, "A")); err != nil || !strings.Contains(string(stored), "ref-r1") {
		t.Fatalf("the store must hold the rotated full credential: %v %s", err, stored)
	}
}

func TestStartTokenCopiesRefusesAnotherRootForTheSameTool(t *testing.T) {
	f, m, _ := startupEnv(t, 20*time.Minute)
	migrateClaude(t, m)
	// Another refresher process, in any root, holds the claim on the live file.
	claimPath, err := m.LiveClaimPath(switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := os.OpenFile(claimPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err := syscall.Flock(int(foreign.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	cli := &startupCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	if err := tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if total, _ := cli.calls(); total != 0 {
		t.Fatalf("conflicting refresher spent %d token(s)", total)
	}
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "B"), []byte(startupClaudeCreds("acc-B0", "ref-B0", time.Now().Add(20*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Switch(switcher.ToolClaude, "B"); err == nil || !strings.Contains(err.Error(), "refresher blocked") {
		t.Fatalf("switch to a due profile must refuse conflicting ownership: %v", err)
	}
	// The conflict is part of the credential targets response, not only a one-off system event.
	resp := f.request("GET", "/api/credentials/targets", nil, false)
	targets, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(targets), `"kind":"refresher_conflict"`) || !strings.Contains(string(targets), `"tool":"claude"`) {
		t.Fatalf("targets lack the conflict alert: HTTP %d, %v, %s", resp.StatusCode, err, targets)
	}
	resp = f.request("GET", "/api/system-events", nil, false)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "refresher_conflict") {
		t.Fatalf("missing conflict alert: HTTP %d, %v, %s", resp.StatusCode, err, body)
	}
	// The check is continuous: once the other refresher is gone, the next pass takes the tool over.
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	f.srv.liveClaims.Refresh()
	if reason := f.srv.liveClaims.Blocked(switcher.ToolClaude); reason != "" {
		t.Fatalf("claim not taken over after the other refresher exited: %s", reason)
	}
}

// Switch hands the refresher the incoming profile (caller holds the account lock).
func TestStartTokenCopiesInstallsTheSwitchRefreshHook(t *testing.T) {
	f, m, _ := startupEnv(t, 6*time.Hour)
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "B"), []byte(startupClaudeCreds("acc-B0", "ref-B0", time.Now().Add(20*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateClaude(t, m)

	cli := &startupCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
		tr.SetEnabled(func() bool { return false }) // the loop stays idle; only Switch may spend
	})
	if _, _, err := m.Switch(switcher.ToolClaude, "B"); err != nil {
		t.Fatalf("switch to a due profile: %v", err)
	}
	if total, twice := cli.calls(); total != 1 || len(twice) != 0 {
		t.Fatalf("Switch refreshed the incoming profile %d times (twice: %v), want once", total, twice)
	}
	if stored, _ := os.ReadFile(m.StoredCredPath(switcher.ToolClaude, "B")); !strings.Contains(string(stored), "ref-r1") {
		t.Fatalf("the incoming profile was not rotated in the store: %s", stored)
	}
}

func TestCredentialTargetsEndpoint(t *testing.T) {
	f, m, _ := startupEnv(t, 6*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetPreflight(func() bool { return false }, func(context.Context) bool { return false }) // nothing may spend here
	})

	get := func() map[string]any {
		t.Helper()
		resp := f.request("GET", "/api/credentials/targets", nil, false)
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "ref-A0") || strings.Contains(string(raw), "acc-A0") || strings.Contains(string(raw), `"detail"`) {
			t.Fatalf("the endpoint must carry state only, never a token or detail text: %s", raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	// Not migrated yet: the refresher is off, so the endpoint says so.
	if body := get(); body["enabled"] != false || len(body["targets"].([]any)) != 0 {
		t.Fatalf("before migration: %v", body)
	}
	migrateClaude(t, m)
	body := get()
	targets, _ := body["targets"].([]any)
	if body["enabled"] != true || len(targets) == 0 {
		t.Fatalf("after migration: %v", body)
	}
	first := targets[0].(map[string]any)
	if first["tool"] != "claude" || first["level"] != "ok" {
		t.Fatalf("first target = %v", first)
	}
	// Anonymous callers get nothing.
	anon, err := http.Get(f.http.URL + "/api/credentials/targets")
	if err != nil {
		t.Fatal(err)
	}
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Fatalf("anonymous status = %d, want 401", anon.StatusCode)
	}
}

func TestCredentialTargetsEndpointNeverCarriesDetail(t *testing.T) {
	const secret = "SECRET-cli-echo-0123456789"
	f, m, _ := startupEnv(t, 20*time.Minute)
	migrateClaude(t, m)
	cli := failingCLI{secret: secret}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	// The failed refresh parks the account; its Detail (kept for in-process
	// callers) holds the CLI's echo, which the endpoint must not.
	var held bool
	for deadline := time.Now().Add(20 * time.Second); !held && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, row := range tr.Statuses() {
			held = held || row.State == usages.AccountNeedsReloginSoon
		}
	}
	if !held {
		t.Fatal("the failed refresh never parked the account; the test would prove nothing")
	}
	resp := f.request("GET", "/api/credentials/targets", nil, false)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"detail"`) {
		t.Fatalf("the endpoint carries detail text: %d %s", resp.StatusCode, raw)
	}
}

// failingCLI fails after the process started, echoing a secret the way a real
// CLI's error output can.
type failingCLI struct{ secret string }

func (c failingCLI) Refresh(context.Context, string, string) error {
	return fmt.Errorf("the CLI said refresh_token=%s", c.secret)
}

// credential_alerts reach the usages response only while the refresher is on, and
// never carry CLI text.
func TestUsagesResponseCarriesCredentialAlertsOnlyWhenTheRefresherIsOn(t *testing.T) {
	f, m, live := startupEnv(t, 6*time.Hour)
	reader, err := usages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Usages = reader
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetPreflight(func() bool { return false }, func(context.Context) bool { return false }) // nothing may spend
	})
	alerts := func() []usages.CredentialAlert { return reader.Snapshot().CredentialAlerts }
	if got := alerts(); len(got) != 0 {
		t.Fatalf("a refresher that is off must add no alerts: %+v", got)
	}
	migrateClaude(t, m)
	// The live token is close to expiry: one `expiring` alert for the live file.
	if err := os.WriteFile(live, []byte(startupClaudeCreds("acc-live", "", time.Now().Add(5*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	got := alerts()
	var live1 bool
	for _, a := range got {
		live1 = live1 || (a.Role == "live" && a.Kind == "expiring")
	}
	if !live1 {
		t.Fatalf("alerts = %+v, want an expiring alert for the live file", got)
	}
	raw, _ := json.Marshal(reader.Snapshot())
	if !strings.Contains(string(raw), `"credential_alerts"`) {
		t.Fatalf("the response lacks credential_alerts: %s", raw)
	}
}

// blockingCLI is a refresh executor that signals when it has started and finishes only when released.
type blockingCLI struct {
	started chan struct{}
	release chan struct{}
	inner   startupCLI
}

func (c *blockingCLI) Refresh(ctx context.Context, dir, flag string) error {
	close(c.started)
	<-c.release
	return c.inner.Refresh(ctx, dir, flag)
}

// A spend ignores the cancellation of the server context once its CLI has started, so the claim on the live
// credential file must be held until it has committed: a second refresher that could take the claim during the spend
// would spend the same login again.
func TestCancelDuringASpendKeepsTheClaimUntilItCommits(t *testing.T) {
	f, m, live := startupEnv(t, 20*time.Minute)
	migrateClaude(t, m)
	claimPath, err := m.LiveClaimPath(switcher.ToolClaude)
	if err != nil {
		t.Fatal(err)
	}
	cli := &blockingCLI{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	if tr == nil {
		t.Fatal("no refresher")
	}
	refreshed := make(chan error, 1)
	go func() { refreshed <- tr.RefreshDue(ctx) }()
	select {
	case <-cli.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never started its CLI")
	}
	cancel()
	// Whatever the claim goroutine does with the cancellation, a second claimant must not get the file yet.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		other, err := os.OpenFile(claimPath, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			other.Close()
			t.Fatal("another refresher took the live-file claim while a spend was still running after the cancel")
		}
		other.Close()
		time.Sleep(20 * time.Millisecond)
	}
	joined := make(chan struct{})
	go func() { f.srv.WaitTokenCopies(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("WaitTokenCopies returned while a spend was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(cli.release)
	select {
	case err := <-refreshed:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("refresh: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never finished after its CLI was released")
	}
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitTokenCopies did not return after the spend committed")
	}
	// The spend committed (the login rotated) and only then was the claim given up.
	if data, err := os.ReadFile(live); err != nil || !strings.Contains(string(data), "acc-r1") {
		t.Fatalf("the spend did not commit its rotated access token to the live file: %v %s", err, data)
	}
	other, err := os.OpenFile(claimPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the claim was not released after the spend: %v", err)
	}
}

// A refresher that predates the claim lock holds no claim, but its root's token-copy marker still says it refreshes the
// same live file. That older check stays as a second block next to the claim: the tool is not ours to refresh, the
// conflict is reported, and it clears once the other root is no longer migrated.
func TestAnOlderRefresherRootBlocksTheToolEvenWithoutAClaim(t *testing.T) {
	f, m, _ := startupEnv(t, 20*time.Minute)
	home, err := m.ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(home, ".aimebu", "switcher")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(otherRoot, "token-copies.json")
	if err := os.WriteFile(marker, []byte(`{"version":2,"migrated":{"claude":"2026-10-17T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateClaude(t, m)
	cli := &startupCLI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := f.srv.StartTokenCopies(ctx, func(tr *usages.TargetRefresher) {
		tr.SetExecutor("claude", cli)
		tr.SetPreflight(func() bool { return true }, func(context.Context) bool { return true })
	})
	if tr == nil {
		t.Fatal("no refresher")
	}
	if err := tr.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}
	if total, _ := cli.calls(); total != 0 {
		t.Fatalf("a tool another root's refresher owns was refreshed (%d spend)", total)
	}
	if reason := f.srv.refresherBlocked(switcher.ToolClaude); !strings.Contains(reason, "may own the same live credentials") || !strings.Contains(reason, otherRoot) {
		t.Fatalf("the older-root conflict is not reported as a block: %q", reason)
	}
	resp := f.request("GET", "/api/credentials/targets", nil, false)
	targets, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(targets), `"kind":"refresher_conflict"`) {
		t.Fatalf("targets lack the conflict alert: HTTP %d, %v, %s", resp.StatusCode, err, targets)
	}
}
