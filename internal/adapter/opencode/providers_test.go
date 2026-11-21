package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/testenv"
)

const providerSecret = "sk-corp-secret-value-123"

// providerRecorder writes a fake opencode (or opencode-docker, which answers the feature probe first) that records its arguments, the
// forward list, the inline config and what it finds in the provider's key variables, then replays a finished turn.
func providerRecorder(t *testing.T, root string, docker bool) (path, record string) {
	t.Helper()
	path, record = filepath.Join(root, "opencode"), filepath.Join(root, "record")
	source := "#!/bin/sh\n"
	if docker {
		path = filepath.Join(root, "opencode-docker")
		source += "if [ \"$HARNESS_DOCKER_PRINT_FEATURES\" = 1 ]; then echo forward_env; exit 0; fi\n"
	}
	source += "{ printf 'ARGS:'; for a in \"$@\"; do printf '[%s]' \"$a\"; done; printf '\\n';" +
		" for v in HARNESS_DOCKER_FORWARD_ENV CORP_LLM_KEY OTHER_KEY PATH_PROVIDER_KEY OPENCODE_CONFIG_CONTENT; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" +
		"cat > /dev/null\ncat " + fixture(t, "ok.jsonl") + "\n"
	if err := testenv.WriteExecutable(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path, record
}

func corpProvider() adapter.CustomProvider {
	return adapter.CustomProvider{ID: "corp-llm", Name: "Corp LLM", NPM: adapter.CustomProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_LLM_KEY",
		ContextTokens: 128000, OutputTokens: 8192, Models: []adapter.CustomProviderModel{{ID: "fast", Name: "Fast"}, {ID: "big", Name: "Big"}}}
}

func runProviderTurn(t *testing.T, a *Adapter, s adapter.TurnSpec) (string, error) {
	t.Helper()
	turn, err := a.Start(context.Background(), s)
	if err != nil {
		return "", err
	}
	if _, exit, err := drain(t, turn); err != nil || !exit.TerminalEvent {
		t.Fatalf("exit=%+v err=%v", exit, err)
	}
	return "", nil
}

func recordedConfig(t *testing.T, record string) (config map[string]any, raw string) {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	raw = string(data)
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "OPENCODE_CONFIG_CONTENT=") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "OPENCODE_CONFIG_CONTENT=")), &config); err != nil {
				t.Fatal(err)
			}
		}
	}
	if config == nil {
		t.Fatalf("no inline config recorded:\n%s", raw)
	}
	return config, raw
}

func providerEntry(t *testing.T, config map[string]any, id string) map[string]any {
	t.Helper()
	providers, _ := config["provider"].(map[string]any)
	entry, _ := providers[id].(map[string]any)
	if entry == nil {
		t.Fatalf("no provider %q in %+v", id, config)
	}
	return entry
}

// A native turn on a custom provider gets the provider in its private config with the key as a reference, passes the key variable through
// by name, and never has the key's value in the config or the arguments.
func TestNativeTurnOnACustomProvider(t *testing.T) {
	root := t.TempDir()
	path, record := providerRecorder(t, root, false)
	t.Setenv("CORP_LLM_KEY", providerSecret)
	t.Setenv("OTHER_KEY", "other-key-must-not-reach-the-child")
	checkout := t.TempDir()
	s := spec(t, checkout)
	s.Agent.Model = "corp-llm/fast"
	s.Providers = []adapter.CustomProvider{corpProvider()}
	if _, err := runProviderTurn(t, &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}, s); err != nil {
		t.Fatal(err)
	}
	config, raw := recordedConfig(t, record)
	entry := providerEntry(t, config, "corp-llm")
	options := entry["options"].(map[string]any)
	if entry["npm"] != adapter.CustomProviderNPM || entry["name"] != "Corp LLM" || options["baseURL"] != "https://llm.example.com/v1" || options["apiKey"] != "{env:CORP_LLM_KEY}" {
		t.Fatalf("provider entry: %+v", entry)
	}
	models := entry["models"].(map[string]any)
	fast := models["fast"].(map[string]any)
	limit := fast["limit"].(map[string]any)
	if len(models) != 2 || fast["name"] != "Fast" || limit["context"] != float64(128000) || limit["output"] != float64(8192) {
		t.Fatalf("models: %+v", models)
	}
	if !strings.Contains(raw, "[-m][corp-llm/fast]") {
		t.Fatalf("the turn does not run the provider's model:\n%s", raw)
	}
	// The harness finds the key where it looks for it, and the value is nowhere else.
	if !strings.Contains(raw, "CORP_LLM_KEY="+providerSecret+"\n") {
		t.Fatalf("the key variable did not reach the child:\n%s", raw)
	}
	if strings.Count(raw, providerSecret) != 1 {
		t.Fatalf("the key's value appears outside its variable:\n%s", raw)
	}
	if strings.Contains(raw, "other-key-must-not-reach-the-child") {
		t.Fatalf("a variable nobody named reached the child:\n%s", raw)
	}
	// A turn with no providers is unchanged: no provider section.
	plain := spec(t, checkout)
	if _, err := runProviderTurn(t, &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}, plain); err != nil {
		t.Fatal(err)
	}
	if config, _ := recordedConfig(t, record); config["provider"] != nil {
		t.Fatalf("a turn without providers got a provider section: %+v", config["provider"])
	}
}

// The limits are written only as a pair; a provider that takes no key writes no apiKey at all.
func TestCustomProviderOptionalParts(t *testing.T) {
	root := t.TempDir()
	path, record := providerRecorder(t, root, false)
	s := spec(t, t.TempDir())
	s.Agent.Model = "local-llm/x"
	local := adapter.CustomProvider{ID: "local-llm", Name: "", NPM: adapter.CustomProviderNPM, BaseURL: "http://127.0.0.1:8080/v1", ContextTokens: 1000,
		Models: []adapter.CustomProviderModel{{ID: "x", Name: "X"}}}
	s.Providers = []adapter.CustomProvider{local}
	if _, err := runProviderTurn(t, &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}, s); err != nil {
		t.Fatal(err)
	}
	config, _ := recordedConfig(t, record)
	entry := providerEntry(t, config, "local-llm")
	options := entry["options"].(map[string]any)
	if _, ok := options["apiKey"]; ok || options["baseURL"] != "http://127.0.0.1:8080/v1" || entry["name"] != "local-llm" {
		t.Fatalf("a provider without a key: %+v", entry)
	}
	if _, ok := entry["models"].(map[string]any)["x"].(map[string]any)["limit"]; ok {
		t.Fatalf("a lone limit was written: %+v", entry)
	}
}

// What a turn on a custom provider refuses before anything starts: a key variable that is not set (naming it), one the harness uses itself,
// another npm package, an unusable id or URL.
func TestCustomProviderRefusals(t *testing.T) {
	root := t.TempDir()
	path, record := providerRecorder(t, root, false)
	for name, c := range map[string]struct {
		edit func(*adapter.CustomProvider)
		want string
	}{
		"key variable not set":  {func(p *adapter.CustomProvider) { p.KeyEnv = "UNSET_CORP_KEY" }, "UNSET_CORP_KEY is not set on this machine"},
		"protected variable":    {func(p *adapter.CustomProvider) { p.KeyEnv = "OPENCODE_CONFIG_CONTENT" }, "cannot hold a key"},
		"path variable":         {func(p *adapter.CustomProvider) { p.KeyEnv = "PATH" }, "cannot hold a key"},
		"built-in provider key": {func(p *adapter.CustomProvider) { p.KeyEnv = "OPENAI_API_KEY" }, "cannot hold a key"},
		"other npm package":     {func(p *adapter.CustomProvider) { p.NPM = "left-pad" }, "only @ai-sdk/openai-compatible is allowed"},
		"id with a slash":       {func(p *adapter.CustomProvider) { p.ID = "corp/llm" }, "is not usable"},
		"no id":                 {func(p *adapter.CustomProvider) { p.ID = "" }, "is not usable"},
		"file url":              {func(p *adapter.CustomProvider) { p.BaseURL = "file:///etc/passwd" }, "no usable base URL"},
	} {
		t.Setenv("CORP_LLM_KEY", providerSecret)
		_ = os.Remove(record)
		s := spec(t, t.TempDir())
		s.Agent.Model = "corp-llm/fast"
		p := corpProvider()
		c.edit(&p)
		s.Providers = []adapter.CustomProvider{p}
		_, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), s)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want %q", name, err, c.want)
		}
		if strings.Contains(errString(err), providerSecret) {
			t.Errorf("%s: the error carries a key: %v", name, err)
		}
		if _, statErr := os.Stat(record); statErr == nil {
			t.Errorf("%s: the harness was started", name)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// A launcher that references another variable for the key supplies it from there; the key variable itself need not be set.
func TestCustomProviderKeyFromALauncherReference(t *testing.T) {
	root := t.TempDir()
	path, record := providerRecorder(t, root, false)
	t.Setenv("HOST_HELD_KEY", providerSecret)
	s := spec(t, t.TempDir())
	s.Agent.Model = "corp-llm/fast"
	s.Providers = []adapter.CustomProvider{corpProvider()}
	s.LauncherEnvRefs = map[string]string{"CORP_LLM_KEY": "HOST_HELD_KEY"}
	if _, err := runProviderTurn(t, &Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}, s); err != nil {
		t.Fatal(err)
	}
	config, raw := recordedConfig(t, record)
	if providerEntry(t, config, "corp-llm")["options"].(map[string]any)["apiKey"] != "{env:CORP_LLM_KEY}" || !strings.Contains(raw, "CORP_LLM_KEY="+providerSecret+"\n") {
		t.Fatalf("the referenced key did not arrive under the provider's variable:\n%s", raw)
	}
	// With neither set the turn is refused, naming the variable.
	t.Setenv("HOST_HELD_KEY", "")
	if _, err := (&Adapter{Command: path, RuntimeRoot: filepath.Join(root, "runtime")}).Start(context.Background(), s); err == nil || !strings.Contains(err.Error(), "CORP_LLM_KEY is not set") {
		t.Fatalf("a reference to an unset variable: %v", err)
	}
}

// A Docker turn forwards the key by name, in the launcher's forward list, and the value is only in the launcher's environment. Where the
// owner's launcher references another variable for the key, that variable's value is the key, forwarded under the provider's name.
func TestDockerTurnOnACustomProviderForwardsTheKeyByName(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(t *testing.T, s *adapter.TurnSpec)
	}{
		"from the key variable": {func(t *testing.T, s *adapter.TurnSpec) { t.Setenv("CORP_LLM_KEY", providerSecret) }},
		"from a launcher reference": {func(t *testing.T, s *adapter.TurnSpec) {
			t.Setenv("CORP_LLM_KEY", "")
			t.Setenv("HOST_HELD_KEY", providerSecret)
			s.LauncherEnvRefs = map[string]string{"CORP_LLM_KEY": "HOST_HELD_KEY"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path, record := providerRecorder(t, root, true)
			s := dockerSpec(t, t.TempDir())
			s.Agent.Model = "corp-llm/fast"
			s.Providers = []adapter.CustomProvider{corpProvider()}
			c.setup(t, &s)
			if _, err := runProviderTurn(t, &Adapter{Command: path, Variant: "docker"}, s); err != nil {
				t.Fatal(err)
			}
			config, raw := recordedConfig(t, record)
			if providerEntry(t, config, "corp-llm")["options"].(map[string]any)["apiKey"] != "{env:CORP_LLM_KEY}" {
				t.Fatalf("provider entry: %+v", config["provider"])
			}
			var forward string
			for _, line := range strings.Split(raw, "\n") {
				if strings.HasPrefix(line, "HARNESS_DOCKER_FORWARD_ENV=") {
					forward = strings.TrimPrefix(line, "HARNESS_DOCKER_FORWARD_ENV=")
				}
			}
			found := 0
			for _, item := range strings.Split(forward, ",") {
				if item == "CORP_LLM_KEY" {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("the key variable is forwarded %d times: %q", found, forward)
			}
			if !strings.Contains(raw, "CORP_LLM_KEY="+providerSecret+"\n") {
				t.Fatalf("the launcher does not carry the key under its name:\n%s", raw)
			}
			configAt := strings.Index(raw, "OPENCODE_CONFIG_CONTENT=")
			if strings.Contains(strings.SplitN(raw, "\n", 2)[0], providerSecret) || strings.Contains(raw[configAt:], providerSecret) {
				t.Fatalf("the key's value is in the arguments or the config:\n%s", raw)
			}
		})
	}
	// Not set anywhere: refused before the container starts.
	t.Setenv("CORP_LLM_KEY", "")
	root := t.TempDir()
	path, record := providerRecorder(t, root, true)
	s := dockerSpec(t, t.TempDir())
	s.Agent.Model = "corp-llm/fast"
	s.Providers = []adapter.CustomProvider{corpProvider()}
	if _, err := (&Adapter{Command: path, Variant: "docker"}).Start(context.Background(), s); err == nil || !strings.Contains(err.Error(), "CORP_LLM_KEY is not set") {
		t.Fatalf("an unset key in Docker: %v", err)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the launcher was started for a turn that was refused")
	}
}
