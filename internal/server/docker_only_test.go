package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/store"
)

func (f *fixture) setDockerOnly(t *testing.T, on bool) {
	t.Helper()
	resp := f.request("PUT", "/api/agent-isolation", map[string]bool{"docker_only": on}, true)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT /api/agent-isolation status=%d", resp.StatusCode)
	}
}

// The setting is read by any signed-in person, written by an admin, and off until someone turns it on.
func TestAgentIsolationSettingReadWrite(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	read := func() (bool, bool) {
		t.Helper()
		resp := f.request("GET", "/api/agent-isolation", nil, false)
		defer resp.Body.Close()
		var body struct {
			DockerOnly bool `json:"docker_only"`
			CanEdit    bool `json:"can_edit"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&body) != nil {
			t.Fatalf("GET status=%d", resp.StatusCode)
		}
		return body.DockerOnly, body.CanEdit
	}
	if on, canEdit := read(); on || !canEdit {
		t.Fatalf("default: on=%v canEdit=%v", on, canEdit)
	}
	resp := f.request("PUT", "/api/agent-isolation", map[string]string{}, true)
	if resp.StatusCode != 400 {
		t.Fatalf("a PUT with no value: %d", resp.StatusCode)
	}
	resp.Body.Close()
	f.setDockerOnly(t, true)
	if on, _ := read(); !on {
		t.Fatal("the setting did not stick")
	}
	f.setDockerOnly(t, false)
	if on, _ := read(); on {
		t.Fatal("the setting did not turn off")
	}
}

// With the setting on a native launcher is refused: by the helper the spawn and the switch use, by the dispatch of a turn (the agent gets no turn
// and the error says why), and by the card's hold. A Docker launcher, and the test agent, are not refused.
func TestDockerOnlyRefusesNativeLaunchers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, _, _, delivery := f.execution(t)
	ctx := context.Background()
	native := store.Launcher{Containerized: false}
	container := store.Launcher{Containerized: true}
	refused := func(tool, variant string, launcher *store.Launcher) bool {
		t.Helper()
		got, err := f.srv.refusedNative(ctx, tool, variant, launcher)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if refused("claude", "native", nil) || refused("claude", "native", &native) {
		t.Fatal("refused with the setting off")
	}
	f.setDockerOnly(t, true)
	for _, c := range []struct {
		tool, variant string
		launcher      *store.Launcher
		want          bool
	}{
		{"claude", "native", nil, true}, {"claude", "docker", nil, false}, {"codex", "mine", &native, true}, {"codex", "mine", &container, false}, {"fake", "test", nil, false},
	} {
		if got := refused(c.tool, c.variant, c.launcher); got != c.want {
			t.Errorf("%s/%s launcher=%v: refused=%v want %v", c.tool, c.variant, c.launcher, got, c.want)
		}
	}
	// A turn of an agent on a native launcher is refused at dispatch, a Docker one is not, whatever the stored agent says.
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	delivery.Agent = adapter.AgentSpec{ID: agent.ID, Tool: "claude", Variant: "native", Model: "haiku"}
	if err := f.srv.Dispatch(ctx, delivery); !errors.Is(err, errDockerOnly) {
		t.Fatalf("a native turn was dispatched: %v", err)
	}
	if state, err := f.db.DeliveryState(ctx, delivery.TurnID); err == nil && state.Exec != "pending" {
		t.Fatalf("a refused turn was admitted: %+v", state)
	}
	// The card says why.
	holds := f.srv.agentHolds(ctx, []store.AgentCard{{ID: agent.ID, MachineID: f.machineID, Tool: "claude", Variant: "native", Desired: "active", Observed: "idle"}, {ID: "agt_docker", MachineID: f.machineID, Tool: "claude", Variant: "docker", Desired: "active", Observed: "idle"}, {ID: "agt_stopped", MachineID: f.machineID, Tool: "claude", Variant: "native", Desired: "stopped"}})
	if hold := holds[agent.ID]; hold == nil || hold.Code != "docker_only" {
		t.Fatalf("the native agent's card has no docker_only hold: %+v", holds)
	}
	if holds["agt_docker"] != nil || holds["agt_stopped"] != nil {
		t.Fatalf("a Docker or stopped agent was held: %+v", holds)
	}
}

// Anyone signed in reads the setting; only an admin writes it: an operator, a viewer and a project owner are refused, and the setting stays as it was.
func TestAgentIsolationSettingIsAdminOnly(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	for _, who := range []adminActor{w.pOwner, w.oper, w.viewer, w.out} {
		if code := w.call(t, who, "PUT", "/api/agent-isolation", map[string]bool{"docker_only": true}); code != 403 {
			t.Errorf("%s turned the setting on: %d", who.name, code)
		}
		code, body := w.json(t, who, "GET", "/api/agent-isolation", nil)
		if code != 200 || body["docker_only"] != false || body["can_edit"] != false {
			t.Errorf("%s reads %d %v", who.name, code, body)
		}
	}
	if on, err := w.f.srv.dockerOnly(t.Context()); err != nil || on {
		t.Fatalf("a refused write changed the setting: %v %v", on, err)
	}
	if code := w.call(t, w.admin, "PUT", "/api/agent-isolation", map[string]bool{"docker_only": true}); code != 200 {
		t.Fatalf("an admin turns it on: %d", code)
	}
	if code, body := w.json(t, w.viewer, "GET", "/api/agent-isolation", nil); code != 200 || body["docker_only"] != true {
		t.Fatalf("a viewer reads the new value: %d %v", code, body)
	}
}

// An agent on a native launcher is held before a turn is made for it: with the setting on, a pass of the dispatcher over an agent with a queued message makes
// no turn row (it used to make one and cancel it on every pass), says why once in the room, and sends the message once the setting goes off.
func TestDockerOnlyHoldsANativeAgentBeforeAnyTurnIsMade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	ctx := context.Background()
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_native','worker','Worker','worker','worker')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_native',?,?,?)`, f.project.ID, f.machineID, "/tmp/native")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := f.db.CreateAgent(ctx, store.Agent{Slug: "nat", ProjectID: f.project.ID, RoleID: "role_native", Tool: "claude", Variant: "native", Model: "haiku", Provider: "claude", MachineID: f.machineID, CheckoutID: "chk_native", KeyHash: "pending", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES ('ses_native',?,0,'fresh_with_history','2026-10-13T00:00:00.000000000Z')`, agent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.setDockerOnly(t, true)
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if response := f.request("POST", "/api/rooms/"+f.room.ID+"/messages", map[string]string{"body": "@nat do this"}, true); response.StatusCode != 201 {
		t.Fatalf("post status=%d", response.StatusCode)
	}
	reader, err := sql.Open("sqlite", "file:"+f.db.DBPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count := func(query string, args ...any) int {
		var n int
		if err := reader.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	turns := func() int { return count(`SELECT COUNT(*) FROM turn WHERE agent_id=?`, agent.ID) }
	notices := func() int {
		return count(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%Only run agents in docker%'`, f.room.ID)
	}
	d := NewDispatcher(f.srv)
	for pass := 0; pass < 3; pass++ {
		if sent, err := d.Pass(ctx); err != nil || sent != 0 {
			t.Fatalf("pass %d dispatched %d: %v", pass, sent, err)
		}
	}
	if n := turns(); n != 0 {
		t.Fatalf("a held agent had %d turn row(s) made for it", n)
	}
	if n := notices(); n != 1 {
		t.Fatalf("the hold is said %d times in the room, want once", n)
	}
	f.setDockerOnly(t, false)
	if sent, err := d.Pass(ctx); err != nil || sent != 1 {
		t.Fatalf("with the setting off the agent's queued message is dispatched: %d %v", sent, err)
	}
	if n := turns(); n != 1 {
		t.Fatalf("the released agent has %d turn rows, want 1", n)
	}
}

// When the setting is on and an agent's launcher cannot be read, the agent is held (a native launcher cannot be ruled out), no turn is made for it, and the room is
// told once, not on every pass.
func TestDockerOnlyHoldsAnAgentWhoseLauncherCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	agent, _, _, _ := f.execution(t)
	ctx := context.Background()
	// The row the agent names is gone: written around the foreign key, on a connection of its own (the schema would not let a launcher go while an agent uses it).
	writer, err := sql.Open("sqlite", "file:"+f.db.DBPath()+"?_pragma=foreign_keys(0)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`UPDATE agent SET tool='claude', variant='mine', launcher_id='lch_gone' WHERE id=?`, agent.ID); err != nil {
		t.Fatalf("pointing the agent at a missing launcher: %v", err)
	}
	f.setDockerOnly(t, true)
	conn := f.dialMachine(t, nil)
	defer conn.Close(websocket.StatusNormalClosure, "")
	reader, err := sql.Open("sqlite", "file:"+f.db.DBPath()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	count := func(query string, args ...any) int {
		var n int
		if err := reader.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	turns := func() int { return count(`SELECT COUNT(*) FROM turn WHERE agent_id=?`, agent.ID) }
	before := turns() // the fixture made one turn of its own
	d := NewDispatcher(f.srv)
	for pass := 0; pass < 3; pass++ {
		if sent, err := d.Pass(ctx); err != nil || sent != 0 {
			t.Fatalf("pass %d dispatched %d: %v", pass, sent, err)
		}
	}
	if n := turns() - before; n != 0 {
		t.Fatalf("a held agent had %d turn row(s) made for it", n)
	}
	if n := count(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%launcher could not be read%'`, f.room.ID); n != 1 {
		t.Fatalf("the hold is said %d times, want once", n)
	}
}

// The warm window of the agents is read by anyone signed in and changed only by the owner (an admin may change "only in docker" but not how much
// memory the machines hold between turns). It is five minutes until set, 0 is off, it is in minutes, and is bounded.
func TestWarmIdleSettingIsOwnerOnly(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	read := func(who adminActor) (any, any) {
		t.Helper()
		code, body := w.json(t, who, "GET", "/api/agent-isolation", nil)
		if code != 200 {
			t.Fatalf("%s reads %d", who.name, code)
		}
		return body["warm_idle_minutes"], body["can_edit_warm"]
	}
	if minutes, canEdit := read(w.viewer); minutes != float64(5) || canEdit != false {
		t.Fatalf("default for a viewer: %v %v", minutes, canEdit)
	}
	if _, body := w.json(t, w.viewer, "GET", "/api/agent-isolation", nil); body["warm_idle_saved"] != false {
		t.Fatalf("nothing is saved yet: %v", body["warm_idle_saved"])
	}
	for _, who := range []adminActor{w.admin, w.pOwner, w.oper, w.viewer, w.out} {
		if code := w.call(t, who, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": 5}); code != 403 {
			t.Errorf("%s changed the warm window: %d", who.name, code)
		}
	}
	if _, saved, err := w.f.srv.warmIdleSetting(t.Context()); err != nil || saved {
		t.Fatalf("a refused write saved something: %v %v", saved, err)
	}
	for _, bad := range []int{-1, 1441} {
		if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": bad}); code != 400 {
			t.Errorf("%d minutes: %d", bad, code)
		}
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": 5}); code != 200 {
		t.Fatalf("the owner sets it: %d", code)
	}
	if minutes, canEdit := read(w.owner); minutes != float64(5) || canEdit != true {
		t.Fatalf("owner reads %v %v", minutes, canEdit)
	}
	if minutes, _ := read(w.viewer); minutes != float64(5) {
		t.Fatalf("a viewer reads %v", minutes)
	}
	if seconds, saved, _ := w.f.srv.warmIdleSetting(t.Context()); seconds != 300 || !saved {
		t.Fatalf("stored as seconds: %d saved=%v", seconds, saved)
	}
	// Setting docker-only alone leaves the window as it was, and an admin may do that.
	if code := w.call(t, w.admin, "PUT", "/api/agent-isolation", map[string]bool{"docker_only": true}); code != 200 {
		t.Fatalf("an admin turns docker-only on: %d", code)
	}
	if seconds, _ := w.f.srv.warmIdleSeconds(t.Context()); seconds != 300 {
		t.Fatalf("docker-only changed the window: %d", seconds)
	}
}

func TestCodexWarmModeRequiresOwnerOptIn(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	if code, body := w.json(t, w.viewer, "GET", "/api/agent-isolation", nil); code != 200 || body["codex_warm_enabled"] != false {
		t.Fatalf("default Codex warm setting: %d %+v", code, body)
	}
	if value, _, err := w.f.srv.Store.GlobalSettingValue(t.Context(), codexWarmEnabledKey); err != nil || value != "0" {
		t.Fatalf("migration did not keep Codex cold by default: %q %v", value, err)
	}
	for _, who := range []adminActor{w.admin, w.pOwner, w.oper, w.viewer} {
		if code := w.call(t, who, "PUT", "/api/agent-isolation", map[string]bool{"codex_warm_enabled": true}); code != 403 {
			t.Fatalf("%s enabled Codex warm mode: %d", who.name, code)
		}
	}
	if code, body := w.json(t, w.owner, "PUT", "/api/agent-isolation", map[string]bool{"codex_warm_enabled": true}); code != 200 || body["codex_warm_enabled"] != true {
		t.Fatalf("owner opt-in: %d %+v", code, body)
	}
	if value, _, err := w.f.srv.Store.GlobalSettingValue(t.Context(), codexWarmEnabledKey); err != nil || value != "1" {
		t.Fatalf("saved opt-in: %q %v", value, err)
	}
}

// The warm window is the owner's saved value when there is one, else five minutes; a saved 0 is off (not "unset"), and what a delivery and an
// agent_settings frame carry is only the saved value, so a supervisor's own default applies when the owner saved none.
func TestWarmIdleDefaultSavedZeroAndWhatIsSent(t *testing.T) {
	t.Parallel()
	w := newAdminWorld(t)
	ctx := t.Context()
	effective := func() (int, bool) {
		t.Helper()
		seconds, err := w.f.srv.warmIdleSeconds(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := w.f.srv.warmIdleToSend(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return seconds, sent != nil
	}
	if seconds, sent := effective(); seconds != 300 || sent {
		t.Fatalf("nothing saved: effective %d, sent=%v; want 300 and nothing sent", seconds, sent)
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": 0}); code != 200 {
		t.Fatalf("the owner turns it off: %d", code)
	}
	if seconds, sent := effective(); seconds != 0 || !sent {
		t.Fatalf("a saved 0 is off and is sent: effective %d, sent=%v", seconds, sent)
	}
	if code := w.call(t, w.owner, "PUT", "/api/agent-isolation", map[string]int{"warm_idle_minutes": 20}); code != 200 {
		t.Fatalf("the owner sets 20: %d", code)
	}
	if seconds, sent := effective(); seconds != 1200 || !sent {
		t.Fatalf("saved 20 minutes: effective %d, sent=%v", seconds, sent)
	}
	// A stored value that cannot be read counts as none: the default applies and nothing is sent.
	if err := w.f.srv.Store.SetGlobalSettingValue(ctx, agentsWarmIdleKey, "soon"); err != nil {
		t.Fatal(err)
	}
	if seconds, sent := effective(); seconds != 300 || sent {
		t.Fatalf("an unreadable value: effective %d, sent=%v", seconds, sent)
	}
}
