package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreStartupRetriesSQLiteBusy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "busy.sqlite")
	blocker, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.Exec(`CREATE TABLE hold (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO hold(value) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- openStoreWithBusyRetry(ctx, db, path) }()
	// Wait for startup to use and release the connection twice. A quick Ping
	// may account for one cycle; the locked schema write accounts for the
	// other. This observes an actual busy attempt before unlocking, without
	// tying migration completion to a short wall-clock deadline.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	wasInUse := false
	idleCycles := 0
	for idleCycles < 2 {
		select {
		case err := <-done:
			t.Fatalf("startup finished while write lock held: %v", err)
		case <-ticker.C:
			inUse := db.Stats().InUse > 0
			if wasInUse && !inUse {
				idleCycles++
			}
			wasInUse = inUse
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("startup after lock release: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestMigrationTwentyOneUpgradesExistingTwentyDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "from-20.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:20] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, migration.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_existing','existing','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var login string
	if err := s.db.QueryRow(`SELECT login FROM user WHERE id='usr_existing'`).Scan(&login); err != nil || login != "existing" {
		t.Fatalf("existing user=%q err=%v", login, err)
	}
}

// TestLatestSchemaVersion pins the invariant the function relies on: migration
// versions are contiguous from 1, so the last list entry is the latest.
func TestLatestSchemaVersion(t *testing.T) {
	if got := LatestSchemaVersion(); got != migrations[len(migrations)-1].version {
		t.Fatalf("LatestSchemaVersion() = %d, want %d", got, migrations[len(migrations)-1].version)
	}
	for i, migration := range migrations {
		if migration.version != i+1 {
			t.Fatalf("migrations[%d].version = %d, want %d", i, migration.version, i+1)
		}
	}
}

func TestMigrationTwentyFourBackfillsUsedMachineAdmission(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-23.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:23] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_existing','owner','hash','{}','now')`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_existing','existing','/host/project','now')`,
		`INSERT INTO membership(project_id,user_id,role) VALUES ('prj_existing','usr_existing','owner')`,
		`INSERT INTO machine(id,name,key_hash,approved,is_host) VALUES ('mch_host','host','hash',1,1),('mch_remote','remote','hash',1,0)`,
		`INSERT INTO project_machine_folder(project_id,machine_id,folder) VALUES ('prj_existing','mch_remote','/remote/project')`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_existing','prj_existing','mch_remote','/remote/project/task')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_existing','existing','prj_existing',(SELECT id FROM role WHERE role_key='worker'),'fake','test','test','fake','mch_remote','chk_existing','hash','usr_existing','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(migrations[23].sql); err != nil {
		t.Fatalf("migration 24: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_machine_allowed WHERE project_id='prj_existing'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("backfilled admissions=%d err=%v", count, err)
	}
}

func TestMigrationTwentySixUpgradesImportedDatabase(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-25.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:25] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO schema_version(version,applied_at) VALUES (25,'now')`,
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_imported','Imported','/tmp/imported','now')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_imported','prj_imported',0,'home','Imported','system','now')`,
		`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at,legacy_payload_json,legacy_author_label) VALUES ('msg_imported',1,'room_imported','agent','bot@repo','legacy','[]','now',
			'{"id":7,"from":"bot@repo","from_kind":"ai","from_harness":"codex","from_model":"gpt-6-sol","body":"legacy","needs_human_attention":true,"proposed_answers":["yes","no"],
			"open_questions":[{"id":"q1","question":"which?"}],"visual_plan":{"blocks":[{"type":"text","text":"plan"}]},"appendix_pages":[{"title":"A","body":"long"}],"unknown_field":1}','bot@repo · codex/gpt-6-sol')`,
		`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at,legacy_payload_json,legacy_author_label) VALUES ('msg_plain',2,'room_imported','agent','legacy:alice','from a person nobody mapped','[]','now',
			'{"id":8,"from":"alice","from_kind":"human","body":"x","proposed_answers":[],"visual_plan":null}','alice')`,
		`INSERT INTO legacy_import_source(source_key,map_digest,created_at,updated_at) VALUES ('v1-old','digest','now','now')`,
		`INSERT INTO project_memory(id,project_id,body,rev,author_kind,author_id,created_at,updated_at,legacy_payload_json) VALUES ('knw_pending','prj_imported','pending fact',2,'user','alice','now','now','{"v1":true}')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,legacy_payload_json,active,created_at,updated_at) VALUES ('knw_gpending','agent_shared_notes','global','pending note',1,'agent','bot','{"v1":true}',0,'now','now')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,active,created_at,updated_at) VALUES ('knw_gparked','user_profile','usr_x','parked by the owner',1,'user','usr_x',0,'now','now')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,legacy_payload_json,active,created_at,updated_at) VALUES ('knw_gimported_off','agent_shared_notes','global','imported, then switched off by the owner',2,'user','usr_x','{"v1":true}',0,'now','now')`,
		`INSERT INTO event(id,seq,kind,project_id,payload_json,created_at) VALUES ('evt_goff',9001,'global_knowledge_updated','prj_imported','{"id":"knw_gimported_off","scope":"agent_shared_notes","rev":"2","active":"false","actor_kind":"user","actor_id":"usr_x"}','now')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,legacy_payload_json,active,created_at,updated_at) VALUES ('knw_gimported_edited','agent_shared_notes','global','imported, edited, never switched off',3,'user','usr_x','{"v1":true}',0,'now','now')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,legacy_payload_json,active,created_at,updated_at) VALUES ('knw_guntouched_v3','agent_shared_notes','global','imported, untouched, v1 version 3',3,'agent','bot','{"v1":true}',0,'now','now')`,
		`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,legacy_payload_json,active,created_at,updated_at) VALUES ('knw_gimported_reon','agent_shared_notes','global','imported, switched off, then back on',3,'user','usr_x','{"v1":true}',0,'now','now')`,
		`INSERT INTO event(id,seq,kind,project_id,payload_json,created_at) VALUES ('evt_reoff',9002,'global_knowledge_updated','prj_imported','{"id":"knw_gimported_reon","scope":"agent_shared_notes","rev":"2","active":"false","actor_kind":"user","actor_id":"usr_x"}','now')`,
		`INSERT INTO event(id,seq,kind,project_id,payload_json,created_at) VALUES ('evt_reon',9003,'global_knowledge_updated','prj_imported','{"id":"knw_gimported_reon","scope":"agent_shared_notes","rev":"3","active":"true","actor_kind":"user","actor_id":"usr_x"}','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(context.WithValue(context.Background(), keepHistoryUnnumbered{}, true), db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var applied26 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=26`).Scan(&applied26); err != nil || applied26 != 1 {
		t.Fatalf("migration 26 applied=%d err=%v", applied26, err)
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM message WHERE id='msg_imported'`).Scan(&body); err != nil || body != "legacy" {
		t.Fatalf("message body=%q err=%v", body, err)
	}
	var harness, model, answers, questions, plan, pages, authorKind, authorID string
	var attention int
	if err := db.QueryRow(`SELECT author_harness,author_model,needs_attention,COALESCE(proposed_answers_json,''),COALESCE(open_questions_json,''),COALESCE(visual_plan_json,''),COALESCE(appendix_pages_json,'') FROM message WHERE id='msg_imported'`).
		Scan(&harness, &model, &attention, &answers, &questions, &plan, &pages); err != nil {
		t.Fatal(err)
	}
	if harness != "codex" || model != "gpt-6-sol" || attention != 1 || answers != `["yes","no"]` || !strings.Contains(questions, `"which?"`) || !strings.Contains(plan, `"plan"`) || !strings.Contains(pages, `"long"`) {
		t.Fatalf("an earlier import's payload moves into the native columns: %q %q %d %q %q %q %q", harness, model, attention, answers, questions, plan, pages)
	}
	if err := db.QueryRow(`SELECT author_kind,author_id,COALESCE(proposed_answers_json,''),COALESCE(visual_plan_json,''),needs_attention FROM message WHERE id='msg_plain'`).Scan(&authorKind, &authorID, &answers, &plan, &attention); err != nil {
		t.Fatal(err)
	}
	if authorKind != "user" || authorID != "alice" || answers != "" || plan != "" || attention != 0 {
		t.Fatalf("an unmapped human keeps their name as a user author, empty fields stay NULL: %s %s %q %q %d", authorKind, authorID, answers, plan, attention)
	}
	var choice string
	if err := db.QueryRow(`SELECT state FROM import_v1_choice WHERE id=1`).Scan(&choice); err != nil || choice != "done" {
		t.Fatalf("an install that already imported records the choice as done: %q %v", choice, err)
	}
	var legacyColumns int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('message') WHERE name IN ('legacy_payload_json','legacy_author_label'))
		+ (SELECT COUNT(*) FROM pragma_table_info('attachment') WHERE name='legacy_metadata_json')`).Scan(&legacyColumns); err != nil || legacyColumns != 0 {
		t.Fatalf("obsolete message or attachment columns=%d err=%v", legacyColumns, err)
	}
	var memoryColumns int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('project_memory') WHERE name IN ('legacy_payload_json','legacy_source_message_id','active'))
		+ (SELECT COUNT(*) FROM pragma_table_info('global_memory') WHERE name IN ('legacy_payload_json','legacy_source_message_id'))`).Scan(&memoryColumns); err != nil || memoryColumns != 0 {
		t.Fatalf("obsolete memory columns=%d err=%v", memoryColumns, err)
	}
	var untouchedV3 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM global_memory WHERE id='knw_guntouched_v3' AND active=1`).Scan(&untouchedV3); err != nil || untouchedV3 != 1 {
		t.Fatalf("an untouched import whose v1 version is 3 becomes active: %d %v", untouchedV3, err)
	}
	var editedActive, reonActive int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM global_memory WHERE id='knw_gimported_edited' AND active=1), (SELECT COUNT(*) FROM global_memory WHERE id='knw_gimported_reon' AND active=1)`).Scan(&editedActive, &reonActive); err != nil || editedActive != 1 || reonActive != 1 {
		t.Fatalf("an imported record the owner only edited, and one whose latest event is a re-activation, become ordinary: %d %d %v", editedActive, reonActive, err)
	}
	var importedThenOff int
	if err := db.QueryRow(`SELECT COUNT(*) FROM global_memory WHERE id='knw_gimported_off' AND active=0`).Scan(&importedThenOff); err != nil || importedThenOff != 1 {
		t.Fatalf("an imported record the owner switched off must not be switched back on: %d %v", importedThenOff, err)
	}
	var pendingFact, pendingNote, parked int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM project_memory WHERE id='knw_pending' AND body='pending fact' AND rev=2),
		(SELECT COUNT(*) FROM global_memory WHERE id='knw_gpending' AND active=1),
		(SELECT COUNT(*) FROM global_memory WHERE id='knw_gparked' AND active=0)`).Scan(&pendingFact, &pendingNote, &parked); err != nil || pendingFact != 1 || pendingNote != 1 || parked != 1 {
		t.Fatalf("imported memory becomes ordinary (fact=%d note=%d), an owner-parked record stays parked (%d) err=%v", pendingFact, pendingNote, parked, err)
	}
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name='room_worktree_instance'`).Scan(&table); err != nil || table != "room_worktree_instance" {
		t.Fatalf("room worktree migration missing: %v", err)
	}
}

func TestMigrationTwentySevenUpgradesExistingTwentySixDatabase(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-26.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:26] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, migration.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_existing','Existing','/tmp/existing','now')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_existing','prj_existing',0,'home','Existing','system','now')`,
		`INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_existing',1,'room_existing','system','system','before migration','[]','now')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(context.WithValue(context.Background(), keepHistoryUnnumbered{}, true), db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	var migrated27 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_version WHERE version=27`).Scan(&migrated27); err != nil || migrated27 != 1 {
		t.Fatalf("migration 27 missing: count=%d err=%v", migrated27, err)
	}
	for _, table := range []string{"message", "outbox"} {
		rows, err := db.Query(`SELECT visual_plan_json, appendix_pages_json FROM ` + table + ` LIMIT 0`)
		if err != nil {
			t.Fatalf("%s visual columns: %v", table, err)
		}
		rows.Close()
	}
	var body string
	var visualPlan, appendix sql.NullString
	if err := db.QueryRow(`SELECT body,visual_plan_json,appendix_pages_json FROM message WHERE id='msg_existing'`).Scan(&body, &visualPlan, &appendix); err != nil || body != "before migration" || visualPlan.Valid || appendix.Valid {
		t.Fatalf("existing message body=%q visual=%v appendix=%v err=%v", body, visualPlan, appendix, err)
	}
	roles, err := allLiveRoles(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, roleID := range []string{"role_leader_default", "role_worker_default", "role_reviewer_default"} {
		var roleBody string
		for _, role := range roles {
			if role.ID == roleID {
				roleBody = role.Body
			}
		}
		if !strings.Contains(roleBody, "substantive decisions") {
			t.Fatalf("role %s body lacks delivery guidance: %q", roleID, roleBody)
		}
	}
}

func TestMigrationTwentyTwoPreservesExistingAttachment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "from-21.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:21] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, migration.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','old','/tmp/old',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_old','prj_old','hash','image/png',1,'old.png','owner',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	var filename, oldPath string
	if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != migrations[len(migrations)-1].version {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := s.db.QueryRow(`SELECT filename,path FROM attachment WHERE id='att_old'`).Scan(&filename, &oldPath); err != nil || filename != "" || oldPath != "old.png" {
		t.Fatalf("existing attachment filename=%q path=%q err=%v", filename, oldPath, err)
	}
}

func TestSafeAttachmentName(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"../../notes.txt":        "notes.txt",
		`C:\\secrets\\notes.txt`: "notes.txt",
		"\x00\n":                 "attachment",
		"":                       "attachment",
	} {
		if got := safeAttachmentName(input); got != want {
			t.Errorf("safeAttachmentName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMigrationFourteenBackfillsHomeAndScopeHold(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "from-13.sqlite")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:13] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version(version,applied_at) VALUES (?,?)`, migration.version, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_old','old','/tmp/old','2026-10-13T00:00:00.000000000Z')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_home','prj_old',0,'task','home','usr_old','2026-10-13T00:00:00.000000000Z')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('room_other','prj_old',0,'task','other','usr_old','2026-10-13T00:00:00.000000000Z')`,
		`INSERT INTO machine(id,name,key_hash,approved) VALUES ('mch_old','host','hash',1)`,
		`INSERT INTO checkout(id,project_id,machine_id,canonical_path) VALUES ('chk_old','prj_old','mch_old','/tmp/old')`,
		`INSERT INTO agent(id,slug,project_id,role_id,tool,variant,model,provider,machine_id,checkout_id,key_hash,created_by,created_at) VALUES ('agt_old','old','prj_old','role_worker_default','fake','test','test','fake','mch_old','chk_old','hash','usr_old','2026-10-13T00:00:00.000000000Z')`,
		`INSERT INTO room_member(room_id,member_kind,member_id,joined_at) VALUES ('room_other','agent','agt_old','2026-10-13T00:00:00.000000000Z')`,
		`INSERT INTO room_member(room_id,member_kind,member_id,joined_at) VALUES ('room_home','agent','agt_old','2026-10-13T00:00:01.000000000Z')`,
		`INSERT INTO event(id,seq,kind,project_id,room_id,agent_id,payload_json,created_at) VALUES ('evt_spawn',1,'agent_spawn_requested','prj_old','room_home','agt_old','{}','2026-10-13T00:00:01.000000000Z')`,
		`INSERT INTO scope_control_agent(scope_kind,scope_id,agent_id,restore_desired,bulk_desired,bulk_rev) VALUES ('room','room_home','agt_old','active','not_accepting',1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var home, hold string
	if err := s.db.QueryRow(`SELECT home_room_id FROM agent WHERE id='agt_old'`).Scan(&home); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT hold_desired FROM scope_control_agent WHERE agent_id='agt_old'`).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if home != "room_home" || hold != "not_accepting" {
		t.Fatalf("backfill home=%q hold=%q", home, hold)
	}
}

func testStore(t *testing.T, options Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aimebu.sqlite")
	if err := os.WriteFile(path, blankTestDB, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func fixture(t *testing.T, s *Store) (Project, Room, Message) {
	t.Helper()
	ctx := context.Background()
	project, err := s.CreateProject(ctx, Project{Name: "test", Folder: "/tmp/test"})
	if err != nil {
		t.Fatal(err)
	}
	room, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: "Home", CreatedBy: "usr_test"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	return project, room, message
}

func TestRoomsForProjectIncludesReadOnlySidebarFacts(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	project, room, _ := fixture(t, s)
	if err := s.AddMember(context.Background(), room.ID, "agent", "agent_example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "user", AuthorID: "usr_test", Body: "most recent\npreview"}); err != nil {
		t.Fatal(err)
	}
	rooms, err := s.RoomsForProject(context.Background(), project.ID)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms=%+v, err=%v", rooms, err)
	}
	if rooms[0].MemberCount != 1 || rooms[0].MessageCount != 2 || rooms[0].LastMessageBody != "most recent\npreview" {
		t.Fatalf("sidebar facts=%+v", rooms[0])
	}
}

// A reply carries the first message of its conversation as thread_root, derived at insert and stored; one a caller passes in is dropped.
func TestMessageThreadRootPersists(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	_, room, root := fixture(t, s)
	reply, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: "reply", ReplyTo: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.MessageByID(context.Background(), reply.ID)
	if err != nil || stored.ThreadRoot != root.ID {
		t.Fatalf("MessageByID=%+v, %v", stored, err)
	}
	messages, err := s.ListMessagesAfter(context.Background(), room.ID, 0)
	if err != nil || len(messages) != 2 || messages[1].ThreadRoot != root.ID {
		t.Fatalf("ListMessagesAfter=%+v, %v", messages, err)
	}
}

func TestMigrationFromZeroAndReopen(t *testing.T) {
	t.Parallel()
	s, path := testStore(t, Options{})
	var version, foreignKeys, busyTimeout, synchronous int
	var journal string
	for _, check := range []struct {
		query string
		dest  any
	}{
		{`SELECT MAX(version) FROM schema_version`, &version},
		{`PRAGMA foreign_keys`, &foreignKeys},
		{`PRAGMA busy_timeout`, &busyTimeout},
		{`PRAGMA journal_mode`, &journal},
		{`PRAGMA synchronous`, &synchronous},
	} {
		if err := s.db.QueryRow(check.query).Scan(check.dest); err != nil {
			t.Fatal(err)
		}
	}
	if version != migrations[len(migrations)-1].version || foreignKeys != 1 || busyTimeout != 5000 || journal != "wal" || synchronous != 1 {
		t.Fatalf("migration/pragmas = %d, %d, %d, %s, synchronous %d", version, foreignKeys, busyTimeout, journal, synchronous)
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"schema_version", "seq_counter", "project", "room", "room_member", "user", "membership", "auth_session", "message", "attachment", "reaction", "role", "event", "settings_project", "agent", "session", "turn", "turn_note", "note", "outbox", "receipt", "request_key", "inbox_item", "usage_record", "machine", "checkout", "provider_account", "dispatch_switch_gate", "macro"} {
		if !tables[name] {
			t.Errorf("missing table %s", name)
		}
	}
	fixture(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&count); err != nil || count != len(migrations) {
		t.Fatalf("reopen version rows = %d, %v", count, err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM project WHERE system=0`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reopen project rows = %d, %v", count, err)
	}
}

func TestDSNPragmasOnEveryConnection(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "connections.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	ctx := context.Background()
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, conn := range []*sql.Conn{first, second} {
		var foreignKeys, busyTimeout int
		var journal string
		if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		var synchronous int
		if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		// 1 is NORMAL (0 is OFF, 2 is FULL).
		if foreignKeys != 1 || busyTimeout != 5000 || journal != "wal" || synchronous != 1 {
			t.Fatalf("connection pragmas = %d, %d, %q, synchronous %d", foreignKeys, busyTimeout, journal, synchronous)
		}
	}
}

func TestMigrationTwoToThreePreservesSessions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "from-v2.sqlite")
	db, err := sql.Open("sqlite", fixtureDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE schema_version(version INTEGER PRIMARY KEY CHECK(version>0), applied_at TEXT NOT NULL)`,
		migrations[0].sql,
		`INSERT INTO schema_version VALUES (1,'2026-10-13T12:00:00.000000000Z')`,
		migrations[1].sql,
		`INSERT INTO schema_version VALUES (2,'2026-10-13T12:00:00.000000000Z')`,
		`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_old','old','hash','{}','2026-10-13T12:00:00.000000000Z')`,
		`INSERT INTO auth_session(id,user_id,csrf_token,expires_at) VALUES ('ses_old','usr_old','csrf','2026-10-20T12:00:00.000000000Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var oldHash sql.NullString
	if err := s.db.QueryRow(`SELECT token_hash FROM auth_session WHERE id='ses_old'`).Scan(&oldHash); err != nil || oldHash.Valid {
		t.Fatalf("old session after migration = %+v, %v", oldHash, err)
	}
	if _, err := s.CreateAuthSession(context.Background(), AuthSession{UserID: "usr_old", TokenHash: "hash_1", CSRFToken: "csrf", ExpiresAt: "2026-10-20T12:00:00.000000000Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAuthSession(context.Background(), AuthSession{UserID: "usr_old", TokenHash: "hash_1", CSRFToken: "csrf", ExpiresAt: "2026-10-20T12:00:00.000000000Z"}); err == nil {
		t.Fatal("duplicate token hash accepted")
	}
}

func TestTxContextRejectsStoreReentry(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	err := s.Tx(context.Background(), func(tx *Tx) error {
		if _, err := s.CreateProject(tx.Context(), Project{Name: "nested"}); !errors.Is(err, ErrTxReentry) {
			t.Fatalf("nested write = %v", err)
		}
		if _, err := s.ListMessagesAfter(tx.Context(), "room_any", 0); !errors.Is(err, ErrTxReentry) {
			t.Fatalf("nested read = %v", err)
		}
		return s.Tx(tx.Context(), func(*Tx) error { return nil })
	})
	if !errors.Is(err, ErrTxReentry) {
		t.Fatalf("nested Tx = %v", err)
	}
}

func TestProjectNullableRepoAndInstructionsDefault(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	project, err := s.CreateProject(context.Background(), Project{Name: "without git"})
	if err != nil {
		t.Fatal(err)
	}
	var repo sql.NullString
	if err := s.db.QueryRow(`SELECT repo_url FROM project WHERE id=?`, project.ID).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	if repo.Valid {
		t.Fatalf("empty repository URL stored as %q instead of NULL", repo.String)
	}
	if _, err := s.db.Exec(`INSERT INTO project(id,name,folder,created_at) VALUES ('prj_default','default','','now')`); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaConstraints(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	project, room, message := fixture(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_1','login','hash','{}','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO role(id,key,label,body,role_key) VALUES ('role_1','worker','Worker','body','worker')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO reaction(message_id,member_kind,member_id,emoji,created_at) VALUES (?,?,?,?,?)`, message.ID, "user", "usr_1", "👍", "now"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, query string
		args        []any
	}{
		{"project gate", `UPDATE project SET gate='bad' WHERE id=?`, []any{project.ID}},
		{"project revision", `UPDATE project SET gate_rev=-1 WHERE id=?`, []any{project.ID}},
		{"room kind", `UPDATE room SET kind='bad' WHERE id=?`, []any{room.ID}},
		{"room lifecycle", `UPDATE room SET lifecycle='bad' WHERE id=?`, []any{room.ID}},
		{"room gate", `UPDATE room SET gate='bad' WHERE id=?`, []any{room.ID}},
		{"room lifecycle revision", `UPDATE room SET lifecycle_rev=-1 WHERE id=?`, []any{room.ID}},
		{"room depth", `UPDATE room SET depth=6 WHERE id=?`, []any{room.ID}},
		{"room project FK", `UPDATE room SET project_id='prj_missing' WHERE id=?`, []any{room.ID}},
		{"room parent FK", `UPDATE room SET parent_id='room_missing' WHERE id=?`, []any{room.ID}},
		{"member enum", `INSERT INTO room_member VALUES (?,'bad','x','now',NULL)`, []any{room.ID}},
		{"member room FK", `INSERT INTO room_member VALUES ('room_missing','user','x','now',NULL)`, nil},
		{"user login unique", `INSERT INTO user VALUES ('usr_2','login','hash','{}','now')`, nil},
		{"membership enum", `INSERT INTO membership VALUES (?,'usr_1','bad')`, []any{project.ID}},
		{"membership project FK", `INSERT INTO membership VALUES ('prj_missing','usr_1','owner')`, nil},
		{"membership user FK", `INSERT INTO membership VALUES (?,'usr_missing','owner')`, []any{project.ID}},
		{"session user FK", `INSERT INTO auth_session VALUES ('ses_1','usr_missing','csrf','now',NULL)`, nil},
		{"message author enum", `UPDATE message SET author_kind='bad' WHERE id=?`, []any{message.ID}},
		{"message seq unique", `INSERT INTO message(id,seq,room_id,author_kind,author_id,body,targets_json,created_at) VALUES ('msg_2',? ,?,'system','system','x','[]','now')`, []any{message.Seq, room.ID}},
		{"message room FK", `UPDATE message SET room_id='room_missing' WHERE id=?`, []any{message.ID}},
		{"message reply FK", `UPDATE message SET reply_to='msg_missing' WHERE id=?`, []any{message.ID}},
		{"message thread FK", `UPDATE message SET thread_root='msg_missing' WHERE id=?`, []any{message.ID}},
		{"attachment size", `INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_1',?,'hash','image/png',-1,'p','u','now')`, []any{project.ID}},
		{"attachment project FK", `INSERT INTO attachment(id,project_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_3','prj_missing','hash','image/png',1,'p','u','now')`, nil},
		{"attachment room FK", `INSERT INTO attachment(id,project_id,room_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_4',?,'room_missing','hash','image/png',1,'p','u','now')`, []any{project.ID}},
		{"attachment message FK", `INSERT INTO attachment(id,project_id,message_id,sha256,mime,size,path,created_by,created_at) VALUES ('att_2',?,'msg_missing','hash','image/png',1,'p','u','now')`, []any{project.ID}},
		{"reaction enum", `INSERT INTO reaction VALUES (?,'bad','x','x','now')`, []any{message.ID}},
		{"reaction unique", `INSERT INTO reaction VALUES (?,'user','usr_1','👍','now')`, []any{message.ID}},
		{"reaction message FK", `INSERT INTO reaction VALUES ('msg_missing','user','usr_1','x','now')`, nil},
		{"role key unique", `INSERT INTO role(id,key,label,body,role_key) VALUES ('role_2','worker','Worker','body','worker')`, nil},
		{"role boolean", `UPDATE role SET cap_can_spawn=2 WHERE id='role_1'`, nil},
		{"role lead boolean", `UPDATE role SET cap_is_lead=2 WHERE id='role_1'`, nil},
		{"role writer boolean", `UPDATE role SET cap_singleton_writer=2 WHERE id='role_1'`, nil},
		{"team project FK", `INSERT INTO team VALUES ('team_1','prj_missing','name','[]')`, nil},
		{"event seq unique", `INSERT INTO event(id,seq,kind,payload_json,created_at) VALUES ('evt_2',1,'x','{}','now')`, nil},
		{"event room FK", `INSERT INTO event(id,seq,kind,room_id,payload_json,created_at) VALUES ('evt_3',999,'x','room_missing','{}','now')`, nil},
		{"event project FK", `INSERT INTO event(id,seq,kind,project_id,payload_json,created_at) VALUES ('evt_4',999,'x','prj_missing','{}','now')`, nil},
		{"settings boolean", `UPDATE settings_project SET auto_resume_after_clean_diagnostic=2 WHERE project_id=?`, []any{project.ID}},
		{"settings limit", `UPDATE settings_project SET max_live_agents=0 WHERE project_id=?`, []any{project.ID}},
		{"settings spawn rate", `UPDATE settings_project SET spawn_rate_per_min=0 WHERE project_id=?`, []any{project.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, tc.query, tc.args...); err == nil {
				t.Fatal("invalid row accepted")
			}
		})
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_1"); err != nil {
		t.Fatal(err)
	}
	var eventProject string
	if err := s.db.QueryRow(`SELECT project_id FROM event WHERE kind='room_member_joined' ORDER BY seq DESC LIMIT 1`).Scan(&eventProject); err != nil || eventProject != project.ID {
		t.Fatalf("join event project = %q, %v", eventProject, err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_1"); err == nil {
		t.Fatal("duplicate active member accepted")
	}
	if err := s.RemoveMember(ctx, room.ID, "user", "usr_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT project_id FROM event WHERE kind='room_member_left' ORDER BY seq DESC LIMIT 1`).Scan(&eventProject); err != nil || eventProject != project.ID {
		t.Fatalf("leave event project = %q, %v", eventProject, err)
	}
	if err := s.AddMember(ctx, room.ID, "user", "usr_1"); err != nil {
		t.Fatalf("rejoin after leave: %v", err)
	}
	if _, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "dm", DMKey: "same", Title: "a", CreatedBy: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "dm", DMKey: "same", Title: "b", CreatedBy: "u"}); err == nil {
		t.Fatal("duplicate DM key accepted")
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", SourceOutID: "out_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMessage(ctx, Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", SourceOutID: "out_1"}); err == nil {
		t.Fatal("duplicate source_out_id accepted")
	}
}

func TestSeqAcrossConcurrentMessagesAndEvents(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	_, room, _ := fixture(t, s)
	const writers = 32
	var wg sync.WaitGroup
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.InsertMessage(context.Background(), Message{RoomID: room.ID, AuthorKind: "system", AuthorID: "system", Body: fmt.Sprint(i)})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.Query(`SELECT seq FROM message UNION ALL SELECT seq FROM event`)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	sort.Ints(seqs)
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("global seq[%d] = %d", i, seq)
		}
	}
	messages, err := s.ListMessagesAfter(context.Background(), room.ID, 0)
	if err != nil || len(messages) != writers+1 {
		t.Fatalf("listed %d messages, %v", len(messages), err)
	}
	filtered, err := s.ListMessagesAfter(context.Background(), room.ID, messages[0].Seq)
	if err != nil || len(filtered) != writers {
		t.Fatalf("filtered %d messages, %v", len(filtered), err)
	}
}

func TestTransactionRollbackIncludesEventAndSequence(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	want := errors.New("forced failure")
	err := s.Tx(context.Background(), func(tx *Tx) error {
		if _, err := tx.CreateProject(Project{Name: "rolled back"}); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Tx error = %v", err)
	}
	for _, table := range []string{"project", "settings_project", "event"} {
		var count int
		// The reserved project for chats between agents is part of the schema, not of any transaction under test.
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table + map[string]string{"project": ` WHERE system=0`, "settings_project": ` WHERE project_id!='` + AgentDMProjectID + `'`}[table]).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s has %d rows after rollback: %v", table, count, err)
		}
	}
	var seq int
	if err := s.db.QueryRow(`SELECT value FROM seq_counter WHERE id=1`).Scan(&seq); err != nil || seq != 0 {
		t.Fatalf("counter after rollback = %d, %v", seq, err)
	}
}

func TestRoomDepthLimit(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	ctx := context.Background()
	project, err := s.CreateProject(ctx, Project{Name: "depth"})
	if err != nil {
		t.Fatal(err)
	}
	parent := ""
	for depth := 0; depth <= defaultMaxDepth; depth++ {
		room, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: parent, Kind: "task", Title: "task", CreatedBy: "usr_1"})
		if err != nil || room.Depth != depth {
			t.Fatalf("depth %d: room=%+v error=%v", depth, room, err)
		}
		parent = room.ID
	}
	_, err = s.CreateRoom(ctx, Room{ProjectID: project.ID, ParentID: parent, Kind: "task", Title: "too deep", CreatedBy: "usr_1"})
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("depth overflow = %v", err)
	}
	if _, err := Open(ctx, filepath.Join(t.TempDir(), "bad.sqlite"), Options{MaxRoomDepth: 6}); err == nil {
		t.Fatal("hard max of five accepted six")
	}
}

// A database that already ran the earlier migration 50 has every imported global record switched back on, the
// ones the owner switched off included. Migration 51 puts the owner's choice back from the record's own events.
func TestMigrationFiftyOneRestoresRecordsTheOwnerSwitchedOff(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "from-50.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:50] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
	}
	event := func(seq int, id, active string) string {
		return fmt.Sprintf(`INSERT INTO event(id,seq,kind,payload_json,created_at) VALUES ('evt_%d',%d,'global_knowledge_updated','{"id":"%s","scope":"agent_shared_notes","active":"%s"}','now')`, seq, seq, id, active)
	}
	row := func(id string, active int) string {
		return fmt.Sprintf(`INSERT INTO global_memory(id,scope,scope_key,body,rev,author_kind,author_id,active,created_at,updated_at) VALUES ('%s','agent_shared_notes','global','%s',2,'user','usr_x',%d,'now','now')`, id, id, active)
	}
	for _, statement := range []string{
		`INSERT INTO schema_version(version,applied_at) VALUES (50,'now')`,
		// What the earlier migration 50 left: active, although the owner's latest event is a deactivation.
		row("knw_flipped", 1), event(9001, "knw_flipped", "false"),
		// Switched off, then on again: the owner's latest choice is on.
		row("knw_back_on", 1), event(9002, "knw_back_on", "false"), event(9003, "knw_back_on", "true"),
		// Never switched off: no event, stays active.
		row("knw_plain", 1),
		// Switched on after being off by an earlier import: no change either way.
		row("knw_parked", 0), event(9004, "knw_parked", "false"),
		// Another record's event must not touch this one.
		row("knw_other", 1), event(9005, "knw_unrelated", "false"),
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	active := map[string]int{}
	rows, err := db.Query(`SELECT id,active FROM global_memory`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var on int
		if err := rows.Scan(&id, &on); err != nil {
			t.Fatal(err)
		}
		active[id] = on
	}
	rows.Close()
	want := map[string]int{"knw_flipped": 0, "knw_back_on": 1, "knw_plain": 1, "knw_parked": 0, "knw_other": 1}
	for id, on := range want {
		if active[id] != on {
			t.Errorf("%s: active=%d, want %d (all: %v)", id, active[id], on, active)
		}
	}
	// Idempotent: a second pass changes nothing.
	if _, err := db.Exec(migrations[50].sql); err != nil {
		t.Fatal(err)
	}
	var flipped int
	if err := db.QueryRow(`SELECT active FROM global_memory WHERE id='knw_flipped'`).Scan(&flipped); err != nil || flipped != 0 {
		t.Fatalf("running the migration again must not change the result: %d %v", flipped, err)
	}
}
