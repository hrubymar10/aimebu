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
	"syscall"
	"testing"
)

// Variables names every environment variable a command takes a home or a data directory from.
var Variables = []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "VIBE_HOME", "AIMEBU_CONFIG_DIR", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR", "OPENCODE_CONFIG"}

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
	// Do not inherit runtime settings from a real installation. In particular,
	// the host's AIMEBU_URL may point at a live bus on another machine.
	for _, name := range []string{"AIMEBU_URL", "AIMEBU_PORT", "AIMEBU_TLS_PORT", "AIMEBU_BIND", "AIMEBU_ALLOW", "AIMEBU_ALLOWED_HOSTS", "AIMEBU_TLS_CERT", "AIMEBU_TLS_KEY", "AIMEBU_PUBLIC_URL", "AIMEBU_AGENT_DEBUG", "AIMEBU_FORCE_BUILD", "AIMEBU_UPDATE_RESTARTED", "AIMEBU_OWNER_LOGIN", "AIMEBU_OWNER_PASSWORD"} {
		if err := os.Unsetenv(name); err != nil {
			panic(err)
		}
	}
	for name, value := range map[string]string{
		"HOME":                     root,
		"CODEX_HOME":               filepath.Join(root, ".codex"),
		"CLAUDE_CONFIG_DIR":        filepath.Join(root, ".claude"),
		"VIBE_HOME":                filepath.Join(root, ".vibe"),
		"XDG_CONFIG_HOME":          filepath.Join(root, ".config"),
		"PI_CODING_AGENT_DIR":      filepath.Join(root, ".pi", "agent"),
		"OPENCODE_CONFIG":          filepath.Join(root, ".config", "opencode", "opencode.json"),
		"AIMEBU_CONFIG_DIR":        filepath.Join(root, ".aimebu"),
		"AIMEBU_DEV_SKIP_V1_PROBE": "1",
		// Error reporting is forced off: a test must never send anything to Sentry, whatever DSN the machine or a default ships.
		"AIMEBU_SENTRY_DSN": "off",
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	// Git reads a system and a global config too, and a runner's system config can carry filters (git-lfs) that win over the ones a test
	// sets in a repository. Every git a test runs, or a server it starts runs, skips the system config and reads an empty global one.
	gitGlobal := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitGlobal, nil, 0o600); err != nil {
		panic(err)
	}
	for name, value := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": gitGlobal} {
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
	if got := os.Getenv("AIMEBU_URL"); got != "" {
		return fmt.Errorf("AIMEBU_URL is set in the test environment: a test must never contact a configured server")
	}
	if got := os.Getenv("AIMEBU_SENTRY_DSN"); got != "off" {
		return fmt.Errorf("AIMEBU_SENTRY_DSN is %q in the test environment, want \"off\": a test must never send error reports", got)
	}
	if os.Getenv("GIT_CONFIG_NOSYSTEM") != "1" {
		return fmt.Errorf("GIT_CONFIG_NOSYSTEM is not 1 in the test environment: the machine's system git config (git-lfs filters) would reach the tests")
	}
	if os.Getenv("GIT_CONFIG_GLOBAL") == "" {
		return fmt.Errorf("GIT_CONFIG_GLOBAL is not set in the test environment: the person's global git config would reach the tests")
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
	paths["GIT_CONFIG_GLOBAL"] = os.Getenv("GIT_CONFIG_GLOBAL")
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
	if err := os.Setenv("AIMEBU_TEST_RESTRICT_PATH", "1"); err != nil {
		panic(err)
	}
}

// WriteExecutable writes a file a test is going to run, so that running it cannot fail with "text file busy" (ETXTBSY).
//
// Two things cause that failure and a plain os.WriteFile has both. While the file is open for writing, a process forked by another goroutine inherits
// the descriptor and keeps it until its exec, and an exec of the file in that window is refused. And opening for writing a file that a process is
// executing is refused, which a test that rewrites its launcher between two runs does. So the contents go to a temporary file in the same directory,
// written while holding the read side of syscall.ForkLock (os/exec takes the write side around every fork, so none happens while the descriptor is open),
// and the file is renamed over the target: the target is never open for writing, and replacing one that is running is allowed.
func WriteExecutable(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".exec-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once the rename has moved it
	syscall.ForkLock.RLock()
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}
