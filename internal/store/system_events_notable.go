package store

import (
	"fmt"
	"strings"

	"github.com/goccy/go-json"
)

// Which events of the system-events feed need a look. The feed holds every operational event (membership, lifecycle, recovery, quota, machines, access); what the
// bell shows, and counts, is this subset, and this file is the one place it is defined: the history is read through it (RecentSystemEvents with notableOnly), the live
// stream marks each event with it (Event.Notable), and the web keeps no list of its own. A burst of routine events therefore cannot push the important ones out of a
// page, and the bell and its badge always agree.
//
// Something is notable when its kind is in notableKinds, or when it is one of the kinds in notableWhen and its payload meets every condition listed for it.

// notableKinds are always notable: something was spawned or failed to be, an agent was stopped or removed, a turn did not run or finish, a limit or a credential needs
// attention, a person's access changed, a room could not close, a machine was cut off, and the clears of the limits (so the alert that came before has its end).
var notableKinds = []string{
	"agent_spawn_confirmed", "agent_spawn_failed", "agent_stopped", "agent_revoked", "agent_switch_failed",
	"agent_turn_budget_exceeded", "turn_lost", "turn_never_started", "special_turn_abandoned", "answer_held", "notes_dropped", "loop_guard",
	"credential_alert", "provider_limit_detected", "provider_limit_cleared", "usage_limit_threshold", "usage_limit_reset", "limit_guard_soft", "limit_guard_hard", "limit_guard_clear",
	"quota_switch", "quota_switch_dry_run", "quota_hold", "quota_switch_failed", "quota_reset_seen", "quota_switcher_tripped",
	"switch_overwritten",
	"native_session_reset", "user_admin_changed", "user_deactivated",
	"agent_context_cleared", "agent_context_compacted",
	"room_close_blocked", "machine_revoked",
}

// payloadCondition is one test on a payload field. A field that is not there counts as "" unless Present is set, which makes its absence a failure.
type payloadCondition struct {
	Field   string
	In      []string // the value is one of these
	NotIn   []string // the value is none of these
	Present bool     // the field must exist
}

// notableWhen are the kinds that are notable only when what their payload says is the notable part: a process that exited with a code other than 0 (an exit with 0 is
// routine), and an agent control that starts, stops or pauses (the control being applied is the same fact as its request and is not listed; the automatic pause of
// an account switch is part of the switch, not something a person did).
var notableWhen = map[string][]payloadCondition{
	"turn_process_exit":       {{Field: "exit_code", Present: true, NotIn: []string{"0", ""}}},
	"agent_control_requested": {{Field: "desired", In: []string{"active", "stopped", "not_accepting"}}, {Field: "actor_id", NotIn: []string{"agent_switch"}}},
}

func payloadString(payload map[string]any, field string) (string, bool) {
	value, ok := payload[field]
	if !ok || value == nil {
		return "", false
	}
	return fmt.Sprint(value), true
}

func (c payloadCondition) holds(payload map[string]any) bool {
	value, ok := payloadString(payload, c.Field)
	if c.Present && !ok {
		return false
	}
	if len(c.In) > 0 && !contains(c.In, value) {
		return false
	}
	return !contains(c.NotIn, value)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// sql is the same test over the stored payload: json_extract of the field, read as text.
func (c payloadCondition) sql() (string, []any) {
	field := "CAST(json_extract(e.payload_json,'$." + c.Field + "') AS TEXT)"
	var parts []string
	var args []any
	if c.Present {
		parts = append(parts, field+" IS NOT NULL")
	}
	marks := func(values []string) string { return strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") }
	if len(c.In) > 0 {
		parts = append(parts, "COALESCE("+field+",'') IN ("+marks(c.In)+")")
		for _, value := range c.In {
			args = append(args, value)
		}
	}
	if len(c.NotIn) > 0 {
		parts = append(parts, "COALESCE("+field+",'') NOT IN ("+marks(c.NotIn)+")")
		for _, value := range c.NotIn {
			args = append(args, value)
		}
	}
	return "(" + strings.Join(parts, " AND ") + ")", args
}

// IsNotableSystemEvent says whether the bell shows this event: its kind is always notable, or it is one of the conditional kinds and its payload meets the conditions.
func IsNotableSystemEvent(kind, payloadJSON string) bool {
	if contains(notableKinds, kind) {
		return true
	}
	conditions, ok := notableWhen[kind]
	if !ok {
		return false
	}
	var payload map[string]any
	if payloadJSON != "" && json.Unmarshal([]byte(payloadJSON), &payload) != nil {
		return false
	}
	for _, condition := range conditions {
		if !condition.holds(payload) {
			return false
		}
	}
	return true
}

// notableSQL is the `WHERE` fragment (and its arguments) that selects exactly what IsNotableSystemEvent accepts from the event table aliased `e`.
func notableSQL() (string, []any) {
	var args []any
	parts := []string{"e.kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(notableKinds)), ",") + ")"}
	for _, kind := range notableKinds {
		args = append(args, kind)
	}
	for kind, conditions := range notableWhen {
		clauses := []string{"e.kind=?"}
		args = append(args, kind)
		for _, condition := range conditions {
			clause, more := condition.sql()
			clauses = append(clauses, clause)
			args = append(args, more...)
		}
		parts = append(parts, "("+strings.Join(clauses, " AND ")+")")
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}
