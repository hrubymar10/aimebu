package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
)

// Keep drain, switch, and migration stall checks aligned for recovered lost turns.
const openFlowTurnPredicate = `(t.exec IN ('pending','running') OR t.settle='pending' OR
	(t.exec='lost' AND NOT EXISTS(SELECT 1 FROM event e WHERE e.kind='recovery_override'
	AND (e.turn_id=t.id OR e.turn_id=t.parent_turn_id))))`

// AgentSwitch is the target and progress of a durable, in-place adapter change.
type AgentSwitch struct {
	State           string          `json:"state"`
	Tool            string          `json:"tool"`
	Variant         string          `json:"variant"`
	Model           string          `json:"model"`
	MachineID       string          `json:"machine_id,omitempty"`
	Error           string          `json:"error,omitempty"`
	HandoffStatus   string          `json:"handoff_status,omitempty"`
	HandoffSource   string          `json:"handoff_source,omitempty"`
	Handoff         json.RawMessage `json:"handoff,omitempty"`
	HandoffDeadline string          `json:"handoff_deadline,omitempty"`
	OverallDeadline string          `json:"overall_deadline,omitempty"`
	CanCancel       bool            `json:"can_cancel"`
	CanRetry        bool            `json:"can_retry"`
	CanContinue     bool            `json:"can_continue"`
}

func (item *AgentSwitch) setContinuityActions(turnOpen bool) {
	item.CanCancel = item.State == "draining" && (item.HandoffStatus == "waiting" || item.HandoffStatus == "pending" || item.HandoffStatus == "handing_off" || item.HandoffStatus == "ready" || item.HandoffStatus == "automatic")
	item.CanContinue = item.CanCancel
	item.CanRetry = item.State == "draining" && item.HandoffStatus == "automatic" && !turnOpen
}

var errSwitchTargetUnavailable = errors.New("target machine is no longer approved")

func (s *Store) agentSwitch(ctx context.Context, id string) (*AgentSwitch, error) {
	var item AgentSwitch
	err := s.readDB.QueryRowContext(ctx, `SELECT state,tool,variant,model,COALESCE(machine_id,''),error FROM agent_switch WHERE agent_id=?`, id).
		Scan(&item.State, &item.Tool, &item.Variant, &item.Model, &item.MachineID, &item.Error)
	if errors.Is(err, sql.ErrNoRows) {
		var status string
		if err := s.readDB.QueryRowContext(ctx, `SELECT status FROM switch_handoff WHERE agent_id=?`, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		} else if err != nil {
			return nil, err
		} else if status != "started" {
			return nil, nil
		}
		if err := s.readDB.QueryRowContext(ctx, `SELECT tool,variant,model,machine_id FROM agent WHERE id=?`, id).Scan(&item.Tool, &item.Variant, &item.Model, &item.MachineID); err != nil {
			return nil, err
		}
		item.State = "started"
	} else if err != nil {
		return nil, err
	}
	var body, deadline, overall sql.NullString
	var turnOpen bool
	err = s.readDB.QueryRowContext(ctx, `SELECT status,source,body_json,handoff_deadline,overall_deadline,
		CASE WHEN turn_id IS NULL THEN 0 ELSE EXISTS(SELECT 1 FROM turn WHERE id=switch_handoff.turn_id AND (exec IN ('pending','running') OR settle='pending')) END
		FROM switch_handoff WHERE agent_id=?`, id).
		Scan(&item.HandoffStatus, &item.HandoffSource, &body, &deadline, &overall, &turnOpen)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if body.Valid && body.String != "" {
		item.Handoff = json.RawMessage(body.String)
	}
	item.HandoffDeadline, item.OverallDeadline = deadline.String, overall.String
	item.setContinuityActions(turnOpen)
	return &item, nil
}

// TrustSwitchActor makes the transaction's harness switches skip the owner/operator membership check. Only the server's own continuation of
// an already authorised decision (a pool respawn) may call it; the person's switch route never does.
func (tx *Tx) TrustSwitchActor() { tx.switchTrusted = true }

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
	if !tx.switchTrusted {
		if err := tx.authorizeSpawnActor(projectID, "user", actorID, false); err != nil {
			return AgentCard{}, err
		}
	}
	if rev != oldRev {
		return AgentCard{}, ErrStaleRev
	}
	var clearPending bool
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT EXISTS(SELECT 1 FROM agent_context_clear WHERE agent_id=?)`, id).Scan(&clearPending); err != nil {
		return AgentCard{}, err
	}
	if clearPending {
		return AgentCard{}, ErrInvalidTransition
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
	// A switch is somebody's choice of harness and model, not the pool's: the agent is not flagged when a pool changes (a respawn on the
	// pool marks itself "pool" again once it has begun). An explicit switch also takes the agent off the respawn queue the pool change may
	// have queued it on, and brings the project's pool card back in line (the agent may have been its last affected one).
	if _, err := tx.Exec(`UPDATE agent SET pool_origin='explicit',pool_kept_sig=NULL WHERE id=? AND pool_origin IN ('pool','unknown')`, id); err != nil {
		return AgentCard{}, err
	}
	if _, err := tx.Exec(`DELETE FROM pool_respawn WHERE agent_id=?`, id); err != nil {
		return AgentCard{}, err
	}
	if err := tx.syncPoolChange(projectID); err != nil {
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
			var projectID, desired, observed, currentTool, currentModel string
			var generation int
			var blocked bool
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT project_id,desired,observed,generation,tool,model,recovery_block FROM agent WHERE id=?`, item.id).
				Scan(&projectID, &desired, &observed, &generation, &currentTool, &currentModel, &blocked); err != nil {
				return err
			}
			handoffStatus, handoffRoom, overallDeadline, handoffTurn, err := tx.switchHandoffStatus(item.id)
			if err != nil {
				return err
			}
			if item.state == "draining" && handoffStatus != "" {
				now := time.Now().UTC().Format(timestampLayout)
				switch handoffStatus {
				case "waiting":
					if now >= overallDeadline || desired != "active" || observed == "stopped" || observed == "lost" {
						if err := tx.switchAutomaticHandoff(item.id, handoffRoom, "Outgoing session unavailable or overall handoff deadline reached."); err != nil {
							return err
						}
						handoffStatus = "automatic"
					} else {
						var open int
						if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn WHERE agent_id=? AND (exec IN ('pending','running') OR settle='pending')`, item.id).Scan(&open); err != nil {
							return err
						}
						if open == 0 && observed == "idle" && !blocked {
							if err := tx.beginSwitchHandoffTurn(item.id, handoffRoom, generation); err != nil {
								return err
							}
						}
						continue
					}
				case "pending", "handing_off":
					var deadline sql.NullString
					if err := tx.tx.QueryRowContext(tx.ctx, `SELECT handoff_deadline FROM switch_handoff WHERE agent_id=?`, item.id).Scan(&deadline); err != nil {
						return err
					}
					if now >= overallDeadline || handoffStatus == "handing_off" && deadline.Valid && now >= deadline.String {
						if handoffTurn != "" {
							if _, err := tx.Exec(`UPDATE turn SET exec='killed',settle='settled',ended_at=? WHERE id=? AND exec='pending'`, timestamp(), handoffTurn); err != nil {
								return err
							}
						}
						if err := tx.switchAutomaticHandoff(item.id, handoffRoom, "Outgoing handoff turn timed out."); err != nil {
							return err
						}
						handoffStatus = "automatic"
					} else {
						continue
					}
				case "ready", "automatic":
				case "cancelled", "started":
					continue
				default:
					return ErrInvalidTransition
				}
				if handoffStatus == "ready" || handoffStatus == "automatic" {
					if desired != "not_accepting" {
						if _, err := tx.Exec(`UPDATE agent SET desired='not_accepting',desired_rev=desired_rev+1 WHERE id=?`, item.id); err != nil {
							return err
						}
						if _, err := tx.AppendEvent(Event{Kind: "agent_control_requested", ProjectID: projectID, AgentID: item.id,
							PayloadJSON: payloadJSON(map[string]string{"desired": "not_accepting", "actor_kind": "system", "actor_id": "agent_switch"})}); err != nil {
							return err
						}
						card, err := tx.agentCardByID(item.id)
						if err != nil {
							return err
						}
						controls = append(controls, card)
						continue
					}
				}
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
					if _, err := tx.Exec(`UPDATE switch_handoff SET status='started' WHERE agent_id=? AND status IN ('ready','automatic')`, item.id); err != nil {
						return err
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
			if err := tx.tx.QueryRowContext(tx.ctx, `SELECT COUNT(*) FROM turn t WHERE t.agent_id=? AND `+openFlowTurnPredicate, item.id).Scan(&open); err != nil {
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
						if errors.Is(err, ErrScopeMismatch) || errors.Is(err, ErrInvalidTransition) || errors.Is(err, errSwitchTargetUnavailable) {
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
			if _, err := tx.Exec(`UPDATE agent SET tool=?,variant=?,launcher_id=?,model=?,provider=?,desired='active',desired_rev=desired_rev+1,generation=generation+1,observed='idle',stopped_at=NULL WHERE id=?`, item.tool, item.variant, nullableString(item.launcherID), item.model, item.provider, item.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE switch_handoff SET successor_generation=? WHERE agent_id=?`, generation+1, item.id); err != nil {
				return err
			}
			if err := tx.clearProviderLimit(item.id); err != nil {
				return err
			}
			if err := tx.insertSwitchHandoff(item.id, item.roomID, currentTool, item.tool, currentModel, item.model, moved); err != nil {
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
func (tx *Tx) insertSwitchHandoff(agentID, roomID, oldTool, newTool, oldModel, newModel string, moved bool) error {
	var slug string
	if err := tx.tx.QueryRowContext(tx.ctx, `SELECT slug FROM agent WHERE id=?`, agentID).Scan(&slug); err != nil {
		return err
	}
	var source, raw string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT source,body_json FROM switch_handoff WHERE agent_id=? AND status IN ('ready','automatic')`, agentID).Scan(&source, &raw)
	if err == nil {
		var handoff SwitchHandoffBody
		if err := json.Unmarshal([]byte(raw), &handoff); err != nil {
			return err
		}
		body := "Switch handoff (" + source + "). The " + oldTool + " session ended; this " + newTool + " session starts fresh.\n" +
			formatSwitchHandoffBody(handoff)
		if moved {
			body += "\nMachine changed; verify files in the target checkout."
		}
		meta, err := switchHandoffMetadata(slug, oldTool, newTool, oldModel, newModel, source)
		if err != nil {
			return err
		}
		_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body, SwitchHandoffJSON: meta, OnlyTargets: []string{agentID}})
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
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
		// The summary quotes the stored bodies for the agent that takes over: mentions stay @agt_<id> (ids are the truth),
		// and the display rewrite is the human view's job.
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
	body := "Continue the work in this room after the model/tool switch. The previous " + oldTool + " session ended cleanly; this " + newTool + " session starts fresh. Recent room context:\n" + strings.Join(lines, "\n")
	if moved {
		body += "\nThis agent moved to another machine. Files in the target checkout may differ from the previous machine."
	}
	meta, err := switchHandoffMetadata(slug, oldTool, newTool, oldModel, newModel, "automatic")
	if err != nil {
		return err
	}
	_, err = tx.InsertMessage(Message{RoomID: roomID, AuthorKind: "system", AuthorID: "system", Body: body, SwitchHandoffJSON: meta, OnlyTargets: []string{agentID}})
	return err
}

func switchHandoffMetadata(slug, oldTool, newTool, oldModel, newModel, source string) (string, error) {
	if source == "agent" {
		source = "written"
	}
	encoded, err := json.Marshal(map[string]string{
		"agent_slug": slug, "old_tool": oldTool, "new_tool": newTool,
		"old_model": oldModel, "new_model": newModel, "source": source,
	})
	return string(encoded), err
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
	var targetID string
	err := tx.tx.QueryRowContext(tx.ctx, `SELECT id FROM checkout WHERE project_id=? AND machine_id=? AND canonical_path=?`, projectID, machineID, path).Scan(&targetID)
	if errors.Is(err, sql.ErrNoRows) {
		targetID, err = newID("chk")
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,?,?)`, targetID, projectID, machineID, path); err != nil {
			return err
		}
	} else if err != nil {
		return err
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
