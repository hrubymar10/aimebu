package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// liveLogin is a host login source a test moves, standing in for the switcher's active profile.
type liveLogin struct {
	mu    sync.Mutex
	login string
}

func (l *liveLogin) set(login string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.login = login
}

func (l *liveLogin) get(tool string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if tool != "codex" {
		return ""
	}
	return l.login
}

// loginFixture is recoveryFixture with the host's codex login A live when the turn is admitted.
func loginFixture(t *testing.T) (*Store, executionFixture, string, *liveLogin) {
	t.Helper()
	s, _ := testStore(t, Options{})
	live := &liveLogin{login: "A"}
	s.SetHostLogin(live.get)
	f := seedExecution(t, s)
	ctx := context.Background()
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_recovery','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_recovery','owner')`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: f.turn.ID, AgentID: f.agent.ID, MachineID: f.agent.MachineID, NoteIDs: []string{f.note.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, f.turn.ID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	var login string
	if err := s.db.QueryRow(`SELECT COALESCE(login,'') FROM turn WHERE id=?`, f.turn.ID).Scan(&login); err != nil || login != "A" {
		t.Fatalf("the turn was admitted on login %q (%v), want A", login, err)
	}
	return s, f, "usr_recovery", live
}

func usageLimitExit() adapter.ExitInfo {
	return adapter.ExitInfo{ExitCode: 1, ErrorEvent: `{"type":"turn.failed","message":"usage limit reached; resets in 3 hours"}`}
}

// A turn admitted on login A that meets A's usage limit after a switch installed B (the switch does not wait for it) does not pause the agent,
// does not mark B limited and raises no notice: its notes go back to the queue and the next turn runs on B.
func TestAProviderLimitMetOnTheOldLoginAfterASwitchDoesNotPauseTheAgent(t *testing.T) {
	t.Parallel()
	s, f, owner, live := loginFixture(t)
	ctx := context.Background()
	live.set("B")
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, usageLimitExit()); err != nil {
		t.Fatal(err)
	}
	if limited, err := s.AgentProviderLimited(ctx, f.agent.ID); err != nil || limited {
		t.Fatalf("the old login's limit paused the agent: %v %v", limited, err)
	}
	if limited, err := s.ToolProviderLimited(ctx, f.agent.MachineID, "codex"); err != nil || limited {
		t.Fatalf("the old login's limit marks the new login limited: %v %v", limited, err)
	}
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
		t.Fatalf("inbox=%+v error=%v", items, err)
	}
	var noteState, outcome string
	if err := s.db.QueryRow(`SELECT n.state,t.outcome FROM note n, turn t WHERE n.id=? AND t.id=?`, f.note.ID, f.turn.ID).Scan(&noteState, &outcome); err != nil {
		t.Fatal(err)
	}
	if noteState != "queued" || outcome != "failed" {
		t.Fatalf("note=%s turn=%s: the request was not queued again", noteState, outcome)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || !ready {
		t.Fatalf("the agent was not dispatched on the new login: ready=%t error=%v", ready, err)
	}
}

// A limit met on the login still live pauses the agent and marks the login limited, as before. When a switch installs another login and the limit
// was not cleared with it (it was recorded as the switch committed), it stops counting: the new login is not limited, and the agent's next dispatch
// lifts it with the ordinary clear.
func TestAProviderLimitCountsOnlyAgainstTheLoginItWasMetOn(t *testing.T) {
	t.Parallel()
	s, f, owner, live := loginFixture(t)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, usageLimitExit()); err != nil {
		t.Fatal(err)
	}
	var login string
	if err := s.db.QueryRow(`SELECT COALESCE(login,'') FROM provider_limit WHERE agent_id=?`, f.agent.ID).Scan(&login); err != nil || login != "A" {
		t.Fatalf("limit login=%q error=%v", login, err)
	}
	if limited, err := s.ToolProviderLimited(ctx, f.agent.MachineID, "codex"); err != nil || !limited {
		t.Fatalf("A's own limit does not mark A limited: %v %v", limited, err)
	}
	if agents, err := s.ProviderLimitedAgents(ctx, f.agent.MachineID); err != nil || len(agents) != 1 {
		t.Fatalf("limited agents=%v error=%v", agents, err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: f.room.ID, AuthorKind: "user", AuthorID: owner, Body: "new request"}); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || ready {
		t.Fatalf("the agent is not paused on its own login's limit: ready=%t error=%v", ready, err)
	}

	live.set("B")
	if limited, err := s.ToolProviderLimited(ctx, f.agent.MachineID, "codex"); err != nil || limited {
		t.Fatalf("A's limit marks B limited: %v %v", limited, err)
	}
	if agents, err := s.ProviderLimitedAgents(ctx, f.agent.MachineID); err != nil || len(agents) != 0 {
		t.Fatalf("limited agents on B=%v error=%v", agents, err)
	}
	if _, ready, err := s.PrepareDispatch(ctx, f.agent.ID, ""); err != nil || !ready {
		t.Fatalf("the agent stays paused on B: ready=%t error=%v", ready, err)
	}
	if limited, err := s.AgentProviderLimited(ctx, f.agent.ID); err != nil || limited {
		t.Fatalf("the dispatch did not lift A's limit: %v %v", limited, err)
	}
	if items, err := s.InboxForUser(ctx, owner); err != nil || len(items) != 0 {
		t.Fatalf("the notice of A's limit was not resolved: %+v %v", items, err)
	}
}

// A limit with no login recorded (written before logins were, or with no switcher profile) counts against whatever login is live, as before; the
// background advance lifts one met on a login that is gone.
func TestALimitWithNoLoginCountsAsBeforeAndTheAdvanceLiftsAGoneLogin(t *testing.T) {
	t.Parallel()
	s, f, _, live := loginFixture(t)
	ctx := context.Background()
	if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, 0, usageLimitExit()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE provider_limit SET login=NULL WHERE agent_id=?`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	live.set("B")
	if limited, err := s.ToolProviderLimited(ctx, f.agent.MachineID, "codex"); err != nil || !limited {
		t.Fatalf("a limit with no login does not count: %v %v", limited, err)
	}
	if err := s.AdvanceProviderLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if limited, err := s.AgentProviderLimited(ctx, f.agent.ID); err != nil || !limited {
		t.Fatalf("the advance lifted a limit with no login: %v %v", limited, err)
	}
	if _, err := s.db.Exec(`UPDATE provider_limit SET login='A', reset_at=? WHERE agent_id=?`, time.Now().Add(time.Hour).UTC().Format(timestampLayout), f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceProviderLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if limited, err := s.AgentProviderLimited(ctx, f.agent.ID); err != nil || limited {
		t.Fatalf("the advance kept a limit of a login that is gone: %v %v", limited, err)
	}
}
