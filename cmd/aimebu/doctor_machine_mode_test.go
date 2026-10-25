package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/agentlog"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func writeMachineKey(t *testing.T, root, relative string, mode os.FileMode, credential hostMachineCredential) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(credential)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckMachineCredential(t *testing.T) {
	credential := hostMachineCredential{MachineID: "mch_1", Key: "key", ServerURL: "https://s.example"}
	t.Run("not paired warns", func(t *testing.T) {
		got, check := checkMachineCredential(t.TempDir())
		if got != nil || check.Status != statusWarn || check.Hint == "" {
			t.Fatalf("%v %+v", got, check)
		}
	})
	t.Run("paired", func(t *testing.T) {
		root := t.TempDir()
		writeMachineKey(t, root, "supervisor/machine.key", 0o600, credential)
		got, check := checkMachineCredential(root)
		if got == nil || got.MachineID != "mch_1" || check.Status != statusOK || strings.Contains(check.Description, "key\"") {
			t.Fatalf("%v %+v", got, check)
		}
	})
	t.Run("host key on the server's machine", func(t *testing.T) {
		root := t.TempDir()
		writeMachineKey(t, root, "server/host-machine.key", 0o600, hostMachineCredential{MachineID: "mch_host", Key: "k"})
		if got, check := checkMachineCredential(root); got == nil || check.Status != statusOK {
			t.Fatalf("%v %+v", got, check)
		}
	})
	t.Run("loose mode fails", func(t *testing.T) {
		root := t.TempDir()
		writeMachineKey(t, root, "supervisor/machine.key", 0o644, credential)
		got, check := checkMachineCredential(root)
		if got != nil || check.Status != statusFail || !strings.Contains(check.Hint, "chmod 600") {
			t.Fatalf("%v %+v", got, check)
		}
	})
	t.Run("garbage fails", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "supervisor", "machine.key")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, check := checkMachineCredential(root); check.Status != statusFail {
			t.Fatalf("%+v", check)
		}
	})
}

func TestCheckSupervisorJournal(t *testing.T) {
	t.Run("none yet", func(t *testing.T) {
		if check := checkSupervisorJournal(filepath.Join(t.TempDir(), "journal.sqlite")); check.Status != statusOK {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("healthy, in use by a running supervisor", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "journal.sqlite")
		createFixtureDatabase(t, path)
		lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(lock, 4242)
		check := checkSupervisorJournal(path)
		if check.Status != statusOK || !strings.Contains(check.Description, "in use by supervisor pid 4242") || !strings.Contains(check.Description, "quick_check ok") {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("a free lock file is not in use", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.sqlite")
		createFixtureDatabase(t, path)
		if err := os.WriteFile(path+".lock", []byte("99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if check := checkSupervisorJournal(path); strings.Contains(check.Description, "in use") {
			t.Fatalf("%+v", check)
		}
	})
	t.Run("corrupt fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.sqlite")
		if err := os.WriteFile(path, []byte(strings.Repeat("junk", 500)), 0o600); err != nil {
			t.Fatal(err)
		}
		if check := checkSupervisorJournal(path); check.Status != statusFail || check.Hint == "" {
			t.Fatalf("%+v", check)
		}
	})
}

func TestCheckAgentToolsConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-tools")
	if check := checkAgentToolsConfig(dir); check.Status != statusOK {
		t.Fatalf("none yet: %+v", check)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := checkAgentToolsConfig(dir); check.Status != statusOK || !strings.Contains(check.Description, "1 config(s)") {
		t.Fatalf("private: %+v", check)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if check := checkAgentToolsConfig(dir); check.Status != statusWarn || !strings.Contains(check.Description, "a.json mode 644") || check.Hint == "" {
		t.Fatalf("readable file: %+v", check)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if check := checkAgentToolsConfig(dir); check.Status != statusWarn || !strings.Contains(check.Description, "folder mode 755") {
		t.Fatalf("readable folder: %+v", check)
	}
}

func TestCheckAgentLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-logs")
	if check := checkAgentLogs(dir); check.Status != statusOK {
		t.Fatalf("none yet: %+v", check)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agent-one"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-one", agentlog.FileName), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := checkAgentLogs(dir); check.Status != statusOK || !strings.Contains(check.Description, "2.0 KiB across 1 agent(s)") {
		t.Fatalf("small: %+v", check)
	}
}

func TestCheckHarnessExecutables(t *testing.T) {
	if check := checkHarnessExecutables(nil); check.Status != statusWarn || check.Hint == "" {
		t.Fatalf("none: %+v", check)
	}
	// A Docker launcher alone is not a native harness.
	if check := checkHarnessExecutables([]protocol.HarnessCandidate{{Family: "claude", Command: "/bin/claude-docker", Containerized: true}}); check.Status != statusWarn {
		t.Fatalf("docker only: %+v", check)
	}
	check := checkHarnessExecutables([]protocol.HarnessCandidate{
		{Family: "codex", Command: "/bin/codex", Version: "codex 1.2"},
		{Family: "claude", Command: "/bin/claude"},
	})
	if check.Status != statusOK || check.Description != "claude /bin/claude; codex /bin/codex (codex 1.2)" {
		t.Fatalf("%+v", check)
	}
}

func TestCheckDockerLauncherFeatures(t *testing.T) {
	original := doctorDockerFeatures
	t.Cleanup(func() { doctorDockerFeatures = original })
	candidates := []protocol.HarnessCandidate{
		{Family: "opencode", Command: "/bin/opencode-docker", Containerized: true},
		{Family: "vibe", Command: "/bin/vibe-docker", Containerized: true},
		{Family: "claude", Command: "/bin/claude-docker", Containerized: true},
		{Family: "pi", Command: "/bin/pi-docker", Containerized: true},
		{Family: "codex", Command: "/bin/codex"}, // native: not probed
	}
	doctorDockerFeatures = func(_ context.Context, command string, _ []string, _ string, _ []string) (map[string]bool, error) {
		switch filepath.Base(command) {
		case "opencode-docker":
			return map[string]bool{adapter.FeatureForwardEnv: true}, nil
		case "vibe-docker":
			return map[string]bool{}, nil // a version string only: too old
		case "claude-docker":
			return map[string]bool{}, nil // claude does not need forward_env
		}
		return nil, fmt.Errorf("probe failed")
	}
	byName := map[string]doctorCheck{}
	for _, check := range checkDockerLauncherFeatures(context.Background(), candidates) {
		byName[check.Name] = check
	}
	if len(byName) != 4 {
		t.Fatalf("want four launcher checks, got %+v", byName)
	}
	if c := byName["launcher opencode-docker"]; c.Status != statusOK || c.Description != "forward_env" {
		t.Fatalf("opencode: %+v", c)
	}
	if c := byName["launcher vibe-docker"]; c.Status != statusWarn || !strings.Contains(c.Description, "too old") || !strings.Contains(c.Hint, "Update harness-docker") {
		t.Fatalf("vibe: %+v", c)
	}
	if c := byName["launcher claude-docker"]; c.Status != statusOK || c.Description != "no features reported" {
		t.Fatalf("claude: %+v", c)
	}
	if c := byName["launcher pi-docker"]; c.Status != statusWarn || !strings.Contains(c.Description, "probe failed") {
		t.Fatalf("pi: %+v", c)
	}
}

func TestCheckDockerForLaunchers(t *testing.T) {
	original := dockerReachable
	t.Cleanup(func() { dockerReachable = original })
	launcher := []protocol.HarnessCandidate{{Family: "claude", Command: "/bin/claude-docker", Containerized: true}}
	dockerReachable = func() bool { return true }
	if got := checkDockerForLaunchers(nil); got[0].Status != statusOK {
		t.Fatalf("%+v", got)
	}
	dockerReachable = func() bool { return false }
	if got := checkDockerForLaunchers(nil); got[0].Status != statusOK {
		t.Fatalf("no launcher, no docker must be ok: %+v", got)
	}
	if got := checkDockerForLaunchers(launcher); got[0].Status != statusWarn || got[0].Hint == "" {
		t.Fatalf("launcher without docker: %+v", got)
	}
}

func TestCheckPairedServer(t *testing.T) {
	credential := hostMachineCredential{MachineID: "mch 1", Key: "secret-key"}
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() != "/api/machines/mch%201/pair-status" || r.Header.Get("Authorization") != "Bearer secret-key" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
	}
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"approved", 200, `{"approved":true,"name":"x"}`, statusOK},
		{"awaiting approval", 200, `{"approved":false}`, statusWarn},
		{"rejected key", 401, `{}`, statusFail},
		{"unreadable answer", 200, `not json`, statusWarn},
		{"server error", 503, ``, statusWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := serve(tt.status, tt.body)
			defer server.Close()
			check := checkPairedServer(context.Background(), server.URL, credential)
			if check.Status != tt.want {
				t.Fatalf("%+v", check)
			}
			if strings.Contains(check.Description, "secret-key") || strings.Contains(check.Hint, "secret-key") {
				t.Fatalf("the machine key leaked into the report: %+v", check)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		server := serve(200, `{}`)
		url := server.URL
		server.Close()
		if check := checkPairedServer(context.Background(), url, credential); check.Status != statusFail || check.Hint == "" {
			t.Fatalf("%+v", check)
		}
	})
}

func TestDoctorMachineMode(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"approved":true}`)
	}))
	defer server.Close()
	writeMachineKey(t, root, "supervisor/machine.key", 0o600, hostMachineCredential{MachineID: "mch_1", Key: "k", ServerURL: server.URL})
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(testServerPort(t, server.URL)))
	t.Setenv("AIMEBU2_URL", "")
	originalDetect, originalDocker := doctorDetectHarnesses, dockerReachable
	t.Cleanup(func() { doctorDetectHarnesses, dockerReachable = originalDetect, originalDocker })
	doctorDetectHarnesses = func(context.Context) []protocol.HarnessCandidate {
		return []protocol.HarnessCandidate{{Family: "claude", Command: "/bin/claude", Version: "1.0"}}
	}
	dockerReachable = func() bool { return false }
	before := treeDigest(t, root)
	var stdout, stderr bytes.Buffer
	code := doctorCommand(context.Background(), []string{"--machine", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	var decoded doctorResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, check := range decoded.Checks {
		names[check.Name] = true
		if check.Scope != scopeMachine {
			t.Errorf("%s scope %q, want machine", check.Name, check.Scope)
		}
	}
	for _, want := range []string{"config dir", "machine credential", "supervisor journal", "agent tools config", "agent logs", "harnesses", "docker", "paired server"} {
		if !names[want] {
			t.Errorf("machine mode is missing %q: %v", want, names)
		}
	}
	for _, server := range []string{"sqlite database", "server", "tls", "disk space", "machines"} {
		if names[server] {
			t.Errorf("machine mode ran the server check %q", server)
		}
	}
	if after := treeDigest(t, root); after != before {
		t.Fatalf("machine mode changed the config root\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
