package usages

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two stores over one directory stand in for two processes (the server and
// `aimebu switcher use`): neither shares memory with the other.

func TestRefreshStateIsSharedBetweenProcesses(t *testing.T) {
	root := t.TempDir()
	server, cli := newRefreshStateStore(root), newRefreshStateStore(root)
	if err := cli.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountNeedsReloginSoon, SpentHash: "h1", Since: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	// The server never restarted and never loaded the file before, yet it must see
	// the terminal state the other process wrote: otherwise it would spend that
	// token again.
	if e, ok := server.account("claude", "h1"); !ok || e.State != AccountNeedsReloginSoon {
		t.Fatalf("the server missed the other process's terminal state: %+v %v", e, ok)
	}
	// And a write by the server must not erase the other process's journal entry.
	if err := cli.setJournal(refreshJournalEntry{Tool: "codex", Name: "work", Phase: journalSpending, SpentHash: "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := server.setAccount(refreshAccountEntry{Tool: "codex", Name: "other", State: AccountReauthRequired, SpentHash: "h3", Since: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	if j := cli.journal(); len(j) != 1 || j[0].SpentHash != "h2" {
		t.Fatalf("the server's save lost the other process's journal entry: %+v", j)
	}
	if _, ok := cli.account("claude", "h1"); !ok {
		t.Fatal("the server's save lost the other process's account state")
	}
	if _, ok := cli.account("codex", "h3"); !ok {
		t.Fatal("the other process does not see the server's state")
	}
	// A clear by one process is seen by the other.
	if err := server.clearJournal("codex", "work"); err != nil {
		t.Fatal(err)
	}
	if j := cli.journal(); len(j) != 0 {
		t.Fatalf("a cleared entry is still visible to the other process: %+v", j)
	}
}

func TestRefreshStateConcurrentWritersLoseNothing(t *testing.T) {
	root := t.TempDir()
	const writers, each = 4, 12
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		store := newRefreshStateStore(root) // one per "process"
		wg.Add(1)
		go func(w int, store *refreshStateStore) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := store.setAccount(refreshAccountEntry{Tool: "claude", Name: "n", State: AccountRefreshFailed, SpentHash: fmt.Sprintf("w%d-%d", w, i), Since: time.Unix(1, 0)}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w, store)
	}
	wg.Wait()
	reader := newRefreshStateStore(root)
	for w := 0; w < writers; w++ {
		for i := 0; i < each; i++ {
			if _, ok := reader.account("claude", fmt.Sprintf("w%d-%d", w, i)); !ok {
				t.Fatalf("writer %d lost entry %d to a concurrent writer", w, i)
			}
		}
	}
}

func TestRefreshStateKeepsAChangeThatCouldNotBeSaved(t *testing.T) {
	root := t.TempDir()
	s := newRefreshStateStore(root)
	failing := true
	s.write = func(path string, data []byte, mode os.FileMode) error {
		if failing {
			return errors.New("disk full")
		}
		return atomicWriteBytes(path, data, mode)
	}
	if err := s.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountNeedsReloginSoon, SpentHash: "h1", Since: time.Unix(1, 0)}); err == nil {
		t.Fatal("the save error must be returned")
	}
	// The terminal state is not forgotten although the disk refused it, even after
	// the file is re-read.
	if e, ok := s.account("claude", "h1"); !ok || e.State != AccountNeedsReloginSoon {
		t.Fatalf("an unsaved terminal state was forgotten: %+v %v", e, ok)
	}
	notices := s.drainNotices()
	if len(notices) != 1 || notices[0].Kind != "state_save_failed" || !strings.Contains(notices[0].Detail, "disk full") {
		t.Fatalf("notices = %+v", notices)
	}
	// Once the disk works again the next write carries the earlier change with it.
	failing = false
	if err := s.setJournal(refreshJournalEntry{Tool: "codex", Name: "work", Phase: journalSpending, SpentHash: "h2"}); err != nil {
		t.Fatal(err)
	}
	if e, ok := newRefreshStateStore(root).account("claude", "h1"); !ok || e.State != AccountNeedsReloginSoon {
		t.Fatalf("the retried change never reached the disk: %+v %v", e, ok)
	}
}

func TestRefreshStateClearJournalReportsASaveFailure(t *testing.T) {
	root := t.TempDir()
	s := newRefreshStateStore(root)
	if err := s.setJournal(refreshJournalEntry{Tool: "claude", Name: "main", Phase: journalSpending, SpentHash: "h"}); err != nil {
		t.Fatal(err)
	}
	s.write = func(string, []byte, os.FileMode) error { return errors.New("read-only file system") }
	if err := s.clearJournal("claude", "main"); err == nil {
		t.Fatal("clearJournal must return the save error")
	}
}

func TestRefreshStateSetsAnUnreadableFileAside(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":        "{not json",
		"future version": `{"version":9,"journal":{},"accounts":{}}`,
		"truncated":      `{"version":1,"journal":{"claude/main":{"tool":"claude"`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, refreshStateFileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := newRefreshStateStore(root)
			// A reader never changes anything on disk.
			if j := s.journal(); len(j) != 0 {
				t.Fatalf("journal = %+v", j)
			}
			if _, err := os.Stat(path + ".corrupt"); err == nil {
				t.Fatal("a read must not move the file")
			}
			// The first write sets it aside instead of overwriting the evidence.
			if err := s.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountReauthRequired, SpentHash: "h", Since: time.Unix(1, 0)}); err != nil {
				t.Fatal(err)
			}
			kept, err := os.ReadFile(path + ".corrupt")
			if err != nil || string(kept) != content {
				t.Fatalf("the unreadable file was not kept intact: %q %v", kept, err)
			}
			notices := s.drainNotices()
			if len(notices) != 1 || notices[0].Kind != "state_quarantined" || !strings.Contains(notices[0].Detail, "refresh-state.json.corrupt") {
				t.Fatalf("notices = %+v", notices)
			}
			// A second corruption does not overwrite the first set-aside file.
			if err := os.WriteFile(path, []byte("{again"), 0o600); err != nil {
				t.Fatal(err)
			}
			s2 := newRefreshStateStore(root)
			if err := s2.setAccount(refreshAccountEntry{Tool: "claude", Name: "main", State: AccountReauthRequired, SpentHash: "h2", Since: time.Unix(1, 0)}); err != nil {
				t.Fatal(err)
			}
			if again, err := os.ReadFile(path + ".corrupt.1"); err != nil || string(again) != "{again" {
				t.Fatalf("the second set-aside file is missing: %q %v", again, err)
			}
			if first, _ := os.ReadFile(path + ".corrupt"); string(first) != content {
				t.Fatal("the first set-aside file was overwritten")
			}
		})
	}
}

func TestRefreshTempDirsLiveUnderTheSwitcherRoot(t *testing.T) {
	root := t.TempDir()
	dir, cred, err := writeClaudeCredTempIn(root, []byte(`{"claudeAiOauth":{}}`), "aimebu-claude-refresh-*")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Join(root, refreshTempDirName) {
		t.Fatalf("temp dir %s is not under %s", dir, filepath.Join(root, refreshTempDirName))
	}
	if info, err := os.Stat(filepath.Dir(dir)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("refresh-tmp mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(cred); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("temp credential mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	codexDir, _, err := writeCodexAuthTempIn(root, []byte(`{}`))
	if err != nil || filepath.Dir(codexDir) != filepath.Join(root, refreshTempDirName) {
		t.Fatalf("codex temp dir %s, %v", codexDir, err)
	}
	// Without a root the helpers keep using the system temp dir.
	plain, _, err := writeClaudeCredTemp([]byte(`{"claudeAiOauth":{}}`), "aimebu-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(plain)
	if strings.HasPrefix(plain, root) {
		t.Fatalf("an empty root must not use %s", root)
	}
}

func TestClearJournalRemovesTheRunsTempDirButOnlyOurs(t *testing.T) {
	root := t.TempDir()
	s := newRefreshStateStore(root)
	own, _, err := writeClaudeCredTempIn(root, []byte(`{"claudeAiOauth":{}}`), "aimebu-claude-refresh-*")
	if err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range []refreshJournalEntry{
		{Tool: "claude", Name: "own", Phase: journalSpending, SpentHash: "a", TempDir: own},
		{Tool: "claude", Name: "foreign", Phase: journalSpending, SpentHash: "b", TempDir: foreign},
		{Tool: "claude", Name: "parent", Phase: journalSpending, SpentHash: "c", TempDir: refreshTempRoot(root)},
		{Tool: "claude", Name: "nested", Phase: journalSpending, SpentHash: "d", TempDir: filepath.Join(own, "..", "..", filepath.Base(foreign))},
	} {
		if err := s.setJournal(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"own", "foreign", "parent", "nested"} {
		if err := s.clearJournal("claude", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatal("the run's own temp dir was not removed")
	}
	if _, err := os.Stat(filepath.Join(foreign, "keep")); err != nil {
		t.Fatal("a path outside refresh-tmp must never be removed on a journal entry's word")
	}
	if _, err := os.Stat(refreshTempRoot(root)); err != nil {
		t.Fatal("the refresh-tmp root itself must not be removed")
	}
}

func TestRefreshTempSweepKeepsReferencedAndYoungDirs(t *testing.T) {
	root := t.TempDir()
	parent := refreshTempRoot(root)
	mk := func(name string, age time.Duration) string {
		dir := filepath.Join(parent, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	oldReferenced := mk("old-referenced", time.Minute)
	s := newRefreshStateStore(root)
	if err := s.setJournal(refreshJournalEntry{Tool: "claude", Name: "main", Phase: journalSpending, SpentHash: "h", TempDir: oldReferenced}); err != nil {
		t.Fatal(err)
	}
	// Only now do the dirs age: the first store's own first write already swept.
	oldOrphan := mk("old-orphan", 2*time.Hour)
	mk("old-referenced", 2*time.Hour)
	young := mk("young", time.Minute)
	// A fresh store (a restart) sweeps before its first write.
	restarted := newRefreshStateStore(root)
	if err := restarted.setAccount(refreshAccountEntry{Tool: "claude", Name: "x", State: AccountRefreshFailed, SpentHash: "z", Since: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Fatal("an old unreferenced temp dir was not swept")
	}
	if _, err := os.Stat(oldReferenced); err != nil {
		t.Fatal("a temp dir a journal entry refers to was swept")
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatal("a young temp dir (a run that may still be starting) was swept")
	}
}
