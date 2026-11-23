package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/ccsessions"
)

const transcriptID = "0f6c1a52-3b7e-4c1d-9a55-7d2e8b1c4f90"

func transcriptSpec(t *testing.T, started bool) adapter.TurnSpec {
	t.Helper()
	spec := warmSpec(t, "t1")
	spec.NativeSessionHandle = transcriptID
	spec.NativeSessionStarted = started
	return spec
}

func writeTranscript(t *testing.T, configDir, cwd string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", ccsessions.FolderName(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, transcriptID+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionArgs(t *testing.T, a *Adapter, spec adapter.TurnSpec) string {
	t.Helper()
	l, err := a.prepare(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(l.args[l.sessionAt[0]:l.sessionAt[1]], " ")
}

func TestResumeDecisionFollowsTranscript(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	cases := []struct {
		name    string
		started bool
		has     bool // transcript in the config dir this launch uses
		inOther bool // transcript only in another (previous account's) config dir
		want    string
	}{
		{"normal resume keeps --resume", true, true, false, "--resume " + transcriptID},
		{"account switch: new config dir has none", true, false, true, "--session-id " + transcriptID},
		{"first turn died before writing a transcript", true, false, false, "--session-id " + transcriptID},
		{"collision: not started but transcript exists", false, true, false, "--resume " + transcriptID},
		{"first turn, nothing there", false, false, false, "--session-id " + transcriptID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := transcriptSpec(t, c.started)
			cfg, other := t.TempDir(), t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", cfg)
			cwd := spec.CheckoutPath
			if c.has {
				writeTranscript(t, cfg, cwd)
			}
			if c.inOther {
				writeTranscript(t, other, cwd)
			}
			if got := sessionArgs(t, a, spec); got != c.want {
				t.Fatalf("session args %q, want %q", got, c.want)
			}
		})
	}
}

// A transcript under a folder name Claude spelled differently (a long path) is still found.
func TestTranscriptFoundUnderAnotherFolderName(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "projects", "some-other-folder")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, transcriptID+".jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := transcriptExists(cfg, "/x/y", transcriptID); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// A read problem, a fork and an unusable id keep the flag as it was.
func TestResumeDecisionFallsBack(t *testing.T) {
	a := &Adapter{}
	if !a.resumeOrFresh(transcriptID, true, false, false, []string{"CLAUDE_CONFIG_DIR="}, "/x") {
		t.Fatal("no usable config dir must keep started")
	}
	if !a.resumeOrFresh(transcriptID, true, true, false, []string{"CLAUDE_CONFIG_DIR=" + t.TempDir()}, "/x") {
		t.Fatal("a fork must keep resuming")
	}
	if !a.resumeOrFresh(transcriptID, true, false, true, []string{"CLAUDE_CONFIG_DIR=" + t.TempDir()}, "/x") {
		t.Fatal("a containerized launch must keep started")
	}
	if !a.resumeOrFresh("../bad", true, false, false, []string{"CLAUDE_CONFIG_DIR=" + t.TempDir()}, "/x") {
		t.Fatal("an unusable id must keep started")
	}
}
