package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func TestDockerMCPUsesTheTurnsAccessWithoutAFile(t *testing.T) {
	access := &AgentToolsAccess{AgentID: "agt_one", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	endpoint, header, err := DockerMCP(access, "agt_one")
	if err != nil || endpoint != "http://host.docker.internal:9897/api/agent/mcp" || header != "Bearer aak_secret" {
		t.Fatalf("endpoint=%q header=%q err=%v", endpoint, header, err)
	}
	if _, _, err := DockerMCP(access, "agt_other"); err == nil {
		t.Fatal("accepted a mismatched agent ID")
	}
	if _, _, err := DockerMCP(nil, "agt_one"); err == nil {
		t.Fatal("accepted a turn with no access")
	}
}

func TestDockerPreflightRejectsUnmountedCheckout(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "claude-docker")
	if err := testenv.WriteExecutable(launcher, []byte("#!/bin/sh\necho 'Directory is not mounted in the harness container' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	err := DockerPreflight("claude", checkout)
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("preflight=%v", err)
	}
}

// A launcher that passes its check and leaves a helper holding the output pipe passed: refusing the spawn for it would turn a working
// launcher into an unusable one. (The check also must not wait for the helper.)
func TestDockerPreflightPassesForALauncherThatLeavesAHelperBehind(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "pi-docker")
	ready, release, done := filepath.Join(root, "helper-ready"), filepath.Join(root, "helper-release"), filepath.Join(root, "helper-done")
	t.Setenv("PREFLIGHT_HELPER_READY", ready)
	t.Setenv("PREFLIGHT_HELPER_RELEASE", release)
	t.Setenv("PREFLIGHT_HELPER_DONE", done)
	t.Cleanup(func() {
		if _, err := os.Stat(ready); os.IsNotExist(err) {
			return
		}
		if err := os.WriteFile(release, []byte("done"), 0600); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(done); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Error("the launcher's helper did not exit after release")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	script := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n( : > \"$PREFLIGHT_HELPER_READY\"; while [ ! -e \"$PREFLIGHT_HELPER_RELEASE\" ]; do sleep 0.1; done; : > \"$PREFLIGHT_HELPER_DONE\" ) &\nwhile [ ! -e \"$PREFLIGHT_HELPER_READY\" ]; do sleep 0.01; done\necho 'pi 1.0'\nexit 0\n"
	if err := testenv.WriteExecutable(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := DockerPreflight("pi", checkout); err != nil {
		t.Fatalf("a launcher that exited 0 failed its preflight: %v", err)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("the helper was not running when preflight returned: %v", err)
	}
}

// The feature probe: a launcher that has it answers with its features before it touches Docker; one that predates it runs the harness's
// --version, whose answer names no feature. The probe always carries --version so the older launcher only reports one.
func TestDockerFeaturesTellANewLauncherFromAnOldOne(t *testing.T) {
	root := t.TempDir()
	probing := filepath.Join(root, "new-launcher")
	if err := testenv.WriteExecutable(probing, []byte("#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then printf 'forward_env\\nsomething_else\\n'; exit 0; fi\necho 'not the probe' >&2; exit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old-launcher")
	if err := testenv.WriteExecutable(old, []byte("#!/bin/sh\ncase \"$*\" in *--version*) echo 1.19.4;; *) echo 'started the harness' >&2; exit 9;; esac\n"), 0700); err != nil {
		t.Fatal(err)
	}
	features, err := DockerFeatures(t.Context(), probing, []string{"--wrapper-flag"}, root, os.Environ())
	if err != nil || !features[FeatureForwardEnv] || !features["something_else"] || len(features) != 2 {
		t.Fatalf("features=%v err=%v", features, err)
	}
	features, err = DockerFeatures(t.Context(), old, nil, root, os.Environ())
	if err != nil || len(features) != 0 {
		t.Fatalf("an older launcher reports no feature: %v %v", features, err)
	}
	if err := RequireDockerFeature(t.Context(), "OpenCode", old, nil, root, os.Environ(), FeatureForwardEnv); err == nil ||
		!strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "HARNESS_DOCKER_FORWARD_ENV") {
		t.Fatalf("an older launcher is refused with the way out: %v", err)
	}
	if err := RequireDockerFeature(t.Context(), "OpenCode", probing, nil, root, os.Environ(), FeatureForwardEnv); err != nil {
		t.Fatalf("a launcher with the feature passes: %v", err)
	}
	failing := filepath.Join(root, "failing")
	if err := testenv.WriteExecutable(failing, []byte("#!/bin/sh\necho 'Docker is not running.' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := DockerFeatures(t.Context(), failing, nil, root, os.Environ()); err == nil || !strings.Contains(err.Error(), "Docker is not running") {
		t.Fatalf("a probe that fails says why: %v", err)
	}
	// The tools whose Docker turns configure the harness through the environment, or hand it the agent's key by name, need the feature.
	if !DockerNeedsForwardEnv("opencode") || !DockerNeedsForwardEnv("vibe") || !DockerNeedsForwardEnv("claude") || !DockerNeedsForwardEnv("codex") || !DockerNeedsForwardEnv("pi") {
		t.Fatal("only the tools configured through the environment need the feature")
	}
}

// Every Docker turn that names forwarded variables must leave the launcher exactly one HARNESS_DOCKER_FORWARD_ENV entry: a second entry
// would win silently and the harness would miss the names the first one carried.
func TestAppendForwardEnvMergesIntoOneList(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		env, add   []string
	}{
		{"no entry yet", "HARNESS_DOCKER_FORWARD_ENV=A", nil, []string{"A"}},
		{"no entry yet, several names", "HARNESS_DOCKER_FORWARD_ENV=A,B", []string{"X=1"}, []string{"A", "B"}},
		{"an existing entry keeps its names first", "HARNESS_DOCKER_FORWARD_ENV=C,A", []string{"HARNESS_DOCKER_FORWARD_ENV=C"}, []string{"A"}},
		{"a name already listed is not added twice", "HARNESS_DOCKER_FORWARD_ENV=A,C,B", []string{"HARNESS_DOCKER_FORWARD_ENV=A,C"}, []string{"A", "B"}},
		{"an empty entry is filled", "HARNESS_DOCKER_FORWARD_ENV=A", []string{"HARNESS_DOCKER_FORWARD_ENV="}, []string{"A"}},
		{"nothing to add leaves the entry alone", "HARNESS_DOCKER_FORWARD_ENV=A", []string{"HARNESS_DOCKER_FORWARD_ENV=A"}, nil},
	} {
		got := AppendForwardEnv(tc.env, tc.add...)
		count := 0
		for _, item := range got {
			if strings.HasPrefix(item, "HARNESS_DOCKER_FORWARD_ENV=") {
				count++
				if item != tc.want {
					t.Fatalf("%s: list = %q, want %q (env %q, add %q)", tc.name, item, tc.want, tc.env, tc.add)
				}
			}
		}
		if count != 1 {
			t.Fatalf("%s: %d forwarding lists after the merge (env %q, add %q): %q", tc.name, count, tc.env, tc.add, got)
		}
	}
	other := AppendForwardEnv([]string{"HARNESS_DOCKER_FORWARD_ENV=A"}, "A")
	if len(other) != 1 || other[0] != "HARNESS_DOCKER_FORWARD_ENV=A" {
		t.Fatalf("a name that is already the only one changed the env: %q", other)
	}
}

func TestDockerPreflightOfOpenCodeNeedsALauncherThatForwards(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "opencode-docker")
	if err := testenv.WriteExecutable(old, []byte("#!/bin/sh\necho 1.19.4\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := DockerPreflight("opencode", checkout, old); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("an older launcher: %v", err)
	}
	if err := testenv.WriteExecutable(old, []byte("#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n[ \"${OPENCODE_LAUNCH_FLAGS+x}\" = x ] && echo 1.19.4\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := DockerPreflight("opencode", checkout, old); err != nil {
		t.Fatalf("a launcher with the feature: %v", err)
	}
	if err := DockerPreflight("cursor", checkout, old); err == nil {
		t.Fatal("an unknown tool is refused")
	}
}

// An agent's runtime state is kept out of the repository's index in the checkout it runs in and in a worktree of it, once however often it is
// asked for, and a folder that is no git checkout is left alone.
func TestExcludeRuntimeStateKeepsAimebuOutOfTheIndex(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "first")
	tree := filepath.Join(root, "tree")
	run(repo, "worktree", "add", "-q", "-b", "other", tree)
	for _, dir := range []string{repo, tree, repo} {
		if err := os.MkdirAll(filepath.Join(dir, ".aimebu", "pi-sessions"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".aimebu", "pi-sessions", "transcript.jsonl"), []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ExcludeRuntimeState(dir); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{repo, tree} {
		status, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--untracked-files=all").Output()
		if err != nil || strings.Contains(string(status), ".aimebu") {
			t.Fatalf("%s: the runtime state shows up as untracked: %q %v", dir, status, err)
		}
	}
	exclude, err := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if err != nil || strings.Count(string(exclude), ".aimebu/") != 1 {
		t.Fatalf("the exclude file lists it %d times: %q %v", strings.Count(string(exclude), ".aimebu/"), exclude, err)
	}
	if err := ExcludeRuntimeState(t.TempDir()); err != nil {
		t.Fatalf("a folder that is no git checkout: %v", err)
	}
}

// An exclude file that is a link is not written through: it would append the line to whatever the link names.
func TestExcludeRuntimeStateRefusesALinkedExcludeFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(exclude)
	if err := os.Symlink(victim, exclude); err != nil {
		t.Fatal(err)
	}
	if err := ExcludeRuntimeState(repo); err == nil {
		t.Fatal("a linked exclude file was written through")
	}
	if data, _ := os.ReadFile(victim); string(data) != "untouched\n" {
		t.Fatalf("the link's target was written: %q", data)
	}
}
