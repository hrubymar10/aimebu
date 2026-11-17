package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/telemetry"
)

// retireWarmTimeout bounds the wait for the host's supervisor to end its idle warm processes of a tool: each gets a few seconds to go on its own,
// and they go side by side.
const retireWarmTimeout = 30 * time.Second

// retireHostWarm asks the host machine's supervisor to end the idle warm processes of a tool and waits until it says they are confirmed gone. An
// account switch changes the login those processes hold, and they are on the machine the login is on (the host), so no other machine is asked.
// Nothing is asked of a host whose supervisor does not keep processes warm (it has none). A host that is not connected is NOT taken to have none: a
// dropped connection or a server restart leaves a supervisor's idle processes running, so unless the server knows the host has nothing warm (it never
// connected, or its last warm_state said 0) the switch is refused until it is back and can say. It returns an error when a process is not confirmed
// gone, or the supervisor does not answer or goes away: the switch is then refused.
//
// Cache keep-alive pings of the tool are held back from the moment this is called until the returned release is called, which the caller does once the
// switch is over (whatever it came to): a ping is a turn on the login being swapped, the check for a running turn does not see it, and the supervisor
// ends the ones that run as part of this request. On an error the hold is already released.
func (s *Server) retireHostWarm(ctx context.Context, tool string) (release func(), err error) {
	release = func() {}
	host, err := s.Store.HostMachine(ctx)
	if err != nil {
		return release, err
	}
	machine, err := s.machineForID(host.ID)
	if err != nil {
		if s.hostMayHaveWarm(ctx, host) {
			return release, errors.New("the host's supervisor is not connected, so whether it keeps agents warm cannot be checked: reconnect it, then try again")
		}
		return release, nil
	}
	if !machine.supportsWarm {
		return release, nil
	}
	s.kaSwitchMu.Lock()
	if s.kaSwitching == nil {
		s.kaSwitching = map[string]int{}
	}
	s.kaSwitching[tool]++
	s.kaSwitchMu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			s.kaSwitchMu.Lock()
			if s.kaSwitching[tool]--; s.kaSwitching[tool] <= 0 {
				delete(s.kaSwitching, tool)
			}
			s.kaSwitchMu.Unlock()
			pushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.sendKeepAlivePause(pushCtx, machine)
		})
	}
	// The hold goes out before the request, on the same connection, so the supervisor applies it before it looks for a ping to end.
	s.sendKeepAlivePause(ctx, machine)
	if err := s.askRetireWarm(ctx, machine, tool); err != nil {
		release()
		return func() {}, err
	}
	return release, nil
}

// warmSeenKeyPrefix names the global setting that keeps a machine's last warm report across a restart of the server.
const warmSeenKeyPrefix = "machine_warm_seen:"

// warmSeenRecord is what a machine's supervisor last said about what it has running: warm slots and cache pings together, and when.
type warmSeenRecord struct {
	Count int    `json:"count"`
	At    string `json:"at"`
}

// hostMayHaveWarm says the host's supervisor may have processes running the server cannot see now (it is not connected). It does not when the host
// never connected, or when the last thing it said about what it has running (what a supervisor that keeps nothing warm says at connect, its last
// warm_state of slots and pings, or the final one a cleanly stopping supervisor sends) was nothing, and no ping of the host is scoped now. The last
// report is kept in the database, so a server that restarted still has it; a host with no report at all is assumed to have some.
func (s *Server) hostMayHaveWarm(ctx context.Context, host store.Machine) bool {
	if !host.LastSeen.Valid || host.LastSeen.String == "" {
		return false
	}
	s.warmSeenMu.Lock()
	seen, known := s.warmSeen[host.ID]
	s.warmSeenMu.Unlock()
	count := seen.count
	if !known {
		raw, ok, err := s.Store.GlobalSettingValue(ctx, warmSeenKeyPrefix+host.ID)
		if err != nil || !ok {
			return true
		}
		var record warmSeenRecord
		if json.Unmarshal([]byte(raw), &record) != nil {
			return true
		}
		count = record.Count
	}
	// A ping that began before the drop is a process on the login too, and its report may not have arrived: the server's scope for it (set before the
	// ping's process exists, held through its quarantine, lifted only after its confirmed end) says it may still be running.
	return count > 0 || s.machineHasLiveScope(host.ID)
}

// machineHasLiveScope says a keep-alive ping of an agent of the machine is scoped now: it runs, or is held in quarantine, or its supervisor went away
// before it lifted the scope (the lease lapses on its own after protocol.KeepAliveScopeTTL).
func (s *Server) machineHasLiveScope(machineID string) bool {
	s.kaScopeMu.Lock()
	defer s.kaScopeMu.Unlock()
	now := time.Now()
	for _, lease := range s.kaScope {
		if lease.machineID == machineID && !now.After(lease.until) {
			return true
		}
	}
	return false
}

// warmSeenEntry is what the server holds of a machine's last count of running processes: the count, the connection and the supervisor's counter value it
// came with (so an older number can never overwrite a newer one).
type warmSeenEntry struct {
	count int
	conn  *machineConn
	seq   uint64
}

// noteWarmSeen records, in memory and (when it changed) in the database, how many processes (warm slots and pings) a machine's supervisor last said it
// has running (0 for one that cannot keep any), for hostMayHaveWarm. It is unordered: a count that comes with a counter value goes through
// applyWarmCount instead.
func (s *Server) noteWarmSeen(machineID string, count int) {
	s.warmSeenMu.Lock()
	defer s.warmSeenMu.Unlock()
	if err := s.setWarmSeenLocked(machineID, warmSeenEntry{count: count}); err != nil {
		log.Printf("machine %s: its last warm report was not saved: %v", machineID, err)
		telemetry.ReportError(context.Background(), "machine", fmt.Errorf("its last warm report was not saved: %w", err))
	}
}

// applyWarmCount applies a count the supervisor gave with its counter value, under the lock that also writes the database, so the saved value is the
// newest one whatever the order the frames are handled in. A count from a connection that is no longer the machine's is ignored; so is one older than
// what this connection already gave (a supervisor that restarts starts its counter again, but it does so on a new connection). It says whether the
// count (or a newer one) is held AND saved: false means the caller must not tell the supervisor it has it (the supervisor then ends the process), and
// the count in memory is not moved ahead of the saved one.
func (s *Server) applyWarmCount(c *machineConn, seq uint64, count int, raiseOnly bool) bool {
	s.warmSeenMu.Lock()
	defer s.warmSeenMu.Unlock()
	if current, err := s.machineForID(c.id); err != nil || current != c {
		return false
	}
	held := s.warmSeen[c.id]
	if held.conn == c && seq != 0 && seq <= held.seq {
		return true // a newer number is already held
	}
	if raiseOnly && held.count > count {
		count = held.count
	}
	if err := s.setWarmSeenLocked(c.id, warmSeenEntry{count: count, conn: c, seq: seq}); err != nil {
		log.Printf("machine %s: its last warm report was not saved: %v", c.id, err)
		telemetry.ReportError(context.Background(), "machine", fmt.Errorf("its last warm report was not saved: %w", err))
		return false
	}
	return true
}

// setWarmSeenLocked saves a machine's count (when it changed, or none is held yet) and only then holds it in memory, so memory is never ahead of what a
// restarted server would read. The caller holds warmSeenMu.
func (s *Server) setWarmSeenLocked(machineID string, entry warmSeenEntry) error {
	if s.warmSeen == nil {
		s.warmSeen = map[string]warmSeenEntry{}
	}
	previous, had := s.warmSeen[machineID]
	if !had || previous.count != entry.count {
		raw, err := json.Marshal(warmSeenRecord{Count: entry.count, At: time.Now().UTC().Format(time.RFC3339)})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		save := s.Store.SetGlobalSettingValue
		if s.saveWarmSeen != nil {
			save = s.saveWarmSeen
		}
		if err := save(ctx, warmSeenKeyPrefix+machineID, string(raw)); err != nil {
			return err
		}
	}
	s.warmSeen[machineID] = entry
	return nil
}

// handleWarmEnded records that a warm process the supervisor had kept idle for an agent has ended, and why: the turns that ended in it were recorded as
// finished with the process alive, so this is where its end is on the record. Only the machine that runs the agent may say it; the reason is bounded and
// plain text.
func (s *Server) handleWarmEnded(ctx context.Context, c *machineConn, frame protocol.Frame) {
	if frame.AgentID == "" {
		return
	}
	agent, err := s.Store.AgentRuntimeByID(ctx, frame.AgentID)
	if err != nil || agent.MachineID != c.id {
		return
	}
	reason := frame.Message
	if len(reason) > 200 {
		reason = reason[:200]
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason})
	err = s.Store.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.AppendEvent(store.Event{Kind: "warm_process_ended", ProjectID: agent.ProjectID, AgentID: agent.ID, PayloadJSON: string(payload)})
		return err
	})
	if err != nil {
		log.Printf("machine %s: a warm process's end was not recorded: %v", c.id, err)
		telemetry.ReportError(context.Background(), "machine", fmt.Errorf("a warm process's end was not recorded: %w", err))
	}
}

// handleWarmClaim applies a warm claim and answers it: the supervisor keeps the process only once it has the answer.
func (s *Server) handleWarmClaim(c *machineConn, frame protocol.Frame) {
	if s.applyWarmCount(c, frame.WarmSeq, frame.WarmCount, false) {
		_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.WarmClaimAckFrame, ReqID: frame.ReqID})
	}
}

// forgetWarmSeen drops what is known of a machine's warm processes (it was revoked: a machine that is gone has none the server needs to wait for).
func (s *Server) forgetWarmSeen(ctx context.Context, machineID string) {
	s.warmSeenMu.Lock()
	delete(s.warmSeen, machineID)
	s.warmSeenMu.Unlock()
	if err := s.Store.DeleteGlobalSettingValue(ctx, warmSeenKeyPrefix+machineID); err != nil {
		log.Printf("machine %s: its last warm report was not removed: %v", machineID, err)
		telemetry.ReportWarning(ctx, "machine", fmt.Errorf("its last warm report was not removed: %w", err))
	}
}

// askRetireWarm sends the retire_warm request to a machine and waits for its answer.
func (s *Server) askRetireWarm(ctx context.Context, machine *machineConn, tool string) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	machine.harnessMu.Lock()
	if machine.pathPending == nil {
		machine.pathPending = make(map[string]chan protocol.Frame)
	}
	machine.pathPending[id] = channel
	machine.harnessMu.Unlock()
	defer func() {
		machine.harnessMu.Lock()
		delete(machine.pathPending, id)
		machine.harnessMu.Unlock()
	}()
	if err := machine.send(protocol.Frame{V: protocol.Version, Type: protocol.RetireWarm, ReqID: id, RetireTool: tool}); err != nil {
		return err
	}
	timer := time.NewTimer(retireWarmTimeout)
	defer timer.Stop()
	select {
	case reply := <-channel:
		if reply.Result != "ok" {
			return fmt.Errorf("agents kept warm on the host could not be ended: %s", reply.Message)
		}
		return nil
	case <-machine.done:
		// It went away before it answered: its processes were not seen ended (a supervisor that loses its connection leaves them running).
		return errors.New("the host's supervisor went away before it confirmed that its warm agents ended")
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("the host's supervisor did not confirm that its warm agents ended")
	}
}

// keepAlivePauseInterval is how often the server looks at what should hold pings back.
const keepAlivePauseInterval = 15 * time.Second

// keepAlivePaused works out what a machine's supervisor should hold pings back for: the tools whose limit guard is holding turns (the guard judges the
// host machine's account, so only the host is told), and the agents on the machine whose provider limit is in force.
func (s *Server) keepAlivePaused(ctx context.Context, c *machineConn) (tools, agents []string, err error) {
	tools, agents = []string{}, []string{}
	if host, hostErr := s.Store.HostMachine(ctx); hostErr == nil && host.ID == c.id {
		for _, tool := range []string{"claude", "codex"} {
			// A tool whose account is being switched has no pings either, for as long as the switch takes.
			if s.Store.ToolHold(tool) != "" || s.kaSwitchingTool(tool) {
				tools = append(tools, tool)
			}
		}
	}
	agents, err = s.Store.ProviderLimitedAgents(ctx, c.id)
	return tools, agents, err
}

// kaSwitchingTool says an account switch of the tool is in progress (retireHostWarm without its release yet).
func (s *Server) kaSwitchingTool(tool string) bool {
	s.kaSwitchMu.Lock()
	defer s.kaSwitchMu.Unlock()
	return s.kaSwitching[tool] > 0
}

// sendKeepAlivePause tells the machine's supervisor what to hold pings back for now, unless it was told exactly that already. The frames of one
// machine are sent one at a time, so a hold sent before a request is applied before it.
func (s *Server) sendKeepAlivePause(ctx context.Context, c *machineConn) {
	c.pauseMu.Lock()
	defer c.pauseMu.Unlock()
	tools, agents, err := s.keepAlivePaused(ctx, c)
	if err != nil {
		return
	}
	key := strings.Join(tools, ",") + "|" + strings.Join(agents, ",")
	if key == c.pauseLast && c.pauseSent {
		return
	}
	if err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.KeepAlivePausedFrame, PausedTools: tools, PausedAgents: agents}); err == nil {
		c.pauseLast, c.pauseSent = key, true
	}
}

// sendAgentSettings tells the machine's supervisor the instance's warm window and keep-alive settings, unless it was told exactly that already. A
// delivery carries them too, but an idle agent gets none: without this a setting that was turned off would only reach it at its next message.
func (s *Server) sendAgentSettings(ctx context.Context, c *machineConn) {
	if !c.supportsWarm {
		return
	}
	warm, err := s.warmIdleToSend(ctx)
	if err != nil {
		return
	}
	keep, err := s.keepAliveConfig(ctx)
	if err != nil {
		return
	}
	frame := protocol.Frame{V: protocol.Version, Type: protocol.AgentSettingsFrame, WarmIdleSeconds: warm, CodexWarmEnabled: s.codexWarmOn.Load(), KeepAliveByTool: map[string]protocol.KeepAliveSettings{}}
	for _, tool := range []string{"claude", "codex"} {
		frame.KeepAliveByTool[tool] = *keep.settingsFor(tool)
	}
	saved := -1
	if warm != nil {
		saved = *warm
	}
	key := fmt.Sprintf("%d|%t|%v", saved, frame.CodexWarmEnabled, frame.KeepAliveByTool)
	c.settingsMu.Lock()
	defer c.settingsMu.Unlock()
	if c.settingsSent && key == c.settingsLast {
		return
	}
	if err := c.send(frame); err == nil {
		c.settingsLast, c.settingsSent = key, true
	}
}

// pushAgentSettings sends every connected machine the settings now, after one was changed.
func (s *Server) pushAgentSettings(ctx context.Context) {
	s.machineMu.Lock()
	machines := make([]*machineConn, 0, len(s.machines))
	for _, c := range s.machines {
		machines = append(machines, c)
	}
	s.machineMu.Unlock()
	for _, c := range machines {
		s.sendAgentSettings(ctx, c)
	}
}

// watchKeepAlivePause tells a warm-capable supervisor what to hold pings back for, at connect and again whenever it changes, until it goes away.
func (s *Server) watchKeepAlivePause(ctx context.Context, c *machineConn) {
	if !c.supportsWarm {
		return
	}
	send := func() { s.sendKeepAlivePause(ctx, c); s.sendAgentSettings(ctx, c) }
	send()
	ticker := time.NewTicker(keepAlivePauseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			send()
		}
	}
}

// recordKeepAliveUsage writes a ping's spend where all spend shows (usage_record), for an agent that runs on the machine that reported it.
func (s *Server) recordKeepAliveUsage(ctx context.Context, c *machineConn, report protocol.KeepAliveReport) {
	agent, err := s.Store.AgentRuntimeByID(ctx, report.AgentID)
	if err != nil || agent.MachineID != c.id {
		return
	}
	cost, isTotal := report.Cost, false
	if report.CostIsTotal {
		cost, isTotal = report.CostCumulative, true
	}
	own, _ := s.Store.RecordKeepAlive(ctx, store.KeepAliveUsage{AgentID: report.AgentID, At: report.At, TokensIn: report.TokensIn, TokensOut: report.TokensOut,
		CacheRead: report.CacheRead, CacheWrite: report.CacheWrite, Cost: cost, CostIsTotal: isTotal})
	if own != nil && isTotal {
		// The card shows what the ping cost: for a running total that is the server's difference from the session's last report, which it knows
		// across a supervisor's restart, where the supervisor's own (kept in memory) is 0 for the first ping.
		c.warmMu.Lock()
		if ping, ok := c.pings[report.AgentID]; ok && ping.At == report.At {
			ping.Cost = *own
			c.pings[report.AgentID] = ping
		}
		c.warmMu.Unlock()
	}
}
