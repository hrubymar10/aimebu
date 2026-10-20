package store

import (
	"strings"
	"testing"
)

func TestPiReceivesServerSessionUUID(t *testing.T) {
	t.Parallel()
	first, err := nativeSessionID("pi")
	if err != nil {
		t.Fatal(err)
	}
	second, err := nativeSessionID("pi")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 36 || strings.Count(first, "-") != 4 || first == second {
		t.Fatalf("Pi session IDs %q and %q", first, second)
	}
	other, err := nativeSessionID("codex")
	if err != nil || other != "" {
		t.Fatalf("Codex session ID %q err=%v", other, err)
	}
}
