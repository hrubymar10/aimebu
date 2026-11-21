package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hrubymar10/aimebu/internal/adapter"
)

// What the supervisor sends for a turn its agent's stop cancelled.
var adapterCancelExit = adapter.ExitInfo{ExitCode: 130, CancelIntent: true}

// On a machine that cuts one turn, the interrupt records a requeue request for the agent's running turn and leaves the agent alone: it is
// not stopped and not marked for a restart. It keeps blocking (and can be interrupted again) until the turn has really ended: a recorded
// request says nothing of whether the frame arrived. The change goes on once the turn is gone.
func TestInterruptFlowMigrationTurnRecordsARequeueAndKeepsTheAgentRunning(t *testing.T) {
	f, setup, _ := interruptFixture(t, "leader-workers", "leader-dispatcher", true)
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != "" {
		t.Fatalf("an agent with no running turn: %+v %v", target, err)
	}
	turnID := openTurn(t, f, leaderID)
	if _, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.bob, leaderID); err == nil {
		t.Fatal("a member who may not control agents interrupted one")
	}
	target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID)
	if err != nil || target.TurnID != turnID || target.AgentID != leaderID || target.MachineID != "mch_members" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	again, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID)
	if err != nil || again != target {
		t.Fatalf("a repeat: %+v %v", again, err)
	}
	var requests, marks int
	var desired, payload string
	if err := f.s.db.QueryRow(`SELECT COUNT(*),MAX(payload_json) FROM event WHERE turn_id=? AND kind='turn_interrupt_requested'`, turnID).Scan(&requests, &payload); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM team_flow_migration_interrupt WHERE agent_id=?`, leaderID).Scan(&marks); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, leaderID).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || marks != 0 || desired != "active" {
		t.Fatalf("requests=%d restart marks=%d desired=%s payload=%s", requests, marks, desired, payload)
	}
	if disposition, err := flowTurnDisposition(f.s, turnID); err != nil || disposition != TurnInterruptRequeue {
		t.Fatalf("disposition=%q %v", disposition, err)
	}
	leaderProgress := func() TeamFlowMigrationAgentProgress {
		t.Helper()
		progress, err := f.s.TeamFlowMigrationProgress(ctx, f.project.ID, f.alice)
		if err != nil {
			t.Fatal(err)
		}
		for _, agent := range progress.Agents {
			if agent.AgentID == leaderID {
				return agent
			}
		}
		t.Fatalf("the leader is not in the progress: %+v", progress.Agents)
		return TeamFlowMigrationAgentProgress{}
	}
	if agent := leaderProgress(); !agent.Blocking || agent.Restarting {
		t.Fatalf("a recorded request alone must leave the agent blocking and not restarting: %+v", agent)
	}
	// The supervisor reports the turn interrupted: it is settled with no continuation, the agent is idle and still active, and the change advances.
	if err := f.s.RecordProcessInfo(ctx, turnID, leaderID, 0, requestedInterruptExit); err != nil {
		t.Fatal(err)
	}
	var settle, outcome, observed string
	var block, children int
	if err := f.s.db.QueryRow(`SELECT settle,outcome FROM turn WHERE id=?`, turnID).Scan(&settle, &outcome); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT desired,observed,recovery_block FROM agent WHERE id=?`, leaderID).Scan(&desired, &observed, &block); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE parent_turn_id=?`, turnID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if settle != "settled" || outcome != "cancelled" || desired != "active" || observed != "idle" || block != 0 || children != 0 {
		t.Fatalf("settle=%s outcome=%s desired=%s observed=%s block=%d children=%d", settle, outcome, desired, observed, block, children)
	}
	if agent := leaderProgress(); agent.Blocking {
		t.Fatalf("the agent still blocks after its turn has ended: %+v", agent)
	}
	if _, err := f.s.AdvanceTeamFlowMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT desired FROM agent WHERE id=?`, leaderID).Scan(&desired); err != nil || desired != "active" {
		t.Fatalf("the interrupted agent did not stay active: %q %v", desired, err)
	}
}

func flowTurnDisposition(s *Store, turnID string) (string, error) {
	var disposition string
	err := s.Tx(context.Background(), func(tx *Tx) error {
		var err error
		disposition, err = tx.turnInterruptDisposition(turnID)
		return err
	})
	return disposition, err
}

// What the turn path does not take is left to the stop path: no change, a change that is over, an agent already stopping, and a turn that
// is not a running ordinary one. The stop path still works after a recorded request (a frame that was not answered falls back to it).
func TestInterruptFlowMigrationTurnLeavesTheRestToTheStopPath(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	if _, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no migration: %v", err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev,
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	turnID := openTurn(t, f, leaderID)
	for name, state := range map[string]string{"pending turn": `UPDATE turn SET exec='pending' WHERE id=?`, "correction turn": `UPDATE turn SET exec='running',mode='correction' WHERE id=?`} {
		if _, err := f.s.db.Exec(state, turnID); err != nil {
			t.Fatal(err)
		}
		if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != "" {
			t.Fatalf("%s: %+v %v", name, target, err)
		}
	}
	if _, err := f.s.db.Exec(`UPDATE turn SET exec='running',mode='normal' WHERE id=?`, turnID); err != nil {
		t.Fatal(err)
	}
	if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != turnID {
		t.Fatalf("running turn: %+v %v", target, err)
	}
	card, err := f.s.InterruptFlowMigrationAgent(ctx, f.project.ID, f.alice, leaderID)
	if err != nil || card.Desired != "stopped" {
		t.Fatalf("the stop path after a recorded request: %+v %v", card, err)
	}
	if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != "" {
		t.Fatalf("an agent that is stopping: %+v %v", target, err)
	}
	// The stop's cancel is not settled as the request's requeue.
	if err := f.s.RecordProcessInfo(ctx, turnID, leaderID, 0, adapterCancelExit); err != nil {
		t.Fatal(err)
	}
	var exec, outcome string
	if err := f.s.db.QueryRow(`SELECT exec,outcome FROM turn WHERE id=?`, turnID).Scan(&exec, &outcome); err != nil || exec != "killed" || outcome == "cancelled" {
		t.Fatalf("exec=%s outcome=%s err=%v", exec, outcome, err)
	}
	if _, err := f.s.CancelTeamFlowMigration(ctx, f.project.ID, f.alice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); !errors.Is(err, ErrTeamFlowMigrationFinal) {
		t.Fatalf("a change that is over: %v", err)
	}
}

// A request that yields (the supervisor drain's) leaves alone the turn of an agent a team flow change in progress holds: the change has
// its own interrupt, which still works, and its requeue is what settles the turn.
func TestYieldingTurnInterruptLeavesAMigratingAgentsTurnAlone(t *testing.T) {
	f, setup, _ := interruptFixture(t, "leader-workers", "leader-dispatcher", true)
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	turnID := openTurn(t, f, leaderID)
	drain := TurnInterruptRequest{TurnID: turnID, ActorKind: "system", ActorID: "supervisor_drain", Purpose: "supervisor_drain", Disposition: TurnInterruptContinue, Yield: true}
	if _, err := f.s.RequestTurnInterrupt(ctx, drain); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a yielding request for a migrating agent's turn: %v", err)
	}
	var requests int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE turn_id=? AND kind='turn_interrupt_requested'`, turnID).Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("requests=%d %v", requests, err)
	}
	if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != turnID {
		t.Fatalf("the change's own interrupt: %+v %v", target, err)
	}
	if _, err := f.s.RequestTurnInterrupt(ctx, drain); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("a yielding request after the change's own: %v", err)
	}
	if disposition, err := flowTurnDisposition(f.s, turnID); err != nil || disposition != TurnInterruptRequeue {
		t.Fatalf("disposition=%q %v", disposition, err)
	}
	// Once the change is over the agent's turns are nobody's but the drain's again.
	if _, err := f.s.CancelTeamFlowMigration(ctx, f.project.ID, f.alice); err != nil {
		t.Fatal(err)
	}
	if target, err := f.s.RequestTurnInterrupt(ctx, drain); err != nil || target.TurnID != turnID {
		t.Fatalf("after the change ended: %+v %v", target, err)
	}
}

// The other order: the supervisor drain recorded its continue on the turn before the change began. The person's interrupt during the
// change replaces it, so the turn is requeued for the change and no continuation turn is made to hold the drain again.
func TestFlowInterruptReplacesAnEarlierDrainRequest(t *testing.T) {
	f, setup := flowMigrationFixture(t, "leader-workers")
	ctx := t.Context()
	leaderID := setup.Agents[0].AgentID
	turnID := openTurn(t, f, leaderID)
	if _, err := f.s.RequestTurnInterrupt(ctx, TurnInterruptRequest{TurnID: turnID, ActorKind: "system", ActorID: "supervisor_drain", Purpose: "supervisor_drain", Disposition: TurnInterruptContinue, Yield: true}); err != nil {
		t.Fatalf("the drain's request before any change: %v", err)
	}
	in := TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev,
		Dispatcher: &TeamSetupTarget{MachineID: "mch_members", Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}}
	if err := f.s.Tx(ctx, func(tx *Tx) error { _, err := tx.BeginTeamFlowMigration(f.project.ID, f.alice, in); return err }); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second is a repeat of the change's own request: nothing new is recorded
		if target, err := f.s.InterruptFlowMigrationTurn(ctx, f.project.ID, f.alice, leaderID); err != nil || target.TurnID != turnID {
			t.Fatalf("the change's interrupt: %+v %v", target, err)
		}
	}
	var requests int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM event WHERE turn_id=? AND kind='turn_interrupt_requested'`, turnID).Scan(&requests); err != nil || requests != 2 {
		t.Fatalf("requests=%d %v, want the drain's and the change's", requests, err)
	}
	if disposition, err := flowTurnDisposition(f.s, turnID); err != nil || disposition != TurnInterruptRequeue {
		t.Fatalf("disposition=%q %v", disposition, err)
	}
	if err := f.s.RecordProcessInfo(ctx, turnID, leaderID, 0, requestedInterruptExit); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var children int
	if err := f.s.db.QueryRow(`SELECT outcome FROM turn WHERE id=?`, turnID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM turn WHERE parent_turn_id=?`, turnID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if outcome != "cancelled" || children != 0 {
		t.Fatalf("the turn was settled as the drain's continue: outcome=%s children=%d", outcome, children)
	}
}
