package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestMessagePagesAndInlineReactions(t *testing.T) {
	f := newFixture(t)
	f.seed()
	var ids []string
	var seqs []int64
	for i := 0; i < 5; i++ {
		resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": fmt.Sprintf("message %d", i)}, true)
		if resp.StatusCode != 201 {
			t.Fatalf("post %d: %d", i, resp.StatusCode)
		}
		var message struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		ids = append(ids, message.ID)
		seqs = append(seqs, message.Seq)
	}
	resp := f.request(http.MethodPut, "/api/messages/"+ids[3]+"/reactions/"+url.PathEscape("👍"), nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("reaction: %d", resp.StatusCode)
	}
	resp.Body.Close()

	read := func(query string, want ...int64) {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages"+query, nil, false)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", query, resp.StatusCode)
		}
		var page []struct {
			ID        string              `json:"id"`
			Seq       int64               `json:"seq"`
			Reactions []map[string]string `json:"reactions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if len(page) != len(want) {
			t.Fatalf("%s: got %d messages, want %d", query, len(page), len(want))
		}
		for i, message := range page {
			if message.Seq != want[i] {
				t.Fatalf("%s[%d]: seq %d, want %d", query, i, message.Seq, want[i])
			}
			if message.Reactions == nil {
				t.Fatalf("%s[%d]: missing reactions", query, i)
			}
			if message.ID == ids[3] && (len(message.Reactions) != 1 || message.Reactions[0]["emoji"] != "👍") {
				t.Fatalf("%s[%d]: reaction=%v", query, i, message.Reactions)
			}
		}
	}
	read("", seqs...)
	read("?limit=2", seqs[3], seqs[4])
	read(fmt.Sprintf("?before=%d&limit=2", seqs[3]), seqs[1], seqs[2])
	read("?before="+ids[3]+"&limit=2", seqs[1], seqs[2])
	read(fmt.Sprintf("?after=%d&limit=2", seqs[1]), seqs[2], seqs[3])
	read("?around="+ids[4]+"&limit=3", seqs[2], seqs[3], seqs[4])
	read("?around="+ids[0]+"&limit=3", seqs[0], seqs[1], seqs[2])
	for _, query := range []string{"?limit=", "?limit=0", "?limit=501", "?before=", "?around=", "?before=-1", "?around=-1", "?before=1&after=1", "?before=1&around=1"} {
		resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages"+query, nil, false)
		if resp.StatusCode != 400 {
			t.Fatalf("%s: got %d, want 400", query, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp = f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages?around=missing-message", nil, false)
	if resp.StatusCode != 404 {
		t.Fatalf("missing cursor: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request(http.MethodGet, fmt.Sprintf("/api/rooms/%s/messages?around=%d", f.room.ID, seqs[0]+1), nil, false)
	if resp.StatusCode != 404 {
		t.Fatalf("missing sequence: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestThreadReplyPersistsWhenCountLookupFails(t *testing.T) {
	f := newFixture(t)
	f.seed()
	root, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "test", Body: "root"})
	if err != nil {
		t.Fatal(err)
	}
	f.srv.threadCountsQuery = func(context.Context, string, []store.Message) (map[string]int, error) {
		return nil, errors.New("injected count lookup failure")
	}
	resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "saved reply", "reply_to": root.ID}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status: got %d, want 201", resp.StatusCode)
	}
	var got map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := got["thread_count"]; ok {
		t.Fatalf("thread_count present after lookup failure: %s", got["thread_count"])
	}
	var id, body string
	if err := json.Unmarshal(got["id"], &id); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got["body"], &body); err != nil {
		t.Fatal(err)
	}
	if id == "" || body != "saved reply" {
		t.Fatalf("response id=%q body=%q", id, body)
	}
	saved, err := f.db.ListThreadRepliesLatest(t.Context(), f.room.ID, root.ID, 10)
	if err != nil || len(saved) != 1 || saved[0].ID != id || saved[0].Body != body {
		t.Fatalf("persisted replies=%+v err=%v", saved, err)
	}
}

func TestThreadPageAndFullRootCount(t *testing.T) {
	f := newFixture(t)
	f.seed()
	post := func(body string, root string) (string, int64, int) {
		t.Helper()
		payload := map[string]string{"body": body}
		if root != "" {
			payload["reply_to"] = root
		}
		resp := f.request(http.MethodPost, "/api/rooms/"+f.room.ID+"/messages", payload, true)
		if resp.StatusCode != 201 {
			t.Fatalf("post %q: %d", body, resp.StatusCode)
		}
		var message struct {
			ID          string `json:"id"`
			Seq         int64  `json:"seq"`
			ThreadCount int    `json:"thread_count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return message.ID, message.Seq, message.ThreadCount
	}
	root, _, _ := post("root", "")
	reply1, seq1, firstCount := post("first reply", root)
	if firstCount != 1 {
		t.Fatalf("first POST count: %d", firstCount)
	}
	post("unrelated", "")
	reply2, seq2, secondCount := post("second reply", root)
	if secondCount != 2 {
		t.Fatalf("second POST count: %d", secondCount)
	}
	read := func(query string) []struct {
		ID          string              `json:"id"`
		Seq         int64               `json:"seq"`
		ThreadCount int                 `json:"thread_count"`
		Reactions   []map[string]string `json:"reactions"`
	} {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages"+query, nil, false)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", query, resp.StatusCode)
		}
		var page []struct {
			ID          string              `json:"id"`
			Seq         int64               `json:"seq"`
			ThreadCount int                 `json:"thread_count"`
			Reactions   []map[string]string `json:"reactions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return page
	}
	rootPage := read("?around=" + root + "&limit=1")
	if len(rootPage) != 1 || rootPage[0].ThreadCount != 2 {
		t.Fatalf("root count: %+v", rootPage)
	}
	first := read("?thread_root=" + root + "&after=0&limit=1")
	if len(first) != 1 || first[0].ID != reply1 || first[0].Seq != seq1 || first[0].ThreadCount != 2 || first[0].Reactions == nil {
		t.Fatalf("first thread page: %+v", first)
	}
	second := read(fmt.Sprintf("?thread_root=%s&after=%d&limit=1", root, seq1))
	if len(second) != 1 || second[0].ID != reply2 || second[0].Seq != seq2 {
		t.Fatalf("second thread page: %+v", second)
	}
	latest := read("?thread_root=" + root + "&limit=1")
	if len(latest) != 1 || latest[0].ID != reply2 {
		t.Fatalf("latest thread page: %+v", latest)
	}
	before := read(fmt.Sprintf("?thread_root=%s&before=%d&limit=1", root, seq2))
	if len(before) != 1 || before[0].ID != reply1 {
		t.Fatalf("older thread page: %+v", before)
	}
	around := read("?thread_root=" + root + "&around=" + reply1 + "&limit=1")
	if len(around) != 1 || around[0].ID != reply1 {
		t.Fatalf("thread context: %+v", around)
	}
	for _, query := range []string{"?thread_root=", "?thread_root=" + root + "&before=-1", "?thread_root=" + root + "&around=-1"} {
		resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages"+query, nil, false)
		if resp.StatusCode != 400 {
			t.Fatalf("%s: got %d, want 400", query, resp.StatusCode)
		}
		resp.Body.Close()
	}
	// Any message of a conversation opens the whole of it: a reply named as thread_root gives its conversation's page.
	if fromReply := read("?thread_root=" + reply1 + "&limit=1"); len(fromReply) != 1 || fromReply[0].ID != reply2 {
		t.Fatalf("a reply opens its conversation: %+v", fromReply)
	}
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/messages?thread_root="+root+"&around="+root, nil, false)
	if resp.StatusCode != 404 {
		t.Fatalf("root is not a reply: %d", resp.StatusCode)
	}
	resp.Body.Close()
}
