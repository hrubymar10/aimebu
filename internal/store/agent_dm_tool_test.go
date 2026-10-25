package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// leadTurn gives the lead one running turn that the given people's messages caused (none: an agent or a schedule woke it).
func (f dmFixture) leadTurn(t *testing.T, lead Agent, people ...string) Turn {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE agent_id=? AND exec IN ('pending','running')`, lead.ID); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := f.s.db.QueryRow(`SELECT id FROM session WHERE agent_id=?`, lead.ID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	home := f.task1
	if lead.ProjectID == f.p2.ID {
		home = f.task2
	}
	turn, err := f.s.RecordTurn(ctx, Turn{AgentID: lead.ID, SessionID: session, Generation: 0, RoomID: home.ID, Attempt: 1, Mode: "normal", Exec: "running"})
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range people {
		msg, err := f.s.InsertMessage(ctx, Message{RoomID: home.ID, AuthorKind: "user", AuthorID: person, Body: "please talk to the other project"})
		if err != nil {
			t.Fatal(err)
		}
		var note string
		if err := f.s.db.QueryRow(`SELECT id FROM note WHERE agent_id=? AND message_id=?`, lead.ID, msg.ID).Scan(&note); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, turn.ID, note); err != nil {
			t.Fatal(err)
		}
	}
	return turn
}

func (f dmFixture) dm(t *testing.T, from, to Agent, body string) (AgentDMResult, error) {
	t.Helper()
	var result AgentDMResult
	err := f.s.Tx(context.Background(), func(tx *Tx) (err error) {
		result, err = tx.DMAgentAsTool(from.ID, to.ID, body)
		return err
	})
	return result, err
}

// The people who caused the lead's turn decide, and every one of them has to reach the target's project.
func TestDMAgentAuthorizationFollowsWhoWokeTheTurn(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		people []string
		want   error
	}{
		"owner of only the lead's project":    {[]string{f.alice}, ErrForbidden},
		"operator of the target's project":    {[]string{f.bob}, nil},
		"viewer of the target's project":      {[]string{f.vera}, ErrForbidden},
		"the instance owner":                  {[]string{f.owner}, nil},
		"one who reaches and one who doesn't": {[]string{f.bob, f.alice}, ErrForbidden},
		"two who reach":                       {[]string{f.bob, f.owner}, nil},
		"a stranger":                          {[]string{f.carol}, ErrForbidden},
		// No person: an owner of the lead's own project stands in; bob owns it here and operates the target's project.
		"woken by an agent or a timer": {nil, nil},
	} {
		f.leadTurn(t, f.lead1, tc.people...)
		if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
	}
	// Without a person, the lead's project's owners are the ones: alice owns it and has no reach into the target's project.
	if _, err := f.s.db.Exec(`DELETE FROM membership WHERE project_id=? AND user_id=?`, f.p1.ID, f.bob); err != nil {
		t.Fatal(err)
	}
	f.leadTurn(t, f.lead1)
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a timer-woken lead whose owner has no reach: %v", err)
	}
	// An owner who is also the instance owner reaches every project.
	if _, err := f.s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, f.p1.ID, f.owner); err != nil {
		t.Fatal(err)
	}
	f.leadTurn(t, f.lead1)
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); err != nil {
		t.Fatalf("a timer-woken lead whose owner is the instance owner: %v", err)
	}
	// A retry or a correction continues the turn that a person caused.
	parent := f.leadTurn(t, f.lead1, f.carol)
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,parent_turn_id,attempt,mode,exec) VALUES ('trn_fix',?,'ses_lead',0,?,?,1,'correction','running')`, f.lead1.ID, f.task1.ID, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a correction of a turn a stranger caused: %v", err)
	}
	// Outside a turn there is nobody to ask.
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='exited' WHERE agent_id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a call outside any turn: %v", err)
	}
}

func TestDMAgentOnlyAnActiveLeadMayStartAChat(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	if _, err := f.dm(t, f.worker1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a worker: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='not_accepting' WHERE id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a lead that is not accepting work: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agent SET desired='active' WHERE id=?`, f.lead1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.lead1, "hello"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a chat with oneself: %v", err)
	}
	if _, err := f.dm(t, f.lead1, Agent{ID: "agt_nobody"}, "hello"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an agent that does not exist answers like one out of reach: %v", err)
	}
}

func TestDMAgentTargetMustBeLive(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	for name, tc := range map[string]struct {
		query string
		want  error
	}{
		"stopped":       {`UPDATE agent SET desired='stopped' WHERE id=?`, ErrAgentUnavailable},
		"not accepting": {`UPDATE agent SET desired='not_accepting' WHERE id=?`, ErrAgentUnavailable},
		"revoked":       {`UPDATE agent SET revoked_at='2026-01-01T00:00:00.000Z' WHERE id=?`, ErrAgentUnavailable},
		"archived":      {`UPDATE project SET archived_at='2026-01-01T00:00:00.000Z' WHERE id=(SELECT project_id FROM agent WHERE id=?)`, ErrProjectArchived},
	} {
		if _, err := f.s.db.Exec(tc.query, f.worker2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want %v", name, err, tc.want)
		}
		for _, undo := range []string{`UPDATE agent SET desired='active',revoked_at=NULL WHERE id=?`, `UPDATE project SET archived_at=NULL WHERE id=(SELECT project_id FROM agent WHERE id=?)`} {
			if _, err := f.s.db.Exec(undo, f.worker2.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The lead's own archived project cannot start anything either.
	if _, err := f.s.db.Exec(`UPDATE project SET archived_at='2026-01-01T00:00:00.000Z' WHERE id=?`, f.p1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "hello"); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("an archived lead project: %v", err)
	}
}

func TestDMAgentOpensTheChatAndWakesTheTarget(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	first, err := f.dm(t, f.lead1, f.worker2, "can you review the API shape?")
	if err != nil || !first.Created || first.No == 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if got := f.notesFor(t, first.MessageID); !sameWakes(got, map[string]string{"peer": "agent_dm"}) {
		t.Fatalf("wakes=%v", got)
	}
	second, err := f.dm(t, f.lead1, f.worker2, "and the error codes")
	if err != nil || second.Created || second.RoomID != first.RoomID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	// The target is woken under its own project and sees the chat.
	delivery, ready, err := f.s.PrepareDispatch(context.Background(), f.worker2.ID, "")
	if err != nil || !ready || len(delivery.Notes) != 2 || delivery.Notes[0].RoomID != first.RoomID {
		t.Fatalf("delivery=%+v ready=%t err=%v", delivery, ready, err)
	}
	// The other lead can open its own chat with the same agent: a pair each.
	var other AgentDMResult
	f.leadTurn(t, f.lead2, f.owner)
	if other, err = f.dm(t, f.lead2, f.worker2, "hi"); err != nil || other.RoomID == first.RoomID {
		t.Fatalf("other=%+v err=%v", other, err)
	}
}

func TestDMAgentLimits(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	f.leadTurn(t, f.lead1, f.owner)
	chat, err := f.dm(t, f.lead1, f.worker2, "hello")
	if err != nil {
		t.Fatal(err)
	}
	// A chat that used its hour of turns refuses a message the lead would send into it.
	for i := 0; i < AgentDMTurnsPerHour; i++ {
		if _, err := f.s.db.Exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec,started_at) VALUES (?,?,'ses_peer2',0,?,1,'normal','exited',?)`,
			"trn_cap_"+string(rune('a'+i)), f.worker2.ID, chat.RoomID, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "more"); !errors.Is(err, ErrDMTurnCap) {
		t.Fatalf("turn limit: %v", err)
	}
	// A lead may open only so many new chats an hour; the ones it already has still work.
	now := time.Now().UTC().Format(timestampLayout)
	for i := 0; i < AgentDMNewChatsPerHour; i++ {
		if _, err := f.s.db.Exec(`INSERT INTO room(id,project_id,depth,kind,dm_key,title,created_by,created_at) VALUES (?,?,0,'dm',?,'x',?,?)`,
			"room_fan_"+string(rune('a'+i)), AgentDMProjectID, "agents:fan"+string(rune('a'+i)), f.lead1.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.dm(t, f.lead1, f.lead2, "a new one"); !errors.Is(err, ErrDMFanOutCap) {
		t.Fatalf("new chat limit: %v", err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM turn WHERE id LIKE 'trn_cap_%'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dm(t, f.lead1, f.worker2, "an existing chat is not a new one"); err != nil {
		t.Fatalf("existing chat: %v", err)
	}
}
