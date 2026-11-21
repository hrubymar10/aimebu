package server

import (
	"context"
	"net/http"
	"net/url"
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
	machine := &machineConn{id: o.machineID, supportsAgentLogs: supports, sendQueue: make(chan protocol.Frame, 8), done: make(chan struct{}), pathPending: map[string]chan protocol.Frame{}}
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
