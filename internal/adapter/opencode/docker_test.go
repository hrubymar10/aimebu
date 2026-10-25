package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/adaptertest"
)

// dockerLauncher writes a fake opencode-docker. A launcher that has the feature probe answers it with the features and exits, as the real
// one does before it touches Docker; one without it behaves like an older release and just runs --version. Otherwise the script records
// its arguments and the environment variables a harness would receive, then runs body.
func dockerLauncher(t *testing.T, root string, probe bool, body string) (path, record string) {
	t.Helper()
	path = filepath.Join(root, "opencode-docker")
	record = filepath.Join(root, "record")
	source := "#!/bin/sh\n"
	if probe {
		source += "if [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n"
	} else {
		source += "case \"$1$2$3\" in *--version*) echo 1.19.0; exit 0;; esac\n"
	}
	source += "{ printf 'ARGS:'; for a in \"$@\"; do printf '[%s]' \"$a\"; done; printf '\\n';" +
		" for v in HARNESS_DOCKER_FORWARD_ENV OPENCODE_LAUNCH_FLAGS XDG_CONFIG_HOME OPENCODE_DB OPENCODE_CONFIG_CONTENT OPENCODE_DISABLE_AUTOUPDATE OPENCODE_DISABLE_PROJECT_CONFIG OPENCODE_DISABLE_CLAUDE_CODE OLLAMA_API_KEY OPENAI_API_KEY GITHUB_TOKEN CODEX_HOME AIMEBU2_AGENT_KEY DOCKER_HOST OWNER_OVERRIDE; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" +
		"prompt=$(cat)\nprintf 'PROMPT:%s\\n' \"$prompt\" >> " + record + "\n" + body
	if err := os.WriteFile(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path, record
}

func dockerSpec(t *testing.T, checkout string) adapter.TurnSpec {
	s := spec(t, checkout)
	s.Agent.Variant = "docker"
	return s
}

func agentHash(agent string) string {
	sum := sha256.Sum256([]byte(agent))
	return hex.EncodeToString(sum[:])
}

func TestDockerConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "opencode-docker")
	marker := filepath.Join(root, "effect")
	events := fixture(t, "ok.jsonl")
	script := `#!/bin/sh
if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then echo forward_env; exit 0; fi
prompt=$(cat)
case "$prompt" in
  *turn-events*) cat ` + events + ` ;;
  *turn-no_terminal*) printf '%s\n' '{"type":"step_start","sessionID":"` + testSession + `","part":{"type":"step-start"}}' ;;
  *turn-cancel*) exec sleep 30 ;;
  *turn-crash_after_effect*) : > "` + marker + `"; exit 17 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	config := testToolsConfig(t, "agt_test")
	adaptertest.Run(t, func(t *testing.T, scenario adaptertest.Scenario) (adapter.Adapter, adapter.TurnSpec, <-chan struct{}) {
		effected := make(chan struct{})
		if scenario == adaptertest.ScenarioCrashAfterEffect {
			go func() {
				for {
					if _, err := os.Stat(marker); err == nil {
						close(effected)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		} else {
			close(effected)
		}
		return &Adapter{Command: path, Variant: "docker"},
			adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Variant: "docker", Model: "ollama-cloud/gemma4:31b"},
				CheckoutPath: checkout, AgentToolsConfigPath: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestDockerTurnForwardsItsConfigurationByNameAndKeepsItOutOfArgv(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n")
	t.Setenv("OLLAMA_API_KEY", "ollama-key-from-the-host")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_leak")
	t.Setenv("CODEX_HOME", "/must/not/leak")
	t.Setenv("DOCKER_HOST", "unix:///the/docker.sock")
	t.Setenv("AIMEBU2_AGENT_KEY", "aak_must_not_leak")
	checkout := t.TempDir()
	state := filepath.Join(checkout, ".aimebu", "opencode", agentHash("agt_test"))
	a := &Adapter{Command: path, Variant: "docker"}

	for _, tc := range []struct {
		name   string
		mutate func(*adapter.TurnSpec)
		auto   bool
		mcp    bool
		dir    string
		state  string
	}{
		{"first turn", func(*adapter.TurnSpec) {}, true, true, checkout, state},
		{"resumed turn", func(s *adapter.TurnSpec) { s.NativeSessionStarted, s.NativeSessionHandle = true, testSession }, true, true, checkout, state},
		{"diagnostic turn", func(s *adapter.TurnSpec) { s.Mode = adapter.ModeDiagnostic }, false, false, checkout, state},
		{"a container that sees the checkout elsewhere", func(s *adapter.TurnSpec) { s.VisibleCheckoutPath = "/visible/project" }, true, true, "/visible/project",
			"/visible/project/.aimebu/opencode/" + agentHash("agt_test")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turnSpec := dockerSpec(t, checkout)
			tc.mutate(&turnSpec)
			turn, err := a.Start(context.Background(), turnSpec)
			if err != nil {
				t.Fatal(err)
			}
			_, exit, err := drain(t, turn)
			if err != nil || !exit.TerminalEvent {
				t.Fatalf("exit=%+v err=%v", exit, err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			argv := strings.SplitN(got, "\n", 2)[0]
			for _, want := range []string{"[run][--pure][--format][json][-m][ollama-cloud/gemma4:31b][--dir][" + tc.dir + "]"} {
				if !strings.Contains(argv, want) {
					t.Fatalf("args missing %s:\n%s", want, got)
				}
			}
			if hasAuto := strings.Contains(argv, "[--auto]"); hasAuto != tc.auto {
				t.Fatalf("--auto = %t, want %t:\n%s", hasAuto, tc.auto, got)
			}
			if tc.name == "resumed turn" && !strings.Contains(argv, "[-s]["+testSession+"]") {
				t.Fatalf("a resumed turn passes the session:\n%s", got)
			}
			if strings.Contains(argv, "aak_") || strings.Contains(argv, "host.docker.internal") || strings.Contains(argv, "ollama-key") {
				t.Fatalf("configuration and keys must not travel in argv:\n%s", argv)
			}
			child := strings.Split(got, "PROMPT:")[0]
			for _, want := range []string{"OPENCODE_LAUNCH_FLAGS=\n", "XDG_CONFIG_HOME=" + tc.state + "/config\n", "OPENCODE_DB=" + tc.state + "/opencode.db\n",
				"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OLLAMA_API_KEY=ollama-key-from-the-host",
				"DOCKER_HOST=unix:///the/docker.sock",
				"HARNESS_DOCKER_FORWARD_ENV=XDG_CONFIG_HOME,OPENCODE_DB,OPENCODE_CONFIG_CONTENT,OPENCODE_DISABLE_AUTOUPDATE,OPENCODE_DISABLE_PROJECT_CONFIG,OPENCODE_DISABLE_CLAUDE_CODE,OLLAMA_API_KEY\n"} {
				if !strings.Contains(child, want) {
					t.Fatalf("missing %q:\n%s", want, child)
				}
			}
			for _, leaked := range []string{"ghp_must_not_leak", "/must/not/leak", "aak_must_not_leak"} {
				if strings.Contains(child, leaked) {
					t.Fatalf("%q reached the launcher's environment:\n%s", leaked, child)
				}
			}
			var config struct {
				MCP map[string]struct {
					Type, URL string
					Headers   map[string]string
				} `json:"mcp"`
				Permission map[string]string `json:"permission"`
			}
			line := ""
			for _, l := range strings.Split(child, "\n") {
				if strings.HasPrefix(l, "OPENCODE_CONFIG_CONTENT=") {
					line = strings.TrimPrefix(l, "OPENCODE_CONFIG_CONTENT=")
				}
			}
			if err := json.Unmarshal([]byte(line), &config); err != nil {
				t.Fatalf("config %q: %v", line, err)
			}
			if tc.mcp {
				server := config.MCP["aimebu"]
				if server.Type != "remote" || !strings.HasPrefix(server.URL, "http://host.docker.internal:9897") || !strings.HasPrefix(server.Headers["Authorization"], "Bearer aak_") || len(config.Permission) != 0 {
					t.Fatalf("a normal Docker turn reaches the server as the container sees it: %+v", config)
				}
			} else if len(config.MCP) != 0 || config.Permission["bash"] != "deny" || config.Permission["edit"] != "deny" {
				t.Fatalf("a read-only turn denies the acting tools and carries no MCP: %+v", config)
			}
			if info, err := os.Stat(filepath.Join(checkout, ".aimebu", "opencode", agentHash("agt_test"))); err != nil || info.Mode().Perm() != 0700 {
				t.Fatalf("the agent's private state folder: %v %v", info, err)
			}
		})
	}
}

// A launcher that predates the feature probe would start the harness without its configuration or toolbox: the turn is refused before
// anything runs, with the way out in the message.
func TestDockerTurnRefusesALauncherThatCannotForwardTheEnvironment(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, false, "echo harness-ran >> "+filepath.Join(root, "ran")+"\n")
	a := &Adapter{Command: path, Variant: "docker"}
	_, err := a.Start(context.Background(), dockerSpec(t, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "HARNESS_DOCKER_FORWARD_ENV") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(record); statErr == nil {
		t.Fatal("the harness was started anyway")
	}
	if _, statErr := os.Stat(filepath.Join(root, "ran")); statErr == nil {
		t.Fatal("the harness was started anyway")
	}
}

// An owner's environment references configure the launcher, not the agent: one that names a variable the adapter sets for the agent is ignored.
func TestDockerOwnerEnvironmentReferencesCannotReplaceTheAgentsConfiguration(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n")
	t.Setenv("OWNER_VALUE", "owner-chosen")
	t.Setenv("OWNER_DOCKER", "unix:///owner/docker.sock")
	checkout := t.TempDir()
	a := &Adapter{Command: path, Variant: "docker"}
	turnSpec := dockerSpec(t, checkout)
	turnSpec.LauncherEnvRefs = map[string]string{"OPENCODE_DB": "OWNER_VALUE", "OPENCODE_CONFIG_CONTENT": "OWNER_VALUE", "XDG_CONFIG_HOME": "OWNER_VALUE",
		"OPENCODE_LAUNCH_FLAGS": "OWNER_VALUE", "HARNESS_DOCKER_FORWARD_ENV": "OWNER_VALUE", "OPENCODE_DISABLE_PROJECT_CONFIG": "OWNER_VALUE", "DOCKER_HOST": "OWNER_DOCKER"}
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(data), "PROMPT:")[0]
	if strings.Contains(got, "owner-chosen") {
		t.Fatalf("an owner's reference replaced what the adapter sets:\n%s", got)
	}
	for _, want := range []string{"OPENCODE_LAUNCH_FLAGS=\n", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "HARNESS_DOCKER_FORWARD_ENV=XDG_CONFIG_HOME,OPENCODE_DB,OPENCODE_CONFIG_CONTENT,", "DOCKER_HOST=unix:///owner/docker.sock"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q (a reference to a launcher variable still applies):\n%s", want, got)
		}
	}
}

func TestDockerAndNativeAdaptersRefuseEachOthersTurns(t *testing.T) {
	root := t.TempDir()
	path, _ := dockerLauncher(t, root, true, "")
	checkout := t.TempDir()
	if _, err := (&Adapter{Command: path, Variant: "docker"}).Start(context.Background(), spec(t, checkout)); err == nil {
		t.Fatal("the Docker adapter must not run a native turn")
	}
	if _, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), dockerSpec(t, checkout)); err == nil {
		t.Fatal("the native adapter must not run a Docker turn")
	}
	bad := dockerSpec(t, checkout)
	bad.CheckoutPath = "relative/path"
	if _, err := (&Adapter{Command: path, Variant: "docker"}).Start(context.Background(), bad); err == nil {
		t.Fatal("a Docker checkout must be an absolute folder")
	}
}

func TestDockerCatalogAndCapabilities(t *testing.T) {
	root := t.TempDir()
	path, _ := dockerLauncher(t, root, true, "")
	script := "#!/bin/sh\nif [ \"$OPENCODE_LAUNCH_FLAGS\" != \"\" ] || [ \"${OPENCODE_LAUNCH_FLAGS+x}\" != x ]; then echo wrong-flags; exit 1; fi\necho 1.19.0\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := NewDocker()
	a.Command = path
	catalog := a.Catalog()
	if len(catalog) != 1 || catalog[0].Tool != "opencode" || catalog[0].Version != "1.19.0" || catalog[0].Environment != (adapter.Environment{Variant: "docker", OS: "linux", Container: true}) {
		t.Fatalf("catalog=%+v", catalog)
	}
	if tool, variant := a.CatalogIdentity(); tool != "opencode" || variant != "docker" {
		t.Fatalf("identity=%s/%s", tool, variant)
	}
	for _, environment := range []adapter.Environment{{Variant: "docker", OS: "linux", Container: true}, {Variant: "native", OS: "linux"}} {
		if caps := a.Capabilities(SupportedVersion, environment); caps != (adapter.Capabilities{}) {
			t.Fatalf("a Docker OpenCode claims nothing before its live canary: %+v for %+v", caps, environment)
		}
	}
	if NewDocker().Command != "opencode-docker" {
		t.Fatalf("the Docker adapter runs the opencode-docker launcher")
	}
}

// A reference to a provider key that the adapter forwards (it is set on the host) cannot swap its value for another variable's.
func TestDockerOwnerReferencesCannotSwapAForwardedProviderKey(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n")
	t.Setenv("OLLAMA_API_KEY", "ollama-from-the-host")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_replace_a_key")
	a := &Adapter{Command: path, Variant: "docker"}
	turnSpec := dockerSpec(t, t.TempDir())
	turnSpec.LauncherEnvRefs = map[string]string{"OLLAMA_API_KEY": "GITHUB_TOKEN"}
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	got := strings.Split(readFile(t, record), "PROMPT:")[0]
	if strings.Contains(got, "ghp_must_not_replace_a_key") || !strings.Contains(got, "OLLAMA_API_KEY=ollama-from-the-host\n") {
		t.Fatalf("a reference replaced a forwarded provider key:\n%s", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
