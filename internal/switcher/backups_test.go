package switcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedBackup(t *testing.T, m *Manager, tool, name string, ts int64, body string) string {
	t.Helper()
	path := m.backupPath(tool, name, ts)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestBackupsPrunedToCap(t *testing.T) {
	m, _ := newTestManager(t)
	var seeded []string
	for i := int64(1); i <= 15; i++ {
		seeded = append(seeded, seedBackup(t, m, ToolClaude, "a", 1000+i, futureClaude(fmt.Sprint(i))))
	}
	must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(futureClaude("new"))))

	backups, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(backups) != maxProfileBackups {
		t.Fatalf("kept %d backups, want %d", len(backups), maxProfileBackups)
	}
	// The newest write plus the 9 newest seeded survive; the 6 oldest go.
	for i, p := range seeded {
		if want := i >= 6; exists(p) != want {
			t.Errorf("seeded backup %s exists=%v, want %v", filepath.Base(p), exists(p), want)
		}
	}
}

func TestBackupPruneIgnoresPrefixCollisionsAndUnrelatedFiles(t *testing.T) {
	m, _ := newTestManager(t)
	var others []string
	for i := int64(1); i <= 15; i++ {
		others = append(others, seedBackup(t, m, ToolClaude, "a-b", 1000+i, futureClaude("ab")))
	}
	dir := filepath.Join(m.backupsRoot(), ToolClaude)
	unrelated := []string{"a-notes.json", "a-123.txt", "a-.json", "readme"}
	for _, n := range unrelated {
		must(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
	}
	for i := int64(1); i <= 12; i++ {
		seedBackup(t, m, ToolClaude, "a", 2000+i, futureClaude("a"))
	}
	must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(futureClaude("new"))))

	for _, p := range others {
		if !exists(p) {
			t.Fatalf("pruning a removed a-b backup %s", filepath.Base(p))
		}
	}
	for _, n := range unrelated {
		if !exists(filepath.Join(dir, n)) {
			t.Fatalf("pruning removed unrelated file %s", n)
		}
	}
	backups, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(backups) != maxProfileBackups {
		t.Fatalf("kept %d a backups, want %d", len(backups), maxProfileBackups)
	}
}

func TestBackupPruneKeepsNewestValidBeyondCap(t *testing.T) {
	m, _ := newTestManager(t)
	good := futureClaude("good")
	oldest := seedBackup(t, m, ToolClaude, "a", 1000, futureClaude("older-valid"))
	validPath := seedBackup(t, m, ToolClaude, "a", 1001, good)
	for i := int64(1); i <= 9; i++ {
		seedBackup(t, m, ToolClaude, "a", 2000+i, corruptClaude())
	}
	// The write makes 10 invalid backups newer than the only valid ones (with
	// different bytes, so it is not skipped as a duplicate of the newest).
	must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(`{"claudeAiOauth":{"accessToken":"new","refreshToken":"r","expiresAt":0}}`)))

	if !exists(validPath) {
		t.Fatal("newest valid backup was pruned")
	}
	if exists(oldest) {
		t.Fatal("older valid backup beyond the cap should be pruned")
	}
	backups, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(backups) != maxProfileBackups+1 {
		t.Fatalf("kept %d backups, want %d (cap + newest valid)", len(backups), maxProfileBackups+1)
	}
	if data, ok := m.newestValidBackup(ToolClaude, "a"); !ok || string(data) != good {
		t.Fatalf("newestValidBackup = %q %v after prune", data, ok)
	}
}

func TestSwitchBackupIsPruned(t *testing.T) {
	m, home := newTestManager(t)
	must(t, m.SetEnabled(true))
	writeLive(t, home, ToolClaude, futureClaude("a"))
	must(t, m.Import(ToolClaude, "a"))
	must(t, m.Add(ToolClaude, "b"))
	must(t, os.WriteFile(m.profileCredPath(ToolClaude, "b"), []byte(futureClaude("b")), 0o600))
	for i := int64(1); i <= 12; i++ {
		seedBackup(t, m, ToolClaude, "a", 1000+i, futureClaude("old"))
	}
	// The harness rotates a's tokens, so the switch-away backup differs from
	// the import backup and is really written.
	writeLive(t, home, ToolClaude, futureClaude("a-rotated"))
	_, _, err := m.Switch(ToolClaude, "b") // step-4 backup of a
	must(t, err)
	backups, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(backups) != maxProfileBackups {
		t.Fatalf("switch-away backup not pruned: %d backups", len(backups))
	}
}

func TestRenameMovesBackupsAndClearsLeftovers(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("x"))
	must(t, m.Import(ToolClaude, "x")) // keeps the inactive profiles below inactive
	must(t, m.Add(ToolClaude, "a"))
	a1, a2, earlier := futureClaude("a1"), futureClaude("a2"), futureClaude("earlier")
	seedBackup(t, m, ToolClaude, "a", 1000, a1)
	seedBackup(t, m, ToolClaude, "a", 1001, a2)
	// Left by an earlier, removed profile named "renamed": another account.
	seedBackup(t, m, ToolClaude, "renamed", 1005, earlier)
	other := seedBackup(t, m, ToolClaude, "a-b", 1000, futureClaude("ab"))

	must(t, m.Rename(ToolClaude, "a", "renamed"))

	if got, _ := m.profileBackups(ToolClaude, "a"); len(got) != 0 {
		t.Fatalf("old-name backups left behind: %v", got)
	}
	if exists(m.backupPath(ToolClaude, "renamed", 1005)) {
		t.Fatal("leftover backup of an earlier profile survived the rename")
	}
	assertRegularFile(t, m.backupPath(ToolClaude, "renamed", 1000), a1)
	assertRegularFile(t, m.backupPath(ToolClaude, "renamed", 1001), a2)
	got, err := m.profileBackups(ToolClaude, "renamed")
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("renamed profile has %d backups, want the 2 moved ones", len(got))
	}
	if !exists(other) {
		t.Fatal("rename moved a-b's backup")
	}
}

func TestRemoveDeletesProfileBackups(t *testing.T) {
	m, home := newTestManager(t)
	writeLive(t, home, ToolClaude, futureClaude("x"))
	must(t, m.Import(ToolClaude, "x"))
	must(t, m.Add(ToolClaude, "a"))
	seedBackup(t, m, ToolClaude, "a", 1000, futureClaude("a"))
	other := seedBackup(t, m, ToolClaude, "a-b", 1000, futureClaude("ab"))

	must(t, m.Remove(ToolClaude, "a", false))
	if got, _ := m.profileBackups(ToolClaude, "a"); len(got) != 0 {
		t.Fatalf("removed profile's backups survive: %v", got)
	}
	if !exists(other) {
		t.Fatal("Remove deleted a-b's backup")
	}
}

// TestRecreatedProfileDoesNotInheritOldAccount: leftover backups under a name
// (from before Remove deleted them) are cleared by Add and Import, so a later
// detach can never restore the old account into the new profile.
func TestRecreatedProfileDoesNotInheritOldAccount(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		m, home := newTestManager(t)
		writeLive(t, home, ToolClaude, futureClaude("x"))
		must(t, m.Import(ToolClaude, "x"))
		seedBackup(t, m, ToolClaude, "a", 1000, futureClaude("old-account"))
		must(t, m.Add(ToolClaude, "a"))
		if got, _ := m.profileBackups(ToolClaude, "a"); len(got) != 0 {
			t.Fatalf("Add kept leftover backups: %v", got)
		}
	})
	t.Run("import", func(t *testing.T) {
		m, home := newTestManager(t)
		mine := futureClaude("mine")
		writeLive(t, home, ToolClaude, mine)
		seedBackup(t, m, ToolClaude, "a", 1000, futureClaude("old-account"))
		must(t, m.Import(ToolClaude, "a"))
		got, err := m.profileBackups(ToolClaude, "a")
		must(t, err)
		if len(got) != 1 {
			t.Fatalf("want only Import's own backup, got %v", got)
		}
		assertRegularFile(t, got[0].path, mine)

		// A later import of another login detaches a to its own bytes.
		writeLive(t, home, ToolClaude, futureClaude("other"))
		must(t, m.Import(ToolClaude, "b"))
		assertRegularFile(t, m.profileCredPath(ToolClaude, "a"), mine)
	})
}

// TestRenameActiveWithAbsentLiveThenImportRestoresMovedBackup: with no live
// file at rename time there is nothing fresh to back up, so only the moved
// backups keep the renamed profile recoverable.
func TestRenameActiveWithAbsentLiveThenImportRestoresMovedBackup(t *testing.T) {
	m, home := newTestManager(t)
	credA := futureClaude("a")
	livePath := writeLive(t, home, ToolClaude, credA)
	must(t, m.Import(ToolClaude, "a")) // import backup a-<unix>.json
	must(t, os.Remove(livePath))
	must(t, m.Rename(ToolClaude, "a", "renamed"))

	writeLive(t, home, ToolClaude, futureClaude("b")) // a different login
	must(t, m.Import(ToolClaude, "b"))
	assertRegularFile(t, m.profileCredPath(ToolClaude, "renamed"), credA)
}

func TestWriteUniqueBackupSkipsDuplicateOfNewest(t *testing.T) {
	m, _ := newTestManager(t)
	older, newest := futureClaude("older"), futureClaude("newest")
	seedBackup(t, m, ToolClaude, "a", 1000, older)
	seedBackup(t, m, ToolClaude, "a", 1001, newest)

	for i := 0; i < 3; i++ {
		must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(newest)))
	}
	if got, _ := m.profileBackups(ToolClaude, "a"); len(got) != 2 {
		t.Fatalf("duplicate of the newest backup was written: %d backups", len(got))
	}

	// Equal to an OLDER backup only: still written, and newest by suffix.
	must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(older)))
	got, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(got) != 3 {
		t.Fatalf("want 3 backups, got %d", len(got))
	}
	assertRegularFile(t, got[0].path, older)
}

func TestWriteUniqueBackupStaysNewestPastSteppedTimestamps(t *testing.T) {
	m, _ := newTestManager(t)
	future := time.Now().Add(time.Hour).Unix()
	seedBackup(t, m, ToolClaude, "a", future, futureClaude("stepped"))
	fresh := futureClaude("fresh")
	must(t, m.writeUniqueBackup(ToolClaude, "a", []byte(fresh)))
	got, err := m.profileBackups(ToolClaude, "a")
	must(t, err)
	if len(got) != 2 || got[0].ts != future+1 {
		t.Fatalf("new backup should sort newest: %+v", got)
	}
	assertRegularFile(t, got[0].path, fresh)
}

// TestImportStateWriteFailureRollsBack forces the final state.json write to
// fail (the switcher root is made read-only, so the state temp file cannot be
// created) and checks Import leaves no trace: the new profile and its backup
// are gone, and the still-active previous profile is linked again — no
// inactive profile is a symlink.
func TestImportStateWriteFailureRollsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the state write cannot be made to fail")
	}
	m, home := newTestManager(t)
	livePath := writeLive(t, home, ToolClaude, futureClaude("a"))
	must(t, m.Import(ToolClaude, "a"))
	writeLive(t, home, ToolClaude, futureClaude("b")) // a different login
	must(t, os.Chmod(m.Root(), 0o500))
	t.Cleanup(func() { _ = os.Chmod(m.Root(), 0o700) })

	err := m.Import(ToolClaude, "b")
	if err == nil || !strings.Contains(err.Error(), "update state") {
		t.Fatalf("Import should fail at the state write, got %v", err)
	}
	must(t, os.Chmod(m.Root(), 0o700))

	if exists(m.profileDir(ToolClaude, "b")) {
		t.Fatal("new profile dir survived the rollback")
	}
	if got, _ := m.profileBackups(ToolClaude, "b"); len(got) != 0 {
		t.Fatalf("new profile's backup survived the rollback: %v", got)
	}
	if active, _ := m.Active(ToolClaude); active != "a" {
		t.Fatalf("active = %q, want a", active)
	}
	assertLinkedToLive(t, m, ToolClaude, "a", livePath)
	profiles, listErr := m.List()
	must(t, listErr)
	for _, p := range profiles {
		info, err := os.Lstat(m.profileCredPath(p.Tool, p.Name))
		if err == nil && !p.Active && info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("inactive profile %s/%s is a symlink after rollback", p.Tool, p.Name)
		}
	}
}
