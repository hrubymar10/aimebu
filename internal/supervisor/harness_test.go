package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestHarnessOverrideValidationWithoutNativeEnvGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := testenv.WriteExecutable(path, []byte("#!/bin/sh\necho codex-cli 1.2.3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"relative", path + " --flag", dir} {
		if _, err := inspectHarness(protocol.HarnessSetting{Tool: "codex", Variant: "native", Enabled: true, OverridePath: bad}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	t.Setenv("AIMEBU_ALLOW_NATIVE", "")
	row, err := inspectHarness(protocol.HarnessSetting{Tool: "codex", Variant: "native", Enabled: true, OverridePath: path})
	if err != nil || row.PolicyDisabled || !row.Available || row.Source != "user" {
		t.Fatalf("native row=%+v err=%v", row, err)
	}
	t.Setenv("AIMEBU_ALLOW_NATIVE", "1")
	row, err = inspectHarness(protocol.HarnessSetting{Tool: "codex", Variant: "native", Enabled: true, OverridePath: path})
	if err != nil || !row.Available || row.LauncherPath != path || row.Version == "" {
		t.Fatalf("available row=%+v err=%v", row, err)
	}
}

func TestDockerSpawnRefreshesVersionAndCapabilitiesAfterFolderMapping(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "mounted")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "pi-docker")
	if err := testenv.WriteExecutable(launcher, []byte("#!/bin/sh\n[ \"$(pwd)\" = \"$TEST_MOUNT\" ] || exit 1\necho 1.1.0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MOUNT", checkout)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Wait for the actual spawn and refreshed status frames. Under the race
	// detector, startup and launcher discovery can exceed a short fixed delay.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got := make(chan []protocol.Frame, 1)
	errorsSeen := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			errorsSeen <- err
			return
		}
		defer conn.CloseNow()
		if _, err := readFrame(ctx, conn); err != nil {
			errorsSeen <- err
			return
		}
		for _, frame := range []protocol.Frame{{V: protocol.Version, Type: protocol.HelloAck},
			{V: protocol.Version, Type: protocol.Spawn, ReqID: "pi/0", AgentID: "agt_pi", AgentKey: "aak_" + strings.Repeat("x", 43), CheckoutPath: checkout,
				Agent: &adapter.AgentSpec{ID: "agt_pi", Tool: "pi", Variant: "docker", Model: "ollama-cloud/gemma4:31b"}}} {
			data, _ := json.Marshal(frame)
			if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
				errorsSeen <- err
				return
			}
		}
		frames := make([]protocol.Frame, 0, 2)
		for range 2 {
			frame, err := readFrame(ctx, conn)
			if err != nil {
				errorsSeen <- err
				return
			}
			frames = append(frames, frame)
		}
		got <- frames
	}))
	defer server.Close()
	journal, err := OpenJournal(ctx, filepath.Join(root, "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	client := NewClient("machine", "key", server.URL, journal, adapter.NewMulti(pi.NewDocker()))
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.RunOnce(ctx) }()
	select {
	case frames := <-got:
		if frames[0].Type != protocol.SpawnResult || frames[0].Result != "ok" || frames[1].Type != protocol.HarnessStatus {
			t.Fatalf("spawn frames=%+v", frames)
		}
		version, noAction := "", false
		for _, row := range frames[1].Harnesses {
			if row.Tool == "pi" && row.Variant == "docker" {
				version = row.Version
			}
		}
		for _, report := range frames[1].Capabilities {
			if report.Tool == "pi" && report.Environment.Variant == "docker" {
				noAction = report.Flags.NoActionMode
			}
		}
		if version != "1.1.0" || !noAction {
			t.Fatalf("Pi Docker version=%q no-action=%v", version, noAction)
		}
	case err := <-errorsSeen:
		t.Fatal(err)
	case err := <-clientDone:
		t.Fatalf("supervisor exited before spawn status: %v", err)
	case <-ctx.Done():
		t.Fatalf("spawn and harness status were not received within 30 seconds: %v", ctx.Err())
	}
}

func TestDockerDiscoveryOutsideMirroredCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "mounted")
	supervisorDir := filepath.Join(root, "supervisor")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(supervisorDir, 0700); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(supervisorDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	launcher := filepath.Join(root, "claude-docker")
	// (The launcher answers the feature probe like a current one: Claude's Docker turns need forward_env.)
	script := "#!/bin/sh\nif [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\nprintf '%s\\n' \"$(pwd)\" >> \"$TEST_PROBE_LOG\"\nif [ \"$(pwd)\" != \"$TEST_MOUNT\" ]; then echo 'not mounted' >&2; exit 1; fi\necho claude 1.2.3\n"
	if err := testenv.WriteExecutable(launcher, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MOUNT", checkout)
	probeLog := filepath.Join(root, "probes")
	t.Setenv("TEST_PROBE_LOG", probeLog)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	row, err := inspectHarness(protocol.HarnessSetting{Tool: "claude", Variant: "docker", Enabled: true})
	if err != nil || !row.Available || row.LauncherPath != launcher || row.Version != "" {
		t.Fatalf("discovery=%+v err=%v", row, err)
	}
	if _, err := os.Stat(probeLog); !os.IsNotExist(err) {
		t.Fatalf("version probe ran without a mounted directory: %v", err)
	}
	row, err = inspectHarness(protocol.HarnessSetting{Tool: "claude", Variant: "docker", Enabled: true}, checkout)
	if err != nil || !row.Available || row.Version != "claude 1.2.3" {
		t.Fatalf("mounted discovery=%+v err=%v", row, err)
	}
	probes, err := os.ReadFile(probeLog)
	if err != nil || string(probes) != checkout+"\n" {
		t.Fatalf("version probe cwd=%q err=%v", probes, err)
	}
	multi := adapter.NewMulti(claude.NewDocker())
	_, err = multi.Start(context.Background(), adapter.TurnSpec{Agent: adapter.AgentSpec{Tool: "claude", Variant: "docker"}})
	if err == nil || strings.Contains(err.Error(), "no adapter") {
		t.Fatalf("adapter registration: %v", err)
	}
	if err := adapter.DockerPreflight("claude", checkout, row.LauncherPath); err != nil {
		t.Fatal(err)
	}
}

// The legacy Docker harness row, like a launcher row, is asked from each of the machine's folders in turn until one answers, within one deadline.
func TestLegacyDockerHarnessIsAskedFromEveryFolder(t *testing.T) {
	root, good, bad := t.TempDir(), t.TempDir(), t.TempDir()
	launcher := filepath.Join(root, "claude-docker")
	script := "#!/bin/sh\ncase \"$PWD\" in *" + filepath.Base(good) + ") echo 'claude 4.5.6';; *) echo \"Error: Directory '$PWD' is not mounted\" >&2; exit 1;; esac\n"
	if err := testenv.WriteExecutable(launcher, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	setting := protocol.HarnessSetting{Tool: "claude", Variant: "docker", Enabled: true}
	row, err := inspectHarness(setting, bad, good)
	if err != nil || !row.Available || row.Version != "claude 4.5.6" {
		t.Fatalf("the second folder is mounted: %+v %v", row, err)
	}
	row, err = inspectHarness(setting, bad)
	if err != nil || !row.Available || row.Version != "" {
		t.Fatalf("no folder is mounted: %+v %v", row, err)
	}
	// Through the client the folders are the ones the server sent: the first known to work, then the machine's.
	client := &Client{harnessProbeDir: bad, harnessProbeMore: []string{good}, Harnesses: []protocol.HarnessRow{{HarnessSetting: setting}}}
	if err := client.setHarness(setting); err != nil || client.Harnesses[0].Version != "claude 4.5.6" {
		t.Fatalf("setHarness: %+v %v", client.Harnesses[0], err)
	}
}
