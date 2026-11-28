package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
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
	_, leadKey := f.createToolAgentNamed(t, "role_leader_default", "leader")
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
	var updated map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || updated["rev"] != float64(1) || updated["line_count"] == nil || updated["sections"] != nil {
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

func TestHandbookReadModesAndCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgentNamed(t, "role_leader_default", "leader")
	state := "Updated 2026-11-20\nneedle\nfollowing\n" + strings.Repeat("line\n", 60)
	resp := handbookToolPut(t, f, key, map[string]any{"section": "current_state", "content": state, "rev": 0})
	var update map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&update); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || update["rev"] != float64(1) || update["sections"] != nil || update["markdown"] != nil || update["line_count"] == nil {
		t.Fatalf("update leaked content: %#v", update)
	}
	get := func(query string) handbookView {
		t.Helper()
		response := f.toolRequest(t, "/api/agent/handbook"+query, key)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || len(body) > handbookAnswerCap {
			t.Fatalf("response %d bytes %d: %s", response.StatusCode, len(body), body)
		}
		var view handbookView
		if err := json.Unmarshal(body, &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	index := get("")
	if len(index.Sections) != 5 || len(index.Lines) != 50 || !index.HasMore || index.NextOffset == nil || *index.NextOffset != 51 {
		t.Fatalf("index: %+v", index)
	}
	section := get("?section=current_state")
	if len(section.Lines) != 50 || !section.HasMore || section.TotalLines != 63 {
		t.Fatalf("section: %+v", section)
	}
	page := get("?section=current_state&offset=51&limit=2")
	if len(page.Lines) != 2 || page.Lines[0].Line != 51 || page.NextOffset == nil || *page.NextOffset != 53 {
		t.Fatalf("page: %+v", page)
	}
	match := get("?grep=NEEDLE")
	if len(match.Lines) != 3 || match.Lines[1].Text != "needle" || match.Lines[0].Section != "current_state" {
		t.Fatalf("grep: %+v", match)
	}
	large := strings.Repeat("line of substantial handbook content\n", 61000)
	if _, err := f.db.UpdateHandbook(context.Background(), f.project.ID, lead.ID, "timeline", &large, nil, 1); err != nil {
		t.Fatal(err)
	}
	capped := get("?section=timeline&limit=61000")
	if !capped.HasMore || capped.NextOffset == nil || len(capped.Lines) == 0 {
		t.Fatalf("cap: %+v", capped)
	}
}

func TestHandbookLineLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgentNamed(t, "role_leader_default", "leader")
	for _, edit := range []struct {
		name string
		body map[string]any
	}{
		{"content", map[string]any{"section": "current_state", "content": strings.Repeat("x", 8<<10+1), "rev": 0}},
		{"patch", map[string]any{"section": "current_state", "patch": strings.Repeat("x", 8<<10+1), "rev": 1}},
	} {
		if edit.name == "patch" {
			resp := handbookToolPut(t, f, key, map[string]any{"section": "current_state", "content": "prefix", "rev": 0})
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("seed status=%d", resp.StatusCode)
			}
		}
		resp := handbookToolPut(t, f, key, edit.body)
		var got struct{ Error, Message string }
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 || got.Error != "invalid_request" || got.Message != "line 1 of section current_state is longer than 8 KiB; split it into shorter lines" {
			t.Fatalf("%s: status=%d body=%+v", edit.name, resp.StatusCode, got)
		}
	}
	resp := handbookToolPut(t, f, key, map[string]any{"section": "current_state", "content": strings.Repeat("x", 8<<10), "rev": 1})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("exact-limit status=%d", resp.StatusCode)
	}
	legacy := strings.Repeat("y", handbookAnswerCap)
	doc, err := f.db.GetHandbook(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc.Sections["timeline"] = legacy
	encoded, err := json.Marshal(doc.Sections)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE project_handbook SET sections_json=? WHERE project_id=?`, string(encoded), f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.toolRequest(t, "/api/agent/handbook?section=timeline", key)
	var view handbookView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !view.HasMore || view.NextOffset == nil || *view.NextOffset != 2 || !strings.Contains(view.Continue, "exceeds the answer cap") {
		t.Fatalf("legacy line: status=%d view=%+v", resp.StatusCode, view)
	}
}

func TestHandbookCapContinuationKeepsFilters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	lead, key := f.createToolAgentNamed(t, "role_leader_default", "leader")
	large := strings.Repeat("needle "+strings.Repeat("x", 400)+"\n", 100)
	if _, err := f.db.UpdateHandbook(context.Background(), f.project.ID, lead.ID, "current_state", &large, nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?grep=needle&limit=100", "?limit=100"} {
		resp := f.toolRequest(t, "/api/agent/handbook"+query, key)
		var view handbookView
		if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !view.HasMore || view.NextOffset == nil || !strings.Contains(view.Continue, "same filters") {
			t.Fatalf("%s: status=%d continue=%q", query, resp.StatusCode, view.Continue)
		}
	}
}

func TestHandbookDispatcherReadScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, leadKey := f.createToolAgentNamed(t, "role_leader_default", "leader")
	_, dispatcherKey := f.createToolAgentNamed(t, "role_dispatcher_default", "dispatcher")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, f.project.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		section, value string
		rev            int
	}{
		{"current_state", "state", 0}, {"open_backlog", "backlog", 1}, {"timeline", "secret timeline", 2},
	} {
		response := handbookToolPut(t, f, leadKey, map[string]any{"section": edit.section, "content": edit.value, "rev": edit.rev})
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("edit %s: %d", edit.section, response.StatusCode)
		}
	}
	response := f.toolRequest(t, "/api/agent/handbook", dispatcherKey)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var view handbookView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(view.Sections) != 2 || len(view.Lines) != 2 || strings.Contains(string(body), "secret timeline") {
		t.Fatalf("dispatcher read: %d %s", response.StatusCode, body)
	}
	response = f.toolRequest(t, "/api/agent/handbook?section=timeline", dispatcherKey)
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("restricted section: %d", response.StatusCode)
	}
	response = handbookToolPut(t, f, dispatcherKey, map[string]any{"section": "current_state", "content": "bad", "rev": 3})
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("dispatcher write: %d", response.StatusCode)
	}
}
