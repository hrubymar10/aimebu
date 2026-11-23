package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/credtarget"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// Warm agents. After a turn the supervisor can keep the agent's harness process up for a short idle window, so a message that arrives in it
// goes straight into the live process instead of starting a new one and resuming the session. When the window ends the agent sleeps: the
// process is ended and the next turn takes the ordinary cold path. Only an adapter that is an adapter.WarmAdapter takes part; for any other
// tool, and for any turn the adapter will not run warm (a correction, a diagnostic, a fork), nothing changes.
//
// One rule runs through it: a warm process is either idle (in the pool, owned by nobody) or busy (running one turn, in c.running/c.active as
// any turn is), and whoever takes it out of the pool owns it. Delivery, the idle timer, a stop, a key change and the memory guard all go
// through take, so they cannot both end up with it.

// WarmConfig is how long an idle agent's process is kept and when it is let go regardless.
type WarmConfig struct {
	// Idle is the window after a turn in which the process stays up. Zero turns warm mode off (the default).
	Idle time.Duration
	// MaxLifetime ends a process this old when its turn ends (and refuses it a new turn): a process keeps the credentials it started with, and
	// nothing refreshes them inside it. Zero means a lifetime of one hour.
	MaxLifetime time.Duration
	// MinFreeBytes and MinFreePercent are the memory guard: when free memory falls under either, the longest-idle process is ended, one every
	// few seconds, until there is enough again. Zero for both turns the guard off.
	MinFreeBytes   int64
	MinFreePercent float64
}

const (
	defaultWarmMaxLifetime = time.Hour
	warmGuardEvery         = 10 * time.Second
	warmCloseGrace         = 3 * time.Second
)

// DefaultWarmIdle is how long an agent's process is kept up after a turn when nobody has said otherwise: the owner's saved setting wins, then
// AIMEBU_WARM_IDLE, then this.
const DefaultWarmIdle = 5 * time.Minute

// WarmConfigFromEnv reads the settings: AIMEBU_WARM_IDLE (a duration such as 10m; 0 is off; unset or unreadable is DefaultWarmIdle),
// AIMEBU_WARM_MAX_LIFETIME (default 1h), AIMEBU_WARM_MIN_FREE_MB (default 1536) and AIMEBU_WARM_MIN_FREE_PERCENT (default 10).
func WarmConfigFromEnv() WarmConfig {
	config := WarmConfig{Idle: DefaultWarmIdle, MinFreeBytes: 1536 << 20, MinFreePercent: 10}
	if value := strings.TrimSpace(os.Getenv("AIMEBU_WARM_IDLE")); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d >= 0 {
			config.Idle = d
		}
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("AIMEBU_WARM_MAX_LIFETIME"))); err == nil && d > 0 {
		config.MaxLifetime = d
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("AIMEBU_WARM_MIN_FREE_MB")), 10, 64); err == nil && n >= 0 {
		config.MinFreeBytes = n << 20
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("AIMEBU_WARM_MIN_FREE_PERCENT")), 64); err == nil && f >= 0 {
		config.MinFreePercent = f
	}
	return config
}

// warmCfg is the warm settings now, read under the lock (they can be changed while turns run).
func (c *Client) warmCfg() WarmConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Warm
}

func (w WarmConfig) lifetime() time.Duration {
	if w.MaxLifetime > 0 {
		return w.MaxLifetime
	}
	return defaultWarmMaxLifetime
}

// warmEntry is one live process and what it was started with.
type warmEntry struct {
	agentID             string
	tool                string // the harness tool (claude, ...): an account switch retires by it
	session             adapter.WarmSession
	fp                  string // what the process was started with (the adapter's fingerprint: command, arguments, environment, folder)
	login               string // the login it works under (warmCredentials' stamp), taken again when a turn ends, see loginHolds
	account             string // which account that login is (warmAccount); "" when it could not be told
	loginNow            func() (stamp, account string, ok bool)
	loginMoved          bool // a turn ended with another account's login on disk (guarded by the slot's lock)
	started             time.Time
	stderr              *swapWriter
	containerized       bool // runs through a Docker launcher: its memory is the container's, not the supervisor's
	timer               *time.Timer
	idleAt              time.Time
	idleGen             int           // which idle window the timer belongs to: a timer of an earlier window, fired late, must not end the process of this one
	idleStop            chan struct{} // closed when the process leaves the idle state, ending its crash watcher
	retire              bool          // an end was asked for while it ran a turn: it ends when the turn does
	gone                atomic.Bool   // its end was confirmed and counted (warmLive), once
	unconfirmedReported atomic.Bool   // retries must not report the same uncertain end repeatedly
	// containerSeen says Docker has shown the container the launcher started the process in (see sightContainer); containerCheckFailed that a
	// question about it failed and was reported.
	containerSeen        atomic.Bool
	containerCheckFailed atomic.Bool
	quarantineReported   atomic.Bool // a still-live process past the quarantine is reported once
	stuckAt              time.Time
	lastNudge            time.Time     // when the watcher was last woken early (guarded by the slot's lock)
	stuckNudge           chan struct{} // wakes the stuck watcher early (buffered, one pending): a stop or delivery that finds the slot stuck asks for a retry now (guarded by the slot's lock)
	stuckReason          string        // why the process was ended and not confirmed gone (the reason its slot went stuck)
	plannedEnd           bool          // the end was decided by a stop of the agent, a requested turn interrupt or the supervisor's shutdown, whatever reason text the path gave (guarded by the slot's lock)
	kept                 bool          // it was reported as alive after a turn (kept idle): its real end is then reported too (guarded by the slot's lock)
}

// swapWriter is the stderr sink of a warm process: it writes to the output log of the turn that is running, and to nothing between turns (a
// turn's log is closed when the turn ends, but the process goes on).
type swapWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *swapWriter) Set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

func (s *swapWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w == nil {
		return len(p), nil
	}
	_, _ = w.Write(p) // a log that fails changes nothing for the turn
	return len(p), nil
}

// warmSlot is the per-agent state machine: the one place a warm process of the agent lives, whatever it is doing. Everything that changes it
// (a delivery taking it, a turn ending, the idle timer, a stop, a key change, the memory guard, the crash watcher) holds the slot's lock for
// the state change, including starting and handing a turn to the process, so none of them can see it half-way: a stop that arrives while
// a delivery is starting the process waits for it and then finds it busy; a turn that ends while a stop lands is settled after it, against the
// desired state the stop already set. A stuck process's background close runs outside the lock, then checks the slot again before releasing it.
type warmSlot struct {
	mu    sync.Mutex
	idle  *warmEntry // the process between turns
	busy  *warmEntry // the process running a turn
	stuck *warmEntry // a process that was ended and not confirmed gone: nothing else starts on the session until it is
	// stuckNow mirrors stuck != nil for the loop that reads the server's frames, which must not wait for the slot's lock (a slow end holds it).
	stuckNow atomic.Bool
}

// setStuck changes the slot's stuck process. The caller holds slot.mu.
func (s *warmSlot) setStuck(entry *warmEntry) {
	s.stuck = entry
	s.stuckNow.Store(entry != nil)
}

type warmPool struct {
	mu      sync.Mutex
	slots   map[string]*warmSlot
	ctx     context.Context
	cancel  context.CancelFunc
	now     func() time.Time
	after   func(time.Duration, func()) *time.Timer
	started int // warm processes started
	reused  int // turns that went into a live process
	// watchers counts the goroutines that watch a stuck process or take a first look at a container; they end when ctx does.
	watchers sync.WaitGroup
}

func newWarmPool() *warmPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &warmPool{slots: map[string]*warmSlot{}, ctx: ctx, cancel: cancel, now: time.Now, after: time.AfterFunc}
}

// pool is the warm pool, made on first use.
func (c *Client) pool() *warmPool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warmPool == nil {
		c.warmPool = newWarmPool()
		if c.warmNow != nil {
			c.warmPool.now = c.warmNow
		}
		if c.warmAfter != nil {
			c.warmPool.after = c.warmAfter
		}
	}
	return c.warmPool
}

// slot is an agent's state machine, made on first use.
func (p *warmPool) slot(agentID string) *warmSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	slot := p.slots[agentID]
	if slot == nil {
		slot = &warmSlot{}
		p.slots[agentID] = slot
	}
	return slot
}

// existing returns the slots as they are now, for a pass over all of them.
func (p *warmPool) existing() map[string]*warmSlot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*warmSlot, len(p.slots))
	for id, slot := range p.slots {
		out[id] = slot
	}
	return out
}

// takeIdle removes the slot's idle process and stops its timer and watcher. The caller holds slot.mu.
func (s *warmSlot) takeIdle() *warmEntry {
	entry := s.idle
	if entry == nil {
		return nil
	}
	s.idle = nil
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	if entry.idleStop != nil {
		close(entry.idleStop)
		entry.idleStop = nil
	}
	return entry
}

// endWarm ends a process (taken out of its slot by the caller, or one that never got in) and waits until it is gone, and says whether it is
// confirmed gone. With a grace it first closes the process's input and lets it go on its own, which for a Docker launcher lets the wrapper
// clean its session up properly instead of leaving that to its watchdog; it is killed when the grace is over. Without one it is killed at once.
// A kill that is not confirmed is tried once more. The journal's note of the process is kept until it is known to be gone, and a caller that
// gets false must not go on as if it were: not acknowledge a stop, not drop the key, not start another process on the session.
func (c *Client) endWarm(entry *warmEntry, reason string, grace time.Duration) bool {
	if entry == nil {
		return true
	}
	if !c.tryEndWarm(entry, reason, grace) {
		return false
	}
	c.recordWarmEnd(entry, reason)
	return true
}

// tryEndWarm only touches the session. A background retry can run it without
// holding the slot lock and then check that it still owns the stuck entry.
func (c *Client) tryEndWarm(entry *warmEntry, reason string, grace time.Duration) bool {
	if entry.containerized && grace == 0 {
		// `docker exec` does not pass a signal on, so a kill of the host side leaves the process in the container for up to about fifteen seconds:
		// a process in a container is always closed first (its input is closed, `docker exec` returns and the wrapper cleans its session up).
		grace = warmCloseGrace
	}
	err := errors.New("not tried")
	if grace > 0 {
		err = entry.session.Close(grace)
	}
	for attempt := 0; err != nil && attempt < 2; attempt++ { // a kill that is not confirmed is tried once more
		err = entry.session.Kill()
	}
	if err != nil {
		if entry.containerized || entry.session.Alive() {
			if why, gone := c.warmContainerGone(entry); gone {
				// The launcher did not say so, but nothing runs in a container that is gone: the process is confirmed gone without a quarantine.
				log.Printf("agent %s: its warm process is confirmed gone (%s): %s", entry.agentID, reason, why)
				return true
			}
			if entry.unconfirmedReported.CompareAndSwap(false, true) {
				werr := fmt.Errorf("agent %s: its warm process is not confirmed gone (%s): %w", entry.agentID, reason, err)
				if entry.containerized && !entry.session.Alive() {
					// The known Docker path: the host side ended and the launcher never confirmed the process in the container. The quarantine
					// handles it and releases it by itself, so it is a warning; a process that is still alive stays an error.
					c.degraded("a warm process is not confirmed gone", werr)
				} else {
					c.report("a warm process is not confirmed gone", werr)
				}
			}
			return false
		}
	}
	return true
}

func (c *Client) recordWarmEnd(entry *warmEntry, reason string) {
	pid := entry.session.Pid()
	if c.Journal != nil && pid > 0 {
		_ = c.Journal.ForgetWarmProcess(context.Background(), pid)
	}
	if entry.gone.CompareAndSwap(false, true) {
		c.warmLive.Add(-1)
		if entry.kept {
			c.noteWarmEnded(entry.agentID, reason) // the turns that ended in it said the process lived on: this is where it ends
		}
	}
	c.warmChanged()
}

// noteWarmEnded tells the server a warm process that was kept idle has ended, and why. Best effort: not connected, nothing is sent (the warm picture the
// server holds corrects itself with the next report).
func (c *Client) noteWarmEnded(agentID, reason string) {
	if c.warmEnded != nil {
		c.warmEnded(agentID, reason)
		return
	}
	go func() {
		defer telemetry.Recover(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.WarmEndedFrame, AgentID: agentID, Message: reason})
	}()
}

// noteBackgroundEnded tells the server that the agent's live process answered, between turns, a background job's end (see
// adapter.TurnSpec.OnBackgroundResult): the server queues a note so the agent gets a real turn (at most one per turn). Best effort and never
// blocking: the adapter's reader calls it, and a report that cannot be sent is only logged (the answer is still in the next turn's journal).
func (c *Client) noteBackgroundEnded(agentID string) {
	if c.backgroundEnded != nil {
		c.backgroundEnded(agentID)
		return
	}
	c.mu.Lock()
	generation := c.desired[agentID].Generation
	c.mu.Unlock()
	go func() {
		defer telemetry.Recover(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.BackgroundJobEndedFrame, AgentID: agentID, Generation: generation}); err != nil {
			c.reportLocal(fmt.Errorf("agent %s: a background job's end could not be reported: %w", agentID, err))
		}
	}()
}

// finish ends a process the slot held and keeps hold of it as stuck when it is not confirmed gone, so that nothing starts another process on the
// session until it is. The caller holds slot.mu.
func (c *Client) finish(slot *warmSlot, entry *warmEntry, reason string, grace time.Duration) bool {
	if entry != nil && entry.gone.Load() {
		return true
	}
	if entry != nil && slot.stuck == entry {
		return false // the background watcher owns this process's slow retries
	}
	if c.endWarm(entry, reason, grace) {
		return true
	}
	if entry != nil {
		slot.setStuck(entry)
		entry.stuckAt = time.Now()
		entry.stuckReason = reason
		entry.stuckNudge = make(chan struct{}, 1)
		pool := c.pool()
		pool.watchers.Add(1)
		telemetry.Go(context.Background(), func() { defer pool.watchers.Done(); c.watchStuck(pool, slot, entry) })
	}
	return false
}

// A host wrapper that was killed may no longer be able to confirm its
// container's end. Keep its session quarantined for the wrapper's cleanup
// window, then release only after the host process has been reaped.
var warmStuckQuarantine = 2 * time.Minute

func (c *Client) watchStuck(pool *warmPool, slot *warmSlot, entry *warmEntry) {
	wait := warmRetryEvery
	exited := entry.session.Exited()
	// The quarantine's own deadline wakes the watcher too, so a release is not left to the backoff (up to five minutes late).
	quarantine := time.NewTimer(time.Until(entry.stuckAt.Add(warmStuckQuarantine)))
	defer quarantine.Stop()
	quarantineDone := quarantine.C
	// Between its retries the watcher asks Docker whether the process's container is still there: one that is gone settles the slot at once.
	var containerPoll <-chan time.Time
	if _, ok := entry.session.(adapter.ContainerSession); ok && entry.containerized {
		ticker := time.NewTicker(warmContainerPollEvery)
		defer ticker.Stop()
		containerPoll = ticker.C
	}
	retryAt := time.Now().Add(wait)
	for {
		timer := time.NewTimer(time.Until(retryAt))
		containerOnly := false
		select {
		case <-containerPoll:
			timer.Stop()
			containerOnly = true
		case <-entry.stuckNudge:
			timer.Stop()
		case <-quarantineDone:
			quarantineDone = nil
			timer.Stop()
		case <-pool.ctx.Done():
			timer.Stop()
			return
		case <-exited:
			exited = nil
			timer.Stop()
		case <-timer.C:
		}
		slot.mu.Lock()
		if slot.stuck != entry {
			slot.mu.Unlock()
			return
		}
		cleared := c.clearStuck(slot)
		slot.mu.Unlock()
		if cleared {
			return
		}
		if containerOnly {
			why, gone := c.warmContainerGone(entry)
			if !gone {
				continue // the retry keeps its own time
			}
			slot.mu.Lock()
			if slot.stuck == entry {
				log.Printf("agent %s: its warm process is confirmed gone: %s", entry.agentID, why)
				c.recordWarmEnd(entry, "its container is gone")
				slot.setStuck(nil)
			}
			slot.mu.Unlock()
			return
		}
		confirmed := c.tryEndWarm(entry, "a background retry", 0)
		slot.mu.Lock()
		if slot.stuck != entry {
			slot.mu.Unlock()
			return
		}
		if confirmed {
			c.recordWarmEnd(entry, "a background retry")
			slot.setStuck(nil)
		} else {
			confirmed = c.clearStuck(slot)
		}
		slot.mu.Unlock()
		if confirmed {
			return
		}
		// Still stuck: try again after twice as long, so a slot that nothing
		// can settle does not spin (and log) forever.
		if wait < warmStuckRetryMax {
			wait *= 2
			if wait > warmStuckRetryMax {
				wait = warmStuckRetryMax
			}
		}
		retryAt = time.Now().Add(wait)
	}
}

// warmStuckRetryMax caps how long a stuck slot's background retry waits once
// the backoff has grown: about five minutes.
var warmStuckRetryMax = 5 * time.Minute

// plannedWarmEnd says why a warm process was ended on purpose: a stop of its
// agent, or the supervisor shutting down. The quarantine such an end runs and
// the release when it is over are the normal course of that decision, not a
// failure to report; anything else (a process that would not die on its own,
// a start that could not be tracked) is worth the report.
func plannedWarmEnd(reason string) bool {
	return reason == "the agent was stopped" || reason == "the supervisor is stopping"
}

// warmNudgeEvery bounds how often one stuck process's watcher is woken early: a stopping agent's retry loop asks every couple of seconds, and
// each wake is an end attempt that skips the backoff.
var warmNudgeEvery = 30 * time.Second

// nudgeStuck asks the stuck watcher to retry now, so a process that has since recovered is confirmed at once and not at the next backoff step.
// At most one nudge per warmNudgeEvery wakes it. The caller holds slot.mu.
func nudgeStuck(slot *warmSlot) {
	entry := slot.stuck
	if entry == nil || entry.stuckNudge == nil {
		return
	}
	if !entry.lastNudge.IsZero() && time.Since(entry.lastNudge) < warmNudgeEvery {
		return
	}
	select {
	case entry.stuckNudge <- struct{}{}:
		entry.lastNudge = time.Now()
	default:
	}
}

// clearStuck makes only cheap checks so a delivery does not wait behind a
// slow background Close or Kill. The caller holds slot.mu.
func (c *Client) clearStuck(slot *warmSlot) bool {
	if slot.stuck == nil {
		return true
	}
	entry := slot.stuck
	if !entry.containerized && !entry.session.Alive() {
		c.recordWarmEnd(entry, "its host process exited")
		slot.setStuck(nil)
		return true
	}
	if time.Since(entry.stuckAt) >= warmStuckQuarantine {
		if !entry.session.Alive() && slot.busy != entry {
			// A released quarantine is the supervisor's own recovery, so it goes to the local log only: the start of the quarantine is
			// the report that matters. No news at all when the process was ended on purpose (a stop, a shutdown): its unconfirmed
			// container end and the quarantine are that decision's own path.
			if !entry.plannedEnd && !plannedWarmEnd(entry.stuckReason) {
				c.reportLocal(fmt.Errorf("agent %s: host wrapper exited but its container end was not confirmed; releasing the session after quarantine", entry.agentID))
			}
			c.recordWarmEnd(entry, "quarantine ended after its host wrapper exited")
			slot.setStuck(nil)
			return true
		}
		if entry.quarantineReported.CompareAndSwap(false, true) {
			c.report("a warm process remains alive after quarantine", fmt.Errorf("agent %s: its warm process is still alive, so the session remains quarantined", entry.agentID))
		}
	}
	return false
}

// retireWarm ends an agent's warm process and returns once it is confirmed gone (false: it is not, and the caller must not go on to acknowledge
// a stop or drop a key: it is tried again by whoever asks next, and a stop is retried in the background): the caller then goes on to acknowledge
// a stop or drop a key, so nothing is left holding either. An idle process is closed gracefully and ended. One that is running a turn is, for a
// stop (killBusy), killed too: the turn then ends as a cancelled one; for a key change it is only marked, so it ends with its turn instead of
// going idle.
func (c *Client) retireWarm(agentID, reason string) bool {
	return c.retireWarmAgent(agentID, reason, false)
}

// stopWarm is retireWarm for an agent that was stopped: whatever its process is doing, it ends before the stop is acknowledged.
func (c *Client) stopWarm(agentID, reason string) bool {
	pingsGone := c.keepAliveForget(agentID) // a stopped agent's cache is no longer kept
	return c.retireWarmAgent(agentID, reason, true) && pingsGone
}

func (c *Client) retireWarmAgent(agentID, reason string, killBusy bool) bool {
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool == nil {
		return true
	}
	pool.mu.Lock()
	slot := pool.slots[agentID]
	pool.mu.Unlock()
	if slot == nil {
		return true
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return c.retireSlot(slot, reason, killBusy, warmCloseGrace)
}

// retireSlot is retireWarmAgent with the slot's lock held.
func (c *Client) retireSlot(slot *warmSlot, reason string, killBusy bool, grace time.Duration) bool {
	ok := c.clearStuck(slot)
	if !ok {
		nudgeStuck(slot)
	}
	if slot.busy != nil {
		slot.busy.retire = true
		if killBusy && !c.finish(slot, slot.busy, reason, 0) {
			ok = false
		}
	}
	if !c.finish(slot, slot.takeIdle(), reason, grace) {
		ok = false
	}
	return ok
}

// RetireAllWarm ends every idle warm process (a settings change, a credential switch) and returns once they are confirmed gone (false: one is
// not); the processes close side by side, so the wait is the longest grace and not their sum.
func (c *Client) RetireAllWarm(reason string) bool { return c.retireAll(reason, warmCloseGrace) }

// warmCapable says this supervisor can keep processes warm: its adapter has a warm mode. (Whether it does is the instance's window.)
func (c *Client) warmCapable() bool {
	if multi, ok := c.Adapter.(interface{ HasWarm() bool }); ok {
		return multi.HasWarm()
	}
	_, ok := c.Adapter.(adapter.WarmAdapter)
	return ok
}

// RetireWarmTool ends the idle warm processes of one tool, and the cache pings that run for it, and returns once they are confirmed gone (false: one is
// not). A running turn is left alone (the account switch refuses while one runs), but its process is marked to end with the turn.
func (c *Client) RetireWarmTool(tool, reason string) bool {
	// Cache pings of the tool come first: a ping is a turn on the login about to change, and the server's own check for a running turn cannot see it. They
	// are held back (no new one starts, and a pending timer finds the hold) and the ones that run are ended and confirmed, so no ping holds a warm
	// process or the login while the slots below are retired.
	pingsEnded := c.keepAliveHoldTool(tool)
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool == nil {
		return true
	}
	var wait sync.WaitGroup
	var failed atomic.Bool
	for _, slot := range pool.existing() {
		wait.Add(1)
		go func() {
			defer telemetry.Recover(context.Background())
			defer wait.Done()
			slot.mu.Lock()
			defer slot.mu.Unlock()
			if slot.busy != nil && slot.busy.tool == tool {
				slot.busy.retire = true
			}
			idleOfTool := slot.idle != nil && slot.idle.tool == tool
			stuckOfTool := slot.stuck != nil && slot.stuck.tool == tool
			switch {
			case idleOfTool:
				if !c.retireSlot(slot, reason, false, warmCloseGrace) {
					failed.Store(true)
				}
			case stuckOfTool:
				if !c.clearStuck(slot) {
					nudgeStuck(slot)
					failed.Store(true)
				}
			}
		}()
	}
	wait.Wait()
	return pingsEnded && !failed.Load()
}

// setCodexWarmEnabled applies the owner's opt-in. Turning it off retires
// Codex app-server processes without holding back ordinary cold keep-alive
// pings, which are controlled by a separate setting.
func (c *Client) setCodexWarmEnabled(enabled bool) {
	c.mu.Lock()
	wasEnabled := c.codexWarmEnabled
	c.codexWarmEnabled = enabled
	pool := c.warmPool
	c.mu.Unlock()
	if enabled != wasEnabled {
		c.reportLauncherStatus()
	}
	if enabled || !wasEnabled || pool == nil {
		return
	}
	for _, slot := range pool.existing() {
		slot.mu.Lock()
		if slot.busy != nil && slot.busy.tool == "codex" {
			slot.busy.retire = true
		}
		if slot.idle != nil && slot.idle.tool == "codex" {
			_ = c.retireSlot(slot, "Codex warm mode was turned off", false, warmCloseGrace)
		} else if slot.stuck != nil && slot.stuck.tool == "codex" {
			if !c.clearStuck(slot) {
				nudgeStuck(slot)
			}
		}
		slot.mu.Unlock()
	}
}

func (c *Client) reportWarmRefusal(spec adapter.TurnSpec, cred credentials, action string, err error) {
	if errors.Is(err, adapter.ErrNoWarm) && err.Error() == adapter.ErrNoWarm.Error() {
		return // ordinary cold-only turns have no refusal reason to report
	}
	if errors.Is(err, adapter.ErrNoWarm) {
		class := warmRefusalClass(err)
		tool := spec.Agent.Tool
		if tool != "claude" && tool != "codex" {
			tool = "other"
		}
		telemetry.CaptureWarning(context.Background(), "warm-refusal:"+tool+":"+class,
			fmt.Errorf("%s warm refused: %s", tool, class))
	}
	key := strings.Join([]string{action, err.Error(), cred.stamp, spec.Agent.Model, spec.Agent.LauncherID}, "\x00")
	c.mu.Lock()
	if errors.Is(err, adapter.ErrNoWarm) {
		if c.warmRefusals == nil {
			c.warmRefusals = make(map[string]protocol.WarmRefusal)
		}
		// Only the adapter's classified reason is sent to the web UI. Other
		// errors can include paths or credentials and remain in the local log.
		reason := strings.TrimPrefix(err.Error(), adapter.ErrNoWarm.Error()+": ")
		if runes := []rune(reason); len(runes) > 240 {
			reason = string(runes[:240])
		}
		c.warmRefusals[spec.Agent.ID] = protocol.WarmRefusal{AgentID: spec.Agent.ID, Reason: reason}
	}
	if c.warmWarnings == nil {
		c.warmWarnings = make(map[string]string)
	}
	if c.warmWarnings[spec.Agent.ID] == key {
		c.mu.Unlock()
		c.warmChanged()
		return
	}
	c.warmWarnings[spec.Agent.ID] = key
	c.mu.Unlock()
	c.warmChanged()
	c.reportLocal(fmt.Errorf("agent %s: %s: %w; the turn runs cold", spec.Agent.ID, action, err)) // its Sentry report is the classified warning above
}

var warmRefusalNamedKey = regexp.MustCompile(`(?:host config key|host feature) "([A-Za-z_][A-Za-z0-9_-]*)"`)

func warmRefusalClass(err error) string {
	reason := err.Error()
	if match := warmRefusalNamedKey.FindStringSubmatch(reason); len(match) == 2 {
		return match[1]
	}
	switch {
	case strings.Contains(reason, "token-only credentials"):
		return "credentials"
	case strings.Contains(reason, "Docker Codex home"):
		return "docker-home"
	case strings.Contains(reason, "plugin"):
		return "plugins"
	case strings.Contains(reason, "MCP"):
		return "mcp-servers"
	default:
		return "other"
	}
}

func (c *Client) clearWarmRefusal(agentID string) {
	c.mu.Lock()
	_, had := c.warmRefusals[agentID]
	delete(c.warmRefusals, agentID)
	c.mu.Unlock()
	if had {
		c.warmChanged()
	}
}

func (c *Client) retireAll(reason string, grace time.Duration) bool {
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool == nil {
		return true
	}
	var wait sync.WaitGroup
	var failed atomic.Bool
	for _, slot := range pool.existing() {
		wait.Add(1)
		go func() {
			defer telemetry.Recover(context.Background())
			defer wait.Done()
			slot.mu.Lock()
			defer slot.mu.Unlock()
			if !c.retireSlot(slot, reason, false, grace) {
				failed.Store(true)
			}
		}()
	}
	wait.Wait()
	return !failed.Load()
}

// warmRetryEvery is how often a stop whose process is not confirmed gone tries again.
var warmRetryEvery = 2 * time.Second

// stopWarmUntilGone keeps trying to end a stopped agent's warm process, and runs then once it is confirmed gone (the stop's ack and the key's
// drop): they do not happen while a process of the agent may still be alive.
func (c *Client) stopWarmUntilGone(agentID string, then func()) {
	every := warmRetryEvery // read once, before the goroutine: the watcher can release the slot, and a test reset the interval, while this still sleeps
	go func() {
		defer telemetry.Recover(context.Background())
		for {
			time.Sleep(every)
			c.mu.Lock()
			stillStopped := c.desired[agentID].Desired == "stopped"
			c.mu.Unlock()
			if !stillStopped { // the agent was started again: the stop no longer waits for anything
				return
			}
			if c.stopWarm(agentID, "a retry after a process was not confirmed gone") {
				then()
				return
			}
		}
	}()
}

// warmTurn is a turn that runs in a live process; the supervisor settles the process when the turn ends.
type warmTurn struct {
	adapter.Turn
	entry *warmEntry
}

func (t *warmTurn) LastOutput() time.Time {
	if lo, ok := t.Turn.(interface{ LastOutput() time.Time }); ok {
		return lo.LastOutput()
	}
	return time.Time{}
}

// Kill ends the process, not only the turn: a turn in a warm process cannot be separated from it.
func (t *warmTurn) Kill() error { return t.entry.session.Kill() }

// Terminate ends the process the turn runs in the way a container's launcher can confirm: its input is closed and it is given the grace to go on
// its own (the launcher cleans up and the end is confirmed), and it is killed after. A ping in a warm container process is ended this way, and without
// it the turn's own Cancel would be all a supervisor could do, which a container's wrapper does not confirm. It says whether the end is confirmed.
func (t *warmTurn) Terminate(grace time.Duration) bool { return t.entry.session.Close(grace) == nil }

// warmCredentials identifies the on-disk login a Claude or Codex process
// would use. For Claude this is the credentials file in the config directory
// the launcher gives it (CLAUDE_CONFIG_DIR through the owner's variable references, else the home's .claude), by its size,
// modification time and a hash of its content (a profile restored to the same size and time still differs). A login that changed on disk (an account switch, a token refresh) is another stamp, so the process started under the old
// one is not reused: each refresh makes the next turn cold. ok is false when it cannot be told reliably, and the turn then runs cold: no file
// (a login kept in the keychain, or none), or a Docker launcher that does not share the host's Claude home, whose login is then not the file's.
func warmCredentials(spec adapter.TurnSpec, sharesHome bool, codexFile ...string) (stamp string, ok bool) {
	if spec.Agent.Tool != "claude" && spec.Agent.Tool != "codex" {
		return "", true
	}
	if !sharesHome {
		return "", false
	}
	if spec.Agent.Tool == "codex" && (spec.LauncherContainerized || spec.Agent.Variant == "docker") && (len(codexFile) == 0 || codexFile[0] == "") {
		return "", false
	}
	var file, stampPath string
	if spec.Agent.Tool == "codex" {
		file = codexAuthPath(spec)
		if len(codexFile) > 0 && codexFile[0] != "" {
			file = codexFile[0]
		}
		stampPath = file
	} else {
		dir, _, known := claudeLoginDir(spec)
		if !known {
			return "", false
		}
		file = filepath.Join(dir, ".credentials.json")
		stampPath = dir
	}
	info, err := os.Stat(file)
	if err != nil {
		if spec.Agent.Tool == "codex" && errors.Is(err, os.ErrNotExist) {
			key := adapter.EnvValue("OPENAI_API_KEY", spec.LauncherEnvRefs)
			if key == "" {
				key = os.Getenv("OPENAI_API_KEY")
			}
			if key != "" {
				sum := sha256.Sum256([]byte(key))
				return fmt.Sprintf("codex-api-key:%x", sum[:8]), true
			}
		}
		return "", false
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%s:%d:%d:%x", stampPath, info.Size(), info.ModTime().UnixNano(), sum[:8]), true
}

func codexAuthPath(spec adapter.TurnSpec) string {
	dir := adapter.EnvValue("CODEX_HOME", spec.LauncherEnvRefs)
	if dir == "" {
		dir = os.Getenv("CODEX_HOME")
	}
	if dir == "" {
		home := adapter.EnvValue("HOME", spec.LauncherEnvRefs)
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		dir = filepath.Join(home, ".codex")
	}
	return filepath.Join(dir, "auth.json")
}

func (c *Client) warmCodexAuthPath(spec adapter.TurnSpec) string {
	if spec.Agent.Tool != "codex" {
		return ""
	}
	if source, ok := c.Adapter.(interface{ WarmAuthPath(adapter.TurnSpec) string }); ok {
		if path := source.WarmAuthPath(spec); path != "" {
			return path
		}
	}
	if spec.LauncherContainerized || spec.Agent.Variant == "docker" {
		return ""
	}
	return codexAuthPath(spec)
}

// claudeLoginDir is the configuration directory a Claude process of the turn would use (CLAUDE_CONFIG_DIR through the launcher's variable
// references, else the home's .claude), and the home it would find its account file in when it has no such variable.
func claudeLoginDir(spec adapter.TurnSpec) (dir, accountHome string, ok bool) {
	if dir = adapter.EnvValue("CLAUDE_CONFIG_DIR", spec.LauncherEnvRefs); dir != "" {
		return dir, "", true
	}
	home := adapter.EnvValue("HOME", spec.LauncherEnvRefs)
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", "", false
		}
	}
	return filepath.Join(home, ".claude"), home, true
}

// warmAccount is which account the login is, as Claude's own account-state file names it (oauthAccount.accountUuid, else the e-mail address, lower
// case): the credentials file holds tokens only, and a renewed token is another stamp of the same account. The file beside the credentials is the one
// a process started with CLAUDE_CONFIG_DIR uses; without that variable it is the one in the home's root; the newest that names an account wins, as
// a /login rewrites the one its process uses. "" when no file names one.
func warmAccount(spec adapter.TurnSpec, codexFile ...string) string {
	if spec.Agent.Tool == "codex" {
		if (spec.LauncherContainerized || spec.Agent.Variant == "docker") && (len(codexFile) == 0 || codexFile[0] == "") {
			return ""
		}
		path := codexAuthPath(spec)
		if len(codexFile) > 0 && codexFile[0] != "" {
			path = codexFile[0]
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return credtarget.CodexAccount(data)
	}
	if spec.Agent.Tool != "claude" {
		return ""
	}
	dir, home, ok := claudeLoginDir(spec)
	if !ok {
		return ""
	}
	candidates := []string{filepath.Join(dir, ".claude.json")}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".claude.json"))
	}
	best, bestTime := "", time.Time{}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file struct {
			OAuthAccount struct {
				AccountUUID  string `json:"accountUuid"`
				EmailAddress string `json:"emailAddress"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(data, &file) != nil {
			continue
		}
		who := strings.ToLower(strings.TrimSpace(file.OAuthAccount.AccountUUID))
		if who == "" {
			who = strings.ToLower(strings.TrimSpace(file.OAuthAccount.EmailAddress))
		}
		if who != "" && (best == "" || info.ModTime().After(bestTime)) {
			best, bestTime = who, info.ModTime()
		}
	}
	return best
}

// startTurn starts a turn: in an agent's live process when there is one that is still what the turn needs, in a new warm process when warm
// mode is on and the adapter serves the turn warm, and as a process of its own otherwise. Warm mode never fails a turn: a process that cannot
// be started, recorded or handed the turn is reported and the turn runs cold.
func (c *Client) startTurn(ctx context.Context, spec adapter.TurnSpec, sink io.Writer, cred credentials) (adapter.Turn, error) {
	if !spec.KeepAlive {
		c.clearWarmRefusal(spec.Agent.ID)
	}
	if !spec.KeepAlive && !c.keepAliveYield(spec.Agent.ID) {
		// A ping of the agent was told to end and is not confirmed gone: it may still be on the session, and a second process must never be. The
		// turn waits for the quarantine to be over (this runs in the delivery's own goroutine) and then goes on, rather than failing its start.
		if !c.keepAliveWaitQuarantine(ctx, spec.Agent.ID) {
			return nil, errWarmUnconfirmed
		}
	}
	if !spec.KeepAlive {
		// The agent's last warm process may not be confirmed gone yet (a turn-limit kill of a process in a container is the usual case): the turn
		// waits for its quarantine to be over (this runs in the delivery's own goroutine, see warmStuck) rather than failing at once, which sent
		// a limit kill's automatic continuation straight to recovery.
		if err := c.waitWarmStuck(ctx, spec.Agent.ID); err != nil {
			return nil, err
		}
		// A lift the last ping owes is sent first (and its ack waited for): a real turn's toolbox is never refused because of a ping.
		if err := c.keepAliveSettleLifts(ctx, spec.Agent.ID); err != nil {
			return nil, fmt.Errorf("the scope of the agent's last keep-alive ping could not be lifted, so its toolbox would be refused: %w", err)
		}
	}
	cfg := c.warmCfg()
	c.mu.Lock()
	codexWarm := c.codexWarmEnabled
	c.mu.Unlock()
	if spec.Agent.Tool == "codex" && !codexWarm {
		if !c.retireWarm(spec.Agent.ID, "Codex warm mode is off") {
			return nil, errWarmUnconfirmed
		}
		return c.Adapter.Start(ctx, spec)
	}
	warm, ok := c.Adapter.(adapter.WarmAdapter)
	if cfg.Idle <= 0 || !ok {
		if !c.retireWarm(spec.Agent.ID, "warm mode is off") {
			return nil, errWarmUnconfirmed // a process of the agent may still be alive: no other starts on its session
		}
		return c.Adapter.Start(ctx, spec)
	}
	fp, err := warm.WarmFingerprint(spec)
	if err != nil {
		c.reportWarmRefusal(spec, cred, "warm unavailable", err)
		// A turn that cannot run warm (a correction, a diagnostic, a fork, a spec that does not build) may be on the session of a live process:
		// that one ends first, so two processes are never working on one session.
		if !c.retireWarm(spec.Agent.ID, "a turn that runs cold") {
			return nil, errWarmUnconfirmed
		}
		return c.Adapter.Start(ctx, spec)
	}
	if !cred.ok || !cred.containerOK {
		// The login cannot be told, or the process would run in a container whose launcher cannot confirm its end: no warm process.
		if !c.retireWarm(spec.Agent.ID, "this turn cannot run warm") {
			return nil, errWarmUnconfirmed
		}
		return c.Adapter.Start(ctx, spec)
	}
	pool := c.pool()
	slot := pool.slot(spec.Agent.ID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if !c.clearStuck(slot) {
		nudgeStuck(slot)
		return nil, errWarmUnconfirmed
	}
	now := pool.now()
	entry := slot.takeIdle()
	if entry != nil {
		reason := ""
		switch {
		case entry.fp != fp:
			reason = "what it was started with changed (its command, arguments or environment)"
		case entry.login != cred.stamp:
			reason = loginChangedReason
		case !entry.session.Alive():
			reason = "it had ended"
		case now.Sub(entry.started) >= cfg.lifetime():
			reason = "its lifetime is over"
		}
		if reason != "" {
			if !c.finish(slot, entry, reason, 0) {
				return nil, errWarmUnconfirmed
			}
			entry = nil
		}
	}
	reused := entry != nil
	if entry == nil && spec.KeepAlive {
		// A ping never starts a warm process (it would sit idle for the window and hold memory for nothing): it is a cold resume.
		return c.Adapter.Start(ctx, spec)
	}
	if entry == nil {
		stderr := &swapWriter{}
		started := spec
		started.StderrSink = stderr
		agentID := spec.Agent.ID
		started.OnBackgroundResult = func() { c.noteBackgroundEnded(agentID) }
		session, err := warm.StartWarm(pool.ctx, started)
		if err != nil {
			c.reportWarmRefusal(spec, cred, "starting a warm process", err)
			return c.Adapter.Start(ctx, spec)
		}
		entry = &warmEntry{agentID: spec.Agent.ID, tool: spec.Agent.Tool, session: session, fp: fp, login: cred.stamp, account: cred.account, loginNow: cred.now, started: now, stderr: stderr, containerized: spec.LauncherContainerized || spec.Agent.Variant == "docker"}
		c.warmLive.Add(1) // counted from the moment it exists until its end is confirmed (endWarm)
		// A process nobody can find again if this supervisor dies is not kept warm: the journal's note, with the process's own start time, is
		// what a later supervisor ends it by.
		identity := c.identityOf(session.Pid())
		if c.Journal == nil || session.Pid() <= 0 || identity == "" {
			c.degraded("a warm process cannot be tracked; the turn runs cold", fmt.Errorf("agent %s: a warm process cannot be tracked (no start time); the turn runs cold", spec.Agent.ID))
			if !c.finish(slot, entry, "it cannot be tracked", 0) {
				return nil, errWarmUnconfirmed
			}
			return c.Adapter.Start(ctx, spec)
		}
		if err := c.Journal.RecordWarmProcess(context.Background(), WarmProcess{PID: session.Pid(), AgentID: spec.Agent.ID, Started: identity}); err != nil {
			c.degraded("noting a warm process failed; the turn runs cold", fmt.Errorf("agent %s: noting a warm process: %w; the turn runs cold", spec.Agent.ID, err))
			if !c.finish(slot, entry, "it could not be noted", 0) {
				return nil, errWarmUnconfirmed
			}
			return c.Adapter.Start(ctx, spec)
		}
		pool.mu.Lock()
		pool.started++
		pool.mu.Unlock()
		c.sightContainer(pool, entry)
	}
	if sink != nil {
		entry.stderr.Set(sink)
	}
	turn, err := entry.session.Run(spec)
	if err != nil {
		// Still as it was reported (kept idle): its end is reported with this reason, because no turn of its own is going to say it.
		c.degraded("handing the turn to its warm process failed; the turn runs cold", fmt.Errorf("agent %s: handing the turn to its warm process: %w; the turn runs cold", spec.Agent.ID, err))
		if !c.finish(slot, entry, "it could not take the turn", 0) {
			return nil, errWarmUnconfirmed
		}
		return c.Adapter.Start(ctx, spec)
	}
	entry.kept = false // it works again: if it ends during this turn, the turn's own exit says so
	slot.busy = entry
	c.warmChanged()
	pool.mu.Lock()
	if reused {
		pool.reused++
	}
	pool.mu.Unlock()
	return &warmTurn{Turn: turn, entry: entry}, nil
}

// warmStuckWaitLimit bounds how long a delivery waits for the agent's last warm process to be confirmed gone or released: the quarantine of a
// process whose end in its container is not confirmed (warmStuckQuarantine) and a little to spare, the same 150 s an unconfirmed keep-alive ping
// blocks its agent. A process that is still provably alive after that is not released, and the turn fails to start as before.
const warmStuckWaitLimit = 150 * time.Second

// warmStuckPollEvery is how often a waiting delivery looks at the slot again (and at whether its agent was stopped meanwhile).
const warmStuckPollEvery = time.Second

// errStoppedWhileWaiting is why a turn that waited for the quarantine of the agent's last warm process is not started: the agent was stopped, or
// the supervisor began stopping, during the wait. The stop wins; the turn ends as the stop's cancel.
var errStoppedWhileWaiting = errors.New("the agent was stopped while its turn waited for its last warm process to be confirmed ended; the turn was not started")

// warmStuck says the agent's last warm process is not confirmed gone, so a delivery for it may wait (see waitWarmStuck) and is handled in a
// goroutine of its own. It takes no slot lock.
func (c *Client) warmStuck(agentID string) bool {
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool == nil {
		return false
	}
	pool.mu.Lock()
	slot := pool.slots[agentID]
	pool.mu.Unlock()
	return slot != nil && slot.stuckNow.Load()
}

// waitWarmStuck waits, up to warmStuckWaitLimit, for the agent's stuck warm process to be confirmed gone or released by its quarantine. It
// returns nil once nothing of the agent is left on the session (at once when nothing was stuck), errStoppedWhileWaiting when the agent was
// stopped or the supervisor began stopping during the wait (a stop wins over the turn), the context's error when the delivery's context ended,
// and errWarmUnconfirmed when the limit passed with the process still not settled. Each look nudges the stuck watcher (rate-limited), so a process
// that has recovered is confirmed without waiting out the backoff.
func (c *Client) waitWarmStuck(ctx context.Context, agentID string) error {
	if !c.warmStuck(agentID) {
		return nil
	}
	limit, poll := warmStuckWaitLimit, warmStuckPollEvery
	if c.warmStuckWait > 0 {
		limit = c.warmStuckWait
	}
	if c.warmStuckPoll > 0 {
		poll = c.warmStuckPoll
	}
	slot := c.pool().slot(agentID)
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	logged := false
	for {
		if c.stoppedWhileWaiting(agentID) {
			return errStoppedWhileWaiting
		}
		slot.mu.Lock()
		cleared := c.clearStuck(slot)
		if !cleared {
			nudgeStuck(slot)
		}
		slot.mu.Unlock()
		if cleared {
			if c.stoppedWhileWaiting(agentID) {
				return errStoppedWhileWaiting
			}
			return nil
		}
		if !time.Now().Before(deadline) {
			return errWarmUnconfirmed
		}
		if !logged {
			logged = true
			log.Printf("agent %s: its last warm process is not confirmed ended; the turn waits up to %v for its quarantine", agentID, limit)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the agent's last warm process to be confirmed ended: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// stoppedWhileWaiting says a turn waiting to start must not: its agent is no longer desired active, or the supervisor is stopping.
func (c *Client) stoppedWhileWaiting(agentID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopping || c.desired[agentID].Desired != "active"
}

// errWarmUnconfirmed is why a turn is not started: a warm process of the agent was ended and is not confirmed gone, and a second process on the
// session must never exist. The turn fails to start; the next delivery tries the process again.
var errWarmUnconfirmed = errors.New("the agent's warm process could not be confirmed ended, so no other process is started on its session")

// credentials is what warmCredentials says about a turn.
type credentials struct {
	stamp string
	ok    bool
	// account is which account the stamp's login is ("" when it cannot be told), and now reads both again, at another time; nil when a caller has
	// nothing to read them with.
	account string
	now     func() (stamp, account string, ok bool)
	// containerOK says the process may be kept warm: it does not run in a container, or its launcher advertises that it confirms the end of
	// the session inside the container (see containerEndFeature). Nothing else proves that process gone, so without it the agent runs cold.
	containerOK bool
}

// containerEndFeature is what a Docker launcher advertises (in its features) when, on a terminate request, its wrapper traps the signal, ends the
// session inside the container, waits for it, and only then exits with 128+signal (143 for TERM): harness-docker's `stop_on_signal`.
const containerEndFeature = "stop_on_signal"

// warmTurnEnded settles a warm process when its turn has ended and the exit is journaled: it goes idle for the window if everything allows
// it, and is ended otherwise. It holds the agent's slot throughout, so a stop that lands meanwhile is settled against: the stop has already set
// the desired state this reads, and its own retire waits for this and then finds the process idle. It runs before the turn's admission is
// released, so the next delivery finds it in the pool or finds it gone.
func (c *Client) warmTurnEnded(turn adapter.Turn, exit adapter.ExitInfo) (kept bool) {
	wt, ok := turn.(*warmTurn)
	if !ok {
		return false
	}
	entry := wt.entry
	pool := c.pool()
	slot := pool.slot(entry.agentID)
	slot.mu.Lock()
	entry.stderr.Set(nil)
	keep := c.warmKeepable(pool, entry, exit)
	var claimErr error
	if keep {
		// The server is told, and says it has it, before the process is kept: if the socket is down or it does not answer, the process ends now (the
		// next turn runs cold) rather than idle where a host that then drops would be judged by a count that never included it. The slot is unlocked
		// while that is waited for (a stop, a spawn or a delivery on the loop that reads the server's frames needs it, and the answer is read by that
		// same loop): the entry stays the slot's busy one meanwhile, so it is counted, and a retire that lands marks or ends it. The admission of the
		// turn is not released yet, so no other turn of the agent starts, and what was decided is decided again after.
		slot.mu.Unlock()
		claimErr = c.claimWarm()
		slot.mu.Lock()
		keep = claimErr == nil && c.warmKeepable(pool, entry, exit)
	}
	defer slot.mu.Unlock()
	if slot.busy == entry {
		slot.busy = nil
	}
	if !keep {
		c.mu.Lock()
		// A turn the server asked to be interrupted ends its process on purpose too.
		entry.plannedEnd = c.desired[entry.agentID].Desired != "active" || c.stopping || exit.CancelIntent || exit.ErrorEvent == turnInterruptedOnRequest
		c.mu.Unlock()
		reason := "its turn did not end cleanly, or the agent was stopped, or its lifetime is over"
		if entry.loginMoved {
			reason = loginChangedReason
		}
		if claimErr != nil {
			// The server confirms a kept process (its cap); one that cannot be asked is ended, which is right: an unconfirmed process would
			// outlive what the server counts. During an outage that is expected and only logged; a server that answered and refused is a warning.
			kerr := fmt.Errorf("agent %s: its warm process is not kept: %w", entry.agentID, claimErr)
			if c.serverUnreachable() {
				c.reportLocal(kerr)
			} else {
				c.degraded("a warm process is not kept", kerr)
			}
			reason = "the server did not confirm that it is kept"
		}
		c.finish(slot, entry, reason, 0)
		return false
	}
	now := pool.now()
	cfg := c.warmCfg()
	entry.kept = true // the turn is reported as finished with the process alive: its end is reported when it comes
	entry.idleAt = now
	entry.idleGen++
	gen := entry.idleGen
	entry.idleStop = make(chan struct{})
	entry.timer = pool.after(cfg.Idle, func() { c.expireWarm(entry, gen) })
	slot.idle = entry
	stop := entry.idleStop // read here, under the slot's lock, as the go statement's arguments always were
	telemetry.Go(context.Background(), func() { c.watchIdle(slot, entry, stop) })
	c.warmChanged()
	return true
}

// warmKeepable says the process of a turn that has ended should be kept idle: warm mode is on, the agent is active, nothing asked for its end, the turn
// ended cleanly, the process is alive and not past its lifetime. The caller holds the slot's lock.
func (c *Client) warmKeepable(pool *warmPool, entry *warmEntry, exit adapter.ExitInfo) bool {
	c.mu.Lock()
	active := c.desired[entry.agentID].Desired == "active" && !c.stopping && (entry.tool != "codex" || c.codexWarmEnabled)
	c.mu.Unlock()
	cfg := c.warmCfg()
	return cfg.Idle > 0 && active && !entry.retire && exit.TerminalEvent && exit.ExitCode == 0 && !exit.CancelIntent &&
		entry.session.Alive() && pool.now().Sub(entry.started) < cfg.lifetime() && entry.loginHolds()
}

// loginChangedReason is why a process ends when the login it works under is not the one on disk.
const loginChangedReason = "the login it works under changed"

// loginHolds says the login on disk, after a turn, is still the process's own. Claude renews an expired token itself while it works and writes the
// credentials file, so the stamp moves under a process that is perfectly fine: when the file still names the same account the new stamp is adopted
// (the next turn must not take the process's own renewal for another login). A different account (someone ran `claude /login`, outside aimebu's
// account switch, which refuses while a turn runs), or one that cannot be told, is not adopted: the process ends with loginChangedReason rather than
// stay on under a login that was never checked.
func (e *warmEntry) loginHolds() bool {
	if e.loginNow == nil {
		return true
	}
	stamp, account, ok := e.loginNow()
	switch {
	case !ok:
		e.loginMoved = true
		return false
	case stamp == e.login:
		return true
	case account != "" && account == e.account:
		e.login = stamp
		return true
	}
	e.loginMoved = true
	return false
}

// warmClaimWait is how long the server's answer to a warm claim is waited for.
var warmClaimWait = 5 * time.Second

// claimWarm tells the server how many processes (warm and pings) the supervisor has running, counting the one about to be kept, and waits for its
// answer. It takes no slot lock: the count is the supervisor's own atomics, not a walk of the slots (which a caller holding one slot's lock could not do
// without waiting for another's).
func (c *Client) claimWarm() error {
	ctx, cancel := context.WithTimeout(context.Background(), warmClaimWait)
	defer cancel()
	seq := c.warmSeq.Add(1)
	count := int(c.warmLive.Load()) + c.keepAlivePingsActive()
	if c.warmClaim != nil {
		return c.warmClaim(ctx, seq, count)
	}
	return c.sendForAck(ctx, protocol.Frame{V: protocol.Version, Type: protocol.WarmClaimFrame, WarmSeq: seq, WarmCount: count})
}

// expireWarm is the idle timer: the agent sleeps if nothing took its process in the meantime.
func (c *Client) expireWarm(entry *warmEntry, gen int) {
	slot := c.pool().slot(entry.agentID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.idle != entry || entry.idleGen != gen {
		return
	}
	slot.takeIdle()
	c.finish(slot, entry, "the idle window ended", warmCloseGrace)
}

// watchIdle watches a process while it is idle: if it ends on its own (it crashed, or was killed from outside) the agent is asleep from that
// moment, not at its next message, and what the process said on stderr is put in the supervisor's log, bounded.
func (c *Client) watchIdle(slot *warmSlot, entry *warmEntry, stop <-chan struct{}) {
	select {
	case <-stop:
		return
	case <-entry.session.Exited():
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.idle != entry {
		return
	}
	slot.takeIdle()
	tail := ""
	if reporter, ok := entry.session.(interface{ StderrTail() string }); ok {
		tail = strings.TrimSpace(reporter.StderrTail())
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
	}
	c.warmEndedByItself(entry.agentID, fmt.Errorf("agent %s: its idle warm process ended by itself; the next turn starts a new one%s", entry.agentID, tailNote(tail)))
	c.finish(slot, entry, "it ended while idle", 0) // already gone: this only notes that it is
}

func tailNote(tail string) string {
	if tail == "" {
		return ""
	}
	return ": " + adapter.SanitizeStderr(tail)
}

// warmIdleEndWindow is how far back a previous idle end of the same agent still counts against it.
const warmIdleEndWindow = time.Hour

// warmEndedByItself reports an idle warm process that ended by itself. The first two within the window are warnings (the
// system recovers by starting a new one on the next turn); from the third within the window it is an error, because a
// harness that keeps falling over while idle is a failure to triage, not noise; once the window has passed it is a warning
// again. The error has its own fixed phrase (and so its own once-a-day report), because every cause is sent once a day:
// under the warning's phrase the escalated level would never go out, the day's first warning always being sent first.
func (c *Client) warmEndedByItself(agentID string, err error) {
	now := time.Now()
	if c.warmEndClock != nil {
		now = c.warmEndClock()
	}
	c.warmEndsMu.Lock()
	if c.warmEnds == nil {
		c.warmEnds = map[string][]time.Time{}
	}
	recent := c.warmEnds[agentID][:0]
	for _, at := range c.warmEnds[agentID] {
		if now.Sub(at) < warmIdleEndWindow {
			recent = append(recent, at)
		}
	}
	c.warmEnds[agentID] = append(recent, now)
	count := len(c.warmEnds[agentID])
	c.warmEndsMu.Unlock()
	if count >= 3 {
		c.report("an idle warm process ended by itself repeatedly within an hour", err)
		return
	}
	c.degraded("an idle warm process ended by itself", err)
}

// WarmState is what Health and the agent card need to say about warm processes.
type WarmState struct {
	Idle    map[string]time.Time // agent -> when its idle window ends
	Busy    int
	Started int
	Reused  int
	// UnconfirmedPingEnds counts keep-alive pings in a container whose end was never confirmed and whose quarantine ran out (see keepAliveQuarantine).
	UnconfirmedPingEnds int
	// RSSBytes is the resident memory of the warm processes the supervisor runs itself. ContainerWideBytes is what every running container uses
	// (Docker reports no more finely, and the harness container is shared by all agents), where the memory of Docker warm processes is: it is a
	// figure for the machine's containers, not the warm agents' own, and Health says so.
	RSSBytes           int64
	ContainerWideBytes int64
}

// WarmState reports the warm processes. The processes are listed under the locks and measured after them.
func (c *Client) WarmState() WarmState {
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	state := WarmState{Idle: map[string]time.Time{}, UnconfirmedPingEnds: int(c.kaUnconfirmed.Load())}
	if pool == nil {
		return state
	}
	cfg := c.warmCfg()
	type measured struct {
		pid           int
		containerized bool
	}
	var processes []measured
	for id, slot := range pool.existing() {
		slot.mu.Lock()
		if slot.idle != nil {
			state.Idle[id] = slot.idle.idleAt.Add(cfg.Idle)
			processes = append(processes, measured{slot.idle.session.Pid(), slot.idle.containerized})
		}
		if slot.busy != nil {
			state.Busy++
			processes = append(processes, measured{slot.busy.session.Pid(), slot.busy.containerized})
		}
		slot.mu.Unlock()
	}
	pool.mu.Lock()
	state.Started, state.Reused = pool.started, pool.reused
	pool.mu.Unlock()
	docker := false
	for _, process := range processes {
		if process.containerized {
			docker = true
			continue
		}
		state.RSSBytes += groupRSS(process.pid)
	}
	if docker {
		measure := dockerMemory
		if c.FreeDockerMemory != nil {
			measure = c.FreeDockerMemory
		}
		if used, _, err := measure(); err == nil {
			state.ContainerWideBytes = used
		}
	}
	return state
}

// warmGuard is the memory guard: while free memory is under the limit it ends the longest-idle warm process, one at a time.
func (c *Client) warmGuard(ctx context.Context) {
	ticker := time.NewTicker(warmGuardEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.guardOnce()
		}
	}
}

// guardOnce checks memory once and ends the longest-idle warm process if it is short. Memory is the host's; with a Docker warm process it is
// also the container side's (the VM's memory less what the containers use), and either being short counts.
func (c *Client) guardOnce() bool {
	cfg := c.warmCfg()
	if cfg.Idle <= 0 || (cfg.MinFreeBytes <= 0 && cfg.MinFreePercent <= 0) {
		return false
	}
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool == nil {
		return false
	}
	short := func(free, total int64) bool {
		return total > 0 && (free < cfg.MinFreeBytes || float64(free)*100 < float64(total)*cfg.MinFreePercent)
	}
	free, total, err := freeMemory()
	if c.FreeMemory != nil {
		free, total, err = c.FreeMemory()
	}
	low := err == nil && short(free, total)
	if !low && c.warmHasDocker(pool) {
		measure := dockerMemory
		if c.FreeDockerMemory != nil {
			measure = c.FreeDockerMemory
		}
		if used, vmTotal, derr := measure(); derr == nil && vmTotal > 0 {
			low = short(vmTotal-used, vmTotal)
		}
	}
	if !low {
		return false
	}
	// The longest-idle process, chosen from a look at every slot; then its slot is locked and the choice re-checked, so a process that was taken
	// (or taken and put back with a new window) in the meantime is left alone.
	var oldest *warmSlot
	var chosen *warmEntry
	var chosenAt time.Time
	for _, slot := range pool.existing() {
		slot.mu.Lock()
		if slot.idle != nil && (chosen == nil || slot.idle.idleAt.Before(chosenAt)) {
			oldest, chosen, chosenAt = slot, slot.idle, slot.idle.idleAt
		}
		slot.mu.Unlock()
	}
	if chosen == nil {
		return false
	}
	oldest.mu.Lock()
	defer oldest.mu.Unlock()
	if oldest.idle != chosen || !chosen.idleAt.Equal(chosenAt) {
		return false
	}
	oldest.takeIdle()
	return c.finish(oldest, chosen, "memory pressure", warmCloseGrace)
}

// warmHasDocker says whether any warm process runs through a Docker launcher.
func (c *Client) warmHasDocker(pool *warmPool) bool {
	for _, slot := range pool.existing() {
		slot.mu.Lock()
		docker := (slot.idle != nil && slot.idle.containerized) || (slot.busy != nil && slot.busy.containerized)
		slot.mu.Unlock()
		if docker {
			return true
		}
	}
	return false
}

// shutdownWarm ends every warm process for good: no more are started.
func (c *Client) shutdownWarm() {
	c.keepAliveForgetAll()
	c.retireAll("the supervisor is stopping", 0)
	c.mu.Lock()
	pool := c.warmPool
	c.mu.Unlock()
	if pool != nil {
		pool.cancel()
	}
	c.sendFinalWarmState()
}

// sendFinalWarmState tells the server, synchronously and before the connection closes, what is left after a clean stop: normally no warm slot and no
// ping, which is what lets a server that then restarts (or whose switch finds the host gone) know the host has nothing running. It reports what is
// really there: a process that could not be confirmed gone is still counted. Not connected, or a warm-less supervisor, sends nothing.
func (c *Client) sendFinalWarmState() {
	if !c.warmCapable() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.sendWarmReport(ctx)
}

// identityOf is the system's record of when a process started (tests replace it).
func (c *Client) identityOf(pid int) string {
	if c.ProcessStart != nil {
		return c.ProcessStart(pid)
	}
	return processStart(pid)
}

// ProcessStart is processStart for the server's supervisor handover, which tells the supervisor it asked to exit from a later process
// that reuses its pid.
func ProcessStart(pid int) string { return processStart(pid) }

// processStart is the system's own record of when a process started, for telling a process from a later one that reuses its pid.
func processStart(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// groupRSS is the resident memory of a process group, in bytes, or 0 where it cannot be read.
func groupRSS(pgid int) int64 {
	if pgid <= 0 {
		return 0
	}
	out, err := exec.Command("ps", "-o", "rss=", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		return 0
	}
	var total int64
	for _, field := range strings.Fields(string(out)) {
		if n, err := strconv.ParseInt(field, 10, 64); err == nil {
			total += n * 1024
		}
	}
	return total
}

// SweepWarmOrphans ends the warm processes an earlier supervisor started and did not end (it died, or was killed): each one the journal
// noted that is still running with the same start time is killed with its group. It runs once when the supervisor starts.
func (c *Client) SweepWarmOrphans(ctx context.Context) {
	if c.Journal == nil {
		return
	}
	processes, err := c.Journal.WarmProcesses(ctx)
	if err != nil {
		c.report("the journal could not list the warm processes", err)
		return
	}
	for _, process := range processes {
		if started := processStart(process.PID); started != "" && started == process.Started {
			_ = syscall.Kill(-process.PID, syscall.SIGKILL)
		}
		_ = c.Journal.ForgetWarmProcess(ctx, process.PID)
	}
}

// applyWarmIdle takes the instance's warm window from a delivery (nil: the owner has not saved one, and the supervisor's own stays: AIMEBU_WARM_IDLE,
// else DefaultWarmIdle). Turning it
// off ends every idle process at once.
func (c *Client) applyWarmIdle(seconds *int) {
	if seconds == nil || *seconds < 0 {
		return
	}
	idle := time.Duration(*seconds) * time.Second
	c.mu.Lock()
	changed := c.Warm.Idle != idle
	c.Warm.Idle = idle
	c.mu.Unlock()
	if changed {
		c.warmChanged() // the server shows what the window is (a card's warm or sleeping), so it hears when it changes
		c.reportLauncherStatus()
	}
	if changed && idle <= 0 {
		c.RetireAllWarm("warm mode was turned off")
	}
}

// WarmReport is the supervisor's warm picture for the server: the agents with a live process (idle until when, or busy) and the figures.
func (c *Client) WarmReport() protocol.WarmReport {
	report := protocol.WarmReport{Agents: []protocol.WarmAgent{}}
	// The number comes first: what is counted below is at least as new as it, so a count can never be older than its number (the server ignores a report
	// whose number is older than one it holds, and a ping that begins after this line carries a later one).
	report.Seq = c.warmSeq.Add(1)
	idleSeconds := int(c.warmCfg().Idle / time.Second)
	report.IdleSeconds = &idleSeconds // the window in force, whoever set it: a card says warm or sleeping by it
	counted := func() protocol.WarmReport {
		report.UnconfirmedPingEnds = int(c.kaUnconfirmed.Load())
		report.Processes = int(c.warmLive.Load())
		report.PingsActive = c.keepAlivePingsActive()
		return report
	}
	c.mu.Lock()
	pool := c.warmPool
	for _, refusal := range c.warmRefusals {
		report.Refusals = append(report.Refusals, refusal)
	}
	c.mu.Unlock()
	sort.Slice(report.Refusals, func(i, j int) bool { return report.Refusals[i].AgentID < report.Refusals[j].AgentID })
	if pool == nil {
		return counted() // pings run without a pool (warm mode off): they still count
	}
	cfg := c.warmCfg()
	for id, slot := range pool.existing() {
		slot.mu.Lock()
		switch {
		case slot.busy != nil:
			report.Agents = append(report.Agents, protocol.WarmAgent{AgentID: id, State: "busy"})
		case slot.idle != nil:
			report.Agents = append(report.Agents, protocol.WarmAgent{AgentID: id, State: "idle", SleepsAt: slot.idle.idleAt.Add(cfg.Idle).UTC().Format(time.RFC3339)})
		}
		slot.mu.Unlock()
	}
	sort.Slice(report.Agents, func(i, j int) bool { return report.Agents[i].AgentID < report.Agents[j].AgentID })
	state := c.WarmState()
	report.UnconfirmedPingEnds = state.UnconfirmedPingEnds
	report.Started, report.Reused, report.RSSBytes, report.ContainerWideBytes = state.Started, state.Reused, state.RSSBytes, state.ContainerWideBytes
	return counted()
}

// warmChanged tells the server the warm picture changed (a process went idle, was taken, ended). The report is built and sent off the caller's
// path, one at a time and never stale: a burst of changes sends the last picture, not each.
func (c *Client) warmChanged() {
	if c.warmReports == nil {
		return
	}
	select {
	case c.warmReports <- struct{}{}:
	default:
	}
}

// sendWarmReports sends a warm_state frame whenever the picture changed, for as long as the connection lives.
func (c *Client) sendWarmReports(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.warmReports:
			if err := c.sendWarmReport(ctx); err != nil && ctx.Err() == nil {
				c.unsent(err)
			}
		}
	}
}

// sendWarmReport numbers, builds and sends one report under a single lock so
// an older asynchronous report cannot arrive after a turn's settled report.
func (c *Client) sendWarmReport(ctx context.Context) error {
	c.warmReportMu.Lock()
	defer c.warmReportMu.Unlock()
	report := c.WarmReport()
	return c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.WarmStateFrame, Warm: &report})
}

// keepAliveForgetAll ends every agent's keep-alive (the supervisor is stopping).
func (c *Client) keepAliveForgetAll() {
	c.kaMu.Lock()
	ids := make([]string, 0, len(c.ka))
	for id := range c.ka {
		ids = append(ids, id)
	}
	c.kaMu.Unlock()
	for _, id := range ids {
		c.keepAliveForget(id)
	}
}
