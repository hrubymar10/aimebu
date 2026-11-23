package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// attachmentSaveFixture is a live turn of one agent whose note carries a zip, with the agent's key.
func attachmentSaveFixture(t *testing.T) (*fixture, store.Agent, store.Turn, store.Attachment, string) {
	t.Helper()
	f := newFixture(t)
	agent, turn, _, delivery := f.execution(t)
	resp, a := uploadFile(t, f, f.room.ID, "bundle.zip", append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0, 1, 2}, 2000)...))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE attachment SET message_id=? WHERE id=?`, delivery.Notes[0].MessageID, a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	key, err := f.srv.Auth.IssueAgentKey(t.Context(), agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, agent, turn, a, key
}

// installSaveMachine puts a stand-in connection for the agent's machine in place. answer, when set, answers each attachment_save frame.
func installSaveMachine(t *testing.T, f *fixture, supported bool, answer func(protocol.Frame) protocol.Frame) chan protocol.Frame {
	t.Helper()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{}, supportsAttachmentSave: supported}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	frames := make(chan protocol.Frame, 8)
	go func() {
		for {
			select {
			case frame := <-machine.sendQueue:
				frames <- frame
				if frame.Type == protocol.AttachmentSave && answer != nil {
					machine.harnessMu.Lock()
					waiter := machine.pathPending[frame.ReqID]
					machine.harnessMu.Unlock()
					if waiter != nil {
						waiter <- answer(frame)
					}
				}
			case <-machine.done:
				return
			}
		}
	}()
	t.Cleanup(func() { close(machine.done) })
	return frames
}

// mcpAttachmentGet calls attachment_get through the server's HTTP MCP endpoint, the channel of Docker agents and native Pi, OpenCode and
// Vibe, and returns the text of the result.
func mcpAttachmentGet(t *testing.T, f *fixture, key, attachmentID string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attachment_get","arguments":{"attachment_id":"` + attachmentID + `"}}}`
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Result.Content) == 0 {
		t.Fatalf("MCP answer %s: %v", raw, err)
	}
	return decoded.Result.Content[len(decoded.Result.Content)-1].Text
}

func TestAttachmentGetOverHTTPAsksTheSupervisorToSave(t *testing.T) {
	t.Parallel()
	f, agent, turn, a, key := attachmentSaveFixture(t)
	visible := "/workspace/.aimebu/attachments/" + a.ID + "/bundle.zip"
	frames := installSaveMachine(t, f, true, func(frame protocol.Frame) protocol.Frame {
		details, _ := json.Marshal(protocol.AttachmentSaved{Path: visible, MIME: "application/zip", Size: int(a.Size)})
		return protocol.Frame{V: protocol.Version, Type: protocol.AttachmentSaveResult, ReqID: frame.ReqID, Details: details}
	})
	text := mcpAttachmentGet(t, f, key, a.ID)
	if !strings.Contains(text, `"saved":true`) || !strings.Contains(text, `"path":"`+visible+`"`) || !strings.Contains(text, `"mime":"application/zip"`) {
		t.Fatalf("tool answer: %s", text)
	}
	select {
	case frame := <-frames:
		if frame.Type != protocol.AttachmentSave || frame.AgentID != agent.ID || frame.TurnID != turn.ID || frame.AttachmentID != a.ID || frame.ReqID == "" {
			t.Fatalf("frame sent to the machine: %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("no attachment_save frame reached the machine")
	}
}

func TestAttachmentGetOverHTTPSaysWhyNothingWasSaved(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]struct {
		install   bool
		supported bool
		answer    func(protocol.Frame) protocol.Frame
		want      string
	}{
		"old supervisor": {true, false, nil, "too old to save attachments"},
		"offline":        {false, false, nil, "not connected"},
		"refused": {true, true, func(frame protocol.Frame) protocol.Frame {
			return protocol.Frame{V: protocol.Version, Type: protocol.AttachmentSaveResult, ReqID: frame.ReqID, Code: "turn_not_running", Message: "the agent runs no such turn on this machine"}
		}, "the agent runs no such turn on this machine"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, _, _, a, key := attachmentSaveFixture(t)
			var frames chan protocol.Frame
			if item.install {
				frames = installSaveMachine(t, f, item.supported, item.answer)
			}
			text := mcpAttachmentGet(t, f, key, a.ID)
			if !strings.Contains(text, "cannot be saved to your workspace here") || !strings.Contains(text, item.want) || !strings.Contains(text, "application/zip") {
				t.Fatalf("tool answer: %s", text)
			}
			if item.install && !item.supported {
				select {
				case frame := <-frames:
					t.Fatalf("an old supervisor was sent %+v", frame)
				default:
				}
			}
		})
	}
}

// The save route has the download's access rule: an attachment outside the caller's live turn is not found, and no frame is sent.
func TestAttachmentSaveRouteRefusesAnAttachmentOutsideTheLiveTurn(t *testing.T) {
	t.Parallel()
	f, _, turn, a, key := attachmentSaveFixture(t)
	frames := installSaveMachine(t, f, true, nil)
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, turn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/attachments/"+a.ID+"/save", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ended turn: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, f.http.URL+"/api/agent/attachments/"+a.ID+"/save", nil)
	req.Header.Set("Authorization", "Bearer aak_"+strings.Repeat("x", 43))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", resp.StatusCode)
	}
	select {
	case frame := <-frames:
		t.Fatalf("a refused save sent %+v", frame)
	case <-time.After(100 * time.Millisecond):
	}
}
