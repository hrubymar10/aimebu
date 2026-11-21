package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (f *fixture) inboxNotes(t *testing.T) []store.InboxEntry {
	t.Helper()
	resp := f.request("GET", "/api/inbox", nil, false)
	defer resp.Body.Close()
	var body struct {
		Items []store.InboxEntry `json:"items"`
	}
	raw := new(bytes.Buffer)
	_, _ = raw.ReadFrom(resp.Body)
	if err := json.Unmarshal(raw.Bytes(), &body); err != nil || body.Items == nil {
		var list []store.InboxEntry
		if err := json.Unmarshal(raw.Bytes(), &list); err != nil {
			t.Fatalf("inbox answer: %v %s", err, raw.String())
		}
		body.Items = list
	}
	var notes []store.InboxEntry
	for _, item := range body.Items {
		if item.NoteID != "" {
			notes = append(notes, item)
		}
	}
	return notes
}

func (f *fixture) decide(t *testing.T, item store.InboxEntry, choice string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"choice": choice, "rev": item.Rev})
	request, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/inbox/"+item.ID+"/decision", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", f.csrf)
	request.Header.Set("Idempotency-Key", "note-"+item.ID+"-"+choice)
	request.AddCookie(f.cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// An agent's shared note waits for approval, and the owner finds it in the inbox with the note's words: Approve switches it on for every project,
// Decline deletes it, and either closes the card. An agent taking its note back withdraws the card.
func TestSharedNoteWaitingForApprovalIsInTheOwnersInbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, key := f.createToolAgent(t, "role_worker_default")
	resp := f.request("PUT", "/api/projects/"+f.project.ID+"/memory-enabled", map[string]bool{"enabled": true}, true)
	resp.Body.Close()
	write := func(body string) store.KnowledgeRecord {
		t.Helper()
		resp := f.toolPost(t, "/api/agent/memory", key, "", map[string]string{"scope": "agent_shared_notes", "scope_key": "global", "body": body})
		defer resp.Body.Close()
		var record store.KnowledgeRecord
		if resp.StatusCode != 201 && resp.StatusCode != 200 {
			t.Fatalf("agent note: %d", resp.StatusCode)
		}
		_ = json.NewDecoder(resp.Body).Decode(&record)
		return record
	}
	first := write("always run the formatter before committing")
	items := f.inboxNotes(t)
	if len(items) != 1 || items[0].NoteID != first.ID || items[0].Kind != "approval" || !items[0].CanDecide || !strings.Contains(items[0].Question, "always run the formatter") || len(items[0].Options) != 2 {
		t.Fatalf("a waiting note must be one inbox card with its words and two choices: %+v", items)
	}
	// A second note is a second card; Approve switches the first on and closes its card only.
	second := write("never edit generated files")
	if items = f.inboxNotes(t); len(items) != 2 {
		t.Fatalf("two waiting notes, two cards: %+v", items)
	}
	var firstCard, secondCard store.InboxEntry
	for _, item := range items {
		if item.NoteID == first.ID {
			firstCard = item
		} else {
			secondCard = item
		}
	}
	if status := f.decide(t, firstCard, "approve"); status != 201 {
		t.Fatalf("approve: %d", status)
	}
	if status := f.decide(t, firstCard, "approve"); status != 409 {
		t.Fatalf("a second answer to the same card must be refused (already resolved): %d", status)
	}
	live, err := f.db.ListActiveGlobalKnowledge(t.Context(), "agent_shared_notes")
	if err != nil || len(live) != 1 || live[0].ID != first.ID {
		t.Fatalf("approving must switch that note on and no other: %+v %v", live, err)
	}
	if items = f.inboxNotes(t); len(items) != 1 || items[0].NoteID != second.ID {
		t.Fatalf("the approved note's card must be gone: %+v", items)
	}
	// Decline deletes the note.
	if status := f.decide(t, secondCard, "deny"); status != 201 {
		t.Fatalf("decline: %d", status)
	}
	all, err := f.db.ListAllGlobalKnowledge(t.Context(), "agent_shared_notes")
	if err != nil || len(all) != 1 || all[0].ID != first.ID {
		t.Fatalf("declining must delete the note: %+v %v", all, err)
	}
	if items = f.inboxNotes(t); len(items) != 0 {
		t.Fatalf("no card is left: %+v", items)
	}
	// A note the agent takes back (or fixes) before anyone decides: the card follows it.
	third := write("a note that changes")
	fourth, err := f.db.ManageGlobalKnowledge(t.Context(), "agent_shared_notes", "global", f.project.ID, "agent", third.AuthorID, third.ID, third.Rev, "a note that changed", false)
	if err != nil {
		t.Fatal(err)
	}
	if items = f.inboxNotes(t); len(items) != 1 || !strings.Contains(items[0].Question, "a note that changed") || strings.Contains(items[0].Question, "that changes") {
		t.Fatalf("one card with the note's current words: %+v", items)
	}
	if _, err := f.db.ManageGlobalKnowledge(t.Context(), "agent_shared_notes", "global", f.project.ID, "agent", third.AuthorID, fourth.ID, fourth.Rev, "", true); err != nil {
		t.Fatal(err)
	}
	if items = f.inboxNotes(t); len(items) != 0 {
		t.Fatalf("a note taken back withdraws its card: %+v", items)
	}
}
