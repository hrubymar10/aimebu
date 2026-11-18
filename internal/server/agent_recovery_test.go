package server

import (
	"context"
	"testing"

	"github.com/hrubymar10/aimebu/internal/store"
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
