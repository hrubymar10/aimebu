package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

const legacyHandle = "0f7c6a1e-1111-4222-8333-444455556666"

type legacyFixture struct {
	*fixture
	cwd string
}

// newLegacyFixture has a connected machine that lists claude natively, an imported v1 agent "sage@old" (claude-code, last seen long ago)
// whose working folder exists inside the project folder, a message of its own in the room (so its profile opens), and its transcript in
// a claude config directory the server's environment points at.
func newLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}, {Tool: "claude", Environment: adapter.Environment{Variant: "native"}}, {Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	// An underscore and a space: claude turns every character that is not a letter or a digit into a dash.
	cwd := filepath.Join(f.project.Folder, "my work_tree")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("CODEX_HOME", t.TempDir())
	writeTranscript := func(dirName string) {
		dir := filepath.Join(config, "projects", dirName)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, legacyHandle+".jsonl"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeTranscript(regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(real, "-"))
	if _, err := f.exec(`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES ('sage@old','claude-code','haiku','old',?,?,'2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`, real, legacyHandle); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES ('sage@old','x','reviewer')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_legacy',999999,?,'agent','sage@old','from the past','[]','2026-10-02T09:00:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	return legacyFixture{fixture: f, cwd: real}
}

func (f legacyFixture) start(t *testing.T, key string, extra map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"role_key": "worker", "tool": "claude", "variant": "native", "model": "haiku", "provider": "anthropic", "resume_legacy": "sage@old"}
	for k, v := range extra {
		body[k] = v
	}
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f legacyFixture) profile(t *testing.T, agent string) store.AgentProfile {
	t.Helper()
	resp := f.request(http.MethodGet, "/api/rooms/"+f.room.ID+"/agents/"+agent+"/profile", nil, false)
	defer resp.Body.Close()
	var profile store.AgentProfile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil || resp.StatusCode != 200 {
		t.Fatalf("profile %s: %d %v", agent, resp.StatusCode, err)
	}
	return profile
}

func TestHistoricalProfileOffersStartingAsAV2Agent(t *testing.T) {
	f := newLegacyFixture(t)
	profile := f.profile(t, "sage@old")
	if !profile.Imported || profile.Start == nil {
		t.Fatalf("profile: %+v", profile)
	}
	start := profile.Start
	if start.Slug != "sage" || start.Tool != "claude" || !start.Resumable || start.V1Active || start.OutsideFolder || start.RoleKey != "reviewer" ||
		start.CWD != f.cwd || start.Model != "haiku" || start.StartedAgentID != "" {
		t.Fatalf("start info: %+v", start)
	}
}

func TestStartAsV2AgentContinuesTheSessionThroughTheAPI(t *testing.T) {
	f := newLegacyFixture(t)
	status, created := f.start(t, "start-1", nil)
	if status != 201 || created["slug"] != "sage" {
		t.Fatalf("start: %d %+v", status, created)
	}
	agentID, _ := created["id"].(string)
	// The agent works in the v1 folder and its session is the v1 one, already started.
	live := f.profile(t, agentID)
	if live.NativeSessionID != legacyHandle || live.StartMode != "native_resume" || live.Cwd != f.cwd || live.ResumedFrom != "sage@old" {
		t.Fatalf("v2 profile: %+v", live)
	}
	// The historical profile now links to it and no longer offers a second start.
	old := f.profile(t, "sage@old")
	if old.Start == nil || old.Start.StartedAgentID != agentID || old.Start.StartedSlug != "sage" {
		t.Fatalf("historical profile after the start: %+v", old.Start)
	}
	status, again := f.start(t, "start-2", nil)
	if status != 409 || again["error"] != "already_started" {
		t.Fatalf("a second start: %d %+v", status, again)
	}
}

func TestStartAsV2AgentRefusesWhatCannotWork(t *testing.T) {
	f := newLegacyFixture(t)
	check := func(name string, extra map[string]any, wantStatus int, wantCode string) {
		t.Helper()
		status, body := f.start(t, "case-"+name, extra)
		if status != wantStatus || body["error"] != wantCode {
			t.Fatalf("%s: %d %+v, want %d %s", name, status, body, wantStatus, wantCode)
		}
	}
	check("unknown", map[string]any{"resume_legacy": "nobody@old"}, 404, "not_found")
	models, err := f.db.HarnessModels(t.Context(), f.harness(t, "codex", "native").ID)
	if err != nil || len(models) == 0 {
		t.Fatalf("no codex model on the harness: %v", err)
	}
	check("tool", map[string]any{"tool": "codex", "model": models[0].Slug, "provider": "openai"}, 400, "tool_mismatch")
	check("worktree", map[string]any{"worktree": true}, 400, "invalid_request")

	// v1 saw the agent a moment ago: refuse until the person confirms it is stopped.
	if _, err := f.exec(`UPDATE legacy_agent_session SET last_seen=? WHERE agent_id='sage@old'`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	check("active", nil, 409, "v1_may_be_active")
	if profile := f.profile(t, "sage@old"); profile.Start == nil || !profile.Start.V1Active {
		t.Fatalf("the profile does not warn: %+v", profile.Start)
	}
	if _, err := f.exec(`UPDATE legacy_agent_session SET last_seen='2026-10-02T10:00:00Z' WHERE agent_id='sage@old'`); err != nil {
		t.Fatal(err)
	}

	// No transcript at all.
	if err := os.RemoveAll(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")); err != nil {
		t.Fatal(err)
	}
	check("missing", nil, 409, "session_not_found")

	// The v1 folder is gone.
	if err := os.RemoveAll(f.cwd); err != nil {
		t.Fatal(err)
	}
	check("nofolder", nil, 409, "v1_folder_missing")
}

// A transcript that exists under a folder name this server would not have chosen is not a refusal: claude's naming has cases that are not
// worth mirroring (long paths, other versions), so the harness is left to find the session or fail the first turn.
func TestStartAsV2AgentDoesNotRefuseATranscriptUnderAnotherFolderName(t *testing.T) {
	f := newLegacyFixture(t)
	projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")
	entries, err := os.ReadDir(projects)
	if err != nil || len(entries) != 1 {
		t.Fatalf("transcript folders: %v %v", entries, err)
	}
	if err := os.Rename(filepath.Join(projects, entries[0].Name()), filepath.Join(projects, "-somewhere-else")); err != nil {
		t.Fatal(err)
	}
	if status, body := f.start(t, "elsewhere", nil); status != 201 {
		t.Fatalf("start: %d %+v", status, body)
	}
}

// Two people press Start at once (different idempotency keys, so neither replays the other): one agent is made, and the loser is told the
// v1 agent was already started, never that the name is in use (the winner's agent holds that name because of this very start).
func TestStartAsV2AgentTwiceAtOnceSaysAlreadyStarted(t *testing.T) {
	f := newLegacyFixture(t)
	type result struct {
		status int
		body   map[string]any
	}
	results := make(chan result, 2)
	for _, key := range []string{"race-a", "race-b"} {
		go func() {
			status, body := f.start(t, key, map[string]any{"role_key": "reviewer"})
			results <- result{status, body}
		}()
	}
	created, refused := 0, 0
	for range 2 {
		switch got := <-results; {
		case got.status == 201:
			created++
		case got.status == 409 && got.body["error"] == "already_started":
			refused++
		default:
			t.Fatalf("a start: %d %+v", got.status, got.body)
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("created %d, refused %d", created, refused)
	}
}

func TestStartAsV2AgentNeedsAPersonAndTheNameFree(t *testing.T) {
	f := newLegacyFixture(t)
	// A name another agent already holds in the project.
	status, _, _ := f.spawnRequest(t, "holder", "reviewer")
	if status != 201 {
		t.Fatal("setup spawn")
	}
	if _, err := f.exec(`UPDATE agent SET slug='sage' WHERE slug<>'sage' AND project_id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	status, body := f.start(t, "taken", nil)
	if status != 409 || body["error"] != "name_in_use" {
		t.Fatalf("name in use: %d %+v", status, body)
	}
	// A viewer does not get the offer.
	cookie, csrf := registerSecondUser(t, f.fixture, "viewer")
	if _, err := f.exec(`INSERT INTO membership(project_id,user_id,role) SELECT ?,id,'viewer' FROM user WHERE login='viewer'`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	response := f.requestAs(t, cookie, csrf, http.MethodGet, "/api/rooms/"+f.room.ID+"/agents/sage@old/profile", nil)
	defer response.Body.Close()
	var seen store.AgentProfile
	if err := json.NewDecoder(response.Body).Decode(&seen); err != nil || response.StatusCode != 200 || seen.Start != nil {
		t.Fatalf("viewer profile: %d %+v %v", response.StatusCode, seen.Start, err)
	}
}

// A supported harness with no usable v1 session id still starts under the same name, with a fresh session, and is linked like any other.
func TestStartAsV2AgentFreshWhenTheHarnessCannotResume(t *testing.T) {
	f := newLegacyFixture(t)
	if _, err := f.exec(`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES ('coder@old','codex','gpt','old','/tmp/anywhere','','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_coder',999998,?,'agent','coder@old','from codex','[]','2026-10-02T09:00:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	view := f.profile(t, "coder@old")
	if view.Start == nil || view.Start.Resumable || view.Start.Tool != "codex" || view.Start.Reason == "" {
		t.Fatalf("start info: %+v", view.Start)
	}
	status, created := f.start(t, "fresh-1", map[string]any{"resume_legacy": "coder@old", "tool": "fake", "variant": "test", "model": "test", "provider": ""})
	if status != 201 || created["slug"] != "coder" {
		t.Fatalf("fresh start: %d %+v", status, created)
	}
	live := f.profile(t, created["id"].(string))
	if live.StartMode != "fresh_with_history" || live.ResumedFrom != "coder@old" || live.NativeSessionID == "thread-abcdef123456" {
		t.Fatalf("a fresh start must not claim the v1 session: %+v", live)
	}
	if again := f.profile(t, "coder@old"); again.Start == nil || again.Start.StartedAgentID != created["id"] {
		t.Fatalf("link: %+v", again.Start)
	}
}

// The refusal to continue a session in a container follows the launcher's own flag, not its name: a containerized launcher called
// "work" is refused, and a native one called "docker" is not.
func TestContainerizationFollowsTheLauncherNotItsName(t *testing.T) {
	t.Parallel()
	if !launcherKeepsOwnSessions(&store.Launcher{Name: "work", Containerized: true}, "work") {
		t.Fatal("a custom containerized launcher keeps its own sessions")
	}
	if launcherKeepsOwnSessions(&store.Launcher{Name: "docker", Containerized: false}, "docker") {
		t.Fatal("a native launcher named docker does not")
	}
	if !launcherKeepsOwnSessions(nil, "docker") || launcherKeepsOwnSessions(nil, "native") {
		t.Fatal("with no launcher record the variant name decides")
	}
}

// startIn posts a start of sage@old into any room, not only the fixture's.
func (f legacyFixture) startIn(t *testing.T, roomID, key string, cookie *http.Cookie, csrf string) (int, map[string]any) {
	t.Helper()
	encoded, _ := json.Marshal(map[string]any{"role_key": "worker", "tool": "claude", "variant": "native", "model": "haiku", "provider": "anthropic", "resume_legacy": "sage@old"})
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+"/api/rooms/"+roomID+"/agents", bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// The lookup is per project: an agent that wrote in one room of the project can be started in another room of it, and the profile still
// opens only where it wrote.
func TestStartAsV2AgentIsScopedToTheProjectsHistory(t *testing.T) {
	f := newLegacyFixture(t)
	elsewhere := f.newTaskRoom("elsewhere")
	// Nothing is offered in a room the agent never wrote in...
	response := f.request(http.MethodGet, "/api/rooms/"+elsewhere.ID+"/agents/sage@old/profile", nil, false)
	response.Body.Close()
	if response.StatusCode == 200 {
		t.Fatal("the profile opened in a room the agent never wrote in")
	}
	// ...but the start works there, since it wrote in the project.
	status, created := f.startIn(t, elsewhere.ID, "project-1", f.cookie, f.csrf)
	if status != 201 {
		t.Fatalf("start in another room of the project: %d %+v", status, created)
	}
	if again, _ := f.startIn(t, f.room.ID, "project-2", f.cookie, f.csrf); again != 409 {
		t.Fatalf("started twice: %d", again)
	}
}

// An agent that wrote only in another project is not found, and nothing about it is revealed.
func TestStartAsV2AgentRefusesAnAgentFromAnotherProject(t *testing.T) {
	f := newLegacyFixture(t)
	resp := f.request("POST", "/api/projects", map[string]string{"name": "q", "folder": f.t.TempDir()}, true)
	var other store.Project
	_ = json.NewDecoder(resp.Body).Decode(&other)
	resp.Body.Close()
	if other.ID == "" || other.ID == f.project.ID {
		t.Fatalf("second project: %+v", other)
	}
	if _, err := f.exec(`UPDATE message SET room_id=(SELECT id FROM room WHERE project_id=? AND kind='task' LIMIT 1) WHERE author_id='sage@old'`, other.ID); err != nil {
		t.Fatal(err)
	}
	status, body := f.start(t, "foreign-1", nil)
	if status != 404 || body["error"] != "not_found" {
		t.Fatalf("an agent that never wrote in this project: %d %+v", status, body)
	}
	for _, leaked := range []string{f.cwd, legacyHandle} {
		if raw, _ := json.Marshal(body); strings.Contains(string(raw), leaked) {
			t.Fatalf("the refusal leaks %q: %s", leaked, raw)
		}
	}
}

// A v1 agent started in a lead-only home room is held to the room's rule like any other spawn: a role that is not a lead role is refused
// (home_is_lead_only), the lead role is taken, and a task room takes any role. The dialog explains the refusal before it gets here.
func TestStartAsV2AgentInALeadOnlyHomeRoomNeedsALeadRole(t *testing.T) {
	f := newLegacyFixture(t)
	task := f.room
	resp := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	resp.Body.Close()
	for _, room := range rooms {
		if room.Kind == "home" {
			f.room = room
		}
	}
	if f.room.ID == task.ID {
		t.Fatal("the project has no home room")
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_legacy_home',999998,?,'agent','sage@old','from the past, at home','[]','2026-10-02T09:30:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	// The agent's history is only in the home room (the one message it had in the task room goes), as for a worker that v1 kept at home.
	if _, err := f.exec(`DELETE FROM message WHERE id='msg_legacy'`); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"reviewer", "worker"} {
		status, body := f.start(t, "home-"+role, map[string]any{"role_key": role})
		if status != 409 || body["error"] != "home_is_lead_only" {
			t.Errorf("%s into the lead-only home room: %d %+v, want 409 home_is_lead_only", role, status, body)
		}
	}
	// Nothing was started, so the agent can still be started where its role is allowed: its history is only in the home room, and a task
	// room of the same project takes it.
	f.room = task
	if status, body := f.start(t, "task-room", map[string]any{"role_key": "reviewer"}); status != 201 {
		t.Fatalf("reviewer into a task room: %d %+v", status, body)
	}
}

// A v1 leader whose leader room became the project's home is started there with the lead role, and is then a full member of home: an
// unaddressed message from a person in home wakes it (an auto member would only read the room).
func TestStartAsV2LeaderInHomeIsAFullMember(t *testing.T) {
	f := newLegacyFixture(t)
	task := f.room
	resp := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/rooms", nil, false)
	var rooms []store.Room
	_ = json.NewDecoder(resp.Body).Decode(&rooms)
	resp.Body.Close()
	for _, room := range rooms {
		if room.Kind == "home" {
			f.room = room
		}
	}
	if f.room.ID == task.ID {
		t.Fatal("the project has no home room")
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_legacy_home',999998,?,'agent','sage@old','from the past, at home','[]','2026-10-02T09:30:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`DELETE FROM message WHERE id='msg_legacy'`); err != nil {
		t.Fatal(err)
	}
	status, created := f.start(t, "home-leader", map[string]any{"role_key": "leader"})
	if status != 201 {
		t.Fatalf("leader into home: %d %+v", status, created)
	}
	agentID, _ := created["id"].(string)
	message, err := f.db.InsertMessage(t.Context(), store.Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: f.ownerUserID(t), Body: "anyone home?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NoteForAgentMessage(t.Context(), agentID, message.ID); err != nil {
		t.Fatalf("the leader started in home was not woken by an unaddressed message: %v", err)
	}
}

// A v1 agent's Claude Code session can also be worked on as a copy: the first turn forks it, the original stays as it is, v1 may still have
// it open, and the agent is linked to its v1 name like any other start.
func TestStartAsV2AgentCanWorkOnACopyOfTheV1Session(t *testing.T) {
	f := newLegacyFixture(t)
	status, body := f.start(t, "fork-old", map[string]any{"resume_mode": "fork"})
	if status != 409 || body["error"] != "supervisor_update_needed" {
		t.Fatalf("a supervisor that cannot fork: %d %+v", status, body)
	}
	if status, body := f.start(t, "fork-bad", map[string]any{"resume_mode": "mirror"}); status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("an unknown mode: %d %+v", status, body)
	}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsSessionFork = true
	f.srv.machineMu.Unlock()
	// v1 saw the agent a moment ago: continuing would need the confirmation, a copy does not.
	if _, err := f.exec(`UPDATE legacy_agent_session SET last_seen=? WHERE agent_id='sage@old'`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if status, body := f.start(t, "continue-busy", nil); status != 409 || body["error"] != "v1_may_be_active" {
		t.Fatalf("continuing while v1 is active: %d %+v", status, body)
	}
	status, created := f.start(t, "fork-ok", map[string]any{"resume_mode": "fork"})
	if status != 201 || created["slug"] != "sage" {
		t.Fatalf("fork start: %d %+v", status, created)
	}
	agentID, _ := created["id"].(string)
	// The first turn forks the v1 session (the session still names the original until the copy's id is known).
	if pending, err := f.db.AgentForkPending(f.t.Context(), agentID); err != nil || !pending {
		t.Fatalf("the first turn must fork the v1 session: pending=%v %v", pending, err)
	}
	if live := f.profile(t, agentID); live.ResumedFrom != "sage@old" || live.StartMode != "native_resume" || live.Cwd != f.cwd {
		t.Fatalf("v2 profile: %+v", live)
	}
	if again := f.profile(t, "sage@old"); again.Start == nil || again.Start.StartedAgentID != created["id"] {
		t.Fatalf("the v1 name is not linked: %+v", again.Start)
	}
}

// There is nothing to copy when the harness cannot continue its v1 session.
func TestStartAsV2AgentCopyNeedsAResumableSession(t *testing.T) {
	f := newLegacyFixture(t)
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsSessionFork = true
	f.srv.machineMu.Unlock()
	if _, err := f.exec(`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES ('coder@old','cursor','gpt','old','/tmp/anywhere','thread-abcdef123456','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_coder',999998,?,'agent','coder@old','from codex','[]','2026-10-02T09:00:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	status, body := f.start(t, "fork-codex", map[string]any{"resume_legacy": "coder@old", "tool": "fake", "variant": "test", "model": "test", "provider": "", "resume_mode": "fork"})
	if status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("a copy of a session that cannot be continued: %d %+v", status, body)
	}
}

func TestStartAsV2AgentNativeCopyRequiresUpdatedSupervisor(t *testing.T) {
	f := newLegacyFixture(t)
	models, err := f.db.HarnessModels(t.Context(), f.harness(t, "codex", "native").ID)
	if err != nil || len(models) == 0 {
		t.Fatalf("no codex model on the harness: %v", err)
	}
	if _, err := f.exec(`INSERT INTO legacy_agent_session(agent_id,harness,model,project,cwd,harness_session_id,registered_at,last_seen) VALUES ('coder@old','codex',?,'old',?,'0f7c6a1e-1111-4222-8333-444455556666','2026-10-01T10:00:00Z','2026-10-02T10:00:00Z')`, models[0].Slug, f.cwd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.exec(`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_coder',999998,?,'agent','coder@old','from codex','[]','2026-10-02T09:00:00.000000000Z')`, f.room.ID); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"resume_legacy": "coder@old", "tool": "codex", "variant": "native", "model": models[0].Slug, "provider": "openai"}
	if status, body := f.start(t, "codex-old-supervisor", request); status != 409 || body["error"] != "supervisor_update_needed" {
		t.Fatalf("old supervisor: %d %+v", status, body)
	}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsLegacySessionCopy = true
	f.srv.machineMu.Unlock()
	if status, body := f.start(t, "codex-new-supervisor", request); status != 201 || body["slug"] != "coder" {
		t.Fatalf("native continuation: %d %+v", status, body)
	}
}

// v1 ran Claude in the folder as it spelled it (a link to it), and Claude keyed the session by that spelling: starting the v1 agent, whether
// continuing or on a copy, keeps the canonical checkout and starts its turns in v1's own spelling, like a session picked from the list. A
// supervisor that cannot do that is refused before anything is created.
func TestStartAsV2AgentFromAFolderV1ReachedThroughALink(t *testing.T) {
	for name, mode := range map[string]string{"continue": "continue", "copy": "fork"} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFixture(t)
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(f.cwd, link); err != nil {
				t.Fatal(err)
			}
			// v1 recorded the link as its working folder, and Claude saved the transcript under the link's name.
			projects := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects")
			entries, err := os.ReadDir(projects)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transcript folders: %v %v", entries, err)
			}
			if err := os.Rename(filepath.Join(projects, entries[0].Name()), filepath.Join(projects, regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(link, "-"))); err != nil {
				t.Fatal(err)
			}
			if _, err := f.exec(`UPDATE legacy_agent_session SET cwd=? WHERE agent_id='sage@old'`, link); err != nil {
				t.Fatal(err)
			}
			f.srv.machineMu.Lock()
			f.srv.machines[f.machineID].supportsSessionFork = true
			f.srv.machines[f.machineID].supportsLaunchPath = false
			f.srv.machineMu.Unlock()
			if status, body := f.start(t, "link-old-"+name, map[string]any{"resume_mode": mode}); status != 409 || body["error"] != "supervisor_update_needed" {
				t.Fatalf("a supervisor that predates the spelling: %d %+v", status, body)
			}
			f.srv.machineMu.Lock()
			f.srv.machines[f.machineID].supportsLaunchPath = true
			f.srv.machineMu.Unlock()
			status, created := f.start(t, "link-"+name, map[string]any{"resume_mode": mode})
			if status != 201 {
				t.Fatalf("start: %d %+v", status, created)
			}
			agentID, _ := created["id"].(string)
			if launch, err := f.db.AgentLaunchPath(t.Context(), agentID); err != nil || launch != link {
				t.Fatalf("v1's own spelling was not kept: %q %v", launch, err)
			}
			if live := f.profile(t, agentID); live.Cwd != f.cwd || (mode == "continue" && live.NativeSessionID != legacyHandle) {
				t.Fatalf("the checkout is not the canonical folder: %+v", live)
			}
			if pending, err := f.db.AgentLaunchPathPending(t.Context(), agentID); err != nil || !pending {
				t.Fatalf("the turn will not start in the spelling: %v %v", pending, err)
			}
		})
	}
}

// The team flow wizard offers the v1 agent that held the leader role, and starting the flow with it continues its session like a Spawn does.
func TestTeamSetupContinuesTheV1LeaderAndTheRoomsAgentCardLinksBack(t *testing.T) {
	f := newLegacyFixture(t)
	if _, err := f.exec(`INSERT INTO legacy_agent_role(agent_id,v1_room_id,role_key) VALUES ('sage@old','y','leader'),('sage@old','z','leader')`); err != nil {
		t.Fatal(err)
	}
	list := func(role string) []store.LegacyStartInfo {
		t.Helper()
		resp := f.request(http.MethodGet, "/api/projects/"+f.project.ID+"/legacy-agents?role="+role, nil, false)
		defer resp.Body.Close()
		var out struct {
			Agents []store.LegacyStartInfo `json:"agents"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
			t.Fatalf("list %s: %d %v", role, resp.StatusCode, err)
		}
		return out.Agents
	}
	if got := list("leader"); len(got) != 1 || got[0].LegacyID != "sage@old" || got[0].Slug != "sage" || !got[0].Resumable || got[0].Tool != "claude" {
		t.Fatalf("leader candidates: %+v", got)
	}
	if got := list("reviewer"); len(got) != 0 {
		t.Fatalf("an agent is offered only for the role it held most: %+v", got)
	}
	leader := map[string]any{"machine_id": f.machineID, "tool": "claude", "variant": "native", "model": "haiku", "resume_legacy": "sage@old"}
	path := "/api/projects/" + f.project.ID + "/team-setup"
	body := map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": leader}
	preview := f.request(http.MethodPost, path+"/preview", body, true)
	preview.Body.Close()
	if preview.StatusCode != 200 {
		t.Fatalf("preview %d", preview.StatusCode)
	}
	encoded, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", "team-leader-v1")
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created store.TeamSetupResult
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 201 || len(created.Agents) != 1 {
		t.Fatalf("create %d %v %+v", resp.StatusCode, err, created)
	}
	live := f.profile(t, created.Agents[0].AgentID)
	if live.NativeSessionID != legacyHandle || live.StartMode != "native_resume" || live.Cwd != f.cwd || live.ResumedFrom != "sage@old" {
		t.Fatalf("the leader did not continue the v1 session: %+v", live)
	}
	cards, err := f.db.AgentCardsForRoom(t.Context(), created.Agents[0].RoomID)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, card := range cards {
		linked = linked || card.ID == created.Agents[0].AgentID && card.Slug == "sage" && card.ResumedFromLegacyID == "sage@old"
	}
	if !linked {
		t.Fatalf("the agent card does not name the v1 agent it continues: %+v", cards)
	}
	if got := list("leader"); len(got) != 0 {
		t.Fatalf("a started v1 agent is offered again: %+v", got)
	}
}

func TestTeamSetupRefusesAV1LeaderThatCannotBeContinuedWithAnotherTool(t *testing.T) {
	f := newLegacyFixture(t)
	leader := map[string]any{"machine_id": f.machineID, "tool": "codex", "variant": "native", "model": "gpt-5", "resume_legacy": "sage@old"}
	resp := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-setup/preview", map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": leader}, true)
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode == 200 || (out["error"] != "tool_mismatch" && out["error"] != "invalid_request") {
		t.Fatalf("a v1 session continued with another tool: %d %+v", resp.StatusCode, out)
	}
}

// A team flow change that adds a role takes over the v1 agent that held it the same way, and the migration remembers it through the apply.
func TestTeamFlowMigrationContinuesTheV1Agent(t *testing.T) {
	f := newLegacyFixture(t)
	base := "/api/projects/" + f.project.ID
	post := func(path, key string, body any) (int, map[string]any) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.Header.Set("Idempotency-Key", key)
		req.AddCookie(f.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	fresh := map[string]any{"machine_id": f.machineID, "tool": "fake", "variant": "test", "model": "test"}
	if status, out := post(base+"/team-setup", "setup-first", map[string]any{"flow_key": "leader-workers", "flow_rev": 0, "leader": fresh}); status != 201 {
		t.Fatalf("setup %d %+v", status, out)
	}
	dispatcher := map[string]any{"machine_id": f.machineID, "tool": "claude", "variant": "native", "model": "haiku", "resume_legacy": "sage@old"}
	body := map[string]any{"flow_key": "leader-dispatcher", "flow_rev": 1, "dispatcher": dispatcher}
	preview := f.request(http.MethodPost, base+"/team-flow/migration/preview", body, true)
	var planned map[string]any
	_ = json.NewDecoder(preview.Body).Decode(&planned)
	preview.Body.Close()
	if preview.StatusCode != 200 || planned["can_apply"] != true {
		t.Fatalf("preview %d %+v", preview.StatusCode, planned)
	}
	if status, out := post(base+"/team-flow/migrate", "migrate-v1", body); status != 202 && status != 200 {
		t.Fatalf("migrate %d %+v", status, out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := f.db.AdvanceTeamFlowMigrations(t.Context()); err != nil {
			t.Fatal(err)
		}
		legacy, err := f.db.LegacyStart(t.Context(), f.project.ID, "sage@old")
		if err == nil && legacy.StartedID != "" && legacy.StartedSlg == "sage" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the v1 agent was not continued by the migration: %+v %v", legacy, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
