package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

type previewMessage struct {
	ID           string `json:"id"`
	No           int64  `json:"no"`
	Seq          int64  `json:"seq"`
	ReplyPreview *struct {
		No         int64  `json:"no"`
		AuthorKind string `json:"author_kind"`
		AuthorID   string `json:"author_id"`
		AuthorName string `json:"author_name"`
		Snippet    string `json:"snippet"`
	} `json:"reply_preview"`
}

func (f *fixture) listMessages(t *testing.T, roomID, query string) []previewMessage {
	t.Helper()
	resp := f.request("GET", "/api/rooms/"+roomID+"/messages"+query, nil, false)
	defer resp.Body.Close()
	var out []previewMessage
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("messages%s: %d", query, resp.StatusCode)
	}
	return out
}

func (f *fixture) postReply(t *testing.T, roomID, body, replyTo string, attachments ...string) previewMessage {
	t.Helper()
	payload := map[string]any{"body": body, "attachments": attachments}
	if replyTo != "" {
		payload["reply_to"] = replyTo
	}
	resp := f.request("POST", "/api/rooms/"+roomID+"/messages", payload, true)
	defer resp.Body.Close()
	var out previewMessage
	if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("post: %d", resp.StatusCode)
	}
	return out
}

// A reply carries a preview of the message it answers, so a client that has not loaded that message can still quote it.
func TestReplyPreviewComesWithTheReply(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	long := "  First line\n\n   " + strings.Repeat("word ", 60)
	parent := f.postReply(t, f.room.ID, long, "")
	reply := f.postReply(t, f.room.ID, "answer", parent.ID)
	if reply.ReplyPreview == nil || reply.ReplyPreview.No != parent.No || reply.ReplyPreview.AuthorName != "owner" {
		t.Fatalf("the reply created through the API has no preview: %+v", reply.ReplyPreview)
	}
	resp, att := uploadFile(t, f, f.room.ID, "plan.png", testPNG)
	resp.Body.Close()
	files := f.postReply(t, f.room.ID, "", "", att.ID)
	second := f.postReply(t, f.room.ID, "about the picture", files.ID)

	// The latest page holds only the last reply; the message it answers is outside it.
	latest := f.listMessages(t, f.room.ID, "?limit=1")
	if len(latest) != 1 || latest[0].ID != second.ID || latest[0].ReplyPreview == nil {
		t.Fatalf("latest page: %+v", latest)
	}
	got := latest[0].ReplyPreview
	if got.No != files.No || got.Snippet != "[attachment]" || got.AuthorKind != "user" || got.AuthorName != "owner" {
		t.Fatalf("a parent that is only a file: %+v", got)
	}
	// A long text is one line cut to 96 characters, whitespace collapsed.
	page := f.listMessages(t, f.room.ID, fmt.Sprintf("?after=%d&limit=1", parent.Seq))
	if len(page) != 1 || page[0].ID != reply.ID || page[0].ReplyPreview == nil {
		t.Fatalf("page after the parent: %+v", page)
	}
	snippet := page[0].ReplyPreview.Snippet
	if !strings.HasPrefix(snippet, "First line word word") || strings.ContainsAny(snippet, "\n") || len([]rune(snippet)) != 96 || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("snippet %q (%d runes)", snippet, len([]rune(snippet)))
	}
	// Leading blank lines and spaces do not hide the text from the snippet, however many there are.
	blanky := f.postReply(t, f.room.ID, strings.Repeat(" \n", 700)+"the real words", "")
	answer := f.postReply(t, f.room.ID, "to the blanks", blanky.ID)
	if answer.ReplyPreview == nil || answer.ReplyPreview.Snippet != "the real words" {
		t.Fatalf("a parent with a long blank start: %+v", answer.ReplyPreview)
	}
	// A message that answers nothing carries no preview.
	for _, message := range f.listMessages(t, f.room.ID, "") {
		if (message.ID == parent.ID || message.ID == files.ID) && message.ReplyPreview != nil {
			t.Fatalf("%s answers nothing but has a preview", message.ID)
		}
	}
}

// A reply link that points into another room (an import can leave one) previews nothing: a page never reveals another room's text.
func TestReplyPreviewNeverCrossesRooms(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	otherResp := f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": "other"}, true)
	var other store.Room
	if err := json.NewDecoder(otherResp.Body).Decode(&other); err != nil || otherResp.StatusCode != 201 {
		t.Fatalf("room: %d %v", otherResp.StatusCode, err)
	}
	otherResp.Body.Close()
	secret := f.postReply(t, other.ID, "secret of the other room", "")
	mine := f.postReply(t, f.room.ID, "mine", "")
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE message SET reply_to=? WHERE id=?`, secret.ID, mine.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, message := range f.listMessages(t, f.room.ID, "") {
		if message.ID == mine.ID && message.ReplyPreview != nil {
			t.Fatalf("the preview crossed rooms: %+v", message.ReplyPreview)
		}
	}
}

func TestMessageSnippet(t *testing.T) {
	t.Parallel()
	if got := store.MessageSnippet("  a \n b\t c ", 0); got != "a b c" {
		t.Fatalf("collapse: %q", got)
	}
	if store.MessageSnippet("", 2) != "[attachment]" || store.MessageSnippet("", 0) != "" || store.MessageSnippet("text", 3) != "text" {
		t.Fatal("attachment placeholder")
	}
}
