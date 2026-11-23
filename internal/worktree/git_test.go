package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// plantRepoPrograms sets up, in repo's own config and hooks (what an agent with the checkout mounted can write), every program git would run for
// a host-side call: an fsmonitor, a post-checkout hook, and a filter driver for every file. Each touches its own file in the returned folder.
func plantRepoPrograms(t *testing.T, repo string) string {
	t.Helper()
	marks := t.TempDir()
	script := func(name, tail string) string {
		path := filepath.Join(t.TempDir(), name+".sh")
		body := "#!/bin/sh\ntouch '" + filepath.Join(marks, name) + "'\n" + tail
		if err := testenv.WriteExecutable(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	filter := script("filter", "cat\n")
	for _, kv := range [][2]string{{"core.fsmonitor", script("fsmonitor", "exit 1\n")}, {"filter.planted.clean", filter},
		{"filter.planted.smudge", filter}, {"filter.planted.required", "true"}} {
		if out, err := exec.Command("git", "-C", repo, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v %s", kv[0], err, out)
		}
	}
	common, err := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSpace(string(common))
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "attributes"), []byte("* filter=planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testenv.WriteExecutable(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "hook")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The repository's own hooks folder is the one git uses: a hooks path set for the whole machine would hide the planted hook.
	if out, err := exec.Command("git", "-C", repo, "config", "core.hooksPath", hooks).CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v %s", err, out)
	}
	return marks
}

func planted(t *testing.T, marks string) []string {
	t.Helper()
	entries, err := os.ReadDir(marks)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// A status, a worktree add and the rest of the host-side calls never run a program the repository's config names: the agent can write that
// config, and the program would run as the host user.
func TestHostGitRunsNoProgramTheRepositoryNames(t *testing.T) {
	root := repoFixture(t)
	marks := plantRepoPrograms(t, root)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("BASE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, err := dirtyCount(ctx, root, true); err != nil || count != 1 {
		t.Fatalf("dirtyCount=%d %v", count, err)
	}
	if got := planted(t, marks); len(got) != 0 {
		t.Fatalf("a status ran planted programs: %v", got)
	}
	result, err := Ensure(ctx, root, "room_planted", "")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(result.Path, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("worktree checkout: %q %v", data, err)
	}
	if _, err := Inspect(ctx, root, "room_planted", result.BaseRef); err != nil {
		t.Fatal(err)
	}
	if got := planted(t, marks); len(got) != 0 {
		t.Fatalf("a worktree add or inspect ran planted programs: %v", got)
	}
	// The plant is live: the same status and a worktree add without the guards run them. (The change keeps the file's size, so a status must
	// read its content, through the clean filter.)
	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("plain status: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", filepath.Join(t.TempDir(), "plain"), "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("plain worktree add: %v %s", err, out)
	}
	if got := strings.Join(planted(t, marks), ","); got != "filter,fsmonitor,hook" {
		t.Fatalf("the planted programs must run without the guards, or this test proves nothing: %q", got)
	}
}

// A driver's name may hold any character, '=' included; the guards still name it apart from its value.
func TestHostGitTurnsOffAFilterWithAnOddName(t *testing.T) {
	root := repoFixture(t)
	marks := t.TempDir()
	path := filepath.Join(t.TempDir(), "filter.sh")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "filter")+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", root, "config", "filter.a=b.c.clean", path).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "attributes"), []byte("* filter=a=b.c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("BASE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, err := dirtyCount(context.Background(), root, false); err != nil || count != 1 {
		t.Fatalf("dirtyCount=%d %v", count, err)
	}
	if got := planted(t, marks); len(got) != 0 {
		t.Fatalf("the filter ran: %v", got)
	}
}

// Settings already in the environment as command-line config are kept, and the guards come after them.
func TestWithGitConfigKeepsInheritedSettings(t *testing.T) {
	env := withGitConfig([]string{"PATH=/bin", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Host"}, [][2]string{{"core.fsmonitor", "false"}})
	want := []string{"PATH=/bin", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Host", "GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_COUNT=2", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Fatalf("env=%q", env)
	}
}

// Filter drivers from the trusted scopes keep working (git-lfs is installed in the global config): a worktree add still runs a global smudge
// filter, and a repository-local setting that redefines that driver gets the global command back. A driver only the repository's config defines
// does not run, and neither does one the repository gives a process the global config does not give it (the whole driver is turned off).
func TestHostGitKeepsGlobalFiltersAndDropsLocalOnes(t *testing.T) {
	root := repoFixture(t)
	for _, name := range []string{"other.txt", "third.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "other.txt", "third.txt"}, {"commit", "-qm", "other"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	marks := t.TempDir()
	script := func(name string) string {
		path := filepath.Join(t.TempDir(), name+".sh")
		if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, name)+"'\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, kv := range [][3]string{{"--global", "filter.trusted.smudge", script("global")}, {"--local", "filter.trusted.smudge", script("local-override")},
		{"--local", "filter.planted.smudge", script("planted")}, {"--global", "filter.whole.smudge", script("global-whole")},
		{"--local", "filter.whole.process", script("local-process")}} {
		if out, err := exec.Command("git", "-C", root, "config", kv[0], kv[1], kv[2]).CombinedOutput(); err != nil {
			t.Fatalf("git config %v: %v %s", kv, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "attributes"), []byte("tracked.txt filter=trusted\nother.txt filter=planted\nthird.txt filter=whole\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), root, "room_filters", ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(planted(t, marks), ","); got != "global" {
		t.Fatalf("only the global smudge filter may run on worktree add: %q", got)
	}
}

// plantEmbeddedRepo commits, in root, an embedded repository x (a gitlink, as `git add x` makes) whose own config has a clean filter for its
// .txt files, then touches x/a.txt, so a status that recurses into x must read the file through that filter. The filter touches "nested" in the
// returned folder. When gitmodules is set, root also gets a .gitmodules entry for x that asks for ignore=none, which outranks the
// diff.ignoreSubmodules setting.
func plantEmbeddedRepo(t *testing.T, root string, gitmodules bool) string {
	t.Helper()
	marks := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	nested := filepath.Join(root, "x")
	git(root, "init", "-q", "x")
	git(nested, "config", "user.name", "Test")
	git(nested, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(nested, ".gitattributes"), []byte("*.txt filter=y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(nested, "add", ".")
	git(nested, "commit", "-qm", "nested")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	if err := testenv.WriteExecutable(filter, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "nested")+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(nested, "config", "filter.y.clean", filter)
	if gitmodules {
		if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath = x\n\turl = ./x\n\tignore = none\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(root, "add", ".gitmodules")
	}
	git(root, "add", "x")
	git(root, "commit", "-qm", "embed x")
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(nested, "a.txt"), now, now); err != nil {
		t.Fatal(err)
	}
	return marks
}

// A host-side status does not recurse into a submodule or an embedded repository: the nested repository has its own config, whose filter
// drivers the guards never read, so a status that went in would run them as the host user. A .gitmodules entry asking for ignore=none does
// not bring the recursion back. The cost: a nested repository's own changes no longer make the checkout dirty.
func TestHostGitStatusSkipsNestedRepositories(t *testing.T) {
	for _, gitmodules := range []bool{false, true} {
		root := repoFixture(t)
		marks := plantEmbeddedRepo(t, root, gitmodules)
		for _, ignored := range []bool{false, true} {
			if count, err := dirtyCount(context.Background(), root, ignored); err != nil || count != 0 {
				t.Fatalf("gitmodules=%v ignored=%v: dirtyCount=%d %v", gitmodules, ignored, count, err)
			}
		}
		if got := planted(t, marks); len(got) != 0 {
			t.Fatalf("gitmodules=%v: a status ran the nested repository's filter: %v", gitmodules, got)
		}
		// The plant is live: a plain status goes into x and runs its filter.
		if out, err := exec.Command("git", "-C", root, "status", "--porcelain").CombinedOutput(); err != nil {
			t.Fatalf("plain status: %v %s", err, out)
		}
		if got := strings.Join(planted(t, marks), ","); got != "nested" {
			t.Fatalf("gitmodules=%v: plain status must run the nested filter, or this test proves nothing: %q", gitmodules, got)
		}
	}
}
