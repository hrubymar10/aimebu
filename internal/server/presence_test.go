package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

// The presence rule on the tracker itself, with a fake clock: a person is
// online while their last real activity is within HumanPresenceWindow, the
// sweep announces the offline switch once, and new activity announces online
// again.
func TestHumanPresenceWindowAndTransitions(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	p := newHumanPresence()
	p.now = func() time.Time { return now }

	var transitions []string
	p.announce = func(userID string, online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		transitions = append(transitions, userID+":"+state)
	}

	if p.Online("usr_a") {
		t.Fatal("nobody is online before their first activity")
	}
	p.Touch("usr_a")
	if !p.Online("usr_a") {
		t.Fatal("activity makes the person online")
	}
	if !transitionsEqual(transitions, "usr_a:online") {
		t.Fatalf("coming online is announced at once: %v", transitions)
	}

	now = now.Add(HumanPresenceWindow - time.Minute)
	if !p.Online("usr_a") {
		t.Fatal("activity nine minutes ago is still online")
	}
	now = now.Add(2 * time.Minute)
	if p.Online("usr_a") {
		t.Fatal("activity eleven minutes ago is offline")
	}
	if p.Online("usr_b") {
		t.Fatal("a person who never showed activity is offline")
	}

	// The sweep announces the switch once; a second sweep says nothing.
	p.sweep()
	if !transitionsEqual(transitions, "usr_a:online", "usr_a:offline") {
		t.Fatalf("the sweep announces the offline switch: %v", transitions)
	}
	p.sweep()
	if !transitionsEqual(transitions, "usr_a:online", "usr_a:offline") {
		t.Fatalf("a sweep without a change announces nothing: %v", transitions)
	}

	// Activity after the window announces online again.
	p.Touch("usr_a")
	if !p.Online("usr_a") {
		t.Fatal("new activity makes the person online again")
	}
	if !transitionsEqual(transitions, "usr_a:online", "usr_a:offline", "usr_a:online") {
		t.Fatalf("returning online is announced: %v", transitions)
	}
	// Activity while already online announces nothing.
	p.Touch("usr_a")
	if !transitionsEqual(transitions, "usr_a:online", "usr_a:offline", "usr_a:online") {
		t.Fatalf("activity while online announces nothing: %v", transitions)
	}
}

func transitionsEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Through the HTTP surface: the heartbeat makes its sender online on the
// member list, a GET read does not keep anyone online by itself, and the
// window runs out on the fake clock. Both transitions are announced as
// `human_presence_changed` events the project's members receive.
func TestPresenceHeartbeatShowsAndExpiresOnMemberList(t *testing.T) {
	f := newFixture(t)
	now := time.Unix(1_700_000_000, 0)
	f.srv.Presence.now = func() time.Time { return now }
	var transitions []string
	owner, err := f.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Presence.announce = func(userID string, online bool) {
		state := "offline"
		if online {
			state = "online"
		}
		transitions = append(transitions, userID+":"+state)
		f.srv.announcePresence(context.Background(), userID, online) // the real announcement, so the events exist too
	}
	f.seed()

	ownerOnline := func() bool {
		t.Helper()
		response := f.request("GET", "/api/rooms/"+f.room.ID+"/members", nil, false)
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("members: %d", response.StatusCode)
		}
		var got struct {
			Humans []struct {
				Login  string `json:"login"`
				Online bool   `json:"online"`
			} `json:"humans"`
		}
		if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		for _, human := range got.Humans {
			if human.Login == "owner" {
				return human.Online
			}
		}
		t.Fatal("the owner is not on the member list")
		return false
	}

	// Creating the project and the room were writes (real user actions), so the
	// owner is online; a plain GET must not keep them so.
	if !ownerOnline() {
		t.Fatal("the owner's writes made them online")
	}
	now = now.Add(time.Minute)
	if !ownerOnline() {
		t.Fatal("a member-list read does not end the presence window")
	}

	// The heartbeat endpoint counts: it is a write, and the page sends it only
	// while the person is visibly using the page.
	response := f.request("POST", "/api/presence", map[string]any{}, true)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("presence: %d", response.StatusCode)
	}
	response.Body.Close()
	now = now.Add(9 * time.Minute)
	if !ownerOnline() {
		t.Fatal("the heartbeat nine minutes ago keeps the person online")
	}
	now = now.Add(2 * time.Minute)
	if ownerOnline() {
		t.Fatal("eleven minutes without activity is offline")
	}

	// A write without the CSRF token is refused and records nothing.
	response = f.request("POST", "/api/presence", map[string]any{}, false)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("presence without CSRF: %d", response.StatusCode)
	}
	if ownerOnline() {
		t.Fatal("a refused write did not make the person online")
	}

	// A heartbeat inside the window brings the person back.
	response = f.request("POST", "/api/presence", map[string]any{}, true)
	response.Body.Close()
	if !ownerOnline() {
		t.Fatal("a fresh heartbeat makes the person online again")
	}

	// The window runs out again: the sweep announces the offline switch, and a
	// fresh heartbeat announces online again.
	now = now.Add(11 * time.Minute)
	f.srv.Presence.sweep()
	response = f.request("POST", "/api/presence", map[string]any{}, true)
	response.Body.Close()
	// The events say the same to the project's members — except the very first
	// transition: its Touch runs before the project-create handler commits the
	// membership, so there is no project to announce it to yet. The next
	// periodic member-list refresh (the web re-reads the members every 30 s)
	// shows it anyway, and every later transition is announced.
	// The tracker's own transitions are the four switches of the test.
	want := []string{owner.ID + ":online", owner.ID + ":online", owner.ID + ":offline", owner.ID + ":online"}
	if !transitionsEqual(transitions, want...) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	events, err := f.db.EventsAfter(t.Context(), owner.ID, 0, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, event := range events {
		if event.Kind != "human_presence_changed" {
			continue
		}
		var payload struct {
			UserID string `json:"user_id"`
			Online string `json:"online"`
		}
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		found = append(found, payload.UserID+":"+payload.Online)
	}
	// The events say the same to the project's members — except the very first
	// transition: its Touch runs before the project-create handler commits the
	// membership, so there is no project to announce it to yet. The next
	// periodic member-list refresh (the web re-reads the members every 30 s)
	// shows it anyway, and every later transition is announced.
	wantEvents := []string{owner.ID + ":online", owner.ID + ":offline", owner.ID + ":online"}
	if !transitionsEqual(found, wantEvents...) {
		t.Fatalf("presence events = %v, want %v", found, wantEvents)
	}
}
