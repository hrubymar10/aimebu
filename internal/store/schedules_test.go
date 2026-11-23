package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduleWakeCatchupAndSkipAggregation(t *testing.T) {
	t.Parallel()
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var item Schedule
	err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		item, e = tx.CreateSchedule(Schedule{RoomID: room.ID, ProjectID: project.ID, CreatorKind: "user", CreatorID: "owner", TargetAgentID: worker.ID, Body: "do X", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, now)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.AdvanceSchedules(ctx, now.Add(16*time.Minute))
	if err != nil || count != 1 {
		t.Fatalf("catchup count=%d err=%v", count, err)
	}
	var runs int
	var messageID string
	if err := s.db.QueryRow(`SELECT count(*),max(message_id) FROM schedule_run WHERE schedule_id=?`, item.ID).Scan(&runs, &messageID); err != nil || runs != 1 || messageID == "" {
		t.Fatalf("runs=%d message=%q err=%v", runs, messageID, err)
	}
	if _, err := s.NoteForAgentMessage(ctx, worker.ID, messageID); err != nil {
		t.Fatalf("target did not wake: %v", err)
	}
	var next string
	if err := s.db.QueryRow(`SELECT next_run_at FROM schedule WHERE id=?`, item.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	due, _ := time.Parse(timestampLayout, next)
	if !due.After(now.Add(16 * time.Minute)) {
		t.Fatalf("next=%s", next)
	}
	if _, err := s.db.Exec(`UPDATE room SET gate='paused' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceSchedules(ctx, due); err != nil {
		t.Fatal(err)
	}
	var firstNotice string
	if err := s.db.QueryRow(`SELECT message_id FROM schedule_run WHERE schedule_id=? AND outcome='skipped'`, item.ID).Scan(&firstNotice); err != nil || firstNotice == "" {
		t.Fatalf("first skip notice=%q err=%v", firstNotice, err)
	}
	if _, err := s.AdvanceSchedules(ctx, due.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var repeatedNotices int
	if err := s.db.QueryRow(`SELECT count(message_id) FROM schedule_run WHERE schedule_id=? AND outcome='skipped'`, item.ID).Scan(&repeatedNotices); err != nil || repeatedNotices != 1 {
		t.Fatalf("repeated notices=%d err=%v", repeatedNotices, err)
	}
	if _, err := s.db.Exec(`UPDATE room SET gate='open' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceSchedules(ctx, due.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var outcome string
	if err := s.db.QueryRow(`SELECT last_outcome FROM schedule WHERE id=?`, item.ID).Scan(&outcome); err != nil || outcome != "delivered" {
		t.Fatalf("outcome=%s err=%v", outcome, err)
	}
}

func TestScheduleCivilTimeDST(t *testing.T) {
	t.Parallel()
	zone := "America/New_York"
	for _, tc := range []struct {
		kind, local string
		weekday     int
		after, want string
	}{
		{"daily", "02:30", 0, "2026-03-08T06:59:00Z", "2026-03-08T07:00:00Z"},
		{"weekly", "01:30", 0, "2026-11-01T04:00:00Z", "2026-11-01T05:30:00Z"},
	} {
		after, _ := time.Parse(time.RFC3339, tc.after)
		got, err := NextScheduleTime(Schedule{Kind: tc.kind, Timezone: zone, LocalTime: tc.local, Weekday: tc.weekday}, after)
		if err != nil || got.Format(time.RFC3339) != tc.want {
			t.Fatalf("%s %s got=%s err=%v want=%s", tc.kind, tc.local, got.Format(time.RFC3339), err, tc.want)
		}
	}
}

func TestScheduleCloseDisablesAndReopenDoesNotResume(t *testing.T) {
	t.Parallel()
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,'usr_owner','owner')`, project.ID); err != nil {
		t.Fatal(err)
	}
	var item Schedule
	err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		item, e = tx.CreateSchedule(Schedule{RoomID: room.ID, CreatorKind: "user", CreatorID: "usr_owner", TargetAgentID: worker.ID, Body: "note", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginRoomClose(ctx, room.ID, "usr_owner", 0, false); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := s.db.QueryRow(`SELECT enabled FROM schedule WHERE id=?`, item.ID).Scan(&enabled); err != nil || enabled {
		t.Fatalf("enabled after close=%v err=%v", enabled, err)
	}
	// Reopen requires the normal close transition; the schedule flag itself must
	// remain disabled even if the room lifecycle changes back to open.
	if _, err := s.db.Exec(`UPDATE room SET lifecycle='closed' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.RoomByID(ctx, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenRoom(ctx, room.ID, "usr_owner", current.LifecycleRev); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT enabled FROM schedule WHERE id=?`, item.ID).Scan(&enabled); err != nil || enabled {
		t.Fatalf("enabled after reopen=%v err=%v", enabled, err)
	}
}

// Closing a room disables its schedules and tells the schedule's creator and target, once, in an open room they still share, so a reminder an
// agent relies on does not stop without a word.
func TestScheduleCloseNoticesCreatorAndTarget(t *testing.T) {
	t.Parallel()
	s, project, room, worker, creator := wakeFixture(t)
	ctx := context.Background()
	shared, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "still open", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []Agent{worker, creator} {
		if err := s.AddMember(ctx, shared.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	var item Schedule
	if err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		item, e = tx.CreateSchedule(Schedule{RoomID: room.ID, CreatorKind: "agent", CreatorID: creator.ID, TargetAgentID: worker.ID, Body: "check the queue", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.beginRoomClose(room.ID, creator.ID, 0, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := s.db.QueryRow(`SELECT enabled FROM schedule WHERE id=?`, item.ID).Scan(&enabled); err != nil || enabled {
		t.Fatalf("enabled after close=%v err=%v", enabled, err)
	}
	rows, err := s.db.Query(`SELECT id,room_id,body FROM message WHERE author_kind='system' AND body LIKE '%stopped because its room%'`)
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	var noticeID string
	for rows.Next() {
		var id, roomID, body string
		if err := rows.Scan(&id, &roomID, &body); err != nil {
			t.Fatal(err)
		}
		if roomID != shared.ID || !strings.Contains(body, item.ID) || !strings.Contains(body, "check the queue") {
			t.Errorf("notice in %s: %q, want it in the shared room %s naming the schedule", roomID, body, shared.ID)
		}
		notices, noticeID = append(notices, body), id
	}
	rows.Close()
	if len(notices) != 1 {
		t.Fatalf("notices=%q, want exactly one", notices)
	}
	if got := wakeReasons(t, s, noticeID); !sameWakes(got, map[string]string{"one": "inbox_answer", "lead": "inbox_answer"}) {
		t.Fatalf("the notice woke %v, want the creator and the target", got)
	}
}

// A schedule in a restricted flow room must not leak its text when the room closes: the notice lands in general, which every project member
// may read, so it names only the schedule and the room.
func TestScheduleCloseNoticeFromRestrictedRoomLeavesTextOut(t *testing.T) {
	t.Parallel()
	s, project, room, worker, creator := wakeFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE room SET team_flow_room_key='routing',restricted=1,title='routing' WHERE id=?`, room.ID); err != nil {
		t.Fatal(err)
	}
	general, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "general", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE room SET team_flow_room_key='general' WHERE id=?`, general.ID); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []Agent{worker, creator} {
		if err := s.AddMember(ctx, general.ID, "agent", agent.ID); err != nil {
			t.Fatal(err)
		}
	}
	var item Schedule
	if err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		item, e = tx.CreateSchedule(Schedule{RoomID: room.ID, CreatorKind: "agent", CreatorID: creator.ID, TargetAgentID: worker.ID, Body: "secret routing plan", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.beginRoomClose(room.ID, creator.ID, 0, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var leaked int
	if err := s.db.QueryRow(`SELECT count(*) FROM message WHERE room_id<>? AND body LIKE '%secret routing plan%'`, room.ID).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("schedule text copied out of the restricted room %d times (err=%v)", leaked, err)
	}
	var id, body string
	if err := s.db.QueryRow(`SELECT id,body FROM message WHERE room_id=? AND author_kind='system' AND body LIKE '%stopped because its room%'`, general.ID).Scan(&id, &body); err != nil {
		t.Fatalf("no notice in general: %v", err)
	}
	if !strings.Contains(body, item.ID) || !strings.Contains(body, "routing") {
		t.Fatalf("notice %q, want the schedule ID and the room title", body)
	}
	if got := wakeReasons(t, s, id); !sameWakes(got, map[string]string{"one": "inbox_answer", "lead": "inbox_answer"}) {
		t.Fatalf("the notice woke %v, want the creator and the target", got)
	}
}

// With no shared room, each agent hears in an open room it is a member of. An agent in no open room is skipped: a line in a room it has not
// joined, such as the home room, would wake nobody.
func TestScheduleCloseNoticeSkipsAgentInNoOpenRoom(t *testing.T) {
	t.Parallel()
	s, project, room, worker, creator := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	own, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "task", Title: "creator only", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, own.ID, "agent", creator.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.CreateSchedule(Schedule{RoomID: room.ID, CreatorKind: "agent", CreatorID: creator.ID, TargetAgentID: worker.ID, Body: "check the queue", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, time.Now())
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, e := tx.beginRoomClose(room.ID, creator.ID, 0, false)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT id,room_id FROM message WHERE author_kind='system' AND body LIKE '%stopped because its room%'`)
	if err != nil {
		t.Fatal(err)
	}
	var rooms, ids []string
	for rows.Next() {
		var id, roomID string
		if err := rows.Scan(&id, &roomID); err != nil {
			t.Fatal(err)
		}
		rooms, ids = append(rooms, roomID), append(ids, id)
	}
	rows.Close()
	if len(rooms) != 1 || rooms[0] != own.ID {
		t.Fatalf("notices in %v, want one in the creator's room %s and none in home %s", rooms, own.ID, home.ID)
	}
	if got := wakeReasons(t, s, ids[0]); !sameWakes(got, map[string]string{"lead": "inbox_answer"}) {
		t.Fatalf("the notice woke %v, want only the creator", got)
	}
}

// A home-room schedule for the lead reaches the project lead, the same
// targeted system message and wake note as in a task room. A stopped earlier
// lead still listed in the room must not shadow the active one.
func TestHomeRoomScheduleFiresToActiveLead(t *testing.T) {
	t.Parallel()
	s, project, _, _, lead := wakeFixture(t)
	ctx := context.Background()
	home, err := s.CreateRoom(ctx, Room{ProjectID: project.ID, Kind: "home", Title: project.Name, CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAgent(ctx, Agent{Slug: "lead-two", ProjectID: project.ID, RoleID: "role_lead", Tool: "fake", Variant: "test", Model: "test", Provider: "test", MachineID: "mch_wake", CheckoutID: "chk_wake", KeyHash: "key", CreatedBy: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	// Stop whichever lead sorts first by ID, so a plain ID order would pick it.
	stale, active := lead, other
	if other.ID < lead.ID {
		stale, active = other, lead
	}
	if _, err := s.db.Exec(`UPDATE agent SET desired='stopped' WHERE id=?`, stale.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{stale.ID, active.ID} {
		if err := s.AddMember(ctx, home.ID, "agent", id); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var item Schedule
	if err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		item, e = tx.CreateSchedule(Schedule{RoomID: home.ID, CreatorKind: "user", CreatorID: "owner", TargetLead: true, Body: "summarize the day", Kind: "interval", Timezone: "UTC", IntervalSeconds: 300}, now)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if item.ProjectID != project.ID {
		t.Fatalf("project=%q", item.ProjectID)
	}
	if count, err := s.AdvanceSchedules(ctx, now.Add(6*time.Minute)); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var outcome, reason, messageID string
	if err := s.db.QueryRow(`SELECT outcome,reason,COALESCE(message_id,'') FROM schedule_run WHERE schedule_id=?`, item.ID).Scan(&outcome, &reason, &messageID); err != nil {
		t.Fatal(err)
	}
	if outcome != "delivered" || messageID == "" {
		t.Fatalf("outcome=%s reason=%s message=%q", outcome, reason, messageID)
	}
	if _, err := s.NoteForAgentMessage(ctx, active.ID, messageID); err != nil {
		t.Fatalf("active home lead did not wake: %v", err)
	}
	if _, err := s.NoteForAgentMessage(ctx, stale.ID, messageID); err == nil {
		t.Fatal("stopped lead was woken")
	}
}

// Migration 123 rebuilds schedule to allow kind cron. It must carry every row and its schedule_run history across (dropping the old table
// with foreign keys on would cascade the history away), keep the due index and the foreign keys, and start old rows with no expression.
func TestCronScheduleMigrationKeepsRowsRunsAndIndex(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(SQLDriverName, sqliteDSN(filepath.Join(t.TempDir(), "cron.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := migrateUpTo(ctx, db, 122); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO project(id,name,folder,created_at) VALUES ('p1','one','/tmp/one','2026-01-01T00:00:00Z')`,
		`INSERT INTO room(id,project_id,depth,kind,title,created_by,created_at) VALUES ('r1','p1',0,'home','home','usr','2026-01-01T00:00:00.000000Z')`,
		`INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_lead,body,kind,timezone,local_time,weekday,next_run_at,rev,created_at,updated_at)
		 VALUES ('sch_old','r1','p1','user','usr',1,'daily note','daily','Europe/Prague','08:00',-1,'2026-01-02T07:00:00.000000000Z',3,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO schedule_run(schedule_id,due_at,processed_at,outcome) VALUES ('sch_old','2026-01-01T07:00:00.000000000Z','2026-01-01T07:00:01.000000000Z','delivered')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_lead,body,kind,timezone,next_run_at,created_at,updated_at)
		VALUES ('sch_cron','r1','p1','user','usr',1,'x','cron','UTC','2026-01-02T07:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("version 122 accepted kind cron")
	}
	if err := migrateUpTo(ctx, db, 123); err != nil {
		t.Fatal(err)
	}
	var foreignKeys, violations, runs, rev int
	var kind, cronExpr, nextRun string
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys after migration: %d %v", foreignKeys, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT kind,cron_expr,next_run_at,rev FROM schedule WHERE id='sch_old'`).Scan(&kind, &cronExpr, &nextRun, &rev); err != nil || kind != "daily" || cronExpr != "" || nextRun != "2026-01-02T07:00:00.000000000Z" || rev != 3 {
		t.Fatalf("old row = %q %q %q %d %v", kind, cronExpr, nextRun, rev, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schedule_run WHERE schedule_id='sch_old'`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("schedule_run rows after rebuild = %d %v", runs, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations = %d %v", violations, err)
	}
	var index string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='schedule_due'`).Scan(&index); err != nil || !strings.Contains(index, "schedule(enabled,next_run_at)") {
		t.Fatalf("schedule_due index = %q %v", index, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_lead,body,kind,timezone,cron_expr,next_run_at,created_at,updated_at)
		VALUES ('sch_cron','r1','p1','user','usr',1,'x','cron','UTC','0 8 * * *','2026-01-02T08:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("kind cron after migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schedule(id,room_id,project_id,creator_kind,creator_id,target_lead,body,kind,timezone,next_run_at,created_at,updated_at)
		VALUES ('sch_bad','r1','p1','user','usr',1,'x','hourly','UTC','2026-01-02T08:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	// Deleting the schedule still cascades its runs: the foreign key survived the rebuild.
	if _, err := db.ExecContext(ctx, `DELETE FROM schedule WHERE id='sch_old'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schedule_run`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("runs after delete = %d %v", runs, err)
	}
}

func TestCronScheduleCreateValidateAndCatchUpOnce(t *testing.T) {
	t.Parallel()
	s, project, room, worker, _ := wakeFixture(t)
	ctx := context.Background()
	prague, _ := time.LoadLocation("Europe/Prague")
	now := time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC)
	create := func(item Schedule) (Schedule, error) {
		var created Schedule
		err := s.Tx(ctx, func(tx *Tx) error {
			var e error
			item.RoomID, item.ProjectID, item.CreatorKind, item.CreatorID, item.TargetAgentID = room.ID, project.ID, "user", "owner", worker.ID
			created, e = tx.CreateSchedule(item, now)
			return e
		})
		return created, err
	}
	item, err := create(Schedule{Body: "standup", Kind: "cron", Timezone: "Europe/Prague", CronExpr: "  0 8,12,16,20 * * *  ", LocalTime: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	// 12:00Z on the 28th is 13:00 CET: the next slot is 16:00 CET.
	if item.NextRunAt != "2026-03-28T15:00:00.000000000Z" || item.CronExpr != "0 8,12,16,20 * * *" {
		t.Fatalf("created = %+v", item)
	}
	// The row round-trips through the table.
	listed, err := s.ListSchedules(ctx, room.ID)
	if err != nil || len(listed) != 1 || listed[0].CronExpr != "0 8,12,16,20 * * *" || listed[0].Kind != "cron" {
		t.Fatalf("listed = %+v %v", listed, err)
	}
	// Other kinds carry no expression, whatever the request said.
	daily, err := create(Schedule{Body: "d", Kind: "daily", Timezone: "UTC", LocalTime: "08:00", CronExpr: "* * * * *"})
	if err != nil || daily.CronExpr != "" {
		t.Fatalf("daily = %+v %v", daily, err)
	}
	// A bad expression is refused with the field named, and still matches the sentinel.
	for expr, want := range map[string]string{"": "expression", "60 * * * *": "minute", "* * * * funday": "day-of-week", "0 0 30 feb *": "never runs"} {
		_, err := create(Schedule{Body: "bad", Kind: "cron", Timezone: "UTC", CronExpr: expr})
		if !errors.Is(err, ErrScheduleInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want invalid schedule mentioning %q", expr, err, want)
		}
	}
	// Two days of downtime: one run, then the next slot after now.
	later := now.Add(48 * time.Hour)
	count, err := s.AdvanceSchedules(ctx, later)
	if err != nil || count < 1 {
		t.Fatalf("advance count=%d err=%v", count, err)
	}
	var runs int
	var next string
	if err := s.db.QueryRow(`SELECT (SELECT count(*) FROM schedule_run WHERE schedule_id=?),next_run_at FROM schedule WHERE id=?`, item.ID, item.ID).Scan(&runs, &next); err != nil || runs != 1 {
		t.Fatalf("runs=%d next=%q err=%v", runs, next, err)
	}
	due, _ := time.Parse(timestampLayout, next)
	if !due.After(later) || due.In(prague).Minute() != 0 || due.Sub(later) > 4*time.Hour {
		t.Fatalf("next after catch-up = %s (now %s)", next, later)
	}
	// Pausing and resuming recomputes from now.
	var paused, resumed Schedule
	var rev int64
	if err := s.db.QueryRow(`SELECT rev FROM schedule WHERE id=?`, item.ID).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		var e error
		if paused, e = tx.SetScheduleEnabled(item.ID, room.ID, false, rev, later); e != nil {
			return e
		}
		resumed, e = tx.SetScheduleEnabled(item.ID, room.ID, true, paused.Rev, later.Add(24*time.Hour))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := time.Parse(timestampLayout, resumed.NextRunAt); !got.After(later.Add(24 * time.Hour)) {
		t.Fatalf("resumed next = %s", resumed.NextRunAt)
	}
	// DST through the store: the 02:30 slot on the spring day runs at 03:00 CEST (01:00Z).
	got, err := NextScheduleTime(Schedule{Kind: "cron", Timezone: "Europe/Prague", CronExpr: "30 2 * * *"}, time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC))
	if err != nil || !got.Equal(time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("spring gap = %s %v", got, err)
	}
}
