package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	env = append(os.Environ(),
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
