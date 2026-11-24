package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// A Docker launcher whose tool depends on a feature is asked for its features when it is inspected in a checkout, and the answer lands on
// the row for doctor and Settings; a launcher that predates the probe, or one that fails it, gets a problem and stays available (the turn's preflight
// still refuses it with its own message).
func TestInspectLauncherRecordsDockerFeatures(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "vibe-docker")
	write := func(body string) {
		t.Helper()
		if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	def := protocol.LauncherDefinition{ID: "mln_v", Tool: "vibe", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}

	checkout := filepath.Join(root, "project")
	if err := os.Mkdir(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\nother_thing\n'; exit 0; fi
echo 2.0`)
	row, err := inspectLauncher(def, checkout)
	if err != nil || !row.Available || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "forward_env,other_thing" {
		t.Fatalf("a current launcher: %+v %v", row, err)
	}

	write("echo 2.0") // predates the probe: it answers with a version, which names no feature
	row, err = inspectLauncher(def, checkout)
	if err != nil || !row.Available || len(row.Features) != 0 || !strings.Contains(row.FeatureProblem, "launcher too old") || !strings.Contains(row.FeatureProblem, "vibe") {
		t.Fatalf("an old launcher: %+v %v", row, err)
	}

	write("echo boom >&2; exit 3")
	row, err = inspectLauncher(def, checkout)
	if err != nil || !row.Available || !strings.Contains(row.FeatureProblem, "feature probe failed") {
		t.Fatalf("a failing probe: %+v %v", row, err)
	}

	// With no project folder the probe still runs, from an empty folder of ours: a current launcher answers before it touches Docker, and one
	// that predates the probe checks the mount first and says so, which is read as "too old", not as a failure.
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\n'; exit 0; fi
echo 2.0`)
	if row, err = inspectLauncher(def, ""); err != nil || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "forward_env" || !row.Available {
		t.Fatalf("no checkout, a current launcher: %+v %v", row, err)
	}
	write(`echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	if row, err = inspectLauncher(def, ""); err != nil || !strings.Contains(row.FeatureProblem, "launcher too old") || !row.Available {
		t.Fatalf("no checkout, an old launcher: %+v %v", row, err)
	}
	write("echo should-not-run; exit 9")
	// Native launchers and Docker launchers of a tool that needs no feature are not probed for one they cannot have.
	native := def
	native.Containerized = false
	if row, err = inspectLauncher(native, checkout); err != nil || row.FeatureProblem != "" || row.Features != nil {
		t.Fatalf("native: %+v %v", row, err)
	}
	// A Docker tool that needs no feature (Pi) is not asked for one, and a launcher that cannot answer is no problem.
	pi := def
	pi.Tool = "pi"
	pi.Toolbox = false
	if row, err = inspectLauncher(pi, checkout); err != nil || row.FeatureProblem != "" || row.Features != nil || !row.Available {
		t.Fatalf("a pi-docker launcher that fails the probe: %+v %v", row, err)
	}
	write("echo 2.0") // predates the probe: nothing is known, and nothing is said
	if row, err = inspectLauncher(pi, checkout); err != nil || row.FeatureProblem != "" || len(row.Features) != 0 || !row.Available {
		t.Fatalf("an old pi-docker launcher: %+v %v", row, err)
	}
	// Claude's Docker turns hand the agent's key over by name, so an old launcher is too old for it, like one for OpenCode.
	claude := def
	claude.Tool = "claude"
	if row, err = inspectLauncher(claude, checkout); err != nil || !strings.Contains(row.FeatureProblem, "launcher too old") || !row.Available {
		t.Fatalf("an old claude-docker launcher: %+v %v", row, err)
	}
	write(`if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then printf 'forward_env\nclaude_home_shared\n'; exit 0; fi
echo 2.0`)
	if row, err = inspectLauncher(claude, checkout); err != nil || row.FeatureProblem != "" || strings.Join(row.Features, ",") != "claude_home_shared,forward_env" {
		t.Fatalf("a claude-docker launcher that shares the Claude home: %+v %v", row, err)
	}
}

func TestCodexLauncherHomeProbeIsCachedAndInvalidated(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "codex-docker")
	logPath := filepath.Join(root, "probes")
	write := func(home string) {
		t.Helper()
		body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo x >> \"" + logPath + "\"; printf 'forward_env\\nstop_on_signal\\ncodex_home_shared\\ncodex_home=" + home + "\\n'; exit 0; fi\necho 2.0\n"
		if err := testenv.WriteExecutable(command, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	def := protocol.LauncherDefinition{ID: "mln_codex", Tool: "codex", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}
	write(filepath.Join(root, "first"))
	c := &Client{}
	c.harnessProbeDir = root
	if err := c.setLauncher(def); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := c.launcherRow(def.ID).CodexHome; got != filepath.Join(root, "first") {
			t.Fatalf("cached home %q", got)
		}
	}
	count, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(count), "x\n") != 1 {
		t.Fatalf("probe count %q %v", count, err)
	}
	write(filepath.Join(root, "second"))
	if err := c.setLauncher(def); err != nil {
		t.Fatal(err)
	}
	if got := c.launcherRow(def.ID).CodexHome; got != filepath.Join(root, "second") {
		t.Fatalf("changed home %q", got)
	}
	count, err = os.ReadFile(logPath)
	if err != nil || strings.Count(string(count), "x\n") != 2 {
		t.Fatalf("reprobe count %q %v", count, err)
	}
	write("")
	if err := c.setLauncher(def); err != nil {
		t.Fatal(err)
	}
	if got := c.launcherRow(def.ID).CodexHome; got != "" {
		t.Fatalf("missing home retained %q", got)
	}
}

func TestCodexHomeProbeTimeoutIsBounded(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "codex-docker")
	body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then sleep 30; exit 0; fi\necho 2.0\n"
	if err := testenv.WriteExecutable(command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "mln_codex", Tool: "codex", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}
	row := protocol.LauncherRow{LauncherDefinition: def}
	started := time.Now()
	probeLauncherFeaturesWithTimeout(&row, def, command, root, 40*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("probe took %v", elapsed)
	}
	if row.CodexHome != "" || !strings.Contains(row.FeatureProblem, "deadline exceeded") {
		t.Fatalf("timeout answer: %+v", row)
	}
}

func TestCodexLauncherWithoutHomeReportsWarmLimitation(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "old-codex-docker")
	body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then printf 'forward_env\\nstop_on_signal\\ncodex_home_shared\\n'; exit 0; fi\necho 2.0\n"
	if err := testenv.WriteExecutable(command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "mln_old_codex", Tool: "codex", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}
	row, err := inspectLauncher(def, root)
	if err != nil || !row.Available || row.CodexHome != "" || !strings.Contains(row.FeatureProblem, "codex_home") {
		t.Fatalf("missing home was not shown on available launcher: %+v %v", row, err)
	}
	c := &Client{Launchers: []protocol.LauncherRow{row}, Warm: WarmConfig{Idle: time.Minute}}
	if got := c.launcherRows()[0].FeatureProblem; got != "" {
		t.Fatalf("warm-off launcher reported a warm limitation: %q", got)
	}
	c.setCodexWarmEnabled(true)
	if got := c.launcherRows()[0].FeatureProblem; !strings.Contains(got, "codex_home") {
		t.Fatalf("warm-on launcher hid missing home: %q", got)
	}
	c.setCodexWarmEnabled(false)
	if got := c.launcherRows()[0].FeatureProblem; got != "" {
		t.Fatalf("turning warm off retained the limitation: %q", got)
	}
}

func TestCodexDockerHomeReprobeAllowsNextTurnWarm(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "codex-docker")
	ready := filepath.Join(root, "container-running")
	logPath := filepath.Join(root, "probes")
	home := filepath.Join(root, "shared-codex")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"account_id":"shared","access_token":"token"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo x >> \"" + logPath + "\"; printf 'forward_env\\nstop_on_signal\\ncodex_home_shared\\n'; if [ -f \"" + ready + "\" ]; then printf 'codex_home=" + home + "\\n'; fi; exit 0; fi\necho 2.0\n"
	if err := testenv.WriteExecutable(command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "mln_codex", Tool: "codex", Name: "docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}
	c := &Client{Adapter: adapter.NewMulti(codex.NewDocker()), Warm: WarmConfig{Idle: time.Minute}, codexWarmEnabled: true, harnessProbeDir: root}
	if err := c.setLauncher(def); err != nil {
		t.Fatal(err)
	}
	row := c.launcherRow(def.ID)
	if row.CodexHome != "" {
		t.Fatalf("stopped container reported home %q", row.CodexHome)
	}
	if !strings.Contains(row.FeatureProblem, "codex_home") {
		t.Fatalf("missing home has no visible warning: %+v", row)
	}
	if err := os.WriteFile(ready, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The first turn has already copied an empty home into its spec. Its probe
	// must return immediately and leave that turn cold.
	first := adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "codex", LauncherID: def.ID}, LauncherContainerized: true, LauncherCodexHome: row.CodexHome}
	if got := c.warmCodexAuthPath(first); got != "" {
		t.Fatalf("first turn unexpectedly warm with %q", got)
	}
	started := time.Now()
	c.maybeReprobeCodexHome(row)
	c.maybeReprobeCodexHome(row)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("reprobe blocked turn for %v", elapsed)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.launcherRow(def.ID).CodexHome == home {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	row = c.launcherRow(def.ID)
	if row.CodexHome != home {
		t.Fatalf("background probe did not cache home: %+v", row)
	}
	if row.FeatureProblem != "" {
		t.Fatalf("successful reprobe retained missing-home warning: %+v", row)
	}
	next := first
	next.LauncherCodexHome = row.CodexHome
	if path := c.warmCodexAuthPath(next); path != filepath.Join(home, "auth.json") {
		t.Fatalf("next turn auth path %q", path)
	} else if _, ok := warmCredentials(next, true, path); !ok {
		t.Fatal("next turn could not use reported Docker home for warm auth")
	}
	count, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(count), "x\n") != 2 {
		t.Fatalf("expected one initial and one background probe, got %q: %v", count, err)
	}
}
