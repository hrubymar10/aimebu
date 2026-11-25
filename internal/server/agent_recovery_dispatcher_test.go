package server

import (
	"io"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// A worker's loop-budget pause is assigned to the dispatcher that spawned the
// worker, and the dispatcher releases it through the same resolve_recovery tool
// a lead uses, with the Continue choice the item carries.
func TestAgentRecoveryToolLetsTheAssignedDispatcherContinue(t *testing.T) {
	f := newFixture(t)
	f.seed()
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE project SET team_flow_key='leader-dispatcher' WHERE id=?`, f.project.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, dispatcherKey := f.createToolAgentNamed(t, "role_dispatcher_default", "budgetdispatcher")
	worker, workerKey := f.createToolAgentNamed(t, "role_worker_default", "budgetworker")
	itemID := "inb_turn_budget_tool"
	err = f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,lead_due_at,recovery_lead_id,created_at)
			VALUES (?,?,?,?,'agent',?,'recovery',?,'2099-01-01T00:00:00.000000000Z',?,?)`,
			itemID, f.project.ID, f.room.ID, worker.ID, worker.ID,
			`{"text":"Agent paused after 20 turns in 10 minutes to prevent an automatic reply loop.","options":["Continue","Abandon"]}`,
			dispatcher.ID, "2026-01-01T00:00:00.000000000Z"); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, worker.ID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(store.Event{ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: worker.ID,
			Kind: "recovery_handed_to_lead", PayloadJSON: `{"inbox_item_id":"` + itemID + `","lead_id":"` + dispatcher.ID + `","reason":"turn_budget"}`})
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
	resp = f.toolPost(t, path, workerKey, "worker-continue", map[string]string{"choice": "continue"})
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("worker decision status=%d", resp.StatusCode)
	}
	resp = f.toolRequest(t, "/api/agent/recoveries", dispatcherKey)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("dispatcher list status=%d body=%s", resp.StatusCode, body)
	}
	var listed struct {
		Recoveries []store.LeadRecovery `json:"recoveries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(listed.Recoveries) != 1 || listed.Recoveries[0].ID != itemID || listed.Recoveries[0].Reason != "turn_budget" {
		t.Fatalf("dispatcher list=%+v", listed.Recoveries)
	}
	resp = f.toolPost(t, path, dispatcherKey, "dispatcher-continue", map[string]string{"choice": "continue"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		t.Fatalf("dispatcher continue status=%d body=%s", resp.StatusCode, body)
	}
	agent, err := f.db.AgentRuntimeByID(t.Context(), worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if agent.Desired != "active" {
		t.Fatalf("worker desired=%q after the dispatcher continued it", agent.Desired)
	}
	resp2 := f.toolPost(t, path, dispatcherKey, "dispatcher-continue", map[string]string{"choice": "continue"})
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("dispatcher replay status=%d", resp2.StatusCode)
	}
}
