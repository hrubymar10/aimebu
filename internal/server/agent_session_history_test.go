package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

func historyOf(t *testing.T, s *store, id string) []types.AgentSessionHistoryEntry {
	t.Helper()
	return s.listAgentSessionHistory(id)
}

func pushSession(t *testing.T, s *store, agent *types.Agent, id, cwd string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.upsertAgentSessionLocked(s.agents[agent.ID], agentSessionOriginWrapper, &types.AgentSession{HarnessSessionID: id, ResumeCommand: "aimebu agent --resume-id " + id, CWD: cwd}); err != nil {
		t.Fatal(err)
	}
}

// A new instance of the same name is a new session: it appends to the history, and agent_sessions still holds only the latest.
func TestAgentSessionHistoryAppendsInsteadOfOverwriting(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agent, _, err := s.registerAI("opus5.5", "claude-code", "proj", map[string]string{"cwd": "/work"}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	pushSession(t, s, agent, "session-1", "/work")
	pushSession(t, s, agent, "session-1", "/work") // the wrapper pushes the same session again after bootstrap
	if got := historyOf(t, s, agent.ID); len(got) != 1 || got[0].HarnessSessionID != "session-1" || got[0].Harness != "claude-code" || got[0].Model != "opus5.5" || got[0].Project != "proj" {
		t.Fatalf("history after a repeated push = %+v, want one session-1 row", got)
	}
	first := historyOf(t, s, agent.ID)[0]
	time.Sleep(5 * time.Millisecond)
	pushSession(t, s, agent, "session-2", "/work")
	got := historyOf(t, s, agent.ID)
	if len(got) != 2 || got[0].HarnessSessionID != "session-2" || got[1].HarnessSessionID != "session-1" {
		t.Fatalf("history = %+v, want session-2 then session-1", got)
	}
	if got[1].ID != first.ID || !got[1].RegisteredAt.Equal(first.RegisteredAt) {
		t.Fatalf("the first row was rewritten: %+v vs %+v", got[1], first)
	}
	if latest := s.listAgentSessions(); len(latest) != 1 || latest[0].HarnessSessionID != "session-2" {
		t.Fatalf("agent_sessions = %+v, want only the latest (session-2)", latest)
	}
	// Resuming the older session updates its row and makes it the most recent, without a third row.
	time.Sleep(5 * time.Millisecond)
	pushSession(t, s, agent, "session-1", "/work")
	if got := historyOf(t, s, agent.ID); len(got) != 2 || got[0].HarnessSessionID != "session-1" {
		t.Fatalf("history after resuming session-1 = %+v", got)
	}
	// A hint with no session id has nothing to key a row by.
	s.mu.Lock()
	err = s.upsertAgentSessionLocked(s.agents[agent.ID], agentSessionOriginMCP, &types.AgentSession{ResumeCommand: "claude --continue"})
	s.mu.Unlock()
	if err != nil || len(historyOf(t, s, agent.ID)) != 2 {
		t.Fatalf("a hint without a session id changed the history: %v", err)
	}
	// The history survives a restart with its ids.
	rows := historyOf(t, s, agent.ID)
	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := historyOf(t, reopened, agent.ID); len(got) != 2 || got[0].ID != rows[0].ID || got[0].HarnessSessionID != rows[0].HarnessSessionID || got[1].ID != rows[1].ID {
		t.Fatalf("history after restart = %+v, want %+v", got, rows)
	}
}

// The cwd is the harness's own spelling: harnesses key their session files by it, so a symlinked folder must not be resolved.
func TestAgentSessionHistoryKeepsTheCWDVerbatim(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, "proj")
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agent, _, err := s.registerAI("sonnet4.6", "claude-code", "proj", nil, "beta")
	if err != nil {
		t.Fatal(err)
	}
	pushSession(t, s, agent, "session-link", linked)
	if got := historyOf(t, s, agent.ID); len(got) != 1 || got[0].CWD != linked {
		t.Fatalf("history cwd = %q, want the unresolved %q", got[0].CWD, linked)
	}
	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := historyOf(t, reopened, agent.ID); len(got) != 1 || got[0].CWD != linked {
		t.Fatalf("history cwd after restart = %+v, want %q", got, linked)
	}
}

// A database from before the history has each identity's latest session only; it becomes the first history row.
func TestAgentSessionHistoryBackfillsAnExistingDatabase(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := s.registerAIWithSession("gpt5", "codex", "proj", map[string]string{"cwd": "/work"}, "gamma", &types.AgentSession{HarnessSessionID: "thread-old", CWD: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE agent_session_history`); err != nil { // the shape of a database written by a build without the table
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := historyOf(t, reopened, agent.ID); len(got) != 1 || got[0].HarnessSessionID != "thread-old" || got[0].CWD != "/work" || got[0].Origin != agentSessionOriginMCP || got[0].RegisteredAt.IsZero() {
		t.Fatalf("backfilled history = %+v", got)
	}
	again, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := historyOf(t, again, agent.ID); len(got) != 1 {
		t.Fatalf("a second start duplicated the backfill: %+v", got)
	}
}

func TestAgentSessionHistoryHTTP(t *testing.T) {
	_, srv := setupTestServer(t)
	postJSONForTest(t, srv, "/agents", map[string]any{"kind": "ai", "name": "alpha", "force": true, "model": "opus5.5", "harness": "claude-code", "project": "proj-a"})
	for _, id := range []string{"sess-a", "sess-b"} {
		postJSONForTest(t, srv, "/agents/alpha@proj-a/session", map[string]any{"harness_session_id": id, "resume_command": "claude --resume " + id, "cwd": "/repo/a"})
		time.Sleep(5 * time.Millisecond)
	}
	var listed struct {
		AgentID  string                           `json:"agent_id"`
		Sessions []types.AgentSessionHistoryEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agents/alpha@proj-a/sessions")), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.AgentID != "alpha@proj-a" || len(listed.Sessions) != 2 || listed.Sessions[0].HarnessSessionID != "sess-b" || listed.Sessions[1].HarnessSessionID != "sess-a" ||
		listed.Sessions[0].CWD != "/repo/a" || listed.Sessions[0].Model != "opus5.5" || listed.Sessions[0].Project != "proj-a" || listed.Sessions[0].Origin != "wrapper" {
		t.Fatalf("sessions = %+v, want sess-b then sess-a with cwd, model and project", listed)
	}
	if err := json.Unmarshal([]byte(getForTest(t, srv, "/agents/nobody@proj-a/sessions")), &listed); err != nil || len(listed.Sessions) != 0 {
		t.Fatalf("unknown agent: %+v %v", listed, err)
	}
}

// A message carries the sender's session at send time: humans, system messages and senders with no known session carry none.
func TestMessageFromSession(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	withSession, _, err := s.registerAIWithSession("gpt5", "codex", "proj", nil, "delta", &types.AgentSession{HarnessSessionID: "thread-d"})
	if err != nil {
		t.Fatal(err)
	}
	noSession, _, err := s.registerAI("gpt5", "codex", "proj", nil, "echo")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{withSession.ID, noSession.ID, human.ID} {
		if _, err := s.joinRoom("general", id); err != nil {
			t.Fatal(err)
		}
	}
	send := func(from string) types.Message {
		t.Helper()
		id, err := s.roomSend("general", from, "hello", false, nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range s.messages["general"] {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("message %d not found", id)
		return types.Message{}
	}
	if m := send(withSession.ID); m.FromSession != "thread-d" || m.FromHarness != "codex" {
		t.Fatalf("AI sender: %+v", m)
	}
	if m := send(noSession.ID); m.FromSession != "" {
		t.Fatalf("AI sender without a session: from_session = %q", m.FromSession)
	}
	if m := send(human.ID); m.FromSession != "" {
		t.Fatalf("human sender: from_session = %q", m.FromSession)
	}
	// The stamp is the session at send time: a later session does not rewrite it.
	pushSession(t, s, withSession, "thread-e", "/work")
	if m := send(withSession.ID); m.FromSession != "thread-e" {
		t.Fatalf("after a new session: %+v", m)
	}
	firstFrom := func(msgs []types.Message) types.Message {
		for _, m := range msgs {
			if m.From == withSession.ID {
				return m
			}
		}
		return types.Message{}
	}
	if first := firstFrom(s.messages["general"]); first.FromSession != "thread-d" {
		t.Fatalf("an earlier message was rewritten: %+v", first)
	}
	// The stamp is part of the persisted message.
	reopened, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := firstFrom(reopened.messages["general"]); got.FromSession != "thread-d" {
		t.Fatalf("from_session lost on reload: %+v", got)
	}
}

// Sends read the sender's session while pushes replace it and add history rows: nothing may race.
func TestAgentSessionHistoryConcurrentSendsAndPushes(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agent, _, err := s.registerAIWithSession("gpt5", "codex", "proj", nil, "zeta", &types.AgentSession{HarnessSessionID: "thread-0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := s.roomSend("general", agent.ID, "hi", false, nil, nil, nil, 0); err != nil {
					t.Error(err)
					return
				}
				s.listAgentSessionHistory(agent.ID)
			}
		}()
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				s.mu.Lock()
				err := s.upsertAgentSessionLocked(s.agents[agent.ID], agentSessionOriginWrapper, &types.AgentSession{HarnessSessionID: "thread-" + string(rune('a'+w)) + string(rune('a'+i%5))})
				s.touchAgentSessionLocked(agent.ID)
				s.mu.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if got := historyOf(t, s, agent.ID); len(got) != 1+4*5 {
		t.Fatalf("history rows = %d, want %d", len(got), 1+4*5)
	}
}

// A name taken over by a new instance still has the previous holder's session record until the new one pushes its own. That record must
// not be stamped on the new holder's messages, nor kept fresh by its liveness; the new holder's own session is attributed from its push.
func TestFromSessionIgnoresThePreviousHoldersSession(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _, err := s.registerAIWithSession("opus5.5", "claude-code", "proj", nil, "alpha", &types.AgentSession{HarnessSessionID: "old-session"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.deregisterAgent(first.ID) {
		t.Fatal("deregister failed")
	}
	// The takeover follows at once, within the same second: registrations are told apart by id, not by time.
	second, _, err := s.registerAI("mistral-medium", "vibe", "proj", nil, "alpha") // no session hint
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("the name was not reused: %s vs %s", second.ID, first.ID)
	}
	oldRow := historyOf(t, s, second.ID)[0]
	if _, err := s.joinRoom("general", second.ID); err != nil {
		t.Fatal(err)
	}
	send := func() types.Message {
		t.Helper()
		id, err := s.roomSend("general", second.ID, "hello", false, nil, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range s.messages["general"] {
			if m.ID == id {
				return m
			}
		}
		t.Fatalf("message %d not found", id)
		return types.Message{}
	}
	if m := send(); m.FromSession != "" || m.FromHarness != "vibe" {
		t.Fatalf("the new holder's message = %+v, want no from_session (the old session is the previous holder's)", m)
	}
	s.mu.Lock()
	before := s.agentSessions[second.ID].LastSeen
	s.touchAgentSessionLocked(second.ID)
	after := s.agentSessions[second.ID].LastSeen
	s.mu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("liveness refreshed the previous holder's session: last_seen %s -> %s", before, after)
	}
	// The old session stays in the history, untouched.
	if got := historyOf(t, s, second.ID); len(got) != 1 || got[0].HarnessSessionID != "old-session" || !got[0].LastSeen.Equal(oldRow.LastSeen) {
		t.Fatalf("history = %+v", got)
	}
	// Once the new holder pushes its own session, that one is attributed.
	pushSession(t, s, second, "new-session", "/work")
	if m := send(); m.FromSession != "new-session" {
		t.Fatalf("after its own push: %+v", m)
	}
	if got := historyOf(t, s, second.ID); len(got) != 2 || got[0].HarnessSessionID != "new-session" {
		t.Fatalf("history after the push = %+v", got)
	}
}

// Records written before registrations had ids carry none, on both sides: they match, so a session pushed before an upgrade is still the
// sender's.
func TestFromSessionOfRecordsFromBeforeRegistrationIDs(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	agent, _, err := s.registerAIWithSession("gpt5", "codex", "proj", nil, "eta", &types.AgentSession{HarnessSessionID: "thread-legacy"})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.agents[agent.ID].RegistrationID = ""
	s.agentSessions[agent.ID].RegistrationID = ""
	s.mu.Unlock()
	if _, err := s.joinRoom("general", agent.ID); err != nil {
		t.Fatal(err)
	}
	id, err := s.roomSend("general", agent.ID, "hello", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.messages["general"] {
		if m.ID == id && m.FromSession != "thread-legacy" {
			t.Fatalf("legacy record not attributed: %+v", m)
		}
	}
}

// Only an AI has a harness session: the endpoint refuses to record one for a person, and a person's message never carries one even when a
// record shares the name.
func TestOnlyAIAgentsHaveSessions(t *testing.T) {
	_, srv := setupTestServer(t)
	postJSONForTest(t, srv, "/agents", map[string]any{"kind": "human", "name": "hank"})
	resp, err := http.Post(srv.URL+"/agents/hank/session", "application/json", strings.NewReader(`{"harness_session_id":"thread-human","resume_command":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a session for a human: status %d, want 400", resp.StatusCode)
	}
	if got := getForTest(t, srv, "/agents/hank/sessions"); strings.Contains(got, "thread-human") {
		t.Fatalf("a human's session was recorded: %s", got)
	}

	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	human, err := s.registerHuman("hank", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.agentSessions[human.ID] = &types.AgentSession{FullID: human.ID, HarnessSessionID: "stray-record", RegistrationID: human.RegistrationID, LastSeen: time.Now().UTC()}
	s.mu.Unlock()
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	id, err := s.roomSend("general", human.ID, "hello", false, nil, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.messages["general"] {
		if m.ID == id && m.FromSession != "" {
			t.Fatalf("a person's message carries a session: %+v", m)
		}
	}
}

// A new holder of a name starts its own session record: the previous holder's registration time is not inherited.
func TestSessionRecordOfANewHolderStartsAtItsOwnRegistration(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _, err := s.registerAIWithSession("opus5.5", "claude-code", "proj", nil, "theta", &types.AgentSession{HarnessSessionID: "old-session"})
	if err != nil {
		t.Fatal(err)
	}
	oldAt := s.listAgentSessions()[0].RegisteredAt
	if !s.deregisterAgent(first.ID) {
		t.Fatal("deregister failed")
	}
	time.Sleep(5 * time.Millisecond)
	second, _, err := s.registerAIWithSession("gpt5", "codex", "proj", nil, "theta", &types.AgentSession{HarnessSessionID: "new-session"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.listAgentSessions()
	if len(got) != 1 || got[0].HarnessSessionID != "new-session" || !got[0].RegisteredAt.After(oldAt) {
		t.Fatalf("the new holder's record = %+v, want its own registration time after %s", got, oldAt)
	}
	// The same registration pushing again keeps its own start.
	startedAt := got[0].RegisteredAt
	time.Sleep(5 * time.Millisecond)
	pushSession(t, s, second, "newer-session", "/work")
	if again := s.listAgentSessions(); len(again) != 1 || !again[0].RegisteredAt.Equal(startedAt) {
		t.Fatalf("a push by the same registration moved its start: %+v", again)
	}
}
