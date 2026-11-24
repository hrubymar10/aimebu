package usages

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/usages/providers"
)

func TestRefreshReadsCredentialsWithoutWritingThem(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "claude.json")
	codexPath := filepath.Join(dir, "codex.json")
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	expiry := time.Now().Add(time.Hour).UnixMilli()
	claude, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "claude-token", "expiresAt": expiry}})
	if err := os.WriteFile(claudePath, claude, 0400); err != nil {
		t.Fatal(err)
	}
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"chatgpt_account_id":"acct-stable"}`)) + ".signature"
	codex, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": "codex-token", "id_token": idToken}})
	if err := os.WriteFile(codexPath, codex, 0400); err != nil {
		t.Fatal(err)
	}
	beforeClaude, _ := os.Stat(claudePath)
	beforeCodex, _ := os.Stat(codexPath)
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path == "/codex" {
			if r.Header.Get("Authorization") != "Bearer codex-token" {
				t.Errorf("Codex bearer missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"plan_type": "pro", "rate_limit": map[string]any{"primary_window": map[string]any{"used_percent": 40, "reset_at": time.Now().Add(2 * time.Hour).Unix(), "limit_window_seconds": 5 * 3600}}})
		} else {
			if r.Header.Get("Authorization") != "Bearer claude-token" {
				t.Errorf("Claude bearer missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 25, "resets_at": reset}})
		}
	}))
	defer endpoint.Close()
	r, err := New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	r.claudePath, r.codexPath = claudePath, codexPath
	r.claudeURL, r.codexURL = endpoint.URL+"/claude", endpoint.URL+"/codex"
	r.Refresh(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	got := r.Snapshot()
	for i := 0; i < 2; i++ {
		p := got.Providers[i].Profiles[0]
		if p.Status != "ok" || len(p.Windows) != 1 || p.Windows[0].Pace == nil {
			t.Errorf("provider %d: %+v", i, p)
		}
		want := []string{alertHash("account\x00acct-stable"), ""}[i]
		if p.AlertCredential != want {
			t.Errorf("provider %d credential fingerprint = %q, want %q", i, p.AlertCredential, want)
		}
	}
	for _, item := range []struct {
		path   string
		before os.FileInfo
	}{{claudePath, beforeClaude}, {codexPath, beforeCodex}} {
		after, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if !after.ModTime().Equal(item.before.ModTime()) || after.Mode() != item.before.Mode() {
			t.Errorf("credential changed: %s", item.path)
		}
	}
	data, err := os.ReadFile(r.cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "claude-token") || strings.Contains(string(data), "codex-token") {
		t.Fatal("cache contains token")
	}
}

func TestExpiredTokenKeepsLastSnapshotWithoutNetworkOrCredentialWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	data, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": token}})
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	r, err := New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	r.codexPath = path
	r.claudePath = filepath.Join(dir, "absent")
	r.response.Providers[0].Profiles[0].Windows = []Window{{Key: "session", PercentUsed: 42}}
	r.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired token sent over network")
		return nil, nil
	})}
	r.Refresh(context.Background())
	p := r.Snapshot().Providers[0].Profiles[0]
	if p.Status != "token_expired" || !p.Stale || len(p.Windows) != 1 || p.Windows[0].PercentUsed != 42 {
		t.Fatalf("profile = %+v", p)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("credential modified")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newFetcherReader returns a Reader pointed at a fake usage endpoint serving
// claudeBody at /claude and codexBody at /codex with the given status.
func newFetcherReader(t *testing.T, status int, claudeBody, codexBody string) *Reader {
	t.Helper()
	dir := t.TempDir()
	claudePath, codexPath := filepath.Join(dir, "claude.json"), filepath.Join(dir, "codex.json")
	expiry := time.Now().Add(time.Hour).UnixMilli()
	claude, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "claude-secret", "expiresAt": expiry, "subscriptionType": "max"}})
	if err := os.WriteFile(claudePath, claude, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(`{"tokens":{"access_token":"codex-secret","account_id":"acct"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if r.URL.Path == "/codex" {
			_, _ = w.Write([]byte(codexBody))
		} else {
			_, _ = w.Write([]byte(claudeBody))
		}
	}))
	t.Cleanup(endpoint.Close)
	r, err := New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	r.claudePath, r.codexPath = claudePath, codexPath
	r.claudeURL, r.codexURL = endpoint.URL+"/claude", endpoint.URL+"/codex"
	return r
}

func TestRefreshPublishesScopedLimitsCreditsAndSparkWindows(t *testing.T) {
	reset := time.Now().Add(48 * time.Hour).UTC()
	soon := time.Now().Add(2 * time.Hour).UTC()
	claude := `{"five_hour":{"utilization":10,"resets_at":"` + reset.Format(time.RFC3339) + `"},
"limits":[{"kind":"weekly_scoped","group":"weekly","percent":33,"resets_at":"` + reset.Format(time.RFC3339) + `","scope":{"model":{"display_name":"Fable"}}}],
"extra_usage":{"is_enabled":true,"used_credits":1234,"monthly_limit":5000}}`
	codex := `{"plan_type":"prolite","credits":{"balance":"7.5"},
"rate_limit":{"primary_window":{"used_percent":20,"reset_at":` + strconv.FormatInt(soon.Unix(), 10) + `,"limit_window_seconds":18000}},
"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"primary_window":{"used_percent":5,"reset_at":` + strconv.FormatInt(reset.Unix(), 10) + `,"limit_window_seconds":18000}}}]}`
	r := newFetcherReader(t, http.StatusOK, claude, codex)
	r.Refresh(context.Background())
	got := r.Snapshot()
	codexProfile, claudeProfile := got.Providers[0].Profiles[0], got.Providers[1].Profiles[0]
	keys := func(p Profile) string {
		var out []string
		for _, w := range p.Windows {
			out = append(out, w.Key)
		}
		return strings.Join(out, ",")
	}
	if claudeProfile.Status != "ok" || keys(claudeProfile) != "session,weekly_scoped:fable" || claudeProfile.Plan != "max" {
		t.Fatalf("claude = %+v (%s)", claudeProfile, keys(claudeProfile))
	}
	if c := claudeProfile.Credits; c == nil || c.Balance != 12.34 || c.SpendLimit != 50 {
		t.Fatalf("claude credits = %+v", c)
	}
	if codexProfile.Status != "ok" || keys(codexProfile) != "session,codex_spark" || codexProfile.Plan != "Pro 5x" {
		t.Fatalf("codex = %+v (%s)", codexProfile, keys(codexProfile))
	}
	if c := codexProfile.Credits; c == nil || c.Balance != 7.5 {
		t.Fatalf("codex credits = %+v", c)
	}
	if codexProfile.Windows[0].Pace == nil {
		t.Fatalf("codex session window lost its pace: %+v", codexProfile.Windows[0])
	}
}

func TestTransientFetchFailureKeepsSnapshotAndRedactsSecrets(t *testing.T) {
	r := newFetcherReader(t, http.StatusInternalServerError, `{"message":"claude-secret leaked"}`, `{"message":"codex-secret leaked"}`)
	for i := 0; i < 2; i++ {
		r.response.Providers[i].Profiles[0].Windows = []Window{{Key: "session", PercentUsed: 42}}
		r.response.Providers[i].Profiles[0].Credits = &Credits{Label: "Credits", Balance: 9.5}
	}
	r.Refresh(context.Background())
	for i, p := range r.Snapshot().Providers[:2] {
		profile := p.Profiles[0]
		if profile.Credits == nil || profile.Credits.Balance != 9.5 {
			t.Errorf("provider %d lost its credits on a transient failure: %+v", i, profile.Credits)
		}
		if profile.Status != "fetch_error" || !profile.Stale || len(profile.Windows) != 1 || profile.Windows[0].PercentUsed != 42 {
			t.Errorf("provider %d: %+v", i, profile)
		}
	}
	data, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := os.ReadFile(r.cachePath)
	for _, body := range []string{string(data), string(cache)} {
		if strings.Contains(body, "claude-secret") || strings.Contains(body, "codex-secret") {
			t.Fatalf("secret leaked: %s", body)
		}
	}
}

// The snapshot marks a window whose series has the gap a rejected login
// left: the first rejection of the profile's current or last episode, when
// it is still inside the history span. A profile without an episode, and
// one whose episode is older than the span, carry nothing — an ordinary gap
// (the server being down) is never marked.
func TestSnapshotMarksARejectedSeriesWithinTheSpan(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.response.Providers[0].Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "ok", Windows: []Window{{Key: "session", PercentUsed: 10}}}
	r.response.Providers[1].Profiles[0] = Profile{ProfileName: "local", Active: true, Status: "ok", Windows: []Window{{Key: "session", PercentUsed: 10}}}
	rejection := now.Add(-2 * time.Hour)
	r.SetRejectionTimes(func() map[string]time.Time { return map[string]time.Time{"codex\x00local": rejection} })
	out := r.Snapshot()
	if got := out.Providers[0].Profiles[0].Windows[0].RejectedAt; got != rejection.Unix() {
		t.Fatalf("rejected_at = %d, want the episode's first-seen time", got)
	}
	if got := out.Providers[1].Profiles[0].Windows[0].RejectedAt; got != 0 {
		t.Fatalf("a profile without an episode is not marked: %d", got)
	}
	r.SetRejectionTimes(func() map[string]time.Time {
		return map[string]time.Time{"codex\x00local": now.Add(-historySpan - time.Hour)}
	})
	if got := r.Snapshot().Providers[0].Profiles[0].Windows[0].RejectedAt; got != 0 {
		t.Fatalf("an episode older than the history span is not marked: %d", got)
	}
	r.SetRejectionTimes(nil)
	if got := r.Snapshot().Providers[0].Profiles[0].Windows[0].RejectedAt; got != 0 {
		t.Fatalf("without a source nothing is marked: %d", got)
	}
	// The same, fed by a real alert tracker: the Observe that started the
	// episode recorded its first-seen time, and the snapshot reads it.
	tracker, err := NewAlertTracker(t.TempDir(), func() time.Time { return rejection })
	if err != nil {
		t.Fatal(err)
	}
	rejected := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true,
		Profiles: []Profile{{ProfileName: "local", Active: true, AlertIdentity: "ident\x00local", Status: "auth_missing", Error: "rejected"}}}}}
	rejected.Settings.AlertWarningRemaining, rejected.Settings.AlertCriticalRemaining = 20, 10
	if _, err := tracker.Observe(rejected); err != nil {
		t.Fatal(err)
	}
	r.SetRejectionTimes(tracker.RejectionTimes)
	if got := r.Snapshot().Providers[0].Profiles[0].Windows[0].RejectedAt; got != rejection.Unix() {
		t.Fatalf("rejected_at from the tracker = %d, want the episode's first-seen time", got)
	}
}

func TestRejectedTokenReportsAuthMissingWithoutStaleSnapshot(t *testing.T) {
	r := newFetcherReader(t, http.StatusUnauthorized, `{"error":"x"}`, `{"error":"x"}`)
	for i := 0; i < 2; i++ {
		r.response.Providers[i].Profiles[0].Windows = []Window{{Key: "session", PercentUsed: 42}}
	}
	r.Refresh(context.Background())
	tails := []string{tokenTail("codex-secret"), tokenTail("claude-secret")}
	for i, p := range r.Snapshot().Providers[:2] {
		profile := p.Profiles[0]
		if profile.Status != "auth_missing" || profile.Stale || len(profile.Windows) != 0 || profile.Error == "" {
			t.Errorf("provider %d: %+v", i, profile)
		}
		if profile.TokenTail != tails[i] {
			t.Errorf("provider %d tail = %q, want the hash tail of the rejected credential", i, profile.TokenTail)
		}
		if strings.HasPrefix(tails[i], "codex-s") || strings.HasPrefix(tails[i], "claud") {
			t.Errorf("the tail must never be a prefix of the credential: %q", tails[i])
		}
	}
	// The tail is in memory only: neither the snapshot nor the cache file
	// carries it.
	data, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := os.ReadFile(r.cachePath)
	for _, body := range []string{string(data), string(cache)} {
		for _, tail := range tails {
			if strings.Contains(body, tail) {
				t.Fatalf("the tail leaked into the persisted output: %s", body)
			}
		}
	}
}

type leakyProvider struct{ message string }

func (leakyProvider) Key() string { return "codex" }
func (p leakyProvider) Fetch(context.Context, *providers.Store) (providers.Snapshot, error) {
	return providers.Snapshot{Status: providers.StatusFetchError, Error: p.message}, errors.New(p.message)
}

type alertUsageProvider struct{}

func (alertUsageProvider) Key() string { return providers.ProviderCodex }
func (alertUsageProvider) Fetch(context.Context, *providers.Store) (providers.Snapshot, error) {
	return providers.Snapshot{Status: providers.StatusOK, Windows: []providers.Window{{Key: "session", PercentUsed: 81}}}, nil
}

func TestLocalAlertIgnoresRotatedAccessToken(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := NewAlertTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker.clock = func() time.Time { return now }
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"same-account"}`)) + ".signature"
	observe := func(access string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": access, "id_token": idToken}})
		login, err := providers.ParseCodexLogin(data)
		if err != nil {
			t.Fatal(err)
		}
		profile, err := r.fetchLocal(context.Background(), Profile{ProfileName: "local"}, login, "Codex", alertUsageProvider{}, func() (providers.Login, error) { return login, nil })
		if err != nil {
			t.Fatal(err)
		}
		profile.LastRefreshAt = &now
		response := Response{Providers: []Provider{{ProviderName: "codex", Enabled: true, Available: true, Profiles: []Profile{profile}}}}
		response.Settings.AlertWarningRemaining, response.Settings.AlertCriticalRemaining = 20, 10
		if _, err := tracker.Observe(response); err != nil {
			t.Fatal(err)
		}
	}
	observe("token-one")
	first := tracker.OpenAlerts()
	if len(first) != 1 {
		t.Fatalf("first alert: %+v", first)
	}
	now = now.Add(time.Minute)
	observe("token-two")
	second := tracker.OpenAlerts()
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("rotation rearmed alert: %+v", second)
	}
	if err := tracker.Dismiss(first[0].ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	observe("token-three")
	if alerts := tracker.OpenAlerts(); len(alerts) != 0 {
		t.Fatalf("rotation revived dismissed alert: %+v", alerts)
	}
}

func TestCodexAlertIdentityIgnoresAccessTokenClaim(t *testing.T) {
	access := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"chatgpt_account_id":"access-only"}`)) + ".signature"
	data, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": access}})
	login, err := providers.ParseCodexLogin(data)
	if err != nil {
		t.Fatal(err)
	}
	if login.StableAccountID != "" {
		t.Fatalf("access-token claim used for alert identity: %q", login.StableAccountID)
	}
	data, _ = json.Marshal(map[string]any{"tokens": map[string]any{"access_token": access, "account_id": "file-account"}})
	login, err = providers.ParseCodexLogin(data)
	if err != nil {
		t.Fatal(err)
	}
	if login.StableAccountID != "file-account" {
		t.Fatalf("plain account ID fallback = %q", login.StableAccountID)
	}
}

// A provider may retry with a token the harness rotated into the file after
// the reader first looked; error text naming either token must be redacted.
func TestFetchLocalRedactsTheRereadToken(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := leakyProvider{message: "upstream echoed old-token and rotated-token"}
	reread := func() (providers.Login, error) { return providers.Login{AccessToken: "rotated-token"}, nil }
	p, _ := r.fetchLocal(context.Background(), Profile{ProfileName: "local"}, providers.Login{AccessToken: "old-token"}, "Codex", provider, reread)
	if strings.Contains(p.Error, "old-token") || strings.Contains(p.Error, "rotated-token") {
		t.Fatalf("token leaked: %q", p.Error)
	}
	if p.AlertCredential != "" {
		t.Fatalf("token-only login must not make an alert fingerprint: %q", p.AlertCredential)
	}
	if data, err := json.Marshal(p); err != nil || strings.Contains(string(data), "old-token") {
		t.Fatalf("credential leaked in profile JSON: %s %v", data, err)
	}
}
