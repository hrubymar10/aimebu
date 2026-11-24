package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrConflict = errors.New("conflict")
var ErrScopeMismatch = errors.New("scope_mismatch")

type Agent struct {
	ID         string
	Slug       string
	ProjectID  string
	RoleID     string
	Tool       string
	Variant    string
	LauncherID string
	Model      string
	Provider   string
	MachineID  string
	CheckoutID string
	KeyHash    string
	CreatedBy  string
	// ApprovedBy is the message of a person that approved this agent's exact
	// harness and model choice outside its role's default pool (an
	// agent-initiated spawn); empty for every other spawn.
	ApprovedBy string
}

func (s *Store) CreateAgent(ctx context.Context, agent Agent) (Agent, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		agent, err = tx.CreateAgent(agent)
		return err
	})
	return agent, err
}

func (tx *Tx) CreateAgent(agent Agent) (Agent, error) {
	var checkoutProject, checkoutMachine string
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT project_id,machine_id FROM checkout WHERE id=?`, agent.CheckoutID).
		Scan(&checkoutProject, &checkoutMachine); err != nil {
		return Agent{}, err
	}
	if checkoutProject != agent.ProjectID || checkoutMachine != agent.MachineID {
		return Agent{}, fmt.Errorf("%w: agent project/machine differs from checkout", ErrScopeMismatch)
	}
	var err error
	agent.ID, err = newID("agt")
	if err != nil {
		return Agent{}, err
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO agent
		(id, slug, project_id, role_id, tool, variant, launcher_id, model, provider,
		 machine_id, checkout_id, key_hash, created_by, created_at, approved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, agent.ID, agent.Slug,
		agent.ProjectID, agent.RoleID, agent.Tool, agent.Variant, nullableString(agent.LauncherID), agent.Model,
		agent.Provider, agent.MachineID, agent.CheckoutID, agent.KeyHash,
		agent.CreatedBy, timestamp(), nullableString(agent.ApprovedBy))
	if err != nil {
		return Agent{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "agent_created", ProjectID: agent.ProjectID,
		AgentID: agent.ID, PayloadJSON: payloadJSON(map[string]string{"agent_id": agent.ID})}); err != nil {
		return Agent{}, err
	}
	return agent, nil
}

type Turn struct {
	ID           string
	AgentID      string
	SessionID    string
	Generation   int
	RoomID       string
	ParentTurnID string
	Attempt      int
	Mode         string
	Exec         string
	PolicyJSON   string
}

func (s *Store) RecordTurn(ctx context.Context, turn Turn) (Turn, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		turn, err = tx.RecordTurn(turn)
		return err
	})
	return turn, err
}

func (tx *Tx) RecordTurn(turn Turn) (Turn, error) {
	var projectID, roomProject, sessionAgent string
	var sessionGeneration int
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT project_id FROM agent WHERE id=?`, turn.AgentID).Scan(&projectID); err != nil {
		return Turn{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT agent_id,generation FROM session WHERE id=?`, turn.SessionID).
		Scan(&sessionAgent, &sessionGeneration); err != nil {
		return Turn{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT project_id FROM room WHERE id=?`, turn.RoomID).Scan(&roomProject); err != nil {
		return Turn{}, err
	}
	if !roomServesAgent(roomProject, projectID) || sessionAgent != turn.AgentID || sessionGeneration != turn.Generation {
		return Turn{}, fmt.Errorf("%w: turn room or session differs from agent", ErrScopeMismatch)
	}
	var err error
	turn.ID, err = newID("turn")
	if err != nil {
		return Turn{}, err
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO turn
		(id, agent_id, session_id, generation, room_id, parent_turn_id,
		 attempt, mode, exec, policy_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.AgentID, turn.SessionID,
		turn.Generation, turn.RoomID, nullable(turn.ParentTurnID), turn.Attempt,
		turn.Mode, turn.Exec, nullable(turn.PolicyJSON))
	if err != nil {
		return Turn{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "turn_recorded", ProjectID: projectID,
		RoomID: turn.RoomID, AgentID: turn.AgentID, TurnID: turn.ID,
		PayloadJSON: payloadJSON(map[string]string{"turn_id": turn.ID})}); err != nil {
		return Turn{}, err
	}
	return turn, nil
}

type Note struct {
	ID         string
	AgentID    string
	MessageID  string
	RoomID     string
	State      string
	WakeReason string
}

func (s *Store) InsertNote(ctx context.Context, note Note) (Note, error) {
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		note, err = tx.InsertNote(note)
		return err
	})
	return note, err
}

func (s *Store) NoteForAgentMessage(ctx context.Context, agentID, messageID string) (Note, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return Note{}, err
	}
	var note Note
	err := s.db.QueryRowContext(ctx, `SELECT id,agent_id,message_id,room_id,state,wake_reason FROM note WHERE agent_id=? AND message_id=?`, agentID, messageID).
		Scan(&note.ID, &note.AgentID, &note.MessageID, &note.RoomID, &note.State, &note.WakeReason)
	return note, err
}

func (tx *Tx) InsertNote(note Note) (Note, error) {
	var messageRoom, roomProject, agentProject string
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT room_id FROM message WHERE id=?`, note.MessageID).Scan(&messageRoom); err != nil {
		return Note{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT project_id FROM room WHERE id=?`, note.RoomID).Scan(&roomProject); err != nil {
		return Note{}, err
	}
	if err := tx.tx.QueryRowContext(tx.ctx,
		`SELECT project_id FROM agent WHERE id=?`, note.AgentID).Scan(&agentProject); err != nil {
		return Note{}, err
	}
	if messageRoom != note.RoomID || !roomServesAgent(roomProject, agentProject) {
		return Note{}, fmt.Errorf("%w: note message or agent differs from room", ErrScopeMismatch)
	}
	var err error
	note.ID, err = newID("note")
	if err != nil {
		return Note{}, err
	}
	if note.State == "" {
		note.State = "queued"
	}
	_, err = tx.tx.ExecContext(tx.ctx, `INSERT INTO note
		(id, agent_id, message_id, room_id, state, wake_reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, note.ID, note.AgentID, note.MessageID,
		note.RoomID, note.State, note.WakeReason, timestamp())
	if err != nil {
		return Note{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "note_queued", ProjectID: agentProject,
		RoomID: note.RoomID, AgentID: note.AgentID,
		PayloadJSON: payloadJSON(map[string]string{"note_id": note.ID})}); err != nil {
		return Note{}, err
	}
	return note, nil
}

type RequestKey struct {
	ActorKind string
	ActorID   string
	Kind      string
	Key       string
	Digest    string
}

// RequestResult checks an idempotency key before resolving mutable resources.
func (s *Store) RequestResult(ctx context.Context, request RequestKey) (string, bool, error) {
	if err := s.checkTxReentry(ctx); err != nil {
		return "", false, err
	}
	var digest, result string
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,result_json FROM request_key
		WHERE actor_kind=? AND actor_id=? AND kind=? AND key=?`, request.ActorKind,
		request.ActorID, request.Kind, request.Key).Scan(&digest, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if digest != request.Digest {
		return "", false, ErrConflict
	}
	return result, true, nil
}

// WithRequestKey runs fn once for a request identity. It stores the result in
// the same transaction as fn's writes; replays return the original result.
func (s *Store) WithRequestKey(ctx context.Context, request RequestKey,
	fn func(*Tx) (string, error)) (result string, reused bool, err error) {
	err = s.Tx(ctx, func(tx *Tx) error {
		var storedDigest, storedResult string
		lookupErr := tx.tx.QueryRowContext(tx.ctx, `SELECT request_digest, result_json
			FROM request_key WHERE actor_kind=? AND actor_id=? AND kind=? AND key=?`,
			request.ActorKind, request.ActorID, request.Kind, request.Key).
			Scan(&storedDigest, &storedResult)
		if lookupErr == nil {
			if storedDigest != request.Digest {
				return ErrConflict
			}
			result, reused = storedResult, true
			return nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		var callErr error
		result, callErr = fn(tx)
		if callErr != nil {
			return callErr
		}
		_, callErr = tx.tx.ExecContext(tx.ctx, `INSERT INTO request_key
			(actor_kind, actor_id, kind, key, request_digest, result_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, request.ActorKind, request.ActorID,
			request.Kind, request.Key, request.Digest, result, timestamp())
		return callErr
	})
	if err != nil {
		return "", false, err
	}
	return result, reused, nil
}
