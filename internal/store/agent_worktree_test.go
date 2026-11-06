package store

import (
	"errors"
	"testing"
)

func TestRecordAgentWorktreeMovesTheAgentToItsOwnCheckout(t *testing.T) {
	t.Parallel()
	s, project, room, agent, _ := wakeFixture(t)
	ctx := t.Context()
	if name := AgentWorktreeName(room.ID, agent.Slug); name != room.ID+"-one" {
		t.Fatalf("name %q", name)
	}
	if mode, err := s.ProjectWorktreeMode(ctx, project.ID); err != nil || mode != "auto" {
		t.Fatalf("mode %q err=%v", mode, err)
	}
	// The agent holds the writer slot of the room's checkout until it is moved.
	if _, err := s.db.Exec(`UPDATE checkout SET writer_state='reserved',writer_agent_id=? WHERE id=?`, agent.ID, agent.CheckoutID); err != nil {
		t.Fatal(err)
	}
	path := "/tmp/wake/.aimebu/worktrees/" + room.ID + "-one"
	worktree := AgentWorktree{AgentID: agent.ID, RoomID: room.ID, MachineID: agent.MachineID, Name: AgentWorktreeName(room.ID, "one"), Branch: "aimebu/" + room.ID + "-one", Path: path, BaseRef: "main", BaseCommit: "abc"}
	record := func() {
		t.Helper()
		if err := s.Tx(ctx, func(tx *Tx) error { return tx.RecordAgentWorktree(worktree) }); err != nil {
			t.Fatal(err)
		}
	}
	record()
	record() // recording again changes nothing
	got, found, err := s.AgentWorktreeOf(ctx, agent.ID)
	if err != nil || !found || got.Path != path || got.Branch != worktree.Branch || got.State != "ready" || got.BaseCommit != "abc" {
		t.Fatalf("worktree %+v found=%v err=%v", got, found, err)
	}
	var checkout, canonical, state string
	if err := s.db.QueryRow(`SELECT a.checkout_id,c.canonical_path,c.writer_state FROM agent a JOIN checkout c ON c.id=a.checkout_id WHERE a.id=?`, agent.ID).Scan(&checkout, &canonical, &state); err != nil {
		t.Fatal(err)
	}
	if checkout == agent.CheckoutID || canonical != path || state != "none" {
		t.Fatalf("the agent should point at a checkout of its own path: %s %s %s", checkout, canonical, state)
	}
	var old string
	if err := s.db.QueryRow(`SELECT writer_state FROM checkout WHERE id=?`, agent.CheckoutID).Scan(&old); err != nil || old != "none" {
		t.Fatalf("the room's checkout should be let go of: %q err=%v", old, err)
	}
	// A spelling of the old folder means nothing in the new one.
	if _, err := s.db.Exec(`UPDATE agent SET launch_path='/old/link' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	moved := worktree
	moved.Path = path + "-again"
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.RecordAgentWorktree(moved) }); err != nil {
		t.Fatal(err)
	}
	var launch string
	if err := s.db.QueryRow(`SELECT launch_path FROM agent WHERE id=?`, agent.ID).Scan(&launch); err != nil || launch != "" {
		t.Fatalf("launch_path %q err=%v", launch, err)
	}
	// A room with a worktree on disk cannot be removed, closed or not; once the worktree is removed it can.
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRoom(ctx, room.ID); !errors.Is(err, ErrRoomNotRemovable) {
		t.Fatalf("a room with an agent worktree must not be removed: %v", err)
	}
	if err := s.SetAgentWorktreeState(ctx, agent.ID, "removed"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.AgentWorktreeOf(ctx, agent.ID); got.State != "removed" {
		t.Fatalf("state %q", got.State)
	}
	if _, found, _ := s.AgentWorktreeOf(ctx, "agt_nobody"); found {
		t.Fatal("an agent without a worktree has none")
	}
}

func TestDetachAgentWorktreeSendsTheAgentBackToItsRoomCheckout(t *testing.T) {
	t.Parallel()
	s, _, room, agent, _ := wakeFixture(t)
	ctx := t.Context()
	own := AgentWorktree{AgentID: agent.ID, RoomID: room.ID, MachineID: agent.MachineID, Name: room.ID + "-one", Branch: "aimebu/" + room.ID + "-one", Path: "/tmp/wake/.aimebu/worktrees/" + room.ID + "-one"}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.RecordAgentWorktree(own) }); err != nil {
		t.Fatal(err)
	}
	// An agent that is not stopped keeps its folder: a removal that raced a start must not send it to a checkout whose writer slot it never took.
	if err := s.DetachAgentWorktree(ctx, agent.ID, agent.MachineID, "/tmp/wake"); !errors.Is(err, ErrConflict) {
		t.Fatalf("detaching an agent that is not stopped: %v", err)
	}
	if got, _, _ := s.AgentWorktreeOf(ctx, agent.ID); got.State != "ready" || got.Path != own.Path {
		t.Fatalf("the refused detach must leave the record alone: %+v", got)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DetachAgentWorktree(ctx, agent.ID, agent.MachineID, "/tmp/wake"); err != nil {
		t.Fatal(err)
	}
	if path, err := s.AgentCheckoutPath(ctx, agent.ID); err != nil || path != "/tmp/wake" {
		t.Fatalf("checkout %q err=%v", path, err)
	}
	if got, found, err := s.AgentWorktreeOf(ctx, agent.ID); err != nil || !found || got.State != "removed" {
		t.Fatalf("worktree %+v found=%v err=%v", got, found, err)
	}
	if err := s.DetachAgentWorktree(ctx, "agt_nobody", agent.MachineID, "/tmp/wake"); err == nil {
		t.Fatal("an agent that does not exist cannot be detached")
	}
}
