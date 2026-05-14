package server

import (
	"reflect"
	"testing"
	"time"

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

		// Code/escape literalization
		{"`@alice` stays literal", nil},
		{"```\n@alice\n```", nil},
		{"~~~\n@alice\n~~~", nil},
		{"hello @alice and `@bob`", []string{"alice"}},
		{"\\@alice stays literal", nil},
	}

	for _, tc := range cases {
		got := parseAddressedTo(tc.body)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseAddressedTo(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestMaskCodeForAddressing(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantMasked string
	}{
		{"inline backticks", "hello `@leader` there", "hello `       ` there"},
		{"triple fence", "```\n@leader\n```", "```\n       \n   "},
		{"tilde fence", "~~~\n@leader\n~~~", "~~~\n       \n   "},
		{"indented block", "lead\n\n    @leader", "lead\n\n           "},
	}

	for _, tc := range cases {
		view := maskCodeForAddressing(tc.body)
		if view.masked != tc.wantMasked {
			t.Errorf("%s: masked = %q, want %q", tc.name, view.masked, tc.wantMasked)
		}
	}
}

func TestMaskCodeForAddressingIndentedNegatives(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"paragraph continuation", "text\n    @leader"},
		{"list continuation", "- item\n    @leader"},
	}

	for _, tc := range cases {
		view := maskCodeForAddressing(tc.body)
		if view.masked != tc.body {
			t.Errorf("%s: got %q, want unchanged %q", tc.name, view.masked, tc.body)
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
	targetedSystem := func(body string, targets ...string) types.Message {
		return types.Message{Body: body, FromKind: "system", RoomID: "general", Targets: targets}
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

		// Untargeted system messages are informational.
		{"system message", system("server started"), "alice", false, false},
		// Targeted system messages wake only their explicit target.
		{"targeted system message to me", targetedSystem("alice@aimebu was assigned as Reviewer", "alice"), "alice", true, true},
		{"targeted system message to other", targetedSystem("alice@aimebu was assigned as Reviewer", "alice"), "bob", false, false},
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
	out := annotate([]types.Message{msg}, "worker", func(types.Message) addressingContext {
		return addressingContext{KnownNames: known}
	})
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

func TestResolveAddressedToGroupMentions(t *testing.T) {
	now := time.Date(2026, time.May, 10, 15, 0, 0, 0, time.UTC)
	ctx := addressingContext{
		SenderID: "worker@aimebu",
		KnownNames: map[string]bool{
			"worker":   true,
			"reviewer": true,
			"matin":    true,
			"alex":     true,
		},
		RoomAgents: []roomAgentContext{
			{ID: "worker@aimebu", Name: "worker", Kind: "ai", Waiting: true, LastSeen: now},
			{ID: "reviewer@aimebu", Name: "reviewer", Kind: "ai", Waiting: true, LastSeen: now.Add(-10 * time.Minute)},
			{ID: "matin", Name: "matin", Kind: "human", Waiting: false, LastSeen: now.Add(-4 * time.Minute)},
			{ID: "alex", Name: "alex", Kind: "human", Waiting: false, LastSeen: now.Add(-10 * time.Minute)},
		},
		Now: now,
	}

	cases := []struct {
		name string
		body string
		want []string
	}{
		{"channel", "@channel", []string{"reviewer", "matin", "alex"}},
		{"humans", "@humans", []string{"matin", "alex"}},
		{"ais", "@ais", []string{"reviewer"}},
		{"everyone alias", "@everyone", []string{"reviewer", "matin", "alex"}},
		{"all alias", "@all", []string{"reviewer", "matin", "alex"}},
		{"here waiting or recent", "@here", []string{"reviewer", "matin"}},
		{"mixed direct and group", "@reviewer @humans", []string{"reviewer", "matin", "alex"}},
		{"escaped literal", "\\@channel @reviewer", []string{"reviewer"}},
		{"code literal", "`@here` @reviewer", []string{"reviewer"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveAddressedTo(tc.body, ctx)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveAddressedTo(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestResolveAddressedToRoleMentions(t *testing.T) {
	ctx := addressingContext{
		SenderID: "leader@aimebu",
		KnownNames: map[string]bool{
			"leader": true,
			"alice":  true,
		},
		RoleNames: map[string][]string{
			"reviewer": {"alice", "bob"},
			"leader":   {"carol"}, // agent-name precedence should win.
			"here":     {"dana"},  // special group precedence should win.
		},
		RoomAgents: []roomAgentContext{
			{ID: "leader@aimebu", Name: "leader", Kind: "ai", Waiting: true},
			{ID: "alice@aimebu", Name: "alice", Kind: "ai", Waiting: true},
			{ID: "bob@aimebu", Name: "bob", Kind: "ai", Waiting: true},
			{ID: "carol@aimebu", Name: "carol", Kind: "ai", Waiting: true},
			{ID: "dana@aimebu", Name: "dana", Kind: "ai", Waiting: true},
		},
		Now: time.Date(2026, time.May, 10, 15, 0, 0, 0, time.UTC),
	}

	if got, want := resolveAddressedTo("@reviewer", ctx), []string{"alice", "bob"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("role mention = %v, want %v", got, want)
	}
	if got, want := resolveAddressedTo("@leader", ctx), []string{"leader"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent-name precedence = %v, want %v", got, want)
	}
	if got, want := resolveAddressedTo("@here", ctx), []string{"alice", "bob", "carol", "dana"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reserved group should resolve as group, got %v, want %v", got, want)
	}

	msg := types.Message{RoomID: "room", From: "matin", FromKind: "human", Body: "@reviewer please review"}
	out := annotate([]types.Message{msg}, "bob", func(types.Message) addressingContext { return ctx })
	if !out[0].AddressedToMe || !out[0].ShouldRespond {
		t.Fatalf("role-addressed bob should respond: %+v", out[0])
	}
}

func TestParseAddressedToNoiseFiltering(t *testing.T) {
	// Without a known-agent filter, non-agent tokens outside code still appear in
	// the raw list. Code-contained tokens are now masked before mention parsing.
	body := "see `@latest` or @master, then @worker @reviewer \\@leader"
	got := parseAddressedTo(body)
	found := map[string]bool{}
	for _, n := range got {
		found[n] = true
	}
	if !found["worker"] || !found["reviewer"] {
		t.Errorf("real agents missing from raw parse: %v", got)
	}
	if found["latest"] || !found["master"] || found["leader"] {
		t.Errorf("unexpected raw parse contents after masking/escape handling: %v", got)
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
		{"`worker: please look`", "", false},
		{"```\nworker: please look\n```", "", false},
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
		{"```\nworker, reviewer — your take?\n```", nil, false},
	}

	for _, tc := range cases {
		gotNames, gotMatch := parseInlineLegacyPrefix(tc.body, known)
		if gotMatch != tc.wantMatch || !reflect.DeepEqual(gotNames, tc.wantNames) {
			t.Errorf("parseInlineLegacyPrefix(%q) = (%v, %v), want (%v, %v)",
				tc.body, gotNames, gotMatch, tc.wantNames, tc.wantMatch)
		}
	}
}

func TestParseAttentionMiss(t *testing.T) {
	for _, phrase := range attentionMissPhrases {
		body := "@matin " + phrase + " this plan"
		gotPhrase, gotMatch := parseAttentionMiss(body)
		if !gotMatch || gotPhrase != phrase {
			t.Errorf("parseAttentionMiss(%q) = (%q, %v), want (%q, true)", body, gotPhrase, gotMatch, phrase)
		}
	}

	cases := []struct {
		body       string
		wantPhrase string
		wantMatch  bool
	}{
		{"@matin Please Look At the diff", "please look at", true},
		{"@matin status update: build is green", "", false},
		{"@matin what time is it?", "", false},
		{"matin said: \"please approve\"", "please approve", true},
		{"@matin `please approve`", "", false},
		{"@matin\n```\nplease approve\n```", "", false},
		{"\\@matin please approve", "please approve", true},
	}

	for _, tc := range cases {
		gotPhrase, gotMatch := parseAttentionMiss(tc.body)
		if gotMatch != tc.wantMatch || gotPhrase != tc.wantPhrase {
			t.Errorf("parseAttentionMiss(%q) = (%q, %v), want (%q, %v)",
				tc.body, gotPhrase, gotMatch, tc.wantPhrase, tc.wantMatch)
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
