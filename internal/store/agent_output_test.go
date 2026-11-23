package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// outputSources runs noteOutputSources over the agent's notes of one wake reason, as a delivery of them would.
func outputSources(t *testing.T, s *Store, agentID, reason string) []protocol.DeliveryNote {
	t.Helper()
	rows, err := s.db.Query(`SELECT id,message_id FROM note WHERE agent_id=? AND wake_reason=? ORDER BY id`, agentID, reason)
	if err != nil {
		t.Fatal(err)
	}
	var notes []protocol.DeliveryNote
	var reasons []string
	for rows.Next() {
		var note protocol.DeliveryNote
		if err := rows.Scan(&note.NoteID, &note.MessageID); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, note)
		reasons = append(reasons, reason)
	}
	rows.Close()
	if err := s.Tx(context.Background(), func(tx *Tx) error { return tx.noteOutputSources(notes, reasons) }); err != nil {
		t.Fatal(err)
	}
	return notes
}

// A silent-turn notice delivered to the agent that addressed the worker names the worker and the turn that ended silently, so the server can
// add the end of that turn's output; a note of another reason names nothing.
func TestSilentTurnNoticeNamesTheTurnItIsAbout(t *testing.T) {
	sf := newSilentFixture(t)
	turn := sf.turnWith(t, "agent", sf.boss.ID, "mention")
	sf.answer(t, turn, emptyAnswer)
	notes := outputSources(t, sf.s, sf.boss.ID, silentTurnEvent)
	if len(notes) != 1 || notes[0].OutputSource == nil || notes[0].OutputSource.AgentID != sf.f.agent.ID || notes[0].OutputSource.TurnID != turn.ID {
		t.Fatalf("notes=%+v source=%+v", notes, notes[0].OutputSource)
	}
	// The same message reached with another reason is not a notice about a worker.
	other := []protocol.DeliveryNote{{NoteID: notes[0].NoteID, MessageID: notes[0].MessageID}}
	if err := sf.s.Tx(context.Background(), func(tx *Tx) error { return tx.noteOutputSources(other, []string{"mention"}) }); err != nil || other[0].OutputSource != nil {
		t.Fatalf("a mention named a source: %+v %v", other[0].OutputSource, err)
	}
}

// A worker recovery handed to the lead names the failed turn in the lead's note.
func TestWorkerRecoveryNoticeNamesTheTurnItIsAbout(t *testing.T) {
	s, f, _, lead := workerRecoveryFixture(t)
	if err := s.RecordProcess(context.Background(), f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	notes := outputSources(t, s, lead.ID, "worker_recovery")
	if len(notes) != 1 || notes[0].OutputSource == nil || notes[0].OutputSource.AgentID != f.agent.ID || notes[0].OutputSource.TurnID != f.turn.ID {
		t.Fatalf("notes=%+v", notes)
	}
}

// TurnShownToAgent shows a lead a worker's turn only when the lead may read every room it served: the worker's ordinary room is open to it,
// a private chat of the worker's and a restricted room the lead is not seated in are not, and a turn the server does not know is withheld.
func TestTurnShownToAgentFollowsTheAgentRoomReadRule(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	known, withheld, rooms, err := s.TurnShownToAgent(ctx, f.turn.ID, f.agent.ID, lead.ID)
	if err != nil || !known || withheld || !slices.Equal(rooms, []string{f.room.ID}) {
		t.Fatalf("an ordinary turn: known=%t withheld=%t rooms=%v %v", known, withheld, rooms, err)
	}
	restricted, err := s.CreateRoom(ctx, Room{ProjectID: f.project.ID, ParentID: f.room.ID, Kind: "task", Title: "routing", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET restricted=1 WHERE id=?`, restricted.ID); err != nil {
		t.Fatal(err)
	}
	// A lead may have joined the new room on its own; it is not seated in it here.
	if _, err := s.db.Exec(`UPDATE room_member SET left_at='2026-01-01T00:00:00.000Z' WHERE room_id=? AND member_kind='agent' AND member_id=? AND left_at IS NULL`, restricted.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnRoomReads(ctx, f.turn.ID, restricted.ID); err != nil {
		t.Fatal(err)
	}
	if _, withheld, rooms, err := s.TurnShownToAgent(ctx, f.turn.ID, f.agent.ID, lead.ID); err != nil || !withheld || len(rooms) != 2 {
		t.Fatalf("a turn that read a restricted room the lead is not in: withheld=%t rooms=%v %v", withheld, rooms, err)
	}
	if _, err := s.db.Exec(`UPDATE room_member SET left_at=NULL WHERE room_id=? AND member_kind='agent' AND member_id=?`, restricted.ID, lead.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, restricted.ID, "agent", lead.ID); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatal(err)
	}
	if _, withheld, _, err := s.TurnShownToAgent(ctx, f.turn.ID, f.agent.ID, lead.ID); err != nil || withheld {
		t.Fatalf("the lead seated in the restricted room: withheld=%t %v", withheld, err)
	}
	dm, err := s.OpenDM(ctx, owner, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTurnRoomReads(ctx, f.turn.ID, dm.ID); err != nil {
		t.Fatal(err)
	}
	if _, withheld, _, err := s.TurnShownToAgent(ctx, f.turn.ID, f.agent.ID, lead.ID); err != nil || !withheld {
		t.Fatalf("a turn that served the worker's private chat: withheld=%t %v", withheld, err)
	}
	if known, withheld, _, err := s.TurnShownToAgent(ctx, "trn_missing", f.agent.ID, lead.ID); err != nil || known || !withheld {
		t.Fatalf("an unknown turn: known=%t withheld=%t %v", known, withheld, err)
	}
	if known, withheld, _, err := s.TurnShownToAgent(ctx, f.turn.ID, lead.ID, lead.ID); err != nil || known || !withheld {
		t.Fatalf("another agent's turn: known=%t withheld=%t %v", known, withheld, err)
	}
}

// agent_output is for an active lead reading a non-lead agent of its own project: a worker, a stopped lead, a lead target and the reader
// itself are refused, and another project's agent is not found.
func TestAuthorizeAgentOutputRead(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if machine, err := s.AuthorizeAgentOutputRead(ctx, lead.ID, f.agent.ID); err != nil || machine != f.agent.MachineID {
		t.Fatalf("lead reads its worker: %q %v", machine, err)
	}
	for _, tc := range []struct{ reader, target, name string }{{f.agent.ID, lead.ID, "a worker reads the lead"}, {lead.ID, lead.ID, "the lead reads itself"}} {
		if _, err := s.AuthorizeAgentOutputRead(ctx, tc.reader, tc.target); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	other, err := s.CreateAgent(ctx, Agent{Slug: "otherworker", ProjectID: f.project.ID, RoleID: f.agent.RoleID, Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.agent.MachineID, CheckoutID: f.agent.CheckoutID, KeyHash: "other-worker-key", CreatedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeAgentOutputRead(ctx, f.agent.ID, other.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a worker reads another worker: %v", err)
	}
	if _, err := s.AuthorizeAgentOutputRead(ctx, lead.ID, "agt_missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a missing agent: %v", err)
	}
	otherProject, err := s.CreateProject(ctx, Project{Name: "Elsewhere", Folder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET project_id=? WHERE id=?`, otherProject.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeAgentOutputRead(ctx, lead.ID, other.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another project's agent: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeAgentOutputRead(ctx, lead.ID, f.agent.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stopped lead: %v", err)
	}
}

// The notices to prefetch are each agent a silent turn woke and the lead a recovery went to, after the scan's cursor; the first scan looks
// at the latest events only, and a scan from the returned cursor finds nothing old again.
func TestNoticeSourcesAfterListsTheRecipients(t *testing.T) {
	sf := newSilentFixture(t)
	ctx := context.Background()
	_, start, err := sf.s.NoticeSourcesAfter(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	turn := sf.turnWith(t, "agent", sf.boss.ID, "mention")
	sf.answer(t, turn, emptyAnswer)
	notices, last, err := sf.s.NoticeSourcesAfter(ctx, start)
	if err != nil || len(notices) != 1 || notices[0] != (NoticeSource{AgentID: sf.f.agent.ID, TurnID: turn.ID, RecipientID: sf.boss.ID}) || last <= start {
		t.Fatalf("notices=%+v last=%d start=%d err=%v", notices, last, start, err)
	}
	if again, _, err := sf.s.NoticeSourcesAfter(ctx, last); err != nil || len(again) != 0 {
		t.Fatalf("a scan from the cursor: %+v %v", again, err)
	}
	if first, _, err := sf.s.NoticeSourcesAfter(ctx, 0); err != nil || len(first) != 1 {
		t.Fatalf("a first scan sees the latest notices: %+v %v", first, err)
	}
}

func TestNoticeSourcesAfterNamesTheRecoveryLead(t *testing.T) {
	s, f, _, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	_, start, err := s.NoticeSourcesAfter(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 137, false, false); err != nil {
		t.Fatal(err)
	}
	notices, _, err := s.NoticeSourcesAfter(ctx, start)
	if err != nil || len(notices) != 1 || notices[0] != (NoticeSource{AgentID: f.agent.ID, TurnID: f.turn.ID, RecipientID: lead.ID}) {
		t.Fatalf("notices=%+v err=%v", notices, err)
	}
}
