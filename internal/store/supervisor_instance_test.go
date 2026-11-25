package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// instanceFixture is a host machine with two agents, each running one turn and one command on its own supervisor instance.
type instanceFixture struct {
	s                      *Store
	f                      executionFixture
	oldAgent, newAgent     Agent
	oldTurn, newTurn       string
	oldJob, newJob         CommandJob
	oldJournal, newJournal string
}

func seedTurnOn(t *testing.T, s *Store, f executionFixture, slug, journal string, first bool) (Agent, string) {
	t.Helper()
	ctx := context.Background()
	agent, turnID := f.agent, f.turn.ID
	if !first {
		var err error
		agent, err = s.CreateAgent(ctx, Agent{Slug: slug, ProjectID: f.project.ID, RoleID: "role_test", Tool: "codex", Variant: "native", Model: "test",
			Provider: "test", MachineID: "mch_test", CheckoutID: "chk_test", KeyHash: "hash-" + slug, CreatedBy: "usr_test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddMember(ctx, f.room.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,0,'fresh_with_history','now')`, "ses_"+slug, agent.ID); err != nil {
			t.Fatal(err)
		}
		turn, err := s.RecordTurn(ctx, Turn{AgentID: agent.ID, SessionID: "ses_" + slug, Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		turnID = turn.ID
	} else {
		if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO turn_note(turn_id,note_id) VALUES (?,?)`, f.turn.ID, f.note.ID); err != nil {
			t.Fatal(err)
		}
	}
	notes := []string{}
	if first {
		notes = []string{f.note.ID}
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: turnID, AgentID: agent.ID, MachineID: "mch_test", Generation: 0, DesiredRev: 0, NoteIDs: notes, JournalID: journal}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, turnID, agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	return agent, turnID
}

func seedRunningJob(t *testing.T, s *Store, f executionFixture, agent Agent, journal, key string) CommandJob {
	t.Helper()
	ctx := context.Background()
	job, _, err := s.CreateCommandJob(ctx, CommandJob{AgentID: agent.ID, MachineID: "mch_test", JournalID: journal, ProjectID: f.project.ID, RoomID: f.room.ID,
		Cmd: "sleep 1", CheckoutPath: "/tmp/checkout"}, RequestKey{ActorKind: "agent", ActorID: agent.ID, Kind: "command_run", Key: key, Digest: key})
	if err != nil {
		t.Fatal(err)
	}
	if job, err = s.RecordCommandStarted(ctx, job.ID, job.JournalID, timestamp(), "/v/log", "/v"); err != nil {
		t.Fatal(err)
	}
	return job
}

func newInstanceFixture(t *testing.T) instanceFixture {
	t.Helper()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	x := instanceFixture{s: s, f: f, oldJournal: "journal-old", newJournal: "journal-new"}
	x.oldAgent, x.oldTurn = seedTurnOn(t, s, f, "", x.oldJournal, true)
	x.newAgent, x.newTurn = seedTurnOn(t, s, f, "second", x.newJournal, false)
	x.oldJob = seedRunningJob(t, s, f, x.oldAgent, x.oldJournal, "old")
	x.newJob = seedRunningJob(t, s, f, x.newAgent, x.newJournal, "new")
	return x
}

func turnState(t *testing.T, s *Store, id string) (exec, owner string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT exec,COALESCE(journal_id,'') FROM turn WHERE id=?`, id).Scan(&exec, &owner); err != nil {
		t.Fatal(err)
	}
	return exec, owner
}

func jobState(t *testing.T, s *Store, id string) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow(`SELECT state FROM command_job WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAdmitAndCommandRecordTheOwningInstance(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	if exec, owner := turnState(t, x.s, x.oldTurn); exec != "running" || owner != x.oldJournal {
		t.Fatalf("old turn exec=%q owner=%q", exec, owner)
	}
	if _, owner := turnState(t, x.s, x.newTurn); owner != x.newJournal {
		t.Fatalf("new turn owner=%q", owner)
	}
	if x.oldJob.JournalID != x.oldJournal || x.newJob.JournalID != x.newJournal {
		t.Fatalf("job owners %q %q", x.oldJob.JournalID, x.newJob.JournalID)
	}
	// Without a named instance the machine's last accepted journal owns the work.
	if _, err := x.s.db.Exec(`UPDATE machine SET journal_id='accepted' WHERE id='mch_test'`); err != nil {
		t.Fatal(err)
	}
	job := seedRunningJob(t, x.s, x.f, x.newAgent, "", "default")
	if job.JournalID != "accepted" {
		t.Fatalf("default owner=%q", job.JournalID)
	}
}

// Two instances of one machine, local or remote, say hello in turn: each speaks only for its own journal, so neither loses the other's turns or jobs.
func TestTwoInstancesHellosNeverLoseEachOthersWork(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"host", "remote"} {
		t.Run(kind, func(t *testing.T) { twoInstancesHellos(t, kind == "host") })
	}
}

func twoInstancesHellos(t *testing.T, host bool) {
	x := newInstanceFixture(t)
	if _, err := x.s.db.Exec(`UPDATE machine SET is_host=? WHERE id='mch_test'`, host); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The old instance was the machine's accepted journal; the new one says hello, knowing only its own turn and job.
	if _, err := x.s.db.Exec(`UPDATE machine SET journal_id=? WHERE id='mch_test'`, x.oldJournal); err != nil {
		t.Fatal(err)
	}
	open := map[string]JournalTurn{x.newTurn: {TurnID: x.newTurn, Exec: "running", ProcessAlive: true}}
	if _, err := x.s.ReconcileMachineHello(ctx, "mch_test", x.newJournal, []byte(`[]`), open); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.ReconcileMachineCommands(ctx, "mch_test", x.newJournal, []protocol.CommandOpen{{JobID: x.newJob.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "running" {
		t.Fatalf("the new instance's hello lost the old instance's turn: %s", exec)
	}
	if state := jobState(t, x.s, x.oldJob.ID); state != "running" {
		t.Fatalf("the new instance's hello ended the old instance's job: %s", state)
	}
	// The old instance reconnects, still running its own work.
	open = map[string]JournalTurn{x.oldTurn: {TurnID: x.oldTurn, Exec: "running", ProcessAlive: true}}
	if _, err := x.s.ReconcileMachineHello(ctx, "mch_test", x.oldJournal, []byte(`[]`), open); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.ReconcileMachineCommands(ctx, "mch_test", x.oldJournal, []protocol.CommandOpen{{JobID: x.oldJob.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{x.oldTurn, x.newTurn} {
		if exec, _ := turnState(t, x.s, id); exec != "running" {
			t.Fatalf("turn %s is %s", id, exec)
		}
	}
	for _, id := range []string{x.oldJob.ID, x.newJob.ID} {
		if state := jobState(t, x.s, id); state != "running" {
			t.Fatalf("job %s is %s", id, state)
		}
	}
	// An instance that no longer knows one of its own turns and jobs still loses exactly those.
	if _, err := x.s.ReconcileMachineHello(ctx, "mch_test", x.newJournal, []byte(`[]`), map[string]JournalTurn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.ReconcileMachineCommands(ctx, "mch_test", x.newJournal, nil, nil); err != nil {
		t.Fatal(err)
	}
	if exec, _ := turnState(t, x.s, x.newTurn); exec != "lost" {
		t.Fatalf("its own unlisted turn is %s", exec)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "running" {
		t.Fatalf("the other instance's turn is %s", exec)
	}
	if state := jobState(t, x.s, x.newJob.ID); state != "lost" {
		t.Fatalf("its own unlisted job is %s", state)
	}
	if state := jobState(t, x.s, x.oldJob.ID); state != "running" {
		t.Fatalf("the other instance's job is %s", state)
	}
}

// A turn recorded before instances were tracked has no owner: the old rule still applies to it, and only to it.
func TestOwnerlessTurnKeepsTheJournalChangeRule(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	if _, err := x.s.db.Exec(`UPDATE turn SET journal_id=NULL WHERE id=?`, x.oldTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`UPDATE machine SET journal_id=? WHERE id='mch_test'`, "journal-before"); err != nil {
		t.Fatal(err)
	}
	open := map[string]JournalTurn{x.newTurn: {TurnID: x.newTurn, Exec: "running", ProcessAlive: true}, x.oldTurn: {TurnID: x.oldTurn, Exec: "running", ProcessAlive: true}}
	if _, err := x.s.ReconcileMachineHello(ctx, "mch_test", x.newJournal, []byte(`[]`), open); err != nil {
		t.Fatal(err)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "lost" {
		t.Fatalf("an ownerless turn under a changed journal is %s", exec)
	}
	if exec, _ := turnState(t, x.s, x.newTurn); exec != "running" {
		t.Fatalf("an owned turn is %s", exec)
	}
}

func TestPerInstanceQueries(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	ids, err := x.s.MachineRunningTurnIDsForInstance(ctx, "mch_test", x.oldJournal)
	if err != nil || len(ids) != 1 || ids[0] != x.oldTurn {
		t.Fatalf("old instance turns=%v err=%v", ids, err)
	}
	all, err := x.s.MachineRunningTurnIDs(ctx, "mch_test")
	if err != nil || len(all) != 2 {
		t.Fatalf("machine turns=%v err=%v", all, err)
	}
	jobs, err := x.s.OpenCommandJobsForInstance(ctx, "mch_test", x.newJournal)
	if err != nil || len(jobs) != 1 || jobs[0].ID != x.newJob.ID {
		t.Fatalf("new instance jobs=%+v err=%v", jobs, err)
	}
	if active, err := x.s.MachineActiveWorkForInstance(ctx, "mch_test", x.oldJournal); err != nil || !active {
		t.Fatalf("old instance active=%v err=%v", active, err)
	}
	if active, err := x.s.MachineActiveWorkForInstance(ctx, "mch_test", "journal-empty"); err != nil || active {
		t.Fatalf("an instance holding nothing is active=%v err=%v", active, err)
	}
	// A job alone keeps an instance busy.
	if _, err := x.s.db.Exec(`UPDATE turn SET exec='exited' WHERE id=?`, x.newTurn); err != nil {
		t.Fatal(err)
	}
	if active, err := x.s.MachineActiveWorkForInstance(ctx, "mch_test", x.newJournal); err != nil || !active {
		t.Fatalf("a running job must count: active=%v err=%v", active, err)
	}
}

func TestMarkSupervisorInstanceGoneLosesOnlyItsOwnWork(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"host", "remote"} {
		t.Run(kind, func(t *testing.T) { markGone(t, kind == "host") })
	}
}

// A journal that vanishes loses only its own instance's work, on a remote machine as on the local one.
func markGone(t *testing.T, host bool) {
	x := newInstanceFixture(t)
	if _, err := x.s.db.Exec(`UPDATE machine SET is_host=? WHERE id='mch_test'`, host); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, journal := range []string{x.oldJournal, x.newJournal} {
		if err := x.s.RecordSupervisorInstance(ctx, "mch_test", journal, "build-"+journal, "env"); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.s.SetSupervisorInstanceState(ctx, "mch_test", x.oldJournal, "retiring"); err != nil {
		t.Fatal(err)
	}
	turns, jobs, err := x.s.MarkSupervisorInstanceGone(ctx, "mch_test", x.oldJournal)
	if err != nil || turns != 1 || jobs != 1 {
		t.Fatalf("turns=%d jobs=%d err=%v", turns, jobs, err)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "lost" {
		t.Fatalf("gone instance's turn is %s", exec)
	}
	if state := jobState(t, x.s, x.oldJob.ID); state != "lost" {
		t.Fatalf("gone instance's job is %s", state)
	}
	var blocked bool
	if err := x.s.db.QueryRow(`SELECT recovery_block FROM agent WHERE id=?`, x.oldAgent.ID).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("the lost turn's agent must wait for recovery: %v %v", blocked, err)
	}
	if exec, _ := turnState(t, x.s, x.newTurn); exec != "running" {
		t.Fatalf("the other instance's turn is %s", exec)
	}
	if state := jobState(t, x.s, x.newJob.ID); state != "running" {
		t.Fatalf("the other instance's job is %s", state)
	}
	instances, err := x.s.SupervisorInstances(ctx, "mch_test")
	if err != nil || len(instances) != 2 {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	for _, item := range instances {
		want := "active"
		if item.JournalID == x.oldJournal {
			want = "gone"
		}
		if item.State != want || (want == "gone") != (item.GoneAt != "") {
			t.Fatalf("instance %+v, want state %s", item, want)
		}
	}
	// Once gone it stays gone and loses nothing twice.
	if turns, jobs, err := x.s.MarkSupervisorInstanceGone(ctx, "mch_test", x.oldJournal); err != nil || turns != 0 || jobs != 0 {
		t.Fatalf("second mark: %d %d %v", turns, jobs, err)
	}
	if err := x.s.SetSupervisorInstanceState(ctx, "mch_test", x.oldJournal, "retiring"); err == nil {
		t.Fatal("a gone instance took a new state")
	}
	// A hello is proof it runs: the instance is active again.
	if err := x.s.RecordSupervisorInstance(ctx, "mch_test", x.oldJournal, "build-again", "env"); err != nil {
		t.Fatal(err)
	}
	if instances, _ := x.s.SupervisorInstances(ctx, "mch_test"); instances[0].JournalID != x.oldJournal || instances[0].State != "active" || instances[0].Build != "build-again" || instances[0].GoneAt != "" {
		t.Fatalf("instances=%+v", instances)
	}
}

// Migration 139 on a database that already holds open turns and jobs gives them the journal the machine last accepted, leaves finished rows
// without an owner and creates one active instance per machine that has a journal.
func TestSupervisorInstanceMigrationBackfillsOpenWork(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	dropSupervisorInstances(t, x.s)
	for _, statement := range []string{
		`DELETE FROM schema_version WHERE version>=139`,
		`UPDATE machine SET journal_id='journal-last' WHERE id='mch_test'`,
		`UPDATE turn SET exec='exited' WHERE id='` + x.newTurn + `'`,
		`UPDATE command_job SET state='done',exit_code=0 WHERE id='` + x.newJob.ID + `'`,
	} {
		if _, err := x.s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrate(context.Background(), x.s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), x.s.db); err != nil { // already migrated: a no-op
		t.Fatal(err)
	}
	if exec, owner := turnState(t, x.s, x.oldTurn); exec != "running" || owner != "journal-last" {
		t.Fatalf("open turn exec=%q owner=%q", exec, owner)
	}
	if _, owner := turnState(t, x.s, x.newTurn); owner != "" {
		t.Fatalf("a finished turn got owner %q", owner)
	}
	var owner string
	if err := x.s.db.QueryRow(`SELECT COALESCE(journal_id,'') FROM command_job WHERE id=?`, x.oldJob.ID).Scan(&owner); err != nil || owner != "journal-last" {
		t.Fatalf("open job owner=%q err=%v", owner, err)
	}
	if err := x.s.db.QueryRow(`SELECT COALESCE(journal_id,'') FROM command_job WHERE id=?`, x.newJob.ID).Scan(&owner); err != nil || owner != "" {
		t.Fatalf("a finished job got owner %q err=%v", owner, err)
	}
	instances, err := x.s.SupervisorInstances(context.Background(), "mch_test")
	if err != nil || len(instances) != 1 || instances[0].JournalID != "journal-last" || instances[0].State != "active" {
		t.Fatalf("instances=%+v err=%v", instances, err)
	}
	var version int
	if err := x.s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != LatestSchemaVersion() {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestSupervisorInstanceLoadCountsWhatAWriteOffLoses(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	load, err := x.s.SupervisorInstanceLoad(ctx, "mch_test", x.oldJournal)
	if err != nil || load.Turns != 1 || load.Commands != 1 || len(load.Agents) != 1 || load.Agents[0].ID != x.oldAgent.ID {
		t.Fatalf("old instance load=%+v err=%v", load, err)
	}
	empty, err := x.s.SupervisorInstanceLoad(ctx, "mch_test", "journal-empty")
	if err != nil || empty.Turns != 0 || empty.Commands != 0 || len(empty.Agents) != 0 {
		t.Fatalf("an instance owning nothing: %+v err=%v", empty, err)
	}
}

func TestMarkSupervisorInstanceGoneByRecordsWhoDidItOnce(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	if err := x.s.RecordSupervisorInstance(ctx, "mch_test", x.oldJournal, "build", "env"); err != nil {
		t.Fatal(err)
	}
	ageInstance(t, x.s, "mch_test", x.oldJournal)
	events := func(kind string) (n int, payload string) {
		rows, err := x.s.db.Query(`SELECT payload_json FROM event WHERE kind=?`, kind)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			n++
			if err := rows.Scan(&payload); err != nil {
				t.Fatal(err)
			}
		}
		return n, payload
	}
	result, err := x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor)
	if err != nil || result.AlreadyGone || result.Turns != 1 || result.Jobs != 1 {
		t.Fatalf("first mark %+v err=%v", result, err)
	}
	if n, payload := events("supervisor_instance_gone"); n != 1 || !strings.Contains(payload, `"actor_id":"usr_test"`) || !strings.Contains(payload, x.oldJournal) || !strings.Contains(payload, `"turns":"1"`) {
		t.Fatalf("record n=%d payload=%s", n, payload)
	}
	if n, _ := events("supervisor_journal_changed"); n != 1 {
		t.Fatalf("recovery notices: %d", n)
	}
	result, err = x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor)
	if err != nil || !result.AlreadyGone || result.Turns != 0 || result.Jobs != 0 {
		t.Fatalf("second mark %+v err=%v", result, err)
	}
	if n, _ := events("supervisor_instance_gone"); n != 1 {
		t.Fatalf("a second write-off recorded again: %d", n)
	}
	// The local manager's write-off has no person and records no decision.
	if err := x.s.RecordSupervisorInstance(ctx, "mch_test", x.newJournal, "build", "env"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.s.MarkSupervisorInstanceGone(ctx, "mch_test", x.newJournal); err != nil {
		t.Fatal(err)
	}
	if n, _ := events("supervisor_instance_gone"); n != 1 {
		t.Fatalf("an automatic write-off recorded a decision: %d", n)
	}
}

// ageInstance makes the instance's last hello old enough for a person to write it off.
func ageInstance(t *testing.T, s *Store, machineID, journalID string) {
	t.Helper()
	old := time.Now().UTC().Add(-2 * InstanceQuietFor).Format(timestampLayout)
	if _, err := s.db.Exec(`UPDATE supervisor_instance SET last_seen_at=? WHERE machine_id=? AND journal_id=?`, old, machineID, journalID); err != nil {
		t.Fatal(err)
	}
}

// A hello within the quiet period proves the instance runs: a person's write-off is refused in the transaction itself, so what it owns is not
// lost, nothing is recorded and the instance keeps its state. Once it has been silent long enough the same call goes through.
// A recent disconnect is a recent sighting: an instance connected for hours that dropped a second ago is refused for the quiet period
// (F005), and a write-off goes through once it has been silent long enough.
func TestMarkSupervisorInstanceGoneByCountsARecentDisconnect(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	if err := x.s.RecordSupervisorInstance(ctx, "mch_test", x.oldJournal, "build", "env"); err != nil {
		t.Fatal(err)
	}
	// The hello is old, but the connection ended just now.
	ageInstance(t, x.s, "mch_test", x.oldJournal)
	if err := x.s.TouchSupervisorInstanceSeen(ctx, "mch_test", x.oldJournal); err != nil {
		t.Fatal(err)
	}
	result, err := x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor)
	if !errors.Is(err, ErrInstanceRecentlySeen) || result.Turns != 0 || result.Jobs != 0 {
		t.Fatalf("recently disconnected: %+v err=%v", result, err)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "running" {
		t.Fatalf("a refused write-off lost the turn: %s", exec)
	}
	ageInstance(t, x.s, "mch_test", x.oldJournal)
	if result, err = x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor); err != nil || result.Turns != 1 || result.Jobs != 1 {
		t.Fatalf("after the quiet period: %+v err=%v", result, err)
	}
	// An instance the table does not hold is not created by the touch.
	if err := x.s.TouchSupervisorInstanceSeen(ctx, "mch_test", "journal-unknown"); err != nil {
		t.Fatalf("unknown instance touch: %v", err)
	}
}

func TestMarkSupervisorInstanceGoneByRefusesARecentlySeenInstance(t *testing.T) {
	t.Parallel()
	x := newInstanceFixture(t)
	ctx := context.Background()
	if err := x.s.RecordSupervisorInstance(ctx, "mch_test", x.oldJournal, "build", "env"); err != nil {
		t.Fatal(err)
	}
	result, err := x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor)
	if !errors.Is(err, ErrInstanceRecentlySeen) || result.Turns != 0 || result.Jobs != 0 {
		t.Fatalf("recently seen: %+v err=%v", result, err)
	}
	if exec, _ := turnState(t, x.s, x.oldTurn); exec != "running" {
		t.Fatalf("a refused write-off lost the turn: %s", exec)
	}
	if state := jobState(t, x.s, x.oldJob.ID); state != "running" {
		t.Fatalf("a refused write-off ended the job: %s", state)
	}
	var events int
	if err := x.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind IN ('supervisor_instance_gone','supervisor_journal_changed')`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("a refused write-off recorded %d events (%v)", events, err)
	}
	if list, err := x.s.SupervisorInstances(ctx, "mch_test"); err != nil || len(list) != 1 || list[0].State != "active" || list[0].GoneAt != "" {
		t.Fatalf("instance after a refusal: %+v %v", list, err)
	}
	ageInstance(t, x.s, "mch_test", x.oldJournal)
	if result, err = x.s.MarkSupervisorInstanceGoneBy(ctx, "mch_test", x.oldJournal, "usr_test", InstanceQuietFor); err != nil || result.Turns != 1 || result.Jobs != 1 {
		t.Fatalf("after the quiet period: %+v err=%v", result, err)
	}
}
