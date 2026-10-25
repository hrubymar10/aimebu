package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

var packageTestRoot string

// TestMain keeps every test of this package off the real machine (see internal/testenv). Most tests set their own environment, but one
// that does not would read and write the person's real ~/.aimebu, ~/.claude and ~/.codex, take the blocking lock on the real v1 switcher
// (which a running v1 server holds for as long as it works, and the test then waits for it, at 0% CPU, with nothing printed), and read
// their credentials.
func TestMain(m *testing.M) {
	// `server start` runs this test binary as the detached server (see daemon_test.go): it already has the parent test's isolated
	// environment, so it runs the command line it was given and nothing else.
	if os.Getenv(daemonHelperEnv) == "1" {
		// The test that started this server may die without cleaning up (a timeout kills the whole test binary); a server left behind keeps
		// whatever it inherited and its port, so it goes when its test process does.
		if watched, err := strconv.Atoi(os.Getenv(daemonWatchEnv)); err == nil && watched > 0 {
			go func() {
				for {
					time.Sleep(500 * time.Millisecond)
					if err := syscall.Kill(watched, 0); errors.Is(err, syscall.ESRCH) {
						os.Exit(1)
					}
				}
			}()
		}
		main()
		return
	}
	root, cleanup := testenv.Isolate()
	packageTestRoot = root
	// The tests that start a server run its supervisor's harness discovery: without this they ran the real claude, codex, opencode and pi
	// of whoever ran them, their docker launchers and docker itself. A test that needs a tool puts its own fake on PATH.
	testenv.RestrictPATH(root)
	// Docker is never contacted by a test unless it replaces this itself: the check runs `docker info`, which on a machine with Docker
	// Desktop stopped can take a long time, and which tells nothing about the code under test.
	dockerReachable = func() bool { return false }
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// The defaults every command falls back to (no variable set by the test) stay under the temporary root.
func TestDefaultPathsStayUnderTemporaryRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(packageTestRoot)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	config, err := resolveServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"home": home, "server root": config.root, "CODEX_HOME": os.Getenv("CODEX_HOME"), "CLAUDE_CONFIG_DIR": os.Getenv("CLAUDE_CONFIG_DIR"), "AIMEBU_CONFIG_DIR": os.Getenv("AIMEBU_CONFIG_DIR")} {
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		if resolved, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
			abs = filepath.Join(resolved, filepath.Base(abs))
		}
		if rel, err := filepath.Rel(root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("%s escaped the test root: %q (root %q)", name, path, root)
		}
	}
}
