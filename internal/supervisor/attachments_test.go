package supervisor

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
)

func TestAttachmentBaseURLAndPrivateTurnSpec(t *testing.T) {
	base, err := attachmentBaseURL("http://127.0.0.1:9897/?secret=old", "turn_1", true)
	if err != nil || base != "http://host.docker.internal:9897/api/agent/turns/turn_1/attachments/" {
		t.Fatalf("docker URL = %q, %v", base, err)
	}
	native, err := attachmentBaseURL("http://127.0.0.1:9897", "turn_1", false)
	if err != nil || native != "http://127.0.0.1:9897/api/agent/turns/turn_1/attachments/" {
		t.Fatalf("native URL = %q, %v", native, err)
	}
	journal, err := OpenJournal(t.Context(), filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	spec, err := json.Marshal(adapter.TurnSpec{TurnID: "turn_1", AttachmentBaseURL: base, AttachmentBearer: "aat_private"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(spec, []byte("aat_private")) || bytes.Contains(spec, []byte("attachment_base_url")) {
		t.Fatalf("turn spec leaked fetch capability: %s", spec)
	}
	if _, err := journal.RecordStart(t.Context(), StartRecord{TurnID: "turn_1", AgentID: "agent", SpecJSON: spec}); err != nil {
		t.Fatal(err)
	}
	stored, err := journal.TurnSpecJSON(t.Context(), "turn_1")
	if err != nil || bytes.Contains(stored, []byte("aat_private")) {
		t.Fatalf("journaled fetch capability: %s, %v", stored, err)
	}
}
