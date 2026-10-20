package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
)

// externalLoginInstall wires the real fan-out and registers an external follow-active target that holds a fresh /login
// nobody imported yet: the only copy of that refresh token.
func externalLoginInstall(t *testing.T, m *Manager, home string) (externalPath, login string) {
	t.Helper()
	return externalLoginInstallFor(t, m, home, "")
}

// externalLoginInstallFor is externalLoginInstall with the target pinned to profile ("" follows the active profile).
func externalLoginInstallFor(t *testing.T, m *Manager, home, profile string) (externalPath, login string) {
	t.Helper()
	reg := credtarget.NewRegistry(filepath.Join(home, "server"), home)
	reg.LivePath = m.LiveCredPath
	externalPath = filepath.Join(t.TempDir(), ".credentials.json")
	login = futureClaude("external-login")
	must(t, os.WriteFile(externalPath, []byte(login), 0o600))
	must(t, reg.Register(credtarget.Target{ID: "docker-worker", Tool: ToolClaude, Path: externalPath, Profile: profile}))
	fan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: m.Active, WithLock: m.WithLock}
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: fan.FanOutActive, ReplaceActive: fan.FanOutActiveReplacing, ClearActive: fan.ClearActive, PendingLogins: fan.PendingLogins, StripLogin: fan.StripLogin})
	return externalPath, login
}

// Importing the host's live login must not touch an external target's unrelated, unimported login.
func TestImportingTheLiveLoginKeepsAnExternalTargetsLogin(t *testing.T) {
	m, home := enabledManager(t)
	must(t, m.SetTokenCopies(true))
	external, login := externalLoginInstall(t, m, home)
	live := writeLive(t, home, ToolClaude, futureClaude("host-login"))
	if err := m.Import(ToolClaude, "main"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, external); got != login {
		t.Fatalf("the external target's login was overwritten: %s", got)
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, live))) {
		t.Fatal("the host live file should have been stripped after the import")
	}
}

// Switching to an empty profile clears the targets so the CLI asks for a login, but never a target that still holds one.
func TestSwitchingToAnEmptyProfileRefusesWhileAnExternalTargetHoldsALogin(t *testing.T) {
	m, home := enabledManager(t)
	must(t, m.SetTokenCopies(true))
	must(t, m.ImportLogin(credtarget.LoginImport{Tool: ToolClaude, Profile: "main", Create: true, Full: []byte(futureClaude("main-token")), Email: "a@example.test", SetActive: true}))
	must(t, m.Add(ToolClaude, "fresh"))
	live := writeLive(t, home, ToolClaude, `{"claudeAiOauth":{"accessToken":"live-access","expiresAt":`+itoa(time.Now().Add(time.Hour).UnixMilli())+`}}`)
	external, login := externalLoginInstall(t, m, home)
	_, switched, err := m.Switch(ToolClaude, "fresh")
	if !errors.Is(err, ErrLoginPending) || switched {
		t.Fatalf("switched=%v err=%v: the switch must be refused", switched, err)
	}
	if got := readFile(t, external); got != login {
		t.Fatalf("the external login was destroyed: %s", got)
	}
	if _, statErr := os.Stat(live); statErr != nil {
		t.Fatalf("a refused switch must leave the live file: %v", statErr)
	}
	if active, _ := m.Active(ToolClaude); active != "main" {
		t.Fatalf("active moved to %q", active)
	}
}

// migrate-copies files a login sitting in an external target (nothing else would on an unmigrated tool), then converts:
// the login ends up in a new profile, the target becomes token-only and the host's own migration is unaffected.
func TestMigratingFilesAnExternalTargetsLoginAndStripsIt(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	external, login := externalLoginInstall(t, m, t.TempDir())
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	names, _ := m.ProfileNames(ToolClaude)
	var filed string
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			filed = name
		}
	}
	if filed == "" {
		t.Fatalf("the external login was not filed into a new profile: %v", names)
	}
	if got := readFile(t, m.StoredCredPath(ToolClaude, filed)); got != login {
		t.Fatalf("the filed profile does not hold the login: %s", got)
	}
	if got := readFile(t, external); credtarget.HasRefreshToken(ToolClaude, []byte(got)) {
		t.Fatalf("the external target still holds the refresh token: %s", got)
	}
	if got := readFile(t, live); strings.Contains(got, "refreshToken") {
		t.Fatalf("the host live file should be token-only: %s", got)
	}
	if active, _ := m.Active(ToolClaude); active != "main" {
		t.Fatalf("filing an external login moved the active profile to %q", active)
	}
}

// A target pinned to an existing profile files into that profile (the replaced credential is backed up); a login whose
// refresh token a profile already holds is that profile, with no new one.
func TestMigratingFilesAPinnedTargetsLoginIntoItsProfile(t *testing.T) {
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	external, login := externalLoginInstallFor(t, m, t.TempDir(), "other")
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := readFile(t, m.StoredCredPath(ToolClaude, "other")); got != login {
		t.Fatalf("the pinned profile does not hold the login: %s", got)
	}
	names, _ := m.ProfileNames(ToolClaude)
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			t.Fatalf("a pinned login created a new profile %s", name)
		}
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, external))) {
		t.Fatal("the pinned target still holds the refresh token")
	}
}

func TestMigratingRecognisesALoginAProfileAlreadyHolds(t *testing.T) {
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	external, login := externalLoginInstall(t, m, t.TempDir())
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "other"), []byte(login), 0o600)) // the same grant is already stored
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	names, _ := m.ProfileNames(ToolClaude)
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			t.Fatalf("an already stored login created a duplicate profile %s", name)
		}
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, external))) {
		t.Fatal("the target should be token-only")
	}
}

// The strip is guarded like the reconciler's: a target whose owner rotated or logged in again between filing and strip keeps
// its newer pair, and a target that was removed stays removed.
func TestMigratingNeverStripsANewerLoginOrRecreatesARemovedTarget(t *testing.T) {
	for _, mode := range []string{"newer", "removed"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
			external, _ := externalLoginInstall(t, m, t.TempDir())
			hooks := m.copyHooks
			newer := futureClaude("rotated-by-owner")
			real := hooks.StripLogin
			hooks.StripLogin = func(tool, id string, original, full []byte) error {
				if mode == "newer" {
					must(t, os.WriteFile(external, []byte(newer), 0o600))
				} else {
					must(t, os.Remove(external))
				}
				return real(tool, id, original, full)
			}
			m.SetTokenCopyHooks(hooks)
			if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			data, err := os.ReadFile(external)
			if mode == "newer" && (err != nil || string(data) != newer) {
				t.Fatalf("the owner's newer login was overwritten: %s %v", data, err)
			}
			if mode == "removed" && err == nil {
				t.Fatalf("a removed target was recreated: %s", data)
			}
		})
	}
}

// A target pinned to the still-linked active profile: the migration turns that profile into a regular file first, so the
// login is filed into it and the migration succeeds.
func TestMigratingFilesALoginPinnedToTheActiveProfile(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	external, login := externalLoginInstallFor(t, m, t.TempDir(), "main")
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := readFile(t, m.StoredCredPath(ToolClaude, "main")); got != login {
		t.Fatalf("the active profile does not hold the pinned login: %s", got)
	}
	if info, _ := os.Lstat(m.StoredCredPath(ToolClaude, "main")); info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the active profile is still a link")
	}
	// The live file follows the account that is now stored: token-only, for the new login.
	if got := readFile(t, live); strings.Contains(got, "refreshToken") || !strings.Contains(got, "external-login") || strings.Contains(got, "tok-live") {
		t.Fatalf("the live file does not carry the filed account token-only: %s", got)
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, external))) {
		t.Fatal("the pinned target still holds the refresh token")
	}
	if active, _ := m.Active(ToolClaude); active != "main" {
		t.Fatalf("active = %q", active)
	}
}

// An external file that looks like a login but is not a usable credential refuses the migration up front: the live file is
// untouched and no marker is written, so one bad target cannot leave the install half converted.
func TestMigratingRefusesCleanlyOnAnUnusableExternalLogin(t *testing.T) {
	m, live := linkedInstall(t, time.Now().Add(6*time.Hour))
	external, _ := externalLoginInstall(t, m, t.TempDir())
	garbage := `{"claudeAiOauth":{"refreshToken":"only-a-refresh-token"}}`
	must(t, os.WriteFile(external, []byte(garbage), 0o600))
	before := readFile(t, live)
	_, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("migrate = %v, want ErrInvalidCredentials", err)
	}
	if got := readFile(t, live); got != before {
		t.Fatalf("the live file was touched by a refused migration: %s", got)
	}
	if m.TokenCopiesFor(ToolClaude) {
		t.Fatal("a marker was written")
	}
	if info, statErr := os.Lstat(m.StoredCredPath(ToolClaude, "main")); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the active profile was converted: %v %v", info, statErr)
	}
	if got := readFile(t, external); got != garbage {
		t.Fatalf("the external file changed: %s", got)
	}
}

type extSpec struct {
	id, profile, login string
}

// externalTargets registers several external targets with the real fan-out and returns their paths by id.
func externalTargets(t *testing.T, m *Manager, home string, specs ...extSpec) map[string]string {
	t.Helper()
	reg := credtarget.NewRegistry(filepath.Join(home, "server"), home)
	reg.LivePath = m.LiveCredPath
	paths := map[string]string{}
	for _, spec := range specs {
		path := filepath.Join(t.TempDir(), "claude", ".credentials.json")
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(spec.login), 0o600))
		must(t, reg.Register(credtarget.Target{ID: spec.id, Tool: ToolClaude, Path: path, Profile: spec.profile}))
		paths[spec.id] = path
	}
	fan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: m.Active, WithLock: m.WithLock}
	m.SetTokenCopyHooks(TokenCopyHooks{FanOutActive: fan.FanOutActive, ReplaceActive: fan.FanOutActiveReplacing, ClearActive: fan.ClearActive, PendingLogins: fan.PendingLogins, StripLogin: fan.StripLogin})
	return paths
}

// Two targets holding one login are one token family: it is filed once and both targets are stripped from that profile.
func TestMigratingFilesTwoTargetsWithOneLoginIntoOneProfile(t *testing.T) {
	m, home := linkedInstallWithHome(t)
	login := futureClaude("shared-login")
	paths := externalTargets(t, m, home, extSpec{"a", "", login}, extSpec{"b", "", login})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	names, _ := m.ProfileNames(ToolClaude)
	filed := 0
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			filed++
		}
	}
	if filed != 1 {
		t.Fatalf("one login family must become one profile, got %d: %v", filed, names)
	}
	for id, path := range paths {
		if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, path))) {
			t.Fatalf("target %s still holds the refresh token", id)
		}
	}
}

// A target pinned to profile P with another login would replace P's credential; a second target that matched P's current
// token would then silently change account. The migration refuses before touching anything.
func TestMigratingRefusesWhenAPinnedFilingWouldPullAProfileFromUnderAnotherTarget(t *testing.T) {
	m, home := linkedInstallWithHome(t)
	otherToken := fullClaude("tok-other", "ref-other", time.Now().Add(6*time.Hour)) // what profile "other" stores
	paths := externalTargets(t, m, home, extSpec{"pinned", "other", futureClaude("different-login")}, extSpec{"matching", "", otherToken})
	before := readFile(t, m.StoredCredPath(ToolClaude, "other"))
	_, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
	if !errors.Is(err, ErrLoginPending) {
		t.Fatalf("migrate = %v, want ErrLoginPending", err)
	}
	if got := readFile(t, m.StoredCredPath(ToolClaude, "other")); got != before {
		t.Fatalf("profile other was changed by a refused migration: %s", got)
	}
	if m.TokenCopiesFor(ToolClaude) {
		t.Fatal("a marker was written")
	}
	if !credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, paths["pinned"]))) || !credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, paths["matching"]))) {
		t.Fatal("a target was touched")
	}
}

// A target whose directory is gone by the time of the strip is skipped without recreating the directory or taking its lock.
func TestMigratingDoesNotRecreateARemovedTargetsDirectory(t *testing.T) {
	m, home := linkedInstallWithHome(t)
	paths := externalTargets(t, m, home, extSpec{"gone", "", futureClaude("soon-gone")})
	dir := filepath.Dir(paths["gone"])
	hooks := m.copyHooks
	real := hooks.StripLogin
	hooks.StripLogin = func(tool, id string, original, full []byte) error {
		must(t, os.RemoveAll(dir))
		return real(tool, id, original, full)
	}
	m.SetTokenCopyHooks(hooks)
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the removed target's directory was recreated")
	}
}

// linkedInstallWithHome is linkedInstall plus the home directory the fan-out registry needs.
func linkedInstallWithHome(t *testing.T) (*Manager, string) {
	t.Helper()
	m, _ := linkedInstall(t, time.Now().Add(6*time.Hour))
	return m, t.TempDir()
}

// Registration order must not decide a group's destination: an unpinned target registered first does not swallow its
// pinned sibling's pin. The login lands in the pinned profile and both targets are stripped from it.
func TestMigratingGivesAGroupItsPinnedMembersProfile(t *testing.T) {
	m, home := linkedInstallWithHome(t)
	login := futureClaude("shared-pinned")
	paths := externalTargets(t, m, home, extSpec{"unpinned-first", "", login}, extSpec{"pinned-second", "other", login})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := readFile(t, m.StoredCredPath(ToolClaude, "other")); got != login {
		t.Fatalf("the pinned profile does not hold the login: %s", got)
	}
	names, _ := m.ProfileNames(ToolClaude)
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			t.Fatalf("a new profile %s was created for a group with a pin", name)
		}
	}
	for id, path := range paths {
		if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, path))) {
			t.Fatalf("target %s still holds the refresh token", id)
		}
	}
}

// Two different pins in one login family, or a pin on a profile other than the one already storing the token, cannot be
// honoured: the migration refuses before changing anything.
func TestMigratingRefusesAGroupWithConflictingPins(t *testing.T) {
	for name, specs := range map[string][]extSpec{
		"two logins pinned to one profile":              {{"a", "other", futureClaude("first-login")}, {"b", "other", futureClaude("second-login")}},
		"two groups, one pinned only through a sibling": {{"a", "", futureClaude("first-family")}, {"a2", "other", futureClaude("first-family")}, {"b", "other", futureClaude("second-family")}},
		"two distinct pins":                             {{"a", "main", futureClaude("family")}, {"b", "other", futureClaude("family")}},
		"pin differs from the holder":                   {{"a", "main", fullClaude("tok-other", "ref-other", time.Now().Add(6*time.Hour))}},
		"sibling pin differs from holder":               {{"a", "", fullClaude("tok-other", "ref-other", time.Now().Add(6*time.Hour))}, {"b", "main", fullClaude("tok-other", "ref-other", time.Now().Add(6*time.Hour))}},
	} {
		t.Run(name, func(t *testing.T) {
			m, home := linkedInstallWithHome(t)
			paths := externalTargets(t, m, home, specs...)
			before := map[string]string{}
			for id, path := range paths {
				before[id] = readFile(t, path)
			}
			_, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}})
			if !errors.Is(err, ErrLoginPending) {
				t.Fatalf("migrate = %v, want ErrLoginPending", err)
			}
			if m.TokenCopiesFor(ToolClaude) {
				t.Fatal("a marker was written")
			}
			for id, path := range paths {
				if readFile(t, path) != before[id] {
					t.Fatalf("target %s was touched", id)
				}
			}
		})
	}
}

// Two profiles holding one token are fine when the pin is one of them: the login is already stored there, nothing new is
// filed and the target is stripped.
func TestMigratingAcceptsAPinAmongSeveralHolders(t *testing.T) {
	m, home := linkedInstallWithHome(t)
	holder := fullClaude("tok-other", "ref-other", time.Now().Add(6*time.Hour))
	must(t, m.Add(ToolClaude, "twin"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "twin"), []byte(holder), 0o600))
	paths := externalTargets(t, m, home, extSpec{"pinned", "other", holder})
	if _, err := m.MigrateToTokenCopies(MigrateOptions{Tools: []string{ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	names, _ := m.ProfileNames(ToolClaude)
	for _, name := range names {
		if strings.HasPrefix(name, "login-") {
			t.Fatalf("a new profile %s was created for a login already stored", name)
		}
	}
	if credtarget.HasRefreshToken(ToolClaude, []byte(readFile(t, paths["pinned"]))) {
		t.Fatal("the pinned target still holds the refresh token")
	}
}
