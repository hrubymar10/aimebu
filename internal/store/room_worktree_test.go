package store

import "testing"

func TestRoomWorktreeInUseWhileAnyAgentUsesTheCheckout(t *testing.T) {
	t.Parallel()
	s, _, room, agent, other := wakeFixture(t)
	ctx := t.Context()
	path := "/tmp/wake/.aimebu/worktrees/" + room.ID
	if _, err := s.RequestRoomWorktree(ctx, room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRoomWorktree(ctx, room.ID, agent.MachineID, path, "main", "base", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE checkout SET canonical_path=? WHERE id=?`, path, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET home_room_id=NULL WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if inUse, err := s.RoomWorktreeInUse(ctx, agent.MachineID, path); err != nil || !inUse {
		t.Fatalf("active checkout in use=%v err=%v", inUse, err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if inUse, err := s.RoomWorktreeInUse(ctx, agent.MachineID, path); err != nil || inUse {
		t.Fatalf("stopped checkout in use=%v err=%v", inUse, err)
	}
}
