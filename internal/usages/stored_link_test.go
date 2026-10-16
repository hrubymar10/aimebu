package usages

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The switcher keeps the active profile's stored credential file as a symlink
// to the live credential file. Copy-back must rotate the live file through the
// link and keep the link; executors must only ever see a temp dir holding the
// resolved bytes as a regular file.

// linkedProfile creates a live credential file and a profile dir whose
// credential file is an absolute symlink to it. Returns (livePath, linkPath).
func linkedProfile(t *testing.T, filename, body string) (string, string) {
	t.Helper()
	liveDir := t.TempDir()
	livePath := filepath.Join(liveDir, filename)
	if err := os.WriteFile(livePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	profileDir := t.TempDir()
	linkPath := filepath.Join(profileDir, filename)
	if err := os.Symlink(livePath, linkPath); err != nil {
		t.Fatal(err)
	}
	return livePath, linkPath
}

func assertStillLinked(t *testing.T, linkPath, livePath, want string) {
	t.Helper()
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("copy-back replaced the symlink with a regular file (mode=%v)", info.Mode())
	}
	if target, _ := os.Readlink(linkPath); target != livePath {
		t.Fatalf("link target = %q, want %q", target, livePath)
	}
	got, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("live file = %s, want %s", got, want)
	}
	liveInfo, err := os.Stat(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if liveInfo.Mode().Perm() != 0o600 {
		t.Fatalf("live mode = %v, want 0600", liveInfo.Mode().Perm())
	}
	// No temp litter next to the link: the temp file belongs in the target dir.
	entries, err := os.ReadDir(filepath.Dir(linkPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("profile dir should hold only the link, got %d entries", len(entries))
	}
}

func requireRegularCredInDir(dir, filename, want string) error {
	info, err := os.Lstat(filepath.Join(dir, filename))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("executor dir holds a non-regular %s (mode=%v)", filename, info.Mode())
	}
	if want == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return err
	}
	if string(data) != want {
		return fmt.Errorf("executor dir %s = %s, want %s", filename, data, want)
	}
	return nil
}

// The switcher links the active profile's stored file to the live credentials.
// Maintenance never starts on it (the flag says active, and the link alone is
// enough even if the flag is stale), and never runs the executor.
func TestMaintenanceRefusesLinkedActiveProfile(t *testing.T) {
	now := time.Unix(6_000_000, 0).UTC()
	for _, flagged := range []bool{true, false} {
		old := claudeCredsWithExpiry("old-tok", now.Add(-time.Minute).UnixMilli())
		rotated := claudeCredsWithExpiry("new-tok", now.Add(11*time.Hour).UnixMilli())
		livePath, linkPath := linkedProfile(t, ".credentials.json", old)

		exec := &fakeExecutor{writeNew: rotated}
		profile := ProfileInfo{Tool: "claude", Name: "work", Active: flagged, CredPath: linkPath, HasCredentials: true, ExpiresAt: ptrTime(now.Add(-time.Minute))}
		if _, err := newTestRefresher(exec, &fakeClock{now: now}).maybeRefreshSync(context.Background(), profile); !errors.Is(err, errMaintenanceActiveProfile) {
			t.Fatalf("claude refresh (flagged=%v) = %v", flagged, err)
		}
		if _, err := newTestWarmer(exec, &fakeClock{now: now}).maybeWarmSync(context.Background(), profile); !errors.Is(err, errMaintenanceActiveProfile) {
			t.Fatalf("claude warmup (flagged=%v) = %v", flagged, err)
		}
		assertStillLinked(t, linkPath, livePath, old)

		codexOld := codexAuth("old", "r1", now.Add(-time.Hour).Format(time.RFC3339))
		codexLive, codexLink := linkedProfile(t, "auth.json", codexOld)
		cexec := &fakeCodexExecutor{write: codexAuth("new", "r2", now.Format(time.RFC3339))}
		cp := ProfileInfo{Tool: "codex", Name: "work", Active: flagged, CredPath: codexLink, HasCredentials: true}
		if _, err := testCodexMaintainer(cexec, now).refreshSync(context.Background(), cp, ""); !errors.Is(err, errMaintenanceActiveProfile) {
			t.Fatalf("codex refresh (flagged=%v) = %v", flagged, err)
		}
		if _, err := testCodexMaintainer(cexec, now).warmSync(context.Background(), cp, ""); !errors.Is(err, errMaintenanceActiveProfile) {
			t.Fatalf("codex warmup (flagged=%v) = %v", flagged, err)
		}
		assertStillLinked(t, codexLink, codexLive, codexOld)
		if exec.callCount() != 0 || cexec.flag != "" {
			t.Fatalf("an executor ran for a linked profile (claude calls=%d)", exec.callCount())
		}
	}
}

// A copy-back that finds the profile linked to live (a switch landed during
// the run) while live still holds the token the run spent writes the rotated
// token through the link and keeps the link.
func TestCopyBackWritesThroughLinkWhenLiveHoldsStartToken(t *testing.T) {
	start := claudeCredsWithExpiry("start", 1000)
	rotated := claudeCredsWithExpiry("rotated", 9000)
	livePath, linkPath := linkedProfile(t, ".credentials.json", start)
	if err := commitClaudeCredCopyBack(nil, nil, "work", linkPath, []byte(start), []byte(rotated), t.TempDir()); err != nil {
		t.Fatalf("copy-back: %v", err)
	}
	assertStillLinked(t, linkPath, livePath, rotated)
}

// TestCopyBackCASReadsThroughSymlink: the fingerprint guard compares against
// the bytes behind the link, so a live rotation that landed mid-run aborts.
func TestCopyBackCASReadsThroughSymlink(t *testing.T) {
	start := claudeCredsWithExpiry("start", 1000)
	changed := `{"claudeAiOauth":{"accessToken":"harness-rotated","refreshToken":"different","expiresAt":2000}}`
	livePath, linkPath := linkedProfile(t, ".credentials.json", start)
	if err := os.WriteFile(livePath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	err := commitClaudeCredCopyBack(nil, nil, "work", linkPath, []byte(start), []byte(start), t.TempDir())
	if !errors.Is(err, errClaudeRefreshProfileChanged) {
		t.Fatalf("err = %v, want errClaudeRefreshProfileChanged", err)
	}
	assertStillLinked(t, linkPath, livePath, changed)
}

func TestCopyBackDanglingSymlinkIsProfileGone(t *testing.T) {
	started := codexAuth("start", "r1", "2026-01-01T00:00:00Z")
	rotated := codexAuth("next", "r2", "2026-01-02T00:00:00Z")
	livePath, linkPath := linkedProfile(t, "auth.json", started)
	if err := os.Remove(livePath); err != nil {
		t.Fatal(err)
	}
	err := commitCodexAuthCopyBack(nil, nil, "work", linkPath, []byte(started), []byte(rotated), t.TempDir())
	if !errors.Is(err, errClaudeRefreshProfileGone) {
		t.Fatalf("err = %v, want errClaudeRefreshProfileGone", err)
	}
	if _, err := os.Stat(livePath); !os.IsNotExist(err) {
		t.Fatalf("dangling copy-back must not recreate the live file; err=%v", err)
	}
	if err := writeStoredCredential(linkPath, []byte("x"), 0o600); err == nil {
		t.Fatal("writeStoredCredential through a dangling link should fail")
	}
}

func TestWriteStoredCredentialRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeStoredCredential(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("got %s", got)
	}
}
