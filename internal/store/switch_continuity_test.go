package store

import (
	"context"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
)

func continuityFixture(t *testing.T, lead bool) (*Store, Project, Room, Agent) {
	t.Helper()
	s, project, room, worker, leader := wakeFixture(t)
	agent := worker
	if lead {
		agent = leader
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_switch_cont','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_switch_cont','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='idle' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	return s, project, room, agent
}

func beginContinuity(t *testing.T, s *Store, room Room, agent Agent) AgentCard {
	t.Helper()
	var card AgentCard
	err := s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		card, err = tx.BeginAgentSwitchWithContinuity(agent.ID, "usr_switch_cont", room.ID, "claude", "native", "haiku", "anthropic", "", "", "", 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return card
}

func TestSwitchContinuityHandoffAndBootstrap(t *testing.T) {
	for _, isLead := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker", true: "lead"}[isLead], func(t *testing.T) {
			s, project, room, agent := continuityFixture(t, isLead)
			ctx := context.Background()
			if isLead {
				state := "Prior handbook state"
				if _, err := s.UpdateHandbook(ctx, project.ID, agent.ID, "current_state", &state, nil, 0); err != nil {
					t.Fatal(err)
				}
			}
			card := beginContinuity(t, s, room, agent)
			if card.Switch == nil || card.Switch.HandoffStatus != "waiting" || card.Desired != "active" {
				t.Fatalf("requested %+v", card)
			}
			if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
				t.Fatalf("handoff start %+v %v", controls, err)
			}
			delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
			if err != nil || !ready || !strings.Contains(string(delivery.Context), "switch_handoff_request") {
				t.Fatalf("handoff delivery %t %v %s", ready, err, delivery.Context)
			}
			if len(delivery.Notes) != 1 || delivery.Notes[0].RoomID != room.ID || !strings.Contains(delivery.Notes[0].Body, "dedicated switch handoff turn") || delivery.Notes[0].FYI {
				t.Fatalf("handoff task must be delivered as an addressed note: %+v", delivery.Notes)
			}
			body, _ := json.Marshal(SwitchHandoffBody{CurrentTask: "Feature X", Status: "Tests green", WorkState: "branch feature, SHA abc, file x.go", NextSteps: "Review and merge", OpenQuestions: "none", Cautions: "Do not push"})
			candidate, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"room": room.ID, "body": string(body)}}})
			parsed, err := answer.Parse(candidate, false)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := s.ApplyAnswer(ctx, AnswerInput{TurnID: delivery.TurnID, AgentID: agent.ID, Generation: 0, AnswerRev: 0, Digest: "handoff-digest", Candidate: candidate, Parsed: &parsed})
			if err != nil || receipt.Result != "committed" {
				t.Fatalf("handoff answer %+v %v", receipt, err)
			}
			if _, err := s.db.Exec(`UPDATE turn SET exec='exited',terminal_event=1,ended_at=? WHERE id=?`, timestamp(), delivery.TurnID); err != nil {
				t.Fatal(err)
			}
			controls, err := s.AdvanceAgentSwitches(ctx)
			if err != nil || len(controls) != 1 || controls[0].Desired != "not_accepting" || controls[0].Generation != 0 {
				t.Fatalf("old session drain %+v %v", controls, err)
			}
			controls, err = s.AdvanceAgentSwitches(ctx)
			if err != nil || len(controls) != 1 || controls[0].Generation != 1 {
				t.Fatalf("successor %+v %v", controls, err)
			}
			if err := s.ConfirmSpawn(ctx, agent.ID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
				t.Fatal(err)
			}
			switched, err := s.AgentCardByID(ctx, agent.ID)
			if err != nil || switched.Switch == nil || switched.Switch.State != "started" || switched.Switch.HandoffSource != "agent" {
				t.Fatalf("started %+v %v", switched, err)
			}
			messages, err := s.ListMessagesAfter(ctx, room.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			foundWritten := false
			for _, message := range messages {
				if strings.Contains(message.Body, "Feature X") && message.AuthorKind == "system" {
					foundWritten = strings.Contains(message.SwitchHandoffJSON, `"source":"written"`)
					if strings.HasPrefix(message.Body, "@") {
						t.Fatalf("raw mention in handoff: %q", message.Body)
					}
				}
			}
			if !foundWritten {
				t.Fatal("written handoff metadata missing")
			}
			next, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
			if err != nil || !ready || !strings.Contains(string(next.Context), "Feature X") || !strings.Contains(string(next.Context), "recent_room_history") {
				t.Fatalf("bootstrap %t %v %s", ready, err, next.Context)
			}
			if isLead != strings.Contains(string(next.Context), "project_handbook") {
				t.Fatalf("handbook scope %s", next.Context)
			}
		})
	}
}

func TestSwitchContinuityWaitFallbackAndCancel(t *testing.T) {
	s, _, room, agent := continuityFixture(t, false)
	ctx := context.Background()
	running, err := s.RecordTurn(ctx, Turn{AgentID: agent.ID, SessionID: "ses_one", Generation: 0, RoomID: room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	card := beginContinuity(t, s, room, agent)
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("mid-turn %+v %v", controls, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM switch_handoff_turn WHERE agent_id=?`, agent.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("handoff before current turn ended %d %v", count, err)
	}
	canceled, err := s.SwitchContinuityAction(ctx, agent.ID, "usr_switch_cont", "cancel", card.DesiredRev)
	if err != nil || canceled.Desired != "active" || canceled.Generation != 0 || canceled.Switch != nil {
		t.Fatalf("cancel %+v %v", canceled, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled',ended_at=? WHERE id=?`, timestamp(), running.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.BeginAgentSwitchWithContinuity(agent.ID, "usr_switch_cont", room.ID, "claude", "native", "haiku", "anthropic", "", "", "", current.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE switch_handoff SET overall_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	controls, err := s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].Generation != 0 || controls[0].Desired != "not_accepting" {
		t.Fatalf("fallback drain %+v %v", controls, err)
	}
	controls, err = s.AdvanceAgentSwitches(ctx)
	if err != nil || len(controls) != 1 || controls[0].Switch == nil || controls[0].Switch.HandoffSource != "automatic" {
		t.Fatalf("fallback %+v %v", controls, err)
	}
}

func TestSwitchContinuityHandoffDeadlineAndRetry(t *testing.T) {
	s, _, room, agent := continuityFixture(t, false)
	ctx := context.Background()
	beginContinuity(t, s, room, agent)
	if _, err := s.AdvanceAgentSwitches(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE switch_handoff SET handoff_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	// A queued handoff has not started its three-minute clock.
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("queued handoff %+v %v", controls, err)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("dispatch %t %v", ready, err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='running' WHERE id=?`, delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE switch_handoff SET handoff_deadline='2000-01-01T00:00:00.000000000Z' WHERE agent_id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 1 || controls[0].Desired != "not_accepting" || controls[0].Generation != 0 {
		t.Fatalf("running handoff drain %+v %v", controls, err)
	}
	card, err := s.AgentCardByID(ctx, agent.ID)
	if err != nil || card.Switch == nil || card.Switch.HandoffStatus != "automatic" {
		t.Fatalf("timeout %+v %v", card, err)
	}
	preview, err := s.AgentSwitchPreview(ctx, agent.ID, "usr_switch_cont", room.ID)
	if err != nil || preview.CanRetry {
		t.Fatalf("running retry preview %+v %v", preview, err)
	}
	if _, err := s.SwitchContinuityAction(ctx, agent.ID, "usr_switch_cont", "retry-handoff", card.DesiredRev); err == nil {
		t.Fatal("retried while outgoing handoff was still running")
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='exited',settle='settled',ended_at=? WHERE id=?`, timestamp(), delivery.TurnID); err != nil {
		t.Fatal(err)
	}
	card, err = s.AgentCardByID(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	preview, err = s.AgentSwitchPreview(ctx, agent.ID, "usr_switch_cont", room.ID)
	if err != nil || !preview.CanRetry {
		t.Fatalf("settled retry preview %+v %v", preview, err)
	}
	retried, err := s.SwitchContinuityAction(ctx, agent.ID, "usr_switch_cont", "retry-handoff", card.DesiredRev)
	if err != nil || retried.Desired != "active" || retried.Switch.HandoffStatus != "waiting" {
		t.Fatalf("retry %+v %v", retried, err)
	}
	if controls, err := s.AdvanceAgentSwitches(ctx); err != nil || len(controls) != 0 {
		t.Fatalf("retried handoff %+v %v", controls, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM switch_handoff_turn WHERE agent_id=?`, agent.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("handoff count %d %v", count, err)
	}
}
