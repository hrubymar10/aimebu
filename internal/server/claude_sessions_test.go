package server

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/ccsessions"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// fakeSupervisor is a connected machine that answers session scans from a fixed list, as a supervisor reading a Claude home would. No real
// Claude home is read anywhere in these tests.
type fakeSupervisor struct {
	mu       sync.Mutex
	sessions []protocol.SessionCandidate
	scans    []protocol.Frame
	// unreadable and truncated are what a scan of folders reports besides the sessions.
	unreadable int
	truncated  bool
}

func (f *fixture) connectSupervisor(t *testing.T, scans bool, sessions ...protocol.SessionCandidate) *fakeSupervisor {
	t.Helper()
	return f.connectSupervisorOn(t, f.machineID, scans, sessions...)
}

func (f *fixture) connectSupervisorOn(t *testing.T, machineID string, scans bool, sessions ...protocol.SessionCandidate) *fakeSupervisor {
	t.Helper()
	fake := &fakeSupervisor{sessions: sessions}
	machine := &machineConn{id: machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{},
		supportsSessionScan: scans, supportsSessionFork: true, supportsLaunchPath: true, supportsPathProbe: true,
		capabilities: []protocol.CapabilityReport{{Tool: "claude", Environment: adapter.Environment{Variant: "native"}}, {Tool: "codex", Environment: adapter.Environment{Variant: "native"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[machineID] = machine
	f.srv.machineMu.Unlock()
	t.Cleanup(func() { close(machine.done) })
	go func() {
		for {
			select {
			case request := <-machine.sendQueue:
				if request.Type == protocol.PathProbe {
					// The folder check a launcher spawn makes through the supervisor: this one finds every folder as it is spelled.
					machine.harnessMu.Lock()
					waiter := machine.pathPending[request.ReqID]
					machine.harnessMu.Unlock()
					if waiter != nil {
						waiter <- protocol.Frame{V: protocol.Version, Type: protocol.PathResult, ReqID: request.ReqID, CanonicalPath: request.CheckoutPath}
					}
					continue
				}
				if request.Type != protocol.SessionScan {
					continue
				}
				fake.mu.Lock()
				fake.scans = append(fake.scans, request)
				reply := protocol.Frame{V: protocol.Version, Type: protocol.SessionScanResult, ReqID: request.ReqID}
				if request.ScanSessionID != "" {
					reply.Code, reply.Message = "session_not_found", "none"
					for _, session := range fake.sessions {
						if session.ID == request.ScanSessionID {
							reply.Code, reply.Message, reply.Sessions = "", "", []protocol.SessionCandidate{session}
						}
					}
				} else {
					reply.Unreadable, reply.ScanTruncated = fake.unreadable, fake.truncated
					for _, session := range fake.sessions {
						for _, folder := range request.ScanFolders {
							if session.CWD == folder || strings.HasPrefix(session.CWD, folder+"/") {
								reply.Sessions = append(reply.Sessions, session)
								break
							}
						}
					}
				}
				fake.mu.Unlock()
				machine.harnessMu.Lock()
				waiter := machine.pathPending[request.ReqID]
				machine.harnessMu.Unlock()
				if waiter != nil {
					waiter <- reply
				}
			case <-machine.done:
				return
			}
		}
	}()
	return fake
}

func candidate(id, cwd, title string) protocol.SessionCandidate {
	return protocol.SessionCandidate{ID: id, CWD: cwd, Dir: ccsessions.FolderName(cwd), Title: title, ModifiedAt: "2026-10-24T10:00:00Z", Size: 1234, Entrypoint: "cli"}
}

const (
	sessionA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	sessionB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	sessionC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func (f *fixture) spawnSession(t *testing.T, key string, body map[string]any) (int, map[string]any) {
	t.Helper()
	request := map[string]any{"role_key": "worker", "tool": "claude", "variant": "native", "model": "haiku", "machine_id": f.machineID}
	if _, ok := body["resume_session"]; ok {
		request["resume_mode"] = "continue" // these tests are about continuing; the fork ones say so
	}
	for k, v := range body {
		request[k] = v
	}
	data, _ := json.Marshal(request)
	req, _ := http.NewRequest("POST", f.http.URL+"/api/rooms/"+f.room.ID+"/agents", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", f.csrf)
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (f *fixture) listSessions(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	resp := f.request("GET", "/api/projects/"+f.project.ID+"/claude-sessions"+query, nil, false)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestClaudeSessionsListsTheProjectFolderAndMarksWhatIsTaken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	sub := filepath.Join(f.project.Folder, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	fake := f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "Fix the login bug"), candidate(sessionB, sub, "Docs pass"), candidate(sessionC, outside, "Elsewhere"))
	status, body := f.listSessions(t, "?machine_id="+f.machineID)
	if status != 200 {
		t.Fatalf("status=%d %v", status, body)
	}
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 2 || body["folder"] == "" {
		t.Fatalf("the folder's sessions: %v", body)
	}
	for _, item := range sessions {
		session := item.(map[string]any)
		if session["outside_folder"] != false || session["agent"] != nil {
			t.Fatalf("a session of the project folder: %v", session)
		}
	}
	if len(fake.scans) != 1 || len(fake.scans[0].ScanFolders) == 0 || fake.scans[0].ScanSessionID != "" {
		t.Fatalf("scan request: %+v", fake.scans)
	}
	// By id it is found wherever it worked, and flagged as outside the project folder.
	status, body = f.listSessions(t, "?session_id="+sessionC)
	sessions, _ = body["sessions"].([]any)
	if status != 200 || len(sessions) != 1 || sessions[0].(map[string]any)["outside_folder"] != true {
		t.Fatalf("by id: %d %v", status, body)
	}
	if status, _ := f.listSessions(t, "?session_id=bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc"); status != 404 {
		t.Fatalf("an unknown id: %d", status)
	}
	if status, _ := f.listSessions(t, "?session_id=../etc"); status != 400 {
		t.Fatalf("a path as id: %d", status)
	}
	// Once an agent continues a session the list says which.
	if status, _ := f.spawnSession(t, "continue-a", map[string]any{"resume_session": sessionA}); status != 201 {
		t.Fatalf("spawn: %d", status)
	}
	_, body = f.listSessions(t, "")
	taken := 0
	for _, item := range body["sessions"].([]any) {
		session := item.(map[string]any)
		if session["id"] == sessionA {
			agent, _ := session["agent"].(map[string]any)
			if agent == nil || agent["slug"] == "" {
				t.Fatalf("a continued session names its agent: %v", session)
			}
			taken++
		} else if session["agent"] != nil {
			t.Fatalf("an unrelated session is taken: %v", session)
		}
	}
	if taken != 1 {
		t.Fatalf("taken=%d", taken)
	}
}

func TestClaudeSessionsNeedAnOperatorAndAnUpdatedSupervisor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.connectSupervisor(t, false, candidate(sessionA, f.project.Folder, "x"))
	if status, body := f.listSessions(t, ""); status != 409 || body["error"] != "supervisor_update_needed" {
		t.Fatalf("an older supervisor: %d %v", status, body)
	}
	if status, body := f.spawnSession(t, "old-supervisor", map[string]any{"resume_session": sessionA}); status != 409 || body["error"] != "supervisor_update_needed" {
		t.Fatalf("spawn through an older supervisor: %d %v", status, body)
	}
	cookie, csrf := f.member(t, "viewer", "viewer")
	resp := f.requestAs(t, cookie, csrf, "GET", "/api/projects/"+f.project.ID+"/claude-sessions", nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("a viewer: %d", resp.StatusCode)
	}
	cookie, csrf = f.member(t, "operator", "operator")
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsSessionScan = true
	f.srv.machineMu.Unlock()
	resp = f.requestAs(t, cookie, csrf, "GET", "/api/projects/"+f.project.ID+"/claude-sessions", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("an operator: %d", resp.StatusCode)
	}
}

func TestSpawnContinuesAClaudeSessionInARoomOfItsOwn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "Fix the login bug"))
	status, card := f.spawnSession(t, "continue-1", map[string]any{"resume_session": sessionA, "new_room": true})
	if status != 201 {
		t.Fatalf("spawn: %d %v", status, card)
	}
	roomID, agentID := card["room_id"].(string), card["id"].(string)
	if roomID == "" || roomID == f.room.ID {
		t.Fatalf("the agent is not in a room of its own: %v", card)
	}
	room, err := f.db.RoomByID(t.Context(), roomID)
	if err != nil || room.Title != "Fix the login bug" || room.Kind != "task" || room.ProjectID != f.project.ID {
		t.Fatalf("room=%+v err=%v", room, err)
	}
	profile, err := f.db.AgentProfile(t.Context(), roomID, agentID)
	if err != nil || profile.NativeSessionID != sessionA || profile.StartMode != "native_resume" {
		t.Fatalf("profile=%+v err=%v", profile, err)
	}
	if want, _ := filepath.EvalSymlinks(f.project.Folder); profile.Cwd != want {
		t.Fatalf("cwd=%q want %q", profile.Cwd, want)
	}
	messages, err := f.db.ListMessagesAfter(t.Context(), roomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, m := range messages {
		said = said || strings.Contains(m.Body, `continuing the Claude Code session "Fix the login bug" (aaaaaaaa)`)
	}
	if !said {
		t.Fatalf("no note in the new room about the continued session: %+v", messages)
	}
	// One agent per session; the same request retried is the same agent.
	if status, body := f.spawnSession(t, "continue-2", map[string]any{"resume_session": sessionA}); status != 409 || body["error"] != "already_started" {
		t.Fatalf("a second agent on the session: %d %v", status, body)
	}
	if status, again := f.spawnSession(t, "continue-1", map[string]any{"resume_session": sessionA, "new_room": true}); status != 201 || again["id"] != agentID {
		t.Fatalf("the retry: %d %v", status, again)
	}
}

func TestSpawnContinuesASessionInTheNamedRoomWithoutANewOne(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "Fix the login bug"))
	status, card := f.spawnSession(t, "same-room", map[string]any{"resume_session": sessionA})
	if status != 201 || card["room_id"] != f.room.ID {
		t.Fatalf("spawn: %d %v", status, card)
	}
}

func TestSpawnResumeSessionRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	outside := t.TempDir()
	active := candidate(sessionB, f.project.Folder, "Open in a terminal")
	active.Active = true
	// A session made through a link to the project folder: Claude saved it under the link's name, which the resolved folder does not have.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(f.project.Folder, link); err != nil {
		t.Fatal(err)
	}
	alias := candidate(sessionC, link, "Linked folder")
	gone := candidate("dddddddd-dddd-4ddd-8ddd-dddddddddddd", filepath.Join(f.project.Folder, "removed"), "Folder is gone")
	f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "Fine"), active, alias, gone, candidate("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", outside, "Outside"))
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
		code   string
	}{
		"unknown id":              {map[string]any{"resume_session": "ffffffff-ffff-4fff-8fff-ffffffffffff"}, 404, "session_not_found"},
		"not an id":               {map[string]any{"resume_session": "../x"}, 400, "invalid_request"},
		"a live process has it":   {map[string]any{"resume_session": sessionB}, 409, "session_active"},
		"another harness":         {map[string]any{"resume_session": sessionA, "tool": "codex", "variant": "native", "model": "gpt-6-sol"}, 400, "tool_mismatch"},
		"a worktree":              {map[string]any{"resume_session": sessionA, "worktree": true}, 400, "invalid_request"},
		"the folder is gone":      {map[string]any{"resume_session": "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}, 409, "session_folder_missing"},
		"a new room with nothing": {map[string]any{"new_room": true}, 400, "invalid_request"},
		"both kinds of resume":    {map[string]any{"resume_session": sessionA, "resume_legacy": "old@proj"}, 400, "invalid_request"},
	} {
		status, body := f.spawnSession(t, "refuse-"+strings.ReplaceAll(name, " ", "-"), tc.body)
		if status != tc.status || body["error"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", name, status, body, tc.status, tc.code)
		}
	}
	// A confirmed live session goes ahead.
	if status, body := f.spawnSession(t, "confirmed", map[string]any{"resume_session": sessionB, "confirm_session_closed": true}); status != 201 {
		t.Fatalf("a confirmed session: %d %v", status, body)
	}
	// A session outside the project folder waits for the owner's approval, like any agent outside its folder.
	status, card := f.spawnSession(t, "outside", map[string]any{"resume_session": "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"})
	if status != 201 || card["desired"] != "not_accepting" {
		t.Fatalf("outside the folder: %d %v", status, card)
	}
	if card["boundary_pending"] == nil {
		t.Fatalf("no approval is pending for a session outside the project folder: %v", card)
	}
}

func TestSessionFoundFromTellsALinkedFolder(t *testing.T) {
	t.Parallel()
	linked := protocol.SessionCandidate{CWD: "/tmp/link/proj", Dir: ccsessions.FolderName("/tmp/link/proj")}
	if sessionFoundFrom(linked, "/tmp/link/proj") != true || sessionFoundFrom(linked, "/private/tmp/real/proj") != false {
		t.Fatal("a session saved under a link's name is not found from the resolved path")
	}
	hashed := protocol.SessionCandidate{CWD: "/very/long/path", Dir: "-very-long-path-0f3a9c"}
	if !sessionFoundFrom(hashed, "/some/resolved/path") || !sessionFoundFrom(protocol.SessionCandidate{CWD: "/p"}, "/q") {
		t.Fatal("a directory that does not follow the naming rule is left to Claude")
	}
}

// On a machine that is not the host the project's folder is the one mapped for that machine; an id lookup scans no folder, and must still
// say a session of the mapped folder is inside the project.
func TestClaudeSessionsOnARemoteMachineClassifyByTheMappedFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	var remote store.Machine
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		remote, err = tx.CreateMachine(store.Machine{Name: "remote", KeyHash: "remote-key", Approved: true})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,?,?)`, f.project.ID, remote.ID, "/srv/checkouts/proj")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.connectSupervisorOn(t, remote.ID, true, candidate(sessionA, "/srv/checkouts/proj/sub", "Inside"), candidate(sessionB, "/srv/elsewhere", "Outside"))
	for id, outside := range map[string]bool{sessionA: false, sessionB: true} {
		status, body := f.listSessions(t, "?machine_id="+remote.ID+"&session_id="+id)
		sessions, _ := body["sessions"].([]any)
		if status != 200 || len(sessions) != 1 || sessions[0].(map[string]any)["outside_folder"] != outside {
			t.Fatalf("id lookup of %s: %d %v", id, status, body)
		}
	}
}

// What the supervisor could not read is shown to the person, not hidden.
func TestClaudeSessionsReportsWhatWasCutOrUnreadable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	fake := f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "x"))
	fake.unreadable, fake.truncated = 3, true
	status, body := f.listSessions(t, "")
	if status != 200 || body["unreadable"] != float64(3) || body["truncated"] != true {
		t.Fatalf("%d %v", status, body)
	}
}

// An id lookup is for a machine where the project has a folder; without one there is nothing to say a session belongs to the project.
func TestClaudeSessionLookupNeedsAFolderMappedOnTheMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	var remote store.Machine
	err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		remote, err = tx.CreateMachine(store.Machine{Name: "unmapped", KeyHash: "unmapped-key", Approved: true})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := f.connectSupervisorOn(t, remote.ID, true, candidate(sessionA, "/srv/anywhere", "x"))
	status, body := f.listSessions(t, "?machine_id="+remote.ID+"&session_id="+sessionA)
	if status != 409 || body["error"] != "machine_folder_missing" {
		t.Fatalf("%d %v", status, body)
	}
	if len(fake.scans) != 0 {
		t.Fatalf("the machine was asked anyway: %+v", fake.scans)
	}
}

// Whoever does not say how a session is taken over gets a copy: the original stays as it is, and nothing already using it stands in the way.
func TestSpawnWorksOnACopyOfASessionByDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	active := candidate(sessionA, f.project.Folder, "Fix the login bug")
	active.Active = true
	f.connectSupervisor(t, true, active)
	status, card := f.spawnSession(t, "fork-1", map[string]any{"resume_session": sessionA, "resume_mode": "", "new_room": true})
	if status != 201 {
		t.Fatalf("a copy of a session a terminal has open needs no confirmation: %d %v", status, card)
	}
	roomID, agentID := card["room_id"].(string), card["id"].(string)
	imported, ok, err := f.db.AgentImportedSession(t.Context(), agentID)
	if err != nil || !ok || imported.Mode != "fork" || imported.SessionID != sessionA || imported.ForkedSessionID != "" {
		t.Fatalf("imported=%+v ok=%v err=%v", imported, ok, err)
	}
	profile, err := f.db.AgentProfile(t.Context(), roomID, agentID)
	if err != nil || profile.NativeSessionID != "" {
		t.Fatalf("a copy not yet named has no session of its own: %+v err=%v", profile, err)
	}
	messages, err := f.db.ListMessagesAfter(t.Context(), roomID, 0)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, m := range messages {
		said = said || strings.Contains(m.Body, `working on a copy of the Claude Code session "Fix the login bug" (aaaaaaaa)`)
	}
	if !said {
		t.Fatalf("the room was not told it is a copy: %+v", messages)
	}
	// The original is still free: the list does not name the copy's agent, and a second copy is made beside the first (as a reviewer, because
	// the folder's one writer is taken).
	_, body := f.listSessions(t, "")
	for _, item := range body["sessions"].([]any) {
		if item.(map[string]any)["agent"] != nil {
			t.Fatalf("the original is held by its copy: %v", item)
		}
	}
	if status, body := f.spawnSession(t, "fork-2", map[string]any{"resume_session": sessionA, "resume_mode": "fork", "role_key": "reviewer", "new_room": true}); status != 201 {
		t.Fatalf("a second copy: %d %v", status, body)
	}
	// Asking for the default and leaving it out are one request.
	if status, again := f.spawnSession(t, "fork-1", map[string]any{"resume_session": sessionA, "resume_mode": "fork", "new_room": true}); status != 200 && status != 201 || again["id"] != agentID {
		t.Fatalf("the same request with the default spelled out: %d %v", status, again)
	}
}

func TestSpawnResumeModeRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	fake := f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "x"))
	if status, body := f.spawnSession(t, "bad-mode", map[string]any{"resume_session": sessionA, "resume_mode": "mirror"}); status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("an unknown mode: %d %v", status, body)
	}
	if status, body := f.spawnSession(t, "mode-alone", map[string]any{"resume_mode": "fork"}); status != 400 || body["error"] != "invalid_request" {
		t.Fatalf("a mode without a session: %d %v", status, body)
	}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID].supportsSessionFork = false
	f.srv.machineMu.Unlock()
	if status, body := f.spawnSession(t, "old-fork", map[string]any{"resume_session": sessionA, "resume_mode": "fork"}); status != 409 || body["error"] != "supervisor_update_needed" {
		t.Fatalf("a supervisor that cannot fork: %d %v", status, body)
	}
	// It can still continue, which does not need the flag.
	if status, body := f.spawnSession(t, "old-continue", map[string]any{"resume_session": sessionA, "resume_mode": "continue"}); status != 201 {
		t.Fatalf("continuing through a supervisor that cannot fork: %d %v", status, body)
	}
	_ = fake
}

// A containerized launcher that does not say its Claude home is the host's keeps its own sessions, so a session the machine has is neither
// continued nor copied through it: the fork needs the native launcher too.
func TestSpawnResumeThroughADockerLauncherNeedsNative(t *testing.T) {
	t.Setenv("AIMEBU2_BIND", "0.0.0.0") // a loopback bind refuses Docker before anything else is looked at
	f := newFixture(t)
	f.seed()
	f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "x"))
	f.srv.machineMu.Lock()
	machine := f.srv.machines[f.machineID]
	machine.capabilities = append(machine.capabilities, protocol.CapabilityReport{Tool: "claude", Environment: adapter.Environment{Variant: "docker"}})
	f.srv.machineMu.Unlock()
	for _, mode := range []string{"fork", "continue"} {
		if status, body := f.spawnSession(t, "docker-"+mode, map[string]any{"resume_session": sessionA, "resume_mode": mode, "variant": "docker"}); status != 409 || body["error"] != "needs_native" {
			t.Fatalf("%s through claude-docker: %d %v", mode, status, body)
		}
	}
}

// dockerClaude is a fixture whose machine reports a Docker Claude launcher with the given features and environment references, and a session
// to continue through it. Tests using it cannot run in parallel: a loopback bind refuses Docker before anything else is looked at.
func dockerClaude(t *testing.T, features []string, refs map[string]string) (*fixture, store.Launcher) {
	t.Helper()
	t.Setenv("AIMEBU2_BIND", "0.0.0.0")
	f := newFixture(t)
	f.seed()
	fake := f.connectSupervisor(t, true, candidate(sessionA, f.project.Folder, "Shared home"))
	_ = fake
	launcher := f.harness(t, "claude", "harness-docker")
	launcher.Enabled = true
	if refs != nil {
		launcher.EnvRefs = refs
	}
	launcher, err := f.db.SaveLauncher(t.Context(), "owner", launcher)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.machineMu.Lock()
	machine := f.srv.machines[f.machineID]
	machine.supportsLaunchers = true
	machine.launchers = []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: launcher.ID, Tool: "claude", Containerized: true, Enabled: true}, Available: true, Features: features}}
	f.srv.machineMu.Unlock()
	return f, launcher
}

func TestSessionsAreContinuedThroughADockerLauncherThatSharesTheClaudeHome(t *testing.T) {
	for _, mode := range []string{"continue", "fork"} {
		t.Run(mode, func(t *testing.T) {
			f, launcher := dockerClaude(t, []string{"forward_env", "claude_home_shared"}, nil)
			status, card := f.spawnSession(t, "shared-"+mode, map[string]any{"resume_session": sessionA, "resume_mode": mode, "launcher_id": launcher.ID})
			if status != 201 {
				t.Fatalf("%s through a launcher that shares the Claude home: %d %v", mode, status, card)
			}
			if card["launcher_id"] != launcher.ID && card["variant"] != "harness-docker" && card["variant"] != "docker" {
				t.Fatalf("the agent is not on the Docker launcher: %v", card)
			}
		})
	}
	// The listing reads the same home through it.
	f, launcher := dockerClaude(t, []string{"claude_home_shared"}, nil)
	if status, body := f.listSessions(t, "?launcher_id="+launcher.ID); status != 200 {
		t.Fatalf("listing through a launcher that shares the home: %d %v", status, body)
	}
}

func TestSessionsThroughADockerLauncherSayWhyTheyAreRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		features []string
		refs     map[string]string
		message  string
	}{
		"an old launcher with no features":  {nil, nil, "claude_home_shared"},
		"a launcher that forwards only env": {[]string{"forward_env"}, nil, "update harness-docker"},
		"a remapped Claude home":            {[]string{"claude_home_shared"}, map[string]string{"CLAUDE_CONFIG_DIR": "ALT_CLAUDE_HOME"}, "remaps CLAUDE_CONFIG_DIR"},
	} {
		t.Run(name, func(t *testing.T) {
			f, launcher := dockerClaude(t, tc.features, tc.refs)
			status, body := f.spawnSession(t, "refused", map[string]any{"resume_session": sessionA, "resume_mode": "continue", "launcher_id": launcher.ID})
			message, _ := body["message"].(string)
			if status != 409 || body["error"] != "needs_native" || !strings.Contains(message, tc.message) {
				t.Fatalf("%d %v, want needs_native saying %q", status, body, tc.message)
			}
			if status, body := f.listSessions(t, "?launcher_id="+launcher.ID); status != 409 || body["error"] != "needs_native" {
				t.Fatalf("listing: %d %v", status, body)
			}
		})
	}
}

// A session Claude saved under a link to the project folder is found only when Claude is started in the link as spelled. The agent keeps the
// canonical checkout (every boundary and launcher rule is about it), and its turns start in the session's own spelling, for a copy as for the
// original. A machine whose supervisor does not know that is refused before anything is created.
func TestSpawnContinuesASessionMadeUnderALink(t *testing.T) {
	t.Parallel()
	for name, mode := range map[string]string{"continue": "continue", "copy": "fork", "old supervisor": "fork"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t) // one agent writes in a checkout, so each case has its own
			f.seed()
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(f.project.Folder, link); err != nil {
				t.Fatal(err)
			}
			f.connectSupervisor(t, true, candidate(sessionC, link, "Linked folder"))
			canonical, err := filepath.EvalSymlinks(f.project.Folder)
			if err != nil {
				t.Fatal(err)
			}
			if name == "old supervisor" {
				// A supervisor that predates the spelling would start Claude where the session cannot be found.
				f.srv.machineMu.Lock()
				f.srv.machines[f.machineID].supportsLaunchPath = false
				f.srv.machineMu.Unlock()
				if status, body := f.spawnSession(t, "link-old", map[string]any{"resume_session": sessionC, "resume_mode": mode}); status != 409 || body["error"] != "supervisor_update_needed" {
					t.Fatalf("an old supervisor: %d %v", status, body)
				}
				return
			}
			status, card := f.spawnSession(t, "link-"+name, map[string]any{"resume_session": sessionC, "resume_mode": mode, "confirm_session_closed": true})
			if status != 201 {
				t.Fatalf("%d %v", status, card)
			}
			agentID := card["id"].(string)
			imported, ok, err := f.db.AgentImportedSession(t.Context(), agentID)
			if err != nil || !ok || imported.CWD != link {
				t.Fatalf("the session's own spelling was not kept: %+v %v", imported, err)
			}
			profile, err := f.db.AgentProfile(t.Context(), f.room.ID, agentID)
			if err != nil || profile.Cwd != canonical {
				t.Fatalf("the checkout is not the canonical folder: %+v %v", profile, err)
			}
			if pending, err := f.db.AgentLaunchPathPending(t.Context(), agentID); err != nil || !pending {
				t.Fatalf("the turn will not start in the spelling: %v %v", pending, err)
			}
		})
	}
}

func TestSameDirectoryTellsALinkFromADifferentFolder(t *testing.T) {
	t.Parallel()
	host := store.Machine{IsHost: true}
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		cwd, canonical string
		machine        store.Machine
		want           bool
	}{
		"a link to the folder":       {link, real, host, true},
		"the folder itself":          {real, real, host, true},
		"a link to another folder":   {link, other, host, false},
		"a folder that is gone":      {filepath.Join(real, "gone"), real, host, false},
		"a relative spelling":        {"link", real, host, false},
		"an unclean spelling":        {link + "/../link", real, host, false},
		"another machine's own path": {"/home/me/link", "/data/real", store.Machine{}, true},
	} {
		if got := sameDirectory(tc.machine, tc.cwd, tc.canonical); got != tc.want {
			t.Errorf("%s: sameDirectory = %v, want %v", name, got, tc.want)
		}
	}
}
