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

		// Simple prefix
		{"alice: what's up?", []string{"alice"}},
		{"Alice: what's up?", []string{"alice"}}, // case-insensitive

		// Multi-name prefix with comma
		{"alice, bob: ready?", []string{"alice", "bob"}},
		{"alice, bob, carol: ready?", []string{"alice", "bob", "carol"}},

		// Multi-name prefix with "and"
		{"alice and bob: ready?", []string{"alice", "bob"}},

		// Mixed comma + and
		{"alice, bob and carol: ready?", []string{"alice", "bob", "carol"}},

		// @mention only
		{"hey @alice, what do you think?", []string{"alice"}},
		{"@alice @bob nice work", []string{"alice", "bob"}},

		// Prefix + @mention (deduplication)
		{"alice: @alice check this", []string{"alice"}},

		// @mention does NOT duplicate prefix names
		{"alice, bob: see @carol", []string{"alice", "bob", "carol"}},

		// Bare name with no colon is room-wide
		{"alice what's up?", nil},
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
		name          string
		msg           types.Message
		agentName     string
		wantAddrToMe  bool
		wantRespond   bool
	}{
		// Human, room-wide → respond
		{"human room-wide", human("hey all"), "alice", false, true},
		// Human, addressed to me → respond
		{"human addressed to me", human("alice: what's up?"), "alice", true, true},
		// Human, addressed to other → don't respond
		{"human addressed to other", human("bob: what's up?"), "alice", false, false},
		// Human, multi-address includes me → respond
		{"human multi-addr includes me", human("alice, bob: ready?"), "alice", true, true},
		// Human, multi-address excludes me → don't respond
		{"human multi-addr excludes me", human("bob, carol: ready?"), "alice", false, false},

		// AI, room-wide → don't respond
		{"ai room-wide", ai("just FYI"), "alice", false, false},
		// AI, addressed to me → respond
		{"ai addressed to me", ai("alice: can you help?"), "alice", true, true},
		// AI, addressed to other → don't respond
		{"ai addressed to other", ai("bob: can you help?"), "alice", false, false},
		// AI, in DM room → respond
		{"ai DM room", dm("hey"), "alice", false, true},

		// System → never respond
		{"system message", system("server started"), "alice", false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := annotate([]types.Message{tc.msg}, tc.agentName)
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
