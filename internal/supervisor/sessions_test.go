package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/ccsessions"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A session_scan that names a session and CopyTo copies it under the folder's Claude project directory and lists nothing; an unknown session
// or a missing session id is answered with a code.
func TestSessionScanCopiesASessionToAnotherFolder(t *testing.T) {
	t.Parallel()
	claude := t.TempDir()
	const id = "11111111-1111-4111-8111-111111111111"
	dir := filepath.Join(claude, "projects", ccsessions.FolderName("/work/proj"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"/work/proj","sessionId":"` + id + `","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{ClaudeConfigDir: claude}
	to := "/work/proj/.aimebu/worktrees/room_a-bert"
	reply := c.sessionScanResult(protocol.Frame{ReqID: "r1", ScanSessionID: id, CopyTo: to})
	if reply.Code != "" || reply.ReqID != "r1" || len(reply.Sessions) != 0 {
		t.Fatalf("copy: %+v", reply)
	}
	copied, err := os.ReadFile(filepath.Join(claude, "projects", ccsessions.FolderName(to), id+".jsonl"))
	if err != nil || string(copied) != line {
		t.Fatalf("copied %q err=%v", copied, err)
	}
	if reply := c.sessionScanResult(protocol.Frame{ScanSessionID: "22222222-2222-4222-8222-222222222222", CopyTo: to}); reply.Code != "session_not_found" {
		t.Fatalf("unknown session: %+v", reply)
	}
	if reply := c.sessionScanResult(protocol.Frame{CopyTo: to}); reply.Code != "invalid_request" {
		t.Fatalf("no session: %+v", reply)
	}
	if reply := c.sessionScanResult(protocol.Frame{ScanSessionID: id, CopyTo: "relative"}); reply.Code != "copy_failed" || !strings.Contains(reply.Message, "absolute") {
		t.Fatalf("relative target: %+v", reply)
	}
}
