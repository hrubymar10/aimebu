package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

func versionLauncher(t *testing.T, body string) (protocol.LauncherDefinition, string) {
	t.Helper()
	root := t.TempDir()
	command := filepath.Join(root, "claude-docker")
	if err := testenv.WriteExecutable(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return protocol.LauncherDefinition{ID: "mln_docker", Tool: "claude", Name: "harness-docker", Command: command, Enabled: true, Containerized: true, Toolbox: true}, root
}

// "Version unknown" used to be all a container launcher could say. The row now says why.
func TestContainerLauncherRowSaysWhyItsVersionIsUnknown(t *testing.T) {
	// Asked from a folder its container does not mount, a launcher says so on stderr and fails.
	def, root := versionLauncher(t, `echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	row, err := inspectLauncher(def, root)
	if err != nil || row.Version != "" || !strings.Contains(row.VersionProblem, "is not mounted in the harness container") || !row.Available {
		t.Fatalf("a launcher that refuses the folder: %+v %v", row, err)
	}
	// With no folder to ask from, nothing is run, and the row says what is missing.
	row, err = inspectLauncher(def, "")
	if err != nil || row.Version != "" || !strings.Contains(row.VersionProblem, "no project folder") {
		t.Fatalf("no folder: %+v %v", row, err)
	}
	// A launcher that answers has no problem to report.
	ok, root := versionLauncher(t, `echo "2.1.310 (Claude Code)"`)
	row, err = inspectLauncher(ok, root)
	if err != nil || row.Version != "2.1.310 (Claude Code)" || row.VersionProblem != "" {
		t.Fatalf("an answering launcher: %+v %v", row, err)
	}
	// A native launcher is not asked from a folder at all: its probe works without one, and its failure stays "unavailable".
	native, _ := versionLauncher(t, `echo "native 1.0"`)
	native.Containerized = false
	if row, err := inspectLauncher(native, ""); err != nil || row.Version != "native 1.0" || row.VersionProblem != "" {
		t.Fatalf("native: %+v %v", row, err)
	}
}

// A launcher that is slow to answer is told apart from one that refuses, so it can be asked again with more time.
func TestLauncherVersionProbeTellsATimeoutFromARefusal(t *testing.T) {
	def, root := versionLauncher(t, `sleep 5`)
	started := time.Now()
	if version, problem := probeLauncherVersion(def, def.Command, root, 100*time.Millisecond); version != "" || problem != versionTimedOut || time.Since(started) > 3*time.Second {
		t.Fatalf("slow launcher: version=%q problem=%q after %s", version, problem, time.Since(started))
	}
	refusing, root := versionLauncher(t, `echo nope >&2; exit 3`)
	if _, problem := probeLauncherVersion(refusing, refusing.Command, root, 2*time.Second); problem != "nope" {
		t.Fatalf("refusal: %q", problem)
	}
	silent, root := versionLauncher(t, `exit 4`)
	if _, problem := probeLauncherVersion(silent, silent.Command, root, 2*time.Second); !strings.Contains(problem, "exit status 4") {
		t.Fatalf("a launcher that fails without a word: %q", problem)
	}
}

func TestContainerLauncherVersionRecoversAfterStartupFailure(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "container-launcher")
	calls := filepath.Join(root, "version-calls")
	first := filepath.Join(root, "first-probe")
	second := filepath.Join(root, "second-probe")
	body := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n" +
		"if [ ! -f \"" + first + "\" ]; then echo first > \"" + first + "\"; echo first >> \"" + calls + "\"; echo 'not started' >&2; exit 1; fi\n" +
		"if [ ! -f \"" + second + "\" ]; then echo second > \"" + second + "\"; echo second >> \"" + calls + "\"; echo 'not started' >&2; exit 1; fi\n" +
		"echo third >> \"" + calls + "\"; echo 'codex 2.0'\n"
	if err := testenv.WriteExecutable(command, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	def := protocol.LauncherDefinition{ID: "container", Tool: "codex", Name: "harness-docker", Command: command, Enabled: true, Containerized: true}
	row, err := inspectLauncher(def, root)
	if err != nil || row.Version != "" || row.VersionProblem != "not started" {
		t.Fatalf("initial row: %+v %v", row, err)
	}
	frames := make(chan protocol.Frame, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			frame, err := readFrame(r.Context(), conn)
			if err != nil {
				return
			}
			frames <- frame
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	client := NewClient("machine", "key", "", nil, adapter.NewMulti(adapter.NewFake()))
	client.Launchers = []protocol.LauncherRow{row,
		{LauncherDefinition: protocol.LauncherDefinition{ID: "native", Command: command}, ResolvedCommand: command, VersionProblem: "not started"},
		{LauncherDefinition: protocol.LauncherDefinition{ID: "known", Command: command, Containerized: true}, ResolvedCommand: command, Version: "already known"}}
	client.harnessProbeDir, client.currentConn, client.launcherReprobeInterval = root, conn, 10*time.Millisecond
	// The main loop builds frames from a snapshot while the background probe replaces the reports; under -race an unlocked read or write fails here.
	done := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
				_ = protocol.Frame{Capabilities: client.capabilityReportsSnapshot()}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer func() { close(done); readers.Wait() }()
	client.startLauncherReprobe(ctx)
	select {
	case frame := <-frames:
		if frame.Type != protocol.LauncherStatus || len(frame.Launchers) != 3 || frame.Launchers[0].Version != "codex 2.0" || frame.Launchers[0].VersionProblem != "" || len(frame.Capabilities) == 0 || frame.Capabilities[0].Version != "codex 2.0" {
			t.Fatalf("updated status: %+v", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no updated launcher status")
	}
	data, err := os.ReadFile(calls)
	if err != nil || strings.TrimSpace(string(data)) != "first\nsecond\nthird" {
		t.Fatalf("unexpected version probes: %q %v", data, err)
	}
}

// A container launcher is asked from the machine's project folders in turn: the first may be one its container does not mount, which must
// not hide the version. When none answers, the most useful problem is returned.
func TestContainerLauncherVersionTriesEveryProjectFolder(t *testing.T) {
	good, bad := t.TempDir(), t.TempDir()
	def, _ := versionLauncher(t, `case "$PWD" in *`+filepath.Base(good)+`) echo "2.1.284 (Claude Code)";; *) echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1;; esac`)
	// The first folder is not mounted, the second is.
	if version, problem := probeLauncherVersionIn(context.Background(), def, def.Command, []string{bad, good}, 5*time.Second); version != "2.1.284 (Claude Code)" || problem != "" {
		t.Fatalf("version after an unmounted folder: %q %q", version, problem)
	}
	row, err := inspectLauncher(def, bad, good)
	if err != nil || row.Version != "2.1.284 (Claude Code)" || row.VersionProblem != "" {
		t.Fatalf("a row asked from both folders: %+v %v", row, err)
	}
	// None mounted: one problem that says so, not the text of whichever folder came first.
	other := t.TempDir()
	if version, problem := probeLauncherVersionIn(context.Background(), def, def.Command, []string{bad, other}, 5*time.Second); version != "" || !strings.Contains(problem, "none of this machine's 2 project folders is mounted") {
		t.Fatalf("no folder mounted: %q %q", version, problem)
	}
	// A single folder keeps the launcher's own words.
	if _, problem := probeLauncherVersionIn(context.Background(), def, def.Command, []string{bad}, 5*time.Second); !strings.Contains(problem, "is not mounted in the harness container") {
		t.Fatalf("one folder: %q", problem)
	}
	// A different failure outranks "not mounted": that is what the owner can act on.
	broken, _ := versionLauncher(t, `case "$PWD" in *`+filepath.Base(bad)+`) echo "Error: Docker is not running." >&2; exit 1;; *) echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1;; esac`)
	if _, problem := probeLauncherVersionIn(context.Background(), broken, broken.Command, []string{other, bad}, 5*time.Second); !strings.Contains(problem, "Docker is not running") {
		t.Fatalf("the actionable problem: %q", problem)
	}
	// No folder at all runs the launcher where the supervisor is, as a native one is.
	if version, _ := probeLauncherVersionIn(context.Background(), def, def.Command, nil, 5*time.Second); version != "" {
		t.Fatalf("no folder, an unmounted working directory: %q", version)
	}
}

// The feature probe exits before it touches Docker, so a container launcher reports its features (shares_claude_home among them) even when
// the machine has no project folder yet; it runs in an empty private folder that is removed afterwards.
func TestContainerLauncherFeaturesNeedNoProjectFolder(t *testing.T) {
	record := filepath.Join(t.TempDir(), "cwd")
	def, _ := versionLauncher(t, `if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then pwd > `+record+`; ls -A . | wc -l >> `+record+`; echo forward_env; echo claude_home_shared; exit 0; fi; echo "9.9.9"`)
	row, err := inspectLauncher(def, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(row.Features, ",") != "claude_home_shared,forward_env" || row.FeatureProblem != "" {
		t.Fatalf("features without a folder: %+v", row)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(data))
	if len(lines) != 2 || lines[1] != "0" || !strings.Contains(lines[0], "aimebu-feature-probe-") {
		t.Fatalf("the probe ran in %v, want an empty private folder", lines)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("the private folder was left behind: %v", err)
	}
	// An old launcher ignores the probe and would start the harness; in a folder its container does not mount it dies first, and that is
	// all the row learns about its features: none, and since Claude's Docker turns need forward_env it says the launcher is too old.
	old, _ := versionLauncher(t, `echo "Error: Directory '$PWD' is not mounted in the harness container." >&2; exit 1`)
	if row, err := inspectLauncher(old, ""); err != nil || len(row.Features) != 0 || !strings.Contains(row.FeatureProblem, "launcher too old") {
		t.Fatalf("an old launcher: %+v %v", row, err)
	}
}

// A launcher that hangs has one deadline however many folders the machine has, and the first timeout ends the asking: the hello loop asks
// launchers one after another before it reads a frame, so eight folders must not make a hanging launcher eight times as slow (nor a few of
// them push the connection past the server's frame timeout).
func TestHangingContainerLauncherIsAskedOnceWithinOneDeadline(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	// It answers the feature probe at once (that exits before Docker) and hangs on --version, as a launcher whose container is starting does.
	def, _ := versionLauncher(t, `if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then echo forward_env; exit 0; fi; echo "$PWD" >> `+calls+`; sleep 30`)
	dirs := make([]string, 8)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	started := time.Now()
	version, problem := probeLauncherVersionIn(context.Background(), def, def.Command, dirs, 500*time.Millisecond)
	elapsed := time.Since(started)
	if version != "" || problem != versionTimedOut {
		t.Fatalf("a hanging launcher: %q %q", version, problem)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("asked %d folders for %v: the whole launcher gets one deadline of 500ms", len(dirs), elapsed)
	}
	data, _ := os.ReadFile(calls)
	if got := len(strings.Fields(string(data))); got != 1 {
		t.Fatalf("the launcher was started %d times; the first timeout must end the asking", got)
	}
	// Through a launcher row the same limit holds: eight folders cost one probe of versionProbeTimeout, not eight.
	started = time.Now()
	row, err := inspectLauncher(def, dirs[0], dirs[1:]...)
	if err != nil || row.Version != "" || row.VersionProblem != versionTimedOut || time.Since(started) > versionProbeTimeout+3*time.Second {
		t.Fatalf("row of a hanging launcher: %+v %v after %v", row, err, time.Since(started))
	}
	if data, _ = os.ReadFile(calls); len(strings.Fields(string(data))) != 2 {
		t.Fatalf("the row's probe started the launcher %d times in all, want once for it", len(strings.Fields(string(data))))
	}
}
