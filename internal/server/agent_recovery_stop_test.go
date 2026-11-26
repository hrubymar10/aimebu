package server

import (
	"bytes"
	"database/sql"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (f *fixture) seedRecoveryItem(t *testing.T, itemID, workerID, leadID string) {
	t.Helper()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if leadID == "" {
			_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES (?,?,?,?,'agent',?,'recovery',?)`,
				itemID, f.project.ID, f.room.ID, workerID, workerID, "2026-01-01T00:00:00.000000000Z")
			return err
		}
		_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,lead_due_at,recovery_lead_id,created_at) VALUES (?,?,?,?,'agent',?,'recovery','2099-01-01T00:00:00.000000000Z',?,?)`,
			itemID, f.project.ID, f.room.ID, workerID, workerID, leadID, "2026-01-01T00:00:00.000000000Z")
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) agentState(t *testing.T, agentID, itemID string) (desired, item string) {
	t.Helper()
	reader, err := sql.Open("sqlite", "file:"+f.db.DBPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.QueryRow(`SELECT desired FROM agent WHERE id=?`, agentID).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRow(`SELECT state FROM inbox_item WHERE id=?`, itemID).Scan(&item); err != nil {
		t.Fatal(err)
	}
	return desired, item
}

// resolve_recovery's abandon_stop abandons the worker's recovery and stops the worker; a replay of the same key answers 200 without a second stop.
func TestAgentRecoveryToolAbandonAndStopStopsWorker(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "stoplead")
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "stopworker")
	f.seedRecoveryItem(t, "inb_abandon_stop", worker.ID, lead.ID)
	path := "/api/agent/recoveries/inb_abandon_stop/decision"
	resp := f.toolPost(t, path, leadKey, "lead-abandon-stop", map[string]string{"choice": "abandon_stop"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("abandon_stop status=%d", resp.StatusCode)
	}
	if desired, item := f.agentState(t, worker.ID, "inb_abandon_stop"); desired != "stopped" || item != "resolved" {
		t.Fatalf("worker desired=%q item=%q, want stopped and resolved", desired, item)
	}
	resp = f.toolPost(t, path, leadKey, "lead-abandon-stop", map[string]string{"choice": "abandon_stop"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("replay status=%d", resp.StatusCode)
	}
}

// A manager that may only spawn (a role without lead or flow management) may abandon but not stop: abandon_stop needs stop_agent's permission,
// is refused with 403 and applies nothing, and the item stays open.
func TestAgentRecoveryToolAbandonAndStopNeedsStopPermission(t *testing.T) {
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO role(id,key,label,body,role_key,cap_is_lead,cap_can_spawn) VALUES ('role_spawn_only','spawn-only','Spawn only','','spawn-only',0,1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	manager, managerKey := f.createToolAgentNamed(t, "role_spawn_only", "spawnonly")
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "spawnworker")
	f.seedRecoveryItem(t, "inb_spawn_only", worker.ID, manager.ID)
	path := "/api/agent/recoveries/inb_spawn_only/decision"
	resp := f.toolPost(t, path, managerKey, "spawn-only-stop", map[string]string{"choice": "abandon_stop"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("abandon_stop status=%d, want 403", resp.StatusCode)
	}
	if desired, item := f.agentState(t, worker.ID, "inb_spawn_only"); desired != "active" || item != "open" {
		t.Fatalf("worker desired=%q item=%q, want nothing applied", desired, item)
	}
	resp = f.toolPost(t, path, managerKey, "spawn-only-abandon", map[string]string{"choice": "abandon"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("plain abandon status=%d, want 201", resp.StatusCode)
	}
	if desired, _ := f.agentState(t, worker.ID, "inb_spawn_only"); desired != "active" {
		t.Fatalf("plain abandon changed desired to %q", desired)
	}
}

// A person's Abandon and stop on the Inbox route stops the agent.
func TestInboxDecisionAbandonAndStopStopsAgent(t *testing.T) {
	f := newFixture(t)
	f.seed()
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "inboxstopworker")
	f.seedRecoveryItem(t, "inb_person_stop", worker.ID, "")
	body, _ := json.Marshal(map[string]any{"choice": "abandon_stop", "rev": 0})
	req, _ := http.NewRequest("POST", f.http.URL+"/api/inbox/inb_person_stop/decision", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", "person-abandon-stop")
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if desired, item := f.agentState(t, worker.ID, "inb_person_stop"); desired != "stopped" || item != "resolved" {
		t.Fatalf("worker desired=%q item=%q, want stopped and resolved", desired, item)
	}
}
