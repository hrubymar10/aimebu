package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// seedProject fills one project with a row in every table that belongs to a project or to one of its rooms, and returns the ids the
// test needs and the attachment file it wrote.
type seededProject struct {
	Project Project
	Task    Room
	Agent   Agent
	File    string
}

func seedWorld(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_worker','worker','Worker','work','worker')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_proj','host','hash',1,1)`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_proj','owner','hash','{}','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// A global prompt override and a password-reset invite have an empty project_id: a deletion must never turn a project's into one.
	for _, statement := range []string{
		`INSERT INTO prompt_override(id,project_id,key,value,rev,updated_at) VALUES ('po_global',NULL,'k','global',1,'now')`,
		`INSERT INTO user_invite(id,token_hash,kind,project_id,role,target_user_id,created_by,expires_at,created_at) VALUES ('inv_reset','h0','reset',NULL,NULL,'usr_proj','usr_proj','later','now')`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func seedProject(t *testing.T, s *Store, tag string) seededProject {
	t.Helper()
	ctx := context.Background()
	project, err := s.CreateProject(ctx, Project{Name: "project " + tag, Folder: "/tmp/" + tag})
	if err != nil {
		t.Fatal(err)
	}
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: "home " + tag, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: home.ID, Kind: "task", Title: "task " + tag, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	checkout := "chk_" + tag
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_proj','owner')`, project.ID)
	exec(`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES (?,?,'mch_proj',?)`, checkout, project.ID, "/tmp/x-"+tag)
	agent, err := s.CreateAgent(ctx, Agent{Slug: "ann", ProjectID: project.ID, RoleID: "role_worker", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_proj", CheckoutID: checkout, KeyHash: "key" + tag, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, task.ID, "agent", agent.ID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO session(id,agent_id,generation,start_mode,started_at) VALUES (?,?,0,'fresh_with_history',?)`, "ses_"+tag, agent.ID, timestamp())
	first, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "agent", AuthorID: agent.ID, Body: "hello " + tag})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.InsertMessage(ctx, Message{RoomID: task.ID, AuthorKind: "user", AuthorID: "usr_proj", Body: "reply " + tag, ReplyTo: first.ID})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "blob-"+tag)
	if err := os.WriteFile(file, []byte(tag), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(file) })
	exec(`INSERT INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,'user','usr_proj','👍','t')`, first.ID)
	exec(`INSERT INTO attachment(id,project_id,room_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES (?,?,?,?,'h','image/png',1,?,'owner','t')`, "att_"+tag, project.ID, task.ID, first.ID, file)
	exec(`INSERT INTO project_memory(id,project_id,body,author_kind,author_id,created_at,updated_at) VALUES (?,?,'a fact','user','usr_proj','t','t')`, "mem_"+tag, project.ID)
	exec(`INSERT INTO task_note(id,room_id,body,rev,author_kind,author_id,created_at,updated_at) VALUES (?,?,'note',1,'user','usr_proj','t','t')`, "tn_"+tag, task.ID)
	exec(`INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_lead,body,kind,timezone,interval_seconds,next_run_at,created_at,updated_at) VALUES (?,?,?,'user','usr_proj',1,'x','interval','UTC',300,'later','t','t')`, "sch_"+tag, task.ID, project.ID)
	exec(`INSERT INTO inbox_item(id,project_id,room_id,message_id,agent_id,requester_kind,requester_id,kind,created_at) VALUES (?,?,?,?,?,'agent',?,'question','now')`, "inb_"+tag, project.ID, task.ID, first.ID, agent.ID, agent.ID)
	exec(`INSERT INTO turn(id,agent_id,session_id,generation,room_id,attempt,mode,exec) VALUES (?,?,?,0,?,1,'normal','exited')`, "trn_"+tag, agent.ID, "ses_"+tag, task.ID)
	exec(`INSERT INTO usage_record(id,source_event_id,turn_id,agent_id,room_id,project_id,source,created_at) VALUES (?,?,?,?,?,?,'measured','t')`, "usg_"+tag, "evt_usage_"+tag, "trn_"+tag, agent.ID, task.ID, project.ID)
	exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_turn_cap,turn_mode) VALUES ('project',?,5,'custom')`, project.ID)
	exec(`INSERT INTO usage_limit(scope_kind,scope_id,daily_turn_cap,turn_mode) VALUES ('room',?,5,'custom')`, task.ID)
	exec(`INSERT INTO usage_limit_day(scope_kind,scope_id,day_start,tz) VALUES ('project',?,'d','UTC')`, project.ID)
	exec(`INSERT INTO usage_limit_day(scope_kind,scope_id,day_start,tz) VALUES ('room',?,'d','UTC')`, task.ID)
	exec(`INSERT INTO scope_control_agent(scope_kind,scope_id,agent_id,restore_desired,bulk_desired,bulk_rev) VALUES ('project',?,?,'active','stopped',1)`, project.ID, agent.ID)
	exec(`INSERT INTO scope_control_failure(scope_kind,scope_id,agent_id,action,error,attempted_at) VALUES ('room',?,?,'stop','e','t')`, task.ID, agent.ID)
	exec(`INSERT INTO prompt_override(id,project_id,key,value,rev,updated_at) VALUES (?,?,'k','mine',1,'now')`, "po_"+tag, project.ID)
	exec(`INSERT INTO user_invite(id,token_hash,kind,project_id,role,created_by,expires_at,created_at) VALUES (?,?,'join',?,'viewer','usr_proj','later','now')`, "inv_"+tag, "h"+tag, project.ID)
	exec(`INSERT INTO cleanup_log(id,seq,category,target,project_id,reason,bytes,action,actor,created_at) VALUES (?,?,'attachment','x',?,'old',1,'removed','schedule','t')`, "clg_"+tag, len(tag)+1000+int(tag[0]), project.ID)
	exec(`INSERT INTO project_machine_allowed(project_id,machine_id,approved_by,approved_at) VALUES (?,'mch_proj','usr_proj','t')`, project.ID)
	exec(`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES (?,'mch_proj','/tmp/f')`, project.ID)
	exec(`INSERT INTO project_role_spawn_default(project_id,role_id,items_json) VALUES (?,'role_worker','[]')`, project.ID)
	exec(`INSERT INTO role_spawn_cursor(project_id,role_id,next_position) VALUES (?,'role_worker',1)`, project.ID)
	exec(`INSERT INTO room_worktree(room_id,base_ref,branch_name,relative_path,state,created_at,updated_at) VALUES (?,'main','b','p','removed','t','t')`, task.ID)
	_ = reply
	return seededProject{Project: project, Task: task, Agent: agent, File: file}
}

// tableCounts is how many rows every table holds.
func tableCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	out := map[string]int{}
	for _, name := range names {
		out[name] = countRows(t, s, `SELECT COUNT(*) FROM "`+name+`"`)
	}
	return out
}

// Deleting a project from a store that holds two leaves exactly what a store that only ever held the other one has (apart from the one
// event that says it is gone): every table, the global rows included.
func TestDeleteProjectLeavesWhatAStoreWithoutItHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	alone, _ := testStore(t, Options{})
	seedWorld(t, alone)
	seedProject(t, alone, "keep")
	want := tableCounts(t, alone)

	both, _ := testStore(t, Options{})
	seedWorld(t, both)
	doomed := seedProject(t, both, "gone")
	keep := seedProject(t, both, "keep")
	// Every table the seed is supposed to reach holds something of the doomed project, or this test would prove nothing.
	reached := 0
	for name, n := range tableCounts(t, both) {
		if n > want[name] {
			reached++
		} else if n < want[name] {
			t.Fatalf("%s: the second store holds less than the first", name)
		}
	}
	if reached < 30 {
		t.Fatalf("the doomed project reaches only %d tables", reached)
	}

	if err := both.DeleteProject(ctx, doomed.Project.ID, doomed.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectNotArchived) {
		t.Fatalf("a live project must be archived first, got %v", err)
	}
	controls, failed, err := both.ArchiveProject(ctx, doomed.Project.ID, "usr_proj")
	if err != nil || len(controls) != 1 || controls[0].Desired != "stopped" || len(failed) != 0 {
		t.Fatalf("archive: %+v %+v %v", controls, failed, err)
	}
	if err := both.DeleteProject(ctx, doomed.Project.ID, "wrong", "usr_proj"); !errors.Is(err, ErrProjectNameMismatch) {
		t.Fatalf("the wrong name must refuse, got %v", err)
	}
	if err := both.DeleteProject(ctx, doomed.Project.ID, doomed.Project.Name, "usr_proj"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got := tableCounts(t, both)
	for name, n := range want {
		wantRows := n
		if name == "event" {
			wantRows++ // project_removed
		}
		if got[name] != wantRows {
			t.Errorf("%s: %d rows after the delete, a store without the project holds %d", name, got[name], wantRows)
		}
	}
	var violations int
	if err := both.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Errorf("foreign key violations after the delete: %d %v", violations, err)
	}
	if _, err := os.Stat(doomed.File); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the project's attachment file remains: %v", err)
	}
	if _, err := os.Stat(keep.File); err != nil {
		t.Errorf("the other project's attachment file was removed: %v", err)
	}
	// The other project and the global rows are the very ones they were, not only as many.
	for query, wantRows := range map[string]int{
		`SELECT COUNT(*) FROM prompt_override WHERE project_id IS NULL`:                                                                            1,
		`SELECT COUNT(*) FROM prompt_override WHERE project_id='` + keep.Project.ID + `'`:                                                          1,
		`SELECT COUNT(*) FROM user_invite WHERE kind='reset' AND project_id IS NULL`:                                                               1,
		`SELECT COUNT(*) FROM user_invite WHERE kind='join' AND project_id='` + keep.Project.ID + `'`:                                              1,
		`SELECT COUNT(*) FROM project WHERE id='` + doomed.Project.ID + `'`:                                                                        0,
		`SELECT COUNT(*) FROM event WHERE kind='project_removed' AND project_id IS NULL AND json_extract(payload_json,'$.user_ids[0]')='usr_proj'`: 1,
	} {
		if n := countRows(t, both, query); n != wantRows {
			t.Errorf("%s: %d, want %d", query, n, wantRows)
		}
	}
}

func TestArchiveStopsAgentsAndReopenKeepsThemStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "one")
	if _, err := s.db.Exec(`UPDATE project SET control_desired='active'`); err != nil {
		t.Fatal(err)
	}
	controls, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj")
	if err != nil || len(controls) != 1 {
		t.Fatalf("archive: %+v %v", controls, err)
	}
	var archived, gate string
	if err := s.db.QueryRow(`SELECT COALESCE(archived_at,''),gate FROM project WHERE id=?`, p.Project.ID).Scan(&archived, &gate); err != nil || archived == "" || gate != "paused" {
		t.Fatalf("archived=%q gate=%q %v", archived, gate, err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM schedule WHERE project_id=? AND enabled=1`, p.Project.ID) != 0 {
		t.Error("a schedule is still on")
	}
	if countRows(t, s, `SELECT COUNT(*) FROM inbox_item WHERE project_id=? AND state='open'`, p.Project.ID) != 0 {
		t.Error("a question is still waiting in the Inbox of an archived project")
	}
	if again, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj"); err != nil || len(again) != 0 {
		t.Fatalf("archiving twice: %+v %v", again, err)
	}
	if got, err := s.ArchivedScope(ctx, "usr_proj", "room", p.Task.ID); err != nil || !got {
		t.Fatalf("ArchivedScope(room) = %v %v", got, err)
	}
	if got, err := s.ArchivedScope(ctx, "usr_proj", "agent", p.Agent.ID); err != nil || !got {
		t.Fatalf("ArchivedScope(agent) = %v %v", got, err)
	}
	if got, err := s.ArchivedScope(ctx, "usr_proj", "room", "room_missing"); err != nil || got {
		t.Fatalf("ArchivedScope(missing) = %v %v", got, err)
	}
	// An admin who is not a member may write to the project, so is told; a stranger is not.
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at,admin) VALUES ('usr_admin','adm','h','{}','later',1)`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ArchivedScope(ctx, "usr_admin", "room", p.Task.ID); err != nil || !got {
		t.Fatalf("ArchivedScope(admin) = %v %v", got, err)
	}
	// Someone who is not a member is told nothing: an id does not show whether its project is archived.
	if got, err := s.ArchivedScope(ctx, "usr_stranger", "room", p.Task.ID); err != nil || got {
		t.Fatalf("ArchivedScope(stranger) = %v %v", got, err)
	}
	if err := s.ReopenProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COALESCE(archived_at,''),gate FROM project WHERE id=?`, p.Project.ID).Scan(&archived, &gate); err != nil || archived != "" || gate != "open" {
		t.Fatalf("after reopening archived=%q gate=%q %v", archived, gate, err)
	}
	var desired string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, p.Agent.ID).Scan(&desired); err != nil || desired != "stopped" {
		t.Fatalf("reopening must leave the agent stopped, got %q %v", desired, err)
	}
	// A project the person had paused stays paused when reopened.
	if _, err := s.db.Exec(`UPDATE project SET control_desired='paused' WHERE id=?`, p.Project.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, p.Project.ID).Scan(&gate); err != nil || gate != "paused" {
		t.Fatalf("a paused project came back %q %v", gate, err)
	}
}

func TestDeleteProjectRefusesARunningAgentAndALiveWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "busy")
	if _, err := s.db.Exec(`UPDATE project SET archived_at='t'`); err != nil {
		t.Fatal(err)
	}
	// The agent was never stopped (an archive does it; this one was marked by hand).
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("a running agent must refuse, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='stopped'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room_worktree SET state='ready'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("a live worktree must refuse, got %v", err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM project WHERE id=?`, p.Project.ID) != 1 {
		t.Fatal("a refused delete removed the project")
	}
	if _, err := s.db.Exec(`UPDATE room_worktree SET state='removed'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); err != nil {
		t.Fatalf("once nothing is live: %v", err)
	}
}

// An agent whose execution is unknown cannot be stopped until a recovery decision; it does not keep the project live, and the rest are
// still stopped.
func TestArchiveGoesOnPastAnAgentThatCannotBeStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "lost")
	other, err := s.CreateAgent(ctx, Agent{Slug: "bea", ProjectID: p.Project.ID, RoleID: "role_worker", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_proj", CheckoutID: "chk_lost", KeyHash: "key2", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, p.Task.ID, "agent", other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET observed='lost' WHERE id=?`, p.Agent.ID); err != nil {
		t.Fatal(err)
	}
	controls, failed, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if len(failed) != 1 || failed[0].AgentID != p.Agent.ID || failed[0].Error == "" {
		t.Fatalf("the agent that could not be stopped must be reported: %+v", failed)
	}
	if len(controls) != 1 || controls[0].ID != other.ID {
		t.Fatalf("the other agent must be stopped: %+v", controls)
	}
	var archived string
	if err := s.db.QueryRow(`SELECT COALESCE(archived_at,'') FROM project WHERE id=?`, p.Project.ID).Scan(&archived); err != nil || archived == "" {
		t.Fatalf("the project is not archived: %q %v", archived, err)
	}
	var desired string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, p.Agent.ID).Scan(&desired); err != nil || desired == "stopped" {
		t.Fatalf("the unstoppable agent was changed: %q %v", desired, err)
	}
	// It keeps the project from being deleted until its recovery is decided.
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("an agent that may be running must refuse the delete, got %v", err)
	}
}

// An agent that was told to stop but has not reported it holds the project while its machine can still come back: a machine that is only
// offline may be running its turn, which would run on, unknown to the server, when it returns. A revoked machine can never come back, so its
// agents do not hold the project.
func TestDeleteProjectNeedsAgentsConfirmedStoppedOrTheirMachineRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "away")
	if _, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped',observed='in_turn' WHERE id=?`, p.Agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("an agent not yet reported stopped on an approved machine must refuse, got %v", err)
	}
	if _, err := s.db.Exec(`UPDATE machine SET revoked_at=? WHERE id='mch_proj'`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); err != nil {
		t.Fatalf("a revoked machine can never come back, so it must not hold the project: %v", err)
	}
}

// An agent that archiving could not stop is freed by a recovery decision: in an archived project the only decision is Abandon, anything
// else is refused, and Abandon ends with the agent stopped. Then the project can be deleted.
func TestAbandonInAnArchivedProjectEndsWithTheAgentStoppedAndUnblocksDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "stuck")
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`UPDATE turn SET exec='lost',outcome='unknown',settle='pending' WHERE id='trn_stuck'`)
	exec(`UPDATE agent SET observed='lost' WHERE id=?`, p.Agent.ID)
	exec(`INSERT INTO inbox_item(id,project_id,room_id,agent_id,turn_id,requester_kind,requester_id,kind,created_at) VALUES ('inb_rec',?,?,?,'trn_stuck','system','system','recovery','now')`, p.Project.ID, p.Task.ID, p.Agent.ID)
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.AppendEvent(Event{Kind: "inbox_item_created", ProjectID: p.Project.ID, RoomID: p.Task.ID, PayloadJSON: payloadJSON(map[string]string{"inbox_item_id": "inb_rec"})})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, failed, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj")
	if err != nil || len(failed) != 1 {
		t.Fatalf("archive: %+v %v", failed, err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); !errors.Is(err, ErrProjectBusy) {
		t.Fatalf("a stuck agent must hold the project, got %v", err)
	}
	for _, choice := range []string{"resume", "retry", "continue", "override"} {
		if _, _, err := s.DecideRecovery(ctx, "inb_rec", "usr_proj", "key-"+choice, choice, 0); !errors.Is(err, ErrProjectArchived) {
			t.Fatalf("%s in an archived project must be refused, got %v", choice, err)
		}
	}
	var desired string
	if err := s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, p.Agent.ID).Scan(&desired); err != nil || desired == "stopped" {
		t.Fatalf("a refused decision changed the agent: %q %v", desired, err)
	}
	decision, reused, err := s.DecideRecovery(ctx, "inb_rec", "usr_proj", "key-abandon", "abandon", 0)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	// The agent it stopped comes back, so the caller can tell its machine; a replay of the same decision has already told it.
	if reused || decision.StoppedAgent == nil || decision.StoppedAgent.ID != p.Agent.ID || decision.StoppedAgent.Desired != "stopped" {
		t.Fatalf("abandon must return the agent it stopped: %+v reused=%v", decision.StoppedAgent, reused)
	}
	if replay, again, err := s.DecideRecovery(ctx, "inb_rec", "usr_proj", "key-abandon", "abandon", 0); err != nil || !again || replay.StoppedAgent != nil {
		t.Fatalf("a replayed decision must not report a stop again: %+v %v %v", replay.StoppedAgent, again, err)
	}
	var observed string
	if err := s.db.QueryRow(`SELECT desired,observed FROM agent WHERE id=?`, p.Agent.ID).Scan(&desired, &observed); err != nil || desired != "stopped" || observed != "stopped" {
		t.Fatalf("abandon must end with the agent stopped, got desired=%q observed=%q %v", desired, observed, err)
	}
	if err := s.DeleteProject(ctx, p.Project.ID, p.Project.Name, "usr_proj"); err != nil {
		t.Fatalf("with the agent stopped the project can be deleted: %v", err)
	}
}

// A usage limit that paused the project today still holds it after the project is reopened.
func TestReopenRespectsAUsageLimitPause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "limit")
	if _, err := s.db.Exec(`UPDATE project SET control_desired='active'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	start, _, _ := localUsageDay(time.Now())
	if _, err := s.db.Exec(`UPDATE usage_limit_day SET paused=1,day_start=? WHERE scope_kind='project' AND scope_id=?`, start.UTC().Format(timestampLayout), p.Project.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	var gate string
	if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, p.Project.ID).Scan(&gate); err != nil || gate != "paused" {
		t.Fatalf("a project that today's limit paused came back %q %v", gate, err)
	}
}

// Nothing starts in an archived project, whatever path it takes: an archive that commits between a route's check and its transaction is
// caught by the transaction, and a race of spawns against an archive leaves no agent running.
func TestNothingStartsInAnArchivedProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := testStore(t, Options{})
	seedWorld(t, s)
	p := seedProject(t, s, "race")
	if _, _, err := s.ArchiveProject(ctx, p.Project.ID, "usr_proj"); err != nil {
		t.Fatal(err)
	}
	err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SpawnAgent(SpawnInput{RoomID: p.Task.ID, RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", ActorKind: "user", ActorID: "usr_proj"})
		return err
	})
	if !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("a spawn into an archived project must be refused, got %v", err)
	}
	var rev int64
	if err := s.db.QueryRow(`SELECT desired_rev FROM agent WHERE id=?`, p.Agent.ID).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	err = s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.SetAgentDesired(p.Agent.ID, "usr_proj", "active", rev)
		return err
	})
	if !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("restarting an agent of an archived project must be refused, got %v", err)
	}

	// The same, racing: spawns keep coming while the project is archived.
	raced := seedProject(t, s, "raced")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = s.Tx(ctx, func(tx *Tx) error {
				_, err := tx.SpawnAgent(SpawnInput{RoomID: raced.Task.ID, RoleKey: "worker", Tool: "fake", Variant: "test", Model: "test", ActorKind: "user", ActorID: "usr_proj"})
				return err
			})
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, _, err := s.ArchiveProject(ctx, raced.Project.ID, "usr_proj"); err != nil {
			t.Error(err)
		}
	}()
	close(start)
	wg.Wait()
	if n := countRows(t, s, `SELECT COUNT(*) FROM agent WHERE project_id=? AND revoked_at IS NULL AND desired<>'stopped'`, raced.Project.ID); n != 0 {
		t.Fatalf("%d agents are running in a project that was archived", n)
	}
}

// Every table that names a project is either reached by the foreign keys, or listed as one the deletion clears by hand. A table added later
// that names a project in neither way fails here, so a deleted project cannot leave rows behind.
func TestProjectDeleteCoversEveryTableThatNamesAProject(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	listed := func(list []string, name string) bool {
		for _, item := range list {
			if item == name {
				return true
			}
		}
		return false
	}
	tables, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	tables.Close()
	for _, table := range names {
		foreign := map[string]string{}
		fks, err := s.db.Query(`SELECT "from","table" FROM pragma_foreign_key_list('` + table + `')`)
		if err != nil {
			t.Fatal(err)
		}
		for fks.Next() {
			var from, parent string
			if err := fks.Scan(&from, &parent); err != nil {
				t.Fatal(err)
			}
			foreign[from] = parent
		}
		fks.Close()
		columns, err := s.db.Query(`SELECT name,"notnull",pk FROM pragma_table_info('` + table + `')`)
		if err != nil {
			t.Fatal(err)
		}
		hasScope := map[string]bool{}
		for columns.Next() {
			var name string
			var notNull, pk int
			if err := columns.Scan(&name, &notNull, &pk); err != nil {
				t.Fatal(err)
			}
			hasScope[name] = true
			switch {
			case name == "project_id" && foreign[name] == "project":
				if notNull == 0 && pk == 0 && !listed(projectOptionalTables, table) && table != "settings_project" {
					t.Errorf("%s.project_id is optional: a deletion would turn the project's rows into global ones; add the table to projectOptionalTables", table)
				}
			case name == "project_id":
				if !listed(projectLooseTables, table) {
					t.Errorf("%s.project_id names a project with no foreign key; add the table to projectLooseTables", table)
				}
			case name == "project" || name == "scope_key":
				if _, ok := projectNotLinked[table+"."+name]; !ok {
					t.Errorf("%s.%s may name a project; add the table to the deletion or say in projectNotLinked why it does not", table, name)
				}
			}
		}
		columns.Close()
		if hasScope["scope_kind"] && hasScope["scope_id"] && !listed(projectScopeTables, table) {
			t.Errorf("%s is scoped by scope_kind and scope_id; add the table to projectScopeTables", table)
		}
	}
	for _, table := range append(append(append([]string{}, projectOptionalTables...), projectLooseTables...), projectScopeTables...) {
		if !listed(names, table) {
			t.Errorf("the deletion lists %q, which is not a table", table)
		}
	}
}
