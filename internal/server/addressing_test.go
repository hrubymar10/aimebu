package server

import (
	"reflect"
	"testing"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestParseAddressedTo(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		// Room-wide (no address)
		{"hello everyone", nil},
		{"", nil},

		// @mention only
		{"hey @alice, what do you think?", []string{"alice"}},
		{"@alice @bob nice work", []string{"alice", "bob"}},

		// @mention deduplication
		{"@alice check this @alice again", []string{"alice"}},

		// Multiple @mentions
		{"@alice @bob see @carol", []string{"alice", "bob", "carol"}},

		// Case-insensitive @mention
		{"@Alice what's up?", []string{"alice"}},

		// Bare name with no colon is room-wide (no @)
		{"alice what's up?", nil},

		// Regression: old IRC-style prefix syntax is now room-wide
		{"alice: what's up?", nil},
		{"alice, bob: ready?", nil},
		{"alice and bob: ready?", nil},
	}

	for _, tc := range cases {
		got := parseAddressedTo(tc.body)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseAddressedTo(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestAnnotate(t *testing.T) {
	human := func(body string) types.Message {
		return types.Message{Body: body, FromKind: "human", RoomID: "general"}
	}
	ai := func(body string) types.Message {
		return types.Message{Body: body, FromKind: "ai", RoomID: "general"}
	}
	system := func(body string) types.Message {
		return types.Message{Body: body, FromKind: "system", RoomID: "general"}
	}
	dm := func(body string) types.Message {
		return types.Message{Body: body, FromKind: "ai", RoomID: "dm:alice:bob"}
	}

	cases := []struct {
		name         string
		msg          types.Message
		agentName    string
		wantAddrToMe bool
		wantRespond  bool
	}{
		// Human, room-wide → respond
		{"human room-wide", human("hey all"), "alice", false, true},
		// Human, addressed to me via @mention → respond
		{"human addressed to me", human("@alice what's up?"), "alice", true, true},
		// Human, addressed to other via @mention → don't respond
		{"human addressed to other", human("@bob what's up?"), "alice", false, false},
		// Human, multi-address includes me → respond
		{"human multi-addr includes me", human("@alice @bob ready?"), "alice", true, true},
		// Human, multi-address excludes me → don't respond
		{"human multi-addr excludes me", human("@bob @carol ready?"), "alice", false, false},
		// Human, old IRC-style prefix is now room-wide → respond (not addressed)
		{"human old prefix style room-wide", human("alice: what's up?"), "alice", false, true},

		// AI, room-wide → don't respond
		{"ai room-wide", ai("just FYI"), "alice", false, false},
		// AI, addressed to me via @mention → respond
		{"ai addressed to me", ai("@alice can you help?"), "alice", true, true},
		// AI, addressed to other via @mention → don't respond
		{"ai addressed to other", ai("@bob can you help?"), "alice", false, false},
		// AI, in DM room → respond
		{"ai DM room", dm("hey"), "alice", false, true},

		// System → never respond
		{"system message", system("server started"), "alice", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := annotate([]types.Message{tc.msg}, tc.agentName, nil)
			if len(out) != 1 {
				t.Fatalf("expected 1 annotated message, got %d", len(out))
			}
			a := out[0]
			if a.AddressedToMe != tc.wantAddrToMe {
				t.Errorf("AddressedToMe: got %v, want %v", a.AddressedToMe, tc.wantAddrToMe)
			}
			if a.ShouldRespond != tc.wantRespond {
				t.Errorf("ShouldRespond: got %v, want %v", a.ShouldRespond, tc.wantRespond)
			}
		})
	}
}

func TestAnnotateKnownAgentsFilter(t *testing.T) {
	known := map[string]bool{"worker": true, "reviewer": true, "leader": true}
	msg := types.Message{
		Body:     "see @latest example, then ping @worker @reviewer — agree?",
		FromKind: "ai",
		RoomID:   "general",
	}
	out := annotate([]types.Message{msg}, "worker", known)
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	a := out[0]
	// Only real agents should appear in addressed_to; @latest must be filtered out.
	for _, name := range a.AddressedTo {
		if !known[name] {
			t.Errorf("addressed_to contains non-agent %q", name)
		}
	}
	if !a.AddressedToMe {
		t.Error("AddressedToMe should be true for worker")
	}
	if !a.ShouldRespond {
		t.Error("ShouldRespond should be true when ai addresses worker directly")
	}
}

func TestParseAddressedToNoiseFiltering(t *testing.T) {
	// Without a known-agent filter, @latest/@master/@v0 appear in the raw list.
	// This test documents the raw behaviour; filtering happens in annotate.
	body := "see `@latest` or @master, then @worker @reviewer"
	got := parseAddressedTo(body)
	found := map[string]bool{}
	for _, n := range got {
		found[n] = true
	}
	if !found["worker"] || !found["reviewer"] {
		t.Errorf("real agents missing from raw parse: %v", got)
	}
	if !found["latest"] || !found["master"] {
		t.Errorf("noise tokens missing from raw parse (expected before filtering): %v", got)
	}
}

func TestParseLegacyPrefix(t *testing.T) {
	known := map[string]bool{"worker": true, "reviewer": true, "leader": true}

	cases := []struct {
		body      string
		wantName  string
		wantMatch bool
	}{
		// Positive: exact matches
		{"worker: here is my analysis", "worker", true},
		{"Worker: here is my analysis", "worker", true},
		{"WORKER: here is my analysis", "worker", true},
		{"leader: finalizing now", "leader", true},
		{"reviewer:no space after colon", "reviewer", true},

		// Negative: not an agent name → no match
		{"note: this is important", "", false},
		{"url: https://example.com", "", false},
		{"todo: fix this later", "", false},
		{"fyi: heads up", "", false},

		// Negative: @-addressed → no match (doesn't start with bare name:)
		{"@worker please look at this", "", false},
		{"hey everyone", "", false},
		{"", "", false},

		// Edge: name not in known set → no match even if pattern matches
		{"phantom: some message", "", false},

		// Edge: too short (< 3 chars after lowercase) → no match
		{"ab: short", "", false},
	}

	for _, tc := range cases {
		gotName, gotMatch := parseLegacyPrefix(tc.body, known)
		if gotMatch != tc.wantMatch || gotName != tc.wantName {
			t.Errorf("parseLegacyPrefix(%q) = (%q, %v), want (%q, %v)",
				tc.body, gotName, gotMatch, tc.wantName, tc.wantMatch)
		}
	}
}

func TestParseInlineLegacyPrefix(t *testing.T) {
	known := map[string]bool{"worker": true, "reviewer": true, "leader": true}

	cases := []struct {
		body      string
		wantNames []string
		wantMatch bool
	}{
		{"worker, reviewer — your take?", []string{"worker", "reviewer"}, true},
		{"Preamble. worker, reviewer — your take?", []string{"worker", "reviewer"}, true},
		{"Preamble\n\nworker, reviewer — your take?", []string{"worker", "reviewer"}, true},
		{"worker and reviewer: thoughts?", []string{"worker", "reviewer"}, true},
		{"worker, reviewer: thoughts?", []string{"worker", "reviewer"}, true},
		{"Worker, Reviewer — case-insensitive", []string{"worker", "reviewer"}, true},
		{"Hi alice, bob is wrong about that", nil, false},
		{"Q: name, what do you think?", nil, false},
		{"worker, NOTANAGENT — please review", nil, false},
		{"@worker @reviewer please review", nil, false},
	}

	for _, tc := range cases {
		gotNames, gotMatch := parseInlineLegacyPrefix(tc.body, known)
		if gotMatch != tc.wantMatch || !reflect.DeepEqual(gotNames, tc.wantNames) {
			t.Errorf("parseInlineLegacyPrefix(%q) = (%v, %v), want (%v, %v)",
				tc.body, gotNames, gotMatch, tc.wantNames, tc.wantMatch)
		}
	}
}

func TestAgentShortName(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"alice@aimebu", "alice"},
		{"bob@project", "bob"},
		{"martin", "martin"},
		{"@broken", "@broken"}, // no name before @
		{"", ""},
	}
	for _, tc := range cases {
		if got := agentShortName(tc.id); got != tc.want {
			t.Errorf("agentShortName(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}
