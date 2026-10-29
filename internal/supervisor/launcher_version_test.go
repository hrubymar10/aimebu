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
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func versionLauncher(t *testing.T, body string) (protocol.LauncherDefinition, string) {
	t.Helper()
	root := t.TempDir()
	command := filepath.Join(root, "claude-docker")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
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
	if err := os.WriteFile(command, []byte(body), 0o700); err != nil {
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
