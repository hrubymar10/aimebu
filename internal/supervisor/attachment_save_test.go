package supervisor

import (
	"bytes"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// attachmentSaveClient is a supervisor running one ordinary turn (turn_live) for agt_a in checkout, holding agt_a's key, whose server
// serves one attachment to that key only.
func attachmentSaveClient(t *testing.T, data []byte, launcher *protocol.LauncherRow) (*Client, string, *atomic.Int64) {
	t.Helper()
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fetches := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/agent/attachments/att_01ZIP" || r.Header.Get("Authorization") != "Bearer aak_agent_a" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "../bundle.zip"}))
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	c := &Client{ServerURL: server.URL, HTTPClient: server.Client(), active: map[string]string{"agt_a": "turn_live", "agt_b": "turn_b"},
		agentKeys: map[string]string{"agt_a": "aak_agent_a", "agt_b": "aak_agent_b"}}
	turn := activeTurn{turnID: "turn_live", mode: adapter.ModeNormal, checkout: checkout}
	if launcher != nil {
		turn.launcherID = launcher.ID
		c.Launchers = []protocol.LauncherRow{*launcher}
	}
	c.noteActiveTurn("agt_a", turn)
	c.noteActiveTurn("agt_b", activeTurn{turnID: "turn_b", mode: adapter.ModeNormal, checkout: checkout})
	return c, checkout, fetches
}

func saveFrame(agentID, turnID string) protocol.Frame {
	return protocol.Frame{V: protocol.Version, Type: protocol.AttachmentSave, ReqID: "req1", AgentID: agentID, TurnID: turnID, AttachmentID: "att_01ZIP"}
}

func TestAttachmentSaveWritesIntoTheTurnCheckout(t *testing.T) {
	data := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{7}, 4096)...)
	c, checkout, _ := attachmentSaveClient(t, data, nil)
	result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live"))
	if result.Type != protocol.AttachmentSaveResult || result.ReqID != "req1" || result.Code != "" {
		t.Fatalf("result = %+v", result)
	}
	var saved protocol.AttachmentSaved
	if err := json.Unmarshal(result.Details, &saved); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(checkout, ".aimebu", "attachments", "att_01ZIP", "bundle.zip")
	if saved.Path != want || saved.MIME != "application/zip" || saved.Size != len(data) {
		t.Fatalf("saved = %+v, want %s", saved, want)
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("bytes differ: %v", err)
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info.Mode(), err)
	}
}

// A Docker launcher sees the checkout under its own path: the answer is the saved file's path mapped through the launcher's path map.
func TestAttachmentSaveAnswersThePathTheContainerSees(t *testing.T) {
	launcher := &protocol.LauncherRow{LauncherDefinition: protocol.LauncherDefinition{ID: "lch_docker", Tool: "claude", Containerized: true}, Available: true}
	c, checkout, _ := attachmentSaveClient(t, []byte("PK"), launcher)
	c.Launchers[0].PathMaps = []protocol.LauncherPathMap{{Host: checkout, Visible: "/workspace"}}
	result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live"))
	var saved protocol.AttachmentSaved
	if result.Code != "" || json.Unmarshal(result.Details, &saved) != nil {
		t.Fatalf("result = %+v", result)
	}
	if saved.Path != "/workspace/.aimebu/attachments/att_01ZIP/bundle.zip" {
		t.Fatalf("visible path = %q", saved.Path)
	}
	if _, err := os.Stat(filepath.Join(checkout, ".aimebu", "attachments", "att_01ZIP", "bundle.zip")); err != nil {
		t.Fatalf("host file: %v", err)
	}
	// A launcher that is gone leaves no path the agent can see.
	c.Launchers = nil
	if result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live")); result.Code != "save_failed" {
		t.Fatalf("missing launcher: %+v", result)
	}
}

func TestAttachmentSaveRefusesATurnThatIsNotTheAgents(t *testing.T) {
	c, checkout, fetches := attachmentSaveClient(t, []byte("PK"), nil)
	for name, frame := range map[string]protocol.Frame{
		"another agent's turn": saveFrame("agt_a", "turn_b"),
		"an ended turn":        saveFrame("agt_a", "turn_old"),
		"no turn":              saveFrame("agt_a", ""),
		"unknown agent":        saveFrame("agt_c", "turn_live"),
	} {
		if result := c.saveAttachment(t.Context(), frame); result.Code != "turn_not_running" {
			t.Fatalf("%s: %+v", name, result)
		}
	}
	c.mu.Lock()
	c.activeTurns["agt_a"] = activeTurn{turnID: "turn_live", mode: adapter.ModeDiagnostic, checkout: checkout}
	c.mu.Unlock()
	if result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live")); result.Code != "turn_not_running" {
		t.Fatalf("diagnostic turn: %+v", result)
	}
	if fetches.Load() != 0 {
		t.Fatalf("a refused save fetched the attachment %d times", fetches.Load())
	}
	if _, err := os.Stat(filepath.Join(checkout, ".aimebu")); !os.IsNotExist(err) {
		t.Fatalf("a refused save touched the checkout: %v", err)
	}
}

func TestAttachmentSaveRelaysFetchAndSizeFailures(t *testing.T) {
	c, checkout, _ := attachmentSaveClient(t, bytes.Repeat([]byte{1}, agenttools.MaxAttachmentBytes+1), nil)
	if result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live")); result.Code != "fetch_failed" || !strings.Contains(result.Message, "larger than 10 MB") {
		t.Fatalf("oversize: %+v", result)
	}
	// The server refuses agt_b's key for this attachment (it is not in agt_b's turn).
	if result := c.saveAttachment(t.Context(), saveFrame("agt_b", "turn_b")); result.Code != "fetch_failed" || !strings.Contains(result.Message, "HTTP 404") {
		t.Fatalf("refused fetch: %+v", result)
	}
	c.mu.Lock()
	delete(c.agentKeys, "agt_a")
	c.mu.Unlock()
	if result := c.saveAttachment(t.Context(), saveFrame("agt_a", "turn_live")); result.Code != "no_key" {
		t.Fatalf("no key: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(checkout, ".aimebu")); !os.IsNotExist(err) {
		t.Fatalf("a failed save touched the checkout: %v", err)
	}
}
