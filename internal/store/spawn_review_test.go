package store

import (
	"context"
	"testing"
)

func TestPendingSpawnsSkipStoppedAgents(t *testing.T) {
	t.Parallel()
	s, _, _, stopped, active := wakeFixture(t)
	ctx := context.Background()
	for _, agent := range []Agent{stopped, active} {
		if err := s.Tx(ctx, func(tx *Tx) error {
			_, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", AgentID: agent.ID})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped' WHERE id=?`, stopped.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingSpawnsForMachine(ctx, stopped.MachineID)
	if err != nil || len(pending) != 1 || pending[0].AgentID != active.ID {
		t.Fatalf("pending spawns=%+v %v", pending, err)
	}
	if _, found, err := s.PendingSpawnByID(ctx, stopped.ID); err != nil || found {
		t.Fatalf("stopped pending=%t %v", found, err)
	}
}

func TestFailedSpawnBecomesTerminalForGeneration(t *testing.T) {
	t.Parallel()
	s, _, _, agent, _ := wakeFixture(t)
	ctx := context.Background()
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "agent_spawn_requested", AgentID: agent.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FailSpawn(ctx, agent.ID, 0, "adapter rejected spawn"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.PendingSpawnByID(ctx, agent.ID); err != nil || found {
		t.Fatalf("failed spawn still pending=%t %v", found, err)
	}
	var failures int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE agent_id=? AND kind='agent_spawn_failed'`, agent.ID).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("spawn failure event=%d %v", failures, err)
	}
}
