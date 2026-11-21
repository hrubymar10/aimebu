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
// developer's exported AIMEBU_UPDATE_* or git prompt settings cannot change the tests.
func cleanBinWrapperEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AIMEBU_UPDATE_") || strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
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
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply")

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
	out, errs = runBinWrapper(t, script, append(apply, "AIMEBU_UPDATE_INTERVAL_HOURS=0"), "run")
	if !strings.HasPrefix(out, "two ") || errs != "" {
		t.Fatalf("current run: out=%q stderr=%q", out, errs)
	}
}

func TestBinWrapperUpdateRunsOnlyOncePerInterval(t *testing.T) {
	_, env, script, publish := binWrapperWithRemote(t)
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply")
	publish("two")
	if out, _ := runBinWrapper(t, script, apply, "run"); !strings.HasPrefix(out, "two ") {
		t.Fatalf("first check did not update: %q", out)
	}
	publish("three")
	if out, errs := runBinWrapper(t, script, apply, "run"); !strings.HasPrefix(out, "two ") || errs != "" {
		t.Fatalf("a second check inside the interval ran: out=%q stderr=%q", out, errs)
	}
	if out, _ := runBinWrapper(t, script, append(apply, "AIMEBU_UPDATE_INTERVAL_HOURS=0"), "run"); !strings.HasPrefix(out, "three ") {
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
	// AIMEBU_UPDATE_CHECK=0 beats apply's sibling modes and leaves everything alone.
	if out, errs := runBinWrapper(t, script, append(env, "AIMEBU_UPDATE_CHECK=0"), "run"); out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("disabled check: out=%q stderr=%q", out, errs)
	}
	// Machine-readable and informational commands skip the check even in apply mode.
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply")
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
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU_UPDATE_CHECK=apply"), "run")
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
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU_UPDATE_CHECK=apply"), "run")
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
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply")

	gitIn(t, repo, env, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if out, errs := runBinWrapper(t, script, apply, "run"); out != "one aimebu v9.9.9 [run]" || errs != "" {
		t.Fatalf("missing remote: out=%q stderr=%q", out, errs)
	}

	// A remote that hangs is cut off by the time limit.
	gitIn(t, repo, env, "remote", "set-url", "origin", "ext::sleep 20")
	hang := append(apply, "GIT_ALLOW_PROTOCOL=ext", "AIMEBU_UPDATE_TIMEOUT_SECONDS=1", "AIMEBU_UPDATE_INTERVAL_HOURS=0")
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
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU_UPDATE_CHECK=apply"), "run")
	if out != "one aimebu v9.9.9 [run]" || !strings.Contains(errs, "automatic update failed: the updated source did not build; continuing on the previous version") {
		t.Fatalf("failed update: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before || gitIn(t, repo, env, "branch", "--show-current") != "trunk" {
		t.Fatalf("the checkout was not restored: HEAD %s, want %s", after, before)
	}
}

// The release channels: the clone follows annotated tags of the running major instead
// of the default branch. The remotes here hold the shared tag table from
// internal/updatecheck, so the bash ordering cannot drift from the Go one.

// binWrapperWithTaggedRemote returns a clone detached at the annotated tag v2.0.0 whose
// origin is a bare remote holding that tag, plus a function that publishes a new
// annotated tag (on a new commit whose stand-in prints the marker).
func binWrapperWithTaggedRemote(t *testing.T) (repo string, env []string, script string, pushTag func(tag, marker string)) {
	t.Helper()
	repo, env = binWrapperRepo(t, "v2.0.0")
	env = withGitTransportShim(t, env)
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
	// The remote's v2.0.0 must be annotated even though the clone's own tag is not:
	// only annotated tags are ever targets.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(other), env, "clone", "-q", remote, other)
	gitIn(t, other, env, "tag", "-a", "v2.0.0", "-m", "v2.0.0")
	gitIn(t, other, env, "push", "-q", "origin", "v2.0.0")
	pushTag = func(tag, marker string) {
		t.Helper()
		if marker != "" {
			setBinWrapperSource(t, other, marker)
			gitIn(t, other, env, "commit", "-q", "-am", "publish "+tag)
		}
		gitIn(t, other, env, "tag", "-a", tag, "-m", tag)
		gitIn(t, other, env, "push", "-q", "origin", tag)
	}
	script = filepath.Join(repo, "bin", "aimebu")
	// Detach the clone at v2.0.0 and warm the cached binary.
	gitIn(t, repo, env, "checkout", "-q", "--detach", "v2.0.0")
	if out, _ := runBinWrapper(t, script, env); out != "one aimebu v2.0.0 []" {
		t.Fatalf("warm-up run printed %q", out)
	}
	return repo, env, script, pushTag
}

// withGitTransportShim returns the extra environment for the local file transport:
// this git dispatches upload-pack and receive-pack through dashed symlinks that do not
// resolve on every machine (the sandbox here cannot exec them), so a private exec path
// carries two one-line shims that call the built-ins the transport would have reached.
func withGitTransportShim(t *testing.T, env []string) []string {
	t.Helper()
	dir := t.TempDir()
	for name, sub := range map[string]string{"git-upload-pack": "upload-pack", "git-receive-pack": "receive-pack"} {
		writeBinWrapperFile(t, filepath.Join(dir, name), "#!/bin/sh\nexec git "+sub+" \"$@\"\n", 0o755)
	}
	return append(env, "GIT_EXEC_PATH="+dir)
}

func TestBinWrapperComputedDefaultChannelFollowsTheTags(t *testing.T) {
	repo, env, script, pushTag := binWrapperWithTaggedRemote(t)
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply", "AIMEBU_UPDATE_INTERVAL_HOURS=0")

	// With no stable tag of major 2 on the remote the computed default is prerelease,
	// and the newest annotated tag of the running major is the one it sits on: current.
	if out, errs := runBinWrapper(t, script, apply, "run"); out != "one aimebu v2.0.0 [run]" || errs != "" {
		t.Fatalf("current run: out=%q stderr=%q", out, errs)
	}
	// The first stable tag of the major switches the computed default to stable, and the
	// next run updates onto it, detached, with a rebuilt cache.
	pushTag("v2.1.0", "two")
	out, errs := runBinWrapper(t, script, apply, "run")
	if out != "two aimebu v2.1.0 [run]" {
		t.Fatalf("updated run printed %q", out)
	}
	if !strings.Contains(errs, "Updated aimebu from ") || !strings.Contains(errs, "Building aimebu") {
		t.Fatalf("update notice missing from stderr: %q", errs)
	}
	if head, tag := gitIn(t, repo, env, "rev-parse", "HEAD"), gitIn(t, repo, env, "rev-parse", "v2.1.0^{commit}"); head != tag {
		t.Fatalf("HEAD %s is not the tag %s", head, tag)
	}
	if branch := gitIn(t, repo, env, "branch", "--show-current"); branch != "" {
		t.Fatalf("the update left the checkout on branch %q", branch)
	}
	// The server subcommand never prompts and never moves the checkout, even in apply mode.
	pushTag("v2.2.0", "three")
	if out, errs := runBinWrapper(t, script, apply, "server"); out != "two aimebu v2.1.0 [server]" || errs != "" {
		t.Fatalf("server run: out=%q stderr=%q", out, errs)
	}
	if out, _, _ := runBinWrapperStatus(t, script, env, "update", "--check"); !strings.HasPrefix(out, "aimebu update available: v2.1.0 -> v2.2.0.") {
		t.Fatalf("--check printed %q", out)
	}
}

func TestBinWrapperChannelFileAndPrereleaseTags(t *testing.T) {
	repo, env, script, pushTag := binWrapperWithTaggedRemote(t)
	pushTag("v2.1.0", "two")
	pushTag("v2.2.0-alpha.1", "")
	pushTag("v2.2.0-alpha.2", "")
	pushTag("v2.2.0-rc.1", "")

	// A one-word channel file in the config root chooses the channel.
	root := t.TempDir()
	withFile := append(env, "AIMEBU_CONFIG_DIR="+root, "AIMEBU_UPDATE_INTERVAL_HOURS=0")
	writeBinWrapperFile(t, filepath.Join(root, "update-channel"), "stable\n", 0o644)
	if out, _, _ := runBinWrapperStatus(t, script, withFile, "update", "--check"); !strings.HasPrefix(out, "aimebu update available: v2.0.0 -> v2.1.0.") {
		t.Fatalf("stable channel: %q", out)
	}
	writeBinWrapperFile(t, filepath.Join(root, "update-channel"), "prerelease\n", 0o644)
	if out, _, _ := runBinWrapperStatus(t, script, withFile, "update", "--check"); !strings.HasPrefix(out, "aimebu update available: v2.0.0 -> v2.2.0-rc.1.") {
		t.Fatalf("prerelease channel: %q", out)
	}
	// alpha.9 < alpha.10, and rc.1 < release, come from the shared ordering.
	if out, errs, code := runBinWrapperStatus(t, script, withFile, "update"); code != 0 || !strings.HasPrefix(out, "Updated aimebu from ") {
		t.Fatalf("prerelease update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if head, tag := gitIn(t, repo, env, "rev-parse", "HEAD"), gitIn(t, repo, env, "rev-parse", "v2.2.0-rc.1^{commit}"); head != tag {
		t.Fatalf("HEAD %s is not the tag %s", head, tag)
	}

	// An invalid channel word warns and counts as unset (computed default: stable here);
	// the warning surfaces when a check actually runs.
	writeBinWrapperFile(t, filepath.Join(root, "update-channel"), "beta\n", 0o644)
	_, errs, _ := runBinWrapperStatus(t, script, withFile, "update", "--check")
	if !strings.Contains(errs, "ignoring invalid channel 'beta'") {
		t.Fatalf("invalid channel warning missing: %q", errs)
	}
}

func TestBinWrapperIgnoresLightweightTags(t *testing.T) {
	repo, env, script, pushTag := binWrapperWithTaggedRemote(t)
	pushTag("v2.1.0", "two")
	// A lightweight tag has no peeled entry on the remote, so it is never a target.
	gitIn(t, repo, env, "push", "-q", "origin", "HEAD:refs/tags/v2.2.0")
	if out, _, _ := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL=stable"), "update", "--check"); !strings.HasPrefix(out, "aimebu update available: v2.0.0 -> v2.1.0.") {
		t.Fatalf("lightweight tag became a target: %q", out)
	}
	if out, errs, code := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL=stable"), "update"); code != 0 || !strings.HasPrefix(out, "Updated aimebu from ") || !strings.Contains(out, "to v2.1.0.") {
		t.Fatalf("update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if head, tag := gitIn(t, repo, env, "rev-parse", "HEAD"), gitIn(t, repo, env, "rev-parse", "v2.1.0^{commit}"); head != tag {
		t.Fatalf("HEAD %s is not the tag %s", head, tag)
	}
}

func TestBinWrapperDivergedCheckoutIsLeftAlone(t *testing.T) {
	repo, env, script, pushTag := binWrapperWithTaggedRemote(t)
	pushTag("v2.1.0", "two")
	stable := append(env, "AIMEBU_UPDATE_CHANNEL=stable")

	// A local branch with commits that are not in the target is reported as diverged
	// and left alone, on the branch or detached.
	gitIn(t, repo, env, "checkout", "-q", "trunk")
	setBinWrapperSource(t, repo, "local-edit")
	gitIn(t, repo, env, "commit", "-q", "-am", "local work")
	// The tag has to be in the clone for the classification to prove divergence; a
	// checkout that never fetched it still gets the same refusal at apply time.
	gitIn(t, repo, env, "fetch", "-q", "origin", "v2.1.0")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	if out, _, code := runBinWrapperStatus(t, script, stable, "update", "--check"); code != 0 || !strings.Contains(out, "commits that are not in v2.1.0") {
		t.Fatalf("--check: code=%d out=%q", code, out)
	}
	if out, errs, code := runBinWrapperStatus(t, script, stable, "update"); code == 0 || out != "" || !strings.Contains(errs, "commits that are not in v2.1.0") {
		t.Fatalf("update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatal("the diverged checkout moved")
	}
	if got, _ := os.ReadFile(filepath.Join(repo, "cmd", "aimebu", "main.go")); !strings.Contains(string(got), "local-edit") {
		t.Fatal("the local edit was lost")
	}
}

func TestBinWrapperNoReleaseState(t *testing.T) {
	repo, env, script, _ := binWrapperWithTaggedRemote(t)
	// A checkout of a major the remote has no release for: no-release, refused.
	gitIn(t, repo, env, "tag", "-a", "v9.9.9", "-m", "v9.9.9")
	gitIn(t, repo, env, "checkout", "-q", "--detach", "v9.9.9")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	stable := append(env, "AIMEBU_UPDATE_CHANNEL=stable")
	if out, _, _ := runBinWrapperStatus(t, script, stable, "update", "--check"); !strings.Contains(out, "no release of major 9 on channel 'stable'") {
		t.Fatalf("--check: %q", out)
	}
	if out, errs, code := runBinWrapperStatus(t, script, stable, "update"); code == 0 || out != "" || !strings.Contains(errs, "no release of major 9") {
		t.Fatalf("update: code=%d out=%q stderr=%q", code, out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatal("the no-release checkout moved")
	}
}

func TestBinWrapperEdgeChannelFollowsABranch(t *testing.T) {
	repo, env, script, _ := binWrapperWithTaggedRemote(t)
	gitIn(t, repo, env, "checkout", "-q", "trunk")
	gitIn(t, repo, env, "push", "-q", "origin", "HEAD:refs/heads/dev")
	edge := append(env, "AIMEBU_UPDATE_CHANNEL=edge:dev", "AIMEBU_UPDATE_INTERVAL_HOURS=0")

	// A checkout on a branch the channel does not track is left alone.
	if out, errs := runBinWrapper(t, script, append(edge, "AIMEBU_UPDATE_CHECK=apply"), "run"); out != "one aimebu v2.0.0 [run]" || !strings.Contains(errs, "checkout is on 'trunk', not branch 'dev'") {
		t.Fatalf("wrong branch: out=%q stderr=%q", out, errs)
	}
	// On the tracked branch a published tip is applied and the checkout stays on it.
	gitIn(t, repo, env, "checkout", "-q", "-b", "dev", "origin/dev")
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(other), env, "clone", "-q", gitIn(t, repo, env, "remote", "get-url", "origin"), other)
	gitIn(t, other, env, "checkout", "-q", "dev")
	setBinWrapperSource(t, other, "two")
	gitIn(t, other, env, "commit", "-q", "-am", "publish two")
	gitIn(t, other, env, "push", "-q", "origin", "dev")
	out, errs := runBinWrapper(t, script, append(edge, "AIMEBU_UPDATE_CHECK=apply"), "run")
	if !strings.HasPrefix(out, "two aimebu v2.0.0-1-g") || !strings.Contains(errs, "Updated aimebu from ") {
		t.Fatalf("edge update: out=%q stderr=%q", out, errs)
	}
	if branch := gitIn(t, repo, env, "branch", "--show-current"); branch != "dev" {
		t.Fatalf("the edge update left the checkout on %q", branch)
	}
}

func TestBinWrapperTagUpdateRollsBackToADetachedHeadWhenTheBuildFails(t *testing.T) {
	repo, env, script, _ := binWrapperWithTaggedRemote(t)
	// The tag's source does not build: the update must restore the detached HEAD and
	// keep the cached binary.
	broken := filepath.Join(t.TempDir(), "broken")
	gitIn(t, filepath.Dir(broken), env, "clone", "-q", gitIn(t, repo, env, "remote", "get-url", "origin"), broken)
	writeBinWrapperFile(t, filepath.Join(broken, "cmd", "aimebu", "main.go"), "package main\n\nfunc main() { undefined() }\n", 0o644)
	gitIn(t, broken, env, "commit", "-q", "-am", "broken")
	gitIn(t, broken, env, "tag", "-a", "v2.1.0", "-m", "v2.1.0")
	gitIn(t, broken, env, "push", "-q", "origin", "v2.1.0")

	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	out, errs := runBinWrapper(t, script, append(env, "AIMEBU_UPDATE_CHECK=apply"), "run")
	if out != "one aimebu v2.0.0 [run]" || !strings.Contains(errs, "automatic update failed: the updated source did not build; continuing on the previous version") {
		t.Fatalf("failed update: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s", before, after)
	}
	if branch := gitIn(t, repo, env, "branch", "--show-current"); branch != "" {
		t.Fatalf("the rollback left the checkout on branch %q", branch)
	}
}

func TestBinWrapperTagUpdateRollsBackToTheBranchItStartedOn(t *testing.T) {
	repo, env, script, _ := binWrapperWithTaggedRemote(t)
	// A checkout on a branch with the channel set explicitly is detached onto the tag;
	// when that tag does not build, the rollback checks the branch back out instead of
	// failing and exiting.
	broken := filepath.Join(t.TempDir(), "broken")
	gitIn(t, filepath.Dir(broken), env, "clone", "-q", gitIn(t, repo, env, "remote", "get-url", "origin"), broken)
	writeBinWrapperFile(t, filepath.Join(broken, "cmd", "aimebu", "main.go"), "package main\n\nfunc main() { undefined() }\n", 0o644)
	gitIn(t, broken, env, "commit", "-q", "-am", "broken")
	gitIn(t, broken, env, "tag", "-a", "v2.1.0", "-m", "v2.1.0")
	gitIn(t, broken, env, "push", "-q", "origin", "v2.1.0")
	gitIn(t, repo, env, "checkout", "-q", "trunk")

	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	out, errs, code := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL=stable", "AIMEBU_UPDATE_CHECK=apply", "AIMEBU_UPDATE_INTERVAL_HOURS=0"), "run")
	if code != 0 || out != "one aimebu v2.0.0 [run]" || !strings.Contains(errs, "automatic update failed: the updated source did not build; continuing on the previous version") || strings.Contains(errs, "could not be restored") {
		t.Fatalf("failed update from a branch: code=%d out=%q stderr=%q", code, out, errs)
	}
	if branch := gitIn(t, repo, env, "branch", "--show-current"); branch != "trunk" {
		t.Fatalf("the rollback left the checkout on %q, want the branch it started on", branch)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved from %s to %s", before, after)
	}
	// An explicit update reports the failure and restores the branch too.
	if _, errs, code := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL=stable"), "update"); code == 0 || !strings.Contains(errs, "the previous checkout was restored") {
		t.Fatalf("explicit update: code=%d stderr=%q", code, errs)
	}
	if branch := gitIn(t, repo, env, "branch", "--show-current"); branch != "trunk" {
		t.Fatalf("the explicit rollback left the checkout on %q", branch)
	}
}

func TestBinWrapperNamedBranchWithNoChannelIsNeverDetached(t *testing.T) {
	repo, env, script, pushTag := binWrapperWithTaggedRemote(t)
	pushTag("v2.1.0", "two")
	gitIn(t, repo, env, "checkout", "-q", "-b", "feature", "v2.0.0")
	before := gitIn(t, repo, env, "rev-parse", "HEAD")
	apply := append(env, "AIMEBU_UPDATE_CHECK=apply", "AIMEBU_UPDATE_INTERVAL_HOURS=0")
	out, errs := runBinWrapper(t, script, apply, "run")
	if out != "one aimebu v2.0.0 [run]" || !strings.Contains(errs, "checkout is on 'feature', not default branch 'trunk'") {
		t.Fatalf("named branch, no channel: out=%q stderr=%q", out, errs)
	}
	if after := gitIn(t, repo, env, "rev-parse", "HEAD"); after != before || gitIn(t, repo, env, "branch", "--show-current") != "feature" {
		t.Fatal("a developer branch was detached or moved")
	}
	// Setting the channel explicitly is the opt-in.
	if _, _, code := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL=stable"), "update"); code != 0 {
		t.Fatal("an explicit channel did not update a named branch")
	}
	if head, tag := gitIn(t, repo, env, "rev-parse", "HEAD"), gitIn(t, repo, env, "rev-parse", "v2.1.0^{commit}"); head != tag {
		t.Fatalf("HEAD %s is not the tag %s", head, tag)
	}
}

func TestBinWrapperIgnoresMalformedTags(t *testing.T) {
	_, env, script, pushTag := binWrapperWithTaggedRemote(t)
	pushTag("v2.1.0", "two")
	// Annotated tags that do not follow vMAJOR.MINOR.PATCH[-(alpha|beta|rc).N] are not
	// releases, however high they would sort.
	for _, bad := range []string{"v2.9.0-foo.1", "v2.8", "v2.7.0-rc", "v2.6.0-RC.1", "v2.5.0.1", "v2.4.0-rc.1.2", "v2.3.x", "v2.99999999.0"} {
		pushTag(bad, "")
	}
	for _, channel := range []string{"stable", "prerelease"} {
		out, _, _ := runBinWrapperStatus(t, script, append(env, "AIMEBU_UPDATE_CHANNEL="+channel), "update", "--check")
		if !strings.HasPrefix(out, "aimebu update available: v2.0.0 -> v2.1.0.") {
			t.Fatalf("%s channel with malformed tags: %q", channel, out)
		}
	}
}
