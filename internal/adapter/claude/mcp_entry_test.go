package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

// launchArgs runs one normal turn through a scripted launcher and returns its argument list and the environment it was given.
func launchArgs(t *testing.T, variant, agentServerURL string) (args []string, env map[string]string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "claude")
	argsPath, envPath := filepath.Join(root, "args"), filepath.Join(root, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsPath + "\nenv > " + envPath + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"session-1\"}' '{\"type\":\"result\",\"result\":\"{\\\"messages\\\":[]}\"}'\n"
	if err := testenv.WriteExecutable(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	access := &adapter.AgentToolsAccess{AgentID: "agt_test", ServerURL: "http://127.0.0.1:9897", Key: "aak_secret", Command: "/usr/bin/aimebu"}
	a := &Adapter{Command: path, Variant: variant, AllowNative: true}
	spec := adapter.TurnSpec{TurnID: "entry", Agent: adapter.AgentSpec{ID: "agt_test", Tool: "claude", Variant: variant, Model: "haiku"}, CheckoutPath: root, Mode: adapter.ModeNormal, AgentTools: access, AgentServerURL: agentServerURL, NativeSessionHandle: "session-1", NotesJSON: json.RawMessage(`[]`)}
	turn, err := a.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	rawEnv, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	env = map[string]string{}
	for _, line := range strings.Split(string(rawEnv), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), env
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no %s in %q", flag, args)
	return ""
}

// The toolbox entry must survive a person's disabledMcpServers, which Claude applies by name to an inline --mcp-config entry too: a project
// where a v1 bus called aimebu was switched off silently switched off the v2 toolbox of every agent working in it (Docker agents run in the
// host's own spelling of the checkout, so they inherit that project's state through the mounted Claude home). The entry has a name of its
// own, and a v1 bus named aimebu is denied rather than shadowed.
func TestToolboxEntryIsNotNamedLikeAV1Bus(t *testing.T) {
	for _, variant := range []string{"native", "docker"} {
		t.Run(variant, func(t *testing.T) {
			args, _ := launchArgs(t, variant, "")
			var config struct {
				Servers map[string]json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(flagValue(t, args, "--mcp-config")), &config); err != nil {
				t.Fatal(err)
			}
			if len(config.Servers) != 1 || config.Servers[toolboxServer] == nil {
				t.Fatalf("the inline config must hold exactly the %s entry: %v", toolboxServer, config.Servers)
			}
			if toolboxServer == "aimebu" || strings.HasPrefix("mcp__"+toolboxServer+"__", legacyToolsRule+"__") {
				t.Fatalf("the toolbox %q must not be named, or denied, like a v1 bus", toolboxServer)
			}
			if got := flagValue(t, args, "--disallowedTools"); got != legacyToolsRule+",Monitor" {
				t.Fatalf("--disallowedTools = %q, want %q", got, legacyToolsRule+",Monitor")
			}
		})
	}
}

// A harness background job does not wake the agent and dies with the process, so a normal turn has none: Monitor is denied and Claude
// Code's background tasks are switched off, in the container too (the launcher forwards the switch by name next to the agent key).
func TestHarnessBackgroundJobsAreOff(t *testing.T) {
	for _, variant := range []string{"native", "docker"} {
		t.Run(variant, func(t *testing.T) {
			args, env := launchArgs(t, variant, "")
			if got := flagValue(t, args, "--disallowedTools"); !strings.Contains(","+got+",", ",Monitor,") {
				t.Fatalf("--disallowedTools = %q, want Monitor denied", got)
			}
			if env[backgroundTasksEnvVar] != "1" {
				t.Fatalf("%s = %q, want 1", backgroundTasksEnvVar, env[backgroundTasksEnvVar])
			}
			forwarded := "," + env["HARNESS_DOCKER_FORWARD_ENV"] + ","
			if variant == "docker" && (!strings.Contains(forwarded, ","+backgroundTasksEnvVar+",") || !strings.Contains(forwarded, ","+adapter.AgentKeyEnv+",")) {
				t.Fatalf("the launcher must forward the switch and the key: %q", env["HARNESS_DOCKER_FORWARD_ENV"])
			}
		})
	}
}

func TestForwardToDockerAddsToTheList(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{nil, "HARNESS_DOCKER_FORWARD_ENV=X"},
		{[]string{"A=1", "HARNESS_DOCKER_FORWARD_ENV=A"}, "HARNESS_DOCKER_FORWARD_ENV=A,X"},
		{[]string{"HARNESS_DOCKER_FORWARD_ENV="}, "HARNESS_DOCKER_FORWARD_ENV=X"},
	} {
		got := forwardToDocker(tc.env, "X")
		count := 0
		for _, item := range got {
			if strings.HasPrefix(item, "HARNESS_DOCKER_FORWARD_ENV=") {
				count++
				if item != tc.want {
					t.Fatalf("forwardToDocker(%q) = %q, want %q", tc.env, item, tc.want)
				}
			}
		}
		if count != 1 {
			t.Fatalf("forwardToDocker(%q) gave %d lists: %q", tc.env, count, got)
		}
	}
}

// The Docker entry is used inside the container: an HTTP endpoint at an address the container reaches (a loopback server becomes
// host.docker.internal, or the launcher's own server URL), with the bearer a reference to a variable the launcher forwards into the
// container, and no host path or command that would have to exist there.
func TestDockerToolboxEntryResolvesInsideTheContainer(t *testing.T) {
	for _, tc := range []struct{ name, serverURL, wantHost string }{
		{"loopback server", "", "host.docker.internal:9897"},
		{"launcher server URL", "http://10.0.0.5:9999/ignored?x=1", "10.0.0.5:9999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, env := launchArgs(t, "docker", tc.serverURL)
			var config struct {
				Servers map[string]map[string]any `json:"mcpServers"`
			}
			if err := json.Unmarshal([]byte(flagValue(t, args, "--mcp-config")), &config); err != nil {
				t.Fatal(err)
			}
			entry := config.Servers[toolboxServer]
			if entry["type"] != "http" || entry["command"] != nil || entry["args"] != nil {
				t.Fatalf("the Docker entry must be HTTP, with no command to resolve in the container: %v", entry)
			}
			endpoint, err := url.Parse(entry["url"].(string))
			if err != nil || endpoint.Scheme != "http" || endpoint.Host != tc.wantHost || endpoint.Path != "/api/agent/mcp" || endpoint.RawQuery != "" {
				t.Fatalf("endpoint %v (err %v), want http://%s/api/agent/mcp", entry["url"], err, tc.wantHost)
			}
			headers, _ := entry["headers"].(map[string]any)
			if headers["Authorization"] != "Bearer ${"+adapter.AgentKeyEnv+"}" {
				t.Fatalf("headers %v", headers)
			}
			if env[adapter.AgentKeyEnv] != "aak_secret" || !strings.Contains(","+env["HARNESS_DOCKER_FORWARD_ENV"]+",", ","+adapter.AgentKeyEnv+",") {
				t.Fatalf("the key must reach the container through a forwarded variable: key %q, forwarded %q", env[adapter.AgentKeyEnv], env["HARNESS_DOCKER_FORWARD_ENV"])
			}
			if strings.Contains(strings.Join(args, "\n"), "aak_secret") {
				t.Fatalf("the key is in the argument list: %q", args)
			}
		})
	}
}

// fakeMCP answers the few MCP calls Claude makes at startup and lists one tool, so the live test sees which servers' tools Claude loads.
func fakeMCP(t *testing.T, bearer string) *httptest.Server {
	t.Helper()
	tools := map[string]string{"/api/agent/mcp": "ping", "/v1": "bus_say"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tool, ok := tools[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(body, &call) != nil || len(call.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch call.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": tool, "description": "test tool", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
}

// TestLiveToolboxSurvivesADisabledV1Bus runs the real `claude` (no login needed: it reports its MCP servers and tools before it calls the
// API) through the Docker entry against a fake toolbox, with a person's user-scope v1 bus named aimebu, once enabled and once disabled in
// the project. The toolbox must connect and list its tool both times, and the v1 bus's tools must never be listed.
func TestLiveToolboxSurvivesADisabledV1Bus(t *testing.T) {
	if os.Getenv("AIMEBU_LIVE_CC_MCP") != "1" {
		t.Skip("set AIMEBU_LIVE_CC_MCP=1 to run the Claude Code MCP canary (needs `claude` on PATH, no login)")
	}
	real, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	const key = "aak_live_mcp"
	server := fakeMCP(t, key)
	defer server.Close()
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "v1 enabled", true: "v1 disabled"}[disabled], func(t *testing.T) {
			home := t.TempDir()
			checkout := filepath.Join(home, "checkout")
			if err := os.MkdirAll(checkout, 0700); err != nil {
				t.Fatal(err)
			}
			checkout, err := filepath.EvalSymlinks(checkout)
			if err != nil {
				t.Fatal(err)
			}
			configDir := filepath.Join(home, "claude")
			if err := os.MkdirAll(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			project := map[string]any{"hasTrustDialogAccepted": true, "disabledMcpServers": []string{}}
			if disabled {
				project["disabledMcpServers"] = []string{"aimebu"}
			}
			state, _ := json.Marshal(map[string]any{
				"mcpServers": map[string]any{"aimebu": map[string]any{"type": "http", "url": server.URL + "/v1", "headers": map[string]string{"Authorization": "Bearer ${" + adapter.AgentKeyEnv + "}"}}},
				"projects":   map[string]any{checkout: project},
			})
			if err := os.WriteFile(filepath.Join(configDir, ".claude.json"), state, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", configDir)
			t.Setenv("ANTHROPIC_API_KEY", "sk-ant-invalid-for-mcp-canary")
			out := filepath.Join(home, "stdout")
			// The launcher stands in for claude-docker: it runs the real CLI with the turn's arguments and keeps a copy of its output.
			launcher := filepath.Join(home, "claude-launcher")
			if err := testenv.WriteExecutable(launcher, []byte("#!/bin/sh\n'"+real+"' \"$@\" | tee '"+out+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{Command: launcher, Variant: "docker"}
			access := &adapter.AgentToolsAccess{AgentID: "agt_live", ServerURL: "http://127.0.0.1:1", Key: key, Command: "/usr/bin/aimebu"}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			turn, err := a.Start(ctx, adapter.TurnSpec{TurnID: "live-mcp", Agent: adapter.AgentSpec{ID: "agt_live", Tool: "claude", Variant: "docker", Model: "haiku"}, CheckoutPath: checkout, Mode: adapter.ModeNormal, AgentTools: access, AgentServerURL: server.URL, NativeSessionHandle: "00000000-0000-4000-8000-000000000001", NotesJSON: json.RawMessage(`[]`)})
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			_, _ = turn.Wait() // the turn itself fails at the API (no login); only the startup report matters
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Tools   []string `json:"tools"`
				Servers []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
				} `json:"mcp_servers"`
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.Contains(line, `"subtype":"init"`) {
					if err := json.Unmarshal([]byte(line), &report); err != nil {
						t.Fatal(err)
					}
				}
			}
			status := map[string]string{}
			for _, s := range report.Servers {
				status[s.Name] = s.Status
			}
			if status[toolboxServer] != "connected" {
				t.Fatalf("toolbox server status %q, servers %v\n%s", status[toolboxServer], status, raw)
			}
			var toolbox bool
			for _, tool := range report.Tools {
				if tool == "mcp__"+toolboxServer+"__ping" {
					toolbox = true
				}
				if strings.HasPrefix(tool, "mcp__aimebu__") {
					t.Fatalf("a v1 bus tool reached the agent: %s", tool)
				}
			}
			if !toolbox {
				t.Fatalf("the toolbox tool is not listed: %v", report.Tools)
			}
		})
	}
}
