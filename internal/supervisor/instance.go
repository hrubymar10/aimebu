package supervisor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

// A machine can run several supervisor instances side by side, each with a journal of its own (docs/supervisor.md, "Side by side"). This file is
// what an instance needs to know about the others and about leaving: when it owes nothing, how it leaves, and what it must not sweep.

// retireOwes says a retiring instance still has something to finish. Unlike hasRetireBlockingWork, nothing is left to a replacement, because no
// replacement adopts this journal: every command counts (native or container, its own or adopted from the journal), every warm process and
// ping that is not confirmed gone, a stop acknowledgement waiting for a process exit, and everything the server has not acknowledged (the end of
// a turn, an answer, the end of a command). It is false only when the instance can remove its journal and leave without losing a word.
func (c *Client) retireOwes() bool {
	if c.hasRetireBlockingWork() {
		return true
	}
	c.mu.Lock()
	stopAck := len(c.pendingStopAck) > 0
	unrecorded := len(c.unrecordedExits) > 0
	runner := c.commands
	c.mu.Unlock()
	if stopAck || unrecorded || c.warmLive.Load() > 0 || c.keepAlivePingsActive() > 0 {
		return true
	}
	if runner != nil {
		runner.mu.Lock()
		jobs := len(runner.jobs)
		runner.mu.Unlock()
		if jobs > 0 {
			return true
		}
	}
	if c.Journal == nil {
		return false
	}
	return c.Journal.Owes(context.Background())
}

// beginRetire starts a retirement on this connection: no new work is taken (stopping), every idle warm process and ping ends at once and the
// warm picture is sent, and the connection stays open (it carries the acknowledgements) until retireOwes is false, then closes. The instance
// leaves from Run once the connection is closed and nothing is owed.
func (c *Client) beginRetire(ctx context.Context, conn *websocket.Conn, probeCtx context.Context) {
	c.retireRequested.Store(true)
	c.mu.Lock()
	c.stopping = true
	c.mu.Unlock()
	go func() {
		c.keepAliveForgetAll()
		c.RetireAllWarm("the supervisor is retiring")
		if c.warmCapable() {
			sendCtx, cancel := context.WithTimeout(probeCtx, 3*time.Second)
			_ = c.sendWarmReport(sendCtx)
			cancel()
		}
	}()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if !c.retireOwes() {
				_ = conn.CloseNow()
				return
			}
			select {
			case <-probeCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// FinishRetire is what a supervisor does after Run returned because it retired with nothing owed: it removes its journal (nothing in it is
// open, so nothing needs a reader) and says it did. A supervisor that stops for any other reason keeps its journal, whose open work a drain-only
// process finishes.
func (c *Client) FinishRetire() (bool, error) {
	if !c.retired.Load() || c.Journal == nil || c.retireOwes() {
		return false, nil
	}
	return true, c.Journal.Remove()
}

// applyAgentKeys holds the keys the server granted, from hello_ack or from an agent_keys frame (the handover of an agent from an instance that
// still held it: until then its key stayed with that instance).
func (c *Client) applyAgentKeys(grants []protocol.AgentKeyGrant) {
	for _, grant := range grants {
		if err := c.setAgentKey(grant.AgentID, grant.Key); err != nil {
			c.report("an agent key from the server could not be stored", fmt.Errorf("key for agent %s: %w", grant.AgentID, err))
		}
	}
}

// otherInstanceHeld says another supervisor instance of this machine holds its journal lock now. The journals of one machine live beside each
// other: the legacy supervisor/journal.sqlite and supervisor/journals/<name>.sqlite.
func (c *Client) otherInstanceHeld() bool {
	if c.Journal == nil || c.Journal.path == "" {
		return false
	}
	dir := filepath.Dir(c.Journal.path)
	if filepath.Base(dir) == "journals" {
		dir = filepath.Dir(dir)
	}
	candidates := []string{filepath.Join(dir, "journal.sqlite")}
	if found, err := filepath.Glob(filepath.Join(dir, "journals", "*.sqlite")); err == nil {
		candidates = append(candidates, found...)
	}
	own := filepath.Clean(c.Journal.path)
	for _, path := range candidates {
		if filepath.Clean(path) == own {
			continue
		}
		if _, err := os.Stat(path + ".lock"); err != nil {
			continue // never created: nobody holds it (and probing would create it)
		}
		if held, _, err := JournalOwner(path); err == nil && held {
			return true
		}
	}
	return false
}

func (c *Client) logSweepSkipped() {
	log.Printf("supervisor start: another supervisor instance of this machine is running; the generated agent configs are left in place")
}
