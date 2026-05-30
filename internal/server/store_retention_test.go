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
	if got := s.emptyRoomWindow(); got != 60*time.Minute {
		t.Fatalf("empty room window = %s, want 60m", got)
	}
	if got := s.cleanupInterval(); got != time.Minute {
		t.Fatalf("cleanup interval = %s, want 1m", got)
	}
	if got := s.messageRetentionWindow(); got != 0 {
		t.Fatalf("message retention window = %s, want unlimited", got)
	}
	if got := s.messageRetentionCount(); got != 0 {
		t.Fatalf("message retention count = %d, want unlimited", got)
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

func TestCleanupEmptyRoomsHonorsConfiguredWindow(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	window := 120
	s.putSettings(Settings{EmptyRoomWindowSeconds: &window})

	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["old-empty"] = &types.Room{ID: "old-empty"}
	s.rooms["new-empty"] = &types.Room{ID: "new-empty"}
	s.roomEmptySince["old-empty"] = now.Add(-3 * time.Minute)
	s.roomEmptySince["new-empty"] = now.Add(-90 * time.Second)
	s.mu.Unlock()

	s.cleanupEmptyRooms()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.rooms["old-empty"]; ok {
		t.Fatal("old empty room was not removed")
	}
	if _, ok := s.rooms["new-empty"]; !ok {
		t.Fatal("new empty room was removed")
	}
}

func TestCleanupMessagesHonorsAgeAndCount(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	retentionSeconds := 120
	retentionCount := 2
	s.putSettings(Settings{
		MessageRetentionSeconds: &retentionSeconds,
		MessageRetentionCount:   &retentionCount,
	})

	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["general"] = &types.Room{ID: "general", Members: []string{"matin"}}
	s.messages["general"] = []types.Message{
		retentionTestMessage(1, "general", now.Add(-5*time.Minute)),
		retentionTestMessage(2, "general", now.Add(-90*time.Second)),
		retentionTestMessage(3, "general", now.Add(-30*time.Second)),
		retentionTestMessage(4, "general", now),
	}
	s.mu.Unlock()
	s.reactionsMu.Lock()
	s.reactions[1] = []types.Reaction{{AgentID: "matin", Emoji: "👍", CreatedAt: now.Format(time.RFC3339)}}
	s.reactions[4] = []types.Reaction{{AgentID: "matin", Emoji: "✅", CreatedAt: now.Format(time.RFC3339)}}
	s.reactionsMu.Unlock()

	s.cleanupMessages()

	s.mu.RLock()
	defer s.mu.RUnlock()
	got := s.messages["general"]
	if len(got) != 2 {
		t.Fatalf("kept %d messages, want 2: %+v", len(got), got)
	}
	if got[0].ID != 3 || got[1].ID != 4 {
		t.Fatalf("kept IDs = [%d %d], want [3 4]", got[0].ID, got[1].ID)
	}

	s.reactionsMu.RLock()
	defer s.reactionsMu.RUnlock()
	if _, ok := s.reactions[1]; ok {
		t.Fatal("reaction for pruned message 1 was not removed")
	}
	if _, ok := s.reactions[4]; !ok {
		t.Fatal("reaction for kept message 4 was removed")
	}
}

func TestCleanupMessagesUnlimitedByDefault(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s.mu.Lock()
	s.messages["general"] = []types.Message{
		retentionTestMessage(1, "general", now.Add(-365*24*time.Hour)),
		retentionTestMessage(2, "general", now),
	}
	s.mu.Unlock()

	s.cleanupMessages()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if got := len(s.messages["general"]); got != 2 {
		t.Fatalf("kept %d messages with default unlimited retention, want 2", got)
	}
}

func TestCleanupMessagesRemovesOrphanReactions(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	retentionCount := 1
	s.putSettings(Settings{MessageRetentionCount: &retentionCount})

	now := time.Now().UTC()
	s.mu.Lock()
	s.rooms["general"] = &types.Room{ID: "general", Members: []string{"matin"}}
	s.messages["general"] = []types.Message{
		retentionTestMessage(1, "general", now.Add(-time.Minute)),
		retentionTestMessage(2, "general", now),
	}
	s.mu.Unlock()
	s.reactionsMu.Lock()
	s.reactions[1] = []types.Reaction{{AgentID: "matin", Emoji: "👍", CreatedAt: now.Format(time.RFC3339)}}
	s.reactions[2] = []types.Reaction{{AgentID: "matin", Emoji: "✅", CreatedAt: now.Format(time.RFC3339)}}
	s.persistReactionsLocked()
	s.reactionsMu.Unlock()

	s.cleanupMessages()

	s.reactionsMu.RLock()
	if _, ok := s.reactions[1]; ok {
		t.Fatal("reaction for pruned message 1 was not removed")
	}
	if _, ok := s.reactions[2]; !ok {
		t.Fatal("reaction for kept message 2 was removed")
	}
	s.reactionsMu.RUnlock()

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.reactionsMu.RLock()
	defer reloaded.reactionsMu.RUnlock()
	if _, ok := reloaded.reactions[1]; ok {
		t.Fatal("persisted reactions still contain pruned message 1")
	}
	if _, ok := reloaded.reactions[2]; !ok {
		t.Fatal("persisted reactions dropped kept message 2")
	}
}

func retentionTestMessage(id int64, roomID string, createdAt time.Time) types.Message {
	return types.Message{
		ID:        id,
		RoomID:    roomID,
		From:      "matin",
		FromKind:  "human",
		Body:      "test",
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
	}
}
