package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// A superseded ask is out of the Inbox but not over: it rides along with its state for the chat, and answering it resolves it and wakes the
// asking agent exactly as an open ask does.
func TestSupersededAskRidesAlongAndStaysAnswerable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, turn, _, _ := f.execution(t)
	ctx := t.Context()
	raw := `{"messages":[{"room":"` + f.room.ID + `","body":"Choose","ask":{"kind":"question","text":"Which?","options":["A","B"]}}]}`
	parsed, err := answer.Parse([]byte(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(raw))
	receipt, err := f.db.ApplyAnswer(ctx, store.AnswerInput{TurnID: turn.ID, AgentID: agent.ID, Generation: turn.Generation,
		Digest: hex.EncodeToString(digest[:]), Candidate: []byte(raw), Parsed: &parsed})
	if err != nil || receipt.Result != "committed" {
		t.Fatalf("ask receipt=%+v err=%v", receipt, err)
	}
	list := func() []store.InboxEntry {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/inbox", nil, false)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("inbox status=%d", resp.StatusCode)
		}
		var items []store.InboxEntry
		if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	items := list()
	if len(items) != 1 || items[0].State != "open" || items[0].MessageID == "" {
		t.Fatalf("open ask=%+v", items)
	}
	item := items[0]
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "A newer message"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("superseding message status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	items = list()
	if len(items) != 1 || items[0].ID != item.ID || items[0].State != "superseded" || items[0].Rev != item.Rev+1 {
		t.Fatalf("superseded ask=%+v", items)
	}
	body, _ := json.Marshal(map[string]any{"body": "A", "rev": items[0].Rev})
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/"+item.ID+"/answer", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", "superseded-answer")
	req.AddCookie(f.cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("answer status=%d: %s", resp.StatusCode, data)
	}
	var view map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	id, _ := view["id"].(string)
	if id == "" || view["reply_to"] != item.MessageID {
		t.Fatalf("answer message=%v", view)
	}
	if items := list(); len(items) != 0 {
		t.Fatalf("answered ask still listed: %+v", items)
	}
	if _, err := f.db.NoteForAgentMessage(ctx, agent.ID, id); err != nil {
		t.Fatalf("asking agent was not woken: %v", err)
	}
}
