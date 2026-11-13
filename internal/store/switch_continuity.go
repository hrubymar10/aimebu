package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

const switchHandoffWindow = 3 * time.Minute
const switchOverallWindow = 15 * time.Minute

// SwitchHandoffBody is written by the outgoing agent or assembled from room history.
type SwitchHandoffBody struct {
	CurrentTask   string `json:"current_task"`
	Status        string `json:"status"`
	WorkState     string `json:"work_state"`
	NextSteps     string `json:"next_steps"`
	OpenQuestions string `json:"open_questions"`
	Cautions      string `json:"cautions"`
}

func (body SwitchHandoffBody) valid() bool {
	return strings.TrimSpace(body.CurrentTask) != "" && strings.TrimSpace(body.Status) != "" && strings.TrimSpace(body.NextSteps) != ""
}

// BeginAgentSwitchWithContinuity retains the outgoing session until its one
// handoff turn settles or the fallback path is chosen.
func (tx *Tx) BeginAgentSwitchWithContinuity(id, actorID, roomID, tool, variant, model, provider, machineID, checkoutPath, launcherID string, rev int64) (AgentCard, error) {
	var prior string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT desired FROM agent WHERE id=?`, id).Scan(&prior); err != nil {
		return AgentCard{}, err
	}
	// A completed prior handoff remains visible on the card until another switch.
	if _, err := tx.Exec(`DELETE FROM switch_handoff WHERE agent_id=?`, id); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.BeginAgentSwitchToLauncher(id, actorID, roomID, tool, variant, model, provider, machineID, checkoutPath, launcherID, rev); err != nil {
		return AgentCard{}, err
	}
	now := time.Now().UTC()
	status := "waiting"
	if prior != "active" {
		status = "automatic"
	}
	if _, err := tx.Exec(`INSERT INTO switch_handoff(agent_id,room_id,status,requested_at,overall_deadline,prior_desired) VALUES (?,?,?,?,?,?)`, id, roomID, status, now.Format(timestampLayout), now.Add(switchOverallWindow).Format(timestampLayout), prior); err != nil {
		return AgentCard{}, err
	}
	if prior == "active" {
		if _, err := tx.Exec(`UPDATE agent SET desired='active',desired_rev=desired_rev+1 WHERE id=?`, id); err != nil {
			return AgentCard{}, err
		}
	} else if err := tx.switchAutomaticHandoff(id, roomID, "The outgoing agent was not accepting turns."); err != nil {
		return AgentCard{}, err
	}
	return tx.agentCardByID(id)
}

func (tx *Tx) switchAutomaticHandoff(agentID, roomID, reason string) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT author_id,body FROM message WHERE room_id=? ORDER BY seq DESC LIMIT 8`, roomID)
	if err != nil {
		return err
	}
	var lines []string
	for rows.Next() {
		var author, body string
		if err := rows.Scan(&author, &body); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, author+": "+excerptRunes(strings.ReplaceAll(body, "\n", " "), 500))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	body := SwitchHandoffBody{CurrentTask: "Review the recent room history", Status: "Automatic fallback: " + reason, WorkState: strings.Join(lines, "\n"), NextSteps: "Verify the current task and repository state before proceeding.", Cautions: "No outgoing-agent handoff was available."}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE switch_handoff SET status='automatic',source='automatic',body_json=?,error=? WHERE agent_id=?`, string(encoded), reason, agentID)
	return err
}

func (tx *Tx) beginSwitchHandoffTurn(agentID, roomID string, generation int) error {
	var sessionID string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM session WHERE agent_id=? AND generation=?`, agentID, generation).Scan(&sessionID); err != nil {
		return err
	}
	turn, err := tx.RecordTurn(Turn{AgentID: agentID, SessionID: sessionID, Generation: generation, RoomID: roomID, Attempt: 1, Mode: "normal", Exec: "pending"})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO switch_handoff_turn(turn_id,agent_id) VALUES (?,?)`, turn.ID, agentID); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE switch_handoff SET status='pending',turn_id=? WHERE agent_id=?`, turn.ID, agentID)
	return err
}

func (tx *Tx) switchHandoffStatus(agentID string) (status, roomID, deadline, turnID string, err error) {
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT status,room_id,overall_deadline,COALESCE(turn_id,'') FROM switch_handoff WHERE agent_id=?`, agentID).Scan(&status, &roomID, &deadline, &turnID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", nil
	}
	return
}

func (tx *Tx) finishSwitchHandoff(agentID, roomID string, source string, body SwitchHandoffBody) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE switch_handoff SET status='ready',source=?,body_json=? WHERE agent_id=?`, source, string(encoded), agentID); err != nil {
		return err
	}
	return nil
}

// applySwitchHandoffAnswer consumes the dedicated turn's answer without
// publishing an agent-authored message. The later room handoff is system-owned.
func (tx *Tx) applySwitchHandoffAnswer(turn answerTurn, input AnswerInput, receipt *AnswerReceipt) (bool, error) {
	var agentID, status, roomID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT ht.agent_id,COALESCE(h.status,''),COALESCE(h.room_id,'') FROM switch_handoff_turn ht LEFT JOIN switch_handoff h ON h.agent_id=ht.agent_id AND h.turn_id=ht.turn_id WHERE ht.turn_id=?`, turn.id).Scan(&agentID, &status, &roomID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if agentID != turn.agentID {
		return true, ErrScopeMismatch
	}
	receipt.Result = "committed"
	if status != "pending" && status != "handing_off" {
		return true, nil
	}
	var body SwitchHandoffBody
	valid := input.ParseError == "" && input.Parsed != nil && len(input.Parsed.Messages) == 1 && input.Parsed.Messages[0].Room == roomID
	if valid {
		message := input.Parsed.Messages[0]
		valid = message.Ask == nil && len(message.Attachments) == 0 && !message.NeedsAttention && len(message.ProposedAnswers) == 0 && len(message.OpenQuestions) == 0 && !message.Handoff
		if valid {
			decoder := json.NewDecoder(bytes.NewReader([]byte(message.Body)))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body) == nil && body.valid() {
				var extra any
				valid = errors.Is(decoder.Decode(&extra), io.EOF)
			} else {
				valid = false
			}
		}
	}
	if valid {
		if err := tx.finishSwitchHandoff(agentID, roomID, "agent", body); err != nil {
			return true, err
		}
	} else {
		if err := tx.switchAutomaticHandoff(agentID, roomID, "Outgoing handoff answer was missing or invalid."); err != nil {
			return true, err
		}
		receipt.Warnings = append(receipt.Warnings, "switch used an automatic handoff summary")
	}
	return true, nil
}

// SwitchContinuityAction is a user action while the old session still owns the agent.
func (tx *Tx) SwitchContinuityAction(agentID, actorID, action string, rev int64) (AgentCard, error) {
	var card AgentCard
	err := func() error {
		var projectID, desired, prior, state, status, roomID, turnID string
		var currentRev int64
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.project_id,a.desired,a.desired_rev,h.prior_desired,sw.state,h.status,h.room_id,COALESCE(h.turn_id,'') FROM agent a JOIN agent_switch sw ON sw.agent_id=a.id JOIN switch_handoff h ON h.agent_id=a.id WHERE a.id=?`, agentID).Scan(&projectID, &desired, &currentRev, &prior, &state, &status, &roomID, &turnID); err != nil {
			return err
		}
		if rev != currentRev {
			return ErrStaleRev
		}
		if err := tx.authorizeSpawnActor(projectID, "user", actorID, false); err != nil {
			return err
		}
		if state != "draining" {
			return ErrInvalidTransition
		}
		if status != "waiting" && status != "pending" && status != "handing_off" && status != "automatic" && status != "ready" {
			return ErrInvalidTransition
		}
		switch action {
		case "cancel":
			if err := tx.withdrawBoundaryPending(agentID); err != nil {
				return err
			}
			if turnID != "" {
				if _, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), turnID); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`UPDATE switch_handoff SET status='cancelled' WHERE agent_id=?`, agentID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, agentID); err != nil {
				return err
			}
			if desired != prior {
				if _, err := tx.Exec(`UPDATE agent SET desired=?,desired_rev=desired_rev+1 WHERE id=?`, prior, agentID); err != nil {
					return err
				}
			}
		case "continue":
			if turnID != "" {
				if _, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), turnID); err != nil {
					return err
				}
			}
			if err := tx.switchAutomaticHandoff(agentID, roomID, "Continued without an outgoing-agent handoff."); err != nil {
				return err
			}
			if desired != "not_accepting" {
				if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, agentID); err != nil {
					return err
				}
			}
		case "retry-handoff":
			if status != "automatic" {
				return ErrInvalidTransition
			}
			if turnID != "" {
				var open bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT exec IN ('pending','running') OR settle='pending' FROM turn WHERE id=?`, turnID).Scan(&open); err != nil {
					return err
				}
				if open {
					return ErrInvalidTransition
				}
			}
			if _, err := tx.Exec(`UPDATE switch_handoff SET status='waiting',source='',body_json='',error='',turn_id=NULL,started_at=NULL,handoff_deadline=NULL,overall_deadline=? WHERE agent_id=?`, time.Now().UTC().Add(switchOverallWindow).Format(timestampLayout), agentID); err != nil {
				return err
			}
			if desired != "active" {
				if _, err := tx.Exec(`UPDATE agent SET desired='active',desired_rev=desired_rev+1 WHERE id=?`, agentID); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%w: unknown switch action", ErrInvalidTransition)
		}
		var err error
		card, err = tx.agentCardByID(agentID)
		return err
	}()
	return card, err
}

func (s *Store) SwitchContinuityAction(ctx context.Context, agentID, actorID, action string, rev int64) (AgentCard, error) {
	var card AgentCard
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		card, err = tx.SwitchContinuityAction(agentID, actorID, action, rev)
		return err
	})
	return card, err
}

// SwitchPreview describes the context the successor will receive. It does not
// start a switch or read the handbook's contents.
type SwitchPreview struct {
	HandoffStatus    string `json:"handoff_status"`
	HistoryWindow    int    `json:"history_window"`
	HandbookIncluded bool   `json:"handbook_included"`
	CanCancel        bool   `json:"can_cancel"`
	CanRetry         bool   `json:"can_retry"`
	CanContinue      bool   `json:"can_continue"`
}

func (s *Store) AgentSwitchPreview(ctx context.Context, agentID, actorID, roomID string) (SwitchPreview, error) {
	var out SwitchPreview
	err := s.Tx(ctx, func(tx *Tx) error {
		var projectID, roleID, desired, observed string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,role_id,desired,observed FROM agent WHERE id=? AND revoked_at IS NULL`, agentID).Scan(&projectID, &roleID, &desired, &observed); err != nil {
			return err
		}
		if err := tx.authorizeSpawnActor(projectID, "user", actorID, false); err != nil {
			return err
		}
		var joined bool
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room r JOIN room_member rm ON rm.room_id=r.id AND rm.member_kind='agent' AND rm.member_id=? AND rm.left_at IS NULL WHERE r.id=? AND r.project_id=? AND r.lifecycle='open')`, agentID, roomID, projectID).Scan(&joined); err != nil {
			return err
		}
		if !joined {
			return ErrInvalidTransition
		}
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT cap_is_lead FROM role WHERE id=?`, roleID).Scan(&out.HandbookIncluded); err != nil {
			return err
		}
		out.HistoryWindow = 40
		out.HandoffStatus = "available"
		if desired != "active" || observed == "stopped" || observed == "lost" {
			out.HandoffStatus = "automatic"
		}
		var status, turnID, switchState string
		err := tx.tx.QueryRowContext(tx.ctx, `SELECT h.status,COALESCE(h.turn_id,''),sw.state FROM switch_handoff h JOIN agent_switch sw ON sw.agent_id=h.agent_id WHERE h.agent_id=?`, agentID).Scan(&status, &turnID, &switchState)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			out.HandoffStatus = status
			out.CanCancel = switchState == "draining" && (status == "waiting" || status == "pending" || status == "handing_off" || status == "automatic" || status == "ready")
			out.CanContinue = out.CanCancel
			out.CanRetry = switchState == "draining" && status == "automatic"
			if out.CanRetry && turnID != "" {
				var open bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT exec IN ('pending','running') OR settle='pending' FROM turn WHERE id=?`, turnID).Scan(&open); err != nil {
					return err
				}
				out.CanRetry = !open
			}
		}
		return nil
	})
	return out, err
}
