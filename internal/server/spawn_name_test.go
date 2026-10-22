package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// namedSpawnFixture has the seeded project and room and a connected fake machine, so a spawn is accepted without a supervisor process.
func namedSpawnFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	return f
}

// spawnNamed asks for a reviewer (the role that does not need the writer slot) with the given extra fields.
func (f *fixture) spawnNamed(t *testing.T, key string, extra map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"role_key": "reviewer", "tool": "fake", "variant": "test", "model": "test"}
	for k, v := range extra {
		body[k] = v
	}
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) agentCount(t *testing.T) int {
	t.Helper()
	cards, err := f.db.AgentCardsForRoom(t.Context(), f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	return len(cards)
}

func (f *fixture) suggestedName(t *testing.T) string {
	t.Helper()
	resp := f.requestAs(t, f.cookie, f.csrf, http.MethodGet, "/api/rooms/"+f.room.ID+"/agent-name", nil)
	defer resp.Body.Close()
	var out struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || out.Name == "" {
		t.Fatalf("suggestion: %d %+v %v", resp.StatusCode, out, err)
	}
	return out.Name
}

func TestSpawnWithAChosenNameUsesIt(t *testing.T) {
	f := namedSpawnFixture(t)
	status, card := f.spawnNamed(t, "chosen", map[string]any{"name": "nova-one"})
	if status != 201 || card["slug"] != "nova-one" {
		t.Fatalf("spawn with a name: %d %+v", status, card)
	}
	// The same key replays the same agent, with its chosen name.
	status, again := f.spawnNamed(t, "chosen", map[string]any{"name": "nova-one"})
	if status != 201 || again["id"] != card["id"] || again["slug"] != "nova-one" {
		t.Fatalf("replay: %d %+v", status, again)
	}
	// Without a name the server still picks one from its pool.
	status, auto := f.spawnNamed(t, "auto", nil)
	if status != 201 || !store.ValidAgentSlug(auto["slug"].(string)) || auto["slug"] == "nova-one" {
		t.Fatalf("spawn without a name: %d %+v", status, auto)
	}
}

func TestSpawnRefusesInvalidNames(t *testing.T) {
	f := namedSpawnFixture(t)
	for i, name := range []string{"ab", "Capital", "has space", "-lead", "trail-", "nine_teen_chars_long_x", "ünïcode", "here", "all", "channel", "everyone", "humans", "ais"} {
		status, body := f.spawnNamed(t, "invalid-"+string(rune('a'+i)), map[string]any{"name": name})
		if status != 400 || body["error"] != "invalid_name" {
			t.Errorf("name %q: %d %+v, want 400 invalid_name", name, status, body)
		}
	}
	if n := f.agentCount(t); n != 0 {
		t.Fatalf("%d agents were created by refused names", n)
	}
}

func TestSpawnRefusesANameInUse(t *testing.T) {
	f := namedSpawnFixture(t)
	if status, body := f.spawnNamed(t, "first", map[string]any{"name": "taken-one"}); status != 201 {
		t.Fatalf("setup: %d %+v", status, body)
	}
	// An agent's name, a role key (@reviewer would be ambiguous) and a person's login are all in use; the check is on lower case.
	for i, name := range []string{"taken-one", "worker", "reviewer", "owner"} {
		status, body := f.spawnNamed(t, "clash-"+string(rune('a'+i)), map[string]any{"name": name})
		if status != 409 || body["error"] != "name_in_use" {
			t.Errorf("name %q: %d %+v, want 409 name_in_use", name, status, body)
		}
	}
	if n := f.agentCount(t); n != 1 {
		t.Fatalf("%d agents exist, want only the first", n)
	}
}

// A name is a person's choice, so a resumed v1 agent (which keeps its own name) must not take one as well.
func TestSpawnNameCannotBeCombinedWithResumeLegacy(t *testing.T) {
	f := namedSpawnFixture(t)
	status, body := f.spawnNamed(t, "both", map[string]any{"name": "nova-one", "resume_legacy": "sage@old"})
	if status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("name with resume_legacy: %d %+v", status, body)
	}
}

func TestSpawnRaceForOneNameCreatesOneAgent(t *testing.T) {
	f := namedSpawnFixture(t)
	type result struct {
		status int
		body   map[string]any
	}
	results := make(chan result, 2)
	for _, key := range []string{"race-a", "race-b"} {
		go func() {
			status, body := f.spawnNamed(t, key, map[string]any{"name": "contested"})
			results <- result{status, body}
		}()
	}
	created, refused := 0, 0
	for range 2 {
		switch got := <-results; {
		case got.status == 201:
			created++
		case got.status == 409 && got.body["error"] == "name_in_use":
			refused++
		default:
			t.Fatalf("a spawn: %d %+v", got.status, got.body)
		}
	}
	if created != 1 || refused != 1 || f.agentCount(t) != 1 {
		t.Fatalf("created %d, refused %d, agents %d", created, refused, f.agentCount(t))
	}
}

func TestNameSuggestionIsFreeAndAcceptedBySpawn(t *testing.T) {
	f := namedSpawnFixture(t)
	taken := map[string]bool{}
	for i := 0; i < 5; i++ {
		name := f.suggestedName(t)
		if !store.ValidAgentSlug(name) || taken[name] {
			t.Fatalf("suggestion %q is invalid or one that is already taken", name)
		}
		status, card := f.spawnNamed(t, "suggested-"+name, map[string]any{"name": name})
		if status != 201 || card["slug"] != name {
			t.Fatalf("spawn with the suggested name %q: %d %+v", name, status, card)
		}
		taken[name] = true
	}
	// Nothing is reserved: asking twice may give the same name.
	if resp := f.requestAs(t, f.cookie, f.csrf, http.MethodGet, "/api/rooms/"+f.room.ID+"/agent-name", nil); resp.StatusCode != 200 {
		t.Fatalf("second ask: %d", resp.StatusCode)
	}
}

func TestNameSuggestionNeedsRoomAccess(t *testing.T) {
	f := namedSpawnFixture(t)
	cookie, csrf := registerSecondUser(t, f, "stranger")
	resp := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/rooms/"+f.room.ID+"/agent-name", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 403 && resp.StatusCode != 404 {
		t.Fatalf("a person outside the project got %d", resp.StatusCode)
	}
}
