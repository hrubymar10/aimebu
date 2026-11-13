package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/store"
)

func handbookToolPut(t *testing.T, f *fixture, key string, body any) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, f.http.URL+"/api/agent/handbook", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandbookAgentAndHumanAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "leader")
	_, workerKey := f.createToolAgentNamed(t, "role_worker_default", "worker")
	path := "/api/projects/" + f.project.ID + "/handbook"
	resp := f.request("GET", path, nil, false)
	var initial store.Handbook
	if err := json.NewDecoder(resp.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || initial.Rev != 0 {
		t.Fatalf("initial %d %+v", resp.StatusCode, initial)
	}
	resp = f.toolRequest(t, "/api/agent/handbook", workerKey)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker get %d", resp.StatusCode)
	}
	resp = handbookToolPut(t, f, workerKey, map[string]any{"section": "current_state", "content": "state", "rev": 0})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker put %d", resp.StatusCode)
	}
	resp = handbookToolPut(t, f, leadKey, map[string]any{"section": "current_state", "content": "state", "rev": 0})
	var updated store.Handbook
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || updated.Rev != 1 || updated.UpdatedBy != lead.ID {
		t.Fatalf("lead update %d %+v", resp.StatusCode, updated)
	}
	resp = handbookToolPut(t, f, leadKey, map[string]any{"section": "timeline", "patch": "entry", "rev": 0})
	var conflict struct {
		Error      string `json:"error"`
		CurrentRev int64  `json:"current_rev"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&conflict); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 409 || conflict.CurrentRev != 1 {
		t.Fatalf("stale %d %+v", resp.StatusCode, conflict)
	}
	resp = f.request("GET", path+"/history?limit=1", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("history %d", resp.StatusCode)
	}
	resp = f.request("GET", path+"/history/1", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot %d", resp.StatusCode)
	}
	resp = f.request("PUT", path, map[string]any{"section": "timeline", "content": "human edit", "rev": 1}, true)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("human write %d", resp.StatusCode)
	}
}
