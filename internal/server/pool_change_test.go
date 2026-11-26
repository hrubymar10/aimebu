package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// A pool change raises one Inbox item for the project's owner; an operator neither sees it nor may decide it (403), and the owner's decision
// is recorded (Keep leaves the agent where it is, Respawn queues it).
func TestPoolChangeItemRouteIsForTheOwnerAndAdminsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	seedFakePool(t, f, "worker")
	agent, _ := f.createToolAgent(t, "role_worker_default")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE agent SET launcher_id='lnch_fake',pool_origin='pool' WHERE id=?`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The pool drops fake/test for model-b.
	resp := f.request(http.MethodPut, "/api/projects/"+f.project.ID+"/role-spawn-defaults/worker",
		map[string]any{"inherit": false, "default_agents": []map[string]string{{"launcher_id": "lnch_fake", "model": "model-b"}}, "pick_mode": "in_turn"}, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change pool: %d", resp.StatusCode)
	}
	list := func(cookie *http.Cookie, csrf string) []store.InboxEntry {
		t.Helper()
		r := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/inbox", nil)
		defer r.Body.Close()
		var all []store.InboxEntry
		if err := json.NewDecoder(r.Body).Decode(&all); err != nil {
			t.Fatal(err)
		}
		var out []store.InboxEntry
		for _, item := range all {
			if item.Kind == "pool_change" {
				out = append(out, item)
			}
		}
		return out
	}
	owner := list(f.cookie, f.csrf)
	if len(owner) != 1 || len(owner[0].PoolAgents) != 1 || owner[0].PoolAgents[0].Slug != agent.Slug || owner[0].PoolAgents[0].RoomTitle != "task" || !owner[0].CanDecide {
		t.Fatalf("owner's items: %+v", owner)
	}
	cookie, csrf := registerSecondUser(t, f, "operator2")
	user, err := f.db.UserByLogin(t.Context(), "operator2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'operator')`, f.project.ID, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if items := list(cookie, csrf); len(items) != 0 {
		t.Fatalf("an operator saw the item: %+v", items)
	}
	decide := func(cookie *http.Cookie, csrf, choice string, rev int64) int {
		t.Helper()
		body := map[string]any{"choice": choice, "rev": rev}
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/"+owner[0].ID+"/decision", strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Idempotency-Key", "pool-"+choice+csrf)
		req.AddCookie(cookie)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}
	if status := decide(cookie, csrf, "respawn", owner[0].Rev); status != http.StatusForbidden {
		t.Fatalf("an operator's decision: %d", status)
	}
	if status := decide(f.cookie, f.csrf, "respawn", owner[0].Rev); status != http.StatusCreated {
		t.Fatalf("the owner's decision: %d", status)
	}
	if items := list(f.cookie, f.csrf); len(items) != 0 {
		t.Fatalf("a decided item stayed: %+v", items)
	}
}
