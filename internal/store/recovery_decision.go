package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/goccy/go-json"
)

type RecoveryDecision struct {
	ItemID string `json:"item_id"`
	Choice string `json:"choice"`
	TurnID string `json:"turn_id,omitempty"`
	Rev    int    `json:"rev"`
	// StoppedAgent is the agent an Abandon in an archived project stopped, for the caller to tell its machine. It is not part of the answer
	// and is not kept with a replayed decision, which has already told it.
	StoppedAgent *AgentCard `json:"-"`
}

func (s *Store) DecideRecovery(ctx context.Context, itemID, userID, key, choice string, rev int) (RecoveryDecision, bool, error) {
	digest := sha256.Sum256([]byte(itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev)))
	var stoppedAgent *AgentCard
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: "user", ActorID: userID, Kind: "recovery_decision",
		Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, roomID, agentID, kind, state, createdAt, options string
		var source, role sql.NullString
		var currentRev int
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.project_id,i.room_id,i.agent_id,i.kind,i.state,i.turn_id,i.rev,i.created_at,COALESCE(i.options_json,''),m.role
			FROM inbox_item i LEFT JOIN membership m ON m.project_id=i.project_id AND m.user_id=? WHERE i.id=?`, userID, itemID).
			Scan(&projectID, &roomID, &agentID, &kind, &state, &source, &currentRev, &createdAt, &options, &role); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrForbidden
			}
			return "", err
		}
		if !role.Valid {
			return "", ErrForbidden
		}
		if role.String != "owner" && role.String != "operator" {
			return "", ErrForbidden
		}
		if kind != "recovery" {
			return "", ErrForbidden
		}
		if currentRev != rev {
			return "", ErrStaleInboxRev
		}
		// In an archived project the only decision is Abandon, and it ends with the agent stopped: anything else would start work again.
		archivedProject := false
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT archived_at IS NOT NULL FROM project WHERE id=?`, projectID).Scan(&archivedProject); err != nil {
			return "", err
		}
		if archivedProject && choice != "abandon" {
			return "", ErrProjectArchived
		}
		if state == "resolved" {
			return "", ErrAlreadyResolved
		}
		if state == "withdrawn" {
			return "", ErrAlreadyWithdrawn
		}
		var ask inboxAsk
		if options != "" {
			if err := json.Unmarshal([]byte(options), &ask); err != nil {
				return "", err
			}
		}
		decision := RecoveryDecision{ItemID: itemID, Choice: choice, Rev: rev + 1}
		waitForQuiescence := false
		if !source.Valid {
			if choice != "continue" && choice != "abandon" {
				return "", ErrInvalidTransition
			}
			if choice == "continue" {
				updated, err := tx.Exec(`UPDATE agent SET desired='active',desired_rev=desired_rev+1 WHERE id=? AND desired='not_accepting'`, agentID)
				if err != nil {
					return "", err
				}
				rows, err := updated.RowsAffected()
				if err != nil {
					return "", err
				}
				if rows != 1 {
					return "", ErrStaleInboxRev
				}
				if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: agentID,
					PayloadJSON: payloadJSON(map[string]string{"desired": "active", "actor": userID})}); err != nil {
					return "", err
				}
			} else {
				if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE agent_id=? AND room_id=? AND state='queued' AND created_at<=?`, agentID, roomID, createdAt); err != nil {
					return "", err
				}
			}
		} else if ask.Phase == "quiescence_wait" {
			childID, err := tx.decideQuiescenceWait(source.String, projectID, roomID, agentID, userID, choice)
			if err != nil {
				return "", err
			}
			decision.TurnID = childID
		} else {
			if choice != "resume" && choice != "retry" && choice != "abandon" {
				return "", ErrInvalidTransition
			}
			var newer int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='normal'`, source.String).Scan(&newer); err != nil {
				return "", err
			}
			if newer > 0 {
				return "", ErrStaleInboxRev
			}
			var itemSeq int64
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT seq FROM event WHERE kind IN ('inbox_item_created','inbox_item_updated') AND json_extract(payload_json,'$.inbox_item_id')=? ORDER BY seq DESC LIMIT 1`, itemID).Scan(&itemSeq); err != nil {
				return "", err
			}
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM event WHERE turn_id=? AND seq>? AND kind IN ('turn_process_exit','turn_lost','turn_ack_accepted','answer_committed','answer_held','recovery_override')`, source.String, itemSeq).Scan(&newer); err != nil {
				return "", err
			}
			if newer > 0 {
				return "", ErrStaleInboxRev
			}
			if choice == "abandon" {
				if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='unknown'`, source.String); err != nil {
					return "", err
				}
				if _, err := tx.Exec(`UPDATE agent SET recovery_block=0,observed=CASE WHEN desired='stopped' THEN 'stopped' ELSE 'idle' END WHERE id=?`, agentID); err != nil {
					return "", err
				}
				if _, err := tx.Exec(`UPDATE turn SET settle='settled' WHERE id=? AND settle='pending'`, source.String); err != nil {
					return "", err
				}
				if _, err := tx.AppendEvent(Event{Kind: "recovery_override", ProjectID: projectID, RoomID: roomID,
					AgentID: agentID, TurnID: source.String, PayloadJSON: payloadJSON(map[string]string{"choice": choice, "actor": userID})}); err != nil {
					return "", err
				}
			} else {
				if err := tx.createRecoveryContinuation(source.String, choice, userID, ""); err != nil {
					return "", err
				}
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM turn WHERE parent_turn_id=? AND mode='normal' ORDER BY id DESC LIMIT 1`, source.String).Scan(&decision.TurnID); err != nil {
					return "", err
				}
				var exec string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT exec FROM turn WHERE id=?`, source.String).Scan(&exec); err != nil {
					return "", err
				}
				if exec == "lost" {
					var proven bool
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind IN ('recovery_quiescent','recovery_overlap_override'))`, source.String).Scan(&proven); err != nil {
						return "", err
					}
					waitForQuiescence = !proven
				}
			}
		}
		if archivedProject {
			// The agent was left running by the archive because its execution was unknown; now that it is decided, it is stopped.
			var desired string
			var desiredRev int64
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT desired,desired_rev FROM agent WHERE id=?`, agentID).Scan(&desired, &desiredRev); err != nil {
				return "", err
			}
			if desired != "stopped" {
				card, err := tx.setAgentDesiredTrusted(agentID, userID, "stopped", desiredRev)
				if err != nil {
					return "", err
				}
				decision.StoppedAgent = &card
			}
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open' AND rev=?`, userID, timestamp(), itemID, rev); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": choice, "actor": userID})}); err != nil {
			return "", err
		}
		if waitForQuiescence {
			if err := tx.createQuiescenceWaitingItem(source.String, decision.TurnID); err != nil {
				return "", err
			}
		}
		stoppedAgent = decision.StoppedAgent
		data, err := json.Marshal(decision)
		return string(data), err
	})
	if err != nil {
		return RecoveryDecision{}, false, err
	}
	var decision RecoveryDecision
	if err := json.Unmarshal([]byte(result), &decision); err != nil {
		return RecoveryDecision{}, false, err
	}
	if !reused {
		decision.StoppedAgent = stoppedAgent
	}
	return decision, reused, nil
}

func (tx *Tx) decideQuiescenceWait(originID, projectID, roomID, agentID, actorID, choice string) (string, error) {
	var childID, exec string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,exec FROM turn WHERE parent_turn_id=? AND mode='normal' ORDER BY id DESC LIMIT 1`, originID).Scan(&childID, &exec); err != nil {
		return "", err
	}
	if exec != "pending" {
		return "", ErrStaleInboxRev
	}
	switch choice {
	case "override":
		const risk = "The old process may still be running; continuing now can overlap it and repeat an action."
		if _, err := tx.AppendEvent(Event{Kind: "recovery_overlap_override", ProjectID: projectID, RoomID: roomID,
			AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{
				"actor": actorID, "continuation_turn_id": childID, "risk": risk})}); err != nil {
			return "", err
		}
		return childID, nil
	case "abandon":
		if _, err := tx.Exec(`UPDATE turn SET exec='killed',outcome='cancelled',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), childID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE id IN(SELECT note_id FROM turn_note WHERE turn_id=?) AND state='queued'`, childID); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "recovery_override", ProjectID: projectID, RoomID: roomID,
			AgentID: agentID, TurnID: originID, PayloadJSON: payloadJSON(map[string]string{"actor": actorID, "choice": "abandon_after_wait"})}); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", ErrInvalidTransition
	}
}
