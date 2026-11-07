package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/usages"
)

type fixture struct {
	t          *testing.T
	db         *store.Store
	srv        *Server
	http       *httptest.Server
	cookie     *http.Cookie
	csrf       string
	project    store.Project
	room       store.Room
	machineID  string
	machineKey string
}

func TestNewUsesProductionPasswordPolicy(t *testing.T) {
	t.Parallel()
	policy := New(nil).Auth.Policy
	if policy.Iterations != 600_000 || policy.SaltBytes != 16 {
		t.Fatalf("production password policy changed: %+v", policy)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.sqlite")
	if err := os.WriteFile(path, blankTestDB, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := auth.NewService(db)
	a.Policy = auth.PasswordPolicy{Iterations: 1_000, SaltBytes: 16}
	_, machineID, machineKey, _, err := a.Bootstrap(context.Background(), "owner", "correct horse battery staple", "test-host")
	if err != nil {
		t.Fatal(err)
	}
	seedTestHarnesses(t, db, machineID)
	s := New(db)
	s.Auth.Policy = a.Policy
	s.PollInterval = 5 * time.Millisecond
	s.WriteTimeout = 100 * time.Millisecond
	h := httptest.NewServer(s.Handler())
	t.Cleanup(h.Close)
	// A test that started the token-copy goroutines cancels their context when it returns (its defer runs before cleanups); this joins them, so
	// none is still writing into the test's directories when those are removed. It returns at once when they were never started.
	t.Cleanup(func() {
		joined := make(chan struct{})
		go func() { s.WaitTokenCopies(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(30 * time.Second):
			t.Error("the token-copy goroutines did not stop after the test's context was cancelled")
		}
	})
	f := &fixture{t: t, db: db, srv: s, http: h, machineID: machineID, machineKey: machineKey}
	resp := f.request(http.MethodPost, "/api/login", map[string]string{"login": "owner", "password": "correct horse battery staple"}, false)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	f.cookie = resp.Cookies()[0]
	var login struct {
		CSRFToken string `json:"csrf_token"`
	}
	json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()
	f.csrf = login.CSRFToken
	return f
}

// offlineTransport answers every request with an error at once: a fixture's provider credentials are made up, and a test must never reach the
// provider's real servers (the refresh a switch starts would otherwise call them, with retries and back-off, long after the test is over).
type offlineTransport struct{}

func (offlineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the network is off in tests")
}

// useUsages gives the server a usages reader that cannot reach the network and, when the test is over, waits for the refreshes a switch
// started in the background (refreshSwitchedUsage), so none can write into the reader's directory while the test's temp directories are
// removed. Call it after the reader's directory exists: cleanups run last-in first-out, so this one then runs before that directory is removed.
func (f *fixture) useUsages(reader *usages.Reader) {
	f.t.Helper()
	reader.SetHTTPClient(&http.Client{Transport: offlineTransport{}})
	f.srv.Usages = reader
	f.t.Cleanup(f.srv.WaitUsageRefreshes)
}

func (f *fixture) request(method, path string, body any, csrf bool) *http.Response {
	f.t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, f.http.URL+path, bytes.NewReader(data))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}
func (f *fixture) seed() {
	f.t.Helper()
	resp := f.request("POST", "/api/projects", map[string]string{"name": "p", "folder": f.t.TempDir()}, true)
	if resp.StatusCode != 201 {
		f.t.Fatalf("project: %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&f.project)
	resp.Body.Close()
	resp = f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": "task"}, true)
	if resp.StatusCode != 201 {
		f.t.Fatalf("room: %d", resp.StatusCode)
	}
	json.NewDecoder(resp.Body).Decode(&f.room)
	resp.Body.Close()
}

func TestProjectHomeAndRoomKindValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request("GET", "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	if err := json.NewDecoder(resp.Body).Decode(&rooms); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	kinds := map[string]int{}
	for _, room := range rooms {
		if room.ProjectID != f.project.ID {
			t.Fatalf("room in wrong project: %+v", room)
		}
		kinds[room.Kind]++
	}
	// The home room, the "general" task room every project starts with, and the seeded task room.
	if len(rooms) != 3 || kinds["home"] != 1 || kinds["task"] != 2 {
		t.Fatalf("project rooms: %+v", rooms)
	}
	for _, kind := range []string{"home", "lobby", "dm", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			resp := f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": "bad", "kind": kind}, true)
			defer resp.Body.Close()
			var result map[string]string
			_ = json.NewDecoder(resp.Body).Decode(&result)
			if resp.StatusCode != 400 || result["error"] != "invalid_kind" {
				t.Fatalf("kind %q: status=%d body=%v", kind, resp.StatusCode, result)
			}
		})
	}
}
func (f *fixture) dial(t *testing.T, after int64, origin string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Origin", origin)
	header.Set("Cookie", f.cookie.String())
	conn, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events?after="+strconv.FormatInt(after, 10), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v status %d", err, status)
	}
	return conn
}
func readFrame(t *testing.T, c *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAuthenticationCSRFAndCRUD(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	req, _ := http.NewRequest("GET", f.http.URL+"/api/projects", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous status=%d", resp.StatusCode)
	}
	var authError map[string]string
	json.NewDecoder(resp.Body).Decode(&authError)
	if authError["error"] != "unauthorized" {
		t.Fatalf("auth error=%v", authError)
	}
	resp.Body.Close()
	resp = f.request("POST", "/api/projects", map[string]string{"name": "p", "folder": "/tmp/p"}, false)
	if resp.StatusCode != 403 {
		t.Fatalf("missing CSRF status=%d", resp.StatusCode)
	}
	var csrfError map[string]string
	json.NewDecoder(resp.Body).Decode(&csrfError)
	if csrfError["error"] != "invalid_csrf" {
		t.Fatalf("csrf error=%v", csrfError)
	}
	resp.Body.Close()
	f.seed()
	resp = f.request("GET", "/api/projects", nil, false)
	var projects []store.Project
	json.NewDecoder(resp.Body).Decode(&projects)
	resp.Body.Close()
	if len(projects) != 1 || projects[0].ID != f.project.ID {
		t.Fatalf("projects=%+v", projects)
	}
	resp = f.request("GET", "/api/projects", nil, false)
	var rawProjects []map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&rawProjects)
	resp.Body.Close()
	if string(rawProjects[0]["repo_url"]) != "null" {
		t.Fatalf("repo_url=%s", rawProjects[0]["repo_url"])
	}
	resp = f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "hello"}, true)
	if resp.StatusCode != 201 {
		t.Fatalf("post status=%d", resp.StatusCode)
	}
	var message store.Message
	json.NewDecoder(resp.Body).Decode(&message)
	resp.Body.Close()
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/messages?after=0", nil, false)
	var messages []store.Message
	json.NewDecoder(resp.Body).Decode(&messages)
	resp.Body.Close()
	if len(messages) != 1 || messages[0].ID != message.ID {
		t.Fatalf("messages=%+v", messages)
	}
	resp = f.request("GET", "/api/rooms/"+f.room.ID+"/messages?after=0", nil, false)
	var rawMessages []map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&rawMessages)
	resp.Body.Close()
	if string(rawMessages[0]["targets"]) != "[]" {
		t.Fatalf("targets=%s", rawMessages[0]["targets"])
	}
	if _, ok := rawMessages[0]["targets_json"]; ok {
		t.Fatal("raw targets_json exposed")
	}
	path := "/api/messages/" + message.ID + "/reactions/" + url.PathEscape("👍")
	resp = f.request("PUT", path, nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("reaction status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("GET", "/api/messages/"+message.ID+"/reactions", nil, false)
	var reactions []map[string]string
	json.NewDecoder(resp.Body).Decode(&reactions)
	resp.Body.Close()
	if len(reactions) != 1 || reactions[0]["emoji"] != "👍" || reactions[0]["member_name"] != "owner" {
		t.Fatalf("reactions=%v", reactions)
	}
	resp = f.request("POST", "/api/logout", nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("logout status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = f.request("GET", "/api/me", nil, false)
	if resp.StatusCode != 401 {
		t.Fatalf("revoked status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestThreadRootValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	post := func(room string, body map[string]string, want int) map[string]any {
		t.Helper()
		resp := f.request("POST", "/api/rooms/"+room+"/messages", body, true)
		defer resp.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("post %v: status=%d result=%v", body, resp.StatusCode, result)
		}
		return result
	}
	root := post(f.room.ID, map[string]string{"body": "root"}, 201)["id"].(string)
	reply := post(f.room.ID, map[string]string{"body": "reply", "thread_root": root}, 201)
	if reply["thread_root"] != root {
		t.Fatalf("thread root omitted: %v", reply)
	}
	if got := post(f.room.ID, map[string]string{"body": "bad", "thread_root": "msg_missing"}, 400); got["error"] != "invalid_answer" {
		t.Fatalf("missing root: %v", got)
	}
	if got := post(f.room.ID, map[string]string{"body": "bad", "thread_root": reply["id"].(string)}, 400); got["error"] != "invalid_answer" {
		t.Fatalf("nested thread: %v", got)
	}
	other := f.request("POST", "/api/projects/"+f.project.ID+"/rooms", map[string]string{"title": "other"}, true)
	defer other.Body.Close()
	var otherRoom store.Room
	if err := json.NewDecoder(other.Body).Decode(&otherRoom); err != nil || other.StatusCode != 201 {
		t.Fatalf("other room=%+v status=%d err=%v", otherRoom, other.StatusCode, err)
	}
	if got := post(otherRoom.ID, map[string]string{"body": "bad", "thread_root": root}, 400); got["error"] != "invalid_answer" {
		t.Fatalf("cross-room root: %v", got)
	}
}

func TestInboxReadShowsOpenQuestions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	resp := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "Choose a route"}, true)
	var ask store.Message
	if err := json.NewDecoder(resp.Body).Decode(&ask); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("ask message status=%d", resp.StatusCode)
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES (?,?,?,?,'agent','agt_test','question',?,'now')`, "inb_test", f.project.ID, f.room.ID, ask.ID, `{"text":"Choose a route","options":["A","B"],"free_text":true}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request("GET", "/api/inbox", nil, false)
	var items []store.InboxEntry
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(items) != 1 || items[0].Question != "Choose a route" || items[0].Rev != 0 {
		t.Fatalf("inbox status=%d items=%+v", resp.StatusCode, items)
	}
	if items[0].MessageID != ask.ID || !items[0].FreeText || len(items[0].Options) != 2 {
		t.Fatalf("inbox fields=%+v", items[0])
	}
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE inbox_item SET state='resolved' WHERE id='inb_test'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = f.request("GET", "/api/inbox", nil, false)
	items = nil
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(items) != 0 {
		t.Fatalf("resolved item still open: %+v", items)
	}
}

func TestInboxReadShowsRecovery(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,created_at)
			VALUES (?,?,?,'agent','agt_missing','recovery','now')`, "inb_recovery", f.project.ID, f.room.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request("GET", "/api/inbox", nil, false)
	defer resp.Body.Close()
	var items []store.InboxEntry
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(items) != 1 || items[0].Kind != "recovery" || items[0].Question == "" {
		t.Fatalf("recovery inbox status=%d items=%+v", resp.StatusCode, items)
	}
}

func TestLoginRateLimitJSON(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	now := time.Now()
	f.srv.Auth.Now = func() time.Time { return now }
	for i := 0; i < 6; i++ {
		resp := f.request("POST", "/api/login", map[string]string{"login": "owner", "password": "wrong"}, false)
		if resp.StatusCode != 401 {
			t.Fatalf("failure %d status=%d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := f.request("POST", "/api/login", map[string]string{"login": "owner", "password": "wrong"}, false)
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("throttle: status=%d retry=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["error"] != "rate_limited" {
		t.Fatalf("throttle body=%v err=%v", body, err)
	}
}

func TestLoginRetryAfterUsesCanonicalName(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	f.srv.Auth.Now = func() time.Time { return now }
	// The failures come from the address the HTTP probe below comes from (loopback in the fixture): the throttle counts each source on its own.
	for attempt := 0; attempt < 9; attempt++ {
		if _, err := f.srv.Auth.Login(t.Context(), "owner", "wrong", "127.0.0.1"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", attempt, err)
		}
		if attempt < 8 {
			now = now.Add(31 * time.Second)
		}
	}
	resp := f.request(http.MethodPost, "/api/login", map[string]string{"login": " OWNER ", "password": "wrong"}, false)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("canonical retry delay: status=%d retry=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestEventReplayLiveOrderAndOrigin(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, high, err := f.db.EventBounds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := f.request("GET", "/api/events/cursor", nil, false)
	var cursor struct {
		Seq int64 `json:"seq"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&cursor) != nil || cursor.Seq != high {
		t.Fatalf("cursor: status=%d seq=%d want=%d", response.StatusCode, cursor.Seq, high)
	}
	response.Body.Close()
	bad := http.Header{}
	bad.Set("Origin", "http://evil.invalid")
	bad.Set("Cookie", f.cookie.String())
	_, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(f.http.URL, "http")+"/api/events", &websocket.DialOptions{HTTPHeader: bad})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %v, %v", err, resp)
	}
	c := f.dial(t, 0, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	var prev int64
	for {
		frame := readFrame(t, c)
		var typ string
		json.Unmarshal(frame["type"], &typ)
		if typ == "ready" {
			var seq int64
			json.Unmarshal(frame["seq"], &seq)
			if seq != high || prev == 0 {
				t.Fatalf("ready=%d last=%d high=%d", seq, prev, high)
			}
			break
		}
		var event store.Event
		json.Unmarshal(frame["event"], &event)
		if event.Seq <= prev {
			t.Fatalf("replay order %d after %d", event.Seq, prev)
		}
		prev = event.Seq
	}
	for i := 0; i < 3; i++ {
		resp := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": fmt.Sprintf("m%d", i)}, true)
		if resp.StatusCode != 201 {
			t.Fatalf("post %d: %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for i := 0; i < 3; i++ {
		frame := readFrame(t, c)
		var event store.Event
		json.Unmarshal(frame["event"], &event)
		if event.Seq <= prev || event.Kind != "message_created" {
			t.Fatalf("live event %+v after %d", event, prev)
		}
		prev = event.Seq
	}
	c2 := f.dial(t, high, f.http.URL)
	defer c2.Close(websocket.StatusNormalClosure, "")
	frame := readFrame(t, c2)
	var event store.Event
	json.Unmarshal(frame["event"], &event)
	if event.Seq <= high {
		t.Fatalf("cursor replay %+v", event)
	}
}

func TestSnapshotRequiredAfterRetentionGap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, max, _ := f.db.EventBounds(context.Background())
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(`DELETE FROM event WHERE seq < ?`, max); return err }); err != nil {
		t.Fatal(err)
	}
	c := f.dial(t, 0, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	frame := readFrame(t, c)
	var typ string
	json.Unmarshal(frame["type"], &typ)
	if typ != "snapshot_required" {
		t.Fatalf("frame=%s", frame)
	}
}

// A page cannot tell a dead event socket from a quiet one, so the stream says how often it will speak (the ready frame) and then does so, whether or
// not anything happened; the ready frame also names the build serving the page, which a page that outlives a deploy compares after reconnecting.
func TestEventStreamHeartbeatsAndNamesItsBuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.HeartbeatInterval = 1200 * time.Millisecond
	f.srv.Version = "aimebu vtest-1"
	_, high, _ := f.db.EventBounds(context.Background())
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	ready := readFrame(t, c)
	var typ, version string
	var beat int
	json.Unmarshal(ready["type"], &typ)
	json.Unmarshal(ready["version"], &version)
	json.Unmarshal(ready["heartbeat"], &beat)
	if typ != "ready" || version != "aimebu vtest-1" || beat != 2 { // 1.2 s is advertised rounded up, so the page never expects it sooner than it comes
		t.Fatalf("ready frame: type=%q version=%q heartbeat=%d (%s)", typ, version, beat, ready)
	}
	// Nothing happens in the room; the next frames are heartbeats, steadily.
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, raw, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		frame := map[string]json.RawMessage{}
		json.Unmarshal(raw, &frame)
		json.Unmarshal(frame["type"], &typ)
		if typ != "heartbeat" {
			t.Fatalf("frame %d on a quiet stream: %s", i, frame)
		}
	}
}

func TestSlowClientQueueIsBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.QueueSize = 1
	_, high, _ := f.db.EventBounds(context.Background())
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readFrame(t, c) // ready
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for i := 0; i < 100; i++ {
			if _, err := tx.AppendEvent(store.Event{Kind: "burst", ProjectID: f.project.ID, PayloadJSON: `{"value":"` + strings.Repeat("x", 4096) + `"}`}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("close=%v", err)
			}
			return
		}
	}
}

func TestEventPollErrorClosesSocketAndRetriesBusy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, high, _ := f.db.EventBounds(context.Background())
	var fail atomic.Bool
	var attempts atomic.Int32
	f.srv.EventBatchQuery = func(ctx context.Context, user string, after, through int64, limit int) ([]store.Event, error) {
		if fail.Load() {
			attempts.Add(1)
			return nil, errors.New("SQLITE_BUSY injected")
		}
		return f.db.EventsBatchAfter(ctx, user, after, through, limit)
	}
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	frame := readFrame(t, c)
	if string(frame["type"]) != `"ready"` {
		t.Fatalf("initial frame=%v", frame)
	}
	fail.Store(true)
	resp := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "trigger poll"}, true)
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError || !strings.Contains(err.Error(), "event poll failed; reconnect") {
		t.Fatalf("poll failure close=%v", err)
	}
	if attempts.Load() != 4 {
		t.Fatalf("BUSY attempts=%d", attempts.Load())
	}
}

func TestEventPollTransientBusyRecovers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	_, high, _ := f.db.EventBounds(context.Background())
	var busy atomic.Bool
	var attempts atomic.Int32
	f.srv.EventBatchQuery = func(ctx context.Context, user string, after, through int64, limit int) ([]store.Event, error) {
		if busy.Load() && attempts.Add(1) <= 2 {
			return nil, errors.New("SQLITE_BUSY injected")
		}
		return f.db.EventsBatchAfter(ctx, user, after, through, limit)
	}
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readFrame(t, c)
	busy.Store(true)
	resp := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "after busy"}, true)
	resp.Body.Close()
	frame := readFrame(t, c)
	var event store.Event
	json.Unmarshal(frame["event"], &event)
	if event.Kind != "message_created" || attempts.Load() < 3 {
		t.Fatalf("recovered event=%+v attempts=%d", event, attempts.Load())
	}
}

func TestEventSocketClosesAfterLogout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.SessionCheckInterval = 10 * time.Millisecond
	_, high, _ := f.db.EventBounds(context.Background())
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readFrame(t, c)
	resp := f.request("POST", "/api/logout", nil, true)
	if resp.StatusCode != 204 {
		t.Fatalf("logout=%d", resp.StatusCode)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || !strings.Contains(err.Error(), "session expired or revoked") {
		t.Fatalf("revoked socket=%v", err)
	}
}

func TestEventSocketClosesAfterExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.SessionCheckInterval = 10 * time.Millisecond
	var expired atomic.Bool
	f.srv.Auth.Now = func() time.Time {
		if expired.Load() {
			return time.Now().Add(8 * 24 * time.Hour)
		}
		return time.Now()
	}
	_, high, _ := f.db.EventBounds(context.Background())
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readFrame(t, c)
	expired.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("expired socket=%v", err)
	}
}

func TestEventSocketSessionCheckDBErrorClosesForReconnect(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.SessionCheckInterval = 10 * time.Millisecond
	var fail atomic.Bool
	var attempts atomic.Int32
	f.srv.SessionCheckQuery = func(ctx context.Context, token string) error {
		if fail.Load() {
			attempts.Add(1)
			return errors.New("SQLITE_BUSY injected")
		}
		_, err := f.srv.Auth.CheckSession(ctx, token)
		return err
	}
	_, high, _ := f.db.EventBounds(context.Background())
	c := f.dial(t, high, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readFrame(t, c)
	fail.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError || !strings.Contains(err.Error(), "session check failed; reconnect") || attempts.Load() != 4 {
		t.Fatalf("database check failure: close=%v attempts=%d", err, attempts.Load())
	}
}

func TestEventReplayUsesBoundedBatches(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for i := 0; i < 501; i++ {
			if _, err := tx.AppendEvent(store.Event{Kind: "bulk", ProjectID: f.project.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f.srv.EventBatchQuery = func(ctx context.Context, user string, after, through int64, limit int) ([]store.Event, error) {
		if limit != eventBatchSize {
			t.Errorf("batch limit=%d", limit)
		}
		calls.Add(1)
		return f.db.EventsBatchAfter(ctx, user, after, through, limit)
	}
	c := f.dial(t, 0, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	bulk := 0
	for {
		frame := readFrame(t, c)
		var typ string
		json.Unmarshal(frame["type"], &typ)
		if typ == "ready" {
			break
		}
		var event store.Event
		json.Unmarshal(frame["event"], &event)
		if event.Kind == "bulk" {
			bulk++
		}
	}
	if bulk != 501 || calls.Load() < 2 {
		t.Fatalf("replay bulk=%d queries=%d", bulk, calls.Load())
	}
}

func TestEventReplayStopsAfterSessionRevokedMidReplay(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.SessionCheckInterval = 10 * time.Millisecond
	if err := f.db.Tx(context.Background(), func(tx *store.Tx) error {
		for i := 0; i < 501; i++ {
			if _, err := tx.AppendEvent(store.Event{Kind: "bulk", ProjectID: f.project.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secondBatch := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	f.srv.EventBatchQuery = func(ctx context.Context, user string, after, through int64, limit int) ([]store.Event, error) {
		if calls.Add(1) == 2 {
			close(secondBatch)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return f.db.EventsBatchAfter(ctx, user, after, through, limit)
	}
	c := f.dial(t, 0, f.http.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	select {
	case <-secondBatch:
	case <-time.After(3 * time.Second):
		t.Fatal("replay did not reach second batch")
	}
	if err := f.srv.Auth.RevokeSession(context.Background(), f.cookie.Value); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("replay close=%v", err)
			}
			return
		}
		if strings.Contains(string(raw), `"type":"ready"`) {
			t.Fatal("revoked replay reached ready")
		}
	}
}
