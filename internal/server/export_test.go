package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func exportBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExportRoomJSONAndMarkdown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp, att := uploadFile(t, f, f.room.ID, "plan.png", testPNG)
	resp.Body.Close()
	first := f.postMessage(f.room, "First  line\n\nsecond line")
	reply := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]any{"body": "An answer", "reply_to": first, "attachments": []string{att.ID}}, true)
	var replyMessage struct {
		ID string `json:"id"`
		No int64  `json:"no"`
	}
	if err := json.NewDecoder(reply.Body).Decode(&replyMessage); err != nil || reply.StatusCode != 201 {
		t.Fatalf("reply: %d %v", reply.StatusCode, err)
	}
	reply.Body.Close()
	if r := f.request("PUT", "/api/messages/"+first+"/reactions/%F0%9F%91%8D", nil, true); r.StatusCode >= 300 {
		t.Fatalf("react: %d", r.StatusCode)
	}

	// JSON: the room, every message with its reactions, the resolved author, the number a reply answers, and no server path.
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/export?format=json", nil, false)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="`) || !strings.HasSuffix(resp.Header.Get("Content-Disposition"), `.json"`) {
		t.Fatalf("json export: %d %v", resp.StatusCode, resp.Header)
	}
	raw := exportBody(t, resp)
	var out struct {
		Room     store.Room       `json:"room"`
		Exported string           `json:"exported_at"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("json export is not JSON: %v\n%s", err, raw)
	}
	if out.Room.ID != f.room.ID || out.Exported == "" || len(out.Messages) != 2 {
		t.Fatalf("json export: %+v", out)
	}
	if out.Messages[0]["body"] != "First  line\n\nsecond line" || out.Messages[0]["author_label"] != "owner" {
		t.Fatalf("first message: %+v", out.Messages[0])
	}
	for _, key := range []string{"expirable", "thread_count", "author_label", "reactions"} {
		if _, ok := out.Messages[0][key]; !ok {
			t.Fatalf("the export lacks %q, which the messages route carries: %+v", key, out.Messages[0])
		}
	}
	if reactions, _ := out.Messages[0]["reactions"].([]any); len(reactions) != 1 {
		t.Fatalf("reactions: %+v", out.Messages[0]["reactions"])
	}
	second := out.Messages[1]
	if int64(second["reply_to_no"].(float64)) != int64(out.Messages[0]["no"].(float64)) || second["reply_to"] != first {
		t.Fatalf("reply: %+v", second)
	}
	if strings.Contains(raw, `"path"`) {
		t.Fatalf("the export names a file's place on the server: %s", raw)
	}
	if attachments, _ := second["attachments"].([]any); len(attachments) != 1 || attachments[0].(map[string]any)["name"] != "plan.png" {
		t.Fatalf("attachments: %+v", second["attachments"])
	}

	// Markdown: readable, with the reply, the attachment and the reactions named.
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/export?format=markdown", nil, false)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") || !strings.HasSuffix(resp.Header.Get("Content-Disposition"), `.md"`) {
		t.Fatalf("markdown export: %d %v", resp.StatusCode, resp.Header)
	}
	md := exportBody(t, resp)
	for _, want := range []string{
		"# task",
		fmt.Sprintf("## #%d · owner · ", int64(out.Messages[0]["no"].(float64))),
		fmt.Sprintf("## #%d · owner · ", replyMessage.No),
		fmt.Sprintf("> ↪ in reply to #%d owner: First line second line", int64(out.Messages[0]["no"].(float64))),
		"An answer",
		"- 📎 plan.png (image/png, ",
		"Reactions: 👍 owner",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestExportRoomRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.postMessage(f.room, "hello")
	for _, query := range []string{"", "?format=", "?format=xml", "?format=JSON"} {
		resp := f.request("GET", "/api/rooms/"+f.room.ID+"/export"+query, nil, false)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("export%s: %d, want 400", query, resp.StatusCode)
		}
	}
	resp := f.request("GET", "/api/rooms/room_nope/export?format=json", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("a room that does not exist: %d", resp.StatusCode)
	}
	// Someone with no access to the project cannot read the room, so cannot export it; nor can a signed-out caller.
	cookie, csrf := registerSecondUser(t, f, "stranger")
	other := f.requestAs(t, cookie, csrf, "GET", "/api/rooms/"+f.room.ID+"/export?format=json", nil)
	other.Body.Close()
	if other.StatusCode != 403 && other.StatusCode != 404 {
		t.Errorf("a user outside the project: %d", other.StatusCode)
	}
	saved := f.cookie
	f.cookie = nil
	anon := f.request("GET", "/api/rooms/"+f.room.ID+"/export?format=json", nil, false)
	anon.Body.Close()
	f.cookie = saved
	if anon.StatusCode != 401 {
		t.Errorf("signed out: %d", anon.StatusCode)
	}
}

// A room longer than one page streams in order, with every message once and valid JSON throughout.
func TestExportRoomStreamsLongHistoryInOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.setExportHooks(7, nil)
	const total = 7*2 + 3
	for i := 0; i < total; i++ {
		if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "owner", Body: fmt.Sprintf("m%d", i), TargetsJSON: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	resp := f.request("GET", "/api/rooms/"+f.room.ID+"/export?format=json", nil, false)
	var out struct {
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(exportBody(t, resp)), &out); err != nil || len(out.Messages) != total {
		t.Fatalf("long export: %d messages, %v", len(out.Messages), err)
	}
	for i, message := range out.Messages {
		if message.Body != fmt.Sprintf("m%d", i) {
			t.Fatalf("message %d is %q", i, message.Body)
		}
	}
	md := exportBody(t, f.request("GET", "/api/rooms/"+f.room.ID+"/export?format=markdown", nil, false))
	if strings.Count(md, "\n## #") != total || strings.Index(md, "\nm0\n") > strings.Index(md, fmt.Sprintf("\nm%d\n", total-1)) {
		t.Fatalf("markdown has %d messages, or not in order", strings.Count(md, "\n## #"))
	}
}

func TestExportFilename(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"General": "General.json", "a b/c..d": "a-b-c-d.json", "../../etc": "etc.json", "": "room.json", "日本語": "room.json"} {
		if got := exportFilename(in, "json"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := exportFilename("Plan", "markdown"); got != "Plan.md" {
		t.Errorf("markdown: %q", got)
	}
}

// An error before anything is sent is an ordinary error response, with no download headers.
func TestExportRoomFailsCleanlyBeforeTheFirstPage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.exportPageFn = func(context.Context, string, int64, int) ([]store.Message, error) {
		return nil, errors.New("disk gone")
	}
	for _, format := range []string{"json", "markdown"} {
		resp := f.request("GET", "/api/rooms/"+f.room.ID+"/export?format="+format, nil, false)
		resp.Body.Close()
		if resp.StatusCode != 500 || resp.Header.Get("Content-Disposition") != "" {
			t.Errorf("%s: %d %v, want a plain 500 with no download headers", format, resp.StatusCode, resp.Header)
		}
	}
}

// An error after the first page cannot change the status that was sent, so it must break the connection: a truncated file must never come
// back as a download that ends cleanly.
func TestExportRoomFailureLaterBreaksTheDownload(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	for i := 0; i < 12; i++ {
		if _, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: "owner", Body: fmt.Sprintf("m%d", i), TargetsJSON: "[]"}); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	f.srv.setExportHooks(5, func(ctx context.Context, roomID string, after int64, limit int) ([]store.Message, error) {
		if calls.Add(1) > 1 {
			return nil, errors.New("disk gone")
		}
		return f.db.ListMessagesAfterLimit(ctx, roomID, after, limit)
	})
	for _, format := range []string{"json", "markdown"} {
		calls.Store(0)
		req, _ := http.NewRequest("GET", f.http.URL+"/api/rooms/"+f.room.ID+"/export?format="+format, nil)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue // the connection broke before the headers arrived: also a failed download
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Errorf("%s: a download that stopped after one page was delivered whole (status %d, %d bytes, ends %q)", format, resp.StatusCode, len(data), string(data[max(0, len(data)-40):]))
		}
		if format == "json" && json.Valid(data) {
			t.Errorf("json: the truncated file is valid JSON")
		}
	}
}
