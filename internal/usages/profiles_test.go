package usages

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/usages/providers"
)

// profileRig is a Reader wired to fake credential files and a fake usage
// endpoint that reports a different utilization per bearer token.
type profileRig struct {
	t         *testing.T
	r         *Reader
	live      string
	stored    string
	mu        sync.Mutex
	active    string // which profile the lister reports as active: "main" or "work"
	requests  []string
	onRequest func(token string)
	locks     int
	lockErr   error
	fail      int // HTTP status to answer with for the stored token, 0 = healthy
}

func claudeCreds(token string, expiresAt time.Time) []byte {
	data, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "expiresAt": expiresAt.UnixMilli(), "subscriptionType": "max"}})
	return data
}

func newProfileRig(t *testing.T) *profileRig {
	t.Helper()
	t.Setenv("PATH", t.TempDir()) // no `claude --version` probe
	dir := t.TempDir()
	rig := &profileRig{t: t, live: filepath.Join(dir, "live.json"), stored: filepath.Join(dir, "work.json"), active: "main"}
	valid := time.Now().Add(time.Hour)
	rig.write(rig.live, claudeCreds("live-token", valid))
	rig.write(rig.stored, claudeCreds("stored-token", valid))
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		rig.mu.Lock()
		rig.requests = append(rig.requests, token)
		hook := rig.onRequest
		rig.mu.Unlock()
		if hook != nil {
			hook(token)
		}
		rig.mu.Lock()
		fail := rig.fail
		rig.mu.Unlock()
		if fail != 0 && token == "stored-token" {
			w.WriteHeader(fail)
			return
		}
		used := map[string]int{"live-token": 10, "stored-token": 70}[token]
		fmt.Fprintf(w, `{"five_hour":{"utilization":%d,"resets_at":%q}}`, used, reset)
	}))
	t.Cleanup(endpoint.Close)
	r, err := New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	r.claudePath, r.codexPath = rig.live, filepath.Join(dir, "absent-codex.json")
	r.claudeURL = endpoint.URL
	r.SetProfiles(func() ([]StoredProfile, error) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return []StoredProfile{
			{Tool: "claude", Name: "main", Active: rig.active == "main", CredPath: filepath.Join(dir, "main.json"), HasCredentials: true},
			{Tool: "claude", Name: "work", Active: rig.active == "work", CredPath: rig.stored, HasCredentials: true, Email: "w@example.test"},
		}, nil
	}, func(fn func() error) error {
		rig.mu.Lock()
		rig.locks++
		failure := rig.lockErr
		rig.mu.Unlock()
		if failure != nil {
			return failure
		}
		return fn()
	})
	rig.r = r
	return rig
}

func (g *profileRig) write(path string, data []byte) {
	g.t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		g.t.Fatal(err)
	}
}

func (g *profileRig) tokens() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

func TestInactiveProfileGetsReadOnlyUsage(t *testing.T) {
	g := newProfileRig(t)
	var pushed int
	g.r.SetUpdateHook(func() { pushed++ })
	beforeInfo, _ := os.Stat(g.stored)
	beforeBytes, _ := os.ReadFile(g.stored)
	g.r.Refresh(context.Background())

	work, ok := g.r.StoredProfile("claude-code", "work")
	if !ok || work.Status != "ok" || len(work.Windows) != 1 || work.Windows[0].PercentUsed != 70 {
		t.Fatalf("stored profile = %+v ok=%v", work, ok)
	}
	if work.Active {
		t.Fatal("stored profile must not be marked active")
	}
	if g.locks == 0 {
		t.Fatal("stored credentials must be read under the switcher lock")
	}
	got := strings.Join(g.tokens(), ",")
	if !strings.Contains(got, "live-token") || !strings.Contains(got, "stored-token") {
		t.Fatalf("requests = %s", got)
	}
	afterInfo, _ := os.Stat(g.stored)
	afterBytes, _ := os.ReadFile(g.stored)
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || string(afterBytes) != string(beforeBytes) {
		t.Fatal("a stored credential file was modified")
	}
	cache, _ := os.ReadFile(g.r.cachePath)
	if strings.Contains(string(cache), "stored-token") || strings.Contains(string(cache), "live-token") {
		t.Fatal("cache contains a token")
	}
	if !strings.Contains(string(cache), `"profile_name":"work"`) {
		t.Fatal("the stored profile's snapshot is not persisted for the CLI")
	}
	if pushed == 0 {
		t.Fatal("a stored snapshot change must be pushed to live listeners")
	}
	// A restart restores it, marked stale until the first refresh.
	restored, err := New(filepath.Dir(g.r.cachePath))
	if err != nil {
		t.Fatal(err)
	}
	if again, ok := restored.StoredProfile("claude-code", "work"); !ok || !again.Stale || len(again.Windows) != 1 {
		t.Fatalf("restored = %+v ok=%v", again, ok)
	}
}

func TestStoredProfileWithTheActiveLoginIsSkipped(t *testing.T) {
	g := newProfileRig(t)
	g.write(g.stored, claudeCreds("live-token", time.Now().Add(time.Hour)))
	// Same bytes as live: point stored at an identical copy.
	live, _ := os.ReadFile(g.live)
	g.write(g.stored, live)
	g.r.Refresh(context.Background())
	work, _ := g.r.StoredProfile("claude-code", "work")
	if work.Status != "not_selected" || !strings.Contains(work.Error, "Same login") {
		t.Fatalf("duplicate login = %+v", work)
	}
	for _, token := range g.tokens() {
		if token != "live-token" {
			t.Fatalf("unexpected request with %q", token)
		}
	}
	if n := len(g.tokens()); n != 1 {
		t.Fatalf("the shared login must be requested once (active only), got %d", n)
	}
}

func TestExpiredStoredTokenIsNotSentAndKeepsLastSnapshot(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	g.write(g.stored, claudeCreds("stored-token", time.Now().Add(-time.Hour)))
	before := len(g.tokens())
	g.r.Refresh(context.Background())
	work, _ := g.r.StoredProfile("claude-code", "work")
	if work.Status != "token_expired" || !work.Stale || len(work.Windows) != 1 {
		t.Fatalf("expired profile = %+v", work)
	}
	for _, token := range g.tokens()[before:] {
		if token == "stored-token" {
			t.Fatal("an expired stored token was sent")
		}
	}
}

func TestStoredResultIsDroppedWhenItsCopyChangesMidFetch(t *testing.T) {
	g := newProfileRig(t)
	g.mu.Lock()
	g.onRequest = func(token string) {
		if token == "stored-token" {
			g.write(g.stored, claudeCreds("rotated-token", time.Now().Add(time.Hour)))
		}
	}
	g.mu.Unlock()
	g.r.Refresh(context.Background())
	if _, ok := g.r.StoredProfile("claude-code", "work"); ok {
		t.Fatal("a result for credentials replaced mid-fetch was stored")
	}
}

func TestStoredResultIsDroppedWhenTheProfileBecomesActive(t *testing.T) {
	g := newProfileRig(t)
	g.mu.Lock()
	g.onRequest = func(token string) {
		if token == "stored-token" {
			g.mu.Lock()
			g.active = "work"
			g.mu.Unlock()
		}
	}
	g.mu.Unlock()
	g.r.refreshStored(context.Background(), 1)
	if _, ok := g.r.StoredProfile("claude-code", "work"); ok {
		t.Fatal("a profile that became active kept its stored snapshot")
	}
}

func TestActiveResultIsDroppedWhenLiveCredentialsChangeMidFetch(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	if p := g.r.Snapshot().Providers[1].Profiles[0]; p.Windows[0].PercentUsed != 10 {
		t.Fatalf("baseline = %+v", p)
	}
	g.mu.Lock()
	g.onRequest = func(token string) {
		if token == "live-token" {
			g.write(g.live, claudeCreds("switched-token", time.Now().Add(time.Hour)))
		}
	}
	g.mu.Unlock()
	g.r.refreshLocalOne(context.Background(), 1)
	if p := g.r.Snapshot().Providers[1].Profiles[0]; p.Windows[0].PercentUsed != 10 || p.Stale {
		t.Fatalf("a late result for a replaced login overwrote the snapshot: %+v", p)
	}
	if !g.r.liveChanged(1) {
		t.Fatal("the poller must notice the live credentials changed")
	}
}

func TestRemovedProfilesLoseTheirSnapshot(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	if _, ok := g.r.StoredProfile("claude-code", "work"); !ok {
		t.Fatal("missing snapshot")
	}
	g.r.SetProfiles(func() ([]StoredProfile, error) {
		return []StoredProfile{{Tool: "claude", Name: "main", Active: true, HasCredentials: true}}, nil
	}, nil)
	g.r.refreshStored(context.Background(), 1)
	if _, ok := g.r.StoredProfile("claude-code", "work"); ok {
		t.Fatal("a removed profile kept its snapshot")
	}
}

func TestStoredProfileBacksOffExponentiallyAndRecovers(t *testing.T) {
	g := newProfileRig(t)
	now := time.Now()
	g.r.now = func() time.Time { return now }
	g.r.Refresh(context.Background())
	count := func() int {
		n := 0
		for _, token := range g.tokens() {
			if token == "stored-token" {
				n++
			}
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("baseline requests = %d", count())
	}

	g.mu.Lock()
	g.fail = http.StatusTooManyRequests
	g.mu.Unlock()
	// First failure: marked backing off, last snapshot kept stale.
	g.r.refreshStored(context.Background(), 1)
	work, _ := g.r.StoredProfile("claude-code", "work")
	if work.Status != "backing_off" || !work.Stale || len(work.Windows) != 1 || !strings.Contains(work.Error, "retrying after") {
		t.Fatalf("after first failure = %+v", work)
	}
	after := count()
	if after < 2 {
		t.Fatalf("the failing request was never sent: %d", after)
	}

	// Inside the window nothing is sent, however often the poller ticks.
	for i := 0; i < 5; i++ {
		now = now.Add(20 * time.Second)
		g.r.refreshStored(context.Background(), 1)
	}
	if count() != after {
		t.Fatalf("requests during backoff: %d -> %d", after, count())
	}

	// Past the first window it tries again; a second failure doubles the delay.
	now = now.Add(backoffBase)
	g.r.refreshStored(context.Background(), 1)
	if count() <= after {
		t.Fatal("no retry after the backoff elapsed")
	}
	if got := g.r.backoff["claude-code\x00work"].until.Sub(now); got != 2*backoffBase {
		t.Fatalf("second delay = %s, want %s", got, 2*backoffBase)
	}

	// The delay is capped.
	if backoffDelay(50) != backoffMax || backoffDelay(1) != backoffBase || backoffDelay(3) != 4*backoffBase {
		t.Fatalf("delays: %s %s %s", backoffDelay(1), backoffDelay(3), backoffDelay(50))
	}

	// Recovery clears the state and the snapshot is fresh again.
	g.mu.Lock()
	g.fail = 0
	g.mu.Unlock()
	now = now.Add(backoffMax)
	g.r.refreshStored(context.Background(), 1)
	work, _ = g.r.StoredProfile("claude-code", "work")
	if work.Status != "ok" || work.Stale {
		t.Fatalf("after recovery = %+v", work)
	}
	if _, waiting := g.r.backoff["claude-code\x00work"]; waiting {
		t.Fatal("backoff survived a successful fetch")
	}
}

func TestForgetProfileDropsSnapshotAndBackoffForAnyProfile(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	g.r.mu.Lock()
	g.r.backoff["claude-code\x00work"] = backoffState{fails: 2, until: time.Now().Add(time.Hour)}
	g.r.mu.Unlock()
	g.r.ForgetProfile("claude-code", "work")
	if _, ok := g.r.StoredProfile("claude-code", "work"); ok {
		t.Fatal("a removed inactive profile kept its snapshot")
	}
	if _, waiting := g.r.backoff["claude-code\x00work"]; waiting {
		t.Fatal("a removed profile kept its backoff")
	}
	if p := g.r.Snapshot().Providers[1].Profiles[0]; p.Status != "ok" {
		t.Fatalf("the live snapshot must be untouched: %+v", p)
	}
}

func TestStoredResultIsDroppedWhenTheProviderGenerationMoves(t *testing.T) {
	g := newProfileRig(t)
	g.mu.Lock()
	g.onRequest = func(token string) {
		if token == "stored-token" {
			g.r.ForgetProfile("claude-code", "someone-else")
		}
	}
	g.mu.Unlock()
	g.r.refreshStored(context.Background(), 1)
	if _, ok := g.r.StoredProfile("claude-code", "work"); ok {
		t.Fatal("a stored result survived a provider-level change that happened mid-fetch")
	}
}

func TestLockOrReadFailureIsTransientNotAuthMissing(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	g.mu.Lock()
	g.lockErr = errors.New("acquire switcher lock: busy")
	g.mu.Unlock()
	g.r.refreshStored(context.Background(), 1)
	work, _ := g.r.StoredProfile("claude-code", "work")
	if work.Status != "read_error" || !work.Stale || len(work.Windows) != 1 {
		t.Fatalf("a failed read must keep the snapshot and report a transient status: %+v", work)
	}
	if strings.Contains(work.Error, "busy") {
		t.Fatalf("internal error text leaked: %q", work.Error)
	}
	if _, waiting := g.r.backoff["claude-code\x00work"]; waiting {
		t.Fatal("a read failure must not trigger the endpoint backoff")
	}
}

func TestLiveChangeRepollsOnlyTheActiveSnapshot(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	before := len(g.tokens())
	g.write(g.live, claudeCreds("live-token", time.Now().Add(2*time.Hour))) // re-login: new bytes
	g.r.pollLiveChanges(context.Background())
	for _, token := range g.tokens()[before:] {
		if token == "stored-token" {
			t.Fatal("a live-file change re-polled a stored profile")
		}
	}
	if len(g.tokens()) == before {
		t.Fatal("the changed live login was not re-polled")
	}
	// A disabled provider does not tick.
	if _, err := g.r.extra.UpdateConfig(func(c *providers.Config) error {
		pc := c.Providers["claude-code"]
		pc.Enabled = false
		c.Providers["claude-code"] = pc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	g.write(g.live, claudeCreds("live-token", time.Now().Add(3*time.Hour)))
	if g.r.liveChanged(1) {
		t.Fatal("a disabled provider must not report live changes")
	}
}

func TestUnreadableConfigFallbackCollapsesAllProfiles(t *testing.T) {
	g := newProfileRig(t)
	g.r.Refresh(context.Background())
	g.r.mu.Lock()
	g.r.response.Providers[1].Enabled = false
	g.r.mu.Unlock()
	if err := os.WriteFile(g.r.ConfigPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := g.r.Snapshot().Providers[1].Profiles
	if len(got) != 1 || got[0].Status != "disabled" {
		t.Fatalf("a disabled provider must collapse to one disabled profile, got %+v", got)
	}
}
