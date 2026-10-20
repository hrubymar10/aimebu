package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
)

func TestParseAnthropicModelsKeepsCurrentOnes(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join("testdata", "anthropic_models_excerpt.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseAnthropicModels(source)
	var slugs []string
	for _, model := range got {
		slugs = append(slugs, model.Slug)
	}
	want := "claude-haiku-5-5 claude-sonnet-5-5 claude-fable-5-1 claude-opus-5-5 claude-sonnet-5 claude-fable-5 claude-opus-5 claude-opus-4-8 claude-opus-4-7 claude-opus-4-6 claude-sonnet-4-6 claude-haiku-4-5 claude-opus-4-5"
	if strings.Join(slugs, " ") != want {
		t.Fatalf("slugs:\n got %v\nwant %s", slugs, want)
	}
	if got[2].Label != "Claude Fable 5.1" || !strings.HasPrefix(got[2].Note, "Frontier intelligence for ambitious tasks across coding") || strings.Contains(got[2].Note, "  ") {
		t.Errorf("label and note of the third: %+v", got[2])
	}
	// The snapshot is what the SDK says today, so it must agree with the parser on this excerpt.
	if len(claudeModelSnapshot) != len(got) {
		t.Fatalf("the built-in snapshot has %d models, the SDK excerpt %d", len(claudeModelSnapshot), len(got))
	}
	for i := range got {
		if claudeModelSnapshot[i] != got[i] {
			t.Errorf("snapshot %d: %+v, parsed %+v", i, claudeModelSnapshot[i], got[i])
		}
	}
}

func TestClaudeModelsAreFetchedCachedAndFallBackToTheSnapshot(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join("testdata", "anthropic_models_excerpt.go.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil)
	var seenETags []string
	mode := "ok"
	s.claudeModelsFetch = func(ctx context.Context, etag string) ([]byte, string, bool, error) {
		seenETags = append(seenETags, etag)
		switch mode {
		case "unchanged":
			return nil, etag, true, nil
		case "down":
			return nil, "", false, errors.New("offline")
		case "empty":
			return []byte("package x"), "e2", false, nil
		}
		return source, "e1", false, nil
	}
	if models, from := s.claudeModelCandidates(t.Context()); from != "github" || len(models) != 13 {
		t.Fatalf("first: %s %d", from, len(models))
	}
	mode = "unchanged"
	if _, from := s.claudeModelCandidates(t.Context()); from != "cache" {
		t.Fatalf("unchanged: %s", from)
	}
	mode = "down"
	if models, from := s.claudeModelCandidates(t.Context()); from != "cache" || len(models) != 13 {
		t.Fatalf("down after a good fetch keeps the cache: %s %d", from, len(models))
	}
	if strings.Join(seenETags, ",") != ",e1,e1" {
		t.Errorf("conditional requests: %v", seenETags)
	}
	fresh := New(nil)
	fresh.claudeModelsFetch = s.claudeModelsFetch
	if models, from := fresh.claudeModelCandidates(t.Context()); from != "snapshot" || len(models) != len(claudeModelSnapshot) {
		t.Fatalf("down with no cache: %s %d", from, len(models))
	}
	mode = "empty"
	if _, from := fresh.claudeModelCandidates(t.Context()); from != "snapshot" {
		t.Fatalf("a source with no models falls back: %s", from)
	}
}

func connectSupervisor(t *testing.T, f *fixture) {
	t.Helper()
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	ready := make(chan struct{}, 1)
	client.OnReady = func() { ready <- struct{}{} }
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go client.RunOnce(ctx)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not connect")
	}
}

// The owner finds a harness on the machine, adds it, finds its models, and saves the ones they tick; none of the finding saves anything.
func TestOwnerDetectsAndAddsAHarnessAndItsModels(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"pi 0.9.3\" ;;\n  --offline) printf 'provider model context max-out thinking images\\nollama-cloud gemma4:31b 256K 32K no no\\nollama-cloud qwen3:32b 128K 16K yes no\\n' ;;\nesac\n"
	piPath := filepath.Join(bin, "pi")
	if err := os.WriteFile(piPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	// The fixture's own pi harnesses would be mistaken for this one; start without them.
	for _, name := range []string{"native", "harness-docker"} {
		if err := f.db.DeleteLauncher(t.Context(), f.machineID, f.harness(t, "pi", name).ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.claudeModelsFetch = func(context.Context, string) ([]byte, string, bool, error) {
		return nil, "", false, errors.New("offline")
	}
	connectSupervisor(t, f)
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		resp := f.request(http.MethodPost, path, body, true)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	base := "/api/machines/" + f.machineID + "/launchers"
	// Detection only reports.
	status, found := post(base+"/detect", nil)
	if status != 200 {
		t.Fatalf("detect: %d %v", status, found)
	}
	var pi map[string]any
	for _, item := range found["candidates"].([]any) {
		if candidate := item.(map[string]any); candidate["command"] == piPath {
			pi = candidate
		}
	}
	if pi == nil || pi["family"] != "pi" || pi["version"] != "pi 0.9.3" || pi["label"] != "Pi" || pi["already_added"] != false {
		t.Fatalf("candidate for the pi script: %v", found)
	}
	// Adding it: no name or label needed, the server names it.
	status, added := post(base, store.Launcher{Tool: "pi", Command: piPath, Enabled: true, Toolbox: true, Args: []string{}, EnvRefs: map[string]string{}, PathMaps: []store.PathMap{}, VisibleRoots: []string{}})
	if status != 200 {
		t.Fatalf("add: %d %v", status, added)
	}
	config := added["config"].(map[string]any)
	harnessID := config["id"].(string)
	if config["name"] != "native" || config["label"] != "Pi" {
		t.Fatalf("auto name and label: %v", config)
	}
	status, again := post(base+"/detect", nil)
	if status != 200 {
		t.Fatalf("second detect: %d", status)
	}
	for _, item := range again["candidates"].([]any) {
		if candidate := item.(map[string]any); candidate["command"] == piPath && candidate["already_added"] != true {
			t.Fatalf("an added harness is marked: %v", candidate)
		}
	}
	// Models: detected, nothing saved yet.
	status, models := post(base+"/"+harnessID+"/models/detect", nil)
	if status != 200 || models["source"] != "harness" {
		t.Fatalf("models detect: %d %v", status, models)
	}
	list := models["candidates"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["slug"] != "ollama-cloud/gemma4:31b" || list[0].(map[string]any)["note"] != "context 256K, max output 32K, thinking no, images no" {
		t.Fatalf("candidates: %v", list)
	}
	if saved, _ := f.db.HarnessModels(t.Context(), harnessID); len(saved) != 0 {
		t.Fatalf("detecting must not save: %+v", saved)
	}
	resp := f.request(http.MethodPut, base+"/"+harnessID+"/models", map[string]any{"models": []store.HarnessModel{{Slug: "ollama-cloud/qwen3:32b", Label: "Qwen3 32B", Note: "ticked", Default: true}}}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("save models: %d", resp.StatusCode)
	}
	status, models = post(base+"/"+harnessID+"/models/detect", nil)
	for _, item := range models["candidates"].([]any) {
		if candidate := item.(map[string]any); candidate["already_listed"] != (candidate["slug"] == "ollama-cloud/qwen3:32b") {
			t.Fatalf("already_listed after saving: %v", candidate)
		}
	}
	// Claude's list comes from the SDK source; offline it is the built-in snapshot, and a Docker wrapper cannot be asked.
	claude := f.harness(t, "claude", "native")
	if status, models = post(base+"/"+claude.ID+"/models/detect", nil); status != 200 || models["source"] != "snapshot" || len(models["candidates"].([]any)) != len(claudeModelSnapshot) {
		t.Fatalf("claude models: %d %v", status, models)
	}
	docker := f.harness(t, "codex", "harness-docker")
	if status, models = post(base+"/"+docker.ID+"/models/detect", nil); status != 400 || models["error"] != "detect_unsupported" {
		t.Fatalf("docker wrapper: %d %v", status, models)
	}
	// Nobody but the owner finds anything.
	otherCookie, otherCSRF := registerSecondUser(t, f, "second")
	resp = f.requestAs(t, otherCookie, otherCSRF, http.MethodPost, base+"/detect", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("non-owner detect: %d", resp.StatusCode)
	}
}
