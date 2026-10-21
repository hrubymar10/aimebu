package store

import (
	"context"
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
