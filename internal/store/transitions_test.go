package store

import (
	"errors"
	"strings"
	"testing"
)

func TestTransitionTablesExhaustive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, states, edges string
		table               TransitionTable
	}{
		{"note", "queued dispatched in_turn done aborted dropped unknown",
			"queued>dispatched queued>aborted queued>dropped dispatched>in_turn dispatched>queued dispatched>unknown dispatched>aborted dispatched>dropped in_turn>done in_turn>unknown in_turn>aborted in_turn>dropped unknown>queued unknown>aborted unknown>dropped", NoteTransitions},
		{"turn exec", "pending running exited killed lost",
			"pending>running pending>killed running>exited running>killed running>lost lost>exited lost>killed", TurnExecTransitions},
		{"turn outcome", "none completed partial_failed failed cancelled unknown",
			"none>completed none>partial_failed none>failed none>cancelled none>unknown", TurnOutcomeTransitions},
		{"turn settle", "pending settled", "pending>settled", TurnSettleTransitions},
		{"outbox", "proposed validated invalid committed held superseded",
			"proposed>validated proposed>invalid proposed>held validated>committed validated>held validated>superseded invalid>held invalid>superseded", OutboxTransitions},
		{"desired", "active not_accepting stopped",
			"active>not_accepting active>stopped not_accepting>active not_accepting>stopped stopped>active", AgentDesiredTransitions},
		{"observed", "idle in_turn interrupting stopped lost",
			"idle>in_turn idle>stopped in_turn>idle in_turn>interrupting in_turn>lost interrupting>stopped interrupting>lost stopped>idle lost>idle lost>stopped", AgentObservedTransitions},
		{"room", "open closing closed", "open>closing closing>closed closed>open", RoomLifecycleTransitions},
		{"writer", "none reserved releasing", "none>reserved reserved>releasing releasing>none", WriterStateTransitions},
		{"inbox", "open resolved withdrawn superseded", "open>resolved open>withdrawn open>superseded", InboxItemTransitions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := make(map[string]bool)
			for _, edge := range strings.Fields(tc.edges) {
				allowed[edge] = true
			}
			states := strings.Fields(tc.states)
			if len(tc.table.edges) != len(states) {
				t.Fatalf("table has %d states, want %d", len(tc.table.edges), len(states))
			}
			for _, from := range states {
				if _, ok := tc.table.edges[from]; !ok {
					t.Errorf("missing state %s", from)
				}
				for _, to := range states {
					err := tc.table.Transition(from, to)
					want := allowed[from+">"+to]
					if want && err != nil || !want && !errors.Is(err, ErrInvalidTransition) {
						t.Errorf("%s→%s: error %v; allowed=%v", from, to, err, want)
					}
				}
			}
			for _, state := range []string{"", "bogus"} {
				if !errors.Is(tc.table.Transition(state, states[0]), ErrInvalidTransition) ||
					!errors.Is(tc.table.Transition(states[0], state), ErrInvalidTransition) {
					t.Errorf("unknown state %q was accepted", state)
				}
			}
		})
	}
}
