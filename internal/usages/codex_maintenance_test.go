package usages

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

type fakeCodexExecutor struct {
	inspect func(string) error
	write   string
	flag    string
}

func (f *fakeCodexExecutor) Refresh(_ context.Context, dir, flag string) error {
	f.flag = flag
	if f.inspect != nil {
		if err := f.inspect(dir); err != nil {
			return err
		}
	}
	if f.write != "" {
		return os.WriteFile(filepath.Join(dir, "auth.json"), []byte(f.write), 0o600)
	}
	return nil
}

func codexAuth(access, refresh, last string) string {
	return `{"tokens":{"access_token":"` + access + `","refresh_token":"` + refresh + `"},"last_refresh":"` + last + `"}`
}

func testCodexMaintainer(exec refreshExecutor, now time.Time) *codexMaintainer {
	return &codexMaintainer{executor: exec, available: func() bool { return true }, reachable: func(context.Context) bool { return true }, clock: &fakeClock{now: now}, refresh: map[string]*codexMaintenanceState{}, warm: map[string]*codexMaintenanceState{}}
}

func TestCodexRefreshForceExpiresAndCopiesBack(t *testing.T) {
	now := time.Unix(9_000_000, 0).UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	exec := &fakeCodexExecutor{write: next, inspect: func(dir string) error {
		creds, _, err := loadCodexAuth(filepath.Join(dir, "auth.json"))
		if err == nil || creds.AccessToken != "" {
			t.Fatalf("temp access token was not blanked")
		}
		return nil
	}}
	m := testCodexMaintainer(exec, now)
	p := ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}
	attempted, err := m.refreshSync(context.Background(), p, "--model gpt-5.6-luna")
	if !attempted || err != nil {
		t.Fatalf("refresh = %v %v", attempted, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != next {
		t.Fatalf("copy-back=%s", got)
	}
	if exec.flag != "--model gpt-5.6-luna" {
		t.Fatalf("flag=%q", exec.flag)
	}
}

func TestCodexRefreshCASRejectsChangedProfile(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	changed := codexAuth("external", "r3", now.Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Add(time.Minute).Format(time.RFC3339))
	os.WriteFile(path, []byte(old), 0o600)
	exec := &fakeCodexExecutor{write: next, inspect: func(string) error { return os.WriteFile(path, []byte(changed), 0o600) }}
	m := testCodexMaintainer(exec, now)
	_, err := m.refreshSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("err=%v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != changed {
		t.Fatal("mid-flight credentials overwritten")
	}
}

func TestCodexWarmupCopiesRotatedCredentialsBack(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	old := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
	next := codexAuth("new", "r2", now.Format(time.RFC3339))
	os.WriteFile(path, []byte(old), 0o600)
	exec := &fakeCodexExecutor{write: next}
	m := testCodexMaintainer(exec, now)
	attempted, err := m.warmSync(context.Background(), ProfileInfo{Tool: "codex", Name: "work", CredPath: path, HasCredentials: true}, DefaultCodexModelFlag)
	if !attempted || err != nil {
		t.Fatalf("warm=%v %v", attempted, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != next {
		t.Fatalf("copy-back=%s", got)
	}
}

func TestCodexProfileNeedsRefreshUsesAccessJWTExpiry(t *testing.T) {
	now := time.Now().UTC()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10) + `}`))
	token := "x." + payload + ".y"
	expiry := CodexTokenExpiry(token)
	if expiry == nil {
		t.Fatal("missing expiry")
	}
	p := ProfileInfo{Tool: "codex", HasCredentials: true, ExpiresAt: expiry}
	if !codexProfileNeedsRefresh(p, now) {
		t.Fatal("near-expiry JWT was not eligible")
	}
}

func TestCodexRefreshArgs(t *testing.T) {
	args := codexRefreshArgs("/tmp/p", "--model gpt-5.6-luna")
	want := []string{"codex", "exec", "--skip-git-repo-check", codexMaintenancePrompt, "--model", "gpt-5.6-luna"}
	if got := args[len(args)-len(want):]; !reflect.DeepEqual(got, want) {
		t.Fatalf("tail=%#v", got)
	}
}
