package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// The Makefile is read by every make call, on machines that have no node (the web tests run in Docker, so `make test-full` needs Docker, Go and make,
// and nothing else). A $(shell ...) in it runs when make reads the file, before any target, so one that calls node breaks every target there. These
// tests keep node out of the parts of the Makefile that are read, and check the CPU-count defaults against a getconf that says what the test wants.

// makeTool finds make, or skips: a test machine without it has nothing to check.
func makeTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make is not installed")
	}
	return path
}

// runMake runs make in the repository root with PATH made of one directory that holds make and the given extra tools, so anything else (node, go,
// docker) is not found. It returns what make wrote to stdout and stderr.
func runMake(t *testing.T, extra map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	bin := t.TempDir()
	if err := os.Symlink(makeTool(t), filepath.Join(bin, "make")); err != nil {
		t.Fatal(err)
	}
	for name, script := range extra {
		if err := testenv.WriteExecutable(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, args...)...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "PATH="+bin)
	// A value the person's shell set would win over the Makefile's default: take the ones under test out. So are the variables a parent make leaves
	// in the environment of what it runs (the gate runs `make -C <dir> test-full`, and this test is in that run): with MAKELEVEL and MAKEFLAGS set,
	// make prints "Entering directory" lines around its output and looks for the parent's job server, and the test would be about the parent.
	cmd.Env = withoutEnv(cmd.Env, "NCPU", "GO_TEST_SHARDS", "UI_JOBS", "MAKELEVEL", "MAKEFLAGS", "MFLAGS", "MAKEOVERRIDES", "GNUMAKEFLAGS")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

func withoutEnv(env []string, names ...string) []string {
	var kept []string
next:
	for _, entry := range env {
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				continue next
			}
		}
		kept = append(kept, entry)
	}
	return kept
}

func TestMakeTestFullReadsTheMakefileWithoutNode(t *testing.T) {
	// getconf is the one tool the Makefile asks for to count CPUs; with only it and make on PATH, a node call or any other missing tool is an error.
	stdout, stderr, err := runMake(t, map[string]string{"getconf": "echo 16"}, "-n", "test-full")
	if err != nil || stderr != "" {
		t.Fatalf("make -n test-full with no node on PATH: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "go vet") {
		t.Fatalf("make -n test-full printed no recipe:\n%s", stdout)
	}
	// A dry run prints recipes and does not start them, and make builds the environment of a recipe only when it starts one, which is when an
	// exported variable (GO_TEST_SHARDS) is expanded and a $(shell) in it runs. So start one: a recipe that does nothing.
	noop := filepath.Join(t.TempDir(), "noop.mk")
	if err := os.WriteFile(noop, []byte("noop: ; @echo \"ok\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := runMake(t, map[string]string{"getconf": "echo 16"}, "-f", "Makefile", "-f", noop, "noop"); err != nil || stderr != "" || strings.TrimSpace(out) != "ok" {
		t.Fatalf("starting a recipe with no node on PATH: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	// The recipes of the Go checks are Go only. (test-web's recipe names node in its local branch, which is text until WEB_TEST_RUNNER=local picks it.)
	goOnly, stderr, err := runMake(t, map[string]string{"getconf": "echo 16"}, "-n", "fmt-check", "vet", "test-race")
	if err != nil || stderr != "" {
		t.Fatalf("make -n fmt-check vet test-race: %v\nstderr: %s", err, stderr)
	}
	if regexp.MustCompile(`(^|[\s;&|(])node(\s|$)`).MatchString(goOnly) {
		t.Fatalf("a Go check calls node:\n%s", goOnly)
	}
}

func TestNoShellCallInTheMakefileUsesNode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	// A $(shell ...) runs whenever the variable it is in is expanded: for an exported or := variable, every make call.
	for number, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "$(shell") && regexp.MustCompile(`\bnode\b`).MatchString(line) {
			t.Errorf("Makefile line %d runs node when make reads the file: %s", number+1, line)
		}
	}
}

func TestMakefileDefaultsFollowTheCPUCount(t *testing.T) {
	printer := filepath.Join(t.TempDir(), "print.mk")
	if err := os.WriteFile(printer, []byte("print-%: ; @echo \"$($*)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What getconf says, and what the Makefile then makes of it: the shards only from 8 CPUs, UI jobs a quarter of them within 1 to 4, and anything
	// that is not a whole number as 1 CPU.
	shards := "internal/server=4,internal/store=3,cmd/aimebu=2"
	for _, tc := range []struct{ getconf, ncpu, shards, jobs string }{
		{"echo 18", "18", shards, "4"},
		{"echo 8", "8", shards, "2"},
		{"echo 7", "7", "", "1"},
		{"echo 2", "2", "", "1"},
		{"echo undefined", "1", "", "1"},
		{"exit 1", "1", "", "1"}, // no getconf answer and no sysctl: one CPU
	} {
		got := map[string]string{}
		for _, name := range []string{"NCPU", "GO_TEST_SHARDS", "UI_JOBS"} {
			stdout, stderr, err := runMake(t, map[string]string{"getconf": tc.getconf}, "-s", "-f", filepath.Join("Makefile"), "-f", printer, "print-"+name)
			if err != nil || stderr != "" {
				t.Fatalf("getconf %q, %s: %v\nstderr: %s", tc.getconf, name, err, stderr)
			}
			got[name] = strings.TrimSpace(stdout)
		}
		if got["NCPU"] != tc.ncpu || got["GO_TEST_SHARDS"] != tc.shards || got["UI_JOBS"] != tc.jobs {
			t.Errorf("getconf %q: NCPU %q, GO_TEST_SHARDS %q, UI_JOBS %q; want %q, %q, %q", tc.getconf, got["NCPU"], got["GO_TEST_SHARDS"], got["UI_JOBS"], tc.ncpu, tc.shards, tc.jobs)
		}
	}
}

// The checks above run inside `make test-full`, which is a make: its environment carries MAKELEVEL, MAKEFLAGS (with -w, which makes a sub-make print
// "Entering directory") and a job server it hands to children. A child make must not take those for its own, or its output is the parent's business.
func TestTheChecksDoNotDependOnAParentMake(t *testing.T) {
	t.Setenv("MAKELEVEL", "1")
	t.Setenv("MAKEFLAGS", "w -j4 --jobserver-auth=3,4")
	t.Setenv("MFLAGS", "-w -j4")
	t.Setenv("GNUMAKEFLAGS", "-w")
	noop := filepath.Join(t.TempDir(), "noop.mk")
	if err := os.WriteFile(noop, []byte("noop: ; @echo \"ok\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runMake(t, map[string]string{"getconf": "echo 16"}, "-f", "Makefile", "-f", noop, "noop")
	if err != nil || strings.TrimSpace(out) != "ok" || stderr != "" {
		t.Fatalf("with a parent make's variables in the environment: %v\nstdout: %q\nstderr: %q", err, out, stderr)
	}
}
