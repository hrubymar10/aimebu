package store

import (
	"context"
	"testing"
	"time"
)

// turnBudgetDispatcher adds a dispatcher (a spawning role: cap_can_spawn, no lead
// cap) to the wake fixture and makes it the worker's spawner.
func turnBudgetDispatcher(t *testing.T, s *Store, project Project, room Room, worker string) Agent {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key,cap_is_lead,cap_can_spawn) VALUES ('role_dispatcher_tb','dispatcher','Dispatcher','dispatch','dispatcher',0,1)`); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := s.CreateAgent(ctx, Agent{Slug: "dispatchtb", ProjectID: project.ID, RoleID: "role_dispatcher_tb", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, room.ID, "agent", dispatcher.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET created_by=? WHERE id=?`, dispatcher.ID, worker); err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

// seedTurnBudget records count completed normal turns inside the guard's window.
// noteAuthor is the author kind of the note each turn delivers — "agent" (a wake
// caused by another agent's message, the only kind that counts; waker sends it
// and it mentions the target by slug), "system" (a finished run_command), "user"
// (a person's message) or "" (no note at all). The waker must not be the target:
// an agent's own message never wakes it.
func seedTurnBudget(t *testing.T, s *Store, target, waker Agent, sessionID, roomID string, count int, noteAuthor string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		turn, err := s.RecordTurn(ctx, Turn{AgentID: target.ID, SessionID: sessionID, Generation: 0, RoomID: roomID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE turn SET exec='exited',outcome='completed',settle='settled',ended_at=? WHERE id=?`, time.Now().UTC().Add(-3*time.Second).Format(timestampLayout), turn.ID); err != nil {
			t.Fatal(err)
		}
		if noteAuthor == "" {
			continue
		}
		authorID := "owner"
		body := "work"
		wakeReason := ""
		switch noteAuthor {
		case "agent":
			authorID = waker.ID
			body = "@" + target.Slug + " ping"
		case "system":
			wakeReason = "command_finished"
		}
		message, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: noteAuthor, AuthorID: authorID, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		// An agent's mention and a person's room message wake the agent on their own (queueMessageWakes creates the note);
		// a system message does not, so one is inserted here. Backdating the turn's end by three seconds keeps the
		// agent-triggered wake cooldown out of the way.
		var note Note
		if noteAuthor == "system" {
			note, err = s.InsertNote(ctx, Note{AgentID: target.ID, MessageID: message.ID, RoomID: roomID, WakeReason: wakeReason})
		} else {
			note, err = s.NoteForAgentMessage(ctx, target.ID, message.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// expectBudgetPause records the wake note that would follow the seeded turns and
// checks that the next dispatch pauses the agent and raises one recovery item.
func expectBudgetPause(t *testing.T, s *Store, room Room, agent Agent) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "one more"}); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, agent.ID, ""); err != nil || ready {
		t.Fatalf("budget ready=%v %v", ready, err)
	}
	found, err := s.AgentRuntimeByID(ctx, agent.ID)
	if err != nil || found.Desired != "not_accepting" {
		t.Fatalf("agent=%+v %v", found, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, agent.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("inbox=%d %v", count, err)
	}
}

// expectBudgetNotReached checks that the next dispatch still goes through, then
// cancels the prepared turn so no pending turn blocks the next seeding.
func expectBudgetNotReached(t *testing.T, s *Store, room Room, agent Agent) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "next"}); err != nil {
		t.Fatal(err)
	}
	prepared, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready {
		t.Fatalf("budget ready=%v %v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, prepared.TurnID); err != nil {
		t.Fatal(err)
	}
	// The cancel stamps the turn's end now; backdate it like the seeded turns so the agent-wake cooldown (two seconds after the
	// last normal turn) does not block the next dispatch.
	if _, err := s.db.Exec(`UPDATE turn SET ended_at=? WHERE id=?`, time.Now().UTC().Add(-3*time.Second).Format(timestampLayout), prepared.TurnID); err != nil {
		t.Fatal(err)
	}
}

// The boundary: 59 agent wakes leave the agent dispatchable, the 60th pauses it.
func TestAgentWakesBudgetBoundary(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 59, "agent")
	expectBudgetNotReached(t, s, room, one)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 1, "agent")
	expectBudgetPause(t, s, room, one)
}

// One hundred finished-command wakes never exhaust the budget.
func TestCommandEndedWakesDoNotCountTowardTheBudget(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 100, "system")
	expectBudgetNotReached(t, s, room, one)
}

// Only agent wakes count: a person's messages and system notes ride along
// without filling the budget, and the 60th agent wake alone fires it.
func TestOnlyAgentWakesCountTowardTheBudget(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 100, "system")
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 100, "user")
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 59, "agent")
	expectBudgetNotReached(t, s, room, one)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 1, "agent")
	expectBudgetPause(t, s, room, one)
}

// A worker's runaway pause is handed to whoever manages it: the dispatcher that
// spawned the worker, not to a person.
func TestTurnBudgetPauseGoesToTheSpawningDispatcher(t *testing.T) {
	t.Parallel()
	s, project, room, one, lead := wakeFixture(t)
	dispatcher := turnBudgetDispatcher(t, s, project, room, one.ID)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 60, "agent")
	expectBudgetPause(t, s, room, one)
	var itemID, leadID, leadDue string
	if err := s.db.QueryRow(`SELECT id,COALESCE(recovery_lead_id,''),COALESCE(lead_due_at,'') FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, one.ID).Scan(&itemID, &leadID, &leadDue); err != nil {
		t.Fatal(err)
	}
	if leadID != dispatcher.ID || leadDue == "" {
		t.Fatalf("pause item assignee=%q due=%q, want the spawning dispatcher %q with a due time", leadID, leadDue, dispatcher.ID)
	}
	var wake int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM note WHERE agent_id=? AND state='queued' AND wake_reason='worker_recovery'`, dispatcher.ID).Scan(&wake); err != nil || wake != 1 {
		t.Fatalf("dispatcher worker_recovery notes=%d %v", wake, err)
	}
	var handed int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind='recovery_handed_to_lead' AND json_extract(payload_json,'$.inbox_item_id')=? AND json_extract(payload_json,'$.lead_id')=?`, itemID, dispatcher.ID).Scan(&handed); err != nil || handed != 1 {
		t.Fatalf("handed event=%d %v", handed, err)
	}
	var line int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body='Agent paused after 60 turns in 10 minutes; review its queued work.'`, room.ID).Scan(&line); err != nil || line != 1 {
		t.Fatalf("system line in the room=%d %v", line, err)
	}
}

// The pause of a lead goes to people, as before.
func TestTurnBudgetPauseOfALeadStaysWithPeople(t *testing.T) {
	t.Parallel()
	s, _, room, one, lead := wakeFixture(t)
	seedTurnBudget(t, s, lead, one, "ses_lead", room.ID, 60, "agent")
	expectBudgetPause(t, s, room, lead)
	var leadID, leadDue string
	if err := s.db.QueryRow(`SELECT COALESCE(recovery_lead_id,''),COALESCE(lead_due_at,'') FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, lead.ID).Scan(&leadID, &leadDue); err != nil {
		t.Fatal(err)
	}
	if leadID != "" || leadDue != "" {
		t.Fatalf("a lead's pause was handed to %q (due %q), want people", leadID, leadDue)
	}
}

// A pause in the project's home room stays with people too.
func TestTurnBudgetPauseInTheHomeRoomStaysWithPeople(t *testing.T) {
	t.Parallel()
	s, project, room, one, lead := wakeFixture(t)
	turnBudgetDispatcher(t, s, project, room, one.ID)
	home, err := s.CreateRoom(context.Background(), Room{ProjectID: project.ID, Kind: "home", Title: "home", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(context.Background(), home.ID, "agent", one.ID); err != nil {
		t.Fatal(err)
	}
	seedTurnBudget(t, s, one, lead, "ses_one", home.ID, 60, "agent")
	expectBudgetPause(t, s, home, one)
	var leadID, leadDue string
	if err := s.db.QueryRow(`SELECT COALESCE(recovery_lead_id,''),COALESCE(lead_due_at,'') FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, one.ID).Scan(&leadID, &leadDue); err != nil {
		t.Fatal(err)
	}
	if leadID != "" || leadDue != "" {
		t.Fatalf("a home-room pause was handed to %q (due %q), want people", leadID, leadDue)
	}
}

// The assigned dispatcher releases the pause through the same decision path a
// person uses, and the Continue resets the budget; another lead is refused.
func TestAssignedDispatcherContinuesTurnBudgetPause(t *testing.T) {
	t.Parallel()
	s, project, room, one, lead := wakeFixture(t)
	dispatcher := turnBudgetDispatcher(t, s, project, room, one.ID)
	seedTurnBudget(t, s, one, lead, "ses_one", room.ID, 60, "agent")
	expectBudgetPause(t, s, room, one)
	var itemID string
	if err := s.db.QueryRow(`SELECT id FROM inbox_item WHERE agent_id=? AND kind='recovery' AND state='open'`, one.ID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	rev, err := s.RecoveryItemRevision(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(context.Background(), itemID, lead.ID, "wrong-lead", "continue", "", rev); err == nil {
		t.Fatal("an unassigned lead decided the pause")
	}
	if _, reused, err := s.DecideRecoveryAsAgent(context.Background(), itemID, dispatcher.ID, "dispatcher-continue", "continue", "", rev); err != nil || reused {
		t.Fatalf("dispatcher continue=%t %v", reused, err)
	}
	found, err := s.AgentRuntimeByID(context.Background(), one.ID)
	if err != nil || found.Desired != "active" {
		t.Fatalf("agent=%+v %v", found, err)
	}
	var resolvedBy, state string
	if err := s.db.QueryRow(`SELECT state,COALESCE(resolved_by,'') FROM inbox_item WHERE id=?`, itemID).Scan(&state, &resolvedBy); err != nil || state != "resolved" || resolvedBy != dispatcher.ID {
		t.Fatalf("item=%s by=%q %v", state, resolvedBy, err)
	}
	var resets int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='agent_control_requested' AND json_extract(payload_json,'$.desired')='active'`, one.ID).Scan(&resets); err != nil || resets != 1 {
		t.Fatalf("budget reset events=%d %v", resets, err)
	}
	// The reset window means the queued work dispatches instead of pausing again.
	expectBudgetNotReached(t, s, room, one)
}
