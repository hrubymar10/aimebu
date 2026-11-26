package claude

import (
	"bytes"
	"log"
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

// A fork, an unreadable config dir, an unusable id and an unmappable launcher keep the flag as it was.
func TestResumeDecisionFallsBack(t *testing.T) {
	a := &Adapter{}
	env := func(dir string) []string { return []string{"CLAUDE_CONFIG_DIR=" + dir} }
	spec := func(mod func(*adapter.TurnSpec)) adapter.TurnSpec {
		s := adapter.TurnSpec{NativeSessionHandle: transcriptID, NativeSessionStarted: true}
		mod(&s)
		return s
	}
	cases := map[string]adapter.TurnSpec{
		"fork":     spec(func(s *adapter.TurnSpec) { s.ForkSession = true }),
		"bad id":   spec(func(s *adapter.TurnSpec) { s.NativeSessionHandle = "../bad" }),
		"own home": spec(func(s *adapter.TurnSpec) { s.LauncherContainerized = true }),
		"unmapped": spec(func(s *adapter.TurnSpec) {
			s.LauncherContainerized = true
			s.LauncherEnvRefs = map[string]string{"CLAUDE_CONFIG_DIR": "X"}
		}),
	}
	for name, s := range cases {
		if !a.resumeOrFresh(s, env(t.TempDir()), "/x") {
			t.Errorf("%s must keep started", name)
		}
	}
	if !a.resumeOrFresh(spec(func(*adapter.TurnSpec) {}), env(""), "/x") {
		t.Error("no usable config dir must keep started")
	}
}

func dockerSpec(t *testing.T, started bool, mod func(*adapter.TurnSpec)) adapter.TurnSpec {
	spec := transcriptSpec(t, started)
	spec.LauncherContainerized = true
	mod(&spec)
	return spec
}

// A Docker launcher is checked in the host's directory: the host's own config dir when it shares the host's Claude home, the host path of
// the container's remapped one otherwise.
func TestDockerLauncherTranscriptCheck(t *testing.T) {
	a := &Adapter{Command: "claude", Variant: "native", AllowNative: true}
	shared := func(s *adapter.TurnSpec) { s.LauncherSharesClaudeHome = true }
	host := t.TempDir()
	t.Run("shared home without a transcript starts fresh", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		if got := sessionArgs(t, a, dockerSpec(t, true, shared)); got != "--session-id "+transcriptID {
			t.Fatal(got)
		}
	})
	t.Run("shared home with a transcript resumes", func(t *testing.T) {
		cfg := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", cfg)
		spec := dockerSpec(t, true, shared)
		writeTranscript(t, cfg, spec.CheckoutPath)
		if got := sessionArgs(t, a, spec); got != "--resume "+transcriptID {
			t.Fatal(got)
		}
	})
	t.Run("a dir under no path map keeps the flag", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		spec := dockerSpec(t, true, func(s *adapter.TurnSpec) {
			s.LauncherEnvRefs = map[string]string{"CLAUDE_CONFIG_DIR": "AIMEBU_TEST_CFG"}
			s.LauncherPathMaps = []adapter.LauncherPathMap{{Host: host, Visible: "/other"}}
		})
		t.Setenv("AIMEBU_TEST_CFG", "/config")
		if got := sessionArgs(t, a, spec); got != "--resume "+transcriptID {
			t.Fatal(got)
		}
	})
	t.Run("a mounted own config dir resolves to its host path", func(t *testing.T) {
		t.Setenv("AIMEBU_TEST_CFG", "/config/acct")
		mapped := func(s *adapter.TurnSpec) {
			s.LauncherEnvRefs = map[string]string{"CLAUDE_CONFIG_DIR": "AIMEBU_TEST_CFG"}
			s.LauncherPathMaps = []adapter.LauncherPathMap{{Host: "/nowhere", Visible: "/"}, {Host: host, Visible: "/config"}}
		}
		spec := dockerSpec(t, true, mapped)
		if got := sessionArgs(t, a, spec); got != "--session-id "+transcriptID {
			t.Fatalf("no transcript: %s", got)
		}
		writeTranscript(t, filepath.Join(host, "acct"), spec.CheckoutPath)
		if got := sessionArgs(t, a, spec); got != "--resume "+transcriptID {
			t.Fatalf("transcript: %s", got)
		}
	})
}

// The Docker variant cannot name the host's config dir. In a real session that is logged once, not on every turn; on the eval's stand-in
// path (StandIn) it is not logged at all.
func TestNoPathMapWarningOncePerSessionAndNeverOnTheStandIn(t *testing.T) {
	count := func(a *Adapter, handles ...string) int {
		var buf bytes.Buffer
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(os.Stderr) })
		for _, h := range handles {
			spec := transcriptSpec(t, true)
			spec.NativeSessionHandle = h
			a.resumeOrFresh(spec, nil, "/x")
		}
		return strings.Count(buf.String(), "no path map to the container's config dir")
	}
	if got := count(&Adapter{Variant: "docker"}, transcriptID, transcriptID, transcriptID); got != 1 {
		t.Fatalf("production: %d warnings for three turns of one session, want 1", got)
	}
	if got := count(&Adapter{Variant: "docker"}, transcriptID, "1f6c1a52-3b7e-4c1d-9a55-7d2e8b1c4f91"); got != 2 {
		t.Fatalf("production: %d warnings for two sessions, want 2", got)
	}
	if got := count(&Adapter{Variant: "docker", StandIn: true}, transcriptID, transcriptID); got != 0 {
		t.Fatalf("stand-in: %d warnings, want 0", got)
	}
}
