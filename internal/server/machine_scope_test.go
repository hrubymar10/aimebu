package server

import (
	"io"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestMachineScopeRouteAndSpawnRefusal(t *testing.T) {
	f := newFixture(t)
	f.seed()
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	response := f.request("PUT", "/api/machines/"+f.machineID+"/scope", map[string]any{"scope_mode": "listed", "allowed_project_ids": []string{other.ID}, "disabled": false}, true)
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("scope write: %d %s", response.StatusCode, data)
	}
	response.Body.Close()
	response = f.request("GET", "/api/machines?project_id="+f.project.ID, nil, false)
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || strings.Contains(string(data), f.machineID) {
		t.Fatalf("project machine list: %d %s", response.StatusCode, data)
	}
	code, _, body := f.spawnRequest(t, "scope-spawn", "worker")
	if code != 409 || body["error"] != "machine_scope" {
		t.Fatalf("spawn: %d %+v", code, body)
	}
	response = f.request("PUT", "/api/machines/"+f.machineID+"/scope", map[string]any{"scope_mode": "all", "allowed_project_ids": []string{}, "disabled": true}, true)
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("disable: %d %s", response.StatusCode, data)
	}
	response.Body.Close()
	code, _, body = f.spawnRequest(t, "scope-disabled", "worker")
	if code != 409 || body["error"] != "machine_scope" {
		t.Fatalf("disabled spawn: %d %+v", code, body)
	}
}

func TestRoleDefaultSkipsMachineOutsideProjectScope(t *testing.T) {
	f := roleDefaultPoolFixture(t, "codex")
	other, err := f.db.CreateProject(t.Context(), store.Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetMachineScope(t.Context(), f.machineID, "listed", []string{other.ID}, false); err != nil {
		t.Fatal(err)
	}
	_, available, err := f.srv.nextRoleSpawnDefault(t.Context(), f.project.ID, "worker", "")
	if err != nil || available {
		t.Fatalf("default outside scope: available=%t err=%v", available, err)
	}
	code, card := spawnRoleDefault(t, f, "scope-pool")
	if code == 201 {
		t.Fatalf("default spawned outside scope: %+v", card)
	}
}
