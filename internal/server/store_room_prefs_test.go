package server

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
)

func TestRoomPrefsSetGetAndPersist(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	hide := true
	if pref := s.setRoomPref(human.ID, "general", &hide, nil); !pref.Hidden {
		t.Fatal("setRoomPref did not hide")
	}
	if got := s.roomPref(human.ID, "general"); !got.Hidden {
		t.Fatal("roomPref did not return hidden")
	}

	// Persisted across reload.
	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if got := reloaded.roomPref(human.ID, "general"); !got.Hidden {
		t.Fatal("hidden preference did not survive reload")
	}

	// Clearing (both false) deletes the entry.
	off := false
	if pref := s.setRoomPref(human.ID, "general", &off, nil); pref.Hidden || pref.Pinned {
		t.Fatal("preference not cleared")
	}
	reloaded2, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded2.Close()
	if got := reloaded2.roomPref(human.ID, "general"); got.Hidden || got.Pinned {
		t.Fatal("cleared preference was persisted")
	}
}

func TestHideRejectedWhenNotAlone(t *testing.T) {
	s, srv := setupTestServer(t)
	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", ai.ID); err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	// hidden=true rejected (409): the room has another member (the AI).
	resp, err := http.Post(srv.URL+"/agents/"+human.ID+"/rooms/general/prefs", "application/json", bytes.NewBufferString(`{"hidden":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("hide with another member present: status %d, want 409", resp.StatusCode)
	}
	// hidden was NOT applied.
	if got := s.roomPref(human.ID, "general"); got.Hidden {
		t.Fatal("hide was applied despite the 409")
	}

	// pinned=true allowed (the solo rule blocks hiding only, not pinning).
	resp2, err := http.Post(srv.URL+"/agents/"+human.ID+"/rooms/general/prefs", "application/json", bytes.NewBufferString(`{"pinned":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("pin with another member: status %d, want 200", resp2.StatusCode)
	}
	if got := s.roomPref(human.ID, "general"); !got.Pinned {
		t.Fatal("pin was not applied")
	}
}

// TestTwoHumanRoomNotHideable pins the rule change: a room with two humans and
// no AI is legal to hide under last night's no-AI rule, but the solo-only
// rule rejects it. This is the case that would have silently stayed hideable
// without widening the guard from AI-member to any-other-member.
func TestTwoHumanRoomNotHideable(t *testing.T) {
	s, srv := setupTestServer(t)
	alice, err := s.registerHuman("alice", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.registerHuman("bob", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{alice.ID, bob.ID} {
		if _, err := s.joinRoom("general", h); err != nil {
			t.Fatal(err)
		}
	}
	// No AI members, but alice is not alone — hide rejected under solo-only.
	resp, err := http.Post(srv.URL+"/agents/"+alice.ID+"/rooms/general/prefs", "application/json", bytes.NewBufferString(`{"hidden":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("hide in a two-human room: status %d, want 409 (solo-only)", resp.StatusCode)
	}
	if got := s.roomPref(alice.ID, "general"); got.Hidden {
		t.Fatal("hide was applied in a two-human room — solo-only rule not enforced")
	}
}

// TestAnyJoinUnhidesHiddenRoom is the load-bearing rule: anyone (human or AI)
// joining a room a human has hidden must clear that hidden preference BEFORE
// the joiner can post. Under solo-only this is the only auto-unhide rule —
// a hidden room has nobody in it, so the moment anyone arrives it comes back.
func TestAnyJoinUnhidesHiddenRoom(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	hide := true
	if p := s.setRoomPref(human.ID, "general", &hide, nil); !p.Hidden {
		t.Fatal("hide failed")
	}

	// A second HUMAN joins the hidden room → hidden clears for the hider.
	// (Under the old AI-only rule this would not have fired — the widening is
	// the rule change.)
	other, err := s.registerHuman("iris", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", other.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.roomPref(human.ID, "general"); got.Hidden {
		t.Fatal("a human joining did not clear the hider's hidden preference (solo-only auto-unhide broken)")
	}
}

// TestAIJoinStillUnhidesHiddenRoom keeps the original AI-join assertion from
// TestAIJoinUnhidesHiddenRoom: an AI joining also unhides (the widen must not
// regress the AI case). Kept as its own test so the human-join and AI-join
// paths are each pinned independently.
func TestAIJoinStillUnhidesHiddenRoom(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	hide := true
	if p := s.setRoomPref(human.ID, "general", &hide, nil); !p.Hidden {
		t.Fatal("hide failed")
	}

	ai, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", ai.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.roomPref(human.ID, "general"); got.Hidden {
		t.Fatal("AI join did not clear the human's hidden preference")
	}
	if _, err := s.roomSend("general", ai.ID, "hello", false, nil, nil, nil, 0); err != nil {
		t.Fatalf("AI post after join failed: %v", err)
	}
}

// TestClearAllRoomPrefsNoRaceWithConcurrentAccess reproduces the bug wren caught
// in #11703: clearAllRoomPrefsLocked used to run without s.mu during prune -a,
// racing with concurrent roomPref reads (the UI polls /agents/{id}/rooms) — a
// concurrent map write that Go fatals on, unrecoverable mid-wipe. With the fix
// (clearAllRoomPrefsLocked under s.mu), this passes under -race; without it the
// process crashes.
func TestClearAllRoomPrefsNoRaceWithConcurrentAccess(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	hide := true
	s.setRoomPref(human.ID, "general", &hide, nil)

	// Hammer roomPref (read) + setRoomPref (write) while clearAll(true) wipes
	// room_prefs. All three take s.mu, so the access is serialised.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = s.roomPref(human.ID, "general")
			s.setRoomPref(human.ID, "general", &hide, nil)
			off := false
			s.setRoomPref(human.ID, "general", &off, nil)
		}
	}()
	s.clearAll(true)
	<-done
}

// TestKickAgentsRemovesAllAIAgentMembers proves the kick-agents endpoint makes
// a room hideable in one atomic server-side operation: every AI member is
// removed, humans are untouched, and the room ends up with no AI members (so
// roomHasAIAgent returns false and hiding is allowed). Idempotent.
func TestKickAgentsRemovesAllAIAgentMembers(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ai1, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	ai2, _, err := s.registerAI("gpt5", "codex", "test", nil, "bet")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{human.ID, ai1.ID, ai2.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}
	if !s.roomHasAIAgent("general") {
		t.Fatal("setup: room should have AI members")
	}

	kicked, remaining, err := s.kickAgents("general")
	if err != nil {
		t.Fatalf("kickAgents: %v", err)
	}
	if len(kicked) != 2 {
		t.Fatalf("kicked %d agents, want 2: %v", len(kicked), kicked)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v after a clean kick, want empty (room should be hideable)", remaining)
	}
	if s.roomHasAIAgent("general") {
		t.Fatal("room still has an AI member after kickAgents — room is not hideable")
	}
	// Human is untouched.
	s.mu.RLock()
	room := s.rooms["general"]
	stillMember := false
	for _, m := range room.Members {
		if m == human.ID {
			stillMember = true
		}
	}
	s.mu.RUnlock()
	if !stillMember {
		t.Fatal("human was removed by kickAgents — humans must never be touched")
	}

	// Idempotent: a room with no AI members returns empty kicked + remaining.
	kicked2, remaining2, err := s.kickAgents("general")
	if err != nil || len(kicked2) != 0 || len(remaining2) != 0 {
		t.Fatalf("idempotent kickAgents: got kicked=%v remaining=%v err=%v — want both empty, nil", kicked2, remaining2, err)
	}
}

// TestKickAgentsReportsRemainingTruthfully is the case wren asked for: when a
// member goes away mid-batch (here: a concurrent leave during kickAgents), the
// response must be truthful about what's left rather than letting the caller
// deduce it from a count. `remaining` is a fresh re-snapshot, so regardless of
// the race the caller knows whether the room is actually hideable.
func TestKickAgentsReportsRemainingTruthfully(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ai1, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	ai2, _, err := s.registerAI("gpt5", "codex", "test", nil, "bet")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ai1.ID, ai2.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}

	// ai2 leaves concurrently while kickAgents runs. Whether the snapshot
	// includes ai2 or not is a race; either way the room ends up empty and
	// `remaining` must report that, not infer it from `kicked`.
	go func() {
		_ = s.leaveRoom("general", ai2.ID)
	}()
	kicked, remaining, err := s.kickAgents("general")
	if err != nil {
		t.Fatalf("kickAgents: %v", err)
	}
	// `remaining` must agree with the actual room state — that's the truthfulness
	// check. After both AIs are gone (one kicked, one self-left), remaining is
	// empty and the room is hideable.
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want empty (room should be hideable regardless of the race)", remaining)
	}
	if s.roomHasAIAgent("general") {
		t.Fatal("room still has an AI member — remaining report lied; room is not hideable")
	}
	// `kicked` is a subset of the snapshot; either ai1 alone or both. Not
	// asserting its exact length — the race decides — only that remaining is
	// truthful, which is the property that protects the UI.
	_ = kicked
}

// TestKickAgentsHTTPRoute is the HTTP-level test viv flagged as missing: the
// store method is tested directly above, but a rename or misroute of
// POST /rooms/{id}/kick-agents would go unnoticed. This hits the real route
// and asserts the response shape (kicked + remaining) + that humans survive.
func TestKickAgentsHTTPRoute(t *testing.T) {
	s, srv := setupTestServer(t)
	ai1, _, err := s.registerAI("gpt5", "codex", "test", nil, "alf")
	if err != nil {
		t.Fatal(err)
	}
	ai2, _, err := s.registerAI("gpt5", "codex", "test", nil, "bet")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ai1.ID, ai2.ID, human.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.Post(srv.URL+"/rooms/general/kick-agents", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kick-agents: status %d, want 200", resp.StatusCode)
	}
	var out struct {
		Status    string   `json:"status"`
		Room      string   `json:"room"`
		Kicked    []string `json:"kicked"`
		Remaining []string `json:"remaining"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "kicked" || out.Room != "general" {
		t.Fatalf("response = %+v, want status=kicked room=general", out)
	}
	if len(out.Kicked) != 2 {
		t.Fatalf("kicked = %v, want 2 AI IDs", out.Kicked)
	}
	if len(out.Remaining) != 0 {
		t.Fatalf("remaining = %v, want empty (room hideable)", out.Remaining)
	}
	if s.roomHasAIAgent("general") {
		t.Fatal("room still has an AI member after HTTP kick-agents")
	}
	// Human survives.
	s.mu.RLock()
	room := s.rooms["general"]
	humanAlive := false
	for _, m := range room.Members {
		if m == human.ID {
			humanAlive = true
		}
	}
	s.mu.RUnlock()
	if !humanAlive {
		t.Fatal("human was removed by kick-agents — humans must never be touched")
	}

	// Idempotent re-kick over HTTP: no AI members → empty kicked + remaining, 200.
	resp2, err := http.Post(srv.URL+"/rooms/general/kick-agents", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("idempotent kick-agents: status %d, want 200", resp2.StatusCode)
	}
	var out2 struct {
		Kicked    []string `json:"kicked"`
		Remaining []string `json:"remaining"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if len(out2.Kicked) != 0 || len(out2.Remaining) != 0 {
		t.Fatalf("idempotent kick-agents: kicked=%v remaining=%v, want both empty", out2.Kicked, out2.Remaining)
	}
}
