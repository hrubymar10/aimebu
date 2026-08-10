package server

import "testing"

// TestRoomSendPersistsSenderHarnessAndModel verifies that roomSend captures the
// sender's harness and model on the message at send time, so a later
// deregistered sender keeps its icon identity instead of falling back to the
// unknown.svg "?". Both fields come from the same agent record under the same
// RLock that already derives FromKind. The assertions also round-trip through
// SQLite to confirm the JSON-blob persistence picks the new fields up with no
// migration.
func TestRoomSendPersistsSenderHarnessAndModel(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAI("gpt5", "codex", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	msgID, err := s.roomSend("general", agent.ID, "hello", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	s.mu.RLock()
	msg, ok := s.messageByIDInRoomLocked("general", msgID)
	s.mu.RUnlock()
	if !ok {
		t.Fatalf("message %d not found in room general", msgID)
	}
	if msg.FromKind != "ai" {
		t.Errorf("FromKind = %q, want %q", msg.FromKind, "ai")
	}
	// Compare against the agent record's own fields rather than the input slug:
	// registerAI canonicalizes the model (e.g. gpt5 -> gpt-5), and the message
	// must persist whatever the record actually holds.
	if msg.FromHarness != agent.Harness {
		t.Errorf("FromHarness = %q, want %q (sender harness must be persisted at send time)", msg.FromHarness, agent.Harness)
	}
	if msg.FromModel != agent.Model {
		t.Errorf("FromModel = %q, want %q (sender model must be persisted at send time)", msg.FromModel, agent.Model)
	}

	// Round-trip through SQLite: the message blob must carry the new fields so a
	// reloaded store resolves a departed sender's identity without the live record.
	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.mu.RLock()
	msg2, ok := reloaded.messageByIDInRoomLocked("general", msgID)
	reloaded.mu.RUnlock()
	if !ok {
		t.Fatalf("reloaded message %d not found", msgID)
	}
	if msg2.FromHarness != agent.Harness || msg2.FromModel != agent.Model {
		t.Errorf("reloaded FromHarness/FromModel = %q/%q, want %q/%q (JSON blob must round-trip)", msg2.FromHarness, msg2.FromModel, agent.Harness, agent.Model)
	}
	if msg2.FromKind != "ai" {
		t.Errorf("reloaded FromKind = %q, want ai", msg2.FromKind)
	}
}

// TestSystemMessageLeavesHarnessAndModelEmpty verifies the system-message path
// (emitSystemMessageFull) does not populate the sender-identity fields. System
// messages have no sender agent record and render without an icon or liveness
// dot, so carrying a harness/model would be meaningless and could confuse the
// frontend fallback chain.
func TestSystemMessageLeavesHarnessAndModelEmpty(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("casey", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	s.emitSystemMessage("general", "casey joined")

	s.mu.RLock()
	msgs := s.messages["general"]
	s.mu.RUnlock()
	if len(msgs) == 0 {
		t.Fatal("no system message emitted")
	}
	last := msgs[len(msgs)-1]
	if last.FromKind != "system" {
		t.Errorf("FromKind = %q, want system", last.FromKind)
	}
	if last.FromHarness != "" {
		t.Errorf("FromHarness = %q, want empty (system messages carry no sender identity)", last.FromHarness)
	}
	if last.FromModel != "" {
		t.Errorf("FromModel = %q, want empty (system messages carry no sender identity)", last.FromModel)
	}
}
