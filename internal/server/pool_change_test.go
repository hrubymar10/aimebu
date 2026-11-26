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

func poolItemsAs(t *testing.T, f *fixture, cookie *http.Cookie, csrf string) []store.InboxEntry {
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

func decidePoolAs(t *testing.T, f *fixture, cookie *http.Cookie, csrf string, item store.InboxEntry, choice string) int {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"choice": choice, "rev": item.Rev})
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/"+item.ID+"/decision", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "pool-"+choice+item.ID+csrf)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	return r.StatusCode
}

// poolMovedFixture has two workers spawned from a two-entry pool, then the pool replaced by two other entries of the same two harnesses.
func poolMovedFixture(t *testing.T, workers int) (*fixture, []store.AgentCard) {
	t.Helper()
	f := roleDefaultPoolFixture(t, "claude", "codex")
	// A real supervisor reports launcher support; without it an agent records no launcher, and an agent with none is never flagged.
	launcherMachine(t, f, f.harness(t, "claude", "native"), f.harness(t, "codex", "native"))
	var cards []store.AgentCard
	for i := 0; i < workers; i++ {
		status, card := spawnRoleDefault(t, f, "pool-spawn-"+string(rune('a'+i)))
		if status != http.StatusCreated || !card.DefaultUsed {
			t.Fatalf("spawn %d: %d %+v", i, status, card)
		}
		cards = append(cards, card)
	}
	pool := `[{"launcher_id":"` + cards[0].LauncherID + `","model":"sonnet"},{"launcher_id":"` + cards[len(cards)-1].LauncherID + `","model":"gpt-6-luna"}]`
	if workers > 1 {
		pool = `[{"launcher_id":"` + cards[0].LauncherID + `","model":"sonnet"},{"launcher_id":"` + cards[1].LauncherID + `","model":"gpt-6-luna"}]`
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE role SET spawn_defaults_json=? WHERE role_key='worker'`, pool)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.DetectPoolChanges(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f, cards
}

// Respawning several agents of one role takes the role's next pool entry each time and advances its cursor, so they spread over the pool. An
// admin who is only a viewer may decide: the switch runs for the server on the strength of that decision, while the person's own switch
// route still refuses the same viewer.
func TestPoolRespawnSpreadsAgentsAndRunsOnTheServersBehalf(t *testing.T) {
	t.Parallel()
	f, cards := poolMovedFixture(t, 2)
	cookie, csrf := registerSecondUser(t, f, "viewer1")
	user, err := f.db.UserByLogin(t.Context(), "viewer1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE user SET admin=1 WHERE id=?`, user.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items := poolItemsAs(t, f, cookie, csrf)
	if len(items) != 1 || len(items[0].PoolAgents) != 2 || !items[0].CanDecide {
		t.Fatalf("the viewer admin's items: %+v", items)
	}
	// The person's own switch route still checks membership for the same viewer.
	data, _ := json.Marshal(map[string]any{"room_id": f.room.ID, "tool": "codex", "variant": "native", "launcher_id": cards[1].LauncherID, "model": "gpt-6-astra", "rev": cards[0].DesiredRev})
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/agents/"+cards[0].ID+"/switch", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", "viewer-own-switch")
	req.AddCookie(cookie)
	own, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	own.Body.Close()
	if own.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer's own switch: %d, want 403", own.StatusCode)
	}
	if status := decidePoolAs(t, f, cookie, csrf, items[0], "respawn"); status != http.StatusCreated {
		t.Fatalf("the viewer admin's decision: %d", status)
	}
	if err := f.srv.advancePoolRespawns(t.Context()); err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{}
	for _, card := range cards {
		now, err := f.db.AgentCardByID(t.Context(), card.ID)
		if err != nil || now.Switch == nil {
			reasons := []string{}
			for _, item := range poolItemsAs(t, f, f.cookie, f.csrf) {
				for _, a := range item.PoolAgents {
					reasons = append(reasons, a.Slug+": "+a.RespawnError)
				}
			}
			t.Fatalf("no switch began for %s (%v): %v", card.Slug, reasons, err)
		}
		targets[now.Switch.Tool+"/"+now.Switch.Model] = true
	}
	if len(targets) != 2 {
		t.Fatalf("both agents went to the same pool entry: %v", targets)
	}
}

// A switch that cannot begin is not dropped quietly: the item comes back listing the agent with its reason, and the person can choose again.
func TestPoolRespawnFailureComesBackWithItsReason(t *testing.T) {
	t.Parallel()
	f, _ := poolMovedFixture(t, 1)
	items := poolItemsAs(t, f, f.cookie, f.csrf)
	if len(items) != 1 {
		t.Fatalf("items: %+v", items)
	}
	if status := decidePoolAs(t, f, f.cookie, f.csrf, items[0], "respawn"); status != http.StatusCreated {
		t.Fatalf("decision: %d", status)
	}
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID) // the machine is gone: the switch cannot start
	f.srv.machineMu.Unlock()
	if err := f.srv.advancePoolRespawns(t.Context()); err != nil {
		t.Fatal(err)
	}
	back := poolItemsAs(t, f, f.cookie, f.csrf)
	if len(back) != 1 || back[0].ID == items[0].ID || len(back[0].PoolAgents) != 1 || !strings.HasPrefix(back[0].PoolAgents[0].RespawnError, "respawn failed: ") {
		t.Fatalf("the item did not come back with the reason: %+v", back)
	}
	if status := decidePoolAs(t, f, f.cookie, f.csrf, back[0], "keep"); status != http.StatusCreated {
		t.Fatalf("keep after a failure: %d", status)
	}
	if left := poolItemsAs(t, f, f.cookie, f.csrf); len(left) != 0 {
		t.Fatalf("a kept agent came back: %+v", left)
	}
}

// A respawn does not run for a decider who lost the right between the decision and the agent's idle moment: the switch never begins, and the
// card comes back listing the agent with the reason. The decider here is an instance admin who is a project member (not the global owner,
// whose privilege no demotion strips), and the demotion takes the admin flag away.
func TestPoolRespawnRefusesADeciderWhoIsNoLongerAllowed(t *testing.T) {
	t.Parallel()
	f, cards := poolMovedFixture(t, 1)
	cookie, csrf := registerSecondUser(t, f, "decider1")
	user, err := f.db.UserByLogin(t.Context(), "decider1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE user SET admin=1 WHERE id=?`, user.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, f.project.ID, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items := poolItemsAs(t, f, cookie, csrf)
	if len(items) != 1 {
		t.Fatalf("the admin's items: %+v", items)
	}
	if status := decidePoolAs(t, f, cookie, csrf, items[0], "respawn"); status != http.StatusCreated {
		t.Fatalf("decision: %d", status)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE user SET admin=0 WHERE id=?`, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.advancePoolRespawns(t.Context()); err != nil {
		t.Fatal(err)
	}
	card, err := f.db.AgentCardByID(t.Context(), cards[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Switch != nil {
		t.Fatalf("the respawn ran for a decider who lost the right: %+v", card.Switch)
	}
	// Give the right back to read the reason off the card.
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE user SET admin=1 WHERE id=?`, user.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	back := poolItemsAs(t, f, f.cookie, f.csrf)
	if len(back) != 1 || len(back[0].PoolAgents) != 1 || !strings.HasPrefix(back[0].PoolAgents[0].RespawnError, "respawn failed: ") {
		t.Fatalf("the item did not carry the reason: %+v", back)
	}
}
