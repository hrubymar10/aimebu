package store

import (
	"context"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestDailyUsageLimitWarnsPausesAndContinuesOnce(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_owner") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE turn SET exec='running',started_at=? WHERE id=?`, timestamp(), f.turn.ID); err != nil {
		t.Fatal(err)
	}
	cap := 10.0
	err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.UpdateUsageLimit(f.project.ID, "usr_owner", UsageLimitInput{CostMode: "custom", Cost: &cap, TurnMode: "none"}, 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	addCost := func(id string, amount float64) {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{"usage": map[string]any{"cost_estimate": amount}})
		if err := s.RecordTurnEvent(ctx, f.turn.ID, f.agent.ID, id, "usage", "", frame); err != nil {
			t.Fatal(err)
		}
	}
	addCost("use_8", 8)
	items, err := s.InboxForUser(ctx, "usr_owner")
	if err != nil || len(items) != 1 || items[0].Kind != "usage_info" {
		t.Fatalf("warning items=%+v err=%v", items, err)
	}
	addCost("use_2", 2)
	items, err = s.InboxForUser(ctx, "usr_owner")
	if err != nil || len(items) != 1 || items[0].Kind != "usage_limit" || len(items[0].Options) != 2 {
		t.Fatalf("limit items=%+v err=%v", items, err)
	}
	limit, err := s.UsageLimit(ctx, f.project.ID)
	if err != nil || limit.DailyCost != 10 || limit.DailyTurns != 1 || limit.CostCoverage != "known" || limit.EffectiveState != "paused" || limit.DayStart == "" || limit.TZ == "" {
		t.Fatalf("limit=%+v err=%v", limit, err)
	}
	control, err := s.ScopeControl(ctx, "project", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ControlScope("project", f.project.ID, "resume", "usr_owner", control.Rev)
		return err
	}); err != ErrInvalidTransition {
		t.Fatalf("manual Resume bypassed budget gate: %v", err)
	}
	decision, reused, err := s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "continue-1", "continue", int(items[0].Rev))
	if err != nil || reused || decision.Choice != "continue" {
		t.Fatalf("continue=%+v reused=%t err=%v", decision, reused, err)
	}
	_, reused, err = s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "continue-1", "continue", int(items[0].Rev))
	if err != nil || !reused {
		t.Fatalf("replay reused=%t err=%v", reused, err)
	}
	limit, err = s.UsageLimit(ctx, f.project.ID)
	if err != nil || limit.DailyCostCap == nil || *limit.DailyCostCap != 20 || limit.EffectiveState != "active" {
		t.Fatalf("raised limit=%+v err=%v", limit, err)
	}
	addCost("use_10", 10)
	items, err = s.InboxForUser(ctx, "usr_owner")
	if err != nil || len(items) != 1 || items[0].Kind != "usage_limit" || len(items[0].Options) != 1 {
		t.Fatalf("second limit items=%+v err=%v", items, err)
	}
	if _, _, err := s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "continue-2", "continue", int(items[0].Rev)); err == nil {
		t.Fatal("second Continue succeeded")
	}
	if _, _, err := s.DecideUsageLimit(ctx, items[0].ID, "usr_owner", "abandon-1", "abandon", int(items[0].Rev)); err != nil {
		t.Fatal(err)
	}
	control, err = s.ScopeControl(ctx, "project", f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.ControlScope("project", f.project.ID, "pause", "usr_owner", control.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	previous := time.Now().In(time.Local).AddDate(0, 0, -1)
	oldStart, _, _ := localUsageDay(previous)
	currentStart, _, _ := localUsageDay(time.Now())
	if _, err := s.db.Exec(`UPDATE usage_limit_day SET day_start=? WHERE scope_kind='project' AND scope_id=? AND day_start=?`,
		oldStart.UTC().Format(timestampLayout), f.project.ID, currentStart.UTC().Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceUsageDays(ctx); err != nil {
		t.Fatal(err)
	}
	var gate string
	if err := s.db.QueryRow(`SELECT gate FROM project WHERE id=?`, f.project.ID).Scan(&gate); err != nil || gate != "paused" {
		t.Fatalf("rollover gate=%q err=%v", gate, err)
	}
}

func TestUnknownCostCountsTurnsAndUsesServerLocalDay(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	oldLocal := time.Local
	time.Local = location
	t.Cleanup(func() { time.Local = oldLocal })
	start, end, tz := localUsageDay(time.Date(2026, time.October, 25, 2, 30, 0, 0, time.UTC))
	if tz != "Europe/Berlin" || start.Format(time.RFC3339) != "2026-10-25T00:00:00+02:00" || end.Sub(start) != 25*time.Hour {
		t.Fatalf("local day=%s to %s tz=%s", start, end, tz)
	}
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_owner','owner','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if err := s.Tx(ctx, func(tx *Tx) error { return tx.AddProjectOwner(f.project.ID, "usr_owner") }); err != nil {
		t.Fatal(err)
	}
	turnCap := int64(1)
	err = s.Tx(ctx, func(tx *Tx) error {
		_, err := tx.UpdateUsageLimit(f.project.ID, "usr_owner", UsageLimitInput{CostMode: "none", TurnMode: "custom", Turns: &turnCap}, 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, f.room.ID, "agent", f.agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTurn(ctx, Admission{TurnID: f.turn.ID, AgentID: f.agent.ID,
		MachineID: f.agent.MachineID, Generation: 0, DesiredRev: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTurn(ctx, f.turn.ID, f.agent.ID, 0, "accepted"); err != nil {
		t.Fatal(err)
	}
	limit, err := s.UsageLimit(ctx, f.project.ID)
	if err != nil || limit.DailyTurns != 1 || limit.DailyCost != 0 || limit.CostCoverage != "unknown" || limit.EffectiveState != "paused" {
		t.Fatalf("unknown-cost limit=%+v err=%v", limit, err)
	}
}

func TestUsageInboxRespectsDMMemberAndOwnerVisibility(t *testing.T) {
	t.Parallel()
	s, _ := testStore(t, Options{})
	f := seedExecution(t, s)
	ctx := context.Background()
	for _, user := range []struct{ id, role string }{{"usr_peer", "owner"}, {"usr_other", "operator"}, {"usr_viewer", "viewer"}} {
		if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES (?,?,'hash','{}',?)`, user.id, user.id, timestamp()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO membership(project_id,user_id,role) VALUES (?,?,?)`, f.project.ID, user.id, user.role); err != nil {
			t.Fatal(err)
		}
	}
	dm, err := s.OpenDM(ctx, "usr_peer", f.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, room string }{{"inb_project_usage", f.room.ID}, {"inb_dm_usage", dm.ID}} {
		if _, err := s.db.Exec(`INSERT INTO inbox_item(id,project_id,room_id,requester_kind,requester_id,kind,options_json,created_at)
			VALUES (?,?,?,'system','test','usage_limit','{"Text":"limit"}',?)`, item.id, f.project.ID, item.room, timestamp()); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		user string
		want int
	}{{"usr_peer", 2}, {"usr_other", 1}, {"usr_viewer", 0}} {
		items, err := s.InboxForUser(ctx, check.user)
		if err != nil || len(items) != check.want {
			t.Fatalf("%s inbox=%+v err=%v", check.user, items, err)
		}
	}
}
