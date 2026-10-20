package switcher

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func targetClaude() string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"live-access","expiresAt":%d}}`, time.Now().Add(time.Hour).UnixMilli())
}

func targetCodex() string {
	return fmt.Sprintf(`{"auth_mode":"chatgptAuthTokens","tokens":{"access_token":"live-access","refresh_token":"","id_token":%q}}`,
		makeTestJWT("a@example.test", time.Now().Add(time.Hour).Unix()))
}

// A token-only live file (the refresher's copy) must never overwrite a stored
// full credential: every mutating call refuses, and the stored file, backups,
// state and live file stay byte-identical.
func TestTokenOnlyLiveFileIsNeverCapturedOrTouched(t *testing.T) {
	for _, tc := range []struct {
		tool        string
		full, token string
	}{
		{ToolClaude, futureClaude("full-a"), targetClaude()},
		{ToolCodex, futureCodex("a@example.test"), targetCodex()},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			m, home := newTestManager(t)
			must(t, m.SetEnabled(true))
			live := writeLive(t, home, tc.tool, tc.full)
			must(t, m.Import(tc.tool, "main"))
			must(t, m.Add(tc.tool, "other"))
			// The refresher's migration: stored profile is a plain full file,
			// live becomes token-only.
			stored := m.profileCredPath(tc.tool, "main")
			must(t, os.Remove(stored))
			must(t, os.WriteFile(stored, []byte(tc.full), 0o600))
			must(t, os.Remove(live))
			must(t, os.WriteFile(live, []byte(tc.token), 0o600))
			liveInfo, err := os.Stat(live)
			must(t, err)
			beforeState := readFile(t, m.statePath())

			if el, err := m.eligibilityFor(tc.tool); err != nil || el.Eligible || !el.TargetCopy || el.Reason == "corrupt" {
				t.Fatalf("eligibility = %+v, %v; want a TargetCopy refusal", el, err)
			}
			for name, call := range map[string]func() error{
				"switch": func() error { _, _, err := m.Switch(tc.tool, "other"); return err },
				"import": func() error { return m.Import(tc.tool, "adopted") },
				"rename": func() error { return m.Rename(tc.tool, "other", "renamed") },
				"remove": func() error { return m.Remove(tc.tool, "other", false) },
				"force":  func() error { return m.Remove(tc.tool, "main", true) },
				"link":   func() error { return m.EnsureActiveLinks() },
			} {
				err := call()
				if name == "link" {
					if err != nil {
						t.Fatalf("EnsureActiveLinks: %v", err)
					}
				} else if !errors.Is(err, ErrTargetCopy) {
					t.Fatalf("%s: err = %v, want ErrTargetCopy", name, err)
				}
			}
			if got := readFile(t, stored); got != tc.full {
				t.Fatalf("stored full credential changed: %q", got)
			}
			if info, err := os.Lstat(stored); err != nil || info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("stored file must stay a regular file: %v %v", info, err)
			}
			if got := readFile(t, live); got != tc.token {
				t.Fatalf("live file changed: %q", got)
			}
			if info, err := os.Stat(live); err != nil || !info.ModTime().Equal(liveInfo.ModTime()) {
				t.Fatalf("live mtime changed")
			}
			if got := readFile(t, m.statePath()); got != beforeState {
				t.Fatalf("state changed: %q", got)
			}
			if _, err := os.Stat(m.profileDir(tc.tool, "adopted")); err == nil {
				t.Fatal("refused import still created a profile")
			}
		})
	}
}

// A full live credential is not a target copy, so existing behaviour holds.
func TestFullLiveFileIsNotATargetCopy(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("full"))
	writeLive(t, home, ToolCodex, futureCodex("a@example.test"))
	for _, tool := range Tools() {
		el, err := m.eligibilityFor(tool)
		if err != nil || !el.Eligible || el.TargetCopy {
			t.Fatalf("%s: %+v %v", tool, el, err)
		}
	}
}
