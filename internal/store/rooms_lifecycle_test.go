package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResumeAgentDuringRoomCloseKeepsSessionAndMembership(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_resume','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_resume") }); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "resumable", CreatedBy: "usr_resume"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, task.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	lead, err := s.CreateAgent(ctx, Agent{Slug: "lead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "lead_hash", CreatedBy: "usr_resume"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, task.ID, "agent", lead.ID); err != nil {
		t.Fatal(err)
	}
	handoff, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "agent", AuthorID: f.agent.ID,
		Body: "complete", Handoff: true, Outcome: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.SetReaction(handoff.ID, "agent", lead.ID, "✅", true) }); err != nil {
		t.Fatal(err)
	}
	var afterReaction string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, f.agent.ID).Scan(&afterReaction); err != nil || afterReaction != "active" {
		t.Fatalf("reaction changed worker desired=%q err=%v", afterReaction, err)
	}
	if _, err := s.db.Exec(`UPDATE session SET native_session_id='native_worker',native_session_started_at=? WHERE id='ses_test'`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(f.agent.ID, "usr_resume", "stopped", 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room_member SET left_at=? WHERE room_id=? AND member_kind='agent' AND member_id=?`, timestamp(), task.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	closing, _, err := s.BeginRoomClose(ctx, task.ID, "usr_resume", 0, false)
	if err != nil || closing.Lifecycle != "closing" {
		t.Fatalf("begin close=%+v err=%v", closing, err)
	}
	var resumed AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		resumed, err = tx.ResumeAgentAsTool(f.agent.ID, lead.ID, "same task")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if resumed.ID != f.agent.ID || resumed.Desired != "active" || resumed.RoomID != task.ID || resumed.Generation != 1 {
		t.Fatalf("resumed=%+v", resumed)
	}
	room, err := s.RoomByID(ctx, task.ID)
	if err != nil || room.Lifecycle != "open" || room.LifecycleRev != 2 {
		t.Fatalf("room=%+v err=%v", room, err)
	}
	var mode, handle string
	if err := s.db.QueryRow(`SELECT start_mode,native_session_id FROM session WHERE agent_id=? AND generation=1`, f.agent.ID).Scan(&mode, &handle); err != nil || mode != "native_resume" || handle != "native_worker" {
		t.Fatalf("session=%q/%q err=%v", mode, handle, err)
	}
	var joined int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, task.ID, f.agent.ID).Scan(&joined); err != nil || joined != 1 {
		t.Fatalf("membership=%d err=%v", joined, err)
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, 0); err != nil || len(failures) != 0 {
		t.Fatalf("advance after cancellation: %v failures=%v", err, failures)
	}
	room, _ = s.RoomByID(ctx, task.ID)
	if room.Lifecycle != "open" {
		t.Fatalf("canceled closure advanced: %+v", room)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ResumeAgentAsTool(f.agent.ID, lead.ID, "again")
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("active worker resumed: %v", err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ResumeAgentAsTool(f.agent.ID, "agt_another_project", "")
		return err
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("other project resumed worker: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET revoked_at=? WHERE id=?`, timestamp(), f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ResumeAgentAsTool(f.agent.ID, lead.ID, "")
		return err
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed worker resumed: %v", err)
	}
}

func TestResumeAgentRejectsCascadedChildClose(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_cascade','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_cascade") }); err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "parent", CreatedBy: "usr_cascade"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: parent.ID, Kind: "task", Title: "child", CreatedBy: "usr_cascade"})
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.CreateAgent(ctx, Agent{Slug: "cascadelead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "cascade_hash", CreatedBy: "usr_cascade"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET home_room_id=?,desired='stopped',observed='stopped' WHERE id=?`, child.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginRoomClose(ctx, parent.ID, "usr_cascade", 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ResumeAgentAsTool(f.agent.ID, lead.ID, "continue child")
		return err
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume under closing parent: %v", err)
	}
	for _, id := range []string{parent.ID, child.ID} {
		room, err := s.RoomByID(ctx, id)
		if err != nil || room.Lifecycle != "closing" {
			t.Fatalf("room %s after refused resume: %+v err=%v", id, room, err)
		}
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, 0); err != nil || len(failures) != 0 {
		t.Fatalf("cascade advance: %v failures=%v", err, failures)
	}
	for _, id := range []string{parent.ID, child.ID} {
		room, err := s.RoomByID(ctx, id)
		if err != nil || room.Lifecycle != "closed" {
			t.Fatalf("room %s after advance: %+v err=%v", id, room, err)
		}
	}
}

func TestResumeAgentBoundaryEvents(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_resume_boundary','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_resume_boundary") }); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "boundary task", CreatedBy: "usr_resume_boundary"})
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.CreateAgent(ctx, Agent{Slug: "boundarylead", ProjectID: f.project.ID, RoleID: "role_leader_default", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "boundary_hash", CreatedBy: "usr_resume_boundary"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET home_room_id=?,desired='stopped',observed='stopped' WHERE id=?`, task.ID, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path='/outside/resume' WHERE id='chk_test'`); err != nil {
		t.Fatal(err)
	}
	countResumed := func() int {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='agent_resumed' AND agent_id=?`, f.agent.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	var pending AgentCard
	t.Run("pending approval emits no resumed event", func(t *testing.T) {
		if err := s.Tx(ctx, func(tx *Tx) error {
			var err error
			pending, err = tx.ResumeAgentAsTool(f.agent.ID, lead.ID, "needs approval")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if pending.Desired != "stopped" || pending.BoundaryPending == "" || countResumed() != 0 {
			t.Fatalf("pending card=%+v resumed events=%d", pending, countResumed())
		}
	})
	t.Run("approval emits one resumed event", func(t *testing.T) {
		if pending.BoundaryPending == "" {
			t.Fatal("resume did not create an approval")
		}
		if _, _, err := s.DecideBoundary(ctx, pending.BoundaryPending, "usr_resume_boundary", "approve-resume-boundary", "approve", 0); err != nil {
			t.Fatal(err)
		}
		card, err := s.AgentCardByID(ctx, f.agent.ID)
		if err != nil || card.Desired != "active" || countResumed() != 1 {
			t.Fatalf("approved card=%+v err=%v resumed events=%d", card, err, countResumed())
		}
	})
}

func TestRoomCloseCascadeReopenAndInbox(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, home, _ := fixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_test','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(project.ID, "usr_test") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_viewer','viewer','hash','{}',?),('usr_other','other','hash','{}',?)`, timestamp(), timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'viewer')`, project.ID, "usr_viewer"); err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "parent", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: parent.ID, Kind: "task", Title: "child", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginRoomClose(ctx, home.ID, "usr_test", 0, false); !errors.Is(err, ErrHomeRoom) {
		t.Fatalf("home close error=%v", err)
	}
	if _, _, err := s.BeginRoomClose(ctx, parent.ID, "usr_test", 0, false); !errors.Is(err, ErrOpenChildren) {
		t.Fatalf("open child error=%v", err)
	}
	if _, _, err := s.BeginRoomClose(ctx, parent.ID, "usr_test", 1, true); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale revision error=%v", err)
	}
	for _, actor := range []string{"usr_viewer", "usr_other"} {
		if _, _, err := s.BeginRoomClose(ctx, parent.ID, actor, 0, true); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s close permission=%v", actor, err)
		}
	}
	for _, kind := range []string{"question", "approval", "recovery"} {
		if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,created_at)
			VALUES (?,?,?,?,?,?,?)`, "inb_"+kind, project.ID, child.ID, "agent", "test", kind, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	closed, controls, err := s.BeginRoomClose(ctx, parent.ID, "usr_test", 0, true)
	if err != nil || closed.Lifecycle != "closing" || closed.LifecycleRev != 1 || len(controls) != 0 {
		t.Fatalf("begin close=%+v controls=%v err=%v", closed, controls, err)
	}
	if _, _, err := s.BeginRoomClose(ctx, parent.ID, "usr_test", 0, true); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale repeated close=%v", err)
	}
	if _, err := s.ReopenRoom(ctx, parent.ID, "usr_test", 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("early reopen error=%v", err)
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, 30*time.Second); err != nil || len(failures) != 0 {
		t.Fatal(err)
	}
	parent, err = s.RoomByID(ctx, parent.ID)
	if err != nil || parent.Lifecycle != "closed" || parent.ClosedAt == "" || parent.LifecycleRev != 2 {
		t.Fatalf("parent after close=%+v err=%v", parent, err)
	}
	child, err = s.RoomByID(ctx, child.ID)
	if err != nil || child.Lifecycle != "closed" {
		t.Fatalf("child after close=%+v err=%v", child, err)
	}
	if _, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: parent.ID, Kind: "task", Title: "late", CreatedBy: "usr_test"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("created child under closed room: %v", err)
	}
	for _, check := range []struct{ kind, state string }{{"question", "withdrawn"}, {"approval", "withdrawn"}, {"recovery", "resolved"}} {
		var state string
		if err := s.db.QueryRow(`SELECT state FROM inbox_item WHERE id=?`, "inb_"+check.kind).Scan(&state); err != nil || state != check.state {
			t.Fatalf("%s state=%q err=%v", check.kind, state, err)
		}
	}
	inbox, err := s.InboxForUser(ctx, "usr_test")
	// Closing the room settled the recovery, and a closed room's items are never listed.
	if err != nil || len(inbox) != 0 {
		t.Fatalf("archived room inbox=%+v err=%v", inbox, err)
	}
	var settledBy string
	if err := s.db.QueryRow(`SELECT resolved_by FROM inbox_item WHERE id='inb_recovery'`).Scan(&settledBy); err != nil || settledBy != "system (room closed)" {
		t.Fatalf("recovery resolved_by=%q err=%v", settledBy, err)
	}
	if _, err := s.ReopenRoom(ctx, parent.ID, "usr_test", 1); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale reopen error=%v", err)
	}
	for _, actor := range []string{"usr_viewer", "usr_other"} {
		if _, err := s.ReopenRoom(ctx, parent.ID, actor, 2); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s reopen permission=%v", actor, err)
		}
	}
	if _, err := s.ReopenRoom(ctx, child.ID, "usr_test", 2); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reopened child under closed parent: %v", err)
	}
	reopened, err := s.ReopenRoom(ctx, parent.ID, "usr_test", 2)
	if err != nil || reopened.Lifecycle != "open" || reopened.Gate != "open" || reopened.ClosedAt != "" || reopened.LifecycleRev != 3 {
		t.Fatalf("reopened=%+v err=%v", reopened, err)
	}
	if _, err := s.ReopenRoom(ctx, parent.ID, "usr_test", 2); !errors.Is(err, ErrStaleRev) {
		t.Fatalf("stale repeated reopen=%v", err)
	}
	child, err = s.RoomByID(ctx, child.ID)
	if err != nil || child.Lifecycle != "closed" {
		t.Fatalf("child was reopened: %+v %v", child, err)
	}
}

func TestRoomCloseStopsOnlyHomeAgentsAfterGrace(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	f := seedExecution(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_test','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_test") }); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	closing, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "closing", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, closing.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: f.agent.ID, PayloadJSON: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owned, err := s.CreateAgent(ctx, Agent{Slug: "owned", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, closing.ID, "agent", owned.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: f.project.ID, RoomID: closing.ID, AgentID: owned.ID, PayloadJSON: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET room_id=?,exec='running' WHERE id=?`, closing.ID, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO outbox(id,turn_id,answer_rev,idx,agent_id,room_id,body,digest,state,dedupe_key)
		VALUES ('out_archive',?,0,0,?,?,'draft','digest','proposed','archive-once')`, f.turn.ID, f.agent.ID, closing.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='in_turn' WHERE id=?`, owned.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at)
		VALUES ('ses_owned_elsewhere',?,0,'fresh_with_history',?)`, owned.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTurn(ctx, Turn{AgentID: owned.ID, SessionID: "ses_owned_elsewhere", Generation: 0,
		RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "running"}); err != nil {
		t.Fatal(err)
	}
	room, controls, err := s.BeginRoomClose(ctx, closing.ID, "usr_test", 0, false)
	if err != nil || room.Lifecycle != "closing" || len(controls) != 1 || controls[0].ID != owned.ID || controls[0].Desired != "not_accepting" {
		t.Fatalf("begin close=%+v controls=%+v err=%v", room, controls, err)
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, 30*time.Second); err != nil || len(failures) != 0 {
		t.Fatal(err)
	}
	room, err = s.RoomByID(ctx, closing.ID)
	if err != nil || room.Lifecycle != "closing" {
		t.Fatalf("closed before grace: %+v %v", room, err)
	}
	var desiredBeforeGrace string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, owned.ID).Scan(&desiredBeforeGrace); err != nil || desiredBeforeGrace != "not_accepting" {
		t.Fatalf("home agent stopped during another room's turn: %q %v", desiredBeforeGrace, err)
	}
	controls, failures, err := s.AdvanceRoomClosures(ctx, 0)
	if err != nil || len(failures) != 0 || len(controls) != 1 || controls[0].Desired != "stopped" {
		t.Fatalf("finish controls=%+v err=%v", controls, err)
	}
	room, err = s.RoomByID(ctx, closing.ID)
	if err != nil || room.Lifecycle != "closed" {
		t.Fatalf("room after grace=%+v err=%v", room, err)
	}
	messages, err := s.ListMessagesAfter(ctx, closing.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundStopNotice := false
	for _, message := range messages {
		if message.AuthorKind == "system" && strings.Contains(message.Body, "stopped because the room closed; resume with `resume_agent`") {
			foundStopNotice = true
		}
	}
	if !foundStopNotice {
		t.Fatal("closing the room did not post the worker stop notice")
	}
	var state, reason string
	if err := s.db.QueryRow(`SELECT state,held_reason FROM outbox WHERE id='out_archive'`).Scan(&state, &reason); err != nil || state != "held" || reason != "room_closed" {
		t.Fatalf("outbox after close=%q/%q err=%v", state, reason, err)
	}
	for _, check := range []struct{ id, desired string }{{owned.ID, "stopped"}, {f.agent.ID, "active"}} {
		var desired string
		if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, check.id).Scan(&desired); err != nil || desired != check.desired {
			t.Fatalf("agent %s desired=%q err=%v", check.id, desired, err)
		}
	}
	stuck, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, Kind: "task", Title: "stuck", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	lost, err := s.CreateAgent(ctx, Agent{Slug: "lost", ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test", Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, stuck.ID, "agent", lost.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", ProjectID: f.project.ID, RoomID: stuck.ID, AgentID: lost.ID, PayloadJSON: "{}"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='lost' WHERE id=?`, lost.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginRoomClose(ctx, stuck.ID, "usr_test", 0, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, failures, err := s.AdvanceRoomClosures(ctx, 0); err != nil || len(failures) != 0 {
			t.Fatalf("blocked close pass %d failures=%+v err=%v", i, failures, err)
		}
	}
	stuck, err = s.RoomByID(ctx, stuck.ID)
	if err != nil || stuck.Lifecycle != "closing" {
		t.Fatalf("lost agent close=%+v err=%v", stuck, err)
	}
	var blockedEvents int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE room_id=? AND agent_id=? AND kind='room_close_blocked'`, stuck.ID, lost.ID).Scan(&blockedEvents); err != nil || blockedEvents != 1 {
		t.Fatalf("blocked events=%d err=%v", blockedEvents, err)
	}
}

func TestRoomCloseBlockedEventAndFailureIsolation(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, _, _ := fixture(t, s)
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_test','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(project.ID, "usr_test") }); err != nil {
		t.Fatal(err)
	}
	stuck, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "stuck", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "healthy", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "broken", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range []Room{stuck, healthy, broken} {
		if _, _, err := s.BeginRoomClose(ctx, room.ID, "usr_test", 0, false); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a damaged close-start event in one room. Other closures must
	// still advance rather than blocking the dispatcher's whole pass.
	if _, err := s.db.Exec(`DELETE FROM event WHERE room_id=? AND kind='room_closing'`, broken.ID); err != nil {
		t.Fatal(err)
	}
	_, failures, err := s.AdvanceRoomClosures(ctx, 0)
	if err != nil || len(failures) != 1 || failures[0].RoomID != broken.ID {
		t.Fatalf("isolated failures=%+v err=%v", failures, err)
	}
	for _, room := range []Room{stuck, healthy} {
		current, err := s.RoomByID(ctx, room.ID)
		if err != nil || current.Lifecycle != "closed" {
			t.Fatalf("healthy close=%+v err=%v", current, err)
		}
	}
}
