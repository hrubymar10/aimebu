package server

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// A turn recorded while a credential swap is running must wait for it. The swap is a real SwitchGuarded with the
// server's quietGuard and Store.HoldTurns; a RecordTurn is launched from inside the swap itself (the fan-out hook), so
// it lands exactly in the window the gate exists for. Removing Hold from either switch path lets the turn in
// mid-swap and fails the test.
func TestATurnRecordedDuringTheSwapWaitsForIt(t *testing.T) {
	for _, path := range []string{"web", "quota"} {
		t.Run(path, func(t *testing.T) {
			f, m, _ := quotaEnv(t)
			agent, turn, _, _ := f.execution(t)
			ctx := context.Background()
			if err := f.db.Tx(ctx, func(tx *store.Tx) error {
				if _, err := tx.Exec(`UPDATE agent SET tool='claude',observed='idle' WHERE id=?`, agent.ID); err != nil {
					return err
				}
				_, err := tx.Exec(`UPDATE turn SET exec='exited',settle='settled' WHERE id=?`, turn.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fan := m.TokenCopyFanOut()
			recorded := make(chan error, 1)
			var duringSwap bool
			m.SetTokenCopyHooks(switcher.TokenCopyHooks{
				ReplaceActive: fan.FanOutActiveReplacing,
				ClearActive:   fan.ClearActive,
				FanOutActive: func(tool string, full []byte) error {
					go func() {
						_, err := f.db.RecordTurn(ctx, store.Turn{AgentID: agent.ID, SessionID: turn.SessionID, Generation: turn.Generation, RoomID: turn.RoomID, Attempt: 2, Mode: "normal", Exec: "pending"})
						recorded <- err
					}()
					select {
					case <-recorded:
						duringSwap = true // the turn got in while the swap was still writing
						recorded <- nil
					case <-time.After(300 * time.Millisecond):
					}
					return fan.FanOutActive(tool, full)
				},
			})
			switch path {
			case "web":
				resp := f.request(http.MethodPost, "/api/switcher/claude/use", map[string]string{"name": "B"}, true)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("status = %d", resp.StatusCode)
				}
			default:
				changed, reason, err := quotaActuator{s: f.srv}.Switch(ctx, "claude", "A", "B")
				if err != nil || !changed {
					t.Fatalf("changed=%v reason=%q err=%v", changed, reason, err)
				}
			}
			if duringSwap {
				t.Fatal("a turn was recorded while the credential swap was running")
			}
			select {
			case err := <-recorded:
				if err != nil {
					t.Fatalf("the waiting turn failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the waiting turn never completed after the swap")
			}
		})
	}
}

// A switch refreshes the usage snapshot in the background, and that refresh writes the usage cache into the usages directory. Once
// WaitUsageRefreshes has returned nothing writes there any more: a test that removes the directory next (its cleanup) does not race the write.
func TestWaitUsageRefreshesJoinsTheRefreshASwitchStarted(t *testing.T) {
	f, _, _ := quotaEnv(t)
	dir := t.TempDir()
	reader, err := usages.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.useUsages(reader)
	for i := 0; i < 20; i++ {
		f.srv.refreshSwitchedUsage("claude")
	}
	f.srv.WaitUsageRefreshes()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refresh wrote into %s after WaitUsageRefreshes returned (stat: %v)", dir, err)
	}
}
