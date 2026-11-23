package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

// Cache keep-alive. A harness's prompt cache is what makes a turn after a long pause cheap (Claude Code writes it with a one-hour lifetime, so a ping
// every fifty minutes keeps it); when the cache expires the next turn pays to write the whole context again. For an agent that is active and idle the
// supervisor therefore sends the harness a tiny turn shortly before the cache would expire, with exactly the arguments of the agent's last real turn (a
// different system prompt, schema or tool list is a different cached prefix, and the ping would write a new one instead of reading the old), and a user
// turn that only asks it to reply with nothing.
//
// A ping is not a turn: no turn exists for it on the server, so nothing is posted, no recovery item can come of it, no read marker moves and the agent
// card does not change. The supervisor throws the model's reply away and sends the server one report of what it read from and wrote to the cache. A real
// turn resets the timer; a delivery that arrives while a ping runs waits for it (or ends it, for a process on this machine). Pings stop after the
// owner's limit of hours without real work, when the agent stops, and after three failures in a row.

// keepAliveQuarantine is how long an agent stays blocked, and its toolbox refused, after a ping in a container whose end could not be confirmed: the
// process in the container may still be running, and nothing can be asked of it. It is a little under the server's own limit on a scope.
//
// Why a timeout is accepted here instead of a confirmation: the ping's process is a tiny turn (it is told to answer with nothing, and its toolbox is
// refused for as long as the scope holds), and the harness container's own watchdog ends a process whose wrapper has gone, an orphan, within about 15 s.
// 150 s is ten times that, so by then the orphan is gone in every case the watchdog works, and a real turn can start. The case where it does not work
// is not silent: the expiry logs a warning and counts in "unconfirmed ping ends" (see WarmState), which Health shows.
const keepAliveQuarantine = 150 * time.Second

// keepAliveWaitTerminate is how long a delivery waits for a containerized ping that was told to terminate: the launcher's own cleanup waits up to
// five seconds for the session in the container, and the supervisor allows a little more.
const keepAliveWaitTerminate = 12 * time.Second

// quarantine and waitTerminate are the two durations above, or what a test set on the client (an injected field, never a changed global: the goroutines
// of a ping read them while a test ends).
func (c *Client) quarantine() time.Duration {
	if c.kaQuarantine > 0 {
		return c.kaQuarantine
	}
	return keepAliveQuarantine
}

func (c *Client) waitTerminate() time.Duration {
	if c.kaWaitTerminate > 0 {
		return c.kaWaitTerminate
	}
	return keepAliveWaitTerminate
}

const (
	keepAliveRetryBusy      = time.Minute
	keepAliveMaxFails       = 3
	keepAliveWaitNative     = 20 * time.Second
	keepAliveTerminateGrace = 8 * time.Second
	// keepAliveForgetWait is how long a stop or a spawn waits for the ping it ends: it runs on the loop that reads the server's frames.
	keepAliveForgetWait = 300 * time.Millisecond
	keepAliveTimeout    = 90 * time.Second
)

type keepAliveEntry struct {
	agentID   string
	spec      adapter.TurnSpec // the agent's last real turn, made ready to be pinged
	confirmed bool             // the harness reported the native session, or the turn resumed one
	settings  protocol.KeepAliveSettings
	cred      credentials // how the last real turn's login was told
	shares    bool        // whether the launcher shares the host's Claude home (to tell the login again)
	lastReal  time.Time
	timer     *time.Timer
	gen       int
	fails     int
	cancel    context.CancelFunc // set while a ping runs
	done      chan struct{}      // closed when the ping that runs has ended
	docker    bool
	held      bool    // the ping that runs was ended for an account switch: that is not a failure of the ping
	lastCost  float64 // the session's cumulative cost as last seen, to tell what a ping cost
}

// keepAlivePause is what the server says to hold pings back for.
type keepAlivePause struct {
	tools  map[string]bool
	agents map[string]bool
}

// applyKeepAlivePaused takes the server's keep_alive_paused frame, which replaces what it said before.
func (c *Client) applyKeepAlivePaused(tools, agents []string) {
	pause := keepAlivePause{tools: map[string]bool{}, agents: map[string]bool{}}
	for _, tool := range tools {
		pause.tools[tool] = true
	}
	for _, id := range agents {
		pause.agents[id] = true
	}
	c.kaMu.Lock()
	c.kaPause = pause
	c.kaMu.Unlock()
}

// keepAliveCapable says the adapter that runs the spec can run a keep-alive turn.
func (c *Client) keepAliveCapable(spec adapter.TurnSpec) bool {
	if multi, ok := c.Adapter.(interface{ CanKeepAlive(adapter.TurnSpec) bool }); ok {
		return multi.CanKeepAlive(spec)
	}
	keeper, ok := c.Adapter.(adapter.KeepAliver)
	return ok && keeper.KeepAliveTool() == spec.Agent.Tool
}

// keepAliveRemember notes a real turn that is about to run: its spec (made ready to be pinged) and the instance's setting for the agent's tool. A turn
// that cannot be pinged after (not a normal turn, a fork, an adapter that cannot) or a server without the setting forgets the agent.
func (c *Client) keepAliveRemember(spec adapter.TurnSpec, settings *protocol.KeepAliveSettings, cred credentials, sharesHome bool) {
	if settings == nil || settings.IntervalSeconds <= 0 || spec.Mode != adapter.ModeNormal || spec.ForkSession || !c.keepAliveCapable(spec) ||
		((spec.LauncherContainerized || spec.Agent.Variant == "docker") && !cred.containerOK) {
		// (A ping in a container whose launcher cannot confirm its end could never be known to have ended, so such an agent is not pinged.)
		c.keepAliveForget(spec.Agent.ID)
		return
	}
	ping := spec
	ping.NotesJSON = []byte("[]")
	ping.NativeSessionStarted = true // the real turn creates the session; the ping continues it
	ping.KeepAlive = true
	ping.StderrSink = nil
	ping.ConversationSink = nil
	ping.AgentTools = nil // the key is the agent's current one at ping time
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if c.ka == nil {
		c.ka = map[string]*keepAliveEntry{}
	}
	entry := c.ka[spec.Agent.ID]
	if entry == nil {
		entry = &keepAliveEntry{agentID: spec.Agent.ID}
		c.ka[spec.Agent.ID] = entry
	}
	entry.spec, entry.settings, entry.cred, entry.shares = ping, *settings, cred, sharesHome
	entry.confirmed = spec.NativeSessionStarted && spec.NativeSessionHandle != ""
	entry.docker = spec.LauncherContainerized || spec.Agent.Variant == "docker"
	entry.lastReal = c.pool().now()
	entry.fails = 0
	if entry.timer != nil { // a real turn is starting: the pending ping is not needed
		entry.timer.Stop()
		entry.timer = nil
	}
}

// keepAliveTurnEnded arms the timer when a real turn has ended cleanly: the ping comes one interval after the last thing the agent did.
func (c *Client) keepAliveTurnEnded(agentID string, exit adapter.ExitInfo) {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	entry := c.ka[agentID]
	if entry == nil {
		return
	}
	if !exit.TerminalEvent || exit.ExitCode != 0 || exit.CancelIntent {
		c.keepAliveDisarmLocked(entry)
		return
	}
	// A first turn may fail before reporting a native session. There is then
	// nothing to resume, so leave the timer unarmed without reporting a failure.
	if !entry.confirmed || entry.spec.NativeSessionHandle == "" {
		c.keepAliveDisarmLocked(entry)
		return
	}
	entry.lastReal = c.pool().now()
	c.keepAliveArmLocked(entry)
}

func (c *Client) keepAliveNativeSessionStarted(agentID, handle string) {
	if handle == "" {
		return
	}
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if entry := c.ka[agentID]; entry != nil {
		entry.spec.NativeSessionHandle = handle
		entry.confirmed = true
	}
}

func (c *Client) keepAliveDisarmLocked(entry *keepAliveEntry) {
	entry.gen++
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
}

func (c *Client) keepAliveArmLocked(entry *keepAliveEntry) {
	c.keepAliveDisarmLocked(entry)
	interval := time.Duration(entry.settings.IntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	gen := entry.gen
	entry.timer = c.pool().after(interval, func() { c.keepAlivePing(entry.agentID, gen) })
}

// keepAliveForget drops an agent's keep-alive for good (it stopped, was spawned again, lost its key, or the setting is off) and ends a ping that runs.
// It says whether no ping of the agent can still be running: the end of a ping is only waited for a moment here (this runs on the loop that reads the
// server's frames), and a ping that is not confirmed gone is remembered, so a caller that needs it gone (a stop's acknowledgement, the key's drop)
// waits and asks again, and no real turn starts on its session meanwhile.
func (c *Client) keepAliveForget(agentID string) bool {
	c.kaMu.Lock()
	entry := c.ka[agentID]
	if entry == nil {
		c.kaMu.Unlock()
		return c.keepAliveGone(agentID)
	}
	c.keepAliveDisarmLocked(entry)
	delete(c.ka, agentID)
	cancel, done, docker, confirmed := entry.cancel, entry.done, entry.docker, entry.cred.containerOK
	c.kaMu.Unlock()
	return c.keepAliveEndFor(agentID, cancel, done, docker, confirmed, keepAliveForgetWait)
}

// keepAliveYield makes way for a real delivery: a ping that runs for the agent is ended (a process on this machine) or waited for (one in a
// container, which a kill of the wrapper would not end), and the agent's timer is left for the real turn to reset. It says whether the ping is
// confirmed gone; a delivery whose ping is not goes on only once it is (startTurn refuses until then). It waits as long as the end takes, so it
// never runs on the loop that reads the server's frames (see keepAliveBusy).
func (c *Client) keepAliveYield(agentID string) bool {
	c.kaMu.Lock()
	entry := c.ka[agentID]
	if entry == nil {
		c.kaMu.Unlock()
		return c.keepAliveGone(agentID)
	}
	c.keepAliveDisarmLocked(entry)
	cancel, done, docker, confirmed := entry.cancel, entry.done, entry.docker, entry.cred.containerOK
	c.kaMu.Unlock()
	return c.keepAliveEndFor(agentID, cancel, done, docker, confirmed, 0)
}

// keepAlivePingsActive counts the pings that run or were told to end and are not confirmed gone (what a warm_state report says besides its slots).
func (c *Client) keepAlivePingsActive() int {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	count := 0
	seen := map[chan struct{}]bool{}
	for _, entry := range c.ka {
		if entry.done != nil && !closed(entry.done) {
			count++
			seen[entry.done] = true
		}
	}
	for _, stuck := range c.kaStuck {
		if !closed(stuck) && !seen[stuck] {
			count++
		}
	}
	return count
}

// keepAliveBusy says a ping of the agent runs or is not confirmed gone: what a delivery for it has to wait for, in a goroutine of its own.
func (c *Client) keepAliveBusy(agentID string) bool {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if entry := c.ka[agentID]; entry != nil && entry.done != nil && !closed(entry.done) {
		return true
	}
	return c.kaStuck[agentID] != nil && !closed(c.kaStuck[agentID])
}

// keepAliveWaitQuarantine waits for the ping that is not confirmed gone to be over (its quarantine ran out and its scope was lifted), for a delivery
// that needs the agent's session: it runs in the delivery's own goroutine (a delivery for an agent whose ping is not confirmed gone is handled there, so
// the loop that reads the server's frames is never held), and ends with the context. It says whether the ping is gone.
func (c *Client) keepAliveWaitQuarantine(ctx context.Context, agentID string) bool {
	c.kaMu.Lock()
	stuck := c.kaStuck[agentID]
	c.kaMu.Unlock()
	if stuck != nil {
		// The quarantine, the lift (its ack takes up to keepAliveScopeWait), and a little to spare.
		limit := time.NewTimer(c.quarantine() + 2*keepAliveScopeWait + 5*time.Second)
		defer limit.Stop()
		select {
		case <-stuck:
		case <-ctx.Done():
		case <-limit.C:
		}
	}
	return c.keepAliveGone(agentID)
}

// keepAliveGone says no ping of the agent is left that was not confirmed gone (one that was waited for and is still running is the case).
func (c *Client) keepAliveGone(agentID string) bool {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if stuck := c.kaStuck[agentID]; stuck != nil && !closed(stuck) {
		return false
	}
	delete(c.kaStuck, agentID)
	return true
}

func closed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// keepAliveEndFor ends a ping (see keepAliveEnd) and notes one that is not confirmed gone, so nothing starts on its session until it is, and
// forgets one that is.
func (c *Client) keepAliveEndFor(agentID string, cancel context.CancelFunc, done chan struct{}, docker, confirmed bool, waitCap time.Duration) bool {
	ended := c.keepAliveEnd(cancel, done, docker, confirmed, waitCap)
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if ended || done == nil {
		if stuck := c.kaStuck[agentID]; stuck == done {
			delete(c.kaStuck, agentID)
		}
		return c.kaStuck[agentID] == nil || closed(c.kaStuck[agentID])
	}
	if c.kaStuck == nil {
		c.kaStuck = map[string]chan struct{}{}
	}
	c.kaStuck[agentID] = done
	return false
}

// keepAliveEnd makes a ping that runs end for a delivery (or a stop). A ping never delays a real message: a process on this machine is killed; one in a
// container whose launcher confirms its end (`stop_on_signal`) is told to terminate and given the wrapper's own cleanup time (a container whose launcher
// cannot confirm one is not pinged at all). It says whether the ping is confirmed ended: running out of the time is not that, the ping may still be
// alive, and a ping whose process in the container is not confirmed gone keeps its agent blocked for the quarantine. waitCap, when it is not zero,
// shortens the wait.
func (c *Client) keepAliveEnd(cancel context.CancelFunc, done chan struct{}, docker, confirmed bool, waitCap time.Duration) bool {
	if done == nil {
		return true
	}
	wait := keepAliveWaitNative
	switch {
	case !docker:
		if cancel != nil {
			cancel()
		}
	case confirmed:
		wait = c.waitTerminate()
		if cancel != nil {
			cancel()
		}
	default:
		return false // a container whose launcher cannot confirm an end is never pinged, so this is not reached; nothing could confirm it
	}
	if waitCap > 0 && waitCap < wait {
		wait = waitCap
	}
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

// keepAliveHoldTool holds back every ping of a tool and ends the ones that run, for an account switch: a ping is a turn on the account's login, but the
// server's check for a running turn does not see it, so the switch must end it itself. Pending ping timers are not stopped: they find the hold when they
// fire and look again later, and a ping that is ended here arms its timer again as it ends; the hold lasts until the server's next keep_alive_paused
// frame (which it keeps sending the tool in for as long as the switch is in progress, and replaces afterwards). It returns false when a ping is not
// confirmed ended.
func (c *Client) keepAliveHoldTool(tool string) bool {
	type running struct {
		agentID      string
		cancel       context.CancelFunc
		done         chan struct{}
		docker, sure bool
	}
	c.kaMu.Lock()
	if c.kaPause.tools == nil {
		c.kaPause.tools = map[string]bool{}
	}
	c.kaPause.tools[tool] = true
	var pings []running
	for id, entry := range c.ka {
		if entry.spec.Agent.Tool != tool || entry.done == nil {
			continue
		}
		entry.held = true // it re-arms its own timer when it ends, and that timer finds the hold and looks again later
		pings = append(pings, running{id, entry.cancel, entry.done, entry.docker, entry.cred.containerOK})
	}
	// A ping that an earlier stop or yield could not confirm gone (its entry is dropped, it is only remembered) may be on this tool's login too:
	// whatever tool it belongs to, the switch does not go on until it is.
	ok := true
	for _, stuck := range c.kaStuck {
		if !closed(stuck) {
			ok = false
		}
	}
	c.kaMu.Unlock()
	for _, ping := range pings {
		if !c.keepAliveEndFor(ping.agentID, ping.cancel, ping.done, ping.docker, ping.sure, 0) {
			ok = false
		}
	}
	return ok
}

// keepAlivePing runs one ping if it is still wanted. gen says which arming the timer belongs to.
func (c *Client) keepAlivePing(agentID string, gen int) {
	c.kaMu.Lock()
	entry := c.ka[agentID]
	if entry == nil || entry.gen != gen || entry.cancel != nil {
		c.kaMu.Unlock()
		return
	}
	if !entry.confirmed || entry.spec.NativeSessionHandle == "" {
		c.kaMu.Unlock()
		return
	}
	now := c.pool().now()
	if stop := time.Duration(entry.settings.StopAfterSeconds) * time.Second; stop > 0 && now.Sub(entry.lastReal) >= stop {
		delete(c.ka, agentID) // no real work for the owner's limit of hours: the cache is let go
		c.kaMu.Unlock()
		return
	}
	c.mu.Lock()
	wanted := c.desired[agentID].Desired == "active" && !c.stopping
	busy := c.active[agentID] != ""
	c.mu.Unlock()
	if !wanted {
		c.kaMu.Unlock()
		return
	}
	if c.kaPause.tools[entry.spec.Agent.Tool] || c.kaPause.agents[agentID] {
		// The account is near its limit, or the agent is at its provider limit: a ping would only meet it. Look again soon; the pause lifts on its own.
		entry.timer = c.pool().after(keepAliveRetryBusy, func() { c.keepAlivePing(agentID, gen) })
		c.kaMu.Unlock()
		return
	}
	if busy { // a turn is running: it will reset the timer; look again soon in case it ends some other way
		entry.timer = c.pool().after(keepAliveRetryBusy, func() { c.keepAlivePing(agentID, gen) })
		c.kaMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), keepAliveTimeout)
	entry.cancel, entry.done = cancel, make(chan struct{})
	spec, cred, shares := entry.spec, entry.cred, entry.shares
	done := entry.done
	c.kaMu.Unlock()
	c.warmChanged() // the picture now has a ping in it (what the server needs to know if the connection drops)
	go func() {
		defer telemetry.Recover(context.Background())
		defer c.warmChanged() // runs after close(done): the picture without it
		defer close(done)
		defer cancel()
		result := c.runKeepAlive(ctx, spec, cred, shares)
		report := result.report
		if result.scoped {
			if !result.confirmed {
				// The process may still be alive in its container (a kill of the wrapper confirms nothing): the ping stays blocked, and the server
				// keeps refusing the toolbox, until the quarantine is over. Neither is lifted on an unconfirmed end.
				log.Printf("keep-alive %s: the ping's end is not confirmed; the agent stays blocked for %v", spec.Agent.ID, c.quarantine())
				telemetry.ReportWarning(ctx, "supervisor", errors.New("keep-alive: the ping's end is not confirmed; the agent stays blocked"))
				c.keepAliveHoldScope(spec.Agent.ID, result.gen, c.quarantine())
				c.kaUnconfirmed.Add(1)
				log.Printf("WARNING keep-alive %s: the quarantine is over and the ping's end was never confirmed; going on as if the orphaned process is gone (%d unconfirmed ping ends so far)", spec.Agent.ID, c.kaUnconfirmed.Load())
				telemetry.ReportWarning(ctx, "supervisor", errors.New("keep-alive: the quarantine is over and the ping's end was never confirmed"))
				c.warmChanged()
			}
			// Lifted only after the process is confirmed gone (or the quarantine is over), and before the ping counts as ended: a real turn that
			// follows is never refused for it. A lift that is not answered lapses on the server by itself.
			liftCtx, liftCancel := context.WithTimeout(context.Background(), keepAliveScopeWait+time.Second)
			if err := c.keepAliveScopeCall(liftCtx, spec.Agent.ID, result.gen, false); err != nil {
				// Owed: re-sent when the supervisor reconnects and before the agent's next real turn starts, so that turn's toolbox is not refused.
				log.Printf("keep-alive %s: the scope was not lifted (it is owed, and re-sent on reconnect and before the next turn): %v", spec.Agent.ID, err)
				telemetry.ReportError(ctx, "supervisor", fmt.Errorf("keep-alive: the scope was not lifted: %w", err))
				c.keepAliveOweLift(spec.Agent.ID, result.gen)
			}
			liftCancel()
		}
		c.kaMu.Lock()
		current := c.ka[agentID]
		if current != nil && current.done == done {
			current.cancel = nil
			if report.OK {
				current.fails = 0
			} else if !current.held {
				current.fails++
			}
			current.held = false
			if current.fails < keepAliveMaxFails && current.gen == gen {
				c.keepAliveArmLocked(current)
			}
		}
		c.kaMu.Unlock()
	}()
}

// pingResult is what runKeepAlive did: the report, whether the ping's process is confirmed gone, and whether the server was told to refuse the
// agent's toolbox for it (so the caller lifts that, once the end is confirmed).
type pingResult struct {
	report    protocol.KeepAliveReport
	confirmed bool
	scoped    bool
	gen       string // the ping's turn id, which names it in its scope frames
}

// runKeepAlive runs the ping and returns what it did. Its reply is dropped. A process in a container counts as ended only by the rules of a warm
// process: it ended on its own with exit 0, or it was told to terminate (the launcher advertises `stop_on_signal`) and its wrapper exited with exactly
// 128 plus the signal (143), or a warm process was closed through its launcher. Anything else (the wrapper killed, an exit the launcher did not
// make) leaves the process in the container possibly running, and is not confirmed.
func (c *Client) runKeepAlive(ctx context.Context, spec adapter.TurnSpec, cred credentials, shares bool) (result pingResult) {
	report := protocol.KeepAliveReport{AgentID: spec.Agent.ID, At: c.pool().now().UTC().Format(time.RFC3339)}
	result.confirmed = true // until a process exists that is not confirmed gone
	started := time.Now()
	defer func() {
		report.DurationSeconds = time.Since(started).Seconds()
		result.report = report
		c.sendKeepAliveReport(report)
	}()
	access, err := c.agentToolsAccess(spec.Agent.ID)
	if err != nil || access == nil {
		report.Error = "no key for the agent"
		return result
	}
	spec.AgentTools = access
	// The server refuses the agent's toolbox for as long as the ping runs, and it is told so before the process exists. A lift an earlier ping still
	// owes is settled first (the server would also ignore it as stale, it names the earlier ping).
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	gen := "ka_" + hex.EncodeToString(nonce[:])
	result.gen = gen
	_ = c.keepAliveSettleOwed(ctx, spec.Agent.ID, true)
	if err := c.keepAliveScopeCall(ctx, spec.Agent.ID, gen, true); err != nil {
		// The server may have applied the scope and its ack been lost or late: it is lifted later (the next real turn settles it first), or it
		// would refuse that turn's toolbox for as long as its lease. A lift for a scope the server never saw is harmless (it names this ping).
		c.keepAliveOweLift(spec.Agent.ID, gen)
		report.Error = adapter.SanitizeStderr("the ping could not be scoped: " + err.Error())
		return result
	}
	result.scoped = true
	spec.TurnID = gen
	cred.stamp, cred.ok = warmCredentials(spec, shares, c.warmCodexAuthPath(spec))
	// The process is started under a context that a cancel does not end (a kill of the wrapper of a container process confirms nothing): the
	// watcher below ends it the right way.
	turn, err := c.startTurn(context.WithoutCancel(ctx), spec, nil, cred)
	if err != nil {
		report.Error = adapter.SanitizeStderr(err.Error())
		return result
	}
	_, report.ViaWarm = turn.(*warmTurn)
	containerized := spec.LauncherContainerized || spec.Agent.Variant == "docker"
	var signalled, signalConfirmed atomic.Bool
	stop := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer telemetry.Recover(context.Background())
		defer close(watching)
		select {
		case <-ctx.Done():
			// A process in a container is told to terminate (its launcher cleans up and confirms), anywhere else it is killed.
			if terminator, ok := turn.(interface{ Terminate(time.Duration) bool }); ok && cred.containerOK && containerized {
				signalConfirmed.Store(terminator.Terminate(keepAliveTerminateGrace))
			} else {
				_ = turn.Cancel()
			}
			signalled.Store(true)
		case <-stop:
		}
	}()
	var stopOnce sync.Once
	endWatch := func() { stopOnce.Do(func() { close(stop) }); <-watching } // the watcher is done, and has said what it did, once this returns
	defer endWatch()
	direct := 0.0
	for event := range turn.Events() {
		if event.Kind == adapter.EventCompaction {
			// A compaction inside a ping is not recorded the way a real turn's is (a ping has no turn row), but its fact goes to
			// the server: the session's context-delivery state must be invalidated, or the next wake would send an unchanged
			// marker for text the compaction summarised away.
			report.Compacted = true
			continue
		}
		if event.Kind != adapter.EventUsage || event.Usage == nil || event.Usage.Partial {
			continue
		}
		u := event.Usage
		if u.TokensIn != nil {
			report.TokensIn = *u.TokensIn
		}
		if u.TokensOut != nil {
			report.TokensOut = *u.TokensOut
		}
		if u.TokensCacheRead != nil {
			report.CacheRead = *u.TokensCacheRead
		}
		if u.TokensCacheWrite != nil {
			report.CacheWrite = *u.TokensCacheWrite
		}
		if u.TokensCumulative {
			report.TokensAreTotal = true
		}
		if u.CostEstimate != nil {
			if u.CostCumulative { // Claude's figure is the session's running total; Codex's (from the price list) is this turn's own
				report.CostCumulative = *u.CostEstimate
				report.CostIsTotal = true
			} else {
				direct = *u.CostEstimate
			}
		}
	}
	exit, _ := turn.Wait()
	c.warmTurnEnded(turn, exit)
	report.Cost = direct
	if direct == 0 {
		report.Cost = c.keepAliveCost(spec.Agent.ID, report.CostCumulative)
	}
	report.OK = exit.TerminalEvent && exit.ExitCode == 0 && !exit.CancelIntent
	if !report.OK {
		report.Error = adapter.SanitizeStderr(firstLine(exit.ErrorSummary, exit.ErrorEvent, exit.StderrTail))
		if report.Error == "" {
			report.Error = fmt.Sprintf("the ping ended with status %d", exit.ExitCode)
		}
	}
	if containerized {
		endWatch() // it may be in the middle of a terminate whose answer is the confirmation
		switch {
		case exit.TerminalEvent && exit.ExitCode == 0 && !exit.CancelIntent:
			result.confirmed = true // it finished by itself, whatever else was done at the same moment
		case !signalled.Load():
			// It ended on its own: `docker exec` passes the inner process's exit status on, so a status of 1..124 (an API error, a failed turn) is the
			// process's own end and it is gone. 125 and above are the docker CLI's own failures (125-127) or a signal's 128+n (a killed wrapper or
			// process, which says nothing about what runs in the container): those are not confirmed.
			result.confirmed = exit.ExitCode >= 0 && exit.ExitCode <= 124
		case report.ViaWarm:
			result.confirmed = signalConfirmed.Load() // a warm process closed through its launcher says whether it is gone
		default:
			result.confirmed = signalConfirmed.Load() && exit.ExitCode == 128+int(syscall.SIGTERM)
		}
	}
	return result
}

func firstLine(parts ...string) string {
	for _, part := range parts {
		for i, r := range part {
			if r == '\n' {
				part = part[:i]
				break
			}
		}
		if part != "" {
			if len(part) > 200 {
				part = truncateKeepAliveError(part)
			}
			return part
		}
	}
	return ""
}

// truncateKeepAliveError cuts one over-long line to 200 bytes (never in the middle of a character). A line that ends on the server's keep-alive
// refusal keeps that refusal whole, at its end, and gives up its own beginning instead: the refusal is recognised only as the end of the error, so
// a cut that dropped it would turn an intended refusal into a Sentry report.
func truncateKeepAliveError(part string) string {
	const limit = 200
	cut := func(text string, n int) string {
		for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
			n--
		}
		return text[:n]
	}
	if keepAliveScopeRefusal(part) {
		body := keepAliveScopeRefusalBody
		if end := strings.LastIndex(part, body); end >= 0 && len(body) < limit {
			head := cut(part[:end], limit-len(body)-1)
			return strings.TrimRight(head, " ") + " " + body
		}
	}
	return cut(part, limit)
}

// keepAliveScopeRefusalBody is the server's whole refusal as the toolbox's REST route writes it (refuseKeepAliveScope: the stable code, then the
// message). The server test TestKeepAliveScopeRefusalBodyIsStable pins it, so a change there cannot silently make this recognise nothing.
const keepAliveScopeRefusalBody = `{"error":"keep_alive_scope","message":"this is a cache keep-alive, not work: the toolbox is not available"}`

// keepAliveScopeRefusal says a failed ping's error ends on the server's own refusal of the toolbox during a ping (403 keep_alive_scope). That refusal is
// intended and the ping is not broken by a bug of ours, so it is logged and counted as a failure but not reported to Sentry.
//
// It is recognised by the server's structured error body (its stable code and message, as one JSON object), and only as the end of the harness's
// output: what the harness prints after a refusal it could not get past is the failure. A real failure that merely quotes an earlier refusal (a retry
// that then timed out, a crash after one) has more words after the body and is reported. The code is the server's, so no wording of the harness
// (its own prefix, its own quoting) decides it; the harness only transports the body, so its end is the one place the body can be matched.
func keepAliveScopeRefusal(message string) bool {
	// Only closing punctuation may follow the body (the harness wraps it in its own parentheses or quotes); any word after it is another failure.
	return strings.HasSuffix(strings.TrimRight(message, " \t\r\n)]'`.,;:"), keepAliveScopeRefusalBody)
}

// sendKeepAliveReport logs the ping and tells the server what it did. A ping that read little from the cache and wrote much missed: it is in the log
// so the interval can be tuned from what actually happened.
func (c *Client) sendKeepAliveReport(report protocol.KeepAliveReport) {
	if report.OK {
		log.Printf("keep-alive %s: cache read %d, written %d, in %d, out %d, %.1fs", report.AgentID, report.CacheRead, report.CacheWrite, report.TokensIn, report.TokensOut, report.DurationSeconds)
	} else {
		log.Printf("keep-alive %s failed: %s", report.AgentID, report.Error)
		if !keepAliveScopeRefusal(report.Error) {
			telemetry.WarnCause(context.Background(), "supervisor", "keep-alive ping failed") // report.Error is the harness's own words: the local log only
		}
	}
	if c.keepAliveSink != nil {
		c.keepAliveSink(report)
	}
	if err := c.sendCurrent(context.Background(), protocol.Frame{V: protocol.Version, Type: protocol.KeepAliveFrame, KeepAlive: &report}); err != nil {
		// Not connected: the report is only in the log. A ping is never a reason to fail anything.
		_ = err
	}
}

// applyAgentSettings takes the instance's settings from an agent_settings frame, for the agents that are idle and so get no delivery to carry them: the
// warm window (off ends every idle process, as a delivery's does), and each tool's keep-alive. An agent whose tool's interval is now zero is forgotten
// (a ping that runs is ended); the others keep their entry with the new settings and their timer is armed again from their last real turn, so a
// shorter interval takes effect at once and a longer one is not cut short by the old timer. It ends processes, so it must not run on the loop that
// reads frames.
func (c *Client) applyAgentSettings(warmIdle *int, codexWarmEnabled bool, byTool map[string]protocol.KeepAliveSettings) {
	c.applyWarmIdle(warmIdle)
	c.setCodexWarmEnabled(codexWarmEnabled)
	if byTool == nil {
		return
	}
	var forget []string
	c.kaMu.Lock()
	for id, entry := range c.ka {
		settings, known := byTool[entry.spec.Agent.Tool]
		if !known {
			continue
		}
		if settings.IntervalSeconds <= 0 {
			forget = append(forget, id)
			continue
		}
		changed := entry.settings != settings
		entry.settings = settings
		if changed && entry.timer != nil { // armed, so waiting between pings: count the new interval from the last real turn
			c.keepAliveArmFromLastLocked(entry)
		}
	}
	c.kaMu.Unlock()
	for _, id := range forget {
		c.keepAliveForget(id)
	}
}

// keepAliveArmFromLastLocked arms the entry's timer for one interval after its last real turn, or for a short moment from now when that is past.
func (c *Client) keepAliveArmFromLastLocked(entry *keepAliveEntry) {
	c.keepAliveDisarmLocked(entry)
	interval := time.Duration(entry.settings.IntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	wait := interval - c.pool().now().Sub(entry.lastReal)
	if wait < time.Second {
		wait = time.Second
	}
	gen := entry.gen
	entry.timer = c.pool().after(wait, func() { c.keepAlivePing(entry.agentID, gen) })
}

// keepAliveScopeWait is how long the server's answer to a scope request is waited for.
var keepAliveScopeWait = 5 * time.Second

// keepAliveScopeCall tells the server a ping of the agent begins or has ended, and waits for its answer. While a ping runs the server refuses every
// toolbox call made with the agent's key, so the ping cannot act even if the model ignores the words that ask it not to; the supervisor does not start
// the ping without that answer, and does not count it ended before the server has lifted the scope. An error means the scope is not certain: the
// ping is not run (and a lift that was not answered lapses on the server by itself after its limit).
func (c *Client) keepAliveScopeCall(ctx context.Context, agentID, gen string, on bool) error {
	// One scope transition at a time per agent, held across the frame and its ack: a lift cannot interleave with the begin of a ping that follows it.
	c.kaScopeMu.Lock()
	if c.kaScopeLocks == nil {
		c.kaScopeLocks = map[string]*sync.Mutex{}
	}
	lock := c.kaScopeLocks[agentID]
	if lock == nil {
		lock = &sync.Mutex{}
		c.kaScopeLocks[agentID] = lock
	}
	c.kaScopeMu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	if c.keepAliveScope != nil {
		return c.keepAliveScope(agentID, gen, on)
	}
	frame := protocol.Frame{V: protocol.Version, Type: protocol.KeepAliveScopeFrame, AgentID: agentID, ScopeOn: on, ScopeGen: gen}
	if on {
		// Counted from the one counter, after the ping is in the count (its entry is set before this): a report built before it carries a smaller
		// number, so the server cannot let it overwrite the ping.
		frame.WarmSeq = c.warmSeq.Add(1)
	}
	return c.sendForAck(ctx, frame)
}

// sendForAck sends a frame that the server answers by its ReqID (a scope, a warm claim) and waits for the answer: an error when it cannot be sent, the
// server does not answer within keepAliveScopeWait, or the context ends. The caller sets everything but the ReqID.
func (c *Client) sendForAck(ctx context.Context, frame protocol.Frame) error {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])
	frame.ReqID = id
	acked := make(chan struct{})
	c.kaScopeMu.Lock()
	if c.kaScopePending == nil {
		c.kaScopePending = map[string]chan struct{}{}
	}
	c.kaScopePending[id] = acked
	c.kaScopeMu.Unlock()
	defer func() { c.kaScopeMu.Lock(); delete(c.kaScopePending, id); c.kaScopeMu.Unlock() }()
	if err := c.sendCurrent(ctx, frame); err != nil {
		return err
	}
	select {
	case <-acked:
		return nil
	case <-time.After(keepAliveScopeWait):
		return errors.New("the server did not confirm it")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// keepAliveHoldScope waits out the quarantine of an unconfirmed ping and renews its scope on the server meanwhile (the "on" frame again extends the
// lease, protocol.KeepAliveScopeTTL), at once and then every protocol.KeepAliveScopeRenewEvery: the ping's run and the quarantine together are longer
// than one TTL, and an orphan must not get the toolbox back while the supervisor still holds the agent blocked. A renewal that fails (the connection
// dropped) is tried again after a short wait, not a whole period.
func (c *Client) keepAliveHoldScope(agentID, gen string, quarantine time.Duration) {
	every := protocol.KeepAliveScopeRenewEvery
	if c.kaRenewEvery > 0 {
		every = c.kaRenewEvery
	}
	deadline := time.Now().Add(quarantine)
	for {
		wait := every
		ctx, cancel := context.WithTimeout(context.Background(), keepAliveScopeWait+time.Second)
		if err := c.keepAliveScopeCall(ctx, agentID, gen, true); err != nil {
			log.Printf("keep-alive %s: the scope could not be renewed during the quarantine: %v", agentID, err)
			telemetry.ReportError(ctx, "supervisor", fmt.Errorf("keep-alive: the scope could not be renewed during the quarantine: %w", err))
			wait = min(wait, keepAliveScopeWait)
		}
		cancel()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		time.Sleep(min(wait, remaining))
		if time.Until(deadline) <= 0 {
			return
		}
	}
}

// keepAliveOweLift remembers that the scope of the agent's ping was not lifted (the connection dropped or the server did not answer), so it can be lifted
// again later: the server lapses it by itself after its limit, but a real turn must not be refused in the meantime.
func (c *Client) keepAliveOweLift(agentID, gen string) {
	c.kaScopeMu.Lock()
	defer c.kaScopeMu.Unlock()
	if c.kaOwed == nil {
		c.kaOwed = map[string]string{}
	}
	c.kaOwed[agentID] = gen
}

// keepAliveSettleLifts sends the lifts the supervisor owes (for one agent, or for every agent when agentID is empty) and waits for each ack; an owed lift
// that is answered is forgotten. It never touches an agent whose ping runs or is not confirmed gone (its lift is the ping's own, after the end). It returns
// an error for the first lift that could not be confirmed, which stays owed.
func (c *Client) keepAliveSettleLifts(ctx context.Context, agentID string) error {
	return c.keepAliveSettleOwed(ctx, agentID, false)
}

// keepAliveSettleOwed is keepAliveSettleLifts; ownPing says the caller is the ping that is about to begin its own scope for the agent (so the agent
// being "busy" is that ping, not another one whose lift is its own).
func (c *Client) keepAliveSettleOwed(ctx context.Context, agentID string, ownPing bool) error {
	c.kaScopeMu.Lock()
	owed := map[string]string{}
	for id, gen := range c.kaOwed {
		if agentID == "" || id == agentID {
			owed[id] = gen
		}
	}
	c.kaScopeMu.Unlock()
	var first error
	for id, gen := range owed {
		if !ownPing && c.keepAliveBusy(id) {
			continue
		}
		liftCtx, cancel := context.WithTimeout(ctx, keepAliveScopeWait+time.Second)
		// The lift names the ping it is for: if a newer ping's scope began in the meantime (between the check above and the frame), the server
		// ignores this stale one instead of clearing the new ping's guard.
		err := c.keepAliveScopeCall(liftCtx, id, gen, false)
		cancel()
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		c.kaScopeMu.Lock()
		if c.kaOwed[id] == gen {
			delete(c.kaOwed, id)
		}
		c.kaScopeMu.Unlock()
	}
	return first
}

// keepAliveScopeAcked is the server's answer to a scope request.
func (c *Client) keepAliveScopeAcked(id string) {
	c.kaScopeMu.Lock()
	acked := c.kaScopePending[id]
	delete(c.kaScopePending, id)
	c.kaScopeMu.Unlock()
	if acked != nil {
		close(acked)
	}
}

// keepAliveNoteCost notes the session's cumulative cost after a real turn, so that the next ping's cost is the difference.
func (c *Client) keepAliveNoteCost(agentID string, cumulative float64) {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	if entry := c.ka[agentID]; entry != nil && cumulative > 0 {
		entry.lastCost = cumulative
	}
}

// keepAliveCost is what a ping cost: the cumulative cost it reports less the last one seen (0 when either is unknown), and notes the new one.
func (c *Client) keepAliveCost(agentID string, cumulative float64) float64 {
	c.kaMu.Lock()
	defer c.kaMu.Unlock()
	entry := c.ka[agentID]
	if entry == nil || cumulative <= 0 {
		return 0
	}
	cost := 0.0
	if entry.lastCost > 0 && cumulative >= entry.lastCost {
		cost = cumulative - entry.lastCost
	}
	entry.lastCost = cumulative
	return cost
}
