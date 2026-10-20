package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPrivateChatPairMembershipAndRoomTurns(t *testing.T) {
	t.Parallel()
	s, project, task, agent, _ := wakeFixture(t)
	ctx := context.Background()
	for _, user := range []string{"usr_peer", "usr_other"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user, user); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, user); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	hidden := true
	if _, err := s.SetRoomViewPreference(ctx, "usr_peer", dm.ID, &hidden); err != nil {
		t.Fatal(err)
	}
	again, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil || again.ID != dm.ID || dm.Kind != "dm" {
		t.Fatalf("dm=%+v replay=%+v err=%v", dm, again, err)
	}
	peerRooms, err := s.RoomsForUserProject(ctx, project.ID, "usr_peer")
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range peerRooms {
		if room.ID == dm.ID && room.Hidden {
			t.Fatal("reopening a DM left it hidden")
		}
	}
	if err := s.UserRoomAccess(ctx, "usr_other", dm.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other access=%v", err)
	}
	if err := s.UserRoomAccess(ctx, "usr_peer", dm.ID); err != nil {
		t.Fatalf("peer access=%v", err)
	}
	var members int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM room_member WHERE room_id=? AND left_at IS NULL`, dm.ID).Scan(&members); err != nil || members != 2 {
		t.Fatalf("DM members=%d %v", members, err)
	}
	rooms, err := s.RoomsForUserProject(ctx, project.ID, "usr_other")
	if err != nil {
		t.Fatal(err)
	}
	for _, room := range rooms {
		if room.ID == dm.ID {
			t.Fatal("private room in another member's list")
		}
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: dm.ID, AuthorKind: "user", AuthorID: "usr_peer", Body: "private"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "usr_peer", Body: "room work"}); err != nil {
		t.Fatal(err)
	}
	first, ready, err := s.PrepareDispatch(ctx, agent.ID, "")
	if err != nil || !ready || len(first.Notes) != 1 {
		t.Fatalf("first=%+v ready=%t err=%v", first, ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, first.TurnID); err != nil {
		t.Fatal(err)
	}
	second, ready, err := s.PrepareDispatch(ctx, agent.ID, first.Notes[0].RoomID)
	if err != nil || !ready || len(second.Notes) != 1 || first.TurnID == second.TurnID || first.Notes[0].RoomID == second.Notes[0].RoomID {
		t.Fatalf("separate room turns: first=%+v second=%+v ready=%t err=%v", first, second, ready, err)
	}
	events, err := s.EventsAfter(ctx, "usr_other", 0, 1<<60)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.RoomID == dm.ID {
			t.Fatal("private event visible to another member")
		}
	}
	batch, err := s.EventsBatchAfter(ctx, "usr_other", 0, 1<<60, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range batch {
		if event.RoomID == dm.ID {
			t.Fatal("private event visible in streamed batch")
		}
	}
}

func TestPrivateTurnCannotAnswerAnotherJoinedRoom(t *testing.T) {
	t.Parallel()
	s, f := answerFixture(t, false)
	dm, err := s.OpenDM(context.Background(), "usr_answers", f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET room_id=? WHERE id=?`, dm.ID, f.turn.ID); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"messages":[{"room":%q,"body":"leak"}]}`, f.room.ID)
	receipt, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, raw)
	if err != nil || receipt.Result == "committed" {
		t.Fatalf("cross-room answer=%+v %v", receipt, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), f.room.ID, 0)
	if err != nil || len(messages) != 1 {
		t.Fatalf("private output escaped: %+v %v", messages, err)
	}
}

func TestPrivateInboxOnlyHumanPeerCanAnswer(t *testing.T) {
	t.Parallel()
	s, project, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	for _, user := range []string{"usr_peer", "usr_other"} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user, user); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, project.ID, user); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	question, err := s.InsertMessage(ctx, Message{RoomID: dm.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "Which path?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_private',?,?,?,?,'agent',?,'question','now')`, project.ID, dm.ID, question.ID, agent.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE membership SET role='viewer' WHERE project_id=? AND user_id='usr_peer'`, project.ID); err != nil {
		t.Fatal(err)
	}
	other, err := s.InboxForUser(ctx, "usr_other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other inbox=%+v %v", other, err)
	}
	peer, err := s.InboxForUser(ctx, "usr_peer")
	if err != nil || len(peer) != 1 {
		t.Fatalf("peer inbox=%+v %v", peer, err)
	}
	if !peer[0].CanDecide {
		t.Fatal("private peer cannot answer in the inbox")
	}
	if _, _, err := s.AnswerInbox(ctx, "inb_private", "usr_other", "other-key", "leak", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other answer=%v", err)
	}
	if _, _, err := s.AnswerInbox(ctx, "inb_private", "usr_peer", "peer-key", "This one", 0); err != nil {
		t.Fatalf("peer answer=%v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at)
		VALUES ('inb_private_approval',?,?,?,?,'agent',?,'approval','now')`, project.ID, dm.ID, question.ID, agent.ID, agent.ID); err != nil {
		t.Fatal(err)
	}
	peer, err = s.InboxForUser(ctx, "usr_peer")
	if err != nil || len(peer) != 1 || !peer[0].CanDecide {
		t.Fatalf("private approval inaccessible to peer: %+v %v", peer, err)
	}
	if _, _, err := s.AnswerInbox(ctx, "inb_private_approval", "usr_other", "other-approval", "leak", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-peer approval=%v", err)
	}
	if _, _, err := s.AnswerInbox(ctx, "inb_private_approval", "usr_peer", "peer-approval", "Approved", 0); err != nil {
		t.Fatalf("peer approval=%v", err)
	}
}

func TestPrivateChatReopensForSamePair(t *testing.T) {
	t.Parallel()
	s, project, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES
		('usr_peer','peer','hash','{}','now'),('usr_owner','owner','hash','{}','now'),
		('usr_operator','operator','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES
		(?,'usr_peer','viewer'),(?,'usr_owner','owner'),(?,'usr_operator','operator')`, project.ID, project.ID, project.ID); err != nil {
		t.Fatal(err)
	}
	dm, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginRoomClose(ctx, dm.ID, "usr_operator", dm.LifecycleRev, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-peer operator close=%v", err)
	}
	closing, _, err := s.BeginRoomClose(ctx, dm.ID, "usr_peer", dm.LifecycleRev, false)
	if err != nil || closing.Lifecycle != "closing" {
		t.Fatalf("close=%+v %v", closing, err)
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, time.Second); err != nil || len(failures) != 0 {
		t.Fatalf("advance=%v %v", failures, err)
	}
	closed, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil || closed.ID != dm.ID || closed.Lifecycle != "closed" {
		t.Fatalf("closed pair=%+v %v", closed, err)
	}
	if _, err := s.ReopenRoom(ctx, dm.ID, "usr_operator", closed.LifecycleRev); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-peer operator reopen=%v", err)
	}
	reopened, err := s.ReopenRoom(ctx, dm.ID, "usr_owner", closed.LifecycleRev)
	if err != nil || reopened.ID != dm.ID || reopened.Lifecycle != "open" {
		t.Fatalf("owner reopen=%+v %v", reopened, err)
	}
	closing, _, err = s.BeginRoomClose(ctx, dm.ID, "usr_owner", reopened.LifecycleRev, false)
	if err != nil || closing.Lifecycle != "closing" {
		t.Fatalf("owner close=%+v %v", closing, err)
	}
	if _, failures, err := s.AdvanceRoomClosures(ctx, time.Second); err != nil || len(failures) != 0 {
		t.Fatalf("advance=%v %v", failures, err)
	}
	closed, err = s.RoomByID(ctx, dm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenRoom(ctx, dm.ID, "usr_peer", closed.LifecycleRev); err != nil {
		t.Fatalf("viewer peer reopen=%v", err)
	}
}

func TestPrivateRecoveryIsReadOnlyForViewerPeerAndDecidableByOwner(t *testing.T) {
	t.Parallel()
	s, project, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	for _, user := range []struct{ id, role string }{
		{"usr_peer", "viewer"}, {"usr_owner", "owner"}, {"usr_operator", "operator"}, {"usr_other", "viewer"},
	} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?, 'hash','{}','now')`, user.id, user.id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, project.ID, user.id, user.role); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDM(ctx, "usr_peer", agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"question", "recovery"} {
		if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,requester_kind,requester_id,kind,created_at)
			VALUES (?,?,?,?, 'agent',?,?,?)`, "inb_dm_"+kind, project.ID, dm.ID, agent.ID, agent.ID, kind, timestamp()); err != nil {
			t.Fatal(err)
		}
		if err := s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: project.ID, RoomID: dm.ID,
				AgentID: agent.ID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": "inb_dm_" + kind})})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, events := range []struct {
		name string
		read func() ([]Event, error)
	}{
		{"all", func() ([]Event, error) { return s.EventsAfter(ctx, "usr_owner", 0, 1<<60) }},
		{"batch", func() ([]Event, error) { return s.EventsBatchAfter(ctx, "usr_owner", 0, 1<<60, 1000) }},
	} {
		rows, err := events.read()
		if err != nil {
			t.Fatal(err)
		}
		var recoveryEvents int
		for _, event := range rows {
			if event.RoomID != dm.ID {
				continue
			}
			if !strings.Contains(event.PayloadJSON, "inb_dm_recovery") {
				t.Fatalf("%s leaked private event=%+v", events.name, event)
			}
			recoveryEvents++
		}
		if recoveryEvents != 1 {
			t.Fatalf("%s recovery events=%d", events.name, recoveryEvents)
		}
	}
	for _, check := range []struct {
		user      string
		count     int
		canDecide bool
	}{
		{"usr_peer", 2, false}, {"usr_owner", 1, true}, {"usr_operator", 1, true}, {"usr_other", 0, false},
	} {
		items, err := s.InboxForUser(ctx, check.user)
		if err != nil || len(items) != check.count {
			t.Fatalf("%s inbox=%+v %v", check.user, items, err)
		}
		for _, item := range items {
			if item.Kind == "recovery" && item.CanDecide != check.canDecide {
				t.Fatalf("%s recovery=%+v", check.user, item)
			}
			if check.user != "usr_peer" && item.Kind != "recovery" {
				t.Fatalf("private question leaked to %s", check.user)
			}
		}
	}
	if _, _, err := s.DecideRecovery(ctx, "inb_dm_recovery", "usr_peer", "viewer-decision", "abandon", 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer decision=%v", err)
	}
	if _, _, err := s.DecideRecovery(ctx, "inb_dm_recovery", "usr_owner", "owner-decision", "abandon", 0); err != nil {
		t.Fatalf("owner decision=%v", err)
	}
}
