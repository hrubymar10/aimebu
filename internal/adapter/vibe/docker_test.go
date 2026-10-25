package vibe

import (
	"context"
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

// dockerLauncher writes a fake vibe-docker. A launcher with the feature probe answers it with the features and exits, as the real one does
// before it touches Docker; one without it behaves like an older release and just runs --version. Otherwise the script records its
// arguments and the environment variables a harness would receive, then runs body.
func dockerLauncher(t *testing.T, root string, probe bool, body string) (path, record string) {
	t.Helper()
	path = filepath.Join(root, "vibe-docker")
	record = filepath.Join(root, "record")
	source := "#!/bin/sh\n"
	if probe {
		source += "if [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n"
	} else {
		source += "case \"$1$2$3\" in *--version*) echo 'vibe 2.25.0'; exit 0;; esac\n"
	}
	source += "{ printf 'ARGS:'; for a in \"$@\"; do printf '[%s]' \"$a\"; done; printf '\\n';" +
		" for v in HARNESS_DOCKER_FORWARD_ENV VIBE_LAUNCH_FLAGS VIBE_HOME VIBE_ACTIVE_MODEL VIBE_SESSION_LOGGING__SAVE_DIR VIBE_MCP_SERVERS AIMEBU2_AGENT_KEY MISTRAL_API_KEY ACME_API_KEY OPENAI_API_KEY GITHUB_TOKEN CODEX_HOME DOCKER_HOST PYTHONUNBUFFERED; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" +
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

func dockerAdapter(t *testing.T, root, path string) (*Adapter, string) {
	t.Helper()
	userHome := filepath.Join(root, "user-vibe")
	if err := os.MkdirAll(userHome, 0700); err != nil {
		t.Fatal(err)
	}
	return &Adapter{Command: path, Variant: "docker", UserHome: userHome}, userHome
}

func TestDockerConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script fixture")
	}
	root := t.TempDir()
	path := filepath.Join(root, "vibe-docker")
	marker := filepath.Join(root, "effect")
	script := `#!/bin/sh
if [ "$HARNESS_DOCKER_PRINT_FEATURES" = 1 ]; then echo forward_env; exit 0; fi
prompt=$(cat)
case "$prompt" in
  *turn-events*) cat ` + fixture(t, "minimal.jsonl") + `
` + persist(testSession, 120, 30, 20, 0.0004, 1.5, 7.5) + ` ;;
  *turn-no_terminal*) : ;;
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
		a, _ := dockerAdapter(t, t.TempDir(), path)
		return a, adapter.TurnSpec{TurnID: "turn-" + string(scenario), Agent: adapter.AgentSpec{ID: "agt_test", Variant: "docker", Model: "mistral-medium-3.5"},
			CheckoutPath: checkout, AgentToolsConfigPath: config, Mode: adapter.ModeNormal, NotesJSON: json.RawMessage(`[]`)}, effected
	}, adaptertest.ChildProcess())
}

func TestDockerTurnForwardsItsConfigurationByNameAndKeepsItOutOfArgv(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	t.Setenv("MISTRAL_API_KEY", "mk-from-the-host")
	t.Setenv("ACME_API_KEY", "acme-from-the-providers-table")
	t.Setenv("OPENAI_API_KEY", "sk-must-not-leak")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_leak")
	t.Setenv("CODEX_HOME", "/must/not/leak")
	t.Setenv("DOCKER_HOST", "unix:///the/docker.sock")
	t.Setenv("AIMEBU2_AGENT_KEY", "aak_must_not_leak")
	a, userHome := dockerAdapter(t, root, path)
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte("[[providers]]\nname = \"acme\"\napi_key_env_var = \"ACME_API_KEY\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	hash := filepath.Base(runtimeDirOf(root, "agt_test"))
	privateHome := filepath.Join(checkout, ".aimebu", "vibe", hash)

	for _, tc := range []struct {
		name    string
		mutate  func(*adapter.TurnSpec)
		trusted bool
		mcp     bool
	}{
		{"first turn", func(*adapter.TurnSpec) {}, true, true},
		{"resumed turn", func(s *adapter.TurnSpec) { s.NativeSessionStarted, s.NativeSessionHandle = true, testSession }, true, true},
		{"diagnostic turn", func(s *adapter.TurnSpec) { s.Mode = adapter.ModeDiagnostic }, false, false},
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
			got := readFile(t, record)
			argv := strings.SplitN(got, "\n", 2)[0]
			if !strings.Contains(argv, "[-p][--output][streaming][--workdir]["+checkout+"]") {
				t.Fatalf("args:\n%s", got)
			}
			if tc.trusted != strings.Contains(argv, "[--trust]") || tc.trusted != strings.Contains(argv, "[--yolo]") || tc.mcp == strings.Contains(argv, "--enabled-tools") {
				t.Fatalf("a normal turn is trusted and approves tools, a read-only one restricts them:\n%s", argv)
			}
			if tc.name == "resumed turn" && !strings.Contains(argv, "[--resume]["+testSession+"]") {
				t.Fatalf("a resumed turn passes the session:\n%s", got)
			}
			if strings.Contains(argv, "aak_") || strings.Contains(argv, "host.docker.internal") || strings.Contains(argv, "mk-from") {
				t.Fatalf("configuration and keys must not travel in argv:\n%s", argv)
			}
			child := strings.Split(got, "PROMPT:")[0]
			names := "VIBE_HOME,VIBE_ACTIVE_MODEL,VIBE_SESSION_LOGGING__SAVE_DIR"
			if tc.mcp {
				names += ",VIBE_MCP_SERVERS,AIMEBU2_AGENT_KEY"
			}
			names += ",PYTHONUNBUFFERED,MISTRAL_API_KEY,ACME_API_KEY"
			for _, want := range []string{"HARNESS_DOCKER_FORWARD_ENV=" + names + "\n", "VIBE_LAUNCH_FLAGS=\n", "VIBE_HOME=" + filepath.Join(privateHome, "home") + "\n",
				"VIBE_SESSION_LOGGING__SAVE_DIR=" + filepath.Join(privateHome, "sessions") + "\n", "VIBE_ACTIVE_MODEL=mistral-medium-3.5\n",
				"MISTRAL_API_KEY=mk-from-the-host", "ACME_API_KEY=acme-from-the-providers-table", "DOCKER_HOST=unix:///the/docker.sock", "PYTHONUNBUFFERED=1\n"} {
				if !strings.Contains(child, want) {
					t.Fatalf("missing %q:\n%s", want, child)
				}
			}
			for _, leaked := range []string{"sk-must-not-leak", "ghp_must_not_leak", "/must/not/leak", "aak_must_not_leak"} {
				if strings.Contains(child, leaked) {
					t.Fatalf("%q reached the launcher's environment:\n%s", leaked, child)
				}
			}
			if tc.mcp {
				var servers []map[string]any
				line := ""
				for _, l := range strings.Split(child, "\n") {
					if strings.HasPrefix(l, "VIBE_MCP_SERVERS=") {
						line = strings.TrimPrefix(l, "VIBE_MCP_SERVERS=")
					}
				}
				if err := json.Unmarshal([]byte(line), &servers); err != nil || len(servers) != 1 || !strings.HasPrefix(servers[0]["url"].(string), "http://host.docker.internal:9897") {
					t.Fatalf("the toolbox as the container reaches the server: %q %v", line, err)
				}
				if strings.Contains(line, "aak_") || !strings.Contains(child, "AIMEBU2_AGENT_KEY=aak_test\n") {
					t.Fatalf("the bearer is bare, in its own variable, and not in the config:\n%s", child)
				}
			} else if !strings.Contains(child, "VIBE_MCP_SERVERS=<unset>") {
				t.Fatalf("a read-only turn carries no toolbox:\n%s", child)
			}
			if _, err := os.Stat(filepath.Join(privateHome, "home", "config.toml")); err != nil {
				t.Fatalf("the private home is seeded below the checkout: %v", err)
			}
		})
	}
	if usage := func() *adapter.Usage {
		turn, err := a.Start(context.Background(), dockerSpec(t, checkout))
		if err != nil {
			t.Fatal(err)
		}
		events, _, _ := drain(t, turn)
		return usageOf(events)
	}(); usage == nil || usage.TokensIn == nil {
		t.Fatalf("the session's stats are read from the host path of the private folder: %+v", usage)
	}
}

// A container that sees the checkout under another path is told the paths as it sees them.
func TestDockerTurnTranslatesPathsForAContainerThatSeesTheCheckoutElsewhere(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\nexit 1\n")
	a, _ := dockerAdapter(t, root, path)
	checkout := t.TempDir()
	turnSpec := dockerSpec(t, checkout)
	turnSpec.VisibleCheckoutPath = "/visible/project"
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, turn)
	got := readFile(t, record)
	hash := filepath.Base(runtimeDirOf(root, "agt_test"))
	for _, want := range []string{"[--workdir][/visible/project]", "VIBE_HOME=/visible/project/.aimebu/vibe/" + hash + "/home\n", "VIBE_SESSION_LOGGING__SAVE_DIR=/visible/project/.aimebu/vibe/" + hash + "/sessions\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(checkout, ".aimebu", "vibe", hash, "home", "config.toml")); err != nil {
		t.Fatalf("the files are written where the host sees them: %v", err)
	}
}

// A launcher that predates the feature probe would start Vibe with none of this configuration: the turn is refused before anything runs.
func TestDockerTurnRefusesALauncherThatCannotForwardTheEnvironment(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, false, "echo harness-ran > "+filepath.Join(root, "ran")+"\n")
	a, _ := dockerAdapter(t, root, path)
	_, err := a.Start(context.Background(), dockerSpec(t, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), "HARNESS_DOCKER_FORWARD_ENV") {
		t.Fatalf("err=%v", err)
	}
	for _, ran := range []string{record, filepath.Join(root, "ran")} {
		if _, statErr := os.Stat(ran); statErr == nil {
			t.Fatal("the harness was started anyway")
		}
	}
}

// An owner's environment references configure the launcher, not the agent.
func TestDockerOwnerEnvironmentReferencesCannotReplaceTheAgentsConfiguration(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	t.Setenv("OWNER_VALUE", "owner-chosen")
	t.Setenv("OWNER_DOCKER", "unix:///owner/docker.sock")
	a, _ := dockerAdapter(t, root, path)
	turnSpec := dockerSpec(t, t.TempDir())
	turnSpec.LauncherEnvRefs = map[string]string{"VIBE_HOME": "OWNER_VALUE", "VIBE_MCP_SERVERS": "OWNER_VALUE", "AIMEBU2_AGENT_KEY": "OWNER_VALUE", "VIBE_LAUNCH_FLAGS": "OWNER_VALUE",
		"HARNESS_DOCKER_FORWARD_ENV": "OWNER_VALUE", "VIBE_ACTIVE_MODEL": "OWNER_VALUE", "DOCKER_HOST": "OWNER_DOCKER"}
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	got := strings.Split(readFile(t, record), "PROMPT:")[0]
	if strings.Contains(got, "owner-chosen") {
		t.Fatalf("an owner's reference replaced what the adapter sets:\n%s", got)
	}
	for _, want := range []string{"VIBE_LAUNCH_FLAGS=\n", "HARNESS_DOCKER_FORWARD_ENV=VIBE_HOME,VIBE_ACTIVE_MODEL,", "VIBE_ACTIVE_MODEL=mistral-medium-3.5", "DOCKER_HOST=unix:///owner/docker.sock"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q (a reference to a launcher variable still applies):\n%s", want, got)
		}
	}
}

func TestDockerAndNativeAdaptersRefuseEachOthersTurns(t *testing.T) {
	root := t.TempDir()
	path, _ := dockerLauncher(t, root, true, "")
	checkout := t.TempDir()
	docker, _ := dockerAdapter(t, root, path)
	if _, err := docker.Start(context.Background(), spec(t, checkout)); err == nil {
		t.Fatal("the Docker adapter must not run a native turn")
	}
	native, _ := adapterFor(t, root, path)
	if _, err := native.Start(context.Background(), dockerSpec(t, checkout)); err == nil {
		t.Fatal("the native adapter must not run a Docker turn")
	}
	bad := dockerSpec(t, checkout)
	bad.CheckoutPath = "relative/path"
	if _, err := docker.Start(context.Background(), bad); err == nil {
		t.Fatal("a Docker checkout must be an absolute folder")
	}
}

func TestDockerCatalogAndCapabilities(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "vibe-docker")
	script := "#!/bin/sh\nif [ \"${VIBE_LAUNCH_FLAGS+x}\" != x ] || [ \"$VIBE_LAUNCH_FLAGS\" != \"\" ]; then echo wrong-flags; exit 1; fi\necho 'vibe 2.25.0'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := NewDocker()
	a.Command = path
	catalog := a.Catalog()
	if len(catalog) != 1 || catalog[0].Tool != "vibe" || catalog[0].Version != "2.25.0" || catalog[0].Environment != (adapter.Environment{Variant: "docker", OS: "linux", Container: true}) {
		t.Fatalf("catalog=%+v", catalog)
	}
	if tool, variant := a.CatalogIdentity(); tool != "vibe" || variant != "docker" {
		t.Fatalf("identity=%s/%s", tool, variant)
	}
	for _, environment := range []adapter.Environment{{Variant: "docker", OS: "linux", Container: true}, {Variant: "native", OS: "linux"}} {
		if caps := a.Capabilities(SupportedVersion, environment); caps != (adapter.Capabilities{}) {
			t.Fatalf("a Docker Vibe claims nothing before its live canary: %+v for %+v", caps, environment)
		}
	}
	if NewDocker().Command != "vibe-docker" {
		t.Fatal("the Docker adapter runs the vibe-docker launcher")
	}
}

// A reference to a variable the adapter forwards (a provider key that is set on the host) cannot swap its value either, nor can one to
// Python's unbuffered switch.
func TestDockerOwnerReferencesCannotSwapAForwardedProviderKey(t *testing.T) {
	root := t.TempDir()
	path, record := dockerLauncher(t, root, true, "cat "+fixture(t, "ok.jsonl")+"\n"+persist(testSession, 10, 5, 0, 0.001, 1, 1))
	t.Setenv("MISTRAL_API_KEY", "mk-from-the-host")
	t.Setenv("ACME_API_KEY", "acme-from-the-host")
	t.Setenv("GITHUB_TOKEN", "ghp_must_not_replace_a_key")
	a, userHome := dockerAdapter(t, root, path)
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"), []byte("[[providers]]\nname = \"acme\"\napi_key_env_var = \"ACME_API_KEY\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turnSpec := dockerSpec(t, t.TempDir())
	turnSpec.LauncherEnvRefs = map[string]string{"MISTRAL_API_KEY": "GITHUB_TOKEN", "ACME_API_KEY": "GITHUB_TOKEN", "PYTHONUNBUFFERED": "GITHUB_TOKEN"}
	turn, err := a.Start(context.Background(), turnSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	got := strings.Split(readFile(t, record), "PROMPT:")[0]
	if strings.Contains(got, "ghp_must_not_replace_a_key") {
		t.Fatalf("a reference replaced a forwarded variable:\n%s", got)
	}
	for _, want := range []string{"MISTRAL_API_KEY=mk-from-the-host\n", "ACME_API_KEY=acme-from-the-host\n", "PYTHONUNBUFFERED=1\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
}

// Vibe's own report of a missing key, from a container, says how the key can get there: the person's .env is linked into the agent's home,
// and the link resolves only if the container mounts their Vibe home at the same path.
func TestDockerMissingKeyReportSaysHowAKeyReachesTheContainer(t *testing.T) {
	root := t.TempDir()
	path, _ := dockerLauncher(t, root, true, "echo 'Error: Missing MISTRAL_API_KEY environment variable for mistral provider.' >&2\nexit 1\n")
	a, userHome := dockerAdapter(t, root, path)
	turn, err := a.Start(context.Background(), dockerSpec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, exit, err := drain(t, turn)
	if err != nil || exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	for _, want := range []string{"Vibe error: Missing MISTRAL_API_KEY", "forwarded by name", userHome + "/.env", "mounts that folder at the same path"} {
		if !strings.Contains(exit.ErrorEvent, want) {
			t.Fatalf("error evidence %q misses %q", exit.ErrorEvent, want)
		}
	}
	// Natively the key comes from the environment or the linked .env, which the process can always see: no hint. An invalid key is not a
	// missing one either.
	native, _ := script(t, t.TempDir(), "echo 'Error: Missing MISTRAL_API_KEY environment variable for mistral provider.' >&2\nexit 1\n")
	b, _ := adapterFor(t, t.TempDir(), native)
	turn, err = b.Start(context.Background(), spec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, nativeExit, _ := drain(t, turn)
	if strings.Contains(nativeExit.ErrorEvent, "mounts that folder") {
		t.Fatalf("a native turn is not told about mounts: %q", nativeExit.ErrorEvent)
	}
	invalid, _ := dockerLauncher(t, t.TempDir(), true, "echo 'Error: API error from mistral: Invalid API key.' >&2\nexit 1\n")
	c, _ := dockerAdapter(t, t.TempDir(), invalid)
	turn, err = c.Start(context.Background(), dockerSpec(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	_, invalidExit, _ := drain(t, turn)
	if strings.Contains(invalidExit.ErrorEvent, "mounts that folder") || !strings.Contains(invalidExit.ErrorEvent, "Invalid API key") {
		t.Fatalf("an invalid key is reported as it is: %q", invalidExit.ErrorEvent)
	}
}
