package store

import (
	"context"
	"database/sql"
	"errors"
)

// A turn interrupt cuts one running turn and leaves its agent as it is (the supervisor is sent a turn_interrupt frame; the agent's desired
// state does not change). The request is recorded first, as a turn_interrupt_requested event on the turn, with what is to become of the
// turn's notes once the supervisor reports the turn interrupted (RecordProcessInfo reads it then):
//
//   - continue: the turn is continued on its own, like a turn whose process was cut off, without counting against the one automatic
//     continuation a chain of turns has (a person or the system asked for this interruption);
//   - requeue: the turn's notes go back to queued, with a caution when the turn had started working, and no turn is made for them: they
//     are delivered by the next dispatch (which a draining team-flow change holds back).
//
// A request whose turn then ends another way (it answered first, the agent was stopped) changes nothing.
const (
	TurnInterruptContinue = "continue"
	TurnInterruptRequeue  = "requeue"
)

const eventTurnInterruptRequested = "turn_interrupt_requested"

// TurnInterruptRequest asks for one turn to be cut. Purpose says what it is for (a short key, such as team_flow_migration).
type TurnInterruptRequest struct {
	TurnID      string
	ActorKind   string
	ActorID     string
	Purpose     string
	Disposition string
	// Yield makes the request give way to whoever already decided about the turn: a turn whose agent is part of a team flow change in
	// progress is refused (the change has its own interrupt), and a turn that already has a request keeps it (the first recorded
	// disposition wins; the target is returned so the frame can be sent again, and nothing new is recorded).
	Yield bool
}

// TurnInterruptTarget is what the server needs to send the frame. An empty TurnID says there is no turn to interrupt this way.
type TurnInterruptTarget struct {
	TurnID     string
	AgentID    string
	MachineID  string
	Generation int
}

// RequestTurnInterrupt records the request for a running ordinary turn and says where to send the frame. It authorizes nobody: the caller
// has decided the actor may do this. A turn that is not running, or is not an ordinary one (a correction, a diagnostic, a switch or
// departing handoff turn, whose answer the server is waiting for), is refused with ErrInvalidTransition; so is, for a request that
// yields, a turn held by a team flow change in progress.
func (s *Store) RequestTurnInterrupt(ctx context.Context, request TurnInterruptRequest) (TurnInterruptTarget, error) {
	var out TurnInterruptTarget
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		out, err = tx.requestTurnInterrupt(request)
		return err
	})
	return out, err
}

func (tx *Tx) requestTurnInterrupt(request TurnInterruptRequest) (TurnInterruptTarget, error) {
	if request.Disposition != TurnInterruptContinue && request.Disposition != TurnInterruptRequeue || request.Purpose == "" || request.ActorID == "" {
		return TurnInterruptTarget{}, ErrInvalidTransition
	}
	out := TurnInterruptTarget{TurnID: request.TurnID}
	var projectID, roomID, exec, mode string
	var handoff, migrating, requested bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT t.agent_id,COALESCE(a.machine_id,''),t.generation,a.project_id,t.room_id,t.exec,t.mode,
		EXISTS(SELECT 1 FROM switch_handoff_turn ht WHERE ht.turn_id=t.id),
		EXISTS(SELECT 1 FROM team_flow_migration_agent ma JOIN team_flow_migration m ON m.id=ma.migration_id WHERE ma.agent_id=t.agent_id AND m.state IN ('draining','applying')),
		EXISTS(SELECT 1 FROM event e WHERE e.turn_id=t.id AND e.kind='turn_interrupt_requested')
		FROM turn t JOIN agent a ON a.id=t.agent_id WHERE t.id=?`, request.TurnID).
		Scan(&out.AgentID, &out.MachineID, &out.Generation, &projectID, &roomID, &exec, &mode, &handoff, &migrating, &requested)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnInterruptTarget{}, ErrInvalidTransition
	}
	if err != nil {
		return TurnInterruptTarget{}, err
	}
	if exec != "running" || mode != "normal" || handoff {
		return TurnInterruptTarget{}, ErrInvalidTransition
	}
	if request.Yield {
		if migrating {
			return TurnInterruptTarget{}, ErrInvalidTransition
		}
		if requested {
			return out, nil
		}
	}
	_, err = tx.AppendEvent(Event{Kind: eventTurnInterruptRequested, ProjectID: projectID, RoomID: roomID, AgentID: out.AgentID, TurnID: request.TurnID,
		PayloadJSON: payloadJSON(map[string]string{"actor": request.ActorID, "actor_kind": request.ActorKind, "purpose": request.Purpose, "disposition": request.Disposition})})
	return out, err
}

// turnInterruptDisposition is what the newest request for the turn asked for, or "" when none was made.
func (tx *Tx) turnInterruptDisposition(turnID string) (string, error) {
	var disposition string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT COALESCE(json_extract(payload_json,'$.disposition'),'') FROM event
		WHERE turn_id=? AND kind='turn_interrupt_requested' ORDER BY seq DESC LIMIT 1`, turnID).Scan(&disposition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return disposition, err
}

// requeueTurnNotes settles a turn that ended without an answer and whose request is to be tried again: its notes go back to queued, and
// when the turn had started working they carry a caution into their next delivery (the agent is told the previous attempt was interrupted
// and to check the state first). The agent is left idle and unblocked. A provider limit, and a requested interrupt that requeues, end this way.
func (tx *Tx) requeueTurnNotes(turnID, agentID, outcome string) error {
	if _, err := tx.Exec(`UPDATE turn SET outcome=?,settle='settled' WHERE id=?`, outcome, turnID); err != nil {
		return err
	}
	var uncertain bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM event WHERE turn_id=? AND kind='supervisor_activity'
		AND COALESCE(json_extract(payload_json,'$.summary'),'') NOT IN ('turn.started','system','rate_limit_event',''))`, turnID).Scan(&uncertain); err != nil {
		return err
	}
	if uncertain {
		if _, err := tx.Exec(`INSERT INTO provider_limit_retry(note_id,source_turn_id,caution)
			SELECT note_id,?,1 FROM turn_note WHERE turn_id=?
			ON CONFLICT(note_id) DO UPDATE SET source_turn_id=excluded.source_turn_id,caution=1`, turnID, turnID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE note SET state='queued' WHERE id IN (SELECT note_id FROM turn_note WHERE turn_id=?) AND state IN ('dispatched','in_turn')`, turnID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE agent SET observed='idle' WHERE id=? AND observed='in_turn'`, agentID)
	return err
}
