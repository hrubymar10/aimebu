package store

import (
	"errors"
	"fmt"
)

var ErrInvalidTransition = errors.New("invalid_transition")

// TransitionTable checks a single state edge. The caller enforces the
// evidence and authorisation required to take an otherwise valid edge.
type TransitionTable struct {
	name  string
	edges map[string][]string
}

func (table TransitionTable) Transition(from, to string) error {
	for _, allowed := range table.edges[from] {
		if to == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: %s %q -> %q", ErrInvalidTransition, table.name, from, to)
}

var NoteTransitions = TransitionTable{"note", map[string][]string{
	"queued":     {"dispatched", "aborted", "dropped"},
	"dispatched": {"in_turn", "queued", "unknown", "aborted", "dropped"},
	"in_turn":    {"done", "unknown", "aborted", "dropped"},
	"unknown":    {"queued", "aborted", "dropped"},
	"done":       {},
	"aborted":    {},
	"dropped":    {},
}}

var TurnExecTransitions = TransitionTable{"turn.exec", map[string][]string{
	"pending": {"running", "killed"},
	"running": {"exited", "killed", "lost"},
	"lost":    {"exited", "killed"},
	"exited":  {},
	"killed":  {},
}}

var TurnOutcomeTransitions = TransitionTable{"turn.outcome", map[string][]string{
	"none":           {"completed", "partial_failed", "failed", "cancelled", "unknown"},
	"completed":      {},
	"partial_failed": {},
	"failed":         {},
	"cancelled":      {},
	"unknown":        {},
}}

var TurnSettleTransitions = TransitionTable{"turn.settle", map[string][]string{
	"pending": {"settled"},
	"settled": {},
}}

var OutboxTransitions = TransitionTable{"outbox", map[string][]string{
	"proposed":   {"validated", "invalid", "held"},
	"validated":  {"committed", "held", "superseded"},
	"invalid":    {"held", "superseded"},
	"committed":  {},
	"held":       {},
	"superseded": {},
}}

var AgentDesiredTransitions = TransitionTable{"agent.desired", map[string][]string{
	"active":        {"not_accepting", "stopped"},
	"not_accepting": {"active", "stopped"},
	"stopped":       {"active"},
}}

var AgentObservedTransitions = TransitionTable{"agent.observed", map[string][]string{
	"idle":         {"in_turn", "stopped"},
	"in_turn":      {"idle", "interrupting", "lost"},
	"interrupting": {"stopped", "lost"},
	"stopped":      {"idle"},
	"lost":         {"idle", "stopped"},
}}

var RoomLifecycleTransitions = TransitionTable{"room.lifecycle", map[string][]string{
	"open":    {"closing"},
	"closing": {"closed"},
	"closed":  {"open"},
}}

var WriterStateTransitions = TransitionTable{"checkout.writer_state", map[string][]string{
	"none":      {"reserved"},
	"reserved":  {"releasing"},
	"releasing": {"none"},
}}

var InboxItemTransitions = TransitionTable{"inbox_item", map[string][]string{
	"open":       {"resolved", "withdrawn", "superseded"},
	"resolved":   {},
	"withdrawn":  {},
	"superseded": {"resolved"},
}}
