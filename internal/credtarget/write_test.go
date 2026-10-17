package credtarget

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// claudeTarget lays out a fake Claude config dir under t.TempDir.
func claudeTarget(t *testing.T) Target {
	t.Helper()
	return Target{ID: "live/claude", Tool: ToolClaude, Path: filepath.Join(t.TempDir(), ".claude", ".credentials.json"), Role: RoleLive}
}

func codexTarget(t *testing.T) Target {
	t.Helper()
	return Target{ID: "live/codex", Tool: ToolCodex, Path: filepath.Join(t.TempDir(), ".codex", "auth.json"), Role: RoleLive}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func inode(t *testing.T, path string) (uint64, time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino, info.ModTime()
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteTargetCreatesTheFileAndItsDirectoryPrivately(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		target Target
		full   string
	}{{claudeTarget(t), fullClaudeA}, {codexTarget(t), fullCodexA}} {
		w := &Writer{}
		if err := w.WriteTarget(tc.target, []byte(tc.full)); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(tc.target.Path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: stat %v %v", tc.target.Tool, info, err)
		}
		if dirInfo, _ := os.Stat(filepath.Dir(tc.target.Path)); dirInfo.Mode().Perm() != 0o700 {
			t.Fatalf("%s: directory mode %v", tc.target.Tool, dirInfo.Mode().Perm())
		}
		want, _ := BuildTarget(tc.target.Tool, nil, []byte(tc.full))
		if got := readFile(t, tc.target.Path); !bytes.Equal(got, want) {
			t.Fatalf("%s: content\n got %s\nwant %s", tc.target.Tool, got, want)
		}
		if names := dirNames(t, filepath.Dir(tc.target.Path)); len(names) != 1 {
			t.Fatalf("%s: leftovers next to the target: %v", tc.target.Tool, names)
		}
	}
}

func TestWriteTargetPreservesWhatTheServerDoesNotOwn(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, []byte(claudeTargetBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Writer{}).WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	got := string(readFile(t, target.Path))
	tail := claudeTargetBefore[strings.Index(claudeTargetBefore, `  "mcpOAuth"`):]
	if !strings.HasSuffix(got, tail) {
		t.Fatalf("members the server does not own changed:\n%s", got)
	}
}

func TestWriteTargetDoesNotTouchAnUnchangedFile(t *testing.T) {
	t.Parallel()
	target := codexTarget(t)
	w := &Writer{}
	if err := w.WriteTarget(target, []byte(fullCodexA)); err != nil {
		t.Fatal(err)
	}
	ino, mtime := inode(t, target.Path)
	time.Sleep(20 * time.Millisecond)
	if err := w.WriteTarget(target, []byte(fullCodexA)); err != nil {
		t.Fatal(err)
	}
	if ino2, mtime2 := inode(t, target.Path); ino2 != ino || !mtime2.Equal(mtime) {
		t.Fatalf("an idle write rewrote the file (inode %d→%d, mtime %v→%v)", ino, ino2, mtime, mtime2)
	}
	// A different credential does replace it.
	other := strings.ReplaceAll(fullCodexA, fakeAccessA, fakeAccessB)
	if err := w.WriteTarget(target, []byte(other)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readFile(t, target.Path), []byte(fakeAccessB)) {
		t.Fatal("new credential not written")
	}
}

func TestWriteTargetFixesALaxModeEvenWhenTheContentIsCurrent(t *testing.T) {
	t.Parallel()
	target := codexTarget(t)
	w := &Writer{}
	if err := w.WriteTarget(target, []byte(fullCodexA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteTarget(target, []byte(fullCodexA)); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(target.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode left at %v", info.Mode().Perm())
	}
}

// A /login that landed in the target holds the only copy of a new refresh token.
func TestWriteTargetWillNotDestroyAnUnimportedLogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		target Target
		login  string
		full   string
	}{
		{claudeTarget(t), `{"claudeAiOauth":{"accessToken":"NEW","refreshToken":"` + fakeRefresh + `"},"mcpOAuth":{}}`, fullClaudeA},
		{codexTarget(t), `{"tokens":{"access_token":"NEW","refresh_token":"` + fakeRefresh + `","account_id":"a"}}`, fullCodexA},
	} {
		if err := os.MkdirAll(filepath.Dir(tc.target.Path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tc.target.Path, []byte(tc.login), 0o600); err != nil {
			t.Fatal(err)
		}
		w := &Writer{}
		if err := w.WriteTarget(tc.target, []byte(tc.full)); !errors.Is(err, ErrRefreshTokenPresent) {
			t.Fatalf("%s: err = %v", tc.target.Tool, err)
		}
		if got := readFile(t, tc.target.Path); string(got) != tc.login {
			t.Fatalf("%s: the login was modified: %s", tc.target.Tool, got)
		}
		// A flag cannot be flipped under a concurrent fan-out: the guard stays.
		if err := w.WriteTarget(tc.target, []byte(tc.full)); !errors.Is(err, ErrRefreshTokenPresent) {
			t.Fatalf("%s: the guard is not unconditional: %v", tc.target.Tool, err)
		}
		// Only the explicit per-call method, used after the import, replaces it.
		if err := w.WriteImportedLogin(tc.target, []byte(tc.full)); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, tc.target.Path); bytes.Contains(got, []byte(fakeRefresh)) {
			t.Fatalf("%s: refresh token survived the overwrite: %s", tc.target.Tool, got)
		}
	}
}

// A blank refresh token (codex's own target form) is not a login.
func TestWriteTargetAcceptsTheTargetFormItWrote(t *testing.T) {
	t.Parallel()
	target := codexTarget(t)
	w := &Writer{}
	for i := 0; i < 3; i++ {
		if err := w.WriteTarget(target, []byte(fullCodexA)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
}

func TestWriteTargetLockLifecycle(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	lockDir := StorageWriteLockPath(filepath.Dir(target.Path))
	w := &Writer{LockTimeout: 200 * time.Millisecond, afterLock: func() {
		if info, err := os.Stat(lockDir); err != nil || !info.IsDir() {
			t.Errorf("lock directory missing while held: %v", err)
		}
	}}
	if err := w.WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock left behind after a write: %v", err)
	}
	// Also released when the write fails.
	if err := os.WriteFile(target.Path, []byte(`{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteTarget(target, []byte(fullClaudeA)); err == nil {
		t.Fatal("a corrupt target was accepted")
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock left behind after a failed write: %v", err)
	}
	if string(readFile(t, target.Path)) != `{"broken` {
		t.Fatal("a corrupt target was overwritten instead of reported")
	}
}

func TestWriteTargetCodexTakesNoLock(t *testing.T) {
	t.Parallel()
	target := codexTarget(t)
	if err := (&Writer{afterLock: func() {
		if _, err := os.Stat(StorageWriteLockPath(filepath.Dir(target.Path))); err == nil {
			t.Error("codex write took a storage lock")
		}
	}}).WriteTarget(target, []byte(fullCodexA)); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTargetBacksOffWhileAWorkerHoldsTheLock(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	dir := filepath.Dir(target.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, []byte(claudeTargetBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	lockDir := StorageWriteLockPath(dir)
	if err := os.Mkdir(lockDir, 0o700); err != nil { // a worker is mid-write
		t.Fatal(err)
	}
	start := time.Now()
	err := (&Writer{LockTimeout: 150 * time.Millisecond}).WriteTarget(target, []byte(fullClaudeA))
	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Fatalf("gave up after %v, before the timeout", time.Since(start))
	}
	if string(readFile(t, target.Path)) != claudeTargetBefore {
		t.Fatal("file changed although the lock was not ours")
	}
	if _, err := os.Stat(lockDir); err != nil {
		t.Fatal("the worker's lock was removed by a contender that was not entitled to")
	}
}

func TestWriteTargetTakesOverAStaleLock(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	dir := filepath.Dir(target.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockDir := StorageWriteLockPath(dir)
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-storageLockStale - 5*time.Second)
	if err := os.Chtimes(lockDir, old, old); err != nil { // its holder died long ago
		t.Fatal(err)
	}
	if err := (&Writer{LockTimeout: time.Second}).WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock left behind")
	}
}

// Workers write mcpOAuth into the same file under the same lock; the server's
// read-modify-write must wait for them and keep what they saved.
func TestWriteTargetKeepsAWorkersMCPLogin(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	dir := filepath.Dir(target.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, []byte(`{"claudeAiOauth":{"accessToken":"OLD"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	lockDir := StorageWriteLockPath(dir)
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the worker saves an MCP login, then lets go
		defer wg.Done()
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(target.Path, []byte(`{"claudeAiOauth":{"accessToken":"OLD"},"mcpOAuth":{"plane":{"accessToken":"MCP-SAVED"}}}`), 0o600); err != nil {
			t.Error(err)
		}
		_ = os.Remove(lockDir)
	}()
	if err := (&Writer{LockTimeout: 3 * time.Second}).WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	got := readFile(t, target.Path)
	if !bytes.Contains(got, []byte("MCP-SAVED")) || !bytes.Contains(got, []byte(fakeAccessA)) {
		t.Fatalf("lost update:\n%s", got)
	}
}

func TestWriteTargetKeepsItsLockAliveWhileWriting(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	lockDir := StorageWriteLockPath(filepath.Dir(target.Path))
	var before, after time.Time
	w := &Writer{HeartbeatInterval: 20 * time.Millisecond, afterLock: func() {
		info, _ := os.Stat(lockDir)
		before = info.ModTime()
		time.Sleep(250 * time.Millisecond) // a slow disk
		info, _ = os.Stat(lockDir)
		after = info.ModTime()
	}}
	if err := w.WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	if !after.After(before) {
		t.Fatalf("lock mtime never refreshed (%v → %v): a contender would take it for stale", before, after)
	}
}

// A concurrent reader sees the previous document or the next one, never a
// partial file and never a refresh token.
func TestWriteTargetIsAtomicForReaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		target Target
		a, b   string
	}{
		{claudeTarget(t), fullClaudeA, fullClaudeBNo},
		{codexTarget(t), fullCodexA, strings.ReplaceAll(fullCodexA, fakeAccessA, fakeAccessB)},
	} {
		w := &Writer{}
		if err := w.WriteTarget(tc.target, []byte(tc.a)); err != nil {
			t.Fatal(err)
		}
		var stop atomic.Bool
		var reads, bad atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				data, err := os.ReadFile(tc.target.Path)
				if err != nil {
					bad.Add(1)
					continue
				}
				reads.Add(1)
				if !json.Valid(data) || bytes.Contains(data, []byte(fakeRefresh)) || len(data) == 0 {
					bad.Add(1)
				}
			}
		}()
		for i := 0; i < 60; i++ {
			next := tc.a
			if i%2 == 0 {
				next = tc.b
			}
			if err := w.WriteTarget(tc.target, []byte(next)); err != nil {
				t.Fatal(err)
			}
		}
		stop.Store(true)
		wg.Wait()
		if bad.Load() != 0 || reads.Load() == 0 {
			t.Fatalf("%s: %d bad reads out of %d", tc.target.Tool, bad.Load(), reads.Load())
		}
		if names := dirNames(t, filepath.Dir(tc.target.Path)); len(names) != 1 {
			t.Fatalf("%s: leftovers: %v", tc.target.Tool, names)
		}
	}
}

// During the migration the live file is a symlink to the stored profile. The
// rewrite replaces the link with a regular file and leaves the profile alone.
func TestWriteTargetReplacesASymlinkWithoutTouchingItsTarget(t *testing.T) {
	t.Parallel()
	target := claudeTarget(t)
	dir := filepath.Dir(target.Path)
	store := filepath.Join(t.TempDir(), "profile", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte(fullClaudeA), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, target.Path); err != nil {
		t.Fatal(err)
	}
	w := &Writer{}
	if err := w.WriteTarget(target, []byte(fullClaudeA)); !errors.Is(err, ErrRefreshTokenPresent) {
		t.Fatalf("a linked live file holds the full credential and must not be overwritten silently: %v", err)
	}
	// migrate-copies saved the full credential to the store first, so it may.
	if err := w.WriteImportedLogin(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target.Path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("target still a link or lax: %v %v", info, err)
	}
	if got := readFile(t, store); string(got) != fullClaudeA {
		t.Fatalf("the stored profile was modified:\n%s", got)
	}
	if got := readFile(t, target.Path); bytes.Contains(got, []byte(fakeRefresh)) {
		t.Fatalf("live file kept the refresh token:\n%s", got)
	}
}

func TestWriteTargetLocksTheRealDirectoryBehindASymlink(t *testing.T) {
	t.Parallel()
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	target := Target{Tool: ToolClaude, Path: filepath.Join(link, ".credentials.json")}
	var held string
	w := &Writer{afterLock: func() {
		if _, err := os.Stat(StorageWriteLockPath(real)); err == nil {
			held = "real"
		}
	}}
	if err := w.WriteTarget(target, []byte(fullClaudeA)); err != nil {
		t.Fatal(err)
	}
	if held != "real" {
		t.Fatal("the lock was not taken on the resolved directory, where Claude takes it")
	}
}

func TestWriteTargetRejectsWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	w := &Writer{}
	if err := w.WriteTarget(Target{Tool: "mistral", Path: "/tmp/x"}, []byte(`{}`)); err == nil {
		t.Error("unknown tool accepted")
	}
	if err := w.WriteTarget(Target{Tool: ToolClaude, Path: "relative/.credentials.json"}, []byte(fullClaudeA)); err == nil {
		t.Error("relative path accepted")
	}
	target := claudeTarget(t)
	if err := w.WriteTarget(target, []byte(`{"claudeAiOauth":{}}`)); err == nil {
		t.Error("a credential without an access token was written")
	}
	if _, err := os.Stat(target.Path); err == nil {
		t.Error("a rejected credential left a file behind")
	}
}

// Concurrent fan-out and strip share one Writer: the strip's permission cannot
// leak into the fan-out running next to it.
func TestImportedLoginPermissionIsPerCall(t *testing.T) {
	t.Parallel()
	target := codexTarget(t)
	login := `{"tokens":{"access_token":"NEW","refresh_token":"` + fakeRefresh + `","account_id":"a"}}`
	if err := os.MkdirAll(filepath.Dir(target.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, []byte(login), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &Writer{}
	var wg sync.WaitGroup
	var refused atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := w.WriteTarget(target, []byte(fullCodexA)); errors.Is(err, ErrRefreshTokenPresent) {
					refused.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if refused.Load() != 160 || string(readFile(t, target.Path)) != login {
		t.Fatalf("a fan-out overwrote an unimported login (%d of 160 refused)", refused.Load())
	}
}
