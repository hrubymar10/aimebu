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
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/testenv"
)

func TestSyncHarnessModelsKeepsManualAndReassignsDefault(t *testing.T) {
	t.Parallel()
	listed := []store.HarnessModel{
		{Slug: "old", Label: "Old", Default: true, Source: "detected"},
		{Slug: "manual", Label: "Manual", Source: "manual"},
		{Slug: "still", Label: "Custom label", Source: "detected"},
	}
	candidates := []detectedModel{
		{ModelCandidate: protocol.ModelCandidate{Slug: "still", Label: "Harness label"}},
		{ModelCandidate: protocol.ModelCandidate{Slug: "new", Label: "New"}},
	}
	got, added, removed, skipped := syncHarnessModels(listed, candidates, nil)
	if added != 1 || removed != 1 || skipped != 0 || len(got) != 3 || got[0].Slug != "manual" || !got[0].Default || got[1].Label != "Custom label" || got[2].Source != "detected" {
		t.Fatalf("sync: %+v, added=%d removed=%d skipped=%d", got, added, removed, skipped)
	}
	kept, _, removed, _ := syncHarnessModels(
		[]store.HarnessModel{{Slug: "corp/old", Label: "Old", Source: "detected", Default: true}},
		[]detectedModel{{ModelCandidate: protocol.ModelCandidate{Slug: "other/new", Label: "New"}}},
		map[string]string{"corp": "endpoint unavailable"},
	)
	if removed != 0 || len(kept) != 2 || kept[0].Slug != "corp/old" {
		t.Fatalf("failed endpoint must not erase previously detected models: %+v, removed=%d", kept, removed)
	}
}

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

func connectSupervisor(t *testing.T, f *fixture) string {
	t.Helper()
	journal, err := supervisor.OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	client := supervisor.NewClient(f.machineID, f.machineKey, f.http.URL, journal, adapter.NewFake())
	client.ConfigDir = t.TempDir()
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
	return client.ConfigDir
}

// The owner finds a harness on the machine, adds it, finds its models, and saves the ones they tick; none of the finding saves anything.
func TestOwnerDetectsAndAddsAHarnessAndItsModels(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) echo \"pi 0.9.3\" ;;\n  --offline) printf 'provider model context max-out thinking images\\nollama-cloud gemma4:31b 256K 32K no no\\nollama-cloud qwen3:32b 128K 16K yes no\\n' ;;\nesac\n"
	piPath := filepath.Join(bin, "pi")
	if err := testenv.WriteExecutable(piPath, []byte(script), 0o755); err != nil {
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
	// One sync request adds a newly offered model without touching the manual one.
	status, synced := post(base+"/"+harnessID+"/models/sync", map[string]any{})
	if status != 200 || synced["added"] != float64(1) || synced["removed"] != float64(0) {
		t.Fatalf("sync: %d %v", status, synced)
	}
	available, err := f.db.HarnessModels(t.Context(), harnessID)
	if err != nil || len(available) != 2 || available[0].Source != "manual" || available[1].Source != "detected" {
		t.Fatalf("saved sync models: %+v %v", available, err)
	}
	// When the harness stops offering one detected model, the next sync removes it.
	if err := testenv.WriteExecutable(piPath, []byte("#!/bin/sh\ncase \"$1\" in\n --version) echo 'pi 0.9.3' ;;\n --offline) printf 'provider model context max-out thinking images\\nollama-cloud qwen3:32b 128K 16K yes no\\n' ;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	status, synced = post(base+"/"+harnessID+"/models/sync", map[string]any{})
	if status != 200 || synced["added"] != float64(0) || synced["removed"] != float64(1) {
		t.Fatalf("second sync: %d %v", status, synced)
	}
	available, err = f.db.HarnessModels(t.Context(), harnessID)
	if err != nil || len(available) != 1 || available[0].Slug != "ollama-cloud/qwen3:32b" {
		t.Fatalf("manual model survives sync: %+v %v", available, err)
	}
	// Claude's list comes from the SDK source; offline it is the built-in snapshot, for the Docker wrapper too.
	for _, name := range []string{"native", "harness-docker"} {
		claude := f.harness(t, "claude", name)
		if status, models = post(base+"/"+claude.ID+"/models/detect", nil); status != 200 || models["source"] != "snapshot" || len(models["candidates"].([]any)) != len(claudeModelSnapshot) {
			t.Fatalf("claude %s models: %d %v", name, status, models)
		}
	}
	// Nobody but the owner finds anything.
	otherCookie, otherCSRF := registerSecondUser(t, f, "second")
	resp = f.requestAs(t, otherCookie, otherCSRF, http.MethodPost, base+"/detect", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("non-owner detect: %d", resp.StatusCode)
	}
}

// A containerized harness is asked for its models like a native one, with its own arguments first; a folder the person chose is passed
// through to the supervisor, and the harness's own complaint reaches the person when it refuses.
func TestContainerizedHarnessModelsAreDetectedAndAChosenFolderIsPassed(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", bin)
	chosen, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refused, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	script := "#!/bin/sh\n" +
		"if [ \"$(pwd -P)\" = \"" + refused + "\" ]; then echo \"cannot answer from '$(pwd -P)'\" >&2; exit 1; fi\n" +
		"printf '%s\\n%s\\n' \"$(pwd -P)\" \"$*\" > " + seen + "\n" +
		"echo '{\"models\":[{\"slug\":\"gpt-6-astra\",\"display_name\":\"GPT-6-Astra\",\"visibility\":\"list\"}]}'\n"
	if err := testenv.WriteExecutable(filepath.Join(bin, "codex-docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	docker := f.harness(t, "codex", "harness-docker")
	docker.Args = []string{"--profile", "work"}
	if _, err := f.db.SaveLauncher(t.Context(), "test", docker); err != nil {
		t.Fatal(err)
	}
	configRoot := connectSupervisor(t, f)
	// OnReady follows the supervisor's send, but the server can still be
	// processing the launcher status frame when the first request arrives.
	waitForGuard(t, machineWaitGuard, func() bool {
		conn, err := f.srv.machineForID(f.machineID)
		if err != nil {
			return false
		}
		conn.harnessMu.Lock()
		defer conn.harnessMu.Unlock()
		for _, row := range conn.launchers {
			if row.ID == docker.ID && row.ResolvedCommand != "" {
				return true
			}
		}
		return false
	})
	post := func(body any) (int, map[string]any) {
		t.Helper()
		resp := f.request(http.MethodPost, "/api/machines/"+f.machineID+"/launchers/"+docker.ID+"/models/detect", body, true)
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	ran := func() []string {
		t.Helper()
		out, err := os.ReadFile(seen)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	status, out := post(nil)
	if status != 200 || out["source"] != "harness" || len(out["candidates"].([]any)) != 1 || out["candidates"].([]any)[0].(map[string]any)["slug"] != "gpt-6-astra" {
		t.Fatalf("a containerized harness is detected: %d %v", status, out)
	}
	if got := ran(); !strings.HasPrefix(got[0], filepath.Join(configRoot, "detect")+string(os.PathSeparator)+"run-") || got[1] != "--profile work debug models --bundled" {
		t.Fatalf("the default folder and launcher's arguments: %q", got)
	}
	if _, err := os.Stat(ran()[0]); !os.IsNotExist(err) {
		t.Fatalf("the default run folder was not removed: %v", err)
	}
	if status, out = post(map[string]string{"dir": chosen}); status != 200 || ran()[0] != chosen {
		t.Fatalf("the chosen folder is used: %d %v %q", status, out, ran())
	}
	status, out = post(map[string]string{"dir": refused})
	if message, _ := out["message"].(string); status != 502 || out["error"] != "detect_failed" || out["detect_dir"] != refused || !strings.Contains(message, "cannot answer from '"+refused+"'. Add the model ID manually.") {
		t.Fatalf("the harness's complaint reaches the person: %d %v", status, out)
	}
	if status, out = post(map[string]string{"dir": "relative/dir"}); status != 400 || out["error"] != "invalid_dir" {
		t.Fatalf("a relative folder: %d %v", status, out)
	}
	if status, out = post(map[string]string{"dir": filepath.Join(chosen, "missing")}); status != 400 || out["error"] != "invalid_dir" || !strings.Contains(out["message"].(string), "does not exist") {
		t.Fatalf("a missing folder: %d %v", status, out)
	}
	// A supervisor too old to run in a chosen folder is not asked to (it would silently ignore it); without a folder it still is.
	conn, err := f.srv.machineForID(f.machineID)
	if err != nil {
		t.Fatal(err)
	}
	conn.supportsDetectDir = false
	if status, out = post(map[string]string{"dir": chosen}); status != 503 || out["error"] != "machine_unavailable" {
		t.Fatalf("an old supervisor with a folder: %d %v", status, out)
	}
	if status, out = post(nil); status != 200 {
		t.Fatalf("an old supervisor without a folder: %d %v", status, out)
	}
}
