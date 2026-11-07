package store

import "testing"

// "Worktree cancelled" is in the system events a person reads, so it is said only when a request was dropped. A spawn that tries a worktree on its
// own and cannot make one (a folder that is not a git repository) clears whatever it started and falls back to the project folder; when it started
// nothing, nothing is cancelled and nothing is said.
func TestCancellingAWorktreeThatWasNeverRequestedSaysNothing(t *testing.T) {
	t.Parallel()
	s, _, room, _, _ := wakeFixture(t)
	ctx := t.Context()
	cancelled := func() int {
		t.Helper()
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE room_id=? AND kind='room_worktree_cancelled'`, room.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if err := s.CancelRoomWorktree(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if got := cancelled(); got != 0 {
		t.Fatalf("a worktree nobody requested was reported cancelled %d times", got)
	}
	// A request that is dropped is said once, and dropping it again, with nothing left, is not said again.
	if _, err := s.RequestRoomWorktree(ctx, room.ID, "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelRoomWorktree(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if got := cancelled(); got != 1 {
		t.Fatalf("a dropped request was reported %d times, want once", got)
	}
	if err := s.CancelRoomWorktree(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if got := cancelled(); got != 1 {
		t.Fatalf("a second cancel with nothing left was reported too: %d", got)
	}
}
