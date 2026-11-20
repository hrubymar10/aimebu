package store

import (
	"context"
	"errors"
	"testing"
)

func workerRecoveryFixture(t *testing.T) (*Store, executionFixture, string, Agent) {
	t.Helper()
	s, f, owner := recoveryFixture(t, false)
	lead, err := s.CreateAgent(context.Background(), Agent{Slug: "recoverylead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.agent.MachineID, CheckoutID: f.agent.CheckoutID, KeyHash: "recovery-lead-key", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := s.AgentInRoom(context.Background(), lead.ID, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !joined {
		if err := s.AddMember(context.Background(), f.room.ID, "agent", lead.ID); err != nil {
			t.Fatal(err)
		}
	}
	return s, f, owner, lead
}

func TestWorkerRecoveryGoesToLeadAndLeadResolves(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 0 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	var noteCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND wake_reason='worker_recovery'`, lead.ID).Scan(&noteCount); err != nil || noteCount != 1 {
		t.Fatalf("lead notes=%d err=%v", noteCount, err)
	}
	var noticeCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND body LIKE 'Recovery handed to the lead%'`, f.room.ID).Scan(&noticeCount); err != nil || noticeCount != 1 {
		t.Fatalf("worker room notices=%d err=%v", noticeCount, err)
	}
	decision, reused, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-resume", "resume", "checked evidence", recoveries[0].Rev)
	if err != nil || reused || decision.TurnID == "" {
		t.Fatalf("decision=%+v reused=%v err=%v", decision, reused, err)
	}
	again, reused, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-resume", "resume", "checked evidence", recoveries[0].Rev+1)
	if err != nil || !reused || again.TurnID != decision.TurnID {
		t.Fatalf("replay=%+v reused=%v err=%v", again, reused, err)
	}
	var blocked bool
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&blocked); err != nil || blocked {
		t.Fatalf("worker blocked=%v err=%v", blocked, err)
	}
}

func TestWorkerRecoveryEscalatesWhenDueOrLeadBlocked(t *testing.T) {
	for _, blockedLead := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "blocked"}[blockedLead], func(t *testing.T) {
			s, f, owner, lead := workerRecoveryFixture(t)
			ctx := context.Background()
			if blockedLead {
				if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, lead.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
				t.Fatal(err)
			}
			if !blockedLead {
				if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at='2000-01-01T00:00:00.000000000Z' WHERE agent_id=? AND kind='recovery'`, f.agent.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.SweepWorkerRecoveries(ctx); err != nil {
					t.Fatal(err)
				}
			}
			items, err := s.InboxForUser(ctx, owner)
			if err != nil || len(items) != 1 {
				t.Fatalf("human inbox=%+v err=%v", items, err)
			}
		})
	}
}

func TestLeadOwnRecoveryGoesToHuman(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET role_id='role_leader_default' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("lead recovery inbox=%+v err=%v", items, err)
	}
}

func TestWorkerRecoveryWithoutLeadGoesToHuman(t *testing.T) {
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
}

// A lead outside the worker's room, in a project with no home room to tell it in, leaves the decision to the person: the turn's exit is
// still recorded, instead of failing for want of a room.
func TestWorkerRecoveryWithLeadOutsideRoomAndNoHomeRoomGoesToHuman(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=?`, timestamp(), f.room.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET kind='task' WHERE project_id=? AND kind='home'`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatalf("the exit must be recorded: %v", err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("human inbox=%+v err=%v", items, err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 0 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
}

// A lead retrying a decision on an item the sweep gave to people and someone then resolved reads who resolved it and how, not a bare
// refusal that sounds like it lacks the lead role.
func TestLateLeadDecisionOnAResolvedRecoverySaysWhoResolvedIt(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead items=%+v err=%v", recoveries, err)
	}
	item := recoveries[0]
	// The sweep hands the item to people: while it is open, the lead's decision is still refused.
	if _, err := s.db.Exec(`UPDATE inbox_item SET lead_due_at=NULL WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, item.ID, lead.ID, "lead-late", "retry", "", item.Rev); err != ErrForbidden {
		t.Fatalf("an open item held by people: %v", err)
	}
	if _, _, err := s.DecideRecovery(ctx, item.ID, owner, "person-retry", "retry", int(item.Rev)); err != nil {
		t.Fatalf("person decision: %v", err)
	}
	rev, err := s.RecoveryItemRevision(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.DecideRecoveryAsAgent(ctx, item.ID, lead.ID, "lead-late", "retry", "", rev)
	var settled *AlreadySettledError
	if !errors.As(err, &settled) || !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("late lead decision: %v", err)
	}
	if settled.By != owner || settled.How != "retry" || settled.Error() != "already resolved by "+owner+" (choice retry)" {
		t.Fatalf("settled=%+v text=%q", settled, settled.Error())
	}
}
