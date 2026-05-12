package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hrubymar10/aimebu/internal/types"
)

func TestExportRoom_JSON_RoundTrip(t *testing.T) {
	s, srv := setupTestServer(t)

	alice, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", alice.ID, "hello world", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("general", alice.ID, "second message", false); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/rooms/general/export?format=json&agent_id=" + alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export returned %d, want 200", resp.StatusCode)
	}

	var got struct {
		Room struct {
			ID         string   `json:"id"`
			Members    []string `json:"members"`
			ExportedBy string   `json:"exported_by"`
			ExportedAt string   `json:"exported_at"`
			CreatedAt  string   `json:"created_at"`
		} `json:"room"`
		Messages []types.Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Room.ID != "general" {
		t.Errorf("room.id = %q, want general", got.Room.ID)
	}
	if got.Room.ExportedBy != alice.ID {
		t.Errorf("exported_by = %q, want %q", got.Room.ExportedBy, alice.ID)
	}
	if got.Room.ExportedAt == "" {
		t.Error("exported_at is empty")
	}
	if len(got.Messages) < 2 {
		t.Fatalf("want >=2 messages, got %d", len(got.Messages))
	}
	// Oldest-first order
	if got.Messages[0].ID >= got.Messages[len(got.Messages)-1].ID {
		t.Error("messages not in ascending ID order")
	}
	bodies := make(map[string]bool)
	for _, m := range got.Messages {
		bodies[m.Body] = true
	}
	for _, want := range []string{"hello world", "second message"} {
		if !bodies[want] {
			t.Errorf("message %q not found in export", want)
		}
	}
}

func TestExportRoom_Markdown_Content(t *testing.T) {
	s, srv := setupTestServer(t)

	bob, _, err := s.registerAI("gpt5", "codex", "test", nil, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("chat", bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.roomSend("chat", bob.ID, "hi there", false); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/rooms/chat/export?format=markdown&agent_id=" + bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export returned %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	md := string(body)

	if !strings.Contains(md, "# Room: chat") {
		t.Error("markdown missing room header")
	}
	if !strings.Contains(md, bob.ID) {
		t.Errorf("markdown missing exporter ID %q", bob.ID)
	}
	if !strings.Contains(md, "> hi there") {
		t.Error("markdown missing block-quoted message body")
	}
	// join system message should use italicized from
	if !strings.Contains(md, "_system_") {
		t.Error("markdown missing italicized _system_ from")
	}
	// message ID must appear in the header line
	if !strings.Contains(md, " #") {
		t.Error("markdown missing message ID (#NN) in header")
	}
}

func TestExportRoom_NonMember_403(t *testing.T) {
	s, srv := setupTestServer(t)

	owner, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "owner")
	if err != nil {
		t.Fatal(err)
	}
	outsider, _, err := s.registerAI("gpt5", "codex", "test", nil, "outsider")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("private", owner.ID); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/rooms/private/export?format=json&agent_id=" + outsider.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member export returned %d, want 403", resp.StatusCode)
	}
}

func TestExportRoom_UnknownFormat_400(t *testing.T) {
	s, srv := setupTestServer(t)

	agent, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/rooms/general/export?format=xml&agent_id=" + agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown format returned %d, want 400", resp.StatusCode)
	}
}

func TestExportRoom_FilenameHeader(t *testing.T) {
	s, srv := setupTestServer(t)

	// Use a DM-style room id to verify colon→underscore sanitization
	roomID := "dm:alice:bob"
	agent, _, err := s.registerAI("sonnet4.6", "claude-code", "test", nil, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom(roomID, agent.ID); err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"json", "markdown"} {
		resp, err := http.Get(srv.URL + "/rooms/" + roomID + "/export?format=" + format + "&agent_id=" + agent.ID)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("format %s returned %d, want 200", format, resp.StatusCode)
		}
		cd := resp.Header.Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") {
			t.Errorf("format %s: Content-Disposition missing 'attachment': %q", format, cd)
		}
		// Sanitized: colons replaced with underscores
		if !strings.Contains(cd, "dm_alice_bob") {
			t.Errorf("format %s: Content-Disposition missing sanitized room id 'dm_alice_bob': %q", format, cd)
		}
		ext := ".json"
		if format == "markdown" {
			ext = ".md"
		}
		if !strings.Contains(cd, ext) {
			t.Errorf("format %s: Content-Disposition missing extension %q: %q", format, ext, cd)
		}
	}
}
