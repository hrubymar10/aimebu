package switcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
)

// full claude credential with a given access token, a refresh token and expiry.
func fullClaude(access, refresh string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, access, refresh, expires.UnixMilli())
}

// tokenCopyManager builds a migrated install: two claude profiles as regular
// files, the live file a token-only copy of the active one, and hooks whose
// FanOutActive rewrites the live file like the real fan-out does (access token
// only, refresh token removed).
func tokenCopyManager(t *testing.T) (m *Manager, livePath string, fanned *[]string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	expiry := time.Now().Add(6 * time.Hour)
	livePath = writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", expiry))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", expiry)), 0o600))
	// Migrate by hand: store file regular, live token-only, marker set.
	stored := m.profileCredPath(ToolClaude, "a")
	must(t, os.Remove(stored))
	must(t, os.WriteFile(stored, []byte(fullClaude("tok-a", "ref-a", expiry)), 0o600))
	must(t, os.WriteFile(livePath, []byte(tokenOnlyClaude("tok-a", expiry)), 0o600))
	must(t, m.SetTokenCopies(true))
	var calls []string
	fanned = &calls
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: func(tool string, full []byte) error {
		calls = append(calls, tool)
		if strings.Contains(string(full), `"refreshToken"`) {
			// the real fan-out strips it; emulate that
			full = []byte(strings.Replace(string(full), `"refreshToken":"ref-b",`, "", 1))
		}
		return os.WriteFile(livePath, full, 0o600)
	}})
	return m, livePath, fanned
}

func tokenOnlyClaude(access string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, access, expires.UnixMilli())
}

func TestTokenCopySwitchRewritesLiveWithoutCaptureOrLinks(t *testing.T) {
	m, live, fanned := tokenCopyManager(t)
	if !m.TokenCopies() {
		t.Fatal("marker not read")
	}
	before := readFile(t, m.profileCredPath(ToolClaude, "a"))
	active, switched, err := m.Switch(ToolClaude, "b")
	if err != nil || !switched || active != "b" {
		t.Fatalf("Switch = %q %v %v", active, switched, err)
	}
	if len(*fanned) != 1 {
		t.Fatalf("fan-out calls = %v", *fanned)
	}
	if got := readFile(t, live); !strings.Contains(got, "tok-b") || strings.Contains(got, "refreshToken") {
		t.Fatalf("live file after switch = %s", got)
	}
	// The outgoing profile is untouched (no capture of the live file) and nothing
	// became a symlink.
	if got := readFile(t, m.profileCredPath(ToolClaude, "a")); got != before {
		t.Fatalf("outgoing profile changed: %s", got)
	}
	for _, name := range []string{"a", "b"} {
		info, err := os.Lstat(m.profileCredPath(ToolClaude, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("profile %s is a symlink or missing: %v %v", name, info, err)
		}
	}
	if a, _ := m.Active(ToolClaude); a != "b" {
		t.Fatalf("active = %q", a)
	}
	if at := switchedAwayOf(t, m, ToolClaude, "a"); at == nil {
		t.Fatal("switch-away time not recorded")
	}
	// Switching back works the same way.
	if _, _, err := m.Switch(ToolClaude, "a"); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	if err := m.EnsureActiveLinks(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(m.profileCredPath(ToolClaude, "a")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("EnsureActiveLinks relinked a token-copy install: %v %v", info, err)
	}
}

// A refresh token in the live file is a /login nobody imported yet: switching
// would throw it away, so the switch refuses and changes nothing.
func TestTokenCopySwitchRefusesWhileALoginIsPending(t *testing.T) {
	m, live, fanned := tokenCopyManager(t)
	login := fullClaude("tok-new", "ref-new", time.Now().Add(8*time.Hour))
	must(t, os.WriteFile(live, []byte(login), 0o600))
	if _, _, err := m.Switch(ToolClaude, "b"); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("Switch = %v, want ErrLoginPending", err)
	}
	if got := readFile(t, live); got != login {
		t.Fatalf("the pending login was modified: %s", got)
	}
	if len(*fanned) != 0 {
		t.Fatal("fan-out ran")
	}
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active changed to %q", a)
	}
}

func TestTokenCopySwitchRefreshesIncomingInsideTheLead(t *testing.T) {
	m, live, _ := tokenCopyManager(t)
	// profile b expires in 30 min: inside the 60-minute claude lead.
	soon := time.Now().Add(30 * time.Minute)
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b-old", "ref-b", soon)), 0o600))
	var refreshed []string
	hooks := m.copyHooks
	hooks.RefreshIncoming = func(_ context.Context, tool, profile string) error {
		refreshed = append(refreshed, tool+"/"+profile)
		// the refresher commits the new credential into the store
		return os.WriteFile(m.profileCredPath(tool, profile), []byte(fullClaude("tok-b-new", "ref-b", time.Now().Add(8*time.Hour))), 0o600)
	}
	m.SetTokenCopyHooks(hooks)
	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if len(refreshed) != 1 || refreshed[0] != "claude/b" {
		t.Fatalf("refreshes = %v", refreshed)
	}
	if got := readFile(t, live); !strings.Contains(got, "tok-b-new") {
		t.Fatalf("live file was not rewritten from the refreshed credential: %s", got)
	}
}

func TestTokenCopySwitchIsRefusedWhenTheIncomingRefreshFails(t *testing.T) {
	m, live, fanned := tokenCopyManager(t)
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", time.Now().Add(5*time.Minute))), 0o600))
	hooks := m.copyHooks
	// Like the real refresher: it decides whether the profile is due, and fails
	// only when it has to refresh.
	hooks.RefreshIncoming = func(_ context.Context, tool, profile string) error {
		data, err := os.ReadFile(m.profileCredPath(tool, profile))
		if err != nil {
			return err
		}
		if cred, perr := credtarget.ParseFull(tool, data); perr == nil && cred.ExpiresAt != nil && time.Until(*cred.ExpiresAt) > time.Hour {
			return nil // not due
		}
		return errors.New("provider down")
	}
	m.SetTokenCopyHooks(hooks)
	liveBefore := readFile(t, live)
	if _, _, err := m.Switch(ToolClaude, "b"); !errors.Is(err, ErrIncomingRefresh) {
		t.Fatalf("Switch = %v, want ErrIncomingRefresh", err)
	}
	if readFile(t, live) != liveBefore || len(*fanned) != 0 {
		t.Fatal("a refused switch changed the live file")
	}
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active changed to %q", a)
	}
	// A profile that is NOT due is switched without calling the refresher.
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", time.Now().Add(6*time.Hour))), 0o600))
	if _, _, err := m.Switch(ToolClaude, "b"); err != nil {
		t.Fatalf("Switch of a fresh profile: %v", err)
	}
}

func TestTokenCopySwitchFanOutFailureLeavesTheOldAccount(t *testing.T) {
	m, live, _ := tokenCopyManager(t)
	hooks := m.copyHooks
	hooks.FanOutActive = func(string, []byte) error { return errors.New("disk full") }
	m.SetTokenCopyHooks(hooks)
	liveBefore := readFile(t, live)
	if _, _, err := m.Switch(ToolClaude, "b"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Switch = %v", err)
	}
	if readFile(t, live) != liveBefore {
		t.Fatal("live file changed")
	}
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active changed to %q", a)
	}
}

func TestTokenCopySwitchToEmptyProfileClearsLive(t *testing.T) {
	m, live, _ := tokenCopyManager(t)
	must(t, m.Add(ToolClaude, "empty"))
	if _, _, err := m.Switch(ToolClaude, "empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live file still exists: %v", err)
	}
	if a, _ := m.Active(ToolClaude); a != "empty" {
		t.Fatalf("active = %q", a)
	}
}

func TestTokenCopySwitchNeedsFanOutWired(t *testing.T) {
	m, _, _ := tokenCopyManager(t)
	m.SetTokenCopyHooks(TokenCopyHooks{})
	if _, _, err := m.Switch(ToolClaude, "b"); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("Switch = %v", err)
	}
}

func TestTokenCopySwitchRefusesAStoreFileWithoutRefreshToken(t *testing.T) {
	m, _, _ := tokenCopyManager(t)
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(tokenOnlyClaude("tok-b", time.Now().Add(6*time.Hour))), 0o600))
	if _, _, err := m.Switch(ToolClaude, "b"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Switch = %v, want ErrInvalidCredentials", err)
	}
}

// A due incoming profile with no refresh hook is refused rather than handed to
// sessions as an expired or dying credential.
func TestTokenCopySwitchRefusesADueIncomingProfileWithoutARefreshHook(t *testing.T) {
	m, live, fanned := tokenCopyManager(t)
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", time.Now().Add(5*time.Minute))), 0o600))
	liveBefore := readFile(t, live)
	if _, _, err := m.Switch(ToolClaude, "b"); !errors.Is(err, ErrIncomingRefresh) {
		t.Fatalf("Switch = %v, want ErrIncomingRefresh", err)
	}
	if readFile(t, live) != liveBefore || len(*fanned) != 0 {
		t.Fatal("a refused switch changed the live file")
	}
}

// N3: the empty-profile switch clears every active target through the hook.
func TestTokenCopySwitchToEmptyProfileUsesTheClearHook(t *testing.T) {
	m, live, _ := tokenCopyManager(t)
	must(t, m.Add(ToolClaude, "empty"))
	var cleared []string
	hooks := m.copyHooks
	hooks.ClearActive = func(tool string) error {
		cleared = append(cleared, tool)
		return os.Remove(live)
	}
	m.SetTokenCopyHooks(hooks)
	if _, _, err := m.Switch(ToolClaude, "empty"); err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 1 || cleared[0] != ToolClaude {
		t.Fatalf("clear hook calls = %v", cleared)
	}
	// A failing clear keeps the old account active and live.
	m2, live2, _ := tokenCopyManager(t)
	must(t, m2.Add(ToolClaude, "empty"))
	h2 := m2.copyHooks
	h2.ClearActive = func(string) error { return errors.New("target busy") }
	m2.SetTokenCopyHooks(h2)
	if _, _, err := m2.Switch(ToolClaude, "empty"); err == nil {
		t.Fatal("a failed clear must fail the switch")
	}
	if a, _ := m2.Active(ToolClaude); a != "a" {
		t.Fatalf("active = %q after a failed clear, want a", a)
	}
	if _, err := os.Stat(live2); err != nil {
		t.Fatalf("live file lost: %v", err)
	}
}

// L-2: migrate-copies finishing between Switch's unlocked marker check and its
// lock must not run the link-model steps (or skip the incoming refresh).
func TestSwitchRacingAMigrationIsRefusedUntouched(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	exp := time.Now().Add(6 * time.Hour)
	live := writeLive(t, home, ToolClaude, fullClaude("tok-a", "ref-a", exp))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(fullClaude("tok-b", "ref-b", exp)), 0o600))
	m.beforeSwitchLock = func() { must(t, m.SetTokenCopies(true)) }
	before := readFile(t, live)
	if _, _, err := m.Switch(ToolClaude, "b"); !errors.Is(err, ErrMigrationRaced) {
		t.Fatalf("Switch = %v, want ErrMigrationRaced", err)
	}
	if readFile(t, live) != before {
		t.Fatal("a refused switch changed the live file")
	}
	if a, _ := m.Active(ToolClaude); a != "a" {
		t.Fatalf("active = %q", a)
	}
}
