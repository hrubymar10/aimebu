package store

import (
	"context"
	"database/sql"
	"errors"
)

// AgentWorktree records an agent-specific Git worktree, used when a session is explicitly copied.
// Name is its directory and branch suffix under .aimebu/worktrees (see AgentWorktreeName).
type AgentWorktree struct {
	AgentID    string `json:"agent_id"`
	RoomID     string `json:"room_id"`
	MachineID  string `json:"machine_id"`
	Name       string `json:"name"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
	BaseRef    string `json:"base_ref,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`
	DirtyBase  int    `json:"dirty_base_count,omitempty"`
	State      string `json:"state"`
	// ProjectID is the project the worktree is made in: the room it is named after may not exist yet (an agent that starts in a new room), so a
	// clean-up cannot always find the project through RoomID.
	ProjectID string `json:"-"`
	// Fresh says the worktree was made for this request, not found already there: a request that fails after making it removes it again.
	Fresh bool `json:"-"`
	// Unlock releases the per-name lock agentWorktreeFor took, which is held through the transaction that records the worktree (and through a
	// failed request's clean-up), so that no other request removes or takes away what this one is about to use. Done calls it once.
	Unlock func() `json:"-"`
}

// Done releases the worktree's lock, if it holds one.
func (w *AgentWorktree) Done() {
	if w != nil && w.Unlock != nil {
		w.Unlock()
		w.Unlock = nil
	}
}

// AgentWorktreeName is the worktree's name for an agent of a room. It is flat (room-slug, not room/slug) because the room's own branch,
// aimebu/<room>, already exists and Git cannot keep a branch under another branch's name.
func AgentWorktreeName(roomID, slug string) string { return roomID + "-" + slug }

// ProjectWorktreeMode is "auto" or "off", the project's setting for Git worktrees (auto when it has never been set).
func (s *Store) ProjectWorktreeMode(ctx context.Context, projectID string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT worktree_mode FROM settings_project WHERE project_id=?`, projectID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "auto", nil
	}
	return mode, err
}

// RoomBaseRef is the base ref the room's own worktree was made from, or "" when the room has none: an agent's worktree starts from the same
// place, or from the project's current HEAD.
func (s *Store) RoomBaseRef(ctx context.Context, roomID string) (string, error) {
	var ref string
	err := s.db.QueryRowContext(ctx, `SELECT base_ref FROM room_worktree WHERE room_id=?`, roomID).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ref, err
}

// AgentWorktreeOf is the agent's worktree, if it has one.
func (s *Store) AgentWorktreeOf(ctx context.Context, agentID string) (AgentWorktree, bool, error) {
	var w AgentWorktree
	err := s.db.QueryRowContext(ctx, `SELECT agent_id,room_id,machine_id,name,branch_name,canonical_path,base_ref,base_commit,dirty_base_count,state
		FROM agent_worktree WHERE agent_id=?`, agentID).Scan(&w.AgentID, &w.RoomID, &w.MachineID, &w.Name, &w.Branch, &w.Path, &w.BaseRef, &w.BaseCommit, &w.DirtyBase, &w.State)
	if errors.Is(err, sql.ErrNoRows) {
		return w, false, nil
	}
	return w, err == nil, err
}

// RecordAgentWorktree makes the worktree the agent's checkout. The checkout of the path must belong to the agent's project.
func (tx *Tx) RecordAgentWorktree(w AgentWorktree) error {
	projectID, err := tx.pointAgentAtCheckout(w.AgentID, w.MachineID, w.Path)
	if err != nil {
		return err
	}
	now := timestamp()
	_, err = tx.Exec(`INSERT INTO agent_worktree(agent_id,room_id,machine_id,name,branch_name,canonical_path,base_ref,base_commit,dirty_base_count,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,'ready',?,?)
		ON CONFLICT(agent_id) DO UPDATE SET room_id=excluded.room_id,machine_id=excluded.machine_id,name=excluded.name,branch_name=excluded.branch_name,
			canonical_path=excluded.canonical_path,base_ref=excluded.base_ref,base_commit=excluded.base_commit,dirty_base_count=excluded.dirty_base_count,state='ready',updated_at=excluded.updated_at`,
		w.AgentID, w.RoomID, w.MachineID, w.Name, w.Branch, w.Path, w.BaseRef, w.BaseCommit, w.DirtyBase, now, now)
	if err != nil {
		return err
	}
	_, err = tx.AppendEvent(Event{Kind: "agent_worktree_recorded", ProjectID: projectID, RoomID: w.RoomID, AgentID: w.AgentID,
		PayloadJSON: payloadJSON(map[string]string{"agent_id": w.AgentID, "path": w.Path, "branch": w.Branch})})
	return err
}

// SetAgentWorktreeState records that an agent's worktree was removed (or kept, because it holds work) by a person.
func (s *Store) SetAgentWorktreeState(ctx context.Context, agentID, state string) error {
	if state != "ready" && state != "preserved" && state != "removed" {
		return ErrInvalidTransition
	}
	return s.Tx(ctx, func(tx *Tx) error {
		res, err := tx.Exec(`UPDATE agent_worktree SET state=?,updated_at=? WHERE agent_id=?`, state, timestamp(), agentID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

// pointAgentAtCheckout moves the agent to the checkout of the path (creating it when needed) and returns the project.
// The checkout of the path must belong to that project.
func (tx *Tx) pointAgentAtCheckout(agentID, machineID, path string) (string, error) {
	var projectID, oldCheckout string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,checkout_id FROM agent WHERE id=?`, agentID).Scan(&projectID, &oldCheckout); err != nil {
		return "", err
	}
	var checkoutID, checkoutProject string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,project_id FROM checkout WHERE machine_id=? AND canonical_path=?`, machineID, path).Scan(&checkoutID, &checkoutProject)
	if errors.Is(err, sql.ErrNoRows) {
		if checkoutID, err = newID("chk"); err != nil {
			return "", err
		}
		_, err = tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, checkoutID, projectID, machineID, path)
		checkoutProject = projectID
	}
	if err != nil {
		return "", err
	}
	if checkoutProject != projectID {
		return "", ErrScopeMismatch
	}
	if checkoutID != oldCheckout {
		if _, err := tx.Exec(`UPDATE agent SET checkout_id=?,launch_path='' WHERE id=?`, checkoutID, agentID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE checkout SET writer_state='none',writer_agent_id=NULL WHERE id=? AND writer_agent_id=?`, oldCheckout, agentID); err != nil {
			return "", err
		}
	}
	return projectID, nil
}

// DetachAgentWorktree records that a person removed an agent's worktree and sends the agent back to the checkout of path (its room's), so that
// no way of starting it ever goes to the folder that is gone.
func (s *Store) DetachAgentWorktree(ctx context.Context, agentID, machineID, path string) error {
	return s.Tx(ctx, func(tx *Tx) error {
		// An agent started while its folder was being taken away cannot be moved to another checkout.
		var desired string
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT desired FROM agent WHERE id=?`, agentID).Scan(&desired); err != nil {
			return err
		}
		if desired != "stopped" {
			return ErrConflict
		}
		if _, err := tx.pointAgentAtCheckout(agentID, machineID, path); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE agent_worktree SET state='removed',updated_at=? WHERE agent_id=?`, timestamp(), agentID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}
