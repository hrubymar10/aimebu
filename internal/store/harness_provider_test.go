package store

import (
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

func goodProvider(id string) HarnessProvider {
	return HarnessProvider{ID: id, Name: "Provider " + id, NPM: HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "LLM_API_KEY"}
}

func TestValidateHarnessProviders(t *testing.T) {
	t.Parallel()
	if err := ValidateHarnessProviders("opencode", nil); err != nil {
		t.Fatalf("an empty list is allowed: %v", err)
	}
	if err := ValidateHarnessProviders("claude", nil); err != nil {
		t.Fatalf("an empty list is allowed for any harness: %v", err)
	}
	if err := ValidateHarnessProviders("opencode", []HarnessProvider{goodProvider("corp-llm"), {ID: "local", Name: "Local", NPM: HarnessProviderNPM, BaseURL: "http://192.168.1.20:8080/v1",
		ContextTokens: 128000, OutputTokens: 8192}}); err != nil {
		t.Fatalf("valid list: %v", err)
	}
	with := func(edit func(*HarnessProvider)) []HarnessProvider {
		p := goodProvider("corp-llm")
		edit(&p)
		return []HarnessProvider{p}
	}
	for name, c := range map[string]struct {
		family    string
		providers []HarnessProvider
	}{
		"not opencode":           {"claude", []HarnessProvider{goodProvider("corp-llm")}},
		"uppercase id":           {"opencode", with(func(p *HarnessProvider) { p.ID = "Corp" })},
		"id with slash":          {"opencode", with(func(p *HarnessProvider) { p.ID = "corp/llm" })},
		"id starting with digit": {"opencode", with(func(p *HarnessProvider) { p.ID = "1corp" })},
		"id too long":            {"opencode", with(func(p *HarnessProvider) { p.ID = "a" + strings.Repeat("b", 40) })},
		"built-in id":            {"opencode", with(func(p *HarnessProvider) { p.ID = "openai" })},
		"duplicate id":           {"opencode", []HarnessProvider{goodProvider("corp-llm"), goodProvider("corp-llm")}},
		"no name":                {"opencode", with(func(p *HarnessProvider) { p.Name = "" })},
		"name too long":          {"opencode", with(func(p *HarnessProvider) { p.Name = strings.Repeat("界", 81) })},
		"other npm package":      {"opencode", with(func(p *HarnessProvider) { p.NPM = "@ai-sdk/openai" })},
		"no npm package":         {"opencode", with(func(p *HarnessProvider) { p.NPM = "" })},
		"no url":                 {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "" })},
		"ftp url":                {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "ftp://llm.example.com/v1" })},
		"url with credentials":   {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "https://user:pass@llm.example.com/v1" })},
		"url with query":         {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "https://llm.example.com/v1?key=1" })},
		"url with fragment":      {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "https://llm.example.com/v1#x" })},
		"url without host":       {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "https:///v1" })},
		"plain http, public":     {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "http://llm.example.com/v1" })},
		"plain http, public ip":  {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "http://8.8.8.8/v1" })},
		"plain http lookalike":   {"opencode", with(func(p *HarnessProvider) { p.BaseURL = "http://localhost.example.com/v1" })},
		"lowercase key variable": {"opencode", with(func(p *HarnessProvider) { p.KeyEnv = "llm_key" })},
		"key variable with dash": {"opencode", with(func(p *HarnessProvider) { p.KeyEnv = "LLM-KEY" })},
		"negative tokens":        {"opencode", with(func(p *HarnessProvider) { p.ContextTokens = -1 })},
		"only a context limit":   {"opencode", with(func(p *HarnessProvider) { p.ContextTokens = 1000 })},
		"only an output limit":   {"opencode", with(func(p *HarnessProvider) { p.OutputTokens = 1000 })},
		"too many tokens":        {"opencode", with(func(p *HarnessProvider) { p.ContextTokens, p.OutputTokens = 1, harnessProviderTokensMax+1 })},
	} {
		if err := ValidateHarnessProviders(c.family, c.providers); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "XDG_CONFIG_HOME", "LC_ALL", "NODE_OPTIONS", "HTTPS_PROXY", "SSL_CERT_FILE",
		"HARNESS_DOCKER_FORWARD_ENV", "OPENCODE_CONFIG_CONTENT", "OPENCODE_DB", "AIMEBU_CONFIG_DIR", "AIMEBU_X", "DOCKER_HOST", "NPM_CONFIG_REGISTRY",
		// The keys of OpenCode's own providers: the host's value of one would be sent to the custom provider's base URL.
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "GEMINI_API_KEY", "XAI_API_KEY"} {
		if err := ValidateHarnessProviders("opencode", with(func(p *HarnessProvider) { p.KeyEnv = name })); err == nil {
			t.Errorf("key variable %s must be refused", name)
		}
	}
	for _, host := range []string{"http://localhost:11434/v1", "http://127.0.0.1:8080/v1", "http://[::1]:8080/v1", "http://host.docker.internal:8080/v1", "http://10.0.0.5/v1",
		"http://172.16.4.2/v1", "http://192.168.0.9/v1", "http://[fd00::1]/v1", "http://llm.localhost/v1"} {
		if err := ValidateHarnessProviders("opencode", with(func(p *HarnessProvider) { p.BaseURL = host })); err != nil {
			t.Errorf("plain http to %s is allowed: %v", host, err)
		}
	}
	// A server that takes no key leaves the variable empty.
	if err := ValidateHarnessProviders("opencode", with(func(p *HarnessProvider) { p.KeyEnv = "" })); err != nil {
		t.Errorf("no key variable: %v", err)
	}
	many := make([]HarnessProvider, harnessProvidersMax+1)
	for i := range many {
		many[i] = goodProvider("p" + strings.Repeat("a", i/26) + string(rune('a'+i%26)))
	}
	if err := ValidateHarnessProviders("opencode", many); err == nil {
		t.Error("too many providers")
	}
}

func TestHarnessProvidersRevChangesWithEveryField(t *testing.T) {
	t.Parallel()
	base := goodProvider("corp-llm")
	rev := HarnessProvidersRev([]HarnessProvider{base})
	for name, edit := range map[string]func(*HarnessProvider){
		"id": func(p *HarnessProvider) { p.ID = "corp-two" }, "name": func(p *HarnessProvider) { p.Name = "x" }, "url": func(p *HarnessProvider) { p.BaseURL += "/x" },
		"key":     func(p *HarnessProvider) { p.KeyEnv = "OTHER_KEY" },
		"context": func(p *HarnessProvider) { p.ContextTokens = 1 }, "output": func(p *HarnessProvider) { p.OutputTokens = 1 },
	} {
		changed := base
		edit(&changed)
		if HarnessProvidersRev([]HarnessProvider{changed}) == rev {
			t.Errorf("revision ignores the %s", name)
		}
	}
	if HarnessProvidersRev(nil) == rev {
		t.Error("an empty list has its own revision")
	}
}

func TestReplaceHarnessProviders(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	ctx := t.Context()
	machine := agent.MachineID
	harness, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: machine, Tool: "opencode", Name: "native", Label: "OpenCode", Command: "opencode", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: machine, Tool: "codex", Name: "native", Label: "Codex", Command: "codex", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	two := []HarnessProvider{goodProvider("corp-llm"), goodProvider("lab-llm")}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, two, "", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.HarnessProviders(ctx, harness.ID)
	if err != nil || len(got) != 2 || got[0] != two[0] || got[1] != two[1] {
		t.Fatalf("providers: %+v %v", got, err)
	}
	all, err := s.MachineHarnessProviders(ctx, machine)
	if err != nil || len(all[harness.ID]) != 2 || len(all[other.ID]) != 0 {
		t.Fatalf("machine providers: %+v %v", all, err)
	}
	// A bad list is refused whole and changes nothing; so is one for another family or another machine's harness.
	bad := []HarnessProvider{goodProvider("corp-llm"), {ID: "bad", Name: "Bad", NPM: "x", BaseURL: "https://x.example.com"}}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, bad, "", false); !errors.Is(err, ErrInvalidHarnessProviders) {
		t.Fatalf("bad npm: %v", err)
	}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, other.ID, two, "", false); !errors.Is(err, ErrInvalidHarnessProviders) {
		t.Fatalf("a codex harness takes no providers: %v", err)
	}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", "mch_other", harness.ID, nil, "", false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("wrong machine: %v", err)
	}
	if again, _ := s.HarnessProviders(ctx, harness.ID); len(again) != 2 {
		t.Fatalf("a refused list must not change the stored one: %+v", again)
	}
	// An edit based on an old revision is refused; on the current one it goes through.
	rev := HarnessProvidersRev(two)
	changed := []HarnessProvider{goodProvider("corp-llm"), goodProvider("lab-llm")}
	changed[0].BaseURL = "https://llm2.example.com/v1"
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, changed, rev, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, two, rev, false); !errors.Is(err, ErrStaleHarnessProviders) {
		t.Fatalf("stale: %v", err)
	}
	// Models of a provider keep it from being removed, or renamed (which removes the old id); removing it with its models works.
	if err := s.ReplaceHarnessModels(ctx, "owner", machine, harness.ID, []HarnessModel{
		{Slug: "corp-llm/fast", Label: "Fast", Default: true}, {Slug: "corp-llm/big", Label: "Big"}, {Slug: "lab-llm/x", Label: "X"}, {Slug: "ollama-cloud/gemma4:31b", Label: "Gemma"}}); err != nil {
		t.Fatal(err)
	}
	var inUse *HarnessProviderInUseError
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, changed[1:], "", false); !errors.As(err, &inUse) || inUse.ProviderID != "corp-llm" || inUse.Models != 2 {
		t.Fatalf("a provider in use: %v", err)
	}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, nil, "", false); !errors.As(err, &inUse) || len(inUse.All) != 2 || inUse.All[0].ProviderID != "corp-llm" || inUse.All[1].ProviderID != "lab-llm" || inUse.All[1].Models != 1 {
		t.Fatalf("dropping both reports both: %v %+v", err, inUse)
	}
	renamed := []HarnessProvider{goodProvider("corp-two"), changed[1]}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, renamed, "", false); !errors.As(err, &inUse) {
		t.Fatalf("a rename drops the old id: %v", err)
	}
	if now, _ := s.HarnessProviders(ctx, harness.ID); len(now) != 2 || now[0].ID != "corp-llm" {
		t.Fatalf("a refused removal must change nothing: %+v", now)
	}
	if models, _ := s.HarnessModels(ctx, harness.ID); len(models) != 4 {
		t.Fatalf("a refused removal must keep the models: %+v", models)
	}
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, changed[1:], "", true); err != nil {
		t.Fatal(err)
	}
	left, _ := s.HarnessModels(ctx, harness.ID)
	if len(left) != 2 || left[0].Slug != "lab-llm/x" || left[1].Slug != "ollama-cloud/gemma4:31b" {
		t.Fatalf("models after removing corp-llm with its models: %+v", left)
	}
	// The default went with its provider, so another model took it over: a non-empty list keeps exactly one default.
	if err := ValidateHarnessModels("opencode", left); err != nil || !left[0].Default {
		t.Fatalf("default after removal: %+v %v", left, err)
	}
	// Models of other providers are untouched by a removal without any (a provider nobody uses goes freely), and an empty list clears all.
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, nil, "", true); err != nil {
		t.Fatal(err)
	}
	if models, _ := s.HarnessModels(ctx, harness.ID); len(models) != 1 || models[0].Slug != "ollama-cloud/gemma4:31b" || !models[0].Default {
		t.Fatalf("models after clearing every provider: %+v", models)
	}
	// Both the provider and the model changes are in the launcher's activity.
	var actions []string
	rows, err := s.db.Query(`SELECT action FROM machine_launcher_activity WHERE launcher_id=? ORDER BY created_at,rowid`, harness.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if joined := strings.Join(actions, ","); !strings.Contains(joined, "providers") || !strings.Contains(joined, "models") {
		t.Fatalf("activity: %s", joined)
	}
	// Deleting the harness takes its providers with it.
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", machine, harness.ID, two, "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLauncher(ctx, machine, harness.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM harness_provider WHERE launcher_id=?`, harness.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("providers left after delete: %d %v", count, err)
	}
}

// A turn of an OpenCode agent carries the one custom provider its model id names, with that provider's models (ids without the prefix),
// and nothing otherwise: not for a model of a provider OpenCode ships, not for another harness. The same facts say whether the agent's
// turns need a supervisor that knows providers.
func TestDeliveryCarriesTheProviderItsModelNames(t *testing.T) {
	t.Parallel()
	s, _, room, one, _ := wakeFixture(t)
	ctx := t.Context()
	harness, err := s.SaveLauncher(ctx, "owner", Launcher{MachineID: one.MachineID, Tool: "opencode", Name: "native", Label: "OpenCode", Command: "opencode", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	corp, lab := goodProvider("corp-llm"), goodProvider("lab-llm")
	corp.ContextTokens, corp.OutputTokens = 128000, 8192
	if err := s.ReplaceHarnessProvidersIfRev(ctx, "owner", one.MachineID, harness.ID, []HarnessProvider{corp, lab}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceHarnessModels(ctx, "owner", one.MachineID, harness.ID, []HarnessModel{{Slug: "corp-llm/fast", Label: "Fast", Default: true},
		{Slug: "corp-llm/big:1", Label: "Big"}, {Slug: "lab-llm/x", Label: "X"}, {Slug: "ollama-cloud/gemma4:31b", Label: "Gemma"}, {Slug: "other-llm/x", Label: "Other"}}); err != nil {
		t.Fatal(err)
	}
	setAgent := func(tool, launcher, model string) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE agent SET tool=?,variant='native',launcher_id=NULLIF(?,''),model=? WHERE id=?`, tool, launcher, model, one.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "go"}); err != nil {
		t.Fatal(err)
	}
	next := func() (providersOut []string, ready bool, document []byte) {
		t.Helper()
		delivery, ready, err := s.PrepareDispatch(ctx, one.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if ready {
			if err := s.CancelPreparedDispatch(ctx, delivery.TurnID); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range delivery.Providers {
			providersOut = append(providersOut, p.ID)
		}
		document, _ = json.Marshal(delivery.Providers)
		return providersOut, ready, document
	}
	setAgent("opencode", harness.ID, "corp-llm/fast")
	if pending, err := s.AgentProviderPending(ctx, one.ID); err != nil || !pending {
		t.Fatalf("a model of a custom provider: pending=%v %v", pending, err)
	}
	ids, ready, document := next()
	if !ready || len(ids) != 1 || ids[0] != "corp-llm" {
		t.Fatalf("providers of the turn: %v ready=%v", ids, ready)
	}
	var got []adapter.CustomProvider
	if err := json.Unmarshal(document, &got); err != nil {
		t.Fatal(err)
	}
	want := adapter.CustomProvider{ID: "corp-llm", Name: "Provider corp-llm", NPM: HarnessProviderNPM, BaseURL: "https://llm.example.com/v1", KeyEnv: "LLM_API_KEY",
		ContextTokens: 128000, OutputTokens: 8192, Models: []adapter.CustomProviderModel{{ID: "fast", Name: "Fast"}, {ID: "big:1", Name: "Big"}}}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("the provider document:\n got %+v\nwant %+v", got, want)
	}
	// Nothing in it can be a key: the document is names, URLs and limits.
	if strings.Contains(string(document), "sk-") || strings.Contains(strings.ToLower(string(document)), "secret") {
		t.Fatalf("a key-looking value in the document: %s", document)
	}
	for name, c := range map[string]struct{ tool, launcher, model string }{
		"a provider OpenCode ships": {"opencode", harness.ID, "ollama-cloud/gemma4:31b"},
		"a provider not configured": {"opencode", harness.ID, "other-llm/x"},
		"no harness":                {"opencode", "", "corp-llm/fast"},
		"another tool":              {"codex", harness.ID, "corp-llm/fast"},
	} {
		setAgent(c.tool, c.launcher, c.model)
		if pending, err := s.AgentProviderPending(ctx, one.ID); err != nil || pending {
			t.Errorf("%s: pending=%v %v", name, pending, err)
		}
		if ids, ready, _ := next(); !ready || len(ids) != 0 {
			t.Errorf("%s: providers %v ready=%v", name, ids, ready)
		}
	}
	// A model the harness no longer lists starts with the harness default, so that is the model whose provider counts: here a custom one.
	setAgent("opencode", harness.ID, "removed-llm/gone")
	if pending, err := s.AgentProviderPending(ctx, one.ID); err != nil || !pending {
		t.Errorf("an unlisted model falls back to the default, a custom provider's: pending=%v %v", pending, err)
	}
	if ids, ready, _ := next(); !ready || len(ids) != 1 || ids[0] != "corp-llm" {
		t.Errorf("an unlisted model: providers %v ready=%v", ids, ready)
	}
	if pending, err := s.AgentProviderPending(ctx, "agt_missing"); err != nil || pending {
		t.Errorf("an unknown agent: %v %v", pending, err)
	}
}
