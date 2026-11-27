package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// Two supervisor builds are one binary under two fake version strings (AIMEBU_TEST_VERSION): the server of the second build finds the
// supervisor of the first and starts a second instance next to it instead of replacing it.
func init() {
	if v := os.Getenv("AIMEBU_TEST_VERSION"); v != "" {
		version = v
	}
}

// sideBySide is a running server of build B with a supervisor instance of build A that is busy: a turn of agent "old" held by a gate and a native
// run_command job of the same agent, and an instance of build B started next to it.
type sideBySide struct {
	t                      *testing.T
	root                   string
	port                   int
	db                     *store.Store
	probe                  *sql.DB
	oldAgent, newAgent     string
	roomID                 string
	oldPID                 int
	oldJournal             string
	turnGate, jobGate      string
	newGate                string
	oldMessage, newMessage string
	jobID                  string
}

func (s *sideBySide) query(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.probe.QueryRow(query, args...).Scan(&n); err != nil {
		return -1
	}
	return n
}

func (s *sideBySide) waitFor(what string, limit time.Duration, ok func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %s; server: %s; supervisor: %s", what, tailOfFile(serverLogPath(s.root), 40), tailOfFile(filepath.Join(s.root, "supervisor", "aimebu.log"), 25))
}

func (s *sideBySide) release(path string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte("go"), 0600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sideBySide) instances() []instanceView {
	held, err := heldSupervisors(s.root)
	if err != nil {
		s.t.Fatal(err)
	}
	return held
}

func (s *sideBySide) newAnswered(agent, messageID string) bool {
	return s.query(`SELECT COUNT(*) FROM message WHERE room_id=? AND author_kind='agent' AND author_id=? AND reply_to=?`, s.roomID, agent, messageID) == 1
}

// startSideBySide brings the scene up to the point where both instances run. The turn and the job of agent "old" are still going.
func startSideBySide(t *testing.T) *sideBySide {
	s := &sideBySide{t: t}
	root, port := daemonFixture(t)
	s.root, s.port = root, port
	t.Setenv("AIMEBU_WEB_BUNDLE", "off")
	previousStartLimit := daemonStartLimit
	daemonStartLimit = 90 * time.Second
	t.Cleanup(func() { daemonStartLimit = previousStartLimit })
	t.Setenv("AIMEBU_ALLOW", "192.0.2.0/24")
	scratch := t.TempDir()
	s.turnGate, s.jobGate, s.newGate = filepath.Join(scratch, "turn-gate"), filepath.Join(scratch, "job-gate"), filepath.Join(scratch, "new-gate")
	t.Setenv("AIMEBU_TEST_FAKE_GATE", s.turnGate)
	t.Setenv("AIMEBU_TEST_VERSION", "build-A")
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start without a supervisor: %d %q %q", code, out, errOut)
	}
	if code, out, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop before seeding: %d %q %q", code, out, errOut)
	}
	credential, err := readHostMachineKey(filepath.Join(root, "server", "host-machine.key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(root, "server", "aimebu.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s.db = db
	ctx := context.Background()
	project, err := db.CreateProject(ctx, store.Project{Name: "sbs", Folder: root})
	if err != nil {
		t.Fatal(err)
	}
	room, err := db.CreateRoom(ctx, store.Room{ProjectID: project.ID, Kind: "task", Title: "sbs", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	s.roomID = room.ID
	if err := db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_sbs',?,?,?)`, project.ID, credential.MachineID, root)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var agents []store.Agent
	for _, slug := range []string{"oldwork", "newwork"} {
		agent, err := db.CreateAgent(ctx, store.Agent{Slug: slug, ProjectID: project.ID, RoleID: "role_reviewer_default", Tool: "fake", Variant: "test", Model: "test", Provider: "fake", MachineID: credential.MachineID, CheckoutID: "chk_sbs", KeyHash: "hash", CreatedBy: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddMember(ctx, room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if err := db.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,0,'fresh_with_history',?)`, "ses_"+slug, agent.ID, time.Now().UTC().Format(time.RFC3339Nano))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		agents = append(agents, agent)
	}
	s.oldAgent, s.newAgent = agents[0].ID, agents[1].ID
	message, err := insertMessageRetryingBusy(db, store.Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "owner", Body: "@oldwork finish on the old instance"})
	if err != nil {
		t.Fatal(err)
	}
	s.oldMessage = message.ID
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "1")
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start build A: %d %q %q", code, out, errOut)
	}
	t.Cleanup(func() {
		held, _ := heldSupervisors(root)
		for _, v := range held {
			_ = syscall.Kill(v.pid, syscall.SIGKILL)
		}
	})
	probe, err := sql.Open("sqlite", "file:"+filepath.Join(root, "server", "aimebu.sqlite")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { probe.Close() })
	s.probe = probe
	s.waitFor("the turn of the old build to run", 60*time.Second, func() bool {
		return s.query(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, s.oldAgent) > 0
	})
	held := s.instances()
	if len(held) != 1 {
		t.Fatalf("one instance of build A expected, got %d", len(held))
	}
	s.oldPID, s.oldJournal = held[0].pid, held[0].path
	// A native job of the same agent, started the way its toolbox starts it: an HTTP call with the agent's key.
	key, err := auth.NewService(db).IssueAgentKey(ctx, s.oldAgent)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"cmd": "while [ ! -f " + s.jobGate + " ]; do sleep 0.1; done; echo job-finished", "room_id": s.roomID, "label": "long"})
	request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/agent/commands", port), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Idempotency-Key", "sbs-job-1")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("run_command: %d %s", response.StatusCode, raw)
	}
	var job struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &job)
	s.jobID = job.ID
	// The new build: restart the server, whose supervisor of build A no longer matches.
	if code, out, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop: %d %q %q", code, out, errOut)
	}
	t.Setenv("AIMEBU_TEST_VERSION", "build-B")
	t.Setenv("AIMEBU_TEST_FAKE_GATE", s.newGate)
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start build B: %d %q %q", code, out, errOut)
	}
	s.waitFor("a second instance next to the first", 60*time.Second, func() bool { return len(s.instances()) == 2 })
	if pid, held, err := journalHolderPID(s.oldJournal); err != nil || !held || pid != s.oldPID {
		t.Fatalf("the old instance was replaced instead of left alone: pid=%d held=%t err=%v", pid, held, err)
	}
	return s
}

// dispatchNew sends a message to the other agent and waits until its turn runs, on the new instance.
func (s *sideBySide) dispatchNew() {
	s.t.Helper()
	message, err := insertMessageRetryingBusy(s.db, store.Message{RoomID: s.roomID, AuthorKind: "user", AuthorID: "owner", Body: "@newwork start on the new instance"})
	if err != nil {
		s.t.Fatal(err)
	}
	s.newMessage = message.ID
	s.waitFor("the turn of the second agent to run on the new instance", 60*time.Second, func() bool {
		return s.query(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, s.newAgent) > 0
	})
	var owner sql.NullString
	if err := s.probe.QueryRow(`SELECT journal_id FROM turn WHERE agent_id=? ORDER BY rowid DESC LIMIT 1`, s.newAgent).Scan(&owner); err != nil {
		s.t.Fatal(err)
	}
	var oldOwner sql.NullString
	if err := s.probe.QueryRow(`SELECT journal_id FROM turn WHERE agent_id=? ORDER BY rowid DESC LIMIT 1`, s.oldAgent).Scan(&oldOwner); err != nil {
		s.t.Fatal(err)
	}
	if !owner.Valid || owner.String == "" || owner.String == oldOwner.String {
		s.t.Fatalf("the new turn is owned by the old instance: new=%q old=%q", owner.String, oldOwner.String)
	}
}

func (s *sideBySide) journalFilesGone(path string) bool {
	for _, suffix := range []string{"", ".lock"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// The upgrade of the first section of the design: the turn and the native job on the old instance run to their ends, uninterrupted and on
// the old instance, while the new one takes new work; then the old instance leaves by itself with its journal removed.
func TestSideBySideUpgradeLeavesTheOldInstanceItsWork(t *testing.T) {
	s := startSideBySide(t)
	s.dispatchNew()
	s.release(s.newGate)
	s.waitFor("the new instance's turn to be answered", 60*time.Second, func() bool { return s.newAnswered(s.newAgent, s.newMessage) })
	// Meanwhile the old turn and job still run, on the instance that has them.
	if gone(s.oldPID) || s.query(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, s.oldAgent) != 1 {
		t.Fatalf("the old instance's turn was cut by the upgrade")
	}
	if n := s.query(`SELECT COUNT(*) FROM command_job WHERE id=? AND state='running'`, s.jobID); n != 1 {
		t.Fatalf("the native job is not running: %d", n)
	}
	s.release(s.turnGate)
	s.waitFor("the old turn to be answered", 60*time.Second, func() bool { return s.newAnswered(s.oldAgent, s.oldMessage) })
	if gone(s.oldPID) {
		t.Fatal("the old instance left while its job still ran")
	}
	s.release(s.jobGate)
	s.waitFor("the job to end with its own exit code", 60*time.Second, func() bool {
		return s.query(`SELECT COUNT(*) FROM command_job WHERE id=? AND state='done' AND exit_code=0`, s.jobID) == 1
	})
	s.waitFor("the old instance to leave empty", 60*time.Second, func() bool { return gone(s.oldPID) })
	if !s.journalFilesGone(s.oldJournal) {
		t.Fatalf("the old instance left its journal behind")
	}
	// The job's end wakes the agent with a turn of its own; the first turn is the one that ran on the old instance, and it ended by itself.
	if n := s.query(`SELECT COUNT(*) FROM turn WHERE id=(SELECT id FROM turn WHERE agent_id=? ORDER BY rowid LIMIT 1) AND exec='exited' AND outcome='completed'`, s.oldAgent); n != 1 {
		t.Fatalf("the old turn did not complete on its own")
	}
	if n := s.query(`SELECT COUNT(*) FROM event WHERE kind IN ('turn_lost','turn_interrupt_requested','turn_interrupted_continuation') AND turn_id IN (SELECT id FROM turn WHERE agent_id=?)`, s.oldAgent); n != 0 {
		t.Fatalf("the old turn was lost or interrupted: %d events", n)
	}
	if held := s.instances(); len(held) != 1 {
		t.Fatalf("expected the new instance alone, got %d", len(held))
	}
	if !strings.Contains(tailOfFile(serverLogPath(s.root), 80), "started supervisor instance") {
		t.Fatalf("the swap log does not show the new instance starting: %s", tailOfFile(serverLogPath(s.root), 40))
	}
}

// The old instance dies while it holds a turn and a native job. Its journal has open work and its lock is free: a drain instance of the
// current build finishes it, reports the job's real end and leaves, with the journal gone.
func TestSideBySideCrashedOldInstanceIsFinishedByADrainInstance(t *testing.T) {
	s := startSideBySide(t)
	s.dispatchNew()
	if err := syscall.Kill(s.oldPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	s.waitFor("a drain instance on the old journal", 60*time.Second, func() bool {
		return strings.Contains(tailOfFile(serverLogPath(s.root), 120), "started drain instance")
	})
	s.waitFor("the lost turn to be recorded", 60*time.Second, func() bool {
		return s.query(`SELECT COUNT(*) FROM turn WHERE agent_id=? AND exec='running'`, s.oldAgent) == 0
	})
	s.release(s.jobGate)
	s.waitFor("the job's real exit code from the drain instance", 90*time.Second, func() bool {
		return s.query(`SELECT COUNT(*) FROM command_job WHERE id=? AND state='done' AND exit_code=0`, s.jobID) == 1
	})
	s.waitFor("the drain instance to remove the journal and leave", 90*time.Second, func() bool { return s.journalFilesGone(s.oldJournal) })
	s.release(s.newGate)
	s.waitFor("the new instance to answer", 60*time.Second, func() bool { return s.newAnswered(s.newAgent, s.newMessage) })
}

// insertMessageRetryingBusy writes a message through the test's own store while the server (another process) writes the same database.
// The store's write transaction is a deferred one: it reads first (the reply parent, mentions) and then writes, so when the server commits
// in between, SQLite refuses the upgrade at once with SQLITE_BUSY_SNAPSHOT ("database is locked (517)") and the busy timeout does not apply.
// The store has one writer process in production, so the test, as the second writer, retries the whole transaction the way a second writer must.
func insertMessageRetryingBusy(db *store.Store, message store.Message) (store.Message, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		stored, err := db.InsertMessage(context.Background(), message)
		if err == nil || !strings.Contains(err.Error(), "database is locked") || time.Now().After(deadline) {
			return stored, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
