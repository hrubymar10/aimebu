package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The wrapper builds ./cmd/aimebu from the repository around it. These tests run the real
// bin/aimebu against a tiny stand-in module in a temporary git repository, so they exercise
// the wrapper's own logic (cache, forced build, version, symlink) without compiling aimebu.

func binWrapperRepo(t *testing.T, tag string) (repo string, env []string) {
	t.Helper()
	for _, tool := range []string{"bash", "go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	wrapper, err := os.ReadFile(filepath.Join("..", "..", "bin", "aimebu"))
	if err != nil {
		t.Fatal(err)
	}
	repo = t.TempDir()
	writeBinWrapperFile(t, filepath.Join(repo, "bin", "aimebu"), string(wrapper), 0o755)
	writeBinWrapperFile(t, filepath.Join(repo, "go.mod"), "module standin\n\ngo 1.21\n", 0o644)
	setBinWrapperSource(t, repo, "one")

	goCache := goEnv(t, "GOCACHE")
	env = append(cleanBinWrapperEnv(),
		"HOME="+t.TempDir(), // never the real home
		"GOCACHE="+goCache,
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
		"AIMEBU_FORCE_BUILD=",
		"TMPDIR="+t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if tag != "" {
		git := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
			cmd.Dir, cmd.Env = repo, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		git("add", "-A")
		git("commit", "-q", "-m", "stand-in")
		git("tag", tag)
	}
	return repo, env
}

// cleanBinWrapperEnv is the current environment without the update settings, so a
// developer's exported AIMEBU2_UPDATE_* or git prompt settings cannot change the tests.
func cleanBinWrapperEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AIMEBU2_UPDATE_") || strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func goEnv(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func writeBinWrapperFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// setBinWrapperSource rewrites the stand-in program; "marker" shows which build ran.
func setBinWrapperSource(t *testing.T, repo, marker string) {
	t.Helper()
	writeBinWrapperFile(t, filepath.Join(repo, "cmd", "aimebu", "main.go"), `package main

import (
	"fmt"
	"os"
)

var version = "default-version"

func main() { fmt.Println("`+marker+` "+version, os.Args[1:]) }
`, 0o644)
}

func runBinWrapper(t *testing.T, script string, env []string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(script, args...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &errs, env
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\nstdout: %s\nstderr: %s", script, args, err, out.String(), errs.String())
	}
	return strings.TrimSpace(out.String()), errs.String()
}

func TestBinWrapperBuildsOnceAndReusesTheCache(t *testing.T) {
	repo, env := binWrapperRepo(t, "v9.9.9")
	script := filepath.Join(repo, "bin", "aimebu")
	cache := filepath.Join(repo, "aimebu-"+runtime.GOOS+"-"+unameMachine(t))

	out, errs := runBinWrapper(t, script, env, "version", "x")
	if out != "one aimebu v9.9.9 [version x]" {
		t.Fatalf("first run printed %q", out)
	}
	if !strings.Contains(errs, "Building aimebu") {
		t.Fatalf("first run did not build: %q", errs)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache %s was not written: %v", cache, err)
	}

	// A source change is not picked up: the second run reuses the cached binary.
	setBinWrapperSource(t, repo, "two")
	out, errs = runBinWrapper(t, script, env)
	if out != "one aimebu v9.9.9 []" || strings.Contains(errs, "Building") {
		t.Fatalf("second run rebuilt: out=%q stderr=%q", out, errs)
	}
	// Any value other than 1 leaves the cache in place.
	forced0 := append(env, "AIMEBU_FORCE_BUILD=0")
	out, errs = runBinWrapper(t, script, forced0)
	if out != "one aimebu v9.9.9 []" || strings.Contains(errs, "Building") {
		t.Fatalf("AIMEBU_FORCE_BUILD=0 rebuilt: out=%q stderr=%q", out, errs)
	}
}

func TestBinWrapperForcedBuildLeavesTheCacheAndItsTempBinaryAlone(t *testing.T) {
	repo, env := binWrapperRepo(t, "v9.9.9")
	script := filepath.Join(repo, "bin", "aimebu")
	if out, _ := runBinWrapper(t, script, env); out != "one aimebu v9.9.9 []" {
		t.Fatalf("seed run printed %q", out)
	}
	cache := filepath.Join(repo, "aimebu-"+runtime.GOOS+"-"+unameMachine(t))
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	setBinWrapperSource(t, repo, "two")
	forced := append(env, "AIMEBU_FORCE_BUILD=1")
	tmp := tmpDirOf(env)
	out, errs := runBinWrapper(t, script, forced, "a", "b")
	if out != "two aimebu v9.9.9-dirty [a b]" || !strings.Contains(errs, "Building aimebu") {
		t.Fatalf("forced run: out=%q stderr=%q", out, errs)
	}
	after, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a forced build overwrote the cached binary")
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "aimebu-*")); len(left) != 0 {
		t.Fatalf("forced build left a temporary binary behind: %v", left)
	}
}

func TestBinWrapperExitStatusPassesThroughForcedBuilds(t *testing.T) {
	repo, env := binWrapperRepo(t, "v9.9.9")
	writeBinWrapperFile(t, filepath.Join(repo, "cmd", "aimebu", "main.go"), "package main\n\nimport \"os\"\n\nfunc main() { os.Exit(7) }\n", 0o644)
	cmd := exec.Command(filepath.Join(repo, "bin", "aimebu"))
	cmd.Env = append(env, "AIMEBU_FORCE_BUILD=1")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errorsAs(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("forced run error = %v, want exit status 7", err)
	}
}

func TestBinWrapperWorksThroughASymlinkOnPath(t *testing.T) {
	repo, env := binWrapperRepo(t, "v9.9.9")
	link := filepath.Join(t.TempDir(), "aimebu")
	if err := os.Symlink(filepath.Join(repo, "bin", "aimebu"), link); err != nil {
		t.Fatal(err)
	}
	// A relative link chain resolves too.
	dir := t.TempDir()
	rel, err := filepath.Rel(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	chained := filepath.Join(dir, "aimebu")
	if err := os.Symlink(rel, chained); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, chained} {
		if out, _ := runBinWrapper(t, path, env); out != "one aimebu v9.9.9 []" {
			t.Fatalf("%s printed %q", path, out)
		}
	}
}

func TestBinWrapperKeepsTheDefaultVersionWithoutGitMetadata(t *testing.T) {
	repo, env := binWrapperRepo(t, "")
	if out, _ := runBinWrapper(t, filepath.Join(repo, "bin", "aimebu"), env); out != "one default-version []" {
		t.Fatalf("no-git run printed %q, want the unmodified default version", out)
	}
}

func tmpDirOf(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], "TMPDIR="); ok {
			return value
		}
	}
	return os.TempDir()
}

func unameMachine(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func errorsAs(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

// The update check: the wrapper's clone has a local bare repository as its origin, and
// "publish" pushes a new commit (a new stand-in marker) to it from a second clone.

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// binWrapperWithRemote returns a clone whose cached binary is built (marker "one") and
// that tracks a bare remote's trunk branch, plus a function that publishes a new commit.
func binWrapperWithRemote(t *testing.T) (repo string, env []string, script string, publish func(marker string)) {
	t.Helper()
	repo, env = binWrapperRepo(t, "v9.9.9")
	remote := t.TempDir()
	gitIn(t, remote, env, "init", "-q", "--bare")
	gitIn(t, remote, env, "symbolic-ref", "HEAD", "refs/heads/trunk")
	probe := exec.Command("git", "ls-remote", remote)
	probe.Env = env
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("git cannot talk to a local repository here: %v\n%s", err, out)
	}
	gitIn(t, repo, env, "branch", "-M", "trunk")
	gitIn(t, repo, env, "remote", "add", "origin", remote)
	gitIn(t, repo, env, "push", "-q", "origin", "trunk")
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(other), env, "clone", "-q", remote, other)
	publish = func(marker string) {
		t.Helper()
		setBinWrapperSource(t, other, marker)
		gitIn(t, other, env, "commit", "-q", "-am", "publish "+marker)
		gitIn(t, other, env, "push", "-q", "origin", "trunk")
	}
	script = filepath.Join(repo, "bin", "aimebu")
	if out, _ := runBinWrapper(t, script, env); out != "one aimebu v9.9.9 []" {
		t.Fatalf("warm-up run printed %q", out)
	}
	return repo, env, script, publish
}

func runBinWrapperStatus(t *testing.T, script string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(script, args...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &errs, env
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errorsAs(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return strings.TrimSpace(out.String()), errs.String(), code
}

func TestBinWrapperUpdateBehindPullsAndRebuilds(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	apply := append(env, "AIMEBU2_UPDATE_CHECK=apply")

	out, errs := runBinWrapper(t, script, apply, "x")
	if !strings.HasPrefix(out, "two aimebu v9.9.9-1-g") || !strings.HasSuffix(out, "[x]") {
		t.Fatalf("updated run printed %q, want the rebuilt binary's output on stdout only", out)
	}
	if !strings.Contains(errs, "Updated aimebu from ") || !strings.Contains(errs, "Building aimebu") {
		t.Fatalf("update notice missing from stderr: %q", errs)
	}
	if head, remote := gitIn(t, repo, env, "rev-parse", "HEAD"), gitIn(t, repo, env, "rev-parse", "origin/trunk"); head != remote {
		t.Fatalf("HEAD %s is not the remote tip %s", head, remote)
	}
	if left, _ := filepath.Glob(filepath.Join(repo, "aimebu-*.update.*")); len(left) != 0 {
		t.Fatalf("update left a temporary binary behind: %v", left)
	}
	// The next run is current: no notice, no rebuild (a fresh interval would re-check, and
	// there is nothing to do).
	out, errs = runBinWrapper(t, script, append(apply, "AIMEBU2_UPDATE_INTERVAL_HOURS=0"), "run")
	if !strings.HasPrefix(out, "two ") || errs != "" {
		t.Fatalf("current run: out=%q stderr=%q", out, errs)
	}
}

func TestBinWrapperUpdateRunsOnlyOncePerInterval(t *testing.T) {
	_, env, script, publish := binWrapperWithRemote(t)
	apply := append(env, "AIMEBU2_UPDATE_CHECK=apply")
	publish("two")
	if out, _ := runBinWrapper(t, script, apply, "run"); !strings.HasPrefix(out, "two ") {
		t.Fatalf("first check did not update: %q", out)
	}
	publish("three")
	if out, errs := runBinWrapper(t, script, apply, "run"); !strings.HasPrefix(out, "two ") || errs != "" {
		t.Fatalf("a second check inside the interval ran: out=%q stderr=%q", out, errs)
	}
	if out, _ := runBinWrapper(t, script, append(apply, "AIMEBU2_UPDATE_INTERVAL_HOURS=0"), "run"); !strings.HasPrefix(out, "three ") {
		t.Fatalf("a zero interval did not check again: %q", out)
	}
}

func TestBinWrapperUpdateCheckNeedsATerminalOrApply(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	// The test has no terminal, so the default mode never reaches the network or the prompt.
	if out, errs := runBinWrapper(t, script, env, "run"); out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("default mode without a terminal: out=%q stderr=%q", out, errs)
	}
	// AIMEBU2_UPDATE_CHECK=0 beats apply's sibling modes and leaves everything alone.
	if out, errs := runBinWrapper(t, script, append(env, "AIMEBU2_UPDATE_CHECK=0"), "run"); out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("disabled check: out=%q stderr=%q", out, errs)
	}
	// Machine-readable and informational commands skip the check even in apply mode.
	apply := append(env, "AIMEBU2_UPDATE_CHECK=apply")
	for _, args := range [][]string{{"version"}, {"help"}, {"doctor", "--json"}, {"doctor", "--machine"}} {
		if _, errs := runBinWrapper(t, script, apply, args...); errs != "" {
			t.Fatalf("%v printed to stderr: %q", args, errs)
		}
	}
	// A forced (development) build skips the check too.
	if _, errs := runBinWrapper(t, script, append(apply, "AIMEBU_FORCE_BUILD=1"), "run"); strings.Contains(errs, "Updated") {
		t.Fatalf("a forced build updated the clone: %q", errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s", before, after)
	}
}

func TestBinWrapperUpdateLeavesADirtyCheckoutAlone(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	setBinWrapperSource(t, repo, "local-edit")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU2_UPDATE_CHECK=apply"), "run")
	if out != "one aimebu v9.9.9 [run]" || !strings.Contains(errs, "update check skipped: checkout is dirty") {
		t.Fatalf("dirty checkout: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved on a dirty checkout")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "cmd", "aimebu", "main.go")); !strings.Contains(string(got), "local-edit") {
		t.Fatal("the local edit was lost")
	}
}

func TestBinWrapperUpdateLeavesANonDefaultBranchAlone(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	gitIn(t, repo, env, "checkout", "-q", "-b", "feature")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU2_UPDATE_CHECK=apply"), "run")
	if out != "one aimebu v9.9.9 [run]" || !strings.Contains(errs, "checkout is on 'feature', not default branch 'trunk'") {
		t.Fatalf("non-default branch: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before || gitIn(t, repo, env, "branch", "--show-current") != "feature" {
		t.Fatal("the checkout moved")
	}
}

func TestBinWrapperUpdateIsFastAndSilentWhenTheRemoteIsUnreachable(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	apply := append(env, "AIMEBU2_UPDATE_CHECK=apply")

	gitIn(t, repo, env, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if out, errs := runBinWrapper(t, script, apply, "run"); out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("missing remote: out=%q stderr=%q", out, errs)
	}

	// A remote that hangs is cut off by the time limit.
	gitIn(t, repo, env, "remote", "set-url", "origin", "ext::sleep 20")
	hang := append(apply, "GIT_ALLOW_PROTOCOL=ext", "AIMEBU2_UPDATE_TIMEOUT_SECONDS=1", "AIMEBU2_UPDATE_INTERVAL_HOURS=0")
	started := time.Now()
	out, errs := runBinWrapper(t, script, hang, "run")
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("a hanging remote held the command for %s", took)
	}
	if out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("hanging remote: out=%q stderr=%q", out, errs)
	}
}

func TestBinWrapperUpdateCommand(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)

	if out, _, code := runBinWrapperStatus(t, script, env, "update", "--check"); code != 0 || !strings.HasPrefix(out, "aimebu is current at v9.9.9") {
		t.Fatalf("current --check: code=%d out=%q", code, out)
	}
	publish("two")
	if out, _, code := runBinWrapperStatus(t, script, env, "update", "--check"); code != 0 || !strings.HasPrefix(out, "aimebu update available: v9.9.9 -> ") {
		t.Fatalf("behind --check: code=%d out=%q", code, out)
	}
	if out, _, code := runBinWrapperStatus(t, script, env, "update", "--bogus"); code == 0 || out != "" {
		t.Fatalf("bad flag: code=%d out=%q", code, out)
	}

	out, errs, code := runBinWrapperStatus(t, script, env, "update")
	if code != 0 || !strings.HasPrefix(out, "Updated aimebu from ") {
		t.Fatalf("update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if out, _ := runBinWrapper(t, script, env, "run"); !strings.HasPrefix(out, "two ") {
		t.Fatalf("after update the cache still runs %q", out)
	}
	if out, _, _ := runBinWrapperStatus(t, script, env, "update"); !strings.HasPrefix(out, "aimebu is already current at ") {
		t.Fatalf("second update printed %q", out)
	}

	// Refusals exit non-zero and print nothing on stdout.
	publish("three")
	setBinWrapperSource(t, repo, "local-edit")
	if out, errs, code := runBinWrapperStatus(t, script, env, "update"); code == 0 || out != "" || !strings.Contains(errs, "update refused: checkout is dirty") {
		t.Fatalf("dirty update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if out, _, _ := runBinWrapperStatus(t, script, env, "update", "--check"); !strings.Contains(out, "dirty checkout; check skipped") {
		t.Fatalf("dirty --check printed %q", out)
	}
}

func TestBinWrapperUpdateKeepsThePreviousVersionWhenTheNewSourceDoesNotBuild(t *testing.T) {
	repo, env, script, publish := binWrapperWithRemote(t)
	publish("two")
	// Break the published source: the update must roll the checkout back and keep the cache.
	other := gitIn(t, repo, env, "remote", "get-url", "origin")
	broken := filepath.Join(t.TempDir(), "broken")
	gitIn(t, filepath.Dir(broken), env, "clone", "-q", other, broken)
	writeBinWrapperFile(t, filepath.Join(broken, "cmd", "aimebu", "main.go"), "package main\n\nfunc main() { undefined() }\n", 0o644)
	gitIn(t, broken, env, "commit", "-q", "-am", "broken")
	gitIn(t, broken, env, "push", "-q", "origin", "trunk")

	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU2_UPDATE_CHECK=apply"), "run")
	if out != "one aimebu v9.9.9 [run]" || !strings.Contains(errs, "automatic update failed: the updated source did not build; continuing on the previous version") {
		t.Fatalf("failed update: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before || gitIn(t, repo, env, "branch", "--show-current") != "trunk" {
		t.Fatalf("the checkout was not restored: HEAD %s, want %s", after, before)
	}
}
