package server

import (
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A remote supervisor that lost its journal file says hello with a new journal_id. The turns the lost journal owned are written off, with the
// usual recovery, once its instance has been gone for ten minutes, and not before: it may still reconnect and report them.
func TestRemoteLostJournalWritesOffOnlyAnInstanceGoneForTenMinutes(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	agent, turn, _, delivery := f.execution(t)
	hello := []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}
	old, _ := dialInstance(t, f, "lost-journal", "first", nil, hello)
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.Deliver })
	owner, err := f.db.AgentOpenTurnOwner(t.Context(), agent.ID)
	if err != nil || owner != "lost-journal" {
		t.Fatalf("turn owner %q: %v", owner, err)
	}
	before, _ := f.db.SupervisorInstances(t.Context(), f.machineID)
	_ = old.CloseNow()
	// The disconnect stamps the instance: ten minutes count from there, not from its hello.
	deadline := time.Now().Add(5 * time.Second)
	for {
		now, _ := f.db.SupervisorInstances(t.Context(), f.machineID)
		if len(now) == 1 && now[0].LastSeenAt != before[0].LastSeenAt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the disconnect was not recorded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	state := func(journal string) string {
		list, _ := f.db.SupervisorInstances(t.Context(), f.machineID)
		for _, item := range list {
			if item.JournalID == journal {
				return item.State
			}
		}
		return ""
	}

	// Just under ten minutes: nothing is lost, and the agent stays blocked behind the old instance.
	f.srv.InstanceClock = func() time.Time { return time.Now().Add(9*time.Minute + 30*time.Second) }
	young, _ := dialInstance(t, f, "replacement-1", "first", nil, hello)
	if state("lost-journal") == "gone" {
		t.Fatal("an instance gone for under ten minutes was written off")
	}
	if owner, _ := f.db.AgentOpenTurnOwner(t.Context(), agent.ID); owner != "lost-journal" {
		t.Fatalf("the turn was lost early: owner %q", owner)
	}
	if blocked, err := f.srv.handoverBlocked(t.Context(), f.machineID, agent.ID); err != nil || !blocked {
		t.Fatalf("handover gate %v: %v", blocked, err)
	}
	_ = young.CloseNow()

	// Over ten minutes: the lost journal's turn is lost with recovery, and the gate opens.
	f.srv.InstanceClock = func() time.Time { return time.Now().Add(10*time.Minute + 30*time.Second) }
	current, _ := dialInstance(t, f, "replacement-2", "first", nil, hello)
	if state("lost-journal") != "gone" {
		t.Fatalf("lost journal state %q", state("lost-journal"))
	}
	if state("replacement-2") == "gone" {
		t.Fatal("the connecting instance was written off")
	}
	if owner, _ := f.db.AgentOpenTurnOwner(t.Context(), agent.ID); owner != "" {
		t.Fatalf("the turn is still open: owner %q", owner)
	}
	if blocked, err := f.srv.handoverBlocked(t.Context(), f.machineID, agent.ID); err != nil || blocked {
		t.Fatalf("handover gate %v: %v", blocked, err)
	}
	person, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.db.EventsAfter(t.Context(), person.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, event := range events {
		if event.Kind == "supervisor_journal_changed" && event.TurnID == turn.ID {
			changed = true
		}
	}
	if !changed {
		t.Fatal("the lost turn got the journal-change recovery event")
	}
	_ = current
}

// An instance that is still connected is not written off by another journal's hello, however old its own hello is.
func TestRemoteLostJournalKeepsAConnectedInstance(t *testing.T) {
	f := newFixture(t)
	f.srv.Version = "first"
	agent, _, _, delivery := f.execution(t)
	hello := []protocol.HelloAgent{{AgentID: agent.ID, Generation: 0}}
	old, _ := dialInstance(t, f, "connected", "first", nil, hello)
	if err := f.srv.Dispatch(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
	readCommandFrame(t, old, func(frame protocol.Frame) bool { return frame.Type == protocol.Deliver })
	f.srv.InstanceClock = func() time.Time { return time.Now().Add(time.Hour) }
	newer, _ := dialInstance(t, f, "newer", "first", nil, hello)
	list, _ := f.db.SupervisorInstances(t.Context(), f.machineID)
	for _, item := range list {
		if item.State == "gone" {
			t.Fatalf("%s was written off while connected", item.JournalID)
		}
	}
	if owner, _ := f.db.AgentOpenTurnOwner(t.Context(), agent.ID); owner != "connected" {
		t.Fatalf("turn owner %q", owner)
	}
	_ = newer
}
