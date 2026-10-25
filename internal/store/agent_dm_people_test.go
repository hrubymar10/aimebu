package store

import (
	"context"
	"errors"
	"testing"
)

func TestAgentDMClosedChatIsNeitherOfferedNorJoinable(t *testing.T) {
	t.Parallel()
	f := newDMFixture(t)
	ctx := context.Background()
	room := f.open(t, f.lead1, f.worker2)
	offers, err := f.s.AgentDMOffers(ctx, f.bob)
	if err != nil || len(offers) != 1 || offers[0].ID != room.ID {
		t.Fatalf("an open chat: %+v err=%v", offers, err)
	}
	f.join(t, room.ID, f.alice)
	if _, err := f.s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if offers, err := f.s.AgentDMOffers(ctx, f.bob); err != nil || len(offers) != 0 {
		t.Fatalf("a closed chat is offered: %+v err=%v", offers, err)
	}
	if err := f.s.JoinAgentDM(ctx, f.bob, room.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a closed chat was joined: %v", err)
	}
	// Someone who was in it keeps reading what was said.
	if err := f.s.UserRoomAccess(ctx, f.alice, room.ID); err != nil {
		t.Fatalf("a member lost a closed chat: %v", err)
	}
	if rooms, err := f.s.AgentDMsForUser(ctx, f.alice); err != nil || len(rooms) != 1 {
		t.Fatalf("a member's list: %+v err=%v", rooms, err)
	}
}
