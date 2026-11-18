package store

import (
	"context"
	"testing"
)

// What the bell shows: failures, starts, stops and crashes, limits and credentials; not what is routine.
func TestIsNotableSystemEvent(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"agent_spawn_confirmed", "agent_spawn_failed", "agent_stopped", "agent_revoked", "agent_switch_failed", "agent_turn_budget_exceeded", "turn_lost",
		"turn_never_started", "loop_guard", "credential_alert", "provider_limit_detected", "provider_limit_cleared", "usage_limit_reset", "limit_guard_clear", "quota_reset_seen",
		"quota_hold", "native_session_reset", "agent_context_cleared", "agent_context_compacted", "user_admin_changed", "user_deactivated", "room_close_blocked", "machine_revoked", "notes_dropped"} {
		if !IsNotableSystemEvent(kind, "{}") {
			t.Errorf("%s should be notable", kind)
		}
	}
	for _, kind := range []string{"agent_created", "agent_key_rotated", "agent_role_changed", "agent_spawn_requested", "agent_control_applied", "agent_switch_requested", "agent_switch_starting",
		"agent_switch_completed", "room_created", "room_renamed", "room_closed", "room_member_joined", "project_created", "project_updated", "recovery_decision", "recovery_quiescent",
		"machine_created", "machine_approved", "machine_key_rotated", "user_created", "user_invite_created", "user_invite_redeemed", "message_created", "turn_recorded"} {
		if IsNotableSystemEvent(kind, "{}") {
			t.Errorf("%s is routine", kind)
		}
	}
	cases := []struct {
		kind, payload string
		want          bool
	}{
		{"turn_process_exit", `{"exit_code":"0"}`, false},
		{"turn_process_exit", `{"exit_code":0}`, false},
		{"turn_process_exit", `{"exit_code":"1"}`, true},
		{"turn_process_exit", `{"exit_code":137}`, true},
		{"turn_process_exit", `{}`, false},
		{"turn_process_exit", `{"exit_code":""}`, false},
		{"turn_process_exit", `not json`, false},
		{"agent_control_requested", `{"desired":"active","actor_kind":"user","actor_id":"usr_1"}`, true},
		{"agent_control_requested", `{"desired":"stopped","actor_kind":"user","actor_id":"usr_1"}`, true},
		{"agent_control_requested", `{"desired":"not_accepting","actor_kind":"user","actor_id":"usr_1"}`, true},
		{"agent_control_requested", `{"desired":"active","actor":"usr_1"}`, true},
		{"agent_control_requested", `{"desired":"active","actor_kind":"system","actor_id":"provider_limit"}`, true},
		{"agent_control_requested", `{"desired":"not_accepting","actor_kind":"system","actor_id":"agent_switch"}`, false},
		{"agent_control_requested", `{"desired":"weird"}`, false},
		{"agent_control_requested", `{}`, false},
	}
	for _, c := range cases {
		if got := IsNotableSystemEvent(c.kind, c.payload); got != c.want {
			t.Errorf("%s %s: notable=%v, want %v", c.kind, c.payload, got, c.want)
		}
	}
}

// The filter is applied in the query and in Go from one definition, and they agree on every kind and payload; and a burst of routine events, which a page of the
// feed used to fill, cannot push the notable ones out of it.
func TestRecentNotableSystemEventsAgreesWithTheRuleAndSurvivesABurst(t *testing.T) {
	t.Parallel()
	s, f, owner := recoveryFixture(t, false)
	ctx := context.Background()
	type probe struct{ kind, payload string }
	var probes []probe
	for _, c := range []probe{
		{"agent_spawn_failed", `{"error":"x"}`}, {"agent_key_rotated", `{}`}, {"turn_process_exit", `{"exit_code":"0"}`}, {"turn_process_exit", `{"exit_code":"137"}`}, {"turn_process_exit", `{"exit_code":2}`},
		{"turn_process_exit", `{}`}, {"agent_control_requested", `{"desired":"stopped","actor_kind":"user","actor_id":"u"}`}, {"agent_control_requested", `{"desired":"not_accepting","actor_id":"agent_switch"}`},
		{"agent_control_requested", `{"desired":"weird"}`}, {"agent_control_applied", `{}`}, {"room_renamed", `{}`}, {"limit_guard_clear", `{}`}, {"native_session_reset", `{}`}, {"machine_approved", `{}`},
	} {
		probes = append(probes, c)
	}
	if err := s.Tx(ctx, func(tx *Tx) error {
		for _, c := range probes {
			if _, err := tx.AppendEvent(Event{Kind: c.kind, ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: f.agent.ID, PayloadJSON: c.payload}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var want int
	for _, c := range probes {
		if IsNotableSystemEvent(c.kind, c.payload) {
			want++
		}
	}
	events, err := s.RecentNotableSystemEvents(ctx, owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != want {
		t.Fatalf("the query found %d notable events, the rule accepts %d", len(events), want)
	}
	for _, event := range events {
		if !event.Notable || !IsNotableSystemEvent(event.Kind, event.PayloadJSON) {
			t.Fatalf("a row the rule rejects came back, or was not marked: %+v", event)
		}
	}
	all, err := s.RecentSystemEvents(ctx, owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range all {
		if event.Notable != IsNotableSystemEvent(event.Kind, event.PayloadJSON) {
			t.Fatalf("the plain history marks %s wrongly", event.Kind)
		}
	}
	// A burst of 150 routine events after them: a page of 100 of the whole feed would be only routine ones, a page of the notable feed still has the notable ones.
	if err := s.Tx(ctx, func(tx *Tx) error {
		for i := 0; i < 150; i++ {
			if _, err := tx.AppendEvent(Event{Kind: "turn_process_exit", ProjectID: f.project.ID, RoomID: f.room.ID, AgentID: f.agent.ID, PayloadJSON: `{"exit_code":"0"}`}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.RecentSystemEvents(ctx, owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page {
		if event.Notable {
			t.Fatal("the burst should have filled the plain page")
		}
	}
	notable, err := s.RecentNotableSystemEvents(ctx, owner, 100)
	if err != nil || len(notable) != want {
		t.Fatalf("the notable page after a burst: %d events (want %d) %v", len(notable), want, err)
	}
	// Access is the same as the plain history's.
	if _, err := s.db.Exec(`INSERT INTO user(id,login,pw_hash,pw_params,created_at) VALUES ('usr_outsider2','outsider2','hash','{}',?)`, timestamp()); err != nil {
		t.Fatal(err)
	}
	if none, err := s.RecentNotableSystemEvents(ctx, "usr_outsider2", 100); err != nil || len(none) != 0 {
		t.Fatalf("a user outside the project sees nothing: %d %v", len(none), err)
	}
}
