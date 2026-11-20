package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func openInboxKinds(t *testing.T, s *Store, owner string) []string {
	t.Helper()
	items, err := s.InboxForUser(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, item := range items {
		kinds = append(kinds, item.Kind)
	}
	return kinds
}

// recoveryAndDeliveryFailure leaves a worker with one open recovery (its turn's process was killed) and one open delivery failure (prose where
// JSON belongs, on a turn that cannot be corrected), both in the fixture's room.
func recoveryAndDeliveryFailure(t *testing.T) (*Store, executionFixture, string) {
	t.Helper()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, false); err != nil {
		t.Fatal(err)
	}
	// A second turn would be needed for a delivery failure on the first, so seed the failure through its own item instead of a second answer.
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES ('inb_df',?,?,?,?,'agent',?,'delivery_failed','{"text":"An agent reply could not be delivered."}',?)`,
		f.project.ID, f.room.ID, f.turn.ID, f.agent.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(openInboxKinds(t, s, owner), ","); got != "recovery,delivery_failed" && got != "delivery_failed,recovery" {
		t.Fatalf("open items %q, want a recovery and a delivery_failed", got)
	}
	return s, f, owner
}

func requireSettled(t *testing.T, s *Store, owner, reason string) {
	t.Helper()
	if kinds := openInboxKinds(t, s, owner); len(kinds) != 0 {
		t.Fatalf("inbox still holds %v", kinds)
	}
	rows, err := s.db.Query(`SELECT kind,state,COALESCE(resolved_by,'') FROM inbox_item WHERE agent_id IS NOT NULL AND kind IN ('recovery','delivery_failed')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var kind, state, by string
		if err := rows.Scan(&kind, &state, &by); err != nil {
			t.Fatal(err)
		}
		n++
		if state != "resolved" || by != "system ("+reason+")" {
			t.Fatalf("%s item state=%q resolved_by=%q, want resolved by system (%s)", kind, state, by, reason)
		}
	}
	if n != 2 {
		t.Fatalf("%d items settled, want 2", n)
	}
}

func TestClosedRoomSettlesItsRecoveryAndDeliveryFailure(t *testing.T) {
	s, f, owner := recoveryAndDeliveryFailure(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed',closed_at=? WHERE id=?`, timestamp(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	// The Inbox never lists a failed delivery of a closed room, even before the sweep has settled it. An open recovery stays listed until then:
	// the settle pass may leave one on purpose, and only a person can decide it.
	if kinds := openInboxKinds(t, s, owner); strings.Join(kinds, ",") != "recovery" {
		t.Fatalf("closed room items listed: %v, want only the recovery", kinds)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	requireSettled(t, s, owner, "room closed")
	var note string
	if err := s.db.QueryRow(`SELECT state FROM note WHERE id=?`, f.note.ID).Scan(&note); err != nil || note == "unknown" {
		t.Fatalf("note state %q err=%v", note, err)
	}
}

func TestStoppedAgentSettlesItsRecoveryAndDeliveryFailure(t *testing.T) {
	s, f, owner := recoveryAndDeliveryFailure(t)
	ctx := context.Background()
	card, err := s.AgentCardByID(ctx, f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stopped AgentCard
	if err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		stopped, err = tx.SetAgentDesired(f.agent.ID, owner, "stopped", card.DesiredRev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireSettled(t, s, owner, "agent stopped")
	if stopped.Observed != "stopped" {
		t.Fatalf("card observed=%q, want stopped once nothing waited on it", stopped.Observed)
	}
	var observed string
	if err := s.db.QueryRow(`SELECT observed FROM agent WHERE id=?`, f.agent.ID).Scan(&observed); err != nil || observed != "stopped" {
		t.Fatalf("agent observed=%q err=%v", observed, err)
	}
}

// The stop kills the turn, and the kill raises its recovery only afterwards; the sweep settles what the stop could not see.
func TestSweepSettlesAnItemRaisedAfterTheStop(t *testing.T) {
	s, f, owner := recoveryAndDeliveryFailure(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	requireSettled(t, s, owner, "agent stopped")
}

func TestSweepLeavesALostTurnForAPerson(t *testing.T) {
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "replacement-journal", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if kinds := openInboxKinds(t, s, owner); len(kinds) != 1 || kinds[0] != "recovery" {
		t.Fatalf("a lost turn's recovery must stay for a person (its process may still run): %v", kinds)
	}
}

func attentionQuestion(t *testing.T, s *Store, f executionFixture) InboxEntry {
	t.Helper()
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"Which way?","needs_attention":true}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	items, err := s.InboxForUser(context.Background(), "usr_answers")
	if err != nil || len(items) != 1 || items[0].Kind != "question" {
		t.Fatalf("open question=%+v err=%v", items, err)
	}
	return items[0]
}

func TestAttentionQuestionSettlesOnlyWhenAnsweredInItsThread(t *testing.T) {
	for _, tc := range []struct {
		name    string
		author  func(f executionFixture) (kind, id string)
		replyTo bool
		other   bool
		settled bool
	}{
		{"person replies to it", func(f executionFixture) (string, string) { return "user", "usr_answers" }, true, false, true},
		{"person writes elsewhere in the room", func(f executionFixture) (string, string) { return "user", "usr_answers" }, false, false, false},
		{"asker replies to it", func(f executionFixture) (string, string) { return "agent", f.agent.ID }, true, false, true},
		{"asker writes without replying", func(f executionFixture) (string, string) { return "agent", f.agent.ID }, false, false, false},
		{"person replies to another thread", func(f executionFixture) (string, string) { return "user", "usr_answers" }, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			item := attentionQuestion(t, s, f)
			kind, id := tc.author(f)
			m := Message{RoomID: f.room.ID, AuthorKind: kind, AuthorID: id, Body: "reply"}
			if tc.replyTo {
				m.ReplyTo = item.MessageID
				if tc.other {
					other, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "system", AuthorID: "system", Body: "unrelated"})
					if err != nil {
						t.Fatal(err)
					}
					m.ReplyTo = other.ID
				}
			}
			if _, err := s.InsertMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			state, _, by := inboxState(t, s, item.ID)
			if tc.settled && (state != "superseded" || by != id) || !tc.settled && state != "open" {
				t.Fatalf("state=%q resolved_by=%q, settled want %v", state, by, tc.settled)
			}
			if tc.settled {
				var reason string
				if err := s.db.QueryRow(`SELECT json_extract(payload_json,'$.reason') FROM event WHERE kind='inbox_item_resolved' AND json_extract(payload_json,'$.inbox_item_id')=?`, item.ID).Scan(&reason); err != nil || reason != "answered in thread" {
					t.Fatalf("reason=%q err=%v", reason, err)
				}
			}
		})
	}
}

// Questions answered before this fix stay open in a stored database; the sweep closes them.
func TestSweepClosesAnAttentionQuestionAnsweredEarlier(t *testing.T) {
	s, f := answerFixture(t, false)
	ctx := context.Background()
	item := attentionQuestion(t, s, f)
	// The reply goes in as a row, as it was before supersedeAsks looked at threads.
	if _, err := s.db.Exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,reply_to,thread_root,targets_json,created_at)
		VALUES ('msg_late',(SELECT MAX(seq)+1 FROM message),?,'user','usr_answers','ok',?,?, '[]',?)`, f.room.ID, item.MessageID, item.MessageID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, by := inboxState(t, s, item.ID); state != "superseded" || by != "system (answered in thread)" {
		t.Fatalf("state=%q by=%q", state, by)
	}
}

// After a restart the same supervisor journal says the old process is gone; that has to be on record by the time anyone decides, or the
// continuation waits for a hello that tells it nothing new.
func TestHelloRecordsQuiescenceBeforeAnyDecision(t *testing.T) {
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE machine SET journal_id='intact' WHERE id=?`, f.agent.MachineID); err != nil {
		t.Fatal(err)
	}
	open := map[string]JournalTurn{f.turn.ID: {TurnID: f.turn.ID, Exec: "running", ProcessAlive: false}}
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "intact", []byte(`[]`), open); err != nil {
		t.Fatal(err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].TurnExec != "lost" {
		t.Fatalf("lost recovery item=%+v %v", items, err)
	}
	decision, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "resume-after-restart", "resume", int(items[0].Rev))
	if err != nil || decision.TurnID == "" {
		t.Fatalf("decision=%+v %v", decision, err)
	}
	if kinds := openInboxKinds(t, s, owner); len(kinds) != 0 {
		t.Fatalf("a continuation of a proven-gone process must not wait: %v", kinds)
	}
	delivery, ready, err := s.PrepareDispatch(ctx, f.agent.ID, "")
	if err != nil || !ready || delivery.TurnID != decision.TurnID {
		t.Fatalf("continuation=%+v ready=%t %v", delivery, ready, err)
	}
}

// The wait for the old process says why nothing proved it gone, and goes to people, not the lead: the overlap risk is a person's call.
func TestQuiescenceWaitSaysWhyAndStaysWithPeople(t *testing.T) {
	s, f, owner, lead := workerRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.ReconcileMachineHello(ctx, f.agent.MachineID, "replacement-journal", []byte(`[]`), nil); err != nil {
		t.Fatal(err)
	}
	recoveries, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("lead recoveries=%+v %v", recoveries, err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, recoveries[0].ID, lead.ID, "lead-resume", "resume", "", recoveries[0].Rev); err != nil {
		t.Fatal(err)
	}
	if waiting, err := s.ListLeadRecoveries(ctx, lead.ID, f.project.ID); err != nil || len(waiting) != 0 {
		t.Fatalf("the lead was handed the quiescence wait: %+v %v", waiting, err)
	}
	items, err := s.InboxForUser(ctx, owner)
	if err != nil || len(items) != 1 || items[0].RecoveryPhase != "quiescence_wait" || !strings.Contains(items[0].Question, "journal was replaced") {
		t.Fatalf("people see %+v %v", items, err)
	}
	if _, _, err := s.DecideRecoveryAsAgent(ctx, items[0].ID, lead.ID, "lead-override", "override", "", int(items[0].Rev)); err == nil {
		t.Fatal("the lead overrode the overlap risk")
	}
	if _, _, err := s.DecideRecovery(ctx, items[0].ID, owner, "owner-override", "override", int(items[0].Rev)); err != nil {
		t.Fatalf("owner override: %v", err)
	}
}

// A turn-less recovery (a loop budget) of an agent that is still paused is not moot because its room closed: only a stopped agent settles it.
func TestClosedRoomKeepsTurnlessRecoveryOfAnActiveAgent(t *testing.T) {
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES ('inb_budget',?,?,?,'agent',?,'recovery','{"text":"Loop budget reached","options":["Continue","Abandon"]}',?)`,
		f.project.ID, f.room.ID, f.agent.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed',closed_at=? WHERE id=?`, timestamp(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, "inb_budget"); state != "open" {
		t.Fatalf("budget recovery state=%q, want open while the agent is not stopped", state)
	}
	if kinds := openInboxKinds(t, s, owner); len(kinds) != 1 {
		t.Fatalf("listed %v", kinds)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, "inb_budget"); state != "resolved" {
		t.Fatalf("budget recovery state=%q, want resolved once the agent is stopped", state)
	}
}

// Items the sweep deliberately keeps must not fill its page: a hundred of them in front of a settleable one still let it settle.
func TestSettleSweepIsNotStarvedByKeptItems(t *testing.T) {
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	for i := 0; i < settleItemLimit+5; i++ {
		if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES (?,?,?,?,'agent',?,'recovery','{"text":"kept"}',?)`, fmt.Sprintf("inb_a%03d", i), f.project.ID, f.room.ID, f.agent.ID, f.agent.ID, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,turn_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES ('inb_z_failed',?,?,?,?,'agent',?,'delivery_failed','{"text":"x"}',?)`, f.project.ID, f.room.ID, f.turn.ID, f.agent.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed',closed_at=? WHERE id=?`, timestamp(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleOrphanedInbox(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := inboxState(t, s, "inb_z_failed"); state != "resolved" {
		t.Fatalf("delivery failure behind %d kept items is %q, want resolved", settleItemLimit+5, state)
	}
}

// Settling one recovery must not lift the agent-wide recovery block while the agent has another open recovery.
func TestSettlingOneRecoveryKeepsTheBlockForAnother(t *testing.T) {
	s, f, _ := recoveryFixture(t, false)
	ctx := context.Background()
	if err := s.RecordProcess(ctx, f.turn.ID, f.agent.ID, 0, 143, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,options_json,created_at)
		VALUES ('inb_other',?,?,?,'agent',?,'recovery','{"text":"Loop budget reached"}',?)`, f.project.ID, f.room.ID, f.agent.ID, f.agent.ID, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET recovery_block=1 WHERE id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	if err := s.db.QueryRow(`SELECT id FROM inbox_item WHERE turn_id=? AND kind='recovery' AND state='open'`, f.turn.ID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.settleOrphanedItem(itemID, "room closed") }); err != nil {
		t.Fatal(err)
	}
	var block bool
	if err := s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, f.agent.ID).Scan(&block); err != nil || !block {
		t.Fatalf("recovery_block=%v err=%v, want it kept for the other open recovery", block, err)
	}
}
