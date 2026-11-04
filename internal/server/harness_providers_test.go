package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// fakeProviderMachine connects a stand-in supervisor to the fixture's machine: it answers every detect request with what answer returns
// (and records the requests), which is all the provider questions need of a supervisor.
func fakeProviderMachine(t *testing.T, f *fixture, launcherID string, supports bool, answer func(protocol.DetectRequest) protocol.Frame) (*machineConn, *[]protocol.DetectRequest) {
	t.Helper()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		supportsLaunchers: true, supportsDetect: true, supportsDetectDir: true, supportsProviders: supports,
		launcherPending: map[string]chan protocol.Frame{}, harnessPending: map[string]chan protocol.Frame{}, pathPending: map[string]chan protocol.Frame{},
		launchers: []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: launcherID, Tool: "opencode"}, ResolvedCommand: "/usr/local/bin/opencode", Available: true}}}
	requests := &[]protocol.DetectRequest{}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	go func() {
		for {
			select {
			case frame := <-machine.sendQueue:
				if frame.Type != protocol.Detect || frame.Detect == nil {
					continue
				}
				reply := answer(*frame.Detect)
				reply.V, reply.Type, reply.ReqID = protocol.Version, protocol.DetectResult, frame.ReqID
				machine.harnessMu.Lock()
				waiter := machine.launcherPending[frame.ReqID]
				*requests = append(*requests, *frame.Detect)
				machine.harnessMu.Unlock()
				if waiter != nil {
					waiter <- reply
				}
			case <-t.Context().Done():
				return
			}
		}
	}()
	return machine, requests
}

func providersFixture(t *testing.T) (*fixture, store.Launcher, string) {
	t.Helper()
	f := newFixture(t)
	f.seed()
	harness := f.harness(t, "opencode", "native")
	corp := store.HarnessProvider{ID: "corp-llm", Name: "Corp", NPM: store.HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "CORP_LLM_KEY"}
	lab := store.HarnessProvider{ID: "lab-llm", Name: "Lab", NPM: store.HarnessProviderNPM, BaseURL: "http://192.168.1.4:8080/v1", KeyEnv: "LAB_KEY"}
	open := store.HarnessProvider{ID: "open-llm", Name: "Open", NPM: store.HarnessProviderNPM, BaseURL: "http://127.0.0.1:8080/v1"}
	if err := f.db.ReplaceHarnessProvidersIfRev(t.Context(), "owner", f.machineID, harness.ID, []store.HarnessProvider{corp, lab, open}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceHarnessModels(t.Context(), "owner", f.machineID, harness.ID, []store.HarnessModel{{Slug: "corp-llm/fast", Label: "Fast", Default: true}, {Slug: "lab-llm/x", Label: "X"}}); err != nil {
		t.Fatal(err)
	}
	return f, harness, "/api/machines/" + f.machineID + "/launchers/" + harness.ID + "/providers"
}

func jsonCall(t *testing.T, f *fixture, method, url string, body any) (int, map[string]any) {
	t.Helper()
	resp := f.request(method, url, body, method != http.MethodGet)
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestProviderKeyStatusSaysOnlyWhetherAKeyIsSet(t *testing.T) {
	t.Parallel()
	f, harness, base := providersFixture(t)
	status := func() map[string]any {
		t.Helper()
		code, out := jsonCall(t, f, "GET", base+"/key-status", nil)
		if code != 200 {
			t.Fatalf("key-status: %d %v", code, out)
		}
		return out
	}
	keys := func(out map[string]any) map[string]any {
		got := map[string]any{}
		for _, item := range out["keys"].([]any) {
			entry := item.(map[string]any)
			got[entry["key_env"].(string)] = entry["set"]
		}
		return got
	}
	// Offline: every key is unknown, and only the providers that name a key are listed.
	out := status()
	if out["machine_online"] != false || out["supervisor_supported"] != false || len(keys(out)) != 2 || keys(out)["CORP_LLM_KEY"] != nil || keys(out)["LAB_KEY"] != nil {
		t.Fatalf("offline: %+v", out)
	}
	// A supervisor that predates providers is not asked.
	machine, requests := fakeProviderMachine(t, f, harness.ID, false, func(protocol.DetectRequest) protocol.Frame { return protocol.Frame{} })
	if out := status(); out["machine_online"] != true || out["supervisor_supported"] != false || keys(out)["CORP_LLM_KEY"] != nil || len(*requests) != 0 {
		t.Fatalf("old supervisor: %+v asked %d", out, len(*requests))
	}
	// A current one answers per name; a name it does not answer stays unknown.
	machine.supportsProviders = true
	answer := func(request protocol.DetectRequest) protocol.Frame {
		reply := protocol.Frame{}
		reply.Details, _ = json.Marshal(protocol.ProviderKeysAnswer{Set: map[string]bool{"CORP_LLM_KEY": true}})
		return reply
	}
	machine2, requests2 := fakeProviderMachine(t, f, harness.ID, true, answer)
	_ = machine2
	got := keys(status())
	if got["CORP_LLM_KEY"] != true || got["LAB_KEY"] != nil {
		t.Fatalf("keys: %+v", got)
	}
	if len(*requests2) != 1 || (*requests2)[0].Kind != "provider_keys" || (*requests2)[0].LauncherID != harness.ID || strings.Join((*requests2)[0].KeyNames, ",") != "CORP_LLM_KEY,LAB_KEY" {
		t.Fatalf("what the supervisor was asked: %+v", *requests2)
	}
	// Not for another family, nor another machine's harness.
	claude := f.harness(t, "claude", "native")
	if code, _ := jsonCall(t, f, "GET", "/api/machines/"+f.machineID+"/launchers/"+claude.ID+"/providers/key-status", nil); code != 400 {
		t.Fatalf("a claude harness: %d", code)
	}
	if code, _ := jsonCall(t, f, "GET", "/api/machines/mch_other/launchers/"+harness.ID+"/providers/key-status", nil); code != 404 {
		t.Fatalf("another machine: %d", code)
	}
}

func TestProviderImportChecksWhatTheSupervisorSuggests(t *testing.T) {
	t.Parallel()
	f, harness, base := providersFixture(t)
	suggest := func(provider map[string]any, inline bool) map[string]any {
		return map[string]any{"provider": provider, "inline_key": inline}
	}
	details, _ := json.Marshal(map[string]any{"source": "~/.config/opencode/opencode.json", "suggestions": []any{
		suggest(map[string]any{"id": "new-llm", "name": "", "npm": store.HarnessProviderNPM, "base_url": "https://new.example.com/v1", "key_env": "NEW_KEY", "context_tokens": 1000, "output_tokens": 100,
			"models": []any{map[string]any{"id": "m1", "name": "M one"}, map[string]any{"id": "bad id", "name": "x"}, map[string]any{"id": "m2", "name": ""}}}, false),
		suggest(map[string]any{"id": "corp-llm", "name": "Corp", "npm": store.HarnessProviderNPM, "base_url": "https://llm.example.com/v1", "key_env": "CORP_LLM_KEY", "models": []any{}}, false),
		suggest(map[string]any{"id": "plain-llm", "name": "P", "npm": store.HarnessProviderNPM, "base_url": "http://llm.example.com/v1", "models": []any{}}, true),
		suggest(map[string]any{"id": "openai", "name": "O", "npm": store.HarnessProviderNPM, "base_url": "https://o.example.com/v1", "models": []any{}}, false),
	}, "skipped": []any{map[string]any{"id": "sdk", "reason": "it uses the package @ai-sdk/anthropic"}}})
	fakeProviderMachine(t, f, harness.ID, true, func(request protocol.DetectRequest) protocol.Frame {
		if request.Kind != "provider_import" || request.LauncherID != harness.ID {
			t.Errorf("request: %+v", request)
		}
		return protocol.Frame{Details: details}
	})
	code, out := jsonCall(t, f, "POST", base+"/import", nil)
	if code != 200 || out["source"] != "~/.config/opencode/opencode.json" {
		t.Fatalf("import: %d %v", code, out)
	}
	byID := map[string]map[string]any{}
	for _, item := range out["suggestions"].([]any) {
		entry := item.(map[string]any)
		byID[entry["provider"].(map[string]any)["id"].(string)] = entry
	}
	fresh := byID["new-llm"]
	freshProvider := fresh["provider"].(map[string]any)
	models := fresh["models"].([]any)
	if fresh["problem"] != nil || fresh["already_added"] != nil || freshProvider["name"] != "new-llm" || freshProvider["key_env"] != "NEW_KEY" || len(models) != 2 ||
		models[0].(map[string]any)["slug"] != "new-llm/m1" || models[0].(map[string]any)["label"] != "M one" || models[1].(map[string]any)["label"] != "m2" {
		t.Fatalf("a good suggestion: %+v", fresh)
	}
	if byID["corp-llm"]["already_added"] != true {
		t.Fatalf("an id the harness has: %+v", byID["corp-llm"])
	}
	if problem, _ := byID["plain-llm"]["problem"].(string); !strings.Contains(problem, "plain http") || byID["plain-llm"]["inline_key"] != true {
		t.Fatalf("plain http to a public host: %+v", byID["plain-llm"])
	}
	if problem, _ := byID["openai"]["problem"].(string); !strings.Contains(problem, "OpenCode ships") {
		t.Fatalf("a built-in id: %+v", byID["openai"])
	}
	if skipped := out["skipped"].([]any); len(skipped) != 1 {
		t.Fatalf("skipped: %v", skipped)
	}
	// An old supervisor, another family and a failure each answer plainly.
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsProviders = false
	f.srv.machineMu.Unlock()
	if code, _ := jsonCall(t, f, "POST", base+"/import", nil); code != 503 {
		t.Fatalf("old supervisor: %d", code)
	}
	claude := f.harness(t, "claude", "native")
	if code, _ := jsonCall(t, f, "POST", "/api/machines/"+f.machineID+"/launchers/"+claude.ID+"/providers/import", nil); code != 400 {
		t.Fatalf("claude: %d", code)
	}
}

func TestProviderImportFailureIsReportedWithoutALeak(t *testing.T) {
	t.Parallel()
	f, harness, base := providersFixture(t)
	fakeProviderMachine(t, f, harness.ID, true, func(protocol.DetectRequest) protocol.Frame {
		return protocol.Frame{Code: "import_failed", Message: "~/.config/opencode/opencode.json is not a config OpenCode can read"}
	})
	code, out := jsonCall(t, f, "POST", base+"/import", nil)
	if code != 502 || out["error"] != "import_failed" || !strings.Contains(out["message"].(string), "not a config OpenCode can read") {
		t.Fatalf("a failed import: %d %v", code, out)
	}
}

// The OpenCode model listing is asked with the harness's providers (and their models) and the harness id, and what a provider's endpoint
// could not say is passed on.
func TestModelDetectionOfAnOpenCodeHarnessCarriesItsProviders(t *testing.T) {
	t.Parallel()
	f, harness, _ := providersFixture(t)
	problems, _ := json.Marshal(protocol.ProviderModelsAnswer{EndpointProblems: map[string]string{"lab-llm": "the endpoint did not answer"}})
	_, requests := fakeProviderMachine(t, f, harness.ID, true, func(request protocol.DetectRequest) protocol.Frame {
		return protocol.Frame{ModelCandidates: []protocol.ModelCandidate{{Slug: "ollama-cloud/gemma4:31b", Label: "Gemma"}, {Slug: "corp-llm/fast", Label: "Fast"}, {Slug: "corp-llm/big", Label: "Big"}}, Details: problems}
	})
	code, out := jsonCall(t, f, "POST", "/api/machines/"+f.machineID+"/launchers/"+harness.ID+"/models/detect", nil)
	if code != 200 {
		t.Fatalf("detect: %d %v", code, out)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests: %+v", *requests)
	}
	request := (*requests)[0]
	if request.Kind != "models" || request.Family != "opencode" || request.LauncherID != harness.ID || len(request.Providers) != 3 {
		t.Fatalf("the listing request: %+v", request)
	}
	corp := request.Providers[0]
	if corp.ID != "corp-llm" || corp.KeyEnv != "CORP_LLM_KEY" || len(corp.Models) != 1 || corp.Models[0].ID != "fast" {
		t.Fatalf("the provider carried: %+v", corp)
	}
	listed := map[string]bool{}
	for _, item := range out["candidates"].([]any) {
		entry := item.(map[string]any)
		listed[entry["slug"].(string)] = entry["already_listed"] == true
	}
	if !listed["corp-llm/fast"] || listed["corp-llm/big"] || listed["ollama-cloud/gemma4:31b"] {
		t.Fatalf("candidates: %+v", listed)
	}
	if got, _ := out["endpoint_problems"].(map[string]any); got["lab-llm"] != "the endpoint did not answer" {
		t.Fatalf("endpoint problems: %+v", out["endpoint_problems"])
	}
	// A harness of another family is asked without providers.
	if request.Family != "opencode" {
		t.Fatal("unreachable")
	}
}

// When nothing usable comes back, the endpoints that could not be asked are still the owner's clue: they are on the 502, whether the machine
// reported a failure or an empty list.
func TestModelDetectionFailureCarriesTheEndpointProblems(t *testing.T) {
	t.Parallel()
	f, harness, _ := providersFixture(t)
	problems, _ := json.Marshal(protocol.ProviderModelsAnswer{EndpointProblems: map[string]string{"lab-llm": "the endpoint answered HTTP 500"}})
	fail := true
	fakeProviderMachine(t, f, harness.ID, true, func(protocol.DetectRequest) protocol.Frame {
		if fail {
			return protocol.Frame{Code: "detect_failed", Message: "model detection in /x: opencode reports no models", Details: problems}
		}
		return protocol.Frame{Details: problems}
	})
	for _, name := range []string{"failure", "empty list"} {
		code, out := jsonCall(t, f, "POST", "/api/machines/"+f.machineID+"/launchers/"+harness.ID+"/models/detect", nil)
		if got, _ := out["endpoint_problems"].(map[string]any); code != 502 || got["lab-llm"] != "the endpoint answered HTTP 500" {
			t.Fatalf("%s: %d %v", name, code, out)
		}
		fail = false
	}
}

// `aimebu doctor --machine` asks the paired server, with the machine's own key, which key variables the providers of its OpenCode
// harnesses use, and the launcher's environment references; names only, and only for an approved machine.
func TestPairStatusListsProviderKeyVariablesForTheMachine(t *testing.T) {
	t.Parallel()
	f, harness, _ := providersFixture(t)
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE machine_launcher SET env_refs_json=?,enabled=1 WHERE id=?`, `{"CORP_LLM_KEY":"HELD_KEY"}`, harness.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	get := func(key string) (int, map[string]any) {
		req, _ := http.NewRequest("GET", f.http.URL+"/api/machines/"+f.machineID+"/pair-status", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := get(f.machineKey)
	if code != 200 || out["approved"] != true {
		t.Fatalf("pair-status: %d %v", code, out)
	}
	keys, _ := out["provider_keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("provider keys: %v", out["provider_keys"])
	}
	first := keys[0].(map[string]any)
	refs, _ := first["env_refs"].(map[string]any)
	if first["provider_id"] != "corp-llm" || first["key_env"] != "CORP_LLM_KEY" || first["launcher"] == "" || refs["CORP_LLM_KEY"] != "HELD_KEY" {
		t.Fatalf("first key reference: %+v", first)
	}
	if code, _ := get("amk_wrong"); code != 401 {
		t.Fatalf("a wrong key: %d", code)
	}
}
