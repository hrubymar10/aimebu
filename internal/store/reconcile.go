package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/goccy/go-json"
)

type JournalTurn struct {
	TurnID, Exec, Digest string
	HasAnswer            bool
	ProcessAlive         bool
	AnswerRev            int
}
type ReceiptRecord struct {
	TurnID                 string
	AnswerRev              int
	Digest, Result, Reason string
}

// ReconcileMachineHello treats an intact journal's missing turn_start as proof
// of non-start. A journaled but unfinished turn remains uncertain, never
// silently requeued after a lost accepted ack.
func (s *Store) ReconcileMachineHello(ctx context.Context, machineID, journalID string, capabilitiesJSON []byte, open map[string]JournalTurn) ([]ReceiptRecord, error) {
	if journalID == "" {
		return nil, errors.New("missing supervisor journal identity")
	}
	if !json.Valid(capabilitiesJSON) {
		return nil, errors.New("invalid machine capabilities")
	}
	receipts := []ReceiptRecord{}
	err := s.Tx(ctx, func(tx *Tx) error {
		var previous sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT journal_id FROM machine WHERE id=?`, machineID).Scan(&previous); err != nil {
			return err
		}
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id,t.agent_id,t.room_id,t.exec,t.mode,a.project_id,a.observed,EXISTS(SELECT 1 FROM receipt r WHERE r.turn_id=t.id) FROM turn t JOIN agent a ON a.id=t.agent_id WHERE a.machine_id=? AND t.exec IN ('pending','running') AND EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_dispatch_requested')`, machineID)
		if err != nil {
			return err
		}
		type item struct {
			id, agentID, roomID, exec, mode, projectID, observed string
			hasReceipt                                           bool
		}
		items := []item{}
		for rows.Next() {
			var item item
			if err := rows.Scan(&item.id, &item.agentID, &item.roomID, &item.exec, &item.mode, &item.projectID, &item.observed, &item.hasReceipt); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		changed := previous.Valid && previous.String != journalID || !previous.Valid && len(items) > 0
		for _, item := range items {
			journaled, found := open[item.id]
			if !changed && found && item.exec == "pending" && journaled.ProcessAlive {
				if _, err := tx.Exec(`UPDATE turn SET exec='running',started_at=COALESCE(started_at,?) WHERE id=?`, timestamp(), item.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE note SET state='in_turn' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('queued','dispatched')`, item.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE agent SET observed=CASE WHEN desired='stopped' THEN 'interrupting' ELSE 'in_turn' END WHERE id=? AND observed='idle'`, item.agentID); err != nil {
					return err
				}
				continue
			}
			if !changed && item.hasReceipt && found && (journaled.ProcessAlive || journaled.Exec == "exited") {
				// A live process or unacknowledged journaled exit can still
				// report completion. A receipt alone does not prove exit.
				continue
			}
			if !changed && !found && !item.hasReceipt && item.exec == "pending" {
				if item.mode == "correction" || item.mode == "diagnostic" {
					if err := tx.abandonSpecialTurn(item.id, "never_started"); err != nil {
						return err
					}
					if err := tx.confirmStoppedIfQuiescent(item.agentID); err != nil {
						return err
					}
					continue
				}
				if err := TurnExecTransitions.Transition("pending", "killed"); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=?`, timestamp(), item.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='dispatched'`, item.id); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(Event{Kind: "turn_never_started", ProjectID: item.projectID, RoomID: item.roomID, AgentID: item.agentID, TurnID: item.id}); err != nil {
					return err
				}
				var recoveryContinuation bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='recovery_decision')`, item.id).Scan(&recoveryContinuation); err != nil {
					return err
				}
				if recoveryContinuation {
					var desired string
					var origin sql.NullString
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.desired,t.parent_turn_id FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, item.id).Scan(&desired, &origin); err != nil {
						return err
					}
					if desired == "active" {
						if err := tx.replaceUnstartedRecoveryContinuation(item.id); err != nil {
							return err
						}
					} else {
						if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='queued'`, item.id); err != nil {
							return err
						}
						if _, err := tx.AppendEvent(Event{Kind: "recovery_continuation_cancelled", ProjectID: item.projectID, RoomID: item.roomID,
							AgentID: item.agentID, TurnID: item.id, PayloadJSON: payloadJSON(map[string]string{"reason": "no_start_while_not_active"})}); err != nil {
							return err
						}
						if origin.Valid {
							if err := tx.resolveQuiescenceWaitingItems(origin.String, "system (agent not active)", "abandon", "journal_never_started"); err != nil {
								return err
							}
						}
					}
				}
				if err := tx.confirmStoppedIfQuiescent(item.agentID); err != nil {
					return err
				}
				continue
			}
			if changed || !found || journaled.Exec == "running" && !journaled.ProcessAlive {
				if item.exec == "pending" {
					if err := TurnExecTransitions.Transition("pending", "running"); err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE turn SET exec='running' WHERE id=?`, item.id); err != nil {
						return err
					}
				}
				if err := TurnExecTransitions.Transition("running", "lost"); err != nil {
					return err
				}
				if err := TurnOutcomeTransitions.Transition("none", "unknown"); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE turn SET exec='lost',outcome='unknown' WHERE id=?`, item.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE note SET state='unknown' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('queued','dispatched','in_turn')`, item.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE agent SET recovery_block=1,observed='lost' WHERE id=?`, item.agentID); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(Event{Kind: "turn_lost", ProjectID: item.projectID, RoomID: item.roomID, AgentID: item.agentID, TurnID: item.id}); err != nil {
					return err
				}
				if err := tx.ensureRecoveryPath(item.id); err != nil {
					return err
				}
				if changed {
					if _, err := tx.AppendEvent(Event{Kind: "supervisor_journal_changed", ProjectID: item.projectID, RoomID: item.roomID, AgentID: item.agentID, TurnID: item.id,
						PayloadJSON: payloadJSON(map[string]string{"machine_id": machineID, "turn_id": item.id})}); err != nil {
						return err
					}
				}
			}
		}
		// Every lost turn this hello can vouch for, not only one that already has a continuation: a person or lead usually decides Resume or Retry
		// after the hello that found the turn lost, and the evidence recorded here is what lets that continuation start without a wait.
		lostRows, err := tx.tx.QueryContext(tx.ctx, `SELECT t.id FROM turn t JOIN agent a ON a.id=t.agent_id
			WHERE a.machine_id=? AND t.exec='lost' AND (t.settle<>'settled'
				OR EXISTS(SELECT 1 FROM turn child WHERE child.parent_turn_id=t.id AND child.mode='normal' AND child.exec='pending'))`, machineID)
		if err != nil {
			return err
		}
		lostIDs := []string{}
		for lostRows.Next() {
			var id string
			if err := lostRows.Scan(&id); err != nil {
				lostRows.Close()
				return err
			}
			lostIDs = append(lostIDs, id)
		}
		err = lostRows.Err()
		lostRows.Close()
		if err != nil {
			return err
		}
		for _, id := range lostIDs {
			journaled, found := open[id]
			if found && !journaled.ProcessAlive && (journaled.Exec == "running" || journaled.Exec == "exited" || journaled.Exec == "killed") {
				if err := tx.recordRecoveryQuiescence(id, "supervisor_hello_exit"); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE machine SET journal_id=?,capabilities_json=? WHERE id=?`, journalID, string(capabilitiesJSON), machineID); err != nil {
			return err
		}
		for _, journaled := range open {
			var resultJSON, digest string
			var answerRev int
			err := tx.tx.QueryRowContext(tx.ctx, `SELECT answer_rev,digest,result_json FROM receipt WHERE turn_id=? AND answer_rev=?`, journaled.TurnID, journaled.AnswerRev).Scan(&answerRev, &digest, &resultJSON)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return err
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(resultJSON), &body); err != nil {
				return err
			}
			var status, reason string
			_ = json.Unmarshal(body["result"], &status)
			_ = json.Unmarshal(body["held_reason"], &reason)
			receipts = append(receipts, ReceiptRecord{TurnID: journaled.TurnID, AnswerRev: answerRev, Digest: digest, Result: status, Reason: reason})
		}
		return nil
	})
	return receipts, err
}
