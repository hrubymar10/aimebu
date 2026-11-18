package store

import (
	"context"
	"database/sql"
	"errors"
)

// SweepWorkerRecoveries hands overdue or unowned worker recoveries to people.
// The conditional update makes repeated ticks and concurrent decisions harmless.
func (s *Store) SweepWorkerRecoveries(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT i.id FROM inbox_item i LEFT JOIN agent lead ON lead.id=i.recovery_lead_id LEFT JOIN settings_project sp ON sp.project_id=i.project_id
			WHERE i.kind='recovery' AND i.state='open' AND i.lead_due_at IS NOT NULL
			AND (i.lead_due_at<=? OR lead.id IS NULL OR lead.desired!='active' OR lead.recovery_block=1 OR lead.revoked_at IS NOT NULL OR sp.worker_recovery_to_lead=0) LIMIT 100`, timestamp())
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			var projectID, roomID, agentID, turnID string
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,room_id,agent_id,turn_id FROM inbox_item WHERE id=? AND state='open' AND lead_due_at IS NOT NULL`, id).Scan(&projectID, &roomID, &agentID, &turnID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			result, err := tx.Exec(`UPDATE inbox_item SET lead_due_at=NULL,rev=rev+1 WHERE id=? AND state='open' AND lead_due_at IS NOT NULL`, id)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if _, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": id, "kind": "recovery", "reason": "lead_escalation"})}); err != nil {
				return err
			}
		}
		return nil
	})
}

type LeadRecovery struct {
	ID           string `json:"item_id"`
	AgentID      string `json:"agent_id"`
	RoomID       string `json:"room_id"`
	TurnID       string `json:"turn_id"`
	Reason       string `json:"reason"`
	Evidence     string `json:"evidence"`
	EvidenceLink string `json:"evidence_link"`
	Rev          int    `json:"rev"`
}

func (s *Store) ListLeadRecoveries(ctx context.Context, leadID, projectID string) ([]LeadRecovery, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT i.id,i.agent_id,i.room_id,COALESCE(i.turn_id,''),COALESCE((SELECT json_extract(e.payload_json,'$.reason') FROM event e WHERE e.kind='recovery_handed_to_lead' AND json_extract(e.payload_json,'$.inbox_item_id')=i.id ORDER BY e.seq DESC LIMIT 1),''),COALESCE((SELECT json_extract(e.payload_json,'$.evidence') FROM event e WHERE e.kind='turn_process_exit' AND e.turn_id=i.turn_id ORDER BY e.seq DESC LIMIT 1),''),i.rev FROM inbox_item i WHERE i.project_id=? AND i.recovery_lead_id=? AND i.state='open' AND i.lead_due_at IS NOT NULL ORDER BY i.created_at,i.id`, projectID, leadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LeadRecovery{}
	for rows.Next() {
		var x LeadRecovery
		if err := rows.Scan(&x.ID, &x.AgentID, &x.RoomID, &x.TurnID, &x.Reason, &x.Evidence, &x.Rev); err != nil {
			return nil, err
		}
		x.EvidenceLink = "/api/rooms/" + x.RoomID + "/agents/" + x.AgentID + "/output"
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) RecoveryItemRevision(ctx context.Context, itemID string) (int, error) {
	var rev int
	err := s.readDB.QueryRowContext(ctx, `SELECT rev FROM inbox_item WHERE id=?`, itemID).Scan(&rev)
	return rev, err
}
