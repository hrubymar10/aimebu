package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func TestMemoryStorePersistenceCapAndVersion(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.addMemory(agent.ID, types.MemoryScopeProjectFacts, "", "first fact", 0)
	if err != nil {
		t.Fatalf("addMemory: %v", err)
	}
	if rec.Version != 1 {
		t.Fatalf("initial version = %d, want 1", rec.Version)
	}
	updated, err := s.updateMemory(agent.ID, rec.ID, rec.Version, "first fact, edited")
	if err != nil {
		t.Fatalf("updateMemory: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("updated version = %d, want 2", updated.Version)
	}
	if _, err := s.updateMemory(agent.ID, rec.ID, rec.Version, "stale edit"); err == nil {
		t.Fatal("stale update should fail")
	} else {
		var conflict *memoryConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale update err = %T, want memoryConflictError", err)
		}
	}
	if _, err := s.removeMemory(agent.ID, rec.ID, rec.Version); err == nil {
		t.Fatal("stale remove should fail")
	}

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reloaded.listMemory(agent.ID, types.MemoryScopeProjectFacts, "")
	if err != nil {
		t.Fatalf("listMemory after reload: %v", err)
	}
	if len(snap.Records) != 1 || snap.Records[0].Body != "first fact, edited" || snap.Records[0].Version != 2 {
		t.Fatalf("reloaded records = %#v", snap.Records)
	}

	big := strings.Repeat("x", maxMemoryBodyRunes)
	for i := 0; i < 2; i++ {
		if _, err := reloaded.addMemory(agent.ID, types.MemoryScopeProjectFacts, "", big, 0); err != nil {
			t.Fatalf("add big %d: %v", i, err)
		}
	}
	if _, err := reloaded.addMemory(agent.ID, types.MemoryScopeProjectFacts, "", strings.Repeat("y", 200), 0); err == nil {
		t.Fatal("memory over cap should fail")
	} else {
		var capErr *memoryCapError
		if !errors.As(err, &capErr) {
			t.Fatalf("cap err = %T, want memoryCapError", err)
		}
		if capErr.Usage.Used <= capErr.Usage.Cap {
			t.Fatalf("cap usage = %+v, want over cap", capErr.Usage)
		}
	}
	if _, err := reloaded.addMemory(agent.ID, types.MemoryScopeAgentSharedNotes, "", strings.Repeat("z", maxMemoryBodyRunes+1), 0); err == nil {
		t.Fatal("overlong body should fail")
	} else {
		var bodyErr *memoryBodyTooLongError
		if !errors.As(err, &bodyErr) {
			t.Fatalf("body err = %T, want memoryBodyTooLongError", err)
		}
	}
}

func TestMemoryRegisterInjectionAndSpawnTagReclaim(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	mux := http.NewServeMux()
	setupHandlers(mux, s, BuildInfo{}, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	registerBody := map[string]any{
		"kind":    "ai",
		"model":   "gpt5",
		"harness": "codex",
		"project": "test",
		"meta":    map[string]string{"spawn_tag": "0123456789abcdef"},
	}
	first := postJSONForTest(t, srv, "/agents", registerBody)
	var firstResp types.RegisterResponse
	if err := json.Unmarshal([]byte(first), &firstResp); err != nil {
		t.Fatal(err)
	}
	if firstResp.ID == "" || firstResp.Reclaimed {
		t.Fatalf("first register = %+v", firstResp)
	}
	if _, err := s.addMemory(firstResp.ID, types.MemoryScopeProjectFacts, "", "remember this", 0); err != nil {
		t.Fatalf("add memory: %v", err)
	}

	second := postJSONForTest(t, srv, "/agents", registerBody)
	var secondResp types.RegisterResponse
	if err := json.Unmarshal([]byte(second), &secondResp); err != nil {
		t.Fatal(err)
	}
	if !secondResp.Reclaimed || secondResp.ID != firstResp.ID {
		t.Fatalf("second register = %+v, want reclaimed same id %q", secondResp, firstResp.ID)
	}
	if secondResp.Memory == nil || len(secondResp.Memory.Records) != 1 || secondResp.Memory.Records[0].Body != "remember this" {
		t.Fatalf("memory injection = %#v", secondResp.Memory)
	}
}

func TestMemoryAgentSharedNotesGlobalBucket(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	alice, _, err := s.registerAI("gpt5", "codex", "alpha", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.registerAI("gpt5", "codex", "beta", nil, "bravo")
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.addMemory(alice.ID, types.MemoryScopeAgentSharedNotes, "ignored-key", "shared across agents", 0)
	if err != nil {
		t.Fatalf("add shared memory: %v", err)
	}
	if rec.ScopeKey != agentSharedNotesMemoryKey {
		t.Fatalf("shared scope key = %q, want %q", rec.ScopeKey, agentSharedNotesMemoryKey)
	}

	snap, err := s.listMemory(bob.ID, types.MemoryScopeAgentSharedNotes, "")
	if err != nil {
		t.Fatalf("bob list shared memory: %v", err)
	}
	if len(snap.Records) != 1 || snap.Records[0].ID != rec.ID {
		t.Fatalf("bob shared snapshot = %#v", snap.Records)
	}
	if len(snap.Usage) != 1 || snap.Usage[0].Key != agentSharedNotesMemoryKey {
		t.Fatalf("shared usage = %#v", snap.Usage)
	}

	updated, err := s.updateMemory(bob.ID, rec.ID, rec.Version, "shared across projects")
	if err != nil {
		t.Fatalf("bob update shared memory: %v", err)
	}
	if _, err := s.updateMemory(alice.ID, rec.ID, rec.Version, "stale shared update"); err == nil {
		t.Fatal("stale shared update should fail")
	} else {
		var conflict *memoryConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("stale shared update err = %T, want memoryConflictError", err)
		}
	}

	injected := s.memorySnapshotForAgent(alice)
	if injected == nil || len(injected.Records) != 1 || injected.Records[0].ID != updated.ID {
		t.Fatalf("shared register snapshot = %#v", injected)
	}
}

func TestHumanProjectFactsHTTPPath(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	mux := http.NewServeMux()
	setupHandlers(mux, s, BuildInfo{}, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	postJSONForTest(t, srv, "/agents", map[string]any{
		"kind": "human",
		"name": "matin",
	})

	added := doJSONForTest(t, srv, http.MethodPost, "/memory", map[string]any{
		"agent_id":  "matin",
		"scope":     types.MemoryScopeProjectFacts,
		"scope_key": "aimebu",
		"body":      "project fact",
	})
	var addResp struct {
		Record types.MemoryRecord `json:"record"`
	}
	if err := json.Unmarshal([]byte(added), &addResp); err != nil {
		t.Fatal(err)
	}
	if addResp.Record.ScopeKey != "aimebu" || addResp.Record.Author != "matin" {
		t.Fatalf("added record = %+v", addResp.Record)
	}

	listed := getForTest(t, srv, "/memory?agent_id=matin&scope=project_facts&scope_key=aimebu")
	var snap types.MemorySnapshot
	if err := json.Unmarshal([]byte(listed), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Records) != 1 || snap.Records[0].ID != addResp.Record.ID {
		t.Fatalf("listed snapshot = %+v", snap)
	}

	updated := doJSONForTest(t, srv, http.MethodPut, "/memory/"+addResp.Record.ID, map[string]any{
		"agent_id": "matin",
		"version":  addResp.Record.Version,
		"body":     "project fact edited",
	})
	var updateResp struct {
		Record types.MemoryRecord `json:"record"`
	}
	if err := json.Unmarshal([]byte(updated), &updateResp); err != nil {
		t.Fatal(err)
	}
	if updateResp.Record.Version != 2 || updateResp.Record.Body != "project fact edited" {
		t.Fatalf("updated record = %+v", updateResp.Record)
	}

	deleted := doJSONForTest(t, srv, http.MethodDelete, "/memory/"+addResp.Record.ID+"?agent_id=matin&version=2", nil)
	var deleteResp struct {
		Record types.MemoryRecord `json:"record"`
	}
	if err := json.Unmarshal([]byte(deleted), &deleteResp); err != nil {
		t.Fatal(err)
	}
	if deleteResp.Record.ID != addResp.Record.ID {
		t.Fatalf("deleted record = %+v", deleteResp.Record)
	}
}

func TestMemoryGlobalGateAndHumanManagement(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	setupHandlers(mux, s, BuildInfo{}, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	offRegister := postJSONForTest(t, srv, "/agents", map[string]any{
		"kind":    "ai",
		"model":   "gpt5",
		"harness": "codex",
		"project": "test",
		"force":   true,
		"name":    "alice",
	})
	var offResp types.RegisterResponse
	if err := json.Unmarshal([]byte(offRegister), &offResp); err != nil {
		t.Fatal(err)
	}
	if offResp.Memory != nil {
		t.Fatalf("default-off register memory = %#v, want nil", offResp.Memory)
	}

	enableMemoryForTest(s)
	humanResp := postJSONForTest(t, srv, "/agents", map[string]any{
		"kind": "human",
		"name": "matin",
	})
	var human types.RegisterResponse
	if err := json.Unmarshal([]byte(humanResp), &human); err != nil {
		t.Fatal(err)
	}
	rec, err := s.addMemory(offResp.ID, types.MemoryScopeProjectFacts, "", "kept while disabled", 0)
	if err != nil {
		t.Fatalf("addMemory enabled: %v", err)
	}

	disableMemoryForTest(s)
	if _, err := s.listMemory(offResp.ID, "", ""); err == nil {
		t.Fatal("AI list should be disabled when global memory is off")
	} else {
		var disabled *memoryDisabledError
		if !errors.As(err, &disabled) {
			t.Fatalf("AI list err = %T, want memoryDisabledError", err)
		}
	}
	if _, err := s.addMemory(human.ID, types.MemoryScopeUserProfile, "", "new while disabled", 0); err == nil {
		t.Fatal("human add should be disabled when global memory is off")
	}
	snap, err := s.listMemory(human.ID, types.MemoryScopeProjectFacts, "test")
	if err != nil {
		t.Fatalf("human management list while disabled: %v", err)
	}
	if len(snap.Records) != 1 || snap.Records[0].ID != rec.ID {
		t.Fatalf("human management snapshot = %#v", snap.Records)
	}
	allSnap, err := s.listMemory(human.ID, "", "")
	if err != nil {
		t.Fatalf("human management list all while disabled: %v", err)
	}
	if len(allSnap.Records) != 1 || allSnap.Records[0].ID != rec.ID {
		t.Fatalf("human all snapshot = %#v", allSnap.Records)
	}
	updated, err := s.updateMemory(human.ID, rec.ID, rec.Version, "edited while disabled")
	if err != nil {
		t.Fatalf("human management update while disabled: %v", err)
	}
	if _, err := s.removeMemory(offResp.ID, rec.ID, updated.Version); err == nil {
		t.Fatal("AI remove should be disabled when global memory is off")
	}
	removed, err := s.cleanMemory(human.ID, types.MemoryScopeProjectFacts, "test")
	if err != nil {
		t.Fatalf("human clean while disabled: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != rec.ID {
		t.Fatalf("removed = %#v", removed)
	}
}

func TestMemoryRoomOverrideContentFlow(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.registerAI("gpt5", "codex", "test", nil, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("enabled", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("enabled", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("disabled", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("disabled", bob.ID); err != nil {
		t.Fatal(err)
	}
	enabledMsg, err := s.roomSend("enabled", bob.ID, "needle from enabled room", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	disabledMsg, err := s.roomSend("disabled", bob.ID, "needle from disabled room", false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := s.setRoomMemoryOverride("disabled", &off); err != nil {
		t.Fatal(err)
	}

	results, err := s.recallMessages(alice.ID, "needle", 10)
	if err != nil {
		t.Fatalf("recallMessages: %v", err)
	}
	if len(results) != 1 || results[0].RoomID != "enabled" {
		t.Fatalf("recall results = %#v", results)
	}
	if _, err := s.addMemory(alice.ID, types.MemoryScopeProjectFacts, "", "from disabled source", disabledMsg); err == nil {
		t.Fatal("add from disabled source room should fail")
	} else {
		var disabled *memoryDisabledError
		if !errors.As(err, &disabled) {
			t.Fatalf("disabled source err = %T, want memoryDisabledError", err)
		}
	}
	if _, err := s.addMemory(alice.ID, types.MemoryScopeProjectFacts, "", "from enabled source", enabledMsg); err != nil {
		t.Fatalf("add from enabled source room: %v", err)
	}
	if _, err := s.addMemory(alice.ID, types.MemoryScopeProjectFacts, "", "missing source", 999999); err == nil {
		t.Fatal("missing source id should fail")
	}
}

func TestMemoryPruneSemantics(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.addMemory(agent.ID, types.MemoryScopeProjectFacts, "", "persist across plain prune", 0); err != nil {
		t.Fatal(err)
	}
	memoryPath := filepath.Join(s.dir, "memory.json")
	if _, err := os.Stat(memoryPath); err != nil {
		t.Fatalf("memory.json before prune: %v", err)
	}
	s.clearAll(false)
	if got := memoryCountForTest(s); got != 1 {
		t.Fatalf("plain clearAll memory count = %d, want 1", got)
	}
	if _, err := os.Stat(memoryPath); err != nil {
		t.Fatalf("plain clearAll should preserve memory.json: %v", err)
	}
	s.clearAll(true)
	if got := memoryCountForTest(s); got != 0 {
		t.Fatalf("includeSettings clearAll memory count = %d, want 0", got)
	}
	if _, err := os.Stat(memoryPath); !os.IsNotExist(err) {
		t.Fatalf("includeSettings clearAll should remove memory.json, stat err=%v", err)
	}
}

func TestRecallVisibleAndReadOnly(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableMemoryForTest(s)
	alice, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.registerAI("gpt5", "codex", "test", nil, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", bob.ID, "the needle is here", false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("private", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("private", bob.ID, "secret needle", false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	s.emitSystemMessage("general", "system needle")

	before := cloneAgentForTest(t, s, alice.ID)
	results, err := s.recallMessages(alice.ID, "needle", 10)
	if err != nil {
		t.Fatalf("recallMessages: %v", err)
	}
	after := cloneAgentForTest(t, s, alice.ID)
	if len(results) != 1 || results[0].RoomID != "general" {
		t.Fatalf("recall results = %#v", results)
	}
	if len(before.ReadCursors) != len(after.ReadCursors) {
		t.Fatalf("recall changed read cursors: before=%#v after=%#v", before.ReadCursors, after.ReadCursors)
	}
}

func cloneAgentForTest(t *testing.T, s *store, id string) types.Agent {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[id]
	if !ok {
		t.Fatalf("agent %s not found", id)
	}
	return cloneAgentLocked(a)
}

func postJSONForTest(t *testing.T, srv *httptest.Server, path string, body any) string {
	t.Helper()
	return doJSONForTest(t, srv, http.MethodPost, path, body)
}

func getForTest(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	return doJSONForTest(t, srv, http.MethodGet, path, nil)
}

func doJSONForTest(t *testing.T, srv *httptest.Server, method, path string, body any) string {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusBadRequest {
		t.Fatalf("%s %s status %d: %s", method, path, resp.StatusCode, out)
	}
	return string(out)
}

func memoryCountForTest(s *store) int {
	s.memoryMu.RLock()
	defer s.memoryMu.RUnlock()
	return len(s.memory)
}

func enableMemoryForTest(s *store) {
	v := true
	s.putSettings(Settings{MemoryEnabled: &v})
}

func disableMemoryForTest(s *store) {
	v := false
	s.putSettings(Settings{MemoryEnabled: &v})
}
