package usages

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// tokenOnlyClaude is a live-file style document: no refresh token.
func tokenOnlyClaude(access string, expires time.Time) string {
	return `{"claudeAiOauth":{"accessToken":"` + access + `","expiresAt":` + itoa(expires.UnixMilli()) + `},"mcpOAuth":{"x":1}}`
}

func itoa(n int64) string { return strconv.Itoa(int(n)) }

func writeLiveClaude(t *testing.T, body string) string {
	t.Helper()
	path, err := LiveCredentialPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return path
}

func TestStatusesAndAlertsJudgeStoreAndLiveCredentials(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	runner := &scriptedRunner{fn: func(int, string) (refreshRunResult, error) {
		return refreshRunResult{Started: true, Output: "timeout"}, errors.New("killed")
	}}
	h := newRefreshHarness(t, runner)
	h.addProfile("claude", "healthy", false, claudeFull("a", "r-ok", now.Add(6*time.Hour)))
	h.addProfile("claude", "main", true, claudeFull("a", "r-main", now.Add(10*time.Minute)))
	h.addProfile("claude", "lapsed", false, claudeFull("a", "r-lapsed", now.Add(-time.Hour)))
	writeLiveClaude(t, tokenOnlyClaude("live", now.Add(5*time.Minute)))

	// Two profiles are due; their refresh ends with an unknown outcome.
	_ = h.r.RefreshDue(context.Background())

	rows := h.r.Statuses()
	byKey := map[string]TargetStatus{}
	for _, r := range rows {
		byKey[r.Tool+"/"+r.Role+"/"+r.Profile] = r
	}
	if got := byKey["claude/store/healthy"]; got.Level != TargetLevelOK || got.State != "" {
		t.Fatalf("healthy = %+v", got)
	}
	if got := byKey["claude/store/main"]; got.Level != TargetLevelExpiring || got.State != AccountNeedsReloginSoon || !got.Active {
		t.Fatalf("main = %+v", got)
	}
	if got := byKey["claude/store/lapsed"]; got.Level != TargetLevelExpired || got.State != AccountNeedsReloginSoon {
		t.Fatalf("lapsed = %+v", got)
	}
	live := byKey["claude/live/"]
	if live.Level != TargetLevelExpiring || live.SecondsLeft == nil || *live.SecondsLeft != 300 {
		t.Fatalf("live = %+v (a token-only live file is judged by its access token)", live)
	}

	kinds := map[string]int{}
	for _, a := range h.r.Alerts() {
		kinds[a.Kind]++
	}
	if kinds[AccountNeedsReloginSoon] != 2 || kinds["expiring"] != 2 || kinds["expired"] != 1 {
		t.Fatalf("alert kinds = %v", kinds)
	}
	for _, a := range h.r.Alerts() {
		if a.Profile == "healthy" {
			t.Fatalf("a healthy profile must not alert: %+v", a)
		}
	}
	raw, _ := json.Marshal(rows)
	for _, secret := range []string{"r-main", "r-lapsed", "r-ok", "accessToken", "refreshToken"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("a status row leaked %q: %s", secret, raw)
		}
	}
}

func TestEmitExpiryAlertsFiresOncePerCredentialAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	live := writeLiveClaude(t, tokenOnlyClaude("live", now.Add(10*time.Minute)))
	h.r.emitExpiryAlerts()
	h.r.emitExpiryAlerts()
	if kinds := h.alertKinds(); len(kinds) != 1 || kinds[0] != TargetLevelExpiring {
		t.Fatalf("alerts = %v, want exactly one expiring edge", kinds)
	}
	// The server rewrites the file; later it runs low again: a new edge.
	if err := os.WriteFile(live, []byte(tokenOnlyClaude("live2", now.Add(8*time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	h.r.emitExpiryAlerts()
	h.clock.now = now.Add(7*time.Hour + 50*time.Minute)
	h.r.emitExpiryAlerts()
	if kinds := h.alertKinds(); len(kinds) != 2 {
		t.Fatalf("alerts = %v, want a second edge for the new expiry", kinds)
	}
}

func TestCredentialAlertsReachTheUsagesResponseOnlyWhenTheRefresherIsOn(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	writeLiveClaude(t, tokenOnlyClaude("live", now.Add(2*time.Minute)))
	enabled := false
	h.r.SetEnabled(func() bool { return enabled })

	resp := h.mgr.assembleResponse(Config{}, Cache{})
	if len(resp.CredentialAlerts) != 0 {
		t.Fatalf("a disabled refresher must not add alerts: %+v", resp.CredentialAlerts)
	}
	enabled = true
	resp = h.mgr.assembleResponse(Config{}, Cache{})
	if len(resp.CredentialAlerts) != 1 || resp.CredentialAlerts[0].Kind != "expiring" || resp.CredentialAlerts[0].Role != "live" {
		t.Fatalf("alerts = %+v", resp.CredentialAlerts)
	}
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"credential_alerts"`) {
		t.Fatalf("response lacks credential_alerts: %s", raw)
	}
	clean := UsagesResponse{}
	raw, _ = json.Marshal(clean)
	if strings.Contains(string(raw), "credential_alerts") {
		t.Fatalf("an empty alert list must be omitted: %s", raw)
	}
}

func TestCredentialTargetsEndpoint(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	h.addProfile("claude", "main", true, claudeFull("a", "r1", now.Add(6*time.Hour)))
	mux := http.NewServeMux()
	Routes{Manager: h.mgr}.Mount(mux)

	call := func() map[string]any {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/credentials/targets", nil))
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "r1") || strings.Contains(rec.Body.String(), "accessToken") {
			t.Fatalf("the endpoint must carry state only, never a token: %s", rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	h.r.SetEnabled(func() bool { return false })
	if body := call(); body["enabled"] != false || len(body["targets"].([]any)) != 0 {
		t.Fatalf("disabled body = %v", body)
	}
	h.r.SetEnabled(func() bool { return true })
	body := call()
	targets := body["targets"].([]any)
	if body["enabled"] != true || len(targets) == 0 {
		t.Fatalf("enabled body = %v", body)
	}
	first := targets[0].(map[string]any)
	if first["role"] != "store" || first["profile"] != "main" || first["level"] != "ok" {
		t.Fatalf("first target = %v", first)
	}
}

func TestCredentialReportReadsFilesOnlyAndMatchesStateToTheCurrentToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "state.json"), `{"version":1,"enabled":true,"active":{"claude":"main"}}`)
	write(filepath.Join(root, "profiles", "claude", "main", ".credentials.json"), claudeFull("a", "r-main", now.Add(3*time.Hour)))
	write(filepath.Join(root, "profiles", "claude", "dead", ".credentials.json"), claudeFull("a", "r-dead", now.Add(-time.Minute)))
	store := newRefreshStateStore(root)
	store.setAccount(refreshAccountEntry{Tool: "claude", Name: "dead", State: AccountReauthRequired, SpentHash: tokenHash("claude", "r-dead"), Since: now})
	// A stale entry for a token the profile no longer holds must not apply.
	store.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountReauthRequired, SpentHash: tokenHash("claude", "an-older-token"), Since: now})

	rows := CredentialReport(root, now)
	got := map[string]TargetStatus{}
	for _, r := range rows {
		got[r.Profile] = r
	}
	if main := got["main"]; main.State != "" || !main.Active || main.Level != TargetLevelOK {
		t.Fatalf("main = %+v", main)
	}
	if dead := got["dead"]; dead.State != AccountReauthRequired || dead.Level != TargetLevelExpired {
		t.Fatalf("dead = %+v", dead)
	}
}

// A row's Detail is CLI output and error text. It is kept for the owner (state
// file, log, doctor) but the endpoint and the alert banner any client can read
// never carry it.
func TestCredentialTargetsEndpointNeverCarriesDetail(t *testing.T) {
	const secret = "SECRET-cli-echo-0123456789"
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	h.addProfile("claude", "main", true, claudeFull("a", "r-main", now.Add(6*time.Hour)))
	h.r.stateStore().setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountNeedsReloginSoon, Detail: "the CLI said refresh_token=" + secret, SpentHash: tokenHash("claude", "r-main"), Since: now})
	h.r.accounts["claude/"+tokenHash("claude", "r-main")] = &targetAccount{detail: "commit aborted: " + secret}

	mux := http.NewServeMux()
	Routes{Manager: h.mgr}.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/credentials/targets", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), `"detail"`) {
		t.Fatalf("the endpoint carries detail text: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), AccountNeedsReloginSoon) {
		t.Fatalf("the state itself is still reported: %s", rec.Body)
	}

	// The doctor reads files directly for the owner and still gets the detail.
	var found bool
	for _, row := range h.r.Statuses() {
		if row.Profile == "main" && strings.Contains(row.Detail, secret) {
			found = true
		}
	}
	if !found {
		t.Fatal("Statuses keeps Detail for in-process callers")
	}
	for _, alert := range h.r.Alerts() {
		if strings.Contains(alert.Message, secret) {
			t.Fatalf("an alert message carries detail text: %+v", alert)
		}
	}
}
