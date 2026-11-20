package store

import (
	"context"
	"database/sql"

	"github.com/goccy/go-json"
)

// Items nobody can act on any more settle by themselves. A recovery or a failed delivery waits on an agent and a room; once the room is closed or
// the agent is stopped, the person's decision would only move a turn nobody will run, so the Inbox would keep a card that can never be finished.
// Each settled item is resolved by "system (<reason>)" and says why in its event, so the history shows it was not a person's choice.

const settleItemLimit = 100

// settleKeepsSQL matches the items a settle pass deliberately leaves open (settleOrphanedItem returns without resolving them). The sweep leaves them
// out of its candidates, so they cannot fill its page and starve the items behind them:
//   - a recovery for a lost turn nothing has proved gone (a person alone can accept that overlap risk);
//   - a turn-less recovery (a loop budget) of an agent that is not stopped: it is paused, and closing a room does not make it moot.
const settleKeepsSQL = `(i.kind='recovery' AND (
	(i.turn_id IS NULL AND a.id IS NOT NULL AND a.desired<>'stopped')
	OR (i.turn_id IS NOT NULL AND COALESCE(json_extract(i.options_json,'$.phase'),'') NOT IN ('quiescence_wait','answer_held','broken_session')
		AND EXISTS(SELECT 1 FROM turn lt WHERE lt.id=i.turn_id AND lt.exec='lost'
			AND NOT EXISTS(SELECT 1 FROM turn c WHERE c.parent_turn_id=lt.id AND c.mode='normal'))
		AND NOT EXISTS(SELECT 1 FROM event q WHERE q.turn_id=i.turn_id AND q.kind IN ('recovery_quiescent','recovery_overlap_override')))))`

type settleCandidate struct{ id, reason string }

// settleCandidates reads the ids (and the reason to record) of the items a settle pass should act on.
func (tx *Tx) settleCandidates(query string, args ...any) ([]settleCandidate, error) {
	rows, err := tx.tx.QueryContext(tx.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []settleCandidate
	for rows.Next() {
		var c settleCandidate
		if err := rows.Scan(&c.id, &c.reason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SettleOrphanedInbox settles, in one pass: the open recovery and delivery_failed items of a stopped agent or a closed room, and the open
// questions whose thread a person (or the asking agent) has since replied in. The room-close, stop and message hooks settle these as they
// happen; this catches what they cannot (a turn killed by the stop only ends, and raises its recovery, after it) and what was open before.
func (s *Store) SettleOrphanedInbox(ctx context.Context) error {
	return s.Tx(ctx, func(tx *Tx) error {
		orphans, err := tx.settleCandidates(`SELECT i.id,CASE WHEN r.lifecycle='closed' THEN 'room closed' ELSE 'agent stopped' END
			FROM inbox_item i JOIN room r ON r.id=i.room_id LEFT JOIN agent a ON a.id=i.agent_id
			WHERE i.state='open' AND i.kind IN ('recovery','delivery_failed')
			AND (r.lifecycle='closed' OR a.desired='stopped')
			AND NOT `+settleKeepsSQL+` ORDER BY i.id LIMIT ?`, settleItemLimit)
		if err != nil {
			return err
		}
		for _, c := range orphans {
			if err := tx.settleOrphanedItem(c.id, c.reason); err != nil {
				return err
			}
		}
		answered, err := tx.settleCandidates(`SELECT i.id,'answered in thread' FROM inbox_item i JOIN message ask ON ask.id=i.message_id
			WHERE i.state='open' AND i.kind IN ('question','approval') AND i.requester_kind='agent' AND ask.ask_json IS NULL
			AND EXISTS(SELECT 1 FROM message m WHERE m.room_id=i.room_id AND m.seq>ask.seq AND (
				m.author_kind='user' AND (m.reply_to=ask.id OR m.thread_root=COALESCE(NULLIF(ask.thread_root,''),ask.id))
				OR m.author_kind='agent' AND m.author_id=i.requester_id AND m.reply_to=ask.id))
			ORDER BY i.id LIMIT ?`, settleItemLimit)
		if err != nil {
			return err
		}
		for _, c := range answered {
			if err := tx.closeAnsweredQuestion(c.id, c.reason); err != nil {
				return err
			}
		}
		return nil
	})
}

// closeAnsweredQuestion supersedes a question already answered in its thread (what supersedeAsks does as the reply arrives).
func (tx *Tx) closeAnsweredQuestion(itemID, reason string) error {
	var projectID, roomID, agentID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,room_id,COALESCE(agent_id,'') FROM inbox_item WHERE id=? AND state='open'`, itemID).Scan(&projectID, &roomID, &agentID); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE inbox_item SET state='superseded',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open'`, "system ("+reason+")", timestamp(), itemID); err != nil {
		return err
	}
	_, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "resolution": "superseded", "reason": reason})})
	return err
}

// settleRoomInbox settles every open recovery and delivery_failed item of a room that has just closed.
func (tx *Tx) settleRoomInbox(roomID string) error {
	return tx.settleWhere("room closed", `i.room_id=?`, roomID)
}

// settleAgentInbox settles every open recovery and delivery_failed item of an agent that has just been stopped.
func (tx *Tx) settleAgentInbox(agentID string) error {
	return tx.settleWhere("agent stopped", `i.agent_id=?`, agentID)
}

func (tx *Tx) settleWhere(reason, cond string, arg any) error {
	items, err := tx.settleCandidates(`SELECT i.id,? FROM inbox_item i WHERE i.state='open' AND i.kind IN ('recovery','delivery_failed') AND `+cond+` ORDER BY i.id`, reason, arg)
	if err != nil {
		return err
	}
	for _, c := range items {
		if err := tx.settleOrphanedItem(c.id, c.reason); err != nil {
			return err
		}
	}
	return nil
}

// settleOrphanedItem resolves one open recovery or delivery_failed item whose room is closed or whose agent is stopped.
//
// A failed delivery is dismissed. A recovery is abandoned the way a person's Abandon does it (its notes are aborted, the recovery block lifted
// and the turn settled), except that a turn whose process may still be running (exec lost, nothing proved it gone) is left for a person: only
// a decision can accept that risk.
func (tx *Tx) settleOrphanedItem(itemID, reason string) error {
	var projectID, roomID, agentID, kind, options string
	var turnID sql.NullString
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,room_id,COALESCE(agent_id,''),kind,turn_id,COALESCE(options_json,'') FROM inbox_item WHERE id=? AND state='open'`, itemID).
		Scan(&projectID, &roomID, &agentID, &kind, &turnID, &options); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	actor := "system (" + reason + ")"
	choice := "dismiss"
	if kind == "recovery" && !turnID.Valid {
		var desired string
		if agentID != "" {
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT desired FROM agent WHERE id=?`, agentID).Scan(&desired); err != nil {
				return err
			}
			if desired != "stopped" {
				return nil
			}
		}
	}
	if kind == "recovery" && turnID.Valid {
		var ask inboxAsk
		if options != "" {
			if err := json.Unmarshal([]byte(options), &ask); err != nil {
				return err
			}
		}
		if ask.Phase == brokenSessionReason {
			// Nothing was lost and no block was raised: it only closes, as decideAnswerRecovery's Abandon does (nothing else changes).
			return tx.resolveSettled(itemID, projectID, roomID, agentID, turnID.String, "abandon", actor, reason)
		} else if ask.Phase == quiescenceWaitReason {
			if err := tx.cancelWaitingContinuation(turnID.String, actor, reason); err != nil {
				return err
			}
			if agentID != "" {
				return tx.confirmStoppedIfQuiescent(agentID)
			}
			return nil
		}
		var exec string
		var newer int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT exec,(SELECT COUNT(*) FROM turn c WHERE c.parent_turn_id=t.id AND c.mode='normal') FROM turn t WHERE t.id=?`, turnID.String).Scan(&exec, &newer); err != nil {
			return err
		}
		if exec == "lost" && newer == 0 {
			var quiescent bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind IN ('recovery_quiescent','recovery_overlap_override'))`, turnID.String).Scan(&quiescent); err != nil {
				return err
			}
			if !quiescent {
				return nil
			}
		}
		choice = "abandon"
		if newer == 0 {
			if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='unknown'`, turnID.String); err != nil {
				return err
			}
			// recovery_block is the agent's alone, not this item's: another open recovery of the agent still needs it.
			if _, err := tx.Exec(`UPDATE agent SET recovery_block=0,observed=CASE WHEN observed='lost' THEN 'idle' ELSE observed END WHERE id=?
				AND NOT EXISTS(SELECT 1 FROM inbox_item o WHERE o.agent_id=agent.id AND o.kind='recovery' AND o.state='open' AND o.id<>?)`, agentID, itemID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, turnID.String); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "recovery_override", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID.String,
				PayloadJSON: payloadJSON(map[string]string{"choice": choice, "actor": actor, "reason": reason})}); err != nil {
				return err
			}
		}
	}
	return tx.resolveSettled(itemID, projectID, roomID, agentID, turnID.String, choice, actor, reason)
}

// resolveSettled closes one auto-settled item and records why.
func (tx *Tx) resolveSettled(itemID, projectID, roomID, agentID, turnID, choice, actor, reason string) error {
	if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open'`, actor, timestamp(), itemID); err != nil {
		return err
	}
	if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
		PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": choice, "actor": actor, "reason": reason, "source": "auto_settle"})}); err != nil {
		return err
	}
	if agentID != "" {
		return tx.confirmStoppedIfQuiescent(agentID)
	}
	return nil
}

// cancelWaitingContinuation drops the continuation a quiescence-wait item holds: no process of its own was started, so its notes return to
// aborted and the item resolves (the same end as the Abandon an owner would choose).
func (tx *Tx) cancelWaitingContinuation(originID, actor, reason string) error {
	var childID, projectID, roomID, agentID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT c.id,a.project_id,t.room_id,t.agent_id FROM turn c JOIN turn t ON t.id=c.parent_turn_id JOIN agent a ON a.id=t.agent_id
		WHERE c.parent_turn_id=? AND c.mode='normal' AND c.exec='pending' ORDER BY c.id DESC LIMIT 1`, originID).Scan(&childID, &projectID, &roomID, &agentID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), childID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='queued'`, childID); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(Event{Kind: "recovery_continuation_cancelled", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: childID,
			PayloadJSON: payloadJSON(map[string]string{"reason": reason})}); err != nil {
			return err
		}
	}
	return tx.resolveQuiescenceWaitingItems(originID, actor, "abandon", reason)
}
