package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

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
	return s.decideRecoveryActor(ctx, itemID, "user", userID, key, choice, "", rev)
}

func (s *Store) DecideRecoveryAsAgent(ctx context.Context, itemID, leadID, key, choice, note string, rev int) (RecoveryDecision, bool, error) {
	return s.decideRecoveryActor(ctx, itemID, "agent", leadID, key, choice, note, rev)
}

func (s *Store) decideRecoveryActor(ctx context.Context, itemID, actorKind, actorID, key, choice, note string, rev int) (RecoveryDecision, bool, error) {
	digestInput := itemID + "\x00" + choice + "\x00" + strconv.Itoa(rev)
	if actorKind == "agent" {
		digestInput = itemID + "\x00" + choice + "\x00" + note
	}
	digest := sha256.Sum256([]byte(digestInput))
	var stoppedAgent *AgentCard
	result, reused, err := s.WithRequestKey(ctx, RequestKey{ActorKind: actorKind, ActorID: actorID, Kind: "recovery_decision",
		Key: key, Digest: hex.EncodeToString(digest[:])}, func(tx *Tx) (string, error) {
		var projectID, roomID, agentID, kind, state, createdAt, options string
		var source, role sql.NullString
		var currentRev int
		var leadID sql.NullString
		var leadDue sql.NullString
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT i.project_id,i.room_id,i.agent_id,i.kind,i.state,i.turn_id,i.rev,i.created_at,COALESCE(i.options_json,''),m.role,i.recovery_lead_id,i.lead_due_at
			FROM inbox_item i LEFT JOIN membership m ON m.project_id=i.project_id AND m.user_id=? WHERE i.id=?`, actorID, itemID).
			Scan(&projectID, &roomID, &agentID, &kind, &state, &source, &currentRev, &createdAt, &options, &role, &leadID, &leadDue); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", ErrForbidden
			}
			return "", err
		}
		if actorKind == "agent" {
			var allowed bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=? AND a.project_id=? AND r.cap_is_lead=1 AND a.desired='active' AND a.recovery_block=0 AND a.revoked_at IS NULL)`, actorID, projectID).Scan(&allowed); err != nil {
				return "", err
			}
			if !allowed || !leadID.Valid || leadID.String != actorID || !leadDue.Valid {
				return "", ErrForbidden
			}
		} else if !role.Valid || role.String != "owner" && role.String != "operator" || leadDue.Valid {
			return "", ErrForbidden
		}
		if kind != "recovery" && kind != "delivery_failed" {
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
		if ask.Phase == answerHeldReason || ask.Phase == brokenSessionReason {
			if err := tx.decideAnswerRecovery(ask.Phase, source.String, projectID, roomID, agentID, choice); err != nil {
				return "", err
			}
		} else if kind != "recovery" {
			return "", ErrForbidden
		} else if !source.Valid {
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
					PayloadJSON: payloadJSON(map[string]string{"desired": "active", "actor": actorID})}); err != nil {
					return "", err
				}
			} else {
				if _, err := tx.Exec(`UPDATE note SET state='aborted' WHERE agent_id=? AND room_id=? AND state='queued' AND created_at<=?`, agentID, roomID, createdAt); err != nil {
					return "", err
				}
			}
		} else if ask.Phase == "quiescence_wait" {
			childID, err := tx.decideQuiescenceWait(source.String, projectID, roomID, agentID, actorID, choice)
			if err != nil {
				return "", err
			}
			decision.TurnID = childID
		} else {
			if choice != "resume" && choice != "retry" && choice != "abandon" {
				return "", ErrInvalidTransition
			}
			// Only what the item offers: a turn that failed before it did anything is offered Retry and Abandon, and Resume has nothing to
			// continue, so the API refuses it too, not just the buttons. (An item stored without options, from before they were kept, offers all.)
			if !choiceOffered(ask.Options, choice) {
				return "", ErrChoiceNotOffered
			}
			var newer int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE parent_turn_id=? AND mode='normal'`, source.String).Scan(&newer); err != nil {
				return "", err
			}
			if newer > 0 {
				return "", ErrStaleInboxRev
			}
			var itemSeq int64
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT seq FROM event WHERE kind IN ('inbox_item_created','inbox_item_updated','recovery_handed_to_lead') AND json_extract(payload_json,'$.inbox_item_id')=? ORDER BY seq DESC LIMIT 1`, itemID).Scan(&itemSeq); err != nil {
				return "", err
			}
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM event WHERE turn_id=? AND seq>? AND kind IN ('turn_process_exit','turn_finished','turn_lost','turn_ack_accepted','answer_committed','answer_held','recovery_override')`, source.String, itemSeq).Scan(&newer); err != nil {
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
					AgentID: agentID, TurnID: source.String, PayloadJSON: payloadJSON(map[string]string{"choice": choice, "actor": actorID})}); err != nil {
					return "", err
				}
			} else {
				if err := tx.createRecoveryContinuation(source.String, choice, actorID, ""); err != nil {
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
				card, err := tx.setAgentDesiredTrusted(agentID, actorID, "stopped", desiredRev)
				if err != nil {
					return "", err
				}
				decision.StoppedAgent = &card
			}
		}
		if _, err := tx.Exec(`UPDATE inbox_item SET state='resolved',rev=rev+1,resolved_by=?,resolved_at=? WHERE id=? AND state='open' AND rev=?`, actorID, timestamp(), itemID, rev); err != nil {
			return "", err
		}
		if _, err := tx.AppendEvent(Event{Kind: "inbox_item_resolved", ProjectID: projectID, RoomID: roomID, AgentID: agentID,
			PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": itemID, "choice": choice, "actor": actorID, "actor_kind": actorKind})}); err != nil {
			return "", err
		}
		if actorKind == "agent" {
			body := "Recovery resolved by the lead: " + choice
			if note != "" {
				body += ". " + note
			}
			if _, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{}}); err != nil {
				return "", err
			}
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

// ErrChoiceNotOffered is a recovery decision that is not among the options the item was stored with.
var ErrChoiceNotOffered = errors.New("choice_not_offered")

// choiceOffered says whether a decision (the lowercase form of a button label) is among the stored options; an item without any offers all.
func choiceOffered(options []string, choice string) bool {
	if len(options) == 0 {
		return true
	}
	for _, option := range options {
		if strings.EqualFold(option, choice) {
			return true
		}
	}
	return false
}

// decideAnswerRecovery applies a decision on a recovery raised for a worker's undeliverable answer (answer_held) or for a session that answers
// with nothing (broken_session). Neither has a lost process to continue, so each has its own small set of choices.
func (tx *Tx) decideAnswerRecovery(phase, turnID, projectID, roomID, agentID, choice string) error {
	switch {
	case phase == answerHeldReason && choice == "retry":
		_, err := tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", OnlyTargets: []string{agentID},
			Body: heldReplyNotePrefix + "your last final answer could not be delivered. Send your final answer again as exactly one JSON object with a messages array."})
		return err
	case phase == brokenSessionReason && choice == "restart":
		// The next turn starts a fresh native session (with the room's history) instead of resuming the one that answers with nothing.
		if _, err := tx.Exec(`UPDATE session SET native_session_id=NULL,native_session_started_at=NULL,start_mode='fresh_with_history'
			WHERE id=(SELECT session_id FROM turn WHERE id=?)`, turnID); err != nil {
			return err
		}
		// The interrupted task runs again on it: the turn's notes go back to the queue (as a vanished native session does it).
		if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN (SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('dispatched','in_turn','done')`, turnID); err != nil {
			return err
		}
		_, err := tx.AppendEvent(Event{Kind: "native_session_reset", ProjectID: projectID, RoomID: roomID, AgentID: agentID, TurnID: turnID,
			PayloadJSON: payloadJSON(map[string]string{"reason": brokenSessionReason})})
		return err
	case choice == "abandon":
		return nil
	}
	return ErrInvalidTransition
}
