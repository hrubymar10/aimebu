package switcher

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

var packageTestRoot string

func TestMain(m *testing.M) {
	root, cleanup := testenv.Isolate()
	packageTestRoot = root
	// The default running-codex scan reads the host's process table and docker
	// containers; a test must never depend on what happens to run on the machine.
	runningCodexScan = func() ([]string, string, error) { return nil, "tests: no host scan", nil }
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestDevelopmentSwitchWaitsForV1LockWithoutChangingV1State(t *testing.T) {
	m, home := newTestManager(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	writeLive(t, home, ToolClaude, futureClaude("first"))
	if err := m.Import(ToolClaude, "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ToolClaude, "second"); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(home, ".aimebu", "switcher")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyState := filepath.Join(legacyDir, "state.json")
	if err := os.WriteFile(legacyState, []byte("v1 state stays put"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(legacyDir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := m.Switch(ToolClaude, "second"); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("development switch ignored the v1 lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("development switch did not resume after v1 lock release")
	}
	data, err := os.ReadFile(legacyState)
	if err != nil || string(data) != "v1 state stays put" {
		t.Fatalf("v1 state changed: %q %v", data, err)
	}
}

func TestDevelopmentSwitchDoesNotCreateMissingV1Lock(t *testing.T) {
	m, home := newTestManager(t)
	legacyDir := filepath.Join(home, ".aimebu", "switcher")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, ".lock")); !os.IsNotExist(err) {
		t.Fatalf("development switch created v1 lock: %v", err)
	}
}

func TestCutoverRootUsesSingleFlock(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".aimebu", "switcher")
	m, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(home)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.SetEnabled(true) }()
	select {
	case err := <-done:
		t.Fatalf("cutover switcher ignored its lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cutover switcher did not resume after lock release")
	}
}

func TestCutoverAliasUsesSingleFlock(t *testing.T) {
	home := t.TempDir()
	legacyRoot := filepath.Join(home, ".aimebu")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "switcher"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(legacyRoot, alias); err != nil {
		t.Fatal(err)
	}
	m, err := New(filepath.Join(alias, "switcher"))
	if err != nil {
		t.Fatal(err)
	}
	m.SetHomeDir(home)
	done := make(chan error, 1)
	go func() { done <- m.SetEnabled(true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("same-file alias took the same flock twice")
	}
}

func TestAllDefaultPathsStayUnderTemporaryRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(packageTestRoot)
	if err != nil {
		t.Fatal(err)
	}
	switchRoot, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(switchRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{switchRoot, m.lockPath()}
	for _, tool := range Tools() {
		path, err := m.liveCredPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("switcher path escaped test root: %q (root %q)", path, root)
		}
	}
}
