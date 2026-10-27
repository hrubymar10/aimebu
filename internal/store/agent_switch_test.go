package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAgentSwitchWaitsForTurnAndHandsOffRoomLocalContext(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_switch','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_switch','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	secretRoom, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "private", CreatedBy: "usr_switch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, secretRoom.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: secretRoom.ID, AuthorKind: "user", AuthorID: "usr_switch", Body: "private secret 123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_switch", Body: "finish the feature"}); err != nil {
		t.Fatal(err)
	}
	turn, err := s.RecordTurn(ctx, Turn{AgentID: agent.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	var requested AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		requested, err = tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if requested.Switch == nil || requested.Switch.State != "draining" || requested.Desired != "not_accepting" {
		t.Fatalf("request %+v", requested)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(agent.ID, "usr_switch", "active", requested.DesiredRev)
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("manual control during switch: %v", err)
	}
	controls, err := s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 0 {
		t.Fatalf("premature switch: %+v %v", controls, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled',ended_at=? WHERE id=?`, timestamp(), turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("recovery bypass: %+v %v", controls, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=0 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	controls, err = s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 {
		t.Fatalf("switch activation: %+v %v", controls, err)
	}
	card := controls[0]
	if card.ID != agent.ID || card.Generation != 1 || card.Tool != "claude" || card.Model != "haiku" || card.Switch == nil || card.Switch.State != "starting" {
		t.Fatalf("new generation %+v", card)
	}
	var ended string
	if err := s.db.QueryRow(`SELECT ended_at FROM session WHERE agent_id=? AND generation=0`, agent.ID).Scan(&ended); err != nil || ended == "" {
		t.Fatalf("old session end %q %v", ended, err)
	}
	messages, err := s.ListMessagesAfter(ctx, room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var handoff Message
	for _, message := range messages {
		if strings.Contains(message.Body, "Recent room context:") {
			handoff = message
		}
	}
	if handoff.ID == "" || !strings.Contains(handoff.Body, "finish the feature") || strings.Contains(handoff.Body, "private secret 123") {
		t.Fatalf("handoff %+v", handoff)
	}
	if _, err := s.NoteForAgentMessage(ctx, agent.ID, handoff.ID); err != nil {
		t.Fatalf("handoff did not wake new generation: %v", err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("dispatched before spawn confirmation: ready=%t %v", ready, err)
	}
	if err := s.ConfirmSpawn(ctx, agent.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
		t.Fatal(err)
	}
	card, err = s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Switch != nil {
		t.Fatalf("completed switch %+v %v", card, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready {
		t.Fatalf("handoff not dispatched after spawn confirmation: ready=%t %v", ready, err)
	}
}

func TestAgentSwitchMovesMachineCheckoutAfterDrain(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_remote','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_remote','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_remote','remote','hash',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,'mch_remote','/remote/wake')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) VALUES (?,'mch_remote','usr_remote',?)`, project.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE role SET cap_singleton_writer=1 WHERE id='role_worker'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET writer_agent_id=?,writer_state='reserved' WHERE id='chk_wake'`, agent.ID); err != nil {
		t.Fatal(err)
	}
	// The agent continues a Claude Code session made under a link to its folder here; the spelling means nothing on another machine.
	if _, err := s.db.Exec(`UPDATE agent SET launch_path='/link/to/wake' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitchTo(agent.ID, "usr_remote", room.ID, "fake", "test", "test", "fake", "mch_remote", "/remote/wake", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	controls, err := s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].MachineID != "mch_remote" {
		t.Fatalf("move %+v %v", controls, err)
	}
	var machine, path, key string
	if err := s.db.QueryRow(`SELECT a.machine_id,c.canonical_path,a.key_hash FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agent.ID).Scan(&machine, &path, &key); err != nil {
		t.Fatal(err)
	}
	if machine != "mch_remote" || path != "/remote/wake" || key != "" {
		t.Fatalf("moved agent: %s %s %q", machine, path, key)
	}
	if launch, err := s.AgentLaunchPath(ctx, agent.ID); err != nil || launch != "" {
		t.Fatalf("the old checkout's spelling followed the agent to another machine: %q %v", launch, err)
	}
	var oldState, newState, newWriter string
	if err := s.db.QueryRow(`SELECT writer_state FROM checkout WHERE id='chk_wake'`).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT writer_state,writer_agent_id FROM checkout WHERE machine_id='mch_remote' AND canonical_path='/remote/wake'`).Scan(&newState, &newWriter); err != nil {
		t.Fatal(err)
	}
	if oldState != "none" || newState != "reserved" || newWriter != agent.ID {
		t.Fatalf("writer transfer: old=%s new=%s owner=%s", oldState, newState, newWriter)
	}
	messages, err := s.ListMessagesAfter(ctx, room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if strings.Contains(message.Body, "Files in the target checkout may differ") {
			found = true
		}
	}
	if !found {
		t.Fatal("handoff lacks file difference warning")
	}
}

func TestAgentSwitchEligibility(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_switch','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_switch','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	request := func(rev int64) error {
		return s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", rev)
			return err
		})
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := request(0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("recovery block: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=0 WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if err := request(0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("closing room: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='open' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := request(0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("stopped agent: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='not_accepting',observed='idle' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := request(0); err != nil {
		t.Fatalf("paused agent: %v", err)
	}
}

func switchReviewFixture(t *testing.T) (*Store, Project, Room, Agent) {
	t.Helper()
	s, project, room, agent, _ := wakeFixture(t)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_switch','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_switch','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	return s, project, room, agent
}

func TestFailedSwitchStopReactivateAndSameTargetRetry(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"stop_reactivate", "same_target"} {
		t.Run(recovery, func(t *testing.T) {
			s, _, room, agent := switchReviewFixture(t)
			ctx := context.Background()
			if err := s.Tx(ctx, func(tx *Tx) error {
				_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", 0)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			controls, err := s.AdvanceAgentSwitches(ctx)
			if err != nil || len(controls) != 1 || controls[0].Generation != 1 {
				t.Fatalf("starting %+v %v", controls, err)
			}
			if err := s.FailSpawn(ctx, agent.ID, 1, "adapter unavailable"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
				t.Fatal(err)
			}
			failed, err := s.AgentCardByID(ctx, agent.ID)
			if err != nil || failed.Switch == nil || failed.Switch.State != "failed" {
				t.Fatalf("failed %+v %v", failed, err)
			}
			if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
				t.Fatalf("failed switch dispatched: ready=%t %v", ready, err)
			}
			if recovery == "same_target" {
				if err := s.Tx(ctx, func(tx *Tx) error {
					_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", failed.DesiredRev)
					return err
				}); err != nil {
					t.Fatalf("same target retry: %v", err)
				}
				controls, err := s.AdvanceAgentSwitches(ctx)
				if err != nil || len(controls) != 1 || controls[0].Generation != 2 {
					t.Fatalf("retry generation %+v %v", controls, err)
				}
				if err := s.ConfirmSpawn(ctx, agent.ID, 2); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				var stopped AgentCard
				if err := s.Tx(ctx, func(tx *Tx) error {
					var err error
					stopped, err = tx.SetAgentDesired(agent.ID, "usr_switch", "stopped", failed.DesiredRev)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				stopped, err = s.AgentCardByID(ctx, agent.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stopped.Switch != nil {
					t.Fatalf("stop kept failed switch %+v", stopped)
				}
				if err := s.Tx(ctx, func(tx *Tx) error {
					_, err := tx.SetAgentDesired(agent.ID, "usr_switch", "active", stopped.DesiredRev)
					return err
				}); err != nil {
					t.Fatalf("reactivate: %v", err)
				}
			}
			card, err := s.AgentCardByID(ctx, agent.ID)
			if err != nil || card.Switch != nil || card.Desired != "active" || card.Generation != 2 {
				t.Fatalf("recovered %+v %v", card, err)
			}
			if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready {
				t.Fatalf("dispatch did not resume: ready=%t %v", ready, err)
			}
		})
	}
}

func TestFailedSwitchFromRoomCloseCanReactivate(t *testing.T) {
	t.Parallel()
	s, project, room, agent := switchReviewFixture(t)
	ctx := context.Background()
	other, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "still open", CreatedBy: "usr_switch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, other.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: other.ID, AuthorKind: "user", AuthorID: "usr_switch", Body: "Continue here"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closing' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || failed.Switch == nil || failed.Switch.State != "failed" || failed.Desired != "not_accepting" {
		t.Fatalf("room-close failure %+v %v", failed, err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(agent.ID, "usr_switch", "active", failed.DesiredRev)
		return err
	}); err != nil {
		t.Fatalf("reactivate after room close: %v", err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Switch != nil || card.Desired != "active" {
		t.Fatalf("recovered %+v %v", card, err)
	}
	if delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || !ready || delivery.Notes[0].RoomID != other.ID {
		t.Fatalf("other room did not dispatch: %+v ready=%t %v", delivery, ready, err)
	}
}

func TestPausedSwitchStaysPausedAndHandoffIsValidUTF8(t *testing.T) {
	t.Parallel()
	s, _, room, agent := switchReviewFixture(t)
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_switch", Body: strings.Repeat("a", 499) + "é" + "tail"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(agent.ID, "usr_switch", "not_accepting", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	controls, err := s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].Desired != "active" {
		t.Fatalf("spawn phase %+v %v", controls, err)
	}
	if err := s.ConfirmSpawn(ctx, agent.ID, 1); err != nil {
		t.Fatal(err)
	}
	controls, err = s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].Desired != "not_accepting" {
		t.Fatalf("pause restore %+v %v", controls, err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Desired != "not_accepting" || card.Switch != nil {
		t.Fatalf("paused final card %+v %v", card, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("paused agent dispatched: ready=%t %v", ready, err)
	}
	messages, err := s.ListMessagesAfter(ctx, room.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var handoff string
	for _, message := range messages {
		if strings.Contains(message.Body, "Recent room context:") {
			handoff = message.Body
		}
	}
	if !utf8.ValidString(handoff) || !strings.Contains(handoff, strings.Repeat("a", 499)+"…") {
		t.Fatalf("invalid handoff truncation %q", handoff)
	}
}

func TestPausedSwitchSpawnFailureRestoresPause(t *testing.T) {
	t.Parallel()
	s, _, room, agent := switchReviewFixture(t)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(agent.ID, "usr_switch", "not_accepting", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitch(agent.ID, "usr_switch", room.ID, "claude", "native", "haiku", "anthropic", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.FailSpawn(ctx, agent.ID, 1, "adapter unavailable"); err != nil {
		t.Fatal(err)
	}
	controls, err := s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].Desired != "not_accepting" || controls[0].Switch == nil || controls[0].Switch.State != "failed" {
		t.Fatalf("paused failure %+v %v", controls, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("failed paused agent dispatched: ready=%t %v", ready, err)
	}
}
