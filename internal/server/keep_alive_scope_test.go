package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// While a cache keep-alive ping of an agent runs, every call made with the agent's key is refused with 403 keep_alive_scope, before any handler: a ping
// that calls a tool anyway posts nothing and changes nothing. A call that follows the lift works again; another agent's key is never affected.
func TestKeepAliveScopeRefusesTheToolbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	lead, key := f.createToolAgent(t, "role_lead_default")
	_, otherKey := f.createToolAgentNamed(t, "role_worker_default", "other")
	var homeID string
	rooms, err := f.db.RoomsForProject(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range rooms {
		if item.Kind == "home" {
			homeID = item.ID
		}
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error { return tx.AddMember(homeID, "agent", lead.ID) }); err != nil {
		t.Fatal(err)
	}
	roomCount := func() int {
		all, err := f.db.RoomsForProject(t.Context(), f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	read := func(response *http.Response) string {
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return string(data)
	}
	runsHere := func(string) bool { return true }
	before := roomCount()
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "r1", AgentID: lead.ID, ScopeOn: true}, runsHere)
	if ack := <-machine.sendQueue; ack.Type != protocol.KeepAliveScopeAckFrame || ack.ReqID != "r1" {
		t.Fatalf("the supervisor waits for this answer: %+v", ack)
	}
	for name, response := range map[string]*http.Response{
		"read":  f.toolRequest(t, "/api/agent/rooms", key),
		"write": f.toolPost(t, "/api/agent/rooms", key, "scoped", map[string]string{"title": "A ping must not make this"}),
		"mcp":   f.toolPost(t, "/api/agent/mcp", key, "scoped-mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}),
	} {
		status := response.StatusCode
		if body := read(response); status != 403 || !strings.Contains(body, "keep_alive_scope") {
			t.Fatalf("%s during a ping: %d %s", name, status, body)
		}
	}
	if roomCount() != before {
		t.Fatal("a refused call changed something")
	}
	if response := f.toolRequest(t, "/api/agent/rooms", otherKey); response.StatusCode != 200 {
		t.Fatalf("another agent's key is not scoped: %d", response.StatusCode)
	}
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "r2", AgentID: lead.ID, ScopeOn: false}, runsHere)
	if ack := <-machine.sendQueue; ack.ReqID != "r2" {
		t.Fatalf("the lift is answered too: %+v", ack)
	}
	if response := f.toolPost(t, "/api/agent/rooms", key, "after", map[string]string{"title": "Real work"}); response.StatusCode != 200 && response.StatusCode != 201 {
		t.Fatalf("after the lift the toolbox works: %d", response.StatusCode)
	}
	if roomCount() != before+1 {
		t.Fatal("the real call after the lift did its work")
	}
}

// Only the machine that runs an agent may scope it (and an answer is sent only then), a lift by another machine does nothing, and a scope that is
// never lifted lapses by itself.
func TestKeepAliveScopeOnlyByTheAgentsMachineAndLapses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "x", AgentID: "agt_not_here", ScopeOn: true}, func(string) bool { return false })
	if len(machine.sendQueue) != 0 || f.srv.keepAliveScoped("agt_not_here") {
		t.Fatal("a machine scoped an agent it does not run")
	}
	f.srv.scopeKeepAlive("agt_a", "mch_one", "", true)
	f.srv.scopeKeepAlive("agt_a", "mch_two", "", false)
	if !f.srv.keepAliveScoped("agt_a") {
		t.Fatal("another machine lifted the scope")
	}
	f.srv.kaScopeMu.Lock()
	f.srv.kaScope["agt_a"] = keepAliveScopeLease{machineID: "mch_one", until: time.Now().Add(-time.Second)}
	f.srv.kaScopeMu.Unlock()
	if f.srv.keepAliveScoped("agt_a") {
		t.Fatal("a scope nobody lifted must lapse")
	}
}

// The supervisor of an unconfirmed ping renews its scope with the same "on" frame, and that extends the lease.
func TestKeepAliveScopeRenewalExtendsTheLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.scopeKeepAlive("agt_a", "mch_one", "", true)
	f.srv.kaScopeMu.Lock()
	f.srv.kaScope["agt_a"] = keepAliveScopeLease{machineID: "mch_one", until: time.Now().Add(time.Second)}
	f.srv.kaScopeMu.Unlock()
	f.srv.scopeKeepAlive("agt_a", "mch_one", "", true)
	f.srv.kaScopeMu.Lock()
	until := f.srv.kaScope["agt_a"].until
	f.srv.kaScopeMu.Unlock()
	if time.Until(until) < keepAliveScopeTTL-5*time.Second {
		t.Fatalf("a renewal must extend the lease to a whole TTL: %v left", time.Until(until))
	}
}

// A lift that names another ping than the one scoped now does not clear the scope: an owed lift of an old ping that arrives after a new ping's scope
// began is stale. A lift for the current ping clears it, and one without a ping named (an older supervisor) always does.
func TestKeepAliveScopeLateLiftOfAnOldPingDoesNotClearTheNewScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.srv.scopeKeepAlive("agt_a", "mch_one", "ka_new", true)
	f.srv.scopeKeepAlive("agt_a", "mch_one", "ka_old", false)
	if !f.srv.keepAliveScoped("agt_a") {
		t.Fatal("a late lift of an older ping cleared the live scope")
	}
	f.srv.scopeKeepAlive("agt_a", "mch_one", "ka_new", false)
	if f.srv.keepAliveScoped("agt_a") {
		t.Fatal("the lift of the current ping must clear the scope")
	}
	f.srv.scopeKeepAlive("agt_a", "mch_one", "ka_new", true)
	f.srv.scopeKeepAlive("agt_a", "mch_one", "", false)
	if f.srv.keepAliveScoped("agt_a") {
		t.Fatal("a lift that names no ping applies")
	}
}

// Through the real toolbox gate: while a ping is held in quarantine its supervisor renews the scope, so the toolbox stays refused (403) past the point
// where the first lease would have lapsed; a late lift of an older ping does not open it; the lift of the quarantined ping does.
func TestKeepAliveScopeStays403ThroughAQuarantineByRenewal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	machine := connectHost(t, f, true)
	lead, key := f.createToolAgent(t, "role_lead_default")
	runsHere := func(string) bool { return true }
	status := func(string) int {
		response := f.toolRequest(t, "/api/agent/rooms", key)
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "a", AgentID: lead.ID, ScopeOn: true, ScopeGen: "ka_stuck"}, runsHere)
	<-machine.sendQueue
	// The first lease is about to lapse (the ping ran, then its quarantine began): the supervisor renews it.
	f.srv.kaScopeMu.Lock()
	lease := f.srv.kaScope[lead.ID]
	lease.until = time.Now().Add(250 * time.Millisecond)
	f.srv.kaScope[lead.ID] = lease
	f.srv.kaScopeMu.Unlock()
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "b", AgentID: lead.ID, ScopeOn: true, ScopeGen: "ka_stuck"}, runsHere)
	<-machine.sendQueue
	time.Sleep(400 * time.Millisecond) // past the first lease
	if got := status("renewed"); got != 403 {
		t.Fatalf("a renewed scope must still refuse the toolbox after the first lease's end: %d", got)
	}
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "c", AgentID: lead.ID, ScopeOn: false, ScopeGen: "ka_older"}, runsHere)
	<-machine.sendQueue
	if got := status("late lift"); got != 403 {
		t.Fatalf("a late lift of an older ping must not open the toolbox: %d", got)
	}
	f.srv.handleKeepAliveScope(machine, protocol.Frame{ReqID: "d", AgentID: lead.ID, ScopeOn: false, ScopeGen: "ka_stuck"}, runsHere)
	<-machine.sendQueue
	if got := status("lifted"); got != 200 {
		t.Fatalf("the quarantine's lift opens the toolbox: %d", got)
	}
}
