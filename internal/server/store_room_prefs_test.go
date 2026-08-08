package server

import (
	"bytes"
	"net/http"
	"testing"
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
