package prompt

import (
	"strings"
	"testing"
)

func TestBuildOrdersSharedSentencesAroundTheDeltas(t *testing.T) {
	t.Parallel()
	got := Build(Deltas{Opening: "OPEN.", Role: "ROLE.", Attachments: "ATT.", Output: "OUT.", Ack: "ACK.", Closing: "CLOSE."})
	order := []string{"OPEN.", "Use context.you_are", "ROLE.", "Input note attachments are files; ATT.", "A note retry_notice", "OUT.", "Use either proposed_answers", "After spawning an agent", "ACK.", "A message from a human that is addressed to you", "A lead with a completed review", "CLOSE."}
	last := -1
	for _, part := range order {
		at := strings.Index(got, part)
		if at <= last {
			t.Fatalf("%q is missing or out of order in %q", part, got)
		}
		last = at
	}
	if strings.Contains(Build(Deltas{Opening: "OPEN.", Attachments: "ATT.", Output: "OUT.", Ack: "ACK.", Closing: "CLOSE."}), "ROLE.") {
		t.Fatal("an empty role must leave its sentence out")
	}
	if strings.Contains(Build(Deltas{Opening: "A", Attachments: "B", Output: "C", Ack: "D", Closing: "E"}), "  ") {
		t.Fatal("an empty role must not leave a double space")
	}
}

func TestEveryPromptTellsAnAgentToAnswerHumans(t *testing.T) {
	t.Parallel()
	got := Build(Deltas{Opening: "O", Attachments: "A", Output: "U", Ack: "K", Closing: "C"})
	for _, want := range []string{"always gets a reply, even a one-line one", "Silence and reaction-only rules apply to agent-to-agent traffic"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the shared prompt lost %q", want)
		}
	}
}

func TestHumanResponseNamesTheChoiceCount(t *testing.T) {
	t.Parallel()
	if got := HumanResponse("short choices"); !strings.Contains(got, "proposed_answers (2-4 short choices)") || strings.Contains(got, "%s") {
		t.Fatal(got)
	}
	if got := HumanResponse("choices"); !strings.Contains(got, "proposed_answers (2-4 choices)") {
		t.Fatal(got)
	}
}

func TestOverridesReplaceOnlyTheirSentenceAndFallBack(t *testing.T) {
	t.Parallel()
	d := Deltas{Opening: "O", Attachments: "A", Output: "OUT", Ack: "K", Closing: "C"}
	shipped := Build(d)
	got := BuildWith(d, map[string]string{"mentions": "  MY MENTIONS  ", "tail": "", "human_reply": "   ", "output": "ignored", "unknown": "x"})
	if !strings.Contains(got, "MY MENTIONS") || strings.Contains(got, "After spawning an agent") {
		t.Fatalf("the override replaces its sentence: %q", got)
	}
	for _, kept := range []string{"A lead with a completed review", "A message from a human that is addressed to you", "OUT", "K", "C"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q must survive: an empty, blank or unknown override changes nothing", kept)
		}
	}
	if strings.Contains(got, "ignored") || strings.Contains(got, "  ") {
		t.Errorf("an unknown slot is never used, and no double space appears: %q", got)
	}
	if BuildWith(d, nil) != shipped || BuildWith(d, map[string]string{}) != shipped {
		t.Fatal("no overrides gives the shipped bytes")
	}
}

func TestEverySlotIsOverridableAndOrdered(t *testing.T) {
	t.Parallel()
	d := Deltas{Opening: "O", Attachments: "A", Output: "OUT", Ack: "K", Closing: "C"}
	overrides := map[string]string{}
	for _, slot := range Slots() {
		if slot.Default == "" || slot.Title == "" || !ValidSlot(slot.Key) {
			t.Fatalf("incomplete slot %+v", slot)
		}
		overrides[slot.Key] = "<<" + slot.Key + ">>"
		if !strings.Contains(Build(d), slot.Default) {
			t.Errorf("slot %s default is not in the shipped prompt", slot.Key)
		}
	}
	got := BuildWith(d, overrides)
	last := -1
	for _, slot := range Slots() {
		at := strings.Index(got, "<<"+slot.Key+">>")
		if at <= last {
			t.Fatalf("slot %s is missing or out of order", slot.Key)
		}
		last = at
		if strings.Contains(got, slot.Default) {
			t.Errorf("slot %s default survived its override", slot.Key)
		}
	}
	if ValidSlot("output") || ValidSlot("") || ValidSlot("opening") {
		t.Error("the output contract and adapter parts are not slots")
	}
}

func TestForContextReadsPromptOverrides(t *testing.T) {
	t.Parallel()
	d := Deltas{Opening: "O", Attachments: "A", Output: "OUT", Ack: "K", Closing: "C"}
	shipped := Build(d)
	for _, context := range []string{"", "{", `{"role_body":"x"}`, `{"prompt_overrides":{}}`, `{"prompt_overrides":null}`, `{"prompt_overrides":"nope"}`} {
		if got := ForContext(d, shipped, []byte(context)); got != shipped {
			t.Errorf("context %q should give the shipped prompt", context)
		}
	}
	got := ForContext(d, shipped, []byte(`{"role_body":"r","prompt_overrides":{"tail":"MY TAIL"}}`))
	if !strings.Contains(got, "MY TAIL") || strings.Contains(got, "A lead with a completed review") {
		t.Errorf("the context's override is applied: %q", got)
	}
}

func TestWithoutOverridesDropsOnlyThatKey(t *testing.T) {
	t.Parallel()
	got := WithoutOverrides([]byte(`{"role_body":"x","prompt_overrides":{"tail":"t"},"rooms":[1]}`))
	if string(got) != `{"role_body":"x","rooms":[1]}` {
		t.Fatalf("stripped: %s", got)
	}
	for _, same := range []string{`{"role_body":"x"}`, `not json`, ``} {
		if got := WithoutOverrides([]byte(same)); string(got) != same {
			t.Fatalf("%q must come back untouched: %q", same, got)
		}
	}
}
