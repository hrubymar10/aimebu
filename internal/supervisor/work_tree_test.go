package supervisor

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

// The description of a limit-killed turn's checkout runs git on the host in a checkout the agent could write to, config included. A program that
// config names (an fsmonitor, a filter driver, the gpg.program a signed tip makes `git log` run) must not run.
func TestWorkTreeStateRunsNoProgramTheRepositoryNames(t *testing.T) {
	dir := gitCheckout(t)
	marks := t.TempDir()
	script := func(name, tail string) string {
		path := filepath.Join(t.TempDir(), name+".sh")
		if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, name)+"'\n"+tail), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// A tip with a signature header, so that log.showSignature makes `git log` call gpg.program.
	commit := "tree " + git("write-tree") + "\nparent " + git("rev-parse", "HEAD") + "\nauthor t <t@example.invalid> 1700000000 +0000\n" +
		"committer t <t@example.invalid> 1700000000 +0000\ngpgsig -----BEGIN PGP SIGNATURE-----\n \n x\n -----END PGP SIGNATURE-----\n\nfeat(test): signed tip\n"
	raw := filepath.Join(t.TempDir(), "commit")
	if err := os.WriteFile(raw, []byte(commit), 0o644); err != nil {
		t.Fatal(err)
	}
	git("update-ref", "HEAD", git("hash-object", "-w", "-t", "commit", raw))
	git("config", "core.fsmonitor", script("fsmonitor", "exit 1\n"))
	git("config", "filter.planted.clean", script("filter", "cat\n"))
	git("config", "log.showSignature", "true")
	git("config", "gpg.program", script("gpg", "exit 1\n"))
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("* filter=planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The same size as the committed "a\n": a status must read the content, through the clean filter.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := workTreeState(context.Background(), dir); !strings.Contains(got, "feat(test): signed tip") || !strings.HasSuffix(got, ", 1 dirty file") {
		t.Fatalf("description: %q", got)
	}
	ran := func() []string {
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
	if got := ran(); len(got) != 0 {
		t.Fatalf("workTreeState ran planted programs: %v", got)
	}
	// The plant is live: plain git runs every one of them.
	git("status", "--porcelain")
	git("log", "-1", "--oneline")
	if got := strings.Join(ran(), ","); got != "filter,fsmonitor,gpg" {
		t.Fatalf("the planted programs must run without the guards, or this test proves nothing: %q", got)
	}
}

// The description does not go into an embedded repository either: its own config names a filter the guards never read. A .gitmodules entry
// asking for ignore=none does not bring the recursion back.
func TestWorkTreeStateSkipsNestedRepositories(t *testing.T) {
	dir := gitCheckout(t)
	marks := t.TempDir()
	git := func(at string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", at, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	nested := filepath.Join(dir, "x")
	git(dir, "init", "-q", "x")
	for name, body := range map[string]string{".gitattributes": "*.txt filter=y\n", "a.txt": "a\n"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(nested, "add", ".")
	git(nested, "commit", "-qm", "nested")
	filter := filepath.Join(t.TempDir(), "filter.sh")
	if err := testenv.WriteExecutable(filter, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "nested")+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(nested, "config", "filter.y.clean", filter)
	if err := os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath = x\n\turl = ./x\n\tignore = none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(dir, "add", "x", ".gitmodules")
	git(dir, "commit", "-qm", "feat(test): embed x")
	now := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(nested, "a.txt"), now, now); err != nil {
		t.Fatal(err)
	}
	ran := func() int {
		entries, err := os.ReadDir(marks)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	if got := workTreeState(context.Background(), dir); !strings.HasSuffix(got, ", tree clean") {
		t.Fatalf("description: %q", got)
	}
	if ran() != 0 {
		t.Fatal("workTreeState ran the nested repository's filter")
	}
	// The plant is live: plain git goes into x and runs it.
	git(dir, "status", "--porcelain")
	if ran() != 1 {
		t.Fatal("plain status must run the nested filter, or this test proves nothing")
	}
}
