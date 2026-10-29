package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// AgentSwitch is the target and progress of a durable, in-place adapter change.
type AgentSwitch struct {
	State     string `json:"state"`
	Tool      string `json:"tool"`
	Variant   string `json:"variant"`
	Model     string `json:"model"`
	MachineID string `json:"machine_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

var errSwitchTargetUnavailable = errors.New("target machine is no longer approved")

func (s *Store) agentSwitch(ctx context.Context, id string) (*AgentSwitch, error) {
	var item AgentSwitch
	err := s.readDB.QueryRowContext(ctx, `SELECT state,tool,variant,model,COALESCE(machine_id,''),error FROM agent_switch WHERE agent_id=?`, id).
		Scan(&item.State, &item.Tool, &item.Variant, &item.Model, &item.MachineID, &item.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &item, err
}

func (tx *Tx) BeginAgentSwitch(id, actorID, roomID, tool, variant, model, provider string, rev int64) (AgentCard, error) {
	return tx.BeginAgentSwitchTo(id, actorID, roomID, tool, variant, model, provider, "", "", rev)
}

func (tx *Tx) BeginAgentSwitchTo(id, actorID, roomID, tool, variant, model, provider, machineID, checkoutPath string, rev int64) (AgentCard, error) {
	return tx.BeginAgentSwitchToLauncher(id, actorID, roomID, tool, variant, model, provider, machineID, checkoutPath, "", rev)
}

func (tx *Tx) BeginAgentSwitchToLauncher(id, actorID, roomID, tool, variant, model, provider, machineID, checkoutPath, launcherID string, rev int64) (AgentCard, error) {
	var projectID, desired, currentTool, currentVariant, currentModel string
	var currentMachine string
	var oldRev int64
	var revoked, blocked bool
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired,desired_rev,tool,variant,model,machine_id,revoked_at IS NOT NULL,recovery_block FROM agent WHERE id=?`, id).
		Scan(&projectID, &desired, &oldRev, &currentTool, &currentVariant, &currentModel, &currentMachine, &revoked, &blocked)
	if err != nil {
		return AgentCard{}, err
	}
	if err := tx.authorizeSpawnActor(projectID, "user", actorID, false); err != nil {
		return AgentCard{}, err
	}
	if rev != oldRev {
		return AgentCard{}, ErrStaleRev
	}
	if revoked || blocked || desired != "active" && desired != "not_accepting" {
		return AgentCard{}, ErrInvalidTransition
	}
	if machineID == "" {
		machineID = currentMachine
	}
	if machineID != currentMachine && checkoutPath == "" {
		return AgentCard{}, ErrInvalidTransition
	}
	var open bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room r JOIN room_member m ON m.room_id=r.id
		WHERE r.id=? AND r.project_id=? AND r.lifecycle='open' AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL)`, roomID, projectID, id).Scan(&open); err != nil {
		return AgentCard{}, err
	}
	if !open {
		return AgentCard{}, ErrInvalidTransition
	}
	var prior string
	err = tx.tx.QueryRowContext(tx.ctx, `SELECT state FROM agent_switch WHERE agent_id=?`, id).Scan(&prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentCard{}, err
	}
	if err == nil && prior != "failed" {
		return AgentCard{}, ErrInvalidTransition
	}
	if currentTool == tool && currentVariant == variant && currentModel == model && machineID == currentMachine && prior != "failed" {
		return AgentCard{}, ErrInvalidTransition
	}
	if prior == "failed" {
		if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, id); err != nil {
			return AgentCard{}, err
		}
	}
	path := checkoutPath
	if path == "" {
		if err := tx.tx.QueryRowContext(tx.ctx, `SELECT c.canonical_path FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, id).Scan(&path); err != nil {
			return AgentCard{}, err
		}
	}
	allowed, err := tx.boundaryAllowed(projectID, id, machineID, path)
	if err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.Exec(`INSERT INTO agent_switch(agent_id,room_id,tool,variant,launcher_id,model,provider,machine_id,checkout_path,state,requested_at) VALUES (?,?,?,?,?,?,?,?,?,'draining',?)`, id, roomID, tool, variant, nullableString(launcherID), model, provider, machineID, nullableString(checkoutPath), timestamp()); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, id); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.AppendEvent(Event{Kind: "agent_switch_requested", ProjectID: projectID, AgentID: id,
		PayloadJSON: payloadJSON(map[string]string{"tool": tool, "variant": variant, "model": model, "machine_id": machineID, "actor_id": actorID, "room_id": roomID, "resume_desired": desired})}); err != nil {
		return AgentCard{}, err
	}
	if !allowed {
		if _, err := tx.createBoundaryPending(projectID, roomID, id, machineID, path, "switch", "user", actorID); err != nil {
			return AgentCard{}, err
		}
	}
	return tx.agentCardByID(id)
}

// AdvanceAgentSwitches finishes clean turns before replacing their native
// session. It is safe to repeat after a server restart.
func (s *Store) AdvanceAgentSwitches(ctx context.Context) ([]AgentCard, error) {
	controls := []AgentCard{}
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.tx.QueryContext(tx.ctx, `SELECT agent_id,room_id,state,tool,variant,COALESCE(launcher_id,''),model,provider,COALESCE(machine_id,''),COALESCE(checkout_path,'') FROM agent_switch WHERE state IN ('draining','starting') AND NOT EXISTS(SELECT 1 FROM boundary_pending b JOIN inbox_item i ON i.id=b.inbox_id WHERE b.agent_id=agent_switch.agent_id AND i.state='open') ORDER BY requested_at,agent_id`)
		if err != nil {
			return err
		}
		type pending struct{ id, roomID, state, tool, variant, launcherID, model, provider, machineID, checkoutPath string }
		items := []pending{}
		for rows.Next() {
			var item pending
			if err := rows.Scan(&item.id, &item.roomID, &item.state, &item.tool, &item.variant, &item.launcherID, &item.model, &item.provider, &item.machineID, &item.checkoutPath); err != nil {
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
		for _, item := range items {
			var projectID, desired, observed, currentTool string
			var generation int
			var blocked bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired,observed,generation,tool,recovery_block FROM agent WHERE id=?`, item.id).
				Scan(&projectID, &desired, &observed, &generation, &currentTool, &blocked); err != nil {
				return err
			}
			if item.state == "draining" && desired != "stopped" {
				machineID, path := item.machineID, item.checkoutPath
				if machineID == "" || path == "" {
					var currentMachine, currentPath string
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.machine_id,c.canonical_path FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, item.id).
						Scan(&currentMachine, &currentPath); err != nil {
						return err
					}
					if machineID == "" {
						machineID = currentMachine
					}
					if path == "" {
						path = currentPath
					}
				}
				allowed, err := tx.boundaryAllowed(projectID, item.id, machineID, path)
				if err != nil {
					return err
				}
				if !allowed {
					var requester string
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT created_by FROM agent WHERE id=?`, item.id).Scan(&requester); err != nil {
						return err
					}
					if _, err := tx.createBoundaryPending(projectID, item.roomID, item.id, machineID, path, "switch", "user", requester); err != nil {
						return err
					}
					continue
				}
			}
			if item.state == "starting" {
				resumeDesired, err := tx.switchResumeDesired(item.id)
				if err != nil {
					return err
				}
				var confirmed, failed bool
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT
					EXISTS(SELECT 1 FROM event WHERE agent_id=? AND kind='agent_spawn_confirmed' AND json_extract(payload_json,'$.generation')=?),
					EXISTS(SELECT 1 FROM event WHERE agent_id=? AND kind='agent_spawn_failed' AND json_extract(payload_json,'$.generation')=?)`, item.id, generation, item.id, generation).Scan(&confirmed, &failed); err != nil {
					return err
				}
				if failed {
					if resumeDesired == "not_accepting" {
						if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, item.id); err != nil {
							return err
						}
						if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: item.id,
							PayloadJSON: payloadJSON(map[string]string{"desired": "not_accepting", "actor_kind": "system", "actor_id": "agent_switch"})}); err != nil {
							return err
						}
					}
					if _, err := tx.Exec(`UPDATE agent_switch SET state='failed',error='Supervisor rejected the new adapter.' WHERE agent_id=?`, item.id); err != nil {
						return err
					}
					if _, err := tx.AppendEvent(Event{Kind: "agent_switch_failed", ProjectID: projectID, AgentID: item.id, PayloadJSON: "{}"}); err != nil {
						return err
					}
					if resumeDesired == "not_accepting" {
						card, err := tx.agentCardByID(item.id)
						if err != nil {
							return err
						}
						controls = append(controls, card)
					}
				} else if confirmed {
					if resumeDesired == "not_accepting" {
						if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, item.id); err != nil {
							return err
						}
						if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: item.id,
							PayloadJSON: payloadJSON(map[string]string{"desired": "not_accepting", "actor_kind": "system", "actor_id": "agent_switch"})}); err != nil {
							return err
						}
					}
					if _, err := tx.Exec(`DELETE FROM agent_switch WHERE agent_id=?`, item.id); err != nil {
						return err
					}
					if _, err := tx.AppendEvent(Event{Kind: "agent_switch_completed", ProjectID: projectID, AgentID: item.id, PayloadJSON: "{}"}); err != nil {
						return err
					}
					if resumeDesired == "not_accepting" {
						card, err := tx.agentCardByID(item.id)
						if err != nil {
							return err
						}
						controls = append(controls, card)
					}
				}
				continue
			}
			if desired != "not_accepting" || observed == "lost" || blocked {
				continue
			}
			var roomOpen bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM room r JOIN room_member m ON m.room_id=r.id
				WHERE r.id=? AND r.lifecycle='open' AND m.member_kind='agent' AND m.member_id=? AND m.left_at IS NULL)`, item.roomID, item.id).Scan(&roomOpen); err != nil {
				return err
			}
			if !roomOpen {
				if _, err := tx.Exec(`UPDATE agent_switch SET state='failed',error='Room closed during the switch.' WHERE agent_id=?`, item.id); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(Event{Kind: "agent_switch_failed", ProjectID: projectID, RoomID: item.roomID, AgentID: item.id, PayloadJSON: "{}"}); err != nil {
					return err
				}
				continue
			}
			var open int
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND
				(t.exec IN ('pending','running') OR t.settle='pending' OR (t.exec='lost' AND NOT EXISTS
				(SELECT 1 FROM event e WHERE e.kind='recovery_override' AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))))`, item.id).Scan(&open); err != nil {
				return err
			}
			if open != 0 {
				continue
			}
			moved := false
			if item.machineID != "" {
				var currentMachine string
				if err := tx.tx.QueryRowContext(tx.ctx, `SELECT machine_id FROM agent WHERE id=?`, item.id).Scan(&currentMachine); err != nil {
					return err
				}
				if item.machineID != currentMachine {
					if err := tx.moveSwitchCheckout(item.id, projectID, item.machineID, item.checkoutPath); err != nil {
						if errors.Is(err, ErrWriterOccupied) || errors.Is(err, ErrScopeMismatch) || errors.Is(err, ErrInvalidTransition) || errors.Is(err, errSwitchTargetUnavailable) {
							if _, updateErr := tx.Exec(`UPDATE agent_switch SET state='failed',error=? WHERE agent_id=?`, err.Error(), item.id); updateErr != nil {
								return updateErr
							}
							if _, eventErr := tx.AppendEvent(Event{Kind: "agent_switch_failed", ProjectID: projectID, RoomID: item.roomID, AgentID: item.id, PayloadJSON: "{}"}); eventErr != nil {
								return eventErr
							}
							continue
						}
						return err
					}
					moved = true
				}
			}
			if _, err := tx.Exec(`UPDATE session SET ended_at=? WHERE agent_id=? AND generation=? AND ended_at IS NULL`, timestamp(), item.id, generation); err != nil {
				return err
			}
			sessionID, err := newID("ses")
			if err != nil {
				return err
			}
			nativeID, err := nativeSessionID(item.tool)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at,native_session_id) VALUES (?,?,?,'fresh_with_history',?,?)`, sessionID, item.id, generation+1, timestamp(), nullableString(nativeID)); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent SET tool=?,variant=?,launcher_id=?,model=?,provider=?,desired='active',desired_rev=desired_rev+1,generation=generation+1,observed='idle' WHERE id=?`, item.tool, item.variant, nullableString(item.launcherID), item.model, item.provider, item.id); err != nil {
				return err
			}
			if err := tx.clearProviderLimit(item.id); err != nil {
				return err
			}
			if err := tx.insertSwitchHandoff(item.id, item.roomID, currentTool, item.tool, moved); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE agent_switch SET state='starting' WHERE agent_id=?`, item.id); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: projectID, AgentID: item.id,
				PayloadJSON: fmt.Sprintf(`{"generation":%d}`, generation+1)}); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: item.id,
				PayloadJSON: payloadJSON(map[string]string{"desired": "active", "actor_kind": "system", "actor_id": "agent_switch"})}); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(Event{Kind: "agent_switch_starting", ProjectID: projectID, AgentID: item.id, PayloadJSON: "{}"}); err != nil {
				return err
			}
			card, err := tx.agentCardByID(item.id)
			if err != nil {
				return err
			}
			controls = append(controls, card)
		}
		return nil
	})
	return controls, err
}

// The handoff is room-local: content from other joined rooms must never be
// copied into this room. The addressed system message creates a queued note.
func (tx *Tx) insertSwitchHandoff(agentID, roomID, oldTool, newTool string, moved bool) error {
	rows, err := tx.tx.QueryContext(tx.ctx, `SELECT author_id,body FROM message WHERE room_id=? ORDER BY seq DESC LIMIT 8`, roomID)
	if err != nil {
		return err
	}
	lines := []string{}
	for rows.Next() {
		var author, body string
		if err := rows.Scan(&author, &body); err != nil {
			rows.Close()
			return err
		}
		if len(body) > 500 {
			cut := 500
			for !utf8.RuneStart(body[cut]) {
				cut--
			}
			body = body[:cut] + "…"
		}
		lines = append(lines, author+": "+strings.ReplaceAll(body, "\n", " "))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	body := "@" + agentID + " Continue the work in this room after the model/tool switch. The previous " + oldTool + " session ended cleanly; this " + newTool + " session starts fresh. Recent room context:\n" + strings.Join(lines, "\n")
	if moved {
		body += "\nThis agent moved to another machine. Files in the target checkout may differ from the previous machine."
	}
	_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body, OnlyTargets: []string{agentID}})
	return err
}

func (tx *Tx) moveSwitchCheckout(agentID, projectID, machineID, path string) error {
	if path == "" {
		return ErrInvalidTransition
	}
	var approved, revoked bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT approved,revoked_at IS NOT NULL FROM machine WHERE id=?`, machineID).Scan(&approved, &revoked); err != nil {
		return err
	}
	if !approved || revoked {
		return errSwitchTargetUnavailable
	}
	var oldCheckout string
	var writer bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT a.checkout_id,r.cap_singleton_writer FROM agent a JOIN role r ON r.id=a.role_id WHERE a.id=?`, agentID).Scan(&oldCheckout, &writer); err != nil {
		return err
	}
	var targetID, state, targetProject string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id,writer_state,project_id FROM checkout WHERE machine_id=? AND canonical_path=?`, machineID, path).Scan(&targetID, &state, &targetProject)
	if errors.Is(err, sql.ErrNoRows) {
		targetID, err = newID("chk")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, targetID, projectID, machineID, path); err != nil {
			return err
		}
		state = "none"
		targetProject = projectID
	} else if err != nil {
		return err
	}
	if targetProject != projectID {
		return ErrScopeMismatch
	}
	if writer && state != "none" {
		return ErrWriterOccupied
	}
	if writer {
		if _, err := tx.Exec(`UPDATE checkout SET writer_agent_id=NULL,writer_state='none' WHERE id=? AND writer_agent_id=?`, oldCheckout, agentID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id=?`, agentID, targetID); err != nil {
			return err
		}
	}
	// A spelling of the old checkout's folder (agent.launch_path) means nothing in another checkout, which may be on another machine.
	_, err = tx.Exec(`UPDATE agent SET machine_id=?,checkout_id=?,key_hash='',launch_path=CASE WHEN checkout_id=? THEN launch_path ELSE '' END WHERE id=?`, machineID, targetID, targetID, agentID)
	return err
}

func (tx *Tx) switchResumeDesired(agentID string) (string, error) {
	var value sql.NullString
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT json_extract(payload_json,'$.resume_desired') FROM event
		WHERE agent_id=? AND kind='agent_switch_requested' ORDER BY seq DESC LIMIT 1`, agentID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "active", nil // Switches requested before this field was recorded.
	}
	if err != nil {
		return "", err
	}
	if value.String == "not_accepting" {
		return "not_accepting", nil
	}
	return "active", nil
}
