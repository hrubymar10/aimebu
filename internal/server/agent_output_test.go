package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// outputFixture is a project with a worker in its task room, running on a machine that keeps agent output and answers reads from a
// script, recording what it was asked.
type outputFixture struct {
	*fixture
	agent   store.Agent
	machine *machineConn
	mu      sync.Mutex
	asked   []protocol.Frame
	answer  func(protocol.Frame) protocol.Frame
	// oldConversation makes the next connect a supervisor that keeps agent output but predates the conversation log.
	oldConversation bool
}

func newOutputFixture(t *testing.T) *outputFixture {
	t.Helper()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_out','worker','Worker','','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_out',?,?,?)`, f.project.ID, f.machineID, f.project.Folder)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "worker", ProjectID: f.project.ID, RoleID: "role_out", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: f.machineID, CheckoutID: "chk_out"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	o := &outputFixture{fixture: f, agent: agent}
	o.answer = func(request protocol.Frame) protocol.Frame {
		page, _ := json.Marshal(agentlog.Page{Text: "2026-10-22T12:00:00Z line one\n2026-10-22T12:00:01Z line two\n", Bytes: 2048, Files: 2, Truncated: true, UpdatedAt: time.Date(2026, 10, 22, 12, 0, 1, 0, time.UTC)})
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: request.ReqID, AgentID: request.AgentID, Details: page}
	}
	o.connect(true)
	return o
}

// connect puts the machine online, with or without support for agent output, answering reads in the background.
func (o *outputFixture) connect(supports bool) {
	machine := &machineConn{id: o.machineID, supportsAgentLogs: supports, supportsAgentConversation: supports && !o.oldConversation, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), pathPending: map[string]chan protocol.Frame{}}
	o.machine = machine
	o.srv.machineMu.Lock()
	o.srv.machines[o.machineID] = machine
	o.srv.machineMu.Unlock()
	go func() {
		for {
			select {
			case request := <-machine.sendQueue:
				o.mu.Lock()
				o.asked = append(o.asked, request)
				answer := o.answer
				o.mu.Unlock()
				machine.harnessMu.Lock()
				waiter := machine.pathPending[request.ReqID]
				machine.harnessMu.Unlock()
				if waiter != nil {
					waiter <- answer(request)
				}
			case <-machine.done:
				return
			}
		}
	}()
	o.t.Cleanup(func() { machine.stop2() })
}

// stop2 ends the answering goroutine without touching a websocket the fixture machine does not have.
func (c *machineConn) stop2() { c.stopOnce.Do(func() { close(c.done) }) }

func (o *outputFixture) path(agent string) string {
	return "/api/rooms/" + o.room.ID + "/agents/" + url.PathEscape(agent) + "/output"
}

func (o *outputFixture) get(t *testing.T, path string) (int, map[string]any, http.Header) {
	t.Helper()
	resp := o.request(http.MethodGet, path, nil, false)
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body, resp.Header
}

func (o *outputFixture) requests() []protocol.Frame {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]protocol.Frame(nil), o.asked...)
}

func TestAgentOutputIsReadFromTheMachineForTheOwner(t *testing.T) {
	o := newOutputFixture(t)
	status, body, header := o.get(t, o.path(o.agent.ID)+"?lines=50")
	if status != 200 || body["text"] != "2026-10-22T12:00:00Z line one\n2026-10-22T12:00:01Z line two\n" || body["truncated"] != true || body["bytes"] != float64(2048) || body["files"] != float64(2) || body["machine_id"] != o.machineID {
		t.Fatalf("read: %d %+v", status, body)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Fatalf("output was cacheable: %q", header.Get("Cache-Control"))
	}
	asked := o.requests()
	if len(asked) != 1 || asked[0].Type != protocol.AgentLogRead || asked[0].AgentID != o.agent.ID || asked[0].LogLines != 50 || asked[0].LogMaxBytes != agentlog.MaxTailBytes || asked[0].CheckoutPath != "" {
		t.Fatalf("the machine was asked %+v", asked)
	}
	// Without lines the machine is asked for its default.
	if status, _, _ := o.get(t, o.path(o.agent.ID)); status != 200 || o.requests()[1].LogLines != 0 {
		t.Fatalf("default read: %d %+v", status, o.requests())
	}
}

// Only a project's owner or an admin may read it: it can hold whatever the agent's tools printed.
func TestAgentOutputIsForOwnersAndAdminsOnly(t *testing.T) {
	o := newOutputFixture(t)
	cases := []struct {
		login, role string
		want        int
	}{{"projowner", "owner", 200}, {"operator", "operator", 403}, {"viewer", "viewer", 403}, {"stranger", "", 403}}
	for _, tc := range cases {
		cookie, csrf := registerSecondUser(t, o.fixture, tc.login)
		if tc.role != "" {
			user, err := o.db.UserByLogin(t.Context(), tc.login)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, o.project.ID, user.ID, tc.role); err != nil {
				t.Fatal(err)
			}
		}
		before := len(o.requests())
		resp := o.requestAs(t, cookie, csrf, http.MethodGet, o.path(o.agent.ID), nil)
		resp.Body.Close()
		if resp.StatusCode != tc.want && !(tc.want == 403 && resp.StatusCode == 404) {
			t.Errorf("%s (%s): %d, want %d", tc.login, tc.role, resp.StatusCode, tc.want)
		}
		if tc.want != 200 && len(o.requests()) != before {
			t.Errorf("%s: the machine was asked for output it must not give", tc.login)
		}
	}
	// No session at all.
	resp, err := http.Get(o.http.URL + o.path(o.agent.ID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("without a session: %d", resp.StatusCode)
	}
}

func TestAgentOutputRefusesWhatIsNotThisRoomsAgent(t *testing.T) {
	o := newOutputFixture(t)
	for name, agent := range map[string]string{"unknown": "agt_nope", "traversal": "../../etc/passwd", "encoded traversal": "%2e%2e%2f%2e%2e%2fetc%2fpasswd", "empty-ish": "."} {
		if status, _, _ := o.get(t, "/api/rooms/"+o.room.ID+"/agents/"+agent+"/output"); status != 404 && status != 400 {
			t.Errorf("%s: %d", name, status)
		}
	}
	// An agent of another project is not found through this room.
	resp := o.request("POST", "/api/projects", map[string]string{"name": "other", "folder": t.TempDir()}, true)
	var other store.Project
	json.NewDecoder(resp.Body).Decode(&other)
	resp.Body.Close()
	if _, err := o.exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_other',?,?,?)`, other.ID, o.machineID, other.Folder); err != nil {
		t.Fatal(err)
	}
	stranger, err := o.db.CreateAgent(t.Context(), store.Agent{Slug: "elsewhere", ProjectID: other.ID, RoleID: "role_out", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: o.machineID, CheckoutID: "chk_other"})
	if err != nil {
		t.Fatal(err)
	}
	if status, body, _ := o.get(t, o.path(stranger.ID)); status != 404 || body["error"] != "not_found" {
		t.Fatalf("an agent of another project: %d %+v", status, body)
	}
	if status, body, _ := o.get(t, o.path("agt_nope")); status != 404 || body["error"] != "not_found" {
		t.Fatalf("an unknown agent: %d %+v", status, body)
	}
	if len(o.requests()) != 0 {
		t.Fatalf("the machine was asked %+v", o.requests())
	}
}

// An agent that has left its room (or was stopped there) still has output on its machine, and the project's owner may read it.
func TestAgentOutputOfADepartedAgentIsStillReadable(t *testing.T) {
	o := newOutputFixture(t)
	if _, err := o.exec(`UPDATE room_member SET left_at='2026-10-22T00:00:00.000000000Z' WHERE member_kind='agent' AND member_id=?`, o.agent.ID); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 200 || body["text"] == "" {
		t.Fatalf("a departed agent: %d %+v", status, body)
	}
}

func TestAgentOutputSaysWhyThereIsNone(t *testing.T) {
	o := newOutputFixture(t)
	if status, body, _ := o.get(t, o.path(o.agent.ID)+"?lines=0"); status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("lines=0: %d %+v", status, body)
	}
	if status, _, _ := o.get(t, o.path(o.agent.ID)+"?lines=many"); status != 400 {
		t.Fatalf("lines=many: %d", status)
	}
	// The machine has captured nothing for this agent.
	o.mu.Lock()
	o.answer = func(r protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: r.ReqID, Code: "no_log", Message: "none"}
	}
	o.mu.Unlock()
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 404 || body["error"] != "no_output" {
		t.Fatalf("no output: %d %+v", status, body)
	}
	// It failed to read it.
	o.mu.Lock()
	o.answer = func(r protocol.Frame) protocol.Frame {
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: r.ReqID, Code: "log_failed", Message: "the output could not be read"}
	}
	o.mu.Unlock()
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 502 || body["error"] != "output_unavailable" {
		t.Fatalf("a failed read: %d %+v", status, body)
	}
	// An older supervisor is not sent a frame it does not know.
	o.connect(false)
	before := len(o.requests())
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 409 || body["error"] != "supervisor_outdated" || len(o.requests()) != before {
		t.Fatalf("an old supervisor: %d %+v (asked %d)", status, body, len(o.requests())-before)
	}
	// An offline machine.
	o.srv.machineMu.Lock()
	delete(o.srv.machines, o.machineID)
	o.srv.machineMu.Unlock()
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 409 || body["error"] != "machine_offline" {
		t.Fatalf("an offline machine: %d %+v", status, body)
	}
}

func TestAgentOutputReadsAreRateLimitedPerPerson(t *testing.T) {
	o := newOutputFixture(t)
	for i := 0; i < outputReadsPerMinute; i++ {
		if status, _, _ := o.get(t, o.path(o.agent.ID)); status != 200 {
			t.Fatalf("read %d: %d", i, status)
		}
	}
	status, body, header := o.get(t, o.path(o.agent.ID))
	if status != 429 || body["error"] != "rate_limited" || header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %+v", status, body)
	}
	// It is per person: someone else is not held back by the owner's reads (they are refused for another reason, not this one).
	cookie, csrf := registerSecondUser(t, o.fixture, "someoneelse")
	resp := o.requestAs(t, cookie, csrf, http.MethodGet, o.path(o.agent.ID), nil)
	resp.Body.Close()
	if resp.StatusCode == 429 {
		t.Fatal("the limit is shared between people")
	}
}

// stream=conversation reads the agent's conversation log: the machine is asked for that stream by name, and the answer says which
// stream it is. Without the parameter the read is the stderr it always was, in a frame an older supervisor has always been sent.
func TestAgentConversationIsReadByNamingTheStream(t *testing.T) {
	o := newOutputFixture(t)
	status, body, header := o.get(t, o.path(o.agent.ID)+"?stream=conversation&lines=50")
	if status != 200 || body["stream"] != "conversation" || !strings.Contains(body["text"].(string), " withheld ") || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("conversation read: %d %+v", status, body)
	}
	if asked := o.requests(); len(asked) != 1 || asked[0].Type != protocol.AgentLogRead || asked[0].LogStream != "conversation" || asked[0].LogLines != 50 || asked[0].LogMaxBytes != agentlog.MaxTailBytes {
		t.Fatalf("the machine was asked %+v", asked)
	}
	for _, query := range []string{"", "?stream=stderr"} {
		before := len(o.requests())
		if status, body, _ := o.get(t, o.path(o.agent.ID)+query); status != 200 || body["stream"] != "stderr" || o.requests()[before].LogStream != "" {
			t.Fatalf("%q: %d %+v asked %+v", query, status, body, o.requests()[before])
		}
	}
	before := len(o.requests())
	if status, body, _ := o.get(t, o.path(o.agent.ID)+"?stream=stdout"); status != 400 || body["error"] != "invalid_request" || len(o.requests()) != before {
		t.Fatalf("an unknown stream: %d %+v", status, body)
	}
}

// The conversation holds everything the model was asked, said and ran: like the output, it is for a project's owner or an admin only.
func TestAgentConversationIsForOwnersAndAdminsOnly(t *testing.T) {
	o := newOutputFixture(t)
	for _, tc := range []struct {
		login, role string
		want        int
	}{{"convowner", "owner", 200}, {"convoperator", "operator", 403}, {"convviewer", "viewer", 403}, {"convstranger", "", 403}} {
		cookie, csrf := registerSecondUser(t, o.fixture, tc.login)
		if tc.role != "" {
			user, err := o.db.UserByLogin(t.Context(), tc.login)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, o.project.ID, user.ID, tc.role); err != nil {
				t.Fatal(err)
			}
		}
		before := len(o.requests())
		resp := o.requestAs(t, cookie, csrf, http.MethodGet, o.path(o.agent.ID)+"?stream=conversation", nil)
		resp.Body.Close()
		if resp.StatusCode != tc.want && !(tc.want == 403 && resp.StatusCode == 404) {
			t.Errorf("%s (%s): %d, want %d", tc.login, tc.role, resp.StatusCode, tc.want)
		}
		if tc.want != 200 && len(o.requests()) != before {
			t.Errorf("%s: the machine was asked for a conversation it must not give", tc.login)
		}
	}
}

// A supervisor that keeps agent output but predates the conversation log would ignore the stream and answer with stderr: it is not
// asked, and the person is told to update it. Its stderr is still readable.
func TestAgentConversationFromAnOlderSupervisor(t *testing.T) {
	o := newOutputFixture(t)
	o.oldConversation = true
	o.connect(true)
	before := len(o.requests())
	if status, body, _ := o.get(t, o.path(o.agent.ID)+"?stream=conversation"); status != 409 || body["error"] != "supervisor_outdated" || len(o.requests()) != before {
		t.Fatalf("an old supervisor: %d %+v (asked %d)", status, body, len(o.requests())-before)
	}
	if status, body, _ := o.get(t, o.path(o.agent.ID)); status != 200 || body["stream"] != "stderr" {
		t.Fatalf("its stderr: %d %+v", status, body)
	}
}

// Both streams count against the same limit of a person's reads.
func TestAgentConversationReadsShareTheRateLimit(t *testing.T) {
	o := newOutputFixture(t)
	for i := 0; i < outputReadsPerMinute; i++ {
		stream := "stderr"
		if i%2 == 0 {
			stream = "conversation"
		}
		if status, _, _ := o.get(t, o.path(o.agent.ID)+"?stream="+stream); status != 200 {
			t.Fatalf("read %d: %d", i, status)
		}
	}
	if status, body, header := o.get(t, o.path(o.agent.ID)+"?stream=conversation"); status != 429 || body["error"] != "rate_limited" || header.Get("Retry-After") == "" {
		t.Fatalf("past the limit: %d %+v", status, body)
	}
}

// conversationTurn records a turn of the fixture's agent in a room, with the notes it was given (messages of other rooms).
func (o *outputFixture) conversationTurn(t *testing.T, id, roomID string, noteRooms ...string) {
	t.Helper()
	owner, err := o.db.UserByLogin(t.Context(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.exec(`INSERT OR IGNORE INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_conv',?,0,'fresh_with_history','2026-01-01T00:00:00.000Z')`, o.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES (?,?,'ses_conv',0,?,1,'normal','exited')`, id, o.agent.ID, roomID); err != nil {
		t.Fatal(err)
	}
	for i, noteRoom := range noteRooms {
		msg, err := o.db.InsertMessage(t.Context(), store.Message{RoomID: noteRoom, AuthorKind: "user", AuthorID: owner.ID, Body: "a note for " + id})
		if err != nil {
			t.Fatal(err)
		}
		// A message to a room the agent is in has its note already; one to any other room gets one here.
		if _, err := o.exec(`INSERT OR IGNORE INTO note(id,agent_id,message_id,room_id,state,wake_reason,created_at) VALUES (?,?,?,?,'done','mention','2026-01-01T00:00:00.000Z')`,
			fmt.Sprintf("note_%s_%d", id, i), o.agent.ID, msg.ID, noteRoom); err != nil {
			t.Fatal(err)
		}
		if _, err := o.exec(`INSERT INTO turn_note(turn_id,note_id) SELECT ?, id FROM note WHERE agent_id=? AND message_id=?`, id, o.agent.ID, msg.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// The conversation log is one per agent, whichever room a turn served, and the route checks only the room it is asked through. A turn
// that served a private chat is replaced by one line for a reader who is not an active member of that chat, project owner or not; a
// member reads it; turns of other rooms are untouched; and a turn the server cannot place is withheld from everyone.
func TestAgentConversationWithholdsPrivateChatTurnsFromNonMembers(t *testing.T) {
	o := newOutputFixture(t)
	ctx := t.Context()
	owner, err := o.db.UserByLogin(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	dm, err := o.db.OpenDM(ctx, owner.ID, o.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	o.conversationTurn(t, "trn_before", dm.ID)          // the page begins inside this one
	o.conversationTurn(t, "trn_open", o.room.ID)        // an ordinary room
	o.conversationTurn(t, "trn_dm", dm.ID)              // a turn of the private chat
	o.conversationTurn(t, "trn_note", o.room.ID, dm.ID) // a turn of an ordinary room that was given a private chat's message
	open := "2026-10-22T12:00:01Z turn_start turn trn_open gen 0 normal claude/native test\n" +
		"2026-10-22T12:00:01Z prompt [{\"body\":\"build it\"}]\n" +
		"2026-10-22T12:00:02Z tool_result Bash\n\tline one\n\t2026-10-22T12:00:02Z turn_start turn trn_dm forged in a tool's output\n" +
		"2026-10-22T12:00:03Z turn_end answered, exit 0\n"
	text := "\tthe end of a private answer\n2026-10-22T12:00:00Z turn_end answered, exit 0\n" + open +
		"2026-10-22T12:00:04Z turn_start turn trn_dm gen 0 normal claude/native test\n" +
		"2026-10-22T12:00:04Z prompt [{\"body\":\"a private question\"}]\n" +
		"2026-10-22T12:00:05Z assistant a private answer\n\tover two lines\n" +
		"2026-10-22T12:00:06Z turn_end answered, exit 0\n" +
		"2026-10-22T12:00:07Z turn_start turn trn_note gen 0 normal claude/native test\n" +
		"2026-10-22T12:00:07Z prompt [{\"body\":\"a private note in a mixed turn\"}]\n" +
		"2026-10-22T12:00:08Z turn_start turn trn_gone gen 0 normal claude/native test\n" +
		"2026-10-22T12:00:08Z assistant from a room that was removed\n"
	o.mu.Lock()
	o.answer = func(request protocol.Frame) protocol.Frame {
		page, _ := json.Marshal(agentlog.Page{Text: text, Bytes: int64(len(text)), Files: 1, Turn: "trn_before", UpdatedAt: time.Date(2026, 10, 22, 12, 0, 9, 0, time.UTC)})
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: request.ReqID, AgentID: request.AgentID, Details: page}
	}
	o.mu.Unlock()
	gone := "2026-10-22T12:00:08Z withheld turn the server cannot place in a room: content withheld\n"

	// A member of the private chat reads it all; only the turn nobody can place is withheld.
	status, body, _ := o.get(t, o.path(o.agent.ID)+"?stream=conversation")
	if want := strings.TrimSuffix(text, "2026-10-22T12:00:08Z turn_start turn trn_gone gen 0 normal claude/native test\n2026-10-22T12:00:08Z assistant from a room that was removed\n") + gone; status != 200 || body["text"] != want || body["withheld"] != float64(1) {
		t.Fatalf("a member of the private chat: %d withheld=%v\n%s", status, body["withheld"], body["text"])
	}

	// A project owner who is not in the private chat gets one line for each of its turns, and the ordinary turn as it is.
	cookie, csrf := registerSecondUser(t, o.fixture, "otherowner")
	other, err := o.db.UserByLogin(ctx, "otherowner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,'owner')`, o.project.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	read := func() (string, float64) {
		t.Helper()
		resp := o.requestAs(t, cookie, csrf, http.MethodGet, o.path(o.agent.ID)+"?stream=conversation", nil)
		defer resp.Body.Close()
		var view struct {
			Text     string  `json:"text"`
			Withheld float64 `json:"withheld"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&view); err != nil || resp.StatusCode != 200 {
			t.Fatalf("read as the other owner: %d %v", resp.StatusCode, err)
		}
		return view.Text, view.Withheld
	}
	private := "private chat turn: content withheld\n"
	want := "2026-10-22T12:00:09Z withheld " + private + open + "2026-10-22T12:00:04Z withheld " + private + "2026-10-22T12:00:07Z withheld " + private + gone
	got, withheld := read()
	if got != want || withheld != 4 {
		t.Fatalf("an owner outside the private chat (withheld=%v):\n%s\nwant:\n%s", withheld, got, want)
	}
	for _, secret := range []string{"private answer", "private question", "private note", "over two lines"} {
		if strings.Contains(got, secret) {
			t.Fatalf("%q reached a person who is not in the private chat", secret)
		}
	}

	// Once that person is an active member of the chat, it reads like any other turn; after leaving, it is withheld again.
	if _, err := o.exec(`INSERT INTO room_member(room_id,member_kind,member_id,joined_at) VALUES (?,'user',?,'2026-01-01T00:00:00.000Z')`, dm.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if got, withheld := read(); !strings.Contains(got, "a private answer\n\tover two lines\n") || withheld != 1 {
		t.Fatalf("a member (withheld=%v):\n%s", withheld, got)
	}
	if _, err := o.exec(`UPDATE room_member SET left_at='2026-01-02T00:00:00.000Z' WHERE room_id=? AND member_id=?`, dm.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if got, withheld := read(); got != want || withheld != 4 {
		t.Fatalf("a person who left the private chat (withheld=%v):\n%s", withheld, got)
	}

	// A page whose first turn the machine did not name is withheld up to its first turn start, for a member too.
	o.mu.Lock()
	o.answer = func(request protocol.Frame) protocol.Frame {
		page, _ := json.Marshal(agentlog.Page{Text: "... earlier part cut\n\tthe end of something\n" + open, Files: 1, UpdatedAt: time.Date(2026, 10, 22, 12, 0, 9, 0, time.UTC)})
		return protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: request.ReqID, AgentID: request.AgentID, Details: page}
	}
	o.mu.Unlock()
	if _, body, _ := o.get(t, o.path(o.agent.ID)+"?stream=conversation"); body["text"] != "2026-10-22T12:00:09Z withheld turn the server cannot place in a room: content withheld\n"+open {
		t.Fatalf("a page with no named first turn:\n%s", body["text"])
	}
	// The stderr stream is not a conversation: it is returned as the machine sent it.
	if _, body, _ := o.get(t, o.path(o.agent.ID)); body["text"] != "... earlier part cut\n\tthe end of something\n"+open || body["withheld"] != nil {
		t.Fatalf("stderr was filtered:\n%s", body["text"])
	}
}

// A turn id in the log that is another agent's turn places nothing: it is withheld like one the server does not know.
func TestAgentConversationDoesNotPlaceAnotherAgentsTurn(t *testing.T) {
	o := newOutputFixture(t)
	o.conversationTurn(t, "trn_mine", o.room.ID)
	for turn, want := range map[string]bool{"trn_mine": true, "trn_missing": false} {
		if known, private, err := o.db.TurnWithheldFrom(t.Context(), turn, o.agent.ID, "usr_nobody"); err != nil || known != want || private {
			t.Fatalf("%s: known=%t private=%t %v", turn, known, private, err)
		}
	}
	if known, _, err := o.db.TurnWithheldFrom(t.Context(), "trn_mine", "agt_other", "usr_nobody"); err != nil || known {
		t.Fatalf("another agent's turn was placed: %t %v", known, err)
	}
}
