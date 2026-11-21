package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
)

// A turn that ends cleanly in a process that goes on living (a warm process kept for the next turn) is recorded as a finished turn: nothing exited, so
// there is no turn_process_exit with exit code 0. The same exit for a process that ended with its turn is the exit it always was, and a failure keeps its
// exit event whatever the process does.
func TestTurnEndedInAKeptProcessIsRecordedAsFinishedNotAnExit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		info     adapter.ExitInfo
		wantKind string
		other    string
	}{
		"kept warm process":              {adapter.ExitInfo{TerminalEvent: true, ProcessAlive: true}, "turn_finished", "turn_process_exit"},
		"process ended with the turn":    {adapter.ExitInfo{TerminalEvent: true}, "turn_process_exit", "turn_finished"},
		"failure keeps its exit event":   {adapter.ExitInfo{ExitCode: 1, ProcessAlive: true}, "turn_process_exit", "turn_finished"},
		"a cancel is not a clean finish": {adapter.ExitInfo{TerminalEvent: true, CancelIntent: true, ProcessAlive: true}, "turn_process_exit", "turn_finished"},
	} {
		t.Run(name, func(t *testing.T) {
			s, f := answerFixture(t, false)
			ctx := context.Background()
			if _, err := applyCandidate(t, s, f.turn, f.agent.ID, 0, fmt.Sprintf(`{"messages":[{"room":%q,"body":"done"}]}`, f.room.ID)); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordProcessInfo(ctx, f.turn.ID, f.agent.ID, f.turn.Generation, tc.info); err != nil {
				t.Fatal(err)
			}
			count := func(kind string) int {
				var n int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE turn_id=? AND kind=?`, f.turn.ID, kind).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if count(tc.wantKind) != 1 || count(tc.other) != 0 {
				t.Fatalf("%s=%d %s=%d, want one %s and no %s", tc.wantKind, count(tc.wantKind), tc.other, count(tc.other), tc.wantKind, tc.other)
			}
			if tc.wantKind == "turn_finished" {
				var payload string
				if err := s.db.QueryRow(`SELECT payload_json FROM event WHERE turn_id=? AND kind='turn_finished'`, f.turn.ID).Scan(&payload); err != nil || payload != `{"process":"kept"}` {
					t.Fatalf("payload = %q, %v", payload, err)
				}
				var exec string
				if err := s.db.QueryRow(`SELECT exec FROM turn WHERE id=?`, f.turn.ID).Scan(&exec); err != nil || exec != "exited" {
					t.Fatalf("the turn itself is still settled as ended: %q %v", exec, err)
				}
			}
		})
	}
}
