package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func TestStripJSONC(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ in, want string }{
		"plain":                  {`{"a":1}`, `{"a":1}`},
		"line comment":           {"{\"a\":1, // note\n\"b\":2}", "{\"a\":1, \n\"b\":2}"},
		"block comment":          {`{"a":/* x */1}`, `{"a": 1}`},
		"comment markers in str": {`{"url":"https://x.example/a//b","c":"/* not */"}`, `{"url":"https://x.example/a//b","c":"/* not */"}`},
		"escaped quote":          {`{"a":"say \"//\" now"}`, `{"a":"say \"//\" now"}`},
		"trailing comma object":  {"{\"a\":1,\n}", "{\"a\":1\n}"},
		"trailing comma array":   {`[1,2,]`, `[1,2]`},
		"trailing comma comment": {"[1,2, // end\n]", "[1,2 \n]"},
		"inner commas stay":      {`{"a":[1,2],"b":3}`, `{"a":[1,2],"b":3}`},
	} {
		got := string(stripJSONC([]byte(c.in)))
		if got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
		var any any
		if err := json.Unmarshal([]byte(got), &any); err != nil {
			t.Errorf("%s: the result is not JSON: %v (%q)", name, err, got)
		}
	}
}

const importFixture = `{
  // the person's own config
  "theme": "dark",
  "provider": {
    "corp-llm": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Corp LLM",
      "options": {"baseURL": "https://llm.example.com/v1", "apiKey": "{env:CORP_LLM_KEY}"},
      "models": {
        "fast": {"name": "Fast", "limit": {"context": 128000, "output": 8192}},
        "big": {"limit": {"context": 128000, "output": 8192}},
      },
    },
    "inline-llm": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "http://127.0.0.1:8080/v1", "apiKey": "sk-SECRETLITERAL-must-never-be-copied"},
      "models": {"x": {"limit": {"context": 1000, "output": 100}}, "y": {}}
    },
    "file-llm": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "https://f.example.com/v1", "apiKey": "{file:~/.secrets/key}"},
      "models": {"m": {}}
    },
    "other-sdk": {"npm": "@ai-sdk/anthropic", "options": {"baseURL": "https://a.example.com"}},
    "no-npm": {"options": {"baseURL": "https://b.example.com"}},
    "no-url": {"npm": "@ai-sdk/openai-compatible", "options": {}}
  }
}`

func TestImportReadsProvidersAndNeverCopiesAKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.jsonc")
	if err := os.WriteFile(path, []byte(importFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	answer, err := importOpencodeProviders([]string{filepath.Join(dir, "missing.json"), path})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Source != path {
		t.Fatalf("source %q", answer.Source)
	}
	byID := map[string]protocol.ProviderSuggestion{}
	for _, s := range answer.Suggestions {
		byID[s.Provider.ID] = s
	}
	if len(byID) != 3 {
		t.Fatalf("suggestions: %+v", answer.Suggestions)
	}
	corp := byID["corp-llm"]
	if corp.Provider.KeyEnv != "CORP_LLM_KEY" || corp.InlineKey || corp.Provider.Name != "Corp LLM" || corp.Provider.BaseURL != "https://llm.example.com/v1" ||
		corp.Provider.ContextTokens != 128000 || corp.Provider.OutputTokens != 8192 || len(corp.Provider.Models) != 2 ||
		corp.Provider.Models[0] != (adapter.CustomProviderModel{ID: "big", Name: "big"}) || corp.Provider.Models[1] != (adapter.CustomProviderModel{ID: "fast", Name: "Fast"}) {
		t.Fatalf("corp-llm: %+v", corp)
	}
	inline := byID["inline-llm"]
	if inline.Provider.KeyEnv != "" || !inline.InlineKey || inline.Provider.ContextTokens != 0 || len(inline.Provider.Models) != 2 {
		t.Fatalf("a key written into the config is only flagged, and models with different limits set none: %+v", inline)
	}
	if file := byID["file-llm"]; file.Provider.KeyEnv != "" || !file.InlineKey {
		t.Fatalf("a file reference is not a variable: %+v", file)
	}
	skipped := map[string]string{}
	for _, s := range answer.Skipped {
		skipped[s.ID] = s.Reason
	}
	if len(skipped) != 3 || !strings.Contains(skipped["other-sdk"], "@ai-sdk/anthropic") || skipped["no-npm"] == "" || skipped["no-url"] != "it has no base URL" {
		t.Fatalf("skipped: %+v", answer.Skipped)
	}
	// Nothing of any key's text is in the answer.
	raw, _ := json.Marshal(answer)
	for _, leaked := range []string{"SECRETLITERAL", ".secrets"} {
		if strings.Contains(string(raw), leaked) {
			t.Fatalf("the answer carries key text %q: %s", leaked, raw)
		}
	}
}

func TestImportWithoutAConfigOrWithABadOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	answer, err := importOpencodeProviders([]string{filepath.Join(dir, "none.json")})
	if err != nil || len(answer.Suggestions) != 0 || answer.Source != "" {
		t.Fatalf("no config: %+v %v", answer, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"provider": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importOpencodeProviders([]string{bad}); err == nil || strings.Contains(err.Error(), dir+"x") {
		t.Fatalf("a broken config: %v", err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat(" ", providerConfigMax+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importOpencodeProviders([]string{big}); err == nil {
		t.Fatal("an oversized config was read")
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if answer, err := importOpencodeProviders([]string{filepath.Join(dir, "empty.json")}); err != nil || len(answer.Suggestions) != 0 || answer.Source == "" {
		t.Fatalf("a config without providers: %+v %v", answer, err)
	}
}

func TestProviderKeysAnswerSaysOnlyWhetherAndNeverForProtectedNames(t *testing.T) {
	t.Setenv("CORP_LLM_KEY", "sk-value-must-not-leave")
	t.Setenv("EMPTY_KEY", "")
	t.Setenv("REF_SOURCE", "referenced-value-must-not-leave")
	t.Setenv("OPENCODE_DB", "x")
	c := &Client{Launchers: []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "mln_a", EnvRefs: map[string]string{"VIA_REF_KEY": "REF_SOURCE"}}}}}
	frame := c.providerKeysAnswer(protocol.DetectRequest{Kind: "provider_keys", LauncherID: "mln_a",
		KeyNames: []string{"CORP_LLM_KEY", "EMPTY_KEY", "UNSET_KEY", "VIA_REF_KEY", "OPENCODE_DB", "PATH", "lower_case"}})
	if frame.Code != "" {
		t.Fatalf("answer: %+v", frame)
	}
	var answer protocol.ProviderKeysAnswer
	if err := json.Unmarshal(frame.Details, &answer); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"CORP_LLM_KEY": true, "EMPTY_KEY": false, "UNSET_KEY": false, "VIA_REF_KEY": true}
	if len(answer.Set) != len(want) {
		t.Fatalf("answered %+v", answer.Set)
	}
	for name, set := range want {
		if got, ok := answer.Set[name]; !ok || got != set {
			t.Errorf("%s = %v (asked: %v)", name, got, ok)
		}
	}
	if strings.Contains(string(frame.Details), "must-not-leave") {
		t.Fatalf("a value is in the answer: %s", frame.Details)
	}
	many := make([]string, providerKeyNamesMax+1)
	for i := range many {
		many[i] = "KEY_X"
	}
	if frame := c.providerKeysAnswer(protocol.DetectRequest{KeyNames: many}); frame.Code != "invalid_request" {
		t.Fatalf("too many names: %+v", frame)
	}
}

func TestEndpointModelsAskTheProvidersOwnEndpoint(t *testing.T) {
	t.Setenv("CORP_LLM_KEY", "sk-endpoint-key")
	var redirectedHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the key followed a redirect to another host")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"stolen"}]}`))
	}))
	defer other.Close()
	var seenAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok/models":
			seenAuth.Store(r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fast"},{"id":"big:1"},{"id":"fast"},{"id":"bad id with spaces"},{"id":""}]}`))
		case "/denied/models":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"bad key sk-endpoint-key"}`))
		case "/broken/models":
			_, _ = w.Write([]byte(`<html>nope</html>`))
		case "/redirect/models":
			http.Redirect(w, r, other.URL+"/models", http.StatusFound)
		case "/open/models":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("a provider without a key was sent one")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"x"}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	provider := func(id, path, key string) adapter.CustomProvider {
		return adapter.CustomProvider{ID: id, Name: id, NPM: adapter.CustomProviderNPM, BaseURL: server.URL + path, KeyEnv: key}
	}
	found, problems := providerEndpointModels(context.Background(), []adapter.CustomProvider{
		provider("okp", "/ok", "CORP_LLM_KEY"), provider("denied", "/denied", "CORP_LLM_KEY"), provider("broken", "/broken", ""), provider("redirects", "/redirect", "CORP_LLM_KEY"),
		provider("open", "/open", ""), provider("nokey", "/ok", "UNSET_PROVIDER_KEY"), {ID: "evil", NPM: "left-pad", BaseURL: server.URL + "/ok"},
		provider("protected", "/ok", "OPENCODE_DB"), provider("builtin", "/ok", "OPENAI_API_KEY")}, nil)
	if auth, _ := seenAuth.Load().(string); auth != "Bearer sk-endpoint-key" {
		t.Fatalf("the endpoint saw %q", auth)
	}
	var slugs []string
	for _, model := range found {
		slugs = append(slugs, model.Slug)
	}
	if strings.Join(slugs, ",") != "okp/fast,okp/big:1,open/x" {
		t.Fatalf("candidates: %v", slugs)
	}
	for id, want := range map[string]string{"denied": "refused the key (HTTP 401)", "broken": "did not answer with a model list", "redirects": "answered HTTP 302",
		"nokey": "UNSET_PROVIDER_KEY is not set", "evil": "not usable", "protected": "not usable", "builtin": "not usable"} {
		if !strings.Contains(problems[id], want) {
			t.Errorf("problem of %s = %q, want %q", id, problems[id], want)
		}
	}
	for id, problem := range problems {
		if strings.Contains(problem, "sk-endpoint-key") {
			t.Errorf("the problem of %s carries the key: %q", id, problem)
		}
	}
	if redirectedHits.Load() != 0 {
		t.Fatal("a redirect was followed")
	}
}

func TestOpencodeListingEnvIsThePrivateConfig(t *testing.T) {
	t.Parallel()
	provider := adapter.CustomProvider{ID: "corp-llm", Name: "Corp", NPM: adapter.CustomProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_LLM_KEY",
		Models: []adapter.CustomProviderModel{{ID: "fast", Name: "Fast"}}}
	dir := t.TempDir()
	env, cleanup, err := opencodeListingEnv(dir, false, []adapter.CustomProvider{provider})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		values[name] = value
	}
	if values["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || values["OPENCODE_DISABLE_CLAUDE_CODE"] != "1" || strings.HasPrefix(values["XDG_CONFIG_HOME"], dir) {
		t.Fatalf("env: %v (a native listing's private config home belongs under the temporary folder, not the folder the owner chose %s)", values, dir)
	}
	if info, err := os.Stat(values["XDG_CONFIG_HOME"]); err != nil || !info.IsDir() {
		t.Fatalf("the private config folder: %v", err)
	}
	var config struct {
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(values["OPENCODE_CONFIG_CONTENT"]), &config); err != nil || config.Provider["corp-llm"].Options["apiKey"] != "{env:CORP_LLM_KEY}" {
		t.Fatalf("config %q: %v", values["OPENCODE_CONFIG_CONTENT"], err)
	}
	cleanup()
	if _, err := os.Stat(values["XDG_CONFIG_HOME"]); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the private config folder: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("the folder the listing ran in was written to: %v", left)
	}
	// A container is handed the document and the config home through the launcher's forward list, as a turn is; its private home is below
	// the folder the launcher runs in (which the container mounts) and is removed again.
	env, cleanup, err = opencodeListingEnv(dir, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	var home string
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "XDG_CONFIG_HOME="); ok {
			home = value
		}
	}
	if !strings.Contains(joined, "HARNESS_DOCKER_FORWARD_ENV=OPENCODE_CONFIG_CONTENT,XDG_CONFIG_HOME,") || !strings.HasPrefix(home, dir+string(filepath.Separator)) {
		t.Fatalf("docker env: %v", env)
	}
	cleanup()
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a container listing left %v in the folder it ran in", left)
	}
	if _, cleanup, err := opencodeListingEnv(dir, false, []adapter.CustomProvider{{ID: "evil", NPM: "left-pad", BaseURL: "https://x.example.com"}}); err == nil || cleanup == nil {
		t.Fatal("a provider with another package was put into the config")
	}
}

// An OpenCode model listing runs in the private config an agent turn gets (the person's own config and the project's are off, the custom
// providers are defined), and adds what each custom provider's own endpoint serves; a listing that finds nothing itself still succeeds
// when an endpoint lists models.
func TestOpencodeListingUsesThePrivateConfigAndAsksEndpoints(t *testing.T) {
	t.Setenv("CORP_LLM_KEY", "sk-listing-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer sk-listing-key" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fast"},{"id":"big"}]}`))
			return
		}
		w.WriteHeader(403)
	}))
	defer server.Close()
	root := t.TempDir()
	record := filepath.Join(root, "record")
	script := "#!/bin/sh\n{ printf 'ARGS:%s\\n' \"$*\"; for v in XDG_CONFIG_HOME OPENCODE_DISABLE_PROJECT_CONFIG OPENCODE_DISABLE_CLAUDE_CODE OPENCODE_CONFIG_CONTENT; do eval \"printf '%s=%s\\n' $v \\\"\\${$v-<unset>}\\\"\"; done; } > " + record + "\n" + "echo ollama-cloud/gemma4:31b\n"
	command := filepath.Join(root, "opencode")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider := adapter.CustomProvider{ID: "corp-llm", Name: "Corp", NPM: adapter.CustomProviderNPM, BaseURL: server.URL + "/v1", KeyEnv: "CORP_LLM_KEY",
		Models: []adapter.CustomProviderModel{{ID: "fast", Name: "Fast"}}}
	broken := adapter.CustomProvider{ID: "broken-llm", Name: "B", NPM: adapter.CustomProviderNPM, BaseURL: server.URL + "/nothing", KeyEnv: "CORP_LLM_KEY"}
	extras := &detectExtras{providers: []adapter.CustomProvider{provider, broken}}
	dir := t.TempDir()
	models, err := detectModelsWith(t.Context(), "opencode", command, nil, dir, false, extras)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, model := range models {
		slugs = append(slugs, model.Slug)
	}
	if strings.Join(slugs, ",") != "ollama-cloud/gemma4:31b,corp-llm/fast,corp-llm/big" {
		t.Fatalf("models: %v", slugs)
	}
	if !strings.Contains(extras.problems["broken-llm"], "HTTP 403") || len(extras.problems) != 1 {
		t.Fatalf("problems: %+v", extras.problems)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"ARGS:models", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", `{env:CORP_LLM_KEY}`} {
		if !strings.Contains(got, want) {
			t.Errorf("the listing's environment lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sk-listing-key\"") && strings.Contains(strings.SplitN(got, "OPENCODE_CONFIG_CONTENT=", 2)[1], "sk-listing-key") {
		t.Errorf("the key's value is in the config:\n%s", got)
	}
	// The folder the listing ran in (the owner may have chosen it) is left as it was found, and the private config home the listing used is gone.
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Fatalf("the listing wrote into %s: %v %v", dir, left, err)
	}
	var home string
	for _, line := range strings.Split(got, "\n") {
		if value, ok := strings.CutPrefix(line, "XDG_CONFIG_HOME="); ok {
			home = value
		}
	}
	if home == "" || strings.HasPrefix(home, dir) {
		t.Fatalf("the private config home %q is not a folder of its own under the temporary folder", home)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the private config home was left behind: %v", err)
	}
	// A listing with nothing of its own is still a list when an endpoint serves models.
	empty := filepath.Join(root, "opencode-empty")
	if err := os.WriteFile(empty, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if models, err := detectModelsWith(t.Context(), "opencode", empty, nil, t.TempDir(), false, &detectExtras{providers: []adapter.CustomProvider{provider}}); err != nil || len(models) != 2 {
		t.Fatalf("endpoint models only: %v %v", models, err)
	}
	if _, err := detectModelsWith(t.Context(), "opencode", empty, nil, t.TempDir(), false, nil); err == nil {
		t.Fatal("a listing with nothing is an error without providers")
	}
}

// The endpoints of a listing are asked at once under one budget: sixteen that never answer cost the budget, not sixteen waits, the ones that do
// answer still come back, and a provider that ran out of time is named with that reason.
func TestProviderEndpointsAreAskedInParallelUnderOneBudget(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/quick/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fast"}]}`))
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	previous := providerEndpointsBudget
	providerEndpointsBudget = 400 * time.Millisecond
	t.Cleanup(func() { providerEndpointsBudget = previous })
	providers := []adapter.CustomProvider{{ID: "quick", Name: "quick", NPM: adapter.CustomProviderNPM, BaseURL: server.URL + "/quick"}}
	for i := 0; i < 15; i++ {
		providers = append(providers, adapter.CustomProvider{ID: fmt.Sprintf("slow%02d", i), Name: "slow", NPM: adapter.CustomProviderNPM, BaseURL: fmt.Sprintf("%s/slow%d", server.URL, i)})
	}
	started := time.Now()
	found, problems := providerEndpointModels(context.Background(), providers, nil)
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("sixteen endpoints took %v: they must be asked at once under one budget", took)
	}
	if len(found) != 1 || found[0].Slug != "quick/fast" {
		t.Fatalf("the endpoint that answered is lost: %+v", found)
	}
	if len(problems) != 15 {
		t.Fatalf("problems: %v", problems)
	}
	for id, problem := range problems {
		if problem != "the endpoint was not asked in time" {
			t.Errorf("%s: %q", id, problem)
		}
	}
}

// OpenCode's own list being empty ("no models") says nothing about a custom provider's endpoint: it is still asked, what it lists is offered,
// and when every endpoint fails as well the failure carries the endpoint problems instead of a bare error.
func TestOpencodeListingWithNoModelsStillAsksTheCustomEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"fast"}]}`))
			return
		}
		w.WriteHeader(500)
	}))
	defer server.Close()
	root := t.TempDir()
	command := filepath.Join(root, "opencode")
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho 'error: no models available' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	custom := func(id, path string) adapter.CustomProvider {
		return adapter.CustomProvider{ID: id, Name: id, NPM: adapter.CustomProviderNPM, BaseURL: server.URL + path}
	}
	// One endpoint lists models: they are the answer.
	extras := &detectExtras{providers: []adapter.CustomProvider{custom("good", "/good"), custom("bad", "/bad")}}
	models, err := detectModelsWith(t.Context(), "opencode", command, nil, t.TempDir(), false, extras)
	if err != nil || len(models) != 1 || models[0].Slug != "good/fast" || !strings.Contains(extras.problems["bad"], "HTTP 500") {
		t.Fatalf("models=%+v err=%v problems=%v", models, err, extras.problems)
	}
	// None does: the harness's own "no models" stays the error, and the problems are kept for the answer.
	extras = &detectExtras{providers: []adapter.CustomProvider{custom("bad", "/bad"), custom("worse", "/worse")}}
	if _, err := detectModelsWith(t.Context(), "opencode", command, nil, t.TempDir(), false, extras); err == nil || !strings.Contains(err.Error(), "no models") {
		t.Fatalf("err=%v", err)
	}
	if len(extras.problems) != 2 {
		t.Fatalf("problems: %v", extras.problems)
	}
	// The answer frame carries them with the failure.
	client := &Client{ConfigDir: t.TempDir()}
	result := client.detectResult(t.Context(), protocol.Frame{ReqID: "r", Detect: &protocol.DetectRequest{Kind: "models", Family: "opencode", Command: command, LauncherID: "none",
		Providers: []adapter.CustomProvider{custom("bad", "/bad")}}})
	var asked protocol.ProviderModelsAnswer
	if result.Code != "detect_failed" || json.Unmarshal(result.Details, &asked) != nil || !strings.Contains(asked.EndpointProblems["bad"], "HTTP 500") {
		t.Fatalf("the failure frame: %+v", result)
	}
}
