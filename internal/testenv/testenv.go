// Package testenv keeps the tests of this module off the machine they run on. It is imported by _test.go files only.
//
// Several commands read and write the person's real credentials and state through defaults that start at their home directory: ~/.claude
// and ~/.codex (logins), ~/.vibe (Vibe's config and key), ~/.aimebu and ~/.aimebu2 (the servers' data, the account switcher and its lock). A test that reaches such a
// default without pointing it somewhere else reads those credentials, can overwrite them, and takes the same locks as the running v1
// server, so it can wait for it for as long as the server works, with no deadline and no output. Every package that has tests calls Main
// from its TestMain (a test in this package fails when one does not), and Main refuses to run when the environment it just set up is not
// under the temporary directory.
package testenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Variables names every environment variable a command takes a home or a data directory from.
var Variables = []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "VIBE_HOME", "AIMEBU_CONFIG_DIR", "AIMEBU2_CONFIG_DIR", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR", "OPENCODE_CONFIG"}

// Isolate points HOME and the tool and server data directories at a new temporary root, so every default that starts at the home directory
// lands there, and returns the root and the function that removes it. Go's own caches stay where they were: a test that runs the go tool
// (the install wrapper's) must not start from an empty build cache. It panics when the result is not isolated.
func Isolate() (root string, cleanup func()) {
	pinGoCacheEnv()
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		panic(fmt.Errorf("resolve temporary directory: %w", err))
	}
	if err := os.Setenv("TMPDIR", temp); err != nil {
		panic(err)
	}
	root, err = os.MkdirTemp("", "aimebu-v2-test-home-")
	if err != nil {
		panic(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		panic(fmt.Errorf("resolve test home: %w", err))
	}
	for name, value := range map[string]string{
		"HOME":                      root,
		"CODEX_HOME":                filepath.Join(root, ".codex"),
		"CLAUDE_CONFIG_DIR":         filepath.Join(root, ".claude"),
		"VIBE_HOME":                 filepath.Join(root, ".vibe"),
		"XDG_CONFIG_HOME":           filepath.Join(root, ".config"),
		"PI_CODING_AGENT_DIR":       filepath.Join(root, ".pi", "agent"),
		"OPENCODE_CONFIG":           filepath.Join(root, ".config", "opencode", "opencode.json"),
		"AIMEBU_CONFIG_DIR":         filepath.Join(root, ".aimebu"),
		"AIMEBU2_CONFIG_DIR":        filepath.Join(root, ".aimebu2"),
		"AIMEBU2_DEV_SKIP_V1_PROBE": "1",
		// Error reporting is forced off: a test must never send anything to Sentry, whatever DSN the machine or a default ships.
		"AIMEBU2_SENTRY_DSN": "off",
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	if err := Check(); err != nil {
		_ = os.RemoveAll(root)
		panic(err)
	}
	return root, func() { _ = os.RemoveAll(root) }
}

func pinGoCacheEnv() {
	// Read the go command's effective values before HOME changes. They may
	// come from `go env -w`, rather than the usual home-based defaults.
	out, err := exec.Command("go", "env", "GOCACHE", "GOPATH", "GOMODCACHE").Output()
	if err != nil {
		panic(fmt.Errorf("read effective Go cache paths: %w", err))
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(values) != 3 {
		panic(fmt.Errorf("go env returned %d cache paths, want 3", len(values)))
	}
	setDefault("GOCACHE", values[0])
	setDefault("GOPATH", values[1])
	setDefault("GOMODCACHE", values[2])
}

func setDefault(name, value string) {
	if os.Getenv(name) == "" {
		_ = os.Setenv(name, value)
	}
}

// Main is a TestMain for a package that needs nothing else: it isolates the environment, runs the tests and removes the root.
func Main(m *testing.M) {
	_, cleanup := Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Check reports an environment in which the home directory, or a data directory that is set, is outside the temporary directory.
func Check() error {
	if got := os.Getenv("AIMEBU2_SENTRY_DSN"); got != "off" {
		return fmt.Errorf("AIMEBU2_SENTRY_DSN is %q in the test environment, want \"off\": a test must never send error reports", got)
	}
	temp, err := resolve(os.TempDir())
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("the test environment has no home directory: %w", err)
	}
	paths := map[string]string{"the home directory (os.UserHomeDir)": home}
	for _, name := range Variables {
		if value := os.Getenv(name); value != "" {
			paths[name] = value
		}
	}
	for name, path := range paths {
		resolved, err := resolve(path)
		if err != nil {
			return err
		}
		if !under(temp, resolved) {
			return fmt.Errorf("%s is %q, outside the temporary directory %q: a test run here would use the real machine's credentials and data", name, path, temp)
		}
	}
	return nil
}

// resolve makes a path absolute and follows the symlinks of its existing part (/var is /private/var on macOS), so two spellings of one
// place compare equal.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		if filepath.Dir(dir) == dir {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

func under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

// BasicTools are the commands a test needs when it runs a shell script or builds with the go tool.
var BasicTools = []string{"sh", "bash", "env", "sleep", "uname", "tr", "mktemp", "rm", "dirname", "basename", "readlink", "cat", "ls", "ps", "kill",
	"chmod", "mkdir", "cp", "mv", "touch", "head", "tail", "date", "grep", "sed", "awk", "wc", "sort", "tee", "git", "go"}

// RestrictPATH makes the tools a test may run the only ones on PATH: a directory of links to the named tools (the BasicTools when none are
// named; a tool that is not installed is left out), so the tests stop running the harness CLIs of whoever runs them. Before this, the
// tests that start a server ran its supervisor's discovery against the real PATH: the person's own claude, codex, opencode, pi and vibe, their
// docker launchers, and docker, as real processes with the person's configuration, on every run. A test that wants a tool on PATH puts
// its own fake there, as it already does.
func RestrictPATH(root string, tools ...string) {
	if len(tools) == 0 {
		tools = BasicTools
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		panic(err)
	}
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		_ = os.Symlink(path, filepath.Join(bin, tool))
	}
	if err := os.Setenv("PATH", bin); err != nil {
		panic(err)
	}
	if err := os.Setenv("AIMEBU2_TEST_RESTRICT_PATH", "1"); err != nil {
		panic(err)
	}
}
