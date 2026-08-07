package server

import (
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestRetentionDefaultsMatchCurrentBehavior(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if got := s.staleAgentWindow(); got != 30*time.Minute {
		t.Fatalf("stale agent window = %s, want 30m", got)
	}
	if got := s.cleanupInterval(); got != time.Minute {
		t.Fatalf("cleanup interval = %s, want 1m", got)
	}
}

func TestCleanupStaleAgentsHonorsConfiguredWindow(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	window := 120
	s.putSettings(Settings{StaleAgentWindowSeconds: &window})

	now := time.Now().UTC()
	s.mu.Lock()
	s.agents["stale@aimebu"] = &types.Agent{ID: "stale@aimebu", Name: "stale", Kind: "ai", LastSeen: now.Add(-3 * time.Minute).Format(time.RFC3339)}
	s.agents["fresh@aimebu"] = &types.Agent{ID: "fresh@aimebu", Name: "fresh", Kind: "ai", LastSeen: now.Add(-90 * time.Second).Format(time.RFC3339)}
	s.rooms["general"] = &types.Room{ID: "general", Members: []string{"stale@aimebu", "fresh@aimebu"}}
	s.mu.Unlock()

	s.cleanupStaleAgents()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.agents["stale@aimebu"]; ok {
		t.Fatal("stale agent was not removed")
	}
	if _, ok := s.agents["fresh@aimebu"]; !ok {
		t.Fatal("fresh agent was removed")
	}
	if got := s.rooms["general"].Members; len(got) != 1 || got[0] != "fresh@aimebu" {
		t.Fatalf("room members = %v, want fresh only", got)
	}
}

func TestHeartbeatPreventsStaleAgentCleanup(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	window := 120
	s.putSettings(Settings{StaleAgentWindowSeconds: &window})

	now := time.Now().UTC()
	s.mu.Lock()
	s.agents["idle@aimebu"] = &types.Agent{ID: "idle@aimebu", Name: "idle", Kind: "ai", LastSeen: now.Add(-3 * time.Minute).Format(time.RFC3339)}
	s.rooms["general"] = &types.Room{ID: "general", Members: []string{"idle@aimebu"}}
	s.mu.Unlock()

	if !s.heartbeatAgent("idle@aimebu") {
		t.Fatal("heartbeatAgent returned false")
	}
	s.cleanupStaleAgents()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.agents["idle@aimebu"]; !ok {
		t.Fatal("heartbeat-refreshed agent was removed")
	}
	if got := s.rooms["general"].Members; len(got) != 1 || got[0] != "idle@aimebu" {
		t.Fatalf("room members = %v, want idle agent", got)
	}
}

// TestEmptyRoomSurvivesSweepAndRestart guards the retention rework:
// rooms are never auto-deleted, by timer (sweep) or by restart. Previously
// cleanupEmptyRooms deleted an empty room and all its messages after an hour,
// and pruneOnStartup deleted any empty room (with all its messages) on every
// restart — silently, logging only the agent count. Both paths destroyed
// real history (the restart path is what took out the user's history on
// 2026-08-07).
func TestEmptyRoomSurvivesSweepAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	// A room with no members but real history — exactly what used to vanish.
	s.rooms["lonely"] = &types.Room{ID: "lonely"}
	s.messages["lonely"] = []types.Message{
		retentionTestMessage(1, "lonely", now.Add(-3*time.Hour)),
		retentionTestMessage(2, "lonely", now.Add(-time.Hour)),
		retentionTestMessage(3, "lonely", now),
	}
	s.persistFullCoreLocked()
	s.mu.Unlock()

	// A sweep must not delete an empty room or its messages. The sweep loop
	// now runs cleanupStaleAgents + cleanupMessages only.
	s.cleanupStaleAgents()
	s.cleanupMessages()

	s.mu.RLock()
	if _, ok := s.rooms["lonely"]; !ok {
		t.Fatal("empty room was deleted by sweep")
	}
	if got := len(s.messages["lonely"]); got != 3 {
		t.Fatalf("messages deleted by sweep: got %d, want 3", got)
	}
	s.mu.RUnlock()

	// A restart (newStore -> pruneOnStartup) must not delete it either. This
	// is the path that destroyed the user's history: it deleted every empty room
	// and its messages, reporting nothing but the agent count.
	restarted, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted.mu.RLock()
	defer restarted.mu.RUnlock()
	if _, ok := restarted.rooms["lonely"]; !ok {
		t.Fatal("empty room was deleted on restart")
	}
	if got := len(restarted.messages["lonely"]); got != 3 {
		t.Fatalf("messages deleted on restart: got %d, want 3", got)
	}
}

// TestCleanupMessagesReapsOrphanReactionsAndKeepsMessages verifies the
// post-retention-rework cleanupMessages: it no longer deletes messages (even
// very old ones survive a sweep — read-time expiry hides them, not deletion),
// but it still reaps orphan reactions (reactions for messages that no longer
// exist, e.g. after an explicit room delete).
func TestCleanupMessagesReapsOrphanReactionsAndKeepsMessages(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["general"] = &types.Room{ID: "general", Members: []string{"alex"}}
	s.messages["general"] = []types.Message{
		retentionTestMessage(1, "general", now.Add(-365*24*time.Hour)), // very old — must NOT be deleted
		retentionTestMessage(2, "general", now),
	}
	s.persistFullCoreLocked()
	s.mu.Unlock()
	s.reactionsMu.Lock()
	s.reactions[1] = []types.Reaction{{AgentID: "alex", Emoji: "👍", CreatedAt: now.Format(time.RFC3339)}}   // live
	s.reactions[999] = []types.Reaction{{AgentID: "alex", Emoji: "✅", CreatedAt: now.Format(time.RFC3339)}} // orphan (no message 999)
	s.persistReactionsLocked()
	s.reactionsMu.Unlock()

	s.cleanupMessages()

	// Messages all survive — no retention deletion anymore.
	s.mu.RLock()
	if got := len(s.messages["general"]); got != 2 {
		t.Fatalf("cleanupMessages deleted messages: got %d, want 2 (no retention deletion)", got)
	}
	s.mu.RUnlock()

	// Orphan reaction reaped; live reaction kept.
	s.reactionsMu.RLock()
	if _, ok := s.reactions[999]; ok {
		t.Fatal("orphan reaction for non-existent message 999 was not reaped")
	}
	if _, ok := s.reactions[1]; !ok {
		t.Fatal("live reaction for message 1 was reaped")
	}
	s.reactionsMu.RUnlock()

	// Persists across reload.
	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.reactionsMu.RLock()
	defer reloaded.reactionsMu.RUnlock()
	if _, ok := reloaded.reactions[999]; ok {
		t.Fatal("persisted reactions still contain orphan message 999")
	}
	if _, ok := reloaded.reactions[1]; !ok {
		t.Fatal("persisted reactions dropped live message 1")
	}
}

func retentionTestMessage(id int64, roomID string, createdAt time.Time) types.Message {
	return types.Message{
		ID:        id,
		RoomID:    roomID,
		From:      "alex",
		FromKind:  "human",
		Body:      "test",
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
	}
}
