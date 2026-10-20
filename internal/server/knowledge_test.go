package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func TestKnowledgeAPIAndAgentToolScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	agent, key := f.createToolAgent(t, "role_worker_default")
	projectPath := "/api/projects/" + f.project.ID + "/memory"
	resp := f.request("GET", projectPath, nil, false)
	var listing struct {
		Enabled bool                    `json:"enabled"`
		Records []store.KnowledgeRecord `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || listing.Enabled || len(listing.Records) != 0 {
		t.Fatalf("default listing: %d %+v", resp.StatusCode, listing)
	}
	resp = f.toolPost(t, "/api/agent/memory", key, "", map[string]string{"scope": "project", "body": "durable fact"})
	if resp.StatusCode != 409 {
		t.Fatalf("agent memory while off: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("PUT", "/api/projects/"+f.project.ID+"/memory-enabled", map[string]bool{"enabled": true}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("enable memory: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/memory", key, "", map[string]string{"scope": "project", "body": "durable fact"})
	if resp.StatusCode != 201 {
		t.Fatalf("agent project fact: %d", resp.StatusCode)
	}
	var fact store.KnowledgeRecord
	if err := json.NewDecoder(resp.Body).Decode(&fact); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if fact.AuthorID != agent.ID || fact.Rev != 1 {
		t.Fatalf("fact: %+v", fact)
	}
	resp = f.request("PUT", projectPath+"/"+fact.ID, map[string]any{"body": "revised fact", "rev": 1}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("update fact: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("PUT", projectPath+"/"+fact.ID, map[string]any{"body": "stale", "rev": 1}, true)
	if resp.StatusCode != 409 {
		t.Fatalf("stale fact: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/memory", key, "", map[string]string{"scope": "task", "room_id": f.room.ID, "body": "task detail"})
	if resp.StatusCode != 201 {
		t.Fatalf("agent task note: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolRequest(t, "/api/agent/recall?query=task%20detail", key)
	if resp.StatusCode != 200 {
		t.Fatalf("agent recall: %d", resp.StatusCode)
	}
	var recall struct {
		Results []store.KnowledgeHit `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&recall); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(recall.Results) != 1 || recall.Results[0].Source != "task_note" {
		t.Fatalf("recall: %+v", recall.Results)
	}
	resp = f.request("DELETE", projectPath+"/"+fact.ID+"?rev=2", nil, true)
	if resp.StatusCode != 200 {
		t.Fatalf("delete fact: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestKnowledgeAgentCannotWriteUnjoinedTask(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	resp := f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": "private task"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("create room: %d", resp.StatusCode)
	}
	var other store.Room
	if err := json.NewDecoder(resp.Body).Decode(&other); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	data, _ := json.Marshal(map[string]string{"scope": "task", "room_id": other.ID, "body": "unjoined"})
	req, _ := http.NewRequest("POST", f.http.URL+"/api/agent/memory", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("unjoined task write: %d", resp.StatusCode)
	}
}

func TestAgentMemoryCurationAndRoomOverrideAPI(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	resp := f.toolRequest(t, "/api/agent/recall?query=fact", key)
	if resp.StatusCode != 409 {
		t.Fatalf("disabled recall: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("PUT", "/api/projects/"+f.project.ID+"/memory-enabled", map[string]bool{"enabled": true}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("enable: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.toolPost(t, "/api/agent/memory", key, "", map[string]string{"scope": "project", "body": "durable fact"})
	if resp.StatusCode != 201 {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	var fact store.KnowledgeRecord
	if err := json.NewDecoder(resp.Body).Decode(&fact); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp = f.toolRequest(t, "/api/agent/memory?scope=project", key)
	var listing struct {
		Records []store.KnowledgeRecord `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(listing.Records) != 1 {
		t.Fatalf("list: %d %+v", resp.StatusCode, listing)
	}
	call := func(method, path string, body any) *http.Response {
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, f.http.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		out, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	path := "/api/agent/memory/" + fact.ID + "?scope=project"
	resp = call("PUT", path, map[string]any{"body": "revised fact", "rev": 1})
	if resp.StatusCode != 200 {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = call("PUT", path, map[string]any{"body": "stale", "rev": 1})
	var stale struct {
		Error   string                `json:"error"`
		Current store.KnowledgeRecord `json:"current"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stale); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || stale.Error != "stale_rev" || stale.Current.Rev != 2 {
		t.Fatalf("stale: %d %+v", resp.StatusCode, stale)
	}
	resp = call("DELETE", path+"&rev=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("PUT", "/api/rooms/"+f.room.ID+"/memory-enabled", map[string]any{"enabled": false}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("room off: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("GET", "/api/rooms/"+f.room.ID, nil, false)
	var room store.Room
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if room.MemoryEnabled == nil || *room.MemoryEnabled {
		t.Fatalf("room setting: %+v", room)
	}
	resp = f.request("PUT", "/api/rooms/"+f.room.ID+"/memory-enabled", map[string]any{}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("missing enabled: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("GET", "/api/rooms/"+f.room.ID, nil, false)
	room = store.Room{}
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if room.MemoryEnabled == nil || *room.MemoryEnabled {
		t.Fatalf("missing field changed override: %+v", room)
	}
	resp = f.request("PUT", "/api/rooms/"+f.room.ID+"/memory-enabled", map[string]any{"enabled": nil}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("explicit null: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("GET", "/api/rooms/"+f.room.ID, nil, false)
	room = store.Room{}
	if err := json.NewDecoder(resp.Body).Decode(&room); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if room.MemoryEnabled != nil {
		t.Fatalf("null did not clear override: %+v", room)
	}
}
