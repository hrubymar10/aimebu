package server

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (f *fixture) roleJSON(t *testing.T, method, path string, body any, wantStatus int) (store.RoleDefinition, map[string]string) {
	t.Helper()
	response := f.request(method, path, body, true)
	defer response.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil && response.StatusCode != 204 {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d (%v)", method, path, response.StatusCode, wantStatus, raw)
	}
	encoded, _ := json.Marshal(raw)
	var role store.RoleDefinition
	_ = json.Unmarshal(encoded, &role)
	errors := map[string]string{}
	if code, ok := raw["error"].(string); ok {
		errors["error"] = code
	}
	return role, errors
}

func (f *fixture) listedRoles(t *testing.T) map[string]store.RoleDefinition {
	t.Helper()
	response := f.request(http.MethodGet, "/api/roles", nil, false)
	defer response.Body.Close()
	var roles []store.RoleDefinition
	if err := json.NewDecoder(response.Body).Decode(&roles); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]store.RoleDefinition{}
	for _, role := range roles {
		byKey[role.Key] = role
	}
	return byKey
}

func TestRoleExtendsAndResetAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	roles := f.listedRoles(t)
	sec := roles["sec-reviewer"]
	if sec.Extends != "reviewer" || !strings.HasPrefix(sec.Body, "Additional focus: ") || !strings.HasPrefix(sec.ResolvedBody, roles["reviewer"].Body+"\n\nAdditional focus: ") {
		t.Fatalf("a built-in specialist should extend reviewer and resolve to the reviewer text plus its focus: %+v", sec)
	}
	qa, _ := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "qa", Label: "QA", Body: "qa focus", Extends: "reviewer", Cardinality: "multi"}, 201)
	if qa.Extends != "reviewer" {
		t.Fatalf("the created role did not keep extends: %+v", qa)
	}
	if got := f.listedRoles(t)["qa"].ResolvedBody; got != roles["reviewer"].Body+"\n\nqa focus" {
		t.Fatalf("qa resolves to %.80q", got)
	}
	// Refusals: an unknown parent, a cycle, and deleting a role others extend.
	if _, e := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "lost", Label: "Lost", Body: "x", Extends: "ghost", Cardinality: "multi"}, 400); e["error"] != "invalid_extends" {
		t.Fatalf("unknown parent error = %v", e)
	}
	cyclic := qa
	cyclic.Extends = "qa"
	if _, e := f.roleJSON(t, http.MethodPut, "/api/roles/"+qa.ID, cyclic, 400); e["error"] != "invalid_extends" {
		t.Fatalf("self extends error = %v", e)
	}
	kid, _ := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "kid", Label: "Kid", Body: "kid", Extends: "qa", Cardinality: "multi"}, 201)
	if _, e := f.roleJSON(t, http.MethodDelete, "/api/roles/"+qa.ID+"?rev=0", nil, 409); e["error"] != "role_extended" {
		t.Fatalf("deleting an extended role error = %v", e)
	}
	f.roleJSON(t, http.MethodDelete, "/api/roles/"+kid.ID+"?rev=0", nil, 204)
	f.roleJSON(t, http.MethodDelete, "/api/roles/"+qa.ID+"?rev=0", nil, 204)
	// Reset: edit a built-in, reset it, and see what shipped again; a stale revision and a role that is not built in are refused.
	edited := sec
	edited.Label, edited.Body, edited.Extends = "Mine", "mine", ""
	edited, _ = f.roleJSON(t, http.MethodPut, "/api/roles/"+sec.ID, edited, 200)
	if _, e := f.roleJSON(t, http.MethodPost, "/api/roles/"+sec.ID+"/reset", map[string]any{"rev": edited.Rev + 5}, 409); e["error"] != "stale_rev" {
		t.Fatalf("stale reset error = %v", e)
	}
	custom, _ := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "custom", Label: "Custom", Body: "c", Cardinality: "multi"}, 201)
	if _, e := f.roleJSON(t, http.MethodPost, "/api/roles/"+custom.ID+"/reset", map[string]any{"rev": custom.Rev}, 400); e["error"] != "invalid_request" {
		t.Fatalf("resetting a custom role error = %v", e)
	}
	reset, _ := f.roleJSON(t, http.MethodPost, "/api/roles/"+sec.ID+"/reset", map[string]any{"rev": edited.Rev}, 200)
	if reset.Label != sec.Label || reset.Body != sec.Body || reset.Extends != "reviewer" || reset.Rev != edited.Rev+1 || reset.ResolvedBody != sec.ResolvedBody {
		t.Fatalf("reset returned %+v, want the shipped %+v", reset, sec)
	}
}

// Roles are global: a write that names a project is refused instead of quietly creating a global role.
func TestRoleWritesRefuseAProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	body := map[string]any{"project_id": f.project.ID, "role_key": "scoped", "label": "Scoped", "body": "x", "cardinality": "multi"}
	if _, e := f.roleJSON(t, http.MethodPost, "/api/roles", body, 400); e["error"] != "invalid_request" {
		t.Fatalf("project role create error = %v", e)
	}
	if _, ok := f.listedRoles(t)["scoped"]; ok {
		t.Fatal("a refused project role was created as a global one")
	}
	custom, _ := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "custom", Label: "Custom", Body: "c", Cardinality: "multi"}, 201)
	update := map[string]any{"project_id": f.project.ID, "id": custom.ID, "rev": custom.Rev, "role_key": "custom", "label": "Custom", "body": "changed", "cardinality": "multi"}
	if _, e := f.roleJSON(t, http.MethodPut, "/api/roles/"+custom.ID, update, 400); e["error"] != "invalid_request" {
		t.Fatalf("project role update error = %v", e)
	}
	if got := f.listedRoles(t)["custom"].Body; got != "c" {
		t.Fatalf("a refused update changed the role: %q", got)
	}
}

// An agent's card says its role was updated when its role was deleted and the key now has a definition that differs from the one the
// agent was spawned with. A definition that differs only by its parent resolves to different instructions, so it counts.
func TestRoleUpdatedWhenOnlyTheParentDiffers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}),
		bound: map[string]int{}, capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	scout, _ := f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "scout", Label: "Scout", Body: "scout text", Cardinality: "multi"}, 201)
	status, agent, raw := f.spawnRequest(t, "role-parent", "scout")
	if status != 201 {
		t.Fatalf("spawn status=%d result=%v", status, raw)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.roleJSON(t, http.MethodDelete, "/api/roles/"+scout.ID+"?rev="+strconv.FormatInt(scout.Rev, 10), nil, 204)
	// Same text, name, emoji, cardinality and capabilities as the deleted role; only the parent is new.
	f.roleJSON(t, http.MethodPost, "/api/roles", store.RoleDefinition{Key: "scout", Label: scout.Label, Emoji: scout.Emoji,
		Body: scout.Body, Cardinality: scout.Cardinality, Extends: "worker"}, 201)
	cards, err := f.db.AgentCardsForRoom(t.Context(), f.room.ID)
	if err != nil || len(cards) != 1 || cards[0].ID != agent.ID || !cards[0].RoleUpdated {
		t.Fatalf("a definition that differs only by its parent should mark the card: %+v err=%v", cards, err)
	}
}
