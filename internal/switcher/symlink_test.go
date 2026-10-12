package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The active profile's stored credential file is a symlink to the live
// credential file; inactive profiles hold regular files. These tests pin that
// invariant across Import, Switch (including failure and dangling-link paths),
// Rename, and Remove.

func assertLinkedToLive(t *testing.T, m *Manager, tool, name, livePath string) {
	t.Helper()
	stored := m.profileCredPath(tool, name)
	info, err := os.Lstat(stored)
	if err != nil {
		t.Fatalf("lstat %s/%s stored creds: %v", tool, name, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s/%s stored creds should be a symlink, mode=%v", tool, name, info.Mode())
	}
	target, err := os.Readlink(stored)
	must(t, err)
	absLive, err := filepath.Abs(livePath)
	must(t, err)
	if target != absLive {
		t.Fatalf("%s/%s link target = %q, want absolute live path %q", tool, name, target, absLive)
	}
	if got, want := readFile(t, stored), readFile(t, livePath); got != want {
		t.Fatalf("%s/%s stored read through link = %q, want live %q", tool, name, got, want)
	}
}

func assertRegularFile(t *testing.T, path, want string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s should be a regular file, mode=%v", path, info.Mode())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %v, want 0600", path, info.Mode().Perm())
	}
	if got := readFile(t, path); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func credsFor(tool, label string) string {
	if tool == ToolCodex {
		return futureCodex(label + "@example.com")
	}
	return futureClaude("tok-" + label)
}

func TestAtomicWriteReplacesLinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")
	must(t, os.WriteFile(target, []byte("live"), 0o600))
	must(t, os.Symlink(target, link))

	must(t, atomicWrite(link, []byte("snapshot"), 0o600))

	assertRegularFile(t, link, "snapshot")
	if got := readFile(t, target); got != "live" {
		t.Fatalf("atomicWrite wrote through the link: target = %q", got)
	}
}

func TestImportLinksStoredToLive(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			m, home := newTestManager(t)
			livePath := writeLive(t, home, tool, credsFor(tool, "a"))
			must(t, m.Import(tool, "a"))
			assertLinkedToLive(t, m, tool, "a", livePath)

			// The link follows the harness rotating the live file.
			rotated := credsFor(tool, "a-rotated")
			must(t, os.WriteFile(livePath, []byte(rotated), 0o600))
			if got := readFile(t, m.profileCredPath(tool, "a")); got != rotated {
				t.Fatalf("stored path did not follow live rotation: %q", got)
			}
		})
	}
}

func TestSwitchLinksActiveAndSnapshotsOutgoing(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			m, home := newTestManager(t)
			must(t, m.SetEnabled(true))
			credA := credsFor(tool, "a")
			credB := credsFor(tool, "b")
			livePath := writeLive(t, home, tool, credA)
			must(t, m.Import(tool, "a"))
			must(t, m.Add(tool, "b"))
			must(t, os.WriteFile(m.profileCredPath(tool, "b"), []byte(credB), 0o600))

			// The harness rotates A's tokens while A is active.
			credA2 := credsFor(tool, "a2")
			must(t, os.WriteFile(livePath, []byte(credA2), 0o600))

			_, switched, err := m.Switch(tool, "b")
			must(t, err)
			if !switched {
				t.Fatal("switched = false")
			}
			// Outgoing A: regular snapshot of the rotated live bytes; capture
			// replaced the link instead of writing through it.
			assertRegularFile(t, m.profileCredPath(tool, "a"), credA2)
			// Live holds B; incoming B is now the link.
			assertRegularFile(t, livePath, credB)
			assertLinkedToLive(t, m, tool, "b", livePath)

			// B rotates while active, then switch back: B is snapshotted with
			// the rotated bytes, A becomes the link.
			credB2 := credsFor(tool, "b2")
			must(t, os.WriteFile(livePath, []byte(credB2), 0o600))
			_, _, err = m.Switch(tool, "a")
			must(t, err)
			assertRegularFile(t, m.profileCredPath(tool, "b"), credB2)
			assertRegularFile(t, livePath, credA2)
			assertLinkedToLive(t, m, tool, "a", livePath)
		})
	}
}

func TestSwitchToEmptyFromLinkedSnapshotsOutgoing(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA := futureClaude("tok-a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "empty"))

	_, _, err := m.Switch(ToolClaude, "empty")
	must(t, err)

	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), credA)
	if _, err := os.Lstat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live should be deleted for an empty target; err=%v", err)
	}
	if _, err := os.Lstat(m.profileCredPath(ToolClaude, "empty")); !os.IsNotExist(err) {
		t.Fatalf("empty active profile should have no stored file; err=%v", err)
	}
}

// TestSwitchAwayFromDanglingLink: the live file vanished (harness logout) while
// the active profile's stored file links to it. Listing must not fail, and
// switching away must succeed, leaving the outgoing profile without stored
// credentials rather than with a dangling link.
func TestSwitchAwayFromDanglingLink(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	livePath := writeLive(t, home, ToolClaude, futureClaude("tok-a"))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	credB := futureClaude("tok-b")
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(credB), 0o600))
	must(t, os.Remove(livePath))

	// Listing and eligibility with a dangling active link.
	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Name == "a" && p.HasCreds {
			t.Fatal("profile with a dangling link reported HasCreds=true")
		}
	}
	els, err := m.Eligibility()
	must(t, err)
	if el := findEligibility(t, els, ToolClaude); !el.Absent {
		t.Fatalf("eligibility with dangling link = %+v, want Absent", el)
	}

	_, switched, err := m.Switch(ToolClaude, "b")
	must(t, err)
	if !switched {
		t.Fatal("switched = false")
	}
	if _, err := os.Lstat(m.profileCredPath(ToolClaude, "a")); !os.IsNotExist(err) {
		t.Fatalf("outgoing dangling link should be removed; lstat err=%v", err)
	}
	assertLinkedToLive(t, m, ToolClaude, "b", livePath)

	profiles, err = m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Name == "a" && p.HasCreds {
			t.Fatal("outgoing profile should have no stored credentials")
		}
	}
}

// TestListInactiveDanglingLink: a dangling link on an inactive profile (not
// produced by the switcher itself, but possible by hand) reads as "no stored
// credentials" rather than an error. Switching to it behaves like switching to
// an empty profile: the outgoing login is captured, live is cleared for a
// fresh login, and the dangling link is dropped.
func TestListInactiveDanglingLink(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA := futureClaude("tok-a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "c"))
	must(t, os.Symlink(filepath.Join(home, "gone.json"), m.profileCredPath(ToolClaude, "c")))

	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Name == "c" && p.HasCreds {
			t.Fatal("inactive dangling link reported HasCreds=true")
		}
	}
	_, _, err = m.Switch(ToolClaude, "c")
	must(t, err)
	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), credA)
	if _, err := os.Lstat(livePath); !os.IsNotExist(err) {
		t.Fatalf("live should be cleared; err=%v", err)
	}
	if _, err := os.Lstat(m.profileCredPath(ToolClaude, "c")); !os.IsNotExist(err) {
		t.Fatalf("dangling target link should be dropped; err=%v", err)
	}
}

// TestSwitchRestoreFailureRelinksOutgoing: a failed restore rolls live back and
// the still-active outgoing profile ends up linked again, not as a stale copy.
func TestSwitchRestoreFailureRelinksOutgoing(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA := futureClaude("tok-a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, os.MkdirAll(m.profileDir(ToolClaude, "b"), 0o700))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte("not-json"), 0o600))

	if _, _, err := m.Switch(ToolClaude, "b"); err == nil {
		t.Fatal("expected restore failure")
	}
	assertRegularFile(t, livePath, credA)
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
	if active, _ := m.Active(ToolClaude); active != "a" {
		t.Fatalf("active = %q, want a", active)
	}
}

// TestSwitchBackupFailureRelinksOutgoing forces a failure right after capture
// (the backup dir cannot be created) and checks the outgoing profile is
// re-linked rather than left holding the capture snapshot.
func TestSwitchBackupFailureRelinksOutgoing(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA := futureClaude("tok-a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(futureClaude("tok-b")), 0o600))
	// A regular file where backups/ should be makes the backup mkdir fail.
	must(t, os.RemoveAll(m.backupsRoot()))
	must(t, os.WriteFile(m.backupsRoot(), []byte("x"), 0o600))

	if _, _, err := m.Switch(ToolClaude, "b"); err == nil {
		t.Fatal("expected backup failure")
	}
	assertRegularFile(t, livePath, credA)
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
}

func TestRenameKeepsActiveLink(t *testing.T) {
	m, home := newTestManager(t)
	livePath := writeLive(t, home, ToolCodex, futureCodex("a@example.com"))
	must(t, m.Import(ToolCodex, "old"))
	must(t, m.Rename(ToolCodex, "old", "new"))
	assertLinkedToLive(t, m, ToolCodex, "new", livePath)
}

// TestRemoveDeletesLinkNotTarget: removing a profile whose stored file is a
// symlink unlinks it without touching the file it points at. (Removing the
// ACTIVE profile needs force and then deletes live on purpose — see
// TestRemoveActiveProfileGuard.)
func TestRemoveDeletesLinkNotTarget(t *testing.T) {
	m, home := newTestManager(t)
	credA := futureClaude("tok-a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "other"))
	must(t, os.Symlink(livePath, m.profileCredPath(ToolClaude, "other")))

	must(t, m.Remove(ToolClaude, "other", false))
	if _, err := os.Stat(m.profileDir(ToolClaude, "other")); !os.IsNotExist(err) {
		t.Fatalf("profile dir should be gone; err=%v", err)
	}
	assertRegularFile(t, livePath, credA)
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)

	err := m.Remove(ToolClaude, "a", false)
	if !errors.Is(err, ErrActiveProfile) {
		t.Fatalf("Remove(active, force=false) = %v, want ErrActiveProfile", err)
	}
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
}

// ── EnsureActiveLinks migration ───────────────────────────────────────────────

// legacyActive sets up an install from before active profiles were linked:
// profile a is active and its stored file is a plain copy holding storedBody.
func legacyActive(t *testing.T, tool string, enabled bool, liveBody, storedBody string) (*Manager, string) {
	t.Helper()
	m, home := newTestManager(t)
	must(t, m.SetEnabled(enabled))
	livePath := writeLive(t, home, tool, liveBody)
	must(t, m.Import(tool, "a"))
	stored := m.profileCredPath(tool, "a")
	must(t, os.Remove(stored))
	must(t, os.WriteFile(stored, []byte(storedBody), 0o600))
	// Pre-link installs wrote no import backup.
	must(t, os.RemoveAll(m.backupsRoot()))
	return m, livePath
}

func backupsFor(t *testing.T, m *Manager, tool string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(m.backupsRoot(), tool, "*.json"))
	must(t, err)
	return matches
}

func TestEnsureActiveLinksBacksUpStaleCopyAndLinks(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			live, stale := credsFor(tool, "live"), credsFor(tool, "stale")
			m, livePath := legacyActive(t, tool, true, live, stale)

			must(t, m.EnsureActiveLinks())
			assertLinkedToLive(t, m, tool, "a", livePath)
			assertRegularFile(t, livePath, live)
			backups := backupsFor(t, m, tool)
			if len(backups) != 1 {
				t.Fatalf("want 1 backup of the stale copy, got %v", backups)
			}
			assertRegularFile(t, backups[0], stale)
			if !strings.HasPrefix(filepath.Base(backups[0]), "a-") {
				t.Fatalf("backup name %q should follow <name>-<unix>.json", backups[0])
			}

			// Idempotent: a second run changes nothing and writes no new backup.
			must(t, m.EnsureActiveLinks())
			assertLinkedToLive(t, m, tool, "a", livePath)
			if got := backupsFor(t, m, tool); len(got) != 1 {
				t.Fatalf("second run wrote backups: %v", got)
			}
		})
	}
}

func TestEnsureActiveLinksIdenticalBytesStillBackedUp(t *testing.T) {
	live := futureClaude("tok")
	m, livePath := legacyActive(t, ToolClaude, true, live, live)
	must(t, m.EnsureActiveLinks())
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
	backups := backupsFor(t, m, ToolClaude)
	if len(backups) != 1 {
		t.Fatalf("identical bytes should still get one backup, got %v", backups)
	}
	assertRegularFile(t, backups[0], live)

	// Idempotent: an already-correct link writes no new backup.
	must(t, m.EnsureActiveLinks())
	if got := backupsFor(t, m, ToolClaude); len(got) != 1 {
		t.Fatalf("second run wrote backups: %v", got)
	}
}

func TestEnsureActiveLinksMissingStoredFileLinked(t *testing.T) {
	live := futureClaude("tok")
	m, livePath := legacyActive(t, ToolClaude, true, live, live)
	must(t, os.Remove(m.profileCredPath(ToolClaude, "a")))
	must(t, m.EnsureActiveLinks())
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
}

func TestEnsureActiveLinksLeavesInactiveProfiles(t *testing.T) {
	live, stale := futureClaude("live"), futureClaude("stale")
	m, livePath := legacyActive(t, ToolClaude, true, live, stale)
	must(t, m.Add(ToolClaude, "b"))
	credB := futureClaude("b")
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(credB), 0o600))

	must(t, m.EnsureActiveLinks())
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
	assertRegularFile(t, m.profileCredPath(ToolClaude, "b"), credB)
}

func TestEnsureActiveLinksDisabledSwitcherUntouched(t *testing.T) {
	live, stale := futureClaude("live"), futureClaude("stale")
	m, _ := legacyActive(t, ToolClaude, false, live, stale)
	must(t, m.EnsureActiveLinks())
	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), stale)
	if got := backupsFor(t, m, ToolClaude); len(got) != 0 {
		t.Fatalf("disabled switcher wrote backups: %v", got)
	}
}

func TestEnsureActiveLinksAbsentLiveUntouched(t *testing.T) {
	live, stale := futureClaude("live"), futureClaude("stale")
	m, livePath := legacyActive(t, ToolClaude, true, live, stale)
	must(t, os.Remove(livePath))
	must(t, m.EnsureActiveLinks())
	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), stale)
	if got := backupsFor(t, m, ToolClaude); len(got) != 0 {
		t.Fatalf("absent live wrote backups: %v", got)
	}
}

func TestEnsureActiveLinksCorruptLiveUntouched(t *testing.T) {
	live, stale := futureClaude("live"), futureClaude("stale")
	m, livePath := legacyActive(t, ToolClaude, true, live, stale)
	must(t, os.WriteFile(livePath, []byte(corruptClaude()), 0o600))
	must(t, m.EnsureActiveLinks())
	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), stale)
}

// ── Import while another profile is active ────────────────────────────────────

// relogin simulates the harness logging into a different account outside the
// switcher: the live file now holds another login.
func relogin(t *testing.T, livePath, body string) {
	t.Helper()
	must(t, os.WriteFile(livePath, []byte(body), 0o600))
}

func TestImportWhileActiveDetachesPreviousToLastKnownBytes(t *testing.T) {
	for _, tool := range Tools() {
		t.Run(tool, func(t *testing.T) {
			m, home := newTestManager(t)
			credA, credB := credsFor(tool, "a"), credsFor(tool, "b")
			livePath := writeLive(t, home, tool, credA)
			must(t, m.Import(tool, "a"))
			relogin(t, livePath, credB)

			must(t, m.Import(tool, "b"))
			// The old profile is a regular file again, holding its own account
			// (from the import-time backup), not the new login.
			assertRegularFile(t, m.profileCredPath(tool, "a"), credA)
			assertLinkedToLive(t, m, tool, "b", livePath)
			if active, _ := m.Active(tool); active != "b" {
				t.Fatalf("active = %q, want b", active)
			}
		})
	}
}

func TestImportWhileActiveAfterSwitchUsesLinkTimeBackup(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	credA, credB, credC := futureClaude("a"), futureClaude("b"), futureClaude("c")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(credB), 0o600))
	_, _, err := m.Switch(ToolClaude, "b")
	must(t, err)
	relogin(t, livePath, credC)

	must(t, m.Import(ToolClaude, "c"))
	assertRegularFile(t, m.profileCredPath(ToolClaude, "b"), credB)
	assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), credA)
	assertLinkedToLive(t, m, ToolClaude, "c", livePath)
}

func TestImportWhileActiveWithoutBackupDropsLink(t *testing.T) {
	m, home := newTestManager(t)
	livePath := writeLive(t, home, ToolClaude, futureClaude("a"))
	must(t, m.Import(ToolClaude, "a"))
	must(t, os.RemoveAll(m.backupsRoot()))
	credB := futureClaude("b")
	relogin(t, livePath, credB)

	must(t, m.Import(ToolClaude, "b"))
	if _, err := os.Lstat(m.profileCredPath(ToolClaude, "a")); !os.IsNotExist(err) {
		t.Fatalf("old profile without a backup should become empty; lstat err=%v", err)
	}
	profiles, err := m.List()
	must(t, err)
	for _, p := range profiles {
		if p.Name == "a" && p.HasCreds {
			t.Fatal("old profile must not report the new login as its credentials")
		}
	}
}

func TestImportWritesBackup(t *testing.T) {
	m, home := newTestManager(t)
	cred := futureCodex("a@example.com")
	writeLive(t, home, ToolCodex, cred)
	must(t, m.Import(ToolCodex, "a"))
	backups := backupsFor(t, m, ToolCodex)
	if len(backups) != 1 || !strings.HasPrefix(filepath.Base(backups[0]), "a-") {
		t.Fatalf("import should write one a-<unix>.json backup, got %v", backups)
	}
	assertRegularFile(t, backups[0], cred)
}

func TestRenameActiveThenImportRestoresUnderNewName(t *testing.T) {
	m, home := newTestManager(t)
	credA := futureClaude("a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a"))
	must(t, os.RemoveAll(m.backupsRoot())) // only the rename-time backup remains
	must(t, m.Rename(ToolClaude, "a", "renamed"))
	relogin(t, livePath, futureClaude("b"))

	must(t, m.Import(ToolClaude, "b"))
	assertRegularFile(t, m.profileCredPath(ToolClaude, "renamed"), credA)
}

func TestNewestValidBackupIgnoresPrefixCollisionsAndInvalid(t *testing.T) {
	m, _ := newTestManager(t)
	dir := filepath.Join(m.backupsRoot(), ToolClaude)
	must(t, os.MkdirAll(dir, 0o700))
	good := futureClaude("good")
	must(t, os.WriteFile(filepath.Join(dir, "a-100.json"), []byte(good), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a-200.json"), []byte(corruptClaude()), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "a-b-300.json"), []byte(futureClaude("other")), 0o600))
	data, ok := m.newestValidBackup(ToolClaude, "a")
	if !ok || string(data) != good {
		t.Fatalf("newestValidBackup = %q %v, want the valid a-100 backup", data, ok)
	}
}

func TestEnsureActiveLinksNeverUsedRootNotCreated(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, m.EnsureActiveLinks())
	if _, err := os.Stat(m.Root()); !os.IsNotExist(err) {
		t.Fatalf("EnsureActiveLinks created the switcher root; stat err=%v", err)
	}
}

func TestEnsureActiveLinksDisabledDoesNotCreateLock(t *testing.T) {
	m, _ := newTestManager(t)
	must(t, os.MkdirAll(m.Root(), 0o700))
	must(t, m.saveState(State{Version: stateVersion, Enabled: false, Active: map[string]string{ToolClaude: "a"}}))
	must(t, m.EnsureActiveLinks())
	if _, err := os.Stat(m.lockPath()); !os.IsNotExist(err) {
		t.Fatalf("disabled switcher got a .lock; stat err=%v", err)
	}
}
