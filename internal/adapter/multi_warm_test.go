package adapter

import "testing"

// A Multi has the warm methods whatever it holds; it is warm only when one of its adapters is.
func TestMultiIsWarmOnlyWithAWarmAdapter(t *testing.T) {
	if NewMulti(NewFake()).HasWarm() {
		t.Fatal("a Multi of adapters without a warm mode is not warm")
	}
	if _, err := NewMulti(NewFake()).WarmFingerprint(TurnSpec{Agent: AgentSpec{Tool: "fake"}}); err != ErrNoWarm {
		t.Fatalf("no warm adapter for the tool: %v", err)
	}
}
