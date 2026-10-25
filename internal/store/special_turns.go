package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/goccy/go-json"
)

// AbandonSpecialTurn is the terminal path for a correction or diagnostic
// that cannot be delivered. A failed correction must also settle its parent.
func (s *Store) AbandonSpecialTurn(ctx context.Context, id, reason string) error {
	return s.Tx(ctx, func(tx *Tx) error { return tx.abandonSpecialTurn(id, reason) })
}

func (tx *Tx) abandonSpecialTurn(id, reason string) error {
	child, err := tx.answerTurn(id)
	if err != nil {
		return err
	}
	if child.mode != "correction" && child.mode != "diagnostic" {
		return ErrInvalidTransition
	}
	if child.exec != "pending" {
		return ErrInvalidTransition
	}
	if reason == "dispatch_failed" {
		var admitted bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='turn_dispatch_requested')`, id).Scan(&admitted); err != nil {
			return err
		}
		if admitted {
			return ErrInvalidTransition
		}
	}
	if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), id); err != nil {
		return err
	}
	var projectID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id FROM agent WHERE id=?`, child.agentID).Scan(&projectID); err != nil {
		return err
	}
	if child.mode == "correction" && child.parentID != "" {
		parent, err := tx.answerTurn(child.parentID)
		if err != nil {
			return err
		}
		var data string
		err = tx.tx.QueryRowContext(tx.ctx, `SELECT result_json FROM receipt WHERE turn_id=? AND answer_rev=0`, parent.id).Scan(&data)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var saved struct {
				AnswerReceipt
				CandidateRaw string `json:"candidate_raw"`
			}
			if err := json.Unmarshal([]byte(data), &saved); err != nil {
				return err
			}
			if saved.Result == "correction_requested" {
				saved.Result = "held"
				saved.HeldReason = "correction_unavailable"
				saved.Error = reason
				updated, err := json.Marshal(saved)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE receipt SET result_json=? WHERE turn_id=? AND answer_rev=0`, string(updated), parent.id); err != nil {
					return err
				}
			}
		}
		if err := tx.settleAnswerTurn(parent); err != nil {
			return err
		}
		if err := tx.deliveryFailedInbox(parent); err != nil {
			return err
		}
	}
	if child.mode == "diagnostic" && child.parentID != "" {
		if err := tx.ensureRecoveryItem(child.parentID, reason); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE agent SET observed='idle' WHERE id=? AND observed='in_turn' AND NOT EXISTS(SELECT 1 FROM turn WHERE agent_id=? AND exec IN ('pending','running'))`, child.agentID, child.agentID); err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "special_turn_abandoned", ProjectID: projectID, RoomID: child.roomID, AgentID: child.agentID, TurnID: id, PayloadJSON: payloadJSON(map[string]string{"reason": reason})})
	return err
}

func (s *Store) ExpireBlockedSpecialTurns(ctx context.Context) (int, error) {
	count := 0
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id,r.lifecycle,r.gate,p.gate,COALESCE((SELECT e.created_at FROM event e WHERE e.turn_id=t.id AND e.kind='turn_recorded' ORDER BY e.seq LIMIT 1),'') FROM turn t JOIN room r ON r.id=t.room_id JOIN agent ag ON ag.id=t.agent_id JOIN project p ON p.id=ag.project_id WHERE t.exec='pending' AND t.mode IN ('correction','diagnostic') AND NOT EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')`)
		if err != nil {
			return err
		}
		type item struct{ id, lifecycle, roomGate, projectGate, created string }
		items := []item{}
		for rows.Next() {
			var x item
			if err := rows.Scan(&x.id, &x.lifecycle, &x.roomGate, &x.projectGate, &x.created); err != nil {
				rows.Close()
				return err
			}
			items = append(items, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		cutoff := time.Now().UTC().Add(-time.Minute).Format("2006-01-02T15:04:05.000000000Z")
		for _, x := range items {
			reason := ""
			if x.lifecycle != "open" {
				reason = "room_closed"
			} else if (x.roomGate != "open" || x.projectGate != "open") && x.created != "" && x.created < cutoff {
				reason = "gate_paused"
			}
			if reason == "" {
				continue
			}
			if err := tx.abandonSpecialTurn(x.id, reason); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}
