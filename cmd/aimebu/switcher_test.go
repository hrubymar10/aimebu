package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/switcher"
)

// ── Fake ─────────────────────────────────────────────────────────────

type switcherFake struct {
	enabled     bool
	profiles    []switcher.Profile
	eligibility []switcher.Eligibility
	active      map[string]string // tool → profile name

	enabledErr    error
	setEnabledErr error
	eligErr       error
	listErr       error
	activeErr     error
	addErr        error
	importErr     error
	renameErr     error
	// removeErr controls Remove(force=false); removeForceErr controls Remove(force=true)
	removeErr      error
	removeForceErr error
	switchErr      error
	// switchResult is the returned active profile name on success
	switchResult string
	// switchSwitched controls the bool returned by Switch; nil defaults to
	// true so existing tests (which expect a real switch) are unchanged. Set
	// false to exercise the already-active no-op branch.
	switchSwitched *bool
}

func (f *switcherFake) Enabled() (bool, error) { return f.enabled, f.enabledErr }
func (f *switcherFake) SetEnabled(v bool) error {
	if f.setEnabledErr != nil {
		return f.setEnabledErr
	}
	f.enabled = v
	return nil
}
func (f *switcherFake) Eligibility() ([]switcher.Eligibility, error) {
	return f.eligibility, f.eligErr
}
func (f *switcherFake) List() ([]switcher.Profile, error)          { return f.profiles, f.listErr }
func (f *switcherFake) Active(tool string) (string, error)         { return f.active[tool], f.activeErr }
func (f *switcherFake) Add(tool, name string) error                { return f.addErr }
func (f *switcherFake) Import(tool, name string) error             { return f.importErr }
func (f *switcherFake) Rename(tool, oldName, newName string) error { return f.renameErr }
func (f *switcherFake) Remove(tool, name string, force bool) error {
	if force {
		return f.removeForceErr
	}
	return f.removeErr
}
func (f *switcherFake) Switch(tool, name string) (string, bool, error) {
	switched := true
	if f.switchSwitched != nil {
		switched = *f.switchSwitched
	}
	return f.switchResult, switched, f.switchErr
}

// ── list ─────────────────────────────────────────────────────────────

func TestSwitcherListEmpty(t *testing.T) {
	var buf bytes.Buffer
	fake := &switcherFake{}
	if err := switcherList(&buf, fake, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No profiles") {
		t.Errorf("expected empty message, got: %q", buf.String())
	}
}

func TestSwitcherListProfiles(t *testing.T) {
	exp := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	fake := &switcherFake{profiles: []switcher.Profile{
		{Tool: "claude", Name: "personal", Active: true, HasCreds: true, Email: "user@example.com", ExpiresAt: &exp},
		{Tool: "claude", Name: "work", Active: false, HasCreds: false},
		{Tool: "codex", Name: "personal", Active: true, HasCreds: true},
	}}
	var buf bytes.Buffer
	if err := switcherList(&buf, fake, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"TOOL", "PROFILE", "ACTIVE", "CREDS", "EMAIL",
		"claude", "personal", "*", "yes", "user@example.com",
		"work", "-", "no",
		"codex",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output: %q", want, out)
		}
	}
}

func TestSwitcherListJSON(t *testing.T) {
	exp := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	fake := &switcherFake{profiles: []switcher.Profile{
		{Tool: "claude", Name: "personal", Active: true, HasCreds: true, Email: "u@e.com", ExpiresAt: &exp},
	}}
	var buf bytes.Buffer
	if err := switcherList(&buf, fake, []string{"--json"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got switcherListJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v — output: %q", err, buf.String())
	}
	if len(got.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(got.Profiles))
	}
	p := got.Profiles[0]
	if p.Tool != "claude" || p.Name != "personal" || !p.Active || !p.HasCreds || p.Email != "u@e.com" {
		t.Errorf("profile fields wrong: %+v", p)
	}
	if p.ExpiresAt == nil || !p.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at wrong: %v", p.ExpiresAt)
	}
}

func TestSwitcherListError(t *testing.T) {
	fake := &switcherFake{listErr: fmt.Errorf("disk: %w", switcher.ErrDisabled)}
	var buf bytes.Buffer
	err := switcherList(&buf, fake, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "switcher is disabled") {
		t.Errorf("wrong error message: %q", err.Error())
	}
}

func TestSwitcherListUnknownFlag(t *testing.T) {
	fake := &switcherFake{}
	err := switcherList(&bytes.Buffer{}, fake, []string{"--bad"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("expected unknown flag error, got: %v", err)
	}
}

// ── status ───────────────────────────────────────────────────────────

func TestSwitcherStatusHappy(t *testing.T) {
	fake := &switcherFake{
		eligibility: []switcher.Eligibility{
			{Tool: "claude", Eligible: true},
			{Tool: "codex", Eligible: false, Reason: "Not logged in, or using the macOS Keychain"},
		},
		active: map[string]string{"claude": "personal", "codex": ""},
	}
	var buf bytes.Buffer
	if err := switcherStatus(&buf, fake, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"TOOL", "ACTIVE", "ELIGIBLE", "REASON", "claude", "personal", "yes", "codex", "no", "macOS Keychain"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output: %q", want, out)
		}
	}
}

func TestSwitcherStatusJSON(t *testing.T) {
	fake := &switcherFake{
		eligibility: []switcher.Eligibility{
			{Tool: "claude", Eligible: true},
		},
		active: map[string]string{"claude": "personal"},
	}
	var buf bytes.Buffer
	if err := switcherStatus(&buf, fake, []string{"--json"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got switcherStatusJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v — output: %q", err, buf.String())
	}
	if len(got.Eligibility) != 1 {
		t.Fatalf("expected 1 eligibility entry, got %d", len(got.Eligibility))
	}
	if got.Active["claude"] != "personal" {
		t.Errorf("active wrong: %v", got.Active)
	}
}

// ── use ──────────────────────────────────────────────────────────────

func TestSwitcherUseHappy(t *testing.T) {
	fake := &switcherFake{
		profiles:     []switcher.Profile{{Tool: "claude", Name: "work", Active: false, HasCreds: true}},
		switchResult: "work",
	}
	var buf bytes.Buffer
	if err := switcherUse(bytes.NewReader(nil), &buf, fake, []string{"claude", "work"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Switched claude to profile "work"`) {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

// TestSwitcherUseAlreadyActiveNoOp covers the branch a user reaches by running
// the same switch command twice: Switch returns switched=false, and the CLI
// must NOT claim "Switched". The fake's switchSwitched=false produces that path
// without a real credential round trip.
func TestSwitcherUseAlreadyActiveNoOp(t *testing.T) {
	switched := false
	fake := &switcherFake{
		profiles:       []switcher.Profile{{Tool: "claude", Name: "work", Active: true, HasCreds: true}},
		switchResult:   "work",
		switchSwitched: &switched,
	}
	var buf bytes.Buffer
	if err := switcherUse(bytes.NewReader(nil), &buf, fake, []string{"claude", "work"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "Switched") {
		t.Errorf("no-op switch claimed a switch: %q", out)
	}
	if !strings.Contains(out, `Already on claude profile "work"`) {
		t.Errorf("no-op switch output missing 'Already on' line: %q", out)
	}
}

func TestSwitcherUseEmptyProfileNonTTY(t *testing.T) {
	fake := &switcherFake{
		profiles: []switcher.Profile{{Tool: "claude", Name: "empty", Active: false, HasCreds: false}},
	}
	// bytes.Reader is not *os.File — treated as non-TTY
	err := switcherUse(bytes.NewReader(nil), &bytes.Buffer{}, fake, []string{"claude", "empty"})
	if err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("expected non-TTY error, got: %v", err)
	}
}

func TestSwitcherUseEmptyProfileAutoYes(t *testing.T) {
	fake := &switcherFake{
		profiles:     []switcher.Profile{{Tool: "claude", Name: "empty", Active: false, HasCreds: false}},
		switchResult: "empty",
	}
	var buf bytes.Buffer
	if err := switcherUse(bytes.NewReader(nil), &buf, fake, []string{"-y", "claude", "empty"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "empty") || !strings.Contains(out, "delete") && !strings.Contains(out, "fresh login") {
		// warning about empty profile must appear even with -y
		t.Errorf("expected empty-profile warning in output: %q", out)
	}
}

func TestSwitcherUseNoActiveProfile(t *testing.T) {
	fake := &switcherFake{
		profiles:  []switcher.Profile{{Tool: "claude", Name: "work", Active: false, HasCreds: true}},
		switchErr: fmt.Errorf("capture: %w", switcher.ErrNoActiveProfile),
	}
	err := switcherUse(bytes.NewReader(nil), &bytes.Buffer{}, fake, []string{"claude", "work"})
	if err == nil || !strings.Contains(err.Error(), "import") {
		t.Errorf("expected no-active-profile message, got: %v", err)
	}
}

func TestSwitcherUseArgs(t *testing.T) {
	fake := &switcherFake{}
	for _, args := range [][]string{{"claude"}, {}, {"claude", "x", "y"}} {
		if err := switcherUse(bytes.NewReader(nil), &bytes.Buffer{}, fake, args); err == nil {
			t.Errorf("expected error for args %v", args)
		}
	}
}

// ── add ──────────────────────────────────────────────────────────────

func TestSwitcherAddHappy(t *testing.T) {
	fake := &switcherFake{}
	var buf bytes.Buffer
	if err := switcherAdd(&buf, fake, []string{"claude", "work"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Created empty claude profile "work"`) {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestSwitcherAddProfileExists(t *testing.T) {
	fake := &switcherFake{addErr: fmt.Errorf("already: %w", switcher.ErrProfileExists)}
	err := switcherAdd(&bytes.Buffer{}, fake, []string{"claude", "work"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected profile-exists message, got: %v", err)
	}
}

func TestSwitcherAddInvalidName(t *testing.T) {
	fake := &switcherFake{addErr: fmt.Errorf("bad name: %w", switcher.ErrInvalidName)}
	err := switcherAdd(&bytes.Buffer{}, fake, []string{"claude", "../bad"})
	if err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Errorf("expected invalid-name message, got: %v", err)
	}
}

func TestSwitcherAddArgs(t *testing.T) {
	fake := &switcherFake{}
	for _, args := range [][]string{{"claude"}, {}, {"claude", "x", "y"}} {
		if err := switcherAdd(&bytes.Buffer{}, fake, args); err == nil {
			t.Errorf("expected error for args %v", args)
		}
	}
}

// ── import ───────────────────────────────────────────────────────────

func TestSwitcherImportHappy(t *testing.T) {
	fake := &switcherFake{}
	var buf bytes.Buffer
	if err := switcherImport(&buf, fake, []string{"codex", "mylogin"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Imported current codex login as profile "mylogin"`) {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestSwitcherImportNotEligible(t *testing.T) {
	reason := "not logged in, or using the macOS Keychain"
	// Use the typed error so errors.As extracts the Reason safely.
	fake := &switcherFake{importErr: &switcher.NotEligibleError{Tool: "claude", Reason: reason}}
	err := switcherImport(&bytes.Buffer{}, fake, []string{"claude", "x"})
	if err == nil || !strings.Contains(err.Error(), reason) {
		t.Errorf("expected eligibility reason in error, got: %v", err)
	}
}

func TestSwitcherImportDisabled(t *testing.T) {
	fake := &switcherFake{importErr: switcher.ErrDisabled}
	err := switcherImport(&bytes.Buffer{}, fake, []string{"claude", "x"})
	if err == nil || !strings.Contains(err.Error(), "aimebu switcher enable") {
		t.Errorf("expected enable hint in error, got: %v", err)
	}
}

// ── rename ───────────────────────────────────────────────────────────

func TestSwitcherRenameHappy(t *testing.T) {
	fake := &switcherFake{}
	var buf bytes.Buffer
	if err := switcherRename(&buf, fake, []string{"claude", "old", "new"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Renamed claude profile "old" to "new"`) {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestSwitcherRenameArgs(t *testing.T) {
	fake := &switcherFake{}
	for _, args := range [][]string{{"claude"}, {"claude", "old"}, {"claude", "old", "new", "extra"}} {
		if err := switcherRename(&bytes.Buffer{}, fake, args); err == nil {
			t.Errorf("expected error for args %v", args)
		}
	}
}

func TestSwitcherRenameProfileNotFound(t *testing.T) {
	fake := &switcherFake{renameErr: fmt.Errorf("rename: %w", switcher.ErrProfileNotFound)}
	err := switcherRename(&bytes.Buffer{}, fake, []string{"claude", "old", "new"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found message, got: %v", err)
	}
}

// ── remove ───────────────────────────────────────────────────────────

func TestSwitcherRemoveHappy(t *testing.T) {
	fake := &switcherFake{}
	var buf bytes.Buffer
	if err := switcherRemove(bytes.NewReader(nil), &buf, fake, []string{"claude", "work"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `Removed claude profile "work"`) {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestSwitcherRemoveActiveNonTTY(t *testing.T) {
	fake := &switcherFake{removeErr: fmt.Errorf("active: %w", switcher.ErrActiveProfile)}
	// bytes.Reader is not *os.File — treated as non-TTY
	err := switcherRemove(bytes.NewReader(nil), &bytes.Buffer{}, fake, []string{"claude", "personal"})
	if err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("expected non-TTY error, got: %v", err)
	}
}

func TestSwitcherRemoveActiveAutoYes(t *testing.T) {
	fake := &switcherFake{removeErr: fmt.Errorf("active: %w", switcher.ErrActiveProfile)}
	var buf bytes.Buffer
	if err := switcherRemove(bytes.NewReader(nil), &buf, fake, []string{"-y", "claude", "personal"}); err != nil {
		t.Fatalf("unexpected error after -y: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `Removed claude profile "personal"`) {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestSwitcherRemoveForceError(t *testing.T) {
	fake := &switcherFake{
		removeErr:      fmt.Errorf("active: %w", switcher.ErrActiveProfile),
		removeForceErr: errors.New("filesystem error"),
	}
	err := switcherRemove(bytes.NewReader(nil), &bytes.Buffer{}, fake, []string{"-y", "claude", "personal"})
	// Unknown errors from S1 must reach the user intact — they contain useful context
	// (path, position) that helps debug. S1 is responsible for never embedding credentials.
	if err == nil || !strings.Contains(err.Error(), "filesystem error") {
		t.Errorf("expected unknown error to reach user intact, got: %v", err)
	}
}

// ── enable / disable ─────────────────────────────────────────────────

func TestSwitcherEnable(t *testing.T) {
	fake := &switcherFake{}
	var buf bytes.Buffer
	if err := switcherSetEnabled(&buf, fake, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Switcher enabled") {
		t.Errorf("unexpected output: %q", buf.String())
	}
	if !fake.enabled {
		t.Error("expected enabled=true after enable")
	}
}

func TestSwitcherDisable(t *testing.T) {
	fake := &switcherFake{enabled: true}
	var buf bytes.Buffer
	if err := switcherSetEnabled(&buf, fake, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Switcher disabled") {
		t.Errorf("unexpected output: %q", buf.String())
	}
	if fake.enabled {
		t.Error("expected enabled=false after disable")
	}
}

// ── credential material ───────────────────────────────────────────────

// TestSwitcherNoCredentialMaterial verifies that known sentinel errors produce
// fixed messages even when the underlying error wraps sensitive strings.
func TestSwitcherNoCredentialMaterial(t *testing.T) {
	secret := "sk-ant-secret-token-ABCDEF123456"
	secretJSON := `{"accessToken":"` + secret + `"}`

	cases := []struct {
		name    string
		err     error
		errWrap func(error) error
	}{
		{
			name:    "invalid credentials",
			err:     switcher.ErrInvalidCredentials,
			errWrap: func(e error) error { return fmt.Errorf("parse failed: %s: %w", secretJSON, e) },
		},
		{
			name:    "disabled",
			err:     switcher.ErrDisabled,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			name:    "no active profile",
			err:     switcher.ErrNoActiveProfile,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			name:    "profile not found",
			err:     switcher.ErrProfileNotFound,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			name:    "profile exists",
			err:     switcher.ErrProfileExists,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			name:    "invalid name",
			err:     switcher.ErrInvalidName,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			// plain-wrap path: no typed error found by errors.As; falls back to the
			// generic "not eligible" message rather than returning err.Error() (which
			// contains the wrapping context with secretJSON).
			name:    "not eligible — plain wrap leaks nothing",
			err:     switcher.ErrNotEligible,
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
		{
			// typed path: errors.As finds the NotEligibleError and extracts only ne.Reason,
			// discarding the outer wrapping context that may contain credential material.
			name:    "not eligible — typed error Reason leaks nothing",
			err:     &switcher.NotEligibleError{Tool: "claude", Reason: "not logged in, or using the macOS Keychain"},
			errWrap: func(e error) error { return fmt.Errorf("%s: %w", secretJSON, e) },
		},
	}
	// Note: the default: branch passes unknown errors through verbatim by design.
	// S1 holds the invariant (no credentials in errors); TestSwitcherRemoveForceError
	// asserts that unknown errors reach the user intact.

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := tc.errWrap(tc.err)
			msg := switcherErrMsg(wrapped)
			if msg == nil {
				t.Fatal("expected non-nil error")
			}
			if strings.Contains(msg.Error(), secret) {
				t.Errorf("error message leaks credential material %q: %q", secret, msg.Error())
			}
			if strings.Contains(msg.Error(), secretJSON) {
				t.Errorf("error message leaks credential JSON: %q", msg.Error())
			}
		})
	}
}

// TestSwitcherStatusNoCredentialMaterial verifies status error paths do not leak credentials.
func TestSwitcherStatusNoCredentialMaterial(t *testing.T) {
	secret := "sk-ant-secret-token-ABCDEF123456"
	secretJSON := `{"accessToken":"` + secret + `"}`

	// Eligibility() returning a sentinel-wrapped error must not leak the wrapping context.
	fake := &switcherFake{
		eligErr: fmt.Errorf("check: %s: %w", secretJSON, switcher.ErrDisabled),
	}
	err := switcherStatus(&bytes.Buffer{}, fake, nil)
	if err == nil {
		t.Fatal("expected error from eligibility")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("status eligibility error leaks credential material: %q", err.Error())
	}

	// Active() returning a sentinel-wrapped error must not leak the wrapping context.
	fake2 := &switcherFake{
		eligibility: []switcher.Eligibility{{Tool: "claude", Eligible: true}},
		activeErr:   fmt.Errorf("lookup: %s: %w", secretJSON, switcher.ErrProfileNotFound),
	}
	err = switcherStatus(&bytes.Buffer{}, fake2, nil)
	if err == nil {
		t.Fatal("expected error from active")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("status active error leaks credential material: %q", err.Error())
	}
}

// TestSwitcherListNoCredentialMaterial verifies list error path does not leak credentials.
func TestSwitcherListNoCredentialMaterial(t *testing.T) {
	secret := "sk-ant-secret-token-ABCDEF123456"
	fake := &switcherFake{
		listErr: fmt.Errorf("parse: %s: %w", secret, switcher.ErrInvalidCredentials),
	}
	err := switcherList(&bytes.Buffer{}, fake, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("list error leaks credential material: %q", err.Error())
	}
}

// TestSwitcherSwitchNoCredentialMaterial verifies use error path does not leak credentials.
func TestSwitcherSwitchNoCredentialMaterial(t *testing.T) {
	secret := "sk-ant-secret-token-ABCDEF123456"
	fake := &switcherFake{
		profiles:  []switcher.Profile{{Tool: "claude", Name: "work", Active: false, HasCreds: true}},
		switchErr: fmt.Errorf("capture failed: %s: %w", secret, switcher.ErrInvalidCredentials),
	}
	err := switcherUse(bytes.NewReader(nil), &bytes.Buffer{}, fake, []string{"claude", "work"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("use error leaks credential material: %q", err.Error())
	}
}

func TestSwitcherErrMsgMaintenanceBusyHint(t *testing.T) {
	err := switcherErrMsg(fmt.Errorf("%w: wait elapsed", switcher.ErrMaintenanceBusy))
	if err == nil || !strings.Contains(err.Error(), "background token refresh") || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("hint = %v", err)
	}
}

// F1: the CLI builds its own manager; without the token-copy hooks every switch
// on a migrated install failed ("fan-out is not wired").
func TestNewSwitcherManagerSwitchesAMigratedInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(home, ".aimebu"))
	t.Setenv("AIMEBU_URL", "http://127.0.0.1:1") // nothing listens: the switch runs locally
	root, err := switcher.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	m, err := switcher.New(root)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(6 * time.Hour).UnixMilli()
	full := func(access, refresh string) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, access, refresh, expiry)
	}
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(full("tok-a", "ref-a")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(switcher.ToolClaude, "b"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "b"), []byte(full("tok-b", "ref-b")), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := switcherMigrateCopies(&out, []string{"--tool", "claude"}); err != nil {
		t.Fatalf("migrate-copies: %v", err)
	}

	cli, err := newSwitcherManager()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cli.Switch(switcher.ToolClaude, "b"); err != nil {
		t.Fatalf("CLI switch on a migrated install: %v", err)
	}
	data, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "tok-b") || strings.Contains(got, "refreshToken") {
		t.Fatalf("live file after the CLI switch: %s", got)
	}
}

// migratedCLIInstall builds a migrated two-profile claude install under a temp
// HOME and returns the CLI manager.
func migratedCLIInstall(t *testing.T) (cli switcherManager, live string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AIMEBU_CONFIG_DIR", filepath.Join(home, ".aimebu"))
	t.Setenv("AIMEBU_URL", "http://127.0.0.1:1")
	root, _ := switcher.DefaultRoot()
	m, err := switcher.New(root)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(6 * time.Hour).UnixMilli()
	full := func(access, refresh string) string {
		return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, access, refresh, expiry)
	}
	live = filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(full("tok-a", "ref-a")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, step := range []error{m.SetEnabled(true), m.Import(switcher.ToolClaude, "a"), m.Add(switcher.ToolClaude, "b"),
		os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "b"), []byte(full("tok-b", "ref-b")), 0o600)} {
		if step != nil {
			t.Fatal(step)
		}
	}
	if err := switcherMigrateCopies(&bytes.Buffer{}, []string{"--tool", "claude"}); err != nil {
		t.Fatalf("migrate-copies: %v", err)
	}
	cli, err = newSwitcherManager()
	if err != nil {
		t.Fatal(err)
	}
	return cli, live
}

// N2: on a token-copy install the CLI hands the switch to a running server (the
// owner of the refresher) instead of switching locally.
func TestCLISwitchGoesThroughARunningServer(t *testing.T) {
	cli, live := migratedCLIInstall(t)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usages/switcher/switch" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"active":"b","switched":true}`))
	}))
	defer srv.Close()
	t.Setenv("AIMEBU_URL", srv.URL)

	active, switched, err := cli.Switch(switcher.ToolClaude, "b")
	if err != nil || active != "b" || !switched {
		t.Fatalf("Switch = %q %v %v", active, switched, err)
	}
	if got["tool"] != "claude" || got["profile"] != "b" {
		t.Fatalf("server got %v", got)
	}
	if data, _ := os.ReadFile(live); !strings.Contains(string(data), "tok-a") {
		t.Fatal("the CLI also switched locally although the server answered")
	}
}

func TestCLISwitchReportsAServerRefusalVerbatim(t *testing.T) {
	cli, _ := migratedCLIInstall(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"a login in the live credentials is waiting to be imported; retry in a minute"}`))
	}))
	defer srv.Close()
	t.Setenv("AIMEBU_URL", srv.URL)
	if _, _, err := cli.Switch(switcher.ToolClaude, "b"); err == nil || !strings.Contains(err.Error(), "waiting to be imported") {
		t.Fatalf("Switch = %v, want the server's message", err)
	}
}

// With no server the switch runs locally; a due incoming profile is then refused
// with a pointer to the server (the CLI process has no refresher).
func TestCLISwitchWithoutAServerRefusesADueIncomingProfile(t *testing.T) {
	cli, live := migratedCLIInstall(t)
	root, _ := switcher.DefaultRoot()
	m, _ := switcher.New(root)
	soon := time.Now().Add(5 * time.Minute).UnixMilli()
	due := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"tok-b","refreshToken":"ref-b","expiresAt":%d}}`, soon)
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "b"), []byte(due), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(live)
	_, _, err := cli.Switch(switcher.ToolClaude, "b")
	if !errors.Is(err, switcher.ErrIncomingRefresh) || !strings.Contains(err.Error(), "start the aimebu server") {
		t.Fatalf("Switch = %v, want ErrIncomingRefresh pointing at the server", err)
	}
	if after, _ := os.ReadFile(live); string(after) != string(before) {
		t.Fatal("a refused switch changed the live file")
	}
}

func TestIsLoopbackURL(t *testing.T) {
	for url, want := range map[string]bool{
		"http://localhost:9997": true, "http://127.0.0.1:9997": true, "https://[::1]:9996": true,
		"http://host.docker.internal:9997": false, "http://192.168.1.5:9997": false, "::bad": false,
	} {
		if got := isLoopbackURL(url); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", url, got, want)
		}
	}
}
