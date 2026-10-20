package usages

import (
	"context"
	"errors"
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
	path, err := testLivePath("claude")
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

	rows := CredentialReport(root, now, testLivePath)
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

// testLivePath is the live credential file under the isolated test HOME.
func testLivePath(tool string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if tool == "codex" {
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return filepath.Join(dir, "auth.json"), nil
		}
		return filepath.Join(home, ".codex", "auth.json"), nil
	}
	return ClaudeLiveCredPath(home), nil
}

// The status rows and alerts carry no CLI output: Detail stays on the row for
// in-process callers (the owner's doctor) and never reaches an alert message.
func TestAlertMessagesNeverCarryDetailText(t *testing.T) {
	const secret = "SECRET-cli-echo-0123456789"
	now := time.Unix(1_800_000_000, 0)
	h := newRefreshHarness(t, &scriptedRunner{fn: func(int, string) (refreshRunResult, error) { return refreshRunResult{Started: true}, nil }})
	h.addProfile("claude", "main", true, claudeFull("a", "r-main", now.Add(6*time.Hour)))
	h.r.stateStore().setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountNeedsReloginSoon, Detail: "the CLI said refresh_token=" + secret, SpentHash: tokenHash("claude", "r-main"), Since: now})
	h.r.accounts["claude/"+tokenHash("claude", "r-main")] = &targetAccount{detail: "commit aborted: " + secret}

	var found bool
	for _, row := range h.r.Statuses() {
		if row.Profile == "main" && strings.Contains(row.Detail, secret) {
			found = true
		}
	}
	if !found {
		t.Fatal("Statuses keeps Detail for in-process callers")
	}
	alerts := h.r.Alerts()
	if len(alerts) == 0 {
		t.Fatal("the needs_relogin_soon state raises an alert")
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Message, secret) {
			t.Fatalf("an alert message carries detail text: %+v", alert)
		}
	}
}
