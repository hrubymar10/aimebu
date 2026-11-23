package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

// driftContext dispatches one turn to agent one and returns the worktree_base_drift block of its context ("" when absent).
func driftContext(t *testing.T, s *Store, roomID, agentID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := s.InsertMessage(ctx, Message{RoomID: roomID, AuthorKind: "user", AuthorID: "owner", Body: "@one go"}); err != nil {
		t.Fatal(err)
	}
	got, ready, err := s.PrepareDispatch(ctx, agentID, "")
	if err != nil || !ready {
		t.Fatalf("prepare ready=%t err=%v", ready, err)
	}
	if err := s.CancelPreparedDispatch(ctx, got.TurnID); err != nil {
		t.Fatal(err)
	}
	raw := string(got.Context)
	i := strings.Index(raw, `"worktree_base_drift"`)
	if i < 0 {
		return ""
	}
	return raw[i:]
}

func TestDispatchContextCarriesBaseDriftOnlyAtTheThreshold(t *testing.T) {
	s, _, room, one, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.RequestRoomWorktree(ctx, room.ID, "v2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRoomWorktree(ctx, room.ID, one.MachineID, "/tmp/wake/wt", "v2", "abc1234", "ready", "", 0); err != nil {
		t.Fatal(err)
	}
	var behind int
	var failure error
	done := make(chan struct{}, 8)
	s.drift.compute = func(_ context.Context, dir, baseRef, baseCommit string, min int) (worktree.Drift, error) {
		if dir != "/tmp/wake/wt" || baseRef != "v2" || baseCommit != "abc1234" {
			t.Errorf("lookup of %q %q %q", dir, baseRef, baseCommit)
		}
		if failure != nil {
			return worktree.Drift{}, failure
		}
		d := worktree.Drift{Behind: behind}
		if behind >= min {
			d.Subjects = []string{"newest one", "odd `quote` \"x\""}
		}
		return d, nil
	}
	s.drift.done = func() { done <- struct{}{} }
	refresh := func() {
		s.drift.mu.Lock()
		for _, e := range s.drift.entries {
			e.at = e.at.Add(-2 * baseDriftTTL)
		}
		s.drift.mu.Unlock()
	}
	// The first dispatch only starts the lookup: the transaction never waits for git.
	behind = 7
	if got := driftContext(t, s, room.ID, one.ID); got != "" {
		t.Fatalf("a cold cache must say nothing: %s", got)
	}
	<-done
	got := driftContext(t, s, room.ID, one.ID)
	if !strings.Contains(got, "your worktree is 7 commits behind `v2`") || !strings.Contains(got, "newest one") {
		t.Fatalf("7 behind: %s", got)
	}
	// Below the configured threshold: nothing.
	t.Setenv("AIMEBU_BASE_DRIFT_MIN", "8")
	driftContext(t, s, room.ID, one.ID)
	<-done
	if got := driftContext(t, s, room.ID, one.ID); got != "" {
		t.Fatalf("7 behind with a threshold of 8: %s", got)
	}
	t.Setenv("AIMEBU_BASE_DRIFT_MIN", "7")
	driftContext(t, s, room.ID, one.ID)
	<-done
	if got := driftContext(t, s, room.ID, one.ID); !strings.Contains(got, "7 commits behind") {
		t.Fatalf("7 behind with a threshold of 7: %s", got)
	}
	// 0 turns the line off, whatever the cache holds.
	t.Setenv("AIMEBU_BASE_DRIFT_MIN", "0")
	if got := driftContext(t, s, room.ID, one.ID); got != "" {
		t.Fatalf("disabled: %s", got)
	}
	// An unresolvable base ref says nothing and never fails the dispatch.
	t.Setenv("AIMEBU_BASE_DRIFT_MIN", "")
	failure = errors.New("unknown revision")
	refresh()
	driftContext(t, s, room.ID, one.ID) // serves the stale answer and starts the refresh
	<-done
	if got := driftContext(t, s, room.ID, one.ID); got != "" {
		t.Fatalf("unresolvable base: %s", got)
	}
}
