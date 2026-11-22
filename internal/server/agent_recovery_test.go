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

func TestAgentRecoveryToolRequiresAssignedProjectLead(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "recoverylead")
	worker, workerKey := f.createToolAgentNamed(t, "role_worker_default", "recoveryworker")
	itemID := "inb_recovery_tool"
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,lead_due_at,recovery_lead_id,created_at) VALUES (?,?,?,?,'agent',?,'recovery','2099-01-01T00:00:00.000000000Z',?,?)`, itemID, f.project.ID, f.room.ID, worker.ID, worker.ID, lead.ID, "2026-01-01T00:00:00.000000000Z")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/agent/recoveries/" + itemID + "/decision"
	resp := f.toolRequest(t, "/api/agent/recoveries", workerKey)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker list status=%d", resp.StatusCode)
	}
	resp = f.toolPost(t, path, workerKey, "worker-recovery", map[string]string{"choice": "abandon"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker decision status=%d", resp.StatusCode)
	}
	other, err := f.db.CreateProject(context.Background(), store.Project{Name: "other", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var otherLead store.Agent
	err = f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_other_recovery',?,?,?)`, other.ID, f.machineID, "/tmp/other-recovery"); err != nil {
			return err
		}
		var err error
		otherLead, err = tx.CreateAgent(store.Agent{Slug: "otherrecoverylead", ProjectID: other.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: f.machineID, CheckoutID: "chk_other_recovery", KeyHash: "other-recovery", CreatedBy: "test"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := f.srv.Auth.IssueAgentKey(context.Background(), otherLead.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp = f.toolPost(t, path, otherKey, "foreign-recovery", map[string]string{"choice": "abandon"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign lead decision status=%d", resp.StatusCode)
	}
	resp = f.toolRequest(t, "/api/agent/recoveries", leadKey)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("assigned lead list status=%d", resp.StatusCode)
	}
	resp = f.toolPost(t, path, leadKey, "assigned-recovery", map[string]string{"choice": "abandon"})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("assigned lead decision status=%d", resp.StatusCode)
	}
	resp = f.toolPost(t, path, leadKey, "assigned-recovery", map[string]string{"choice": "abandon"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("lead replay status=%d", resp.StatusCode)
	}
}

// The overlap risk of a recovery waiting for the old process is a person's call: a lead may not override it through the tool.
func TestAgentRecoveryToolRefusesOverride(t *testing.T) {
	f := newFixture(t)
	f.seed()
	_, leadKey := f.createToolAgentNamed(t, "role_leader_default", "overridelead")
	path := "/api/agent/recoveries/inb_missing/decision"
	resp := f.toolPost(t, path, leadKey, "override-refused", map[string]string{"choice": "override"})
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("override status=%d, want 400", resp.StatusCode)
	}
	resp = f.toolPost(t, path, leadKey, "restart-missing", map[string]string{"choice": "restart"})
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("restart on a missing item status=%d, want 404 (the choice is valid)", resp.StatusCode)
	}
}

// A person may pick restart (the broken-session choice) on the Inbox decision route: it fails on the item that does not exist, not on the choice.
func TestInboxDecisionAcceptsRestart(t *testing.T) {
	f := newFixture(t)
	f.seed()
	post := func(choice string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"choice": choice, "rev": 0})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/inbox/inb_missing/decision", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", "restart-"+choice)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := post("restart"); status != 404 {
		t.Fatalf("restart on a missing item status=%d, want 404 (the choice is valid)", status)
	}
	if status := post("bogus"); status != 400 {
		t.Fatalf("made-up choice status=%d, want 400", status)
	}
}

// A lead deciding a recovery someone resolved while the item was with people reads the resolution, not a bare "project lead required".
func TestAgentRecoveryToolOnAResolvedItemSaysWhoResolvedIt(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "laterecoverylead")
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "laterecoveryworker")
	itemID := "inb_recovery_resolved"
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	ownerID := owner.ID
	err = f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,lead_due_at,recovery_lead_id,created_at) VALUES (?,?,?,?,'agent',?,'recovery','2099-01-01T00:00:00.000000000Z',?,?)`,
			itemID, f.project.ID, f.room.ID, worker.ID, worker.ID, lead.ID, "2026-01-01T00:00:00.000000000Z"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at='2026-01-02T00:00:00.000000000Z' WHERE id=?`, ownerID, itemID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(store.Event{ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: worker.ID, Kind: "inbox_item_resolved",
			PayloadJSON: `{"inbox_item_id":"` + itemID + `","choice":"abandon","actor":"` + ownerID + `"}`})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/recoveries/"+itemID+"/decision", leadKey, "late-recovery", map[string]string{"choice": "abandon"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 409 {
		t.Fatalf("late decision status=%d body=%s", resp.StatusCode, body)
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Error != "already_resolved" ||
		payload.Message != "already resolved by "+ownerID+" (choice abandon)" {
		t.Fatalf("payload=%+v body=%s", payload, body)
	}
}

// Restart exists only for a broken session: on any other recovery the store refuses it, and that is a 409 the lead can read, not a 500
// that lands in Sentry.
func TestAgentRecoveryToolRestartOnAnotherKindIsConflict(t *testing.T) {
	f := newFixture(t)
	f.seed()
	lead, leadKey := f.createToolAgentNamed(t, "role_leader_default", "restartlead")
	worker, _ := f.createToolAgentNamed(t, "role_worker_default", "restartworker")
	itemID := "inb_recovery_restart"
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,lead_due_at,recovery_lead_id,created_at) VALUES (?,?,?,?,'agent',?,'recovery','2099-01-01T00:00:00.000000000Z',?,?)`,
			itemID, f.project.ID, f.room.ID, worker.ID, worker.ID, lead.ID, "2026-01-01T00:00:00.000000000Z")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := f.toolPost(t, "/api/agent/recoveries/"+itemID+"/decision", leadKey, "restart-other-kind", map[string]string{"choice": "restart"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 409 || !strings.Contains(string(body), `"invalid_transition"`) {
		t.Fatalf("restart on another kind: status=%d body=%s", resp.StatusCode, body)
	}
}
