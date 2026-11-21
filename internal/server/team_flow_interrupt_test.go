package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// The interrupt endpoint of a running team flow change answers a request with no agent, and one while no change is running, with the
// documented errors instead of touching any agent.
func TestTeamFlowMigrationInterruptRefusals(t *testing.T) {
	f := newLegacyFixture(t)
	path := "/api/projects/" + f.project.ID + "/team-flow/migration/interrupt"
	if resp := f.request(http.MethodPost, path, map[string]any{}, true); resp.StatusCode != 400 {
		t.Fatalf("no agent_id: %d", resp.StatusCode)
	}
	if resp := f.request(http.MethodPost, path, map[string]any{"agent_id": "agt_none"}, true); resp.StatusCode != 404 {
		t.Fatalf("no migration: %d", resp.StatusCode)
	}
}

// interruptFlowFixture is a draining team flow change whose kept leader runs a turn, on a machine that does or does not cut one turn.
func interruptFlowFixture(t *testing.T, turnInterrupt bool) (*fixture, *machineConn, string, string) {
	t.Helper()
	f := newFixture(t)
	f.seed()
	machine := &machineConn{id: f.machineID, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), bound: map[string]int{}, supportsTurnInterrupt: turnInterrupt,
		capabilities: []protocol.CapabilityReport{{Tool: "fake", Environment: adapter.Environment{Variant: "test"}}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	ownerID := f.ownerUserID(t)
	target := store.TeamSetupTarget{MachineID: f.machineID, Path: f.project.Folder, Tool: "fake", Variant: "test", Model: "test", Provider: "fake"}
	var setup store.TeamSetupResult
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		setup, err = tx.CreateTeamSetup(f.project.ID, ownerID, store.TeamSetupInput{FlowKey: "leader-workers", Leader: target})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	leaderID := setup.Agents[0].AgentID
	in := store.TeamFlowMigrationInput{FlowKey: "leader-dispatcher", FlowRev: setup.Flow.Rev, Dispatcher: &target}
	var turnID string
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE session SET id='ses_flow' WHERE agent_id=? AND generation=0`, leaderID); err != nil {
			return err
		}
		turn, err := tx.RecordTurn(store.Turn{AgentID: leaderID, SessionID: "ses_flow", Generation: 0, RoomID: f.room.ID, Attempt: 1, Mode: "normal", Exec: "running"})
		turnID = turn.ID
		return err
	}); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if err := f.db.Tx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.BeginTeamFlowMigration(f.project.ID, ownerID, in)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for len(machine.sendQueue) > 0 { // frames of the setup are not what is asserted
		<-machine.sendQueue
	}
	return f, machine, leaderID, turnID
}

// An older supervisor (no supports_turn_interrupt) gets the stop: the turn is cut by stopping the agent, which the change starts again.
func TestTeamFlowMigrationInterruptSendsTheStopControl(t *testing.T) {
	t.Parallel()
	f, machine, leaderID, _ := interruptFlowFixture(t, false)
	resp := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-flow/migration/interrupt", map[string]any{"agent_id": leaderID}, true)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("interrupt: %d", resp.StatusCode)
	}
	select {
	case frame := <-machine.sendQueue:
		if frame.Type != protocol.Control || frame.AgentID != leaderID || frame.DesiredState != "stopped" {
			t.Fatalf("frame sent to the machine: %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("no stop control reached the agent's machine")
	}
}

// A supervisor that cuts one turn gets the turn_interrupt frame, and what it answers decides the rest: ok (and not_running, a turn that
// just ended) leave the agent running; unsupported_mode and no answer at all fall back to the stop.
func TestTeamFlowMigrationInterruptUsesTheTurnInterruptFrame(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]struct {
		answer string // "" answers nothing
		stops  bool
	}{"ok": {"ok", false}, "not running": {"not_running", false}, "unsupported mode": {"unsupported_mode", true}, "no answer": {"", true}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, machine, leaderID, turnID := interruptFlowFixture(t, true)
			f.srv.turnInterruptWait = 200 * time.Millisecond
			frames := make(chan protocol.Frame, 8)
			go func() {
				for {
					select {
					case frame := <-machine.sendQueue:
						frames <- frame
						if frame.Type == protocol.TurnInterrupt && item.answer != "" {
							machine.harnessMu.Lock()
							waiter := machine.pathPending[frame.ReqID]
							machine.harnessMu.Unlock()
							if waiter != nil {
								waiter <- protocol.Frame{V: protocol.Version, Type: protocol.TurnInterruptResult, ReqID: frame.ReqID, Result: item.answer}
							}
						}
					case <-machine.done:
						return
					}
				}
			}()
			t.Cleanup(func() { close(machine.done) })
			resp := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-flow/migration/interrupt", map[string]any{"agent_id": leaderID}, true)
			var body struct {
				Migration store.TeamFlowMigrationProgress `json:"migration"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != 200 || decodeErr != nil {
				t.Fatalf("interrupt: %d %v", resp.StatusCode, decodeErr)
			}
			select {
			case frame := <-frames:
				if frame.Type != protocol.TurnInterrupt || frame.AgentID != leaderID || frame.TurnID != turnID || frame.ReqID == "" || frame.Reason == "" {
					t.Fatalf("first frame sent to the machine: %+v", frame)
				}
			case <-time.After(time.Second):
				t.Fatal("no turn_interrupt frame reached the agent's machine")
			}
			stopped := false
			select {
			case frame := <-frames:
				if frame.Type != protocol.Control || frame.AgentID != leaderID || frame.DesiredState != "stopped" {
					t.Fatalf("second frame sent to the machine: %+v", frame)
				}
				stopped = true
			case <-time.After(300 * time.Millisecond):
			}
			if !item.stops {
				// A second request, while the turn still runs, sends the frame again.
				again := f.request(http.MethodPost, "/api/projects/"+f.project.ID+"/team-flow/migration/interrupt", map[string]any{"agent_id": leaderID}, true)
				again.Body.Close()
				select {
				case frame := <-frames:
					if again.StatusCode != 200 || frame.Type != protocol.TurnInterrupt || frame.TurnID != turnID {
						t.Fatalf("repeat: %d %+v", again.StatusCode, frame)
					}
				case <-time.After(time.Second):
					t.Fatal("a repeat did not send the frame again")
				}
			}
			card, err := f.db.AgentCardByID(t.Context(), leaderID)
			if err != nil {
				t.Fatal(err)
			}
			if stopped != item.stops || (card.Desired == "stopped") != item.stops {
				t.Fatalf("stop control sent=%t desired=%s, want stop=%t", stopped, card.Desired, item.stops)
			}
			// The turn has not ended yet: an agent that was only sent the frame keeps blocking (it can be interrupted again); one that was
			// stopped does not.
			found := false
			for _, agent := range body.Migration.Agents {
				if agent.AgentID == leaderID {
					found = true
					if agent.Blocking == item.stops || agent.Restarting != item.stops {
						t.Fatalf("progress of the interrupted agent: %+v", agent)
					}
				}
			}
			if !found {
				t.Fatalf("the interrupted agent is not in the progress: %+v", body.Migration.Agents)
			}
		})
	}
}
