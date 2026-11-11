package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/answer"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

var ErrMachineUnavailable = errors.New("machine unavailable")
var ErrSlowMachine = errors.New("slow machine")

type machineConn struct {
	id                         string
	keyHash                    string
	conn                       *websocket.Conn
	sendQueue                  chan protocol.Frame
	done                       chan struct{}
	stopOnce                   sync.Once
	bound                      map[string]int
	boundMu                    sync.RWMutex
	controlMu                  sync.Mutex
	controlSent                map[string]int64
	controlReplayed            map[string]int64
	replayDone                 chan struct{} // closed once the stops pending at connection have been queued
	capabilities               []protocol.CapabilityReport
	supportsPathProbe          bool
	supportsDirectoryList      bool
	supportsSessionScan        bool
	supportsSessionFork        bool
	supportsSessionCopy        bool
	supportsLegacySessionCopy  bool
	supportsSessionAttribution bool
	supportsLaunchPath         bool
	supportsWorktree           bool
	supportsLaunchers          bool
	supportsProviders          bool
	supportsDetect             bool
	supportsDetectDir          bool
	supportsAgentLogs          bool
	supportsAgentLogSize       bool
	supportsStopReplay         bool
	supportsTelemetryConfig    bool
	supportsAgentKeys          bool
	supportsWarm               bool
	warmMu                     sync.Mutex
	pauseMu                    sync.Mutex // serializes what keep_alive_paused says; pauseLast is what it said last
	pauseLast                  string
	pauseSent                  bool
	settingsMu                 sync.Mutex // serializes agent_settings; settingsLast is what it said last
	settingsLast               string
	settingsSent               bool
	warm                       *protocol.WarmReport                // the supervisor's last report of its warm processes
	pings                      map[string]protocol.KeepAliveReport // each agent's last cache keep-alive ping, as its supervisor reported it
	harnessMu                  sync.Mutex
	harnessSetMu               sync.Mutex
	harnesses                  []protocol.HarnessRow
	launchers                  []protocol.LauncherRow
	harnessPending             map[string]chan protocol.Frame
	launcherPending            map[string]chan protocol.Frame
	pathPending                map[string]chan protocol.Frame
	journaled                  map[string]turnBinding
	lastSeen                   atomic.Int64
}

type turnBinding struct {
	agentID    string
	generation int
}

func (c *machineConn) stop() { c.stopOnce.Do(func() { close(c.done); _ = c.conn.CloseNow() }) }
func (c *machineConn) send(frame protocol.Frame) error {
	select {
	case <-c.done:
		return ErrMachineUnavailable
	default:
	}
	select {
	case c.sendQueue <- frame:
		return nil
	case <-c.done:
		return ErrMachineUnavailable
	default:
		c.stop()
		return ErrSlowMachine
	}
}

// sendControl queues a desired revision only once on this connection. A
// reconnect gets a fresh connection and may replay any unapplied stop.
func (c *machineConn) sendControl(frame protocol.Frame) error {
	return c.sendControlWith(frame, c.send)
}

func (c *machineConn) sendControlWith(frame protocol.Frame, enqueue func(protocol.Frame) error) error {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if c.controlSent[frame.AgentID] >= frame.DesiredRev && frame.DesiredRev > 0 {
		return nil
	}
	if err := enqueue(frame); err != nil {
		return err
	}
	if c.controlSent == nil {
		c.controlSent = make(map[string]int64)
	}
	c.controlSent[frame.AgentID] = frame.DesiredRev
	return nil
}

// A reconnect can have more pending controls than the ordinary bounded send
// queue. Wait for the writer to drain it while the read loop handles acks.
func (c *machineConn) sendReplayControl(ctx context.Context, frame protocol.Frame) error {
	select {
	case <-c.done:
		return ErrMachineUnavailable
	default:
	}
	// Reserve the revision before waiting for queue space so a concurrent live
	// control does not enqueue a duplicate. Release the lock while waiting:
	// the read loop needs it to accept acks and drain the peer's write side.
	c.controlMu.Lock()
	if c.controlSent[frame.AgentID] >= frame.DesiredRev && frame.DesiredRev > 0 {
		c.controlMu.Unlock()
		return nil
	}
	if c.controlSent == nil {
		c.controlSent = make(map[string]int64)
	}
	c.controlSent[frame.AgentID] = frame.DesiredRev
	if c.controlReplayed == nil {
		c.controlReplayed = make(map[string]int64)
	}
	c.controlReplayed[frame.AgentID] = frame.DesiredRev
	c.controlMu.Unlock()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(c.sendQueue) < cap(c.sendQueue)/2 {
			select {
			case c.sendQueue <- frame:
				return nil
			default:
			}
		}
		select {
		case <-c.done:
			return ErrMachineUnavailable
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			c.stop()
			return ErrSlowMachine
		case <-ticker.C:
		}
	}
}

func (c *machineConn) sentControlRevision(agentID string, rev int64) bool {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	return rev > 0 && c.controlSent[agentID] >= rev
}

func (c *machineConn) replayedControlRevision(agentID string, rev int64) bool {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	return c.controlReplayed[agentID] >= rev
}

func (s *Server) machineSocket(w http.ResponseWriter, r *http.Request) {
	if s.maintenance.Load() {
		w.Header().Set("Retry-After", "5")
		apiError(w, 503, "maintenance", "import in progress; reconnect shortly")
		return
	}
	machineID := r.Header.Get("X-Machine-ID")
	authHeader := r.Header.Get("Authorization")
	if machineID == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		apiError(w, 401, "unauthorized", "invalid machine credential")
		return
	}
	keyHash, authErr := s.Auth.VerifiedMachineKeyHash(r.Context(), machineID, strings.TrimPrefix(authHeader, "Bearer "))
	if authErr != nil {
		apiError(w, 401, "unauthorized", "invalid machine credential")
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(protocol.MaxMachineFrameBytes)
	ctx := r.Context()
	hello, err := readMachineFrame(ctx, conn)
	if err != nil || hello.V != protocol.Version || hello.Type != protocol.Hello || hello.MachineID != machineID || hello.JournalID == "" {
		conn.Close(websocket.StatusPolicyViolation, "invalid hello")
		return
	}
	s.maintenanceMu.RLock()
	handshakeLocked := true
	defer func() {
		if handshakeLocked {
			s.maintenanceMu.RUnlock()
		}
	}()
	if s.maintenance.Load() {
		conn.Close(websocket.StatusTryAgainLater, "import in progress")
		return
	}
	agents, err := s.Store.AgentsForMachine(ctx, machineID)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "machine state error")
		return
	}
	bound := make(map[string]int, len(agents))
	for _, agent := range agents {
		if !agent.Revoked {
			bound[agent.ID] = agent.Generation
		}
	}
	seenHello := map[string]bool{}
	journaled := map[string]turnBinding{}
	for _, item := range hello.Agents {
		generation, ok := bound[item.AgentID]
		if !ok || (generation != item.Generation && len(item.OpenTurns) == 0) {
			conn.Close(websocket.StatusPolicyViolation, "invalid agent binding")
			return
		}
		key := item.AgentID + "/" + strconv.Itoa(item.Generation)
		if seenHello[key] {
			conn.Close(websocket.StatusPolicyViolation, "duplicate agent binding")
			return
		}
		seenHello[key] = true
		for _, turn := range item.OpenTurns {
			turnAgent, turnGeneration, err := s.Store.TurnBinding(ctx, turn.TurnID)
			if err != nil || turnAgent != item.AgentID || turnGeneration != item.Generation {
				conn.Close(websocket.StatusPolicyViolation, "invalid journal turn binding")
				return
			}
			if _, duplicate := journaled[turn.TurnID]; duplicate {
				conn.Close(websocket.StatusPolicyViolation, "duplicate journal turn")
				return
			}
			journaled[turn.TurnID] = turnBinding{item.AgentID, item.Generation}
		}
	}
	if err := s.Store.RecordMachineHello(ctx, machineID, hello.SupervisorVersion); err != nil {
		conn.Close(websocket.StatusInternalError, "machine state error")
		return
	}
	open := map[string]store.JournalTurn{}
	for _, agent := range hello.Agents {
		for _, turn := range agent.OpenTurns {
			open[turn.TurnID] = store.JournalTurn{TurnID: turn.TurnID, Exec: turn.Exec, ProcessAlive: turn.ProcessAlive, HasAnswer: turn.HasAnswer, AnswerRev: turn.AnswerRev, Digest: turn.Digest}
		}
	}
	capabilities, err := json.Marshal(hello.Capabilities)
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "invalid capabilities")
		return
	}
	records, err := s.Store.ReconcileMachineHello(ctx, machineID, hello.JournalID, capabilities, open)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "reconciliation error")
		return
	}
	resend := make([]protocol.ReceiptBody, 0, len(records))
	for _, record := range records {
		details, _ := json.Marshal(map[string]string{"held_reason": record.Reason})
		resend = append(resend, protocol.ReceiptBody{TurnID: record.TurnID, AnswerRev: record.AnswerRev, Digest: record.Digest, Result: record.Result, Details: details})
	}
	settings, err := s.Store.HarnessSettings(ctx, machineID)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "harness state error")
		return
	}
	launchers, err := s.Store.MachineLaunchers(ctx, machineID)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "launcher state error")
		return
	}
	definitions := make([]protocol.LauncherDefinition, 0, len(launchers))
	for _, item := range launchers {
		definitions = append(definitions, launcherDefinition(item))
	}
	probeDirs, err := s.Store.MachineProbeFolders(ctx, machineID, maxProbeFolders)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "machine folder error")
		return
	}
	probeDir := ""
	if len(probeDirs) > 0 {
		probeDir = probeDirs[0]
	}
	if len(hello.Harnesses) > 0 {
		if err := s.Store.RecordMachineHarnesses(ctx, machineID, hello.Harnesses); err != nil {
			conn.Close(websocket.StatusInternalError, "harness state error")
			return
		}
	}
	c := &machineConn{id: machineID, keyHash: keyHash, conn: conn, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), replayDone: make(chan struct{}), bound: bound, journaled: journaled, capabilities: hello.Capabilities, supportsPathProbe: hello.SupportsPathProbe, supportsDirectoryList: hello.SupportsDirectoryList, supportsSessionScan: hello.SupportsSessionScan, supportsSessionFork: hello.SupportsSessionFork, supportsSessionCopy: hello.SupportsSessionCopy, supportsLegacySessionCopy: hello.SupportsLegacySessionCopy, supportsSessionAttribution: hello.SupportsSessionAttribution, supportsLaunchPath: hello.SupportsLaunchPath, supportsWorktree: hello.SupportsWorktree, supportsLaunchers: hello.SupportsLaunchers, supportsProviders: hello.SupportsProviders, supportsDetect: hello.SupportsDetect, supportsDetectDir: hello.SupportsDetectDir, supportsAgentLogs: hello.SupportsAgentLogs, supportsAgentLogSize: hello.SupportsAgentLogSize, supportsStopReplay: hello.SupportsStopReplay, supportsTelemetryConfig: hello.SupportsTelemetryConfig, supportsAgentKeys: hello.SupportsAgentKeys, supportsWarm: hello.SupportsWarm, harnesses: hello.Harnesses, launchers: hello.Launchers, harnessPending: make(map[string]chan protocol.Frame), launcherPending: make(map[string]chan protocol.Frame), pathPending: make(map[string]chan protocol.Frame)}
	c.lastSeen.Store(time.Now().UnixNano())
	if !c.supportsWarm {
		s.noteWarmSeen(machineID, 0) // a supervisor that cannot keep anything warm has nothing left running when it drops
	}
	s.machineMu.Lock()
	old := s.machines[machineID]
	s.machines[machineID] = c
	s.machineMu.Unlock()
	if old != nil {
		old.stop()
	}
	defer func() {
		c.stop()
		s.machineMu.Lock()
		if s.machines[machineID] == c {
			delete(s.machines, machineID)
		}
		s.machineMu.Unlock()
	}()
	go s.machineWriter(ctx, c)
	desired := make([]protocol.DesiredAgent, 0, len(agents))
	for _, agent := range agents {
		if agent.Revoked {
			continue
		}
		desired = append(desired, protocol.DesiredAgent{AgentID: agent.ID, Generation: agent.Generation, Desired: agent.Desired, DesiredRev: agent.DesiredRev})
	}
	if !c.supportsStopReplay {
		for i := range desired {
			if desired[i].Desired == "stopped" && desired[i].DesiredRev > 0 {
				// An older supervisor ignores controls at the snapshot revision.
				// It still sees the stopped state, then applies the current
				// revision from the replay and starts its kill fallback.
				desired[i].DesiredRev--
			}
		}
	}
	grants, err := s.grantAgentKeys(ctx, c, hello.HeldKeys)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "agent key error")
		return
	}
	if err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.HelloAck, Telemetry: s.telemetryForMachine(ctx, c), AgentKeys: grants, Desired: desired, Redeliver: []protocol.Delivery{}, ResendReceipts: resend, HarnessSettings: settings, HarnessProbeDir: probeDir, HarnessProbeDirs: probeDirs, LauncherDefinitions: definitions}); err != nil {
		return
	}
	if err := s.enqueuePendingSpawns(ctx, c); err != nil {
		return
	}
	s.maintenanceMu.RUnlock()
	handshakeLocked = false
	go s.machinePinger(ctx, c)
	go s.watchKeepAlivePause(ctx, c)
	go s.replayPendingStops(ctx, c)
	for {
		frame, err := readMachineFrame(ctx, conn)
		if err != nil {
			return
		}
		c.lastSeen.Store(time.Now().UnixNano())
		if s.maintenance.Load() {
			_ = conn.Close(websocket.StatusTryAgainLater, "import in progress")
			return
		}
		if frame.V != protocol.Version {
			_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.Error, Code: "invalid_request", Message: "unsupported protocol version"})
			continue
		}
		if frame.Type == protocol.Ping || frame.Type == protocol.Pong {
			_ = s.Store.TouchMachine(ctx, machineID)
			if frame.Type == protocol.Ping {
				_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.Pong, MachineID: machineID})
			}
			continue
		}
		if frame.Type == protocol.HarnessStatus {
			if len(frame.Harnesses) > 0 {
				c.harnessMu.Lock()
				c.harnesses = frame.Harnesses
				c.harnessMu.Unlock()
				_ = s.Store.RecordMachineHarnesses(ctx, machineID, frame.Harnesses)
			}
			if len(frame.Capabilities) > 0 {
				c.harnessMu.Lock()
				c.capabilities = frame.Capabilities
				c.harnessMu.Unlock()
				_ = s.Store.RecordMachineCapabilities(ctx, machineID, frame.Capabilities)
			}
			continue
		}
		if frame.Type == protocol.HarnessResult {
			c.harnessMu.Lock()
			waiter := c.harnessPending[frame.ReqID]
			c.harnessMu.Unlock()
			if waiter != nil {
				select {
				case waiter <- frame:
				default:
				}
			}
			continue
		}
		if frame.Type == protocol.LauncherStatus {
			c.harnessMu.Lock()
			c.launchers = frame.Launchers
			if len(frame.Capabilities) > 0 {
				c.capabilities = frame.Capabilities
			}
			c.harnessMu.Unlock()
			if len(frame.Capabilities) > 0 {
				_ = s.Store.RecordMachineCapabilities(ctx, machineID, frame.Capabilities)
			}
			continue
		}
		if frame.Type == protocol.KeepAliveScopeFrame {
			s.handleKeepAliveScope(c, frame, func(agentID string) bool {
				agent, err := s.Store.AgentRuntimeByID(ctx, agentID)
				return err == nil && agent.MachineID == c.id
			})
			continue
		}
		if frame.Type == protocol.KeepAliveFrame {
			if frame.KeepAlive != nil && frame.KeepAlive.AgentID != "" {
				c.warmMu.Lock()
				if c.pings == nil {
					c.pings = map[string]protocol.KeepAliveReport{}
				}
				c.pings[frame.KeepAlive.AgentID] = *frame.KeepAlive
				c.warmMu.Unlock()
				s.recordKeepAliveUsage(ctx, c, *frame.KeepAlive)
			}
			continue
		}
		if frame.Type == protocol.WarmClaimFrame {
			s.handleWarmClaim(c, frame)
			continue
		}
		if frame.Type == protocol.WarmEndedFrame {
			s.handleWarmEnded(ctx, c, frame)
			continue
		}
		if frame.Type == protocol.WarmStateFrame {
			if frame.Warm != nil {
				c.warmMu.Lock()
				c.warm = frame.Warm
				c.warmMu.Unlock()
				s.applyWarmCount(c, frame.Warm.Seq, max(len(frame.Warm.Agents), frame.Warm.Processes)+frame.Warm.PingsActive, false)
			}
			continue
		}
		if frame.Type == protocol.LauncherResult || frame.Type == protocol.DetectResult {
			c.harnessMu.Lock()
			waiter := c.launcherPending[frame.ReqID]
			c.harnessMu.Unlock()
			if waiter != nil {
				select {
				case waiter <- frame:
				default:
				}
			}
			continue
		}
		if frame.Type == protocol.PathResult || frame.Type == protocol.DirectoryListResult || frame.Type == protocol.SessionScanResult || frame.Type == protocol.WorktreeResult || frame.Type == protocol.AgentLogResult || frame.Type == protocol.AgentLogUsageResult || frame.Type == protocol.RetireWarmResult {
			c.harnessMu.Lock()
			waiter := c.pathPending[frame.ReqID]
			c.harnessMu.Unlock()
			if waiter != nil {
				select {
				case waiter <- frame:
				default:
				}
			}
			continue
		}
		if code := s.authorizeAgentFrame(ctx, c, frame); code != "" {
			_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: frame.AgentID, TurnID: frame.TurnID, Code: code, Message: "agent frame rejected"})
			continue
		}
		if err := s.handleAgentFrame(ctx, c, frame); err != nil {
			code := "invalid_request"
			if errors.Is(err, store.ErrConflict) {
				code = "conflict"
			} else if errors.Is(err, store.ErrForbidden) {
				code = "forbidden"
			} else if errors.Is(err, store.ErrStaleGeneration) {
				code = "stale_generation"
			} else if errors.Is(err, store.ErrInvalidTransition) {
				code = "invalid_transition"
			}
			_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: frame.AgentID, TurnID: frame.TurnID, Code: code, Message: err.Error()})
		}
	}
}

func readMachineFrame(ctx context.Context, conn *websocket.Conn) (protocol.Frame, error) {
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return protocol.Frame{}, err
	}
	var frame protocol.Frame
	err = json.Unmarshal(raw, &frame)
	return frame, err
}

func (s *Server) machineWriter(ctx context.Context, c *machineConn) {
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			c.stop()
			return
		case frame := <-c.sendQueue:
			if err := s.writeFrame(ctx, c.conn, frame); err != nil {
				c.stop()
				return
			}
		}
	}
}

func (s *Server) replayPendingStops(ctx context.Context, c *machineConn) {
	if c.replayDone != nil {
		defer close(c.replayDone)
	}
	stops, err := s.Store.PendingStopControlsForMachine(ctx, c.id)
	if err != nil {
		c.stop()
		return
	}
	for _, agent := range stops {
		if err := c.sendReplayControl(ctx, protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: agent.ID,
			Generation: agent.Generation, DesiredRev: agent.DesiredRev, DesiredState: "stopped"}); err != nil {
			return
		}
	}
}

func (s *Server) machinePinger(ctx context.Context, c *machineConn) {
	interval := s.MachinePingInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.Auth.VerifyMachineKeyHash(ctx, c.id, c.keyHash) != nil {
				_ = c.conn.Close(websocket.StatusPolicyViolation, "machine credential revoked")
				c.stop()
				return
			}
			if err := s.enqueuePendingSpawns(ctx, c); err != nil {
				c.stop()
				return
			}
			if time.Since(time.Unix(0, c.lastSeen.Load())) > 60*time.Second {
				c.stop()
				return
			}
			if c.send(protocol.Frame{V: protocol.Version, Type: protocol.Ping, MachineID: c.id}) != nil {
				return
			}
		}
	}
}

func (s *Server) authorizeAgentFrame(ctx context.Context, c *machineConn, frame protocol.Frame) string {
	if frame.AgentID == "" {
		return "forbidden"
	}
	c.boundMu.RLock()
	generation, ok := c.bound[frame.AgentID]
	c.boundMu.RUnlock()
	if !ok {
		return "forbidden"
	}
	agent, err := s.Store.AgentRuntimeByID(ctx, frame.AgentID)
	if err != nil {
		return "unauthorized"
	}
	if agent.Revoked || agent.MachineID != c.id {
		return "unauthorized"
	}
	if frame.Generation != generation || agent.Generation != generation {
		if frame.Type == protocol.Answer || frame.Type == protocol.NativeSessionStarted {
			if old, ok := c.journaled[frame.TurnID]; ok && old.agentID == frame.AgentID && old.generation == frame.Generation {
				return ""
			}
		}
		return "stale_generation"
	}
	return ""
}

func (s *Server) handleAgentFrame(ctx context.Context, c *machineConn, frame protocol.Frame) error {
	s.maintenanceMu.RLock()
	defer s.maintenanceMu.RUnlock()
	switch frame.Type {
	case protocol.SpawnResult:
		if frame.ReqID != fmt.Sprintf("%s/%d", frame.AgentID, frame.Generation) {
			return store.ErrConflict
		}
		if frame.Result != "ok" {
			if err := s.Store.FailSpawn(ctx, frame.AgentID, frame.Generation, frame.Message); err != nil {
				return err
			}
			s.spawnMu.Lock()
			delete(s.spawnKeys, fmt.Sprintf("%s/%d", frame.AgentID, frame.Generation))
			s.spawnMu.Unlock()
			return errors.New("spawn failed: " + frame.Message)
		}
		if err := s.Store.ConfirmSpawn(ctx, frame.AgentID, frame.Generation); err != nil {
			return err
		}
		s.spawnMu.Lock()
		delete(s.spawnKeys, fmt.Sprintf("%s/%d", frame.AgentID, frame.Generation))
		s.spawnMu.Unlock()
		return nil
	case protocol.Ack:
		if frame.Level == "control" {
			rev, err := strconv.ParseInt(frame.Ref, 10, 64)
			if err != nil || !c.sentControlRevision(frame.AgentID, rev) {
				return nil
			}
			if !c.supportsStopReplay {
				// A legacy supervisor acks as soon as it sees a control. A
				// replay can be acknowledged while the process is still live,
				// and observed=stopped may predate an offline delivery.
				if c.replayedControlRevision(frame.AgentID, rev) {
					return nil
				}
				card, err := s.Store.AgentCardByID(ctx, frame.AgentID)
				if errors.Is(err, sql.ErrNoRows) {
					return nil // an agent that is gone has nothing left to confirm
				}
				if err != nil {
					return err
				}
				if card.Observed != "stopped" {
					return nil
				}
			}
			return s.Store.AckAgentControl(ctx, frame.AgentID, frame.Generation, frame.Ref)
		}
		return s.Store.AckTurn(ctx, frame.TurnID, frame.AgentID, frame.Generation, frame.Level)
	case protocol.Activity, protocol.Usage, protocol.Compaction:
		data, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		return s.Store.RecordTurnEvent(ctx, frame.TurnID, frame.AgentID, frame.SourceEventID, frame.Type, frame.Summary, data)
	case protocol.NativeSessionStarted:
		return s.Store.RecordNativeSessionStarted(ctx, frame.TurnID, frame.AgentID, frame.Generation,
			frame.SourceEventID, frame.NativeSessionHandle)
	case protocol.Answer:
		owner, generation, err := s.Store.TurnBinding(ctx, frame.TurnID)
		if err != nil {
			return err
		}
		if owner != frame.AgentID {
			return store.ErrForbidden
		}
		if generation != frame.Generation {
			return store.ErrStaleGeneration
		}
		if frame.AnswerTooLarge {
			if frame.Candidate != "" {
				return store.ErrConflict
			}
		} else {
			sum := sha256.Sum256([]byte(frame.Candidate))
			if frame.Digest != hex.EncodeToString(sum[:]) {
				return store.ErrConflict
			}
		}
		mode, err := s.Store.TurnMode(ctx, frame.TurnID)
		if err != nil {
			return err
		}
		var parsed answer.Candidate
		var parseErr error
		if frame.AnswerTooLarge {
			parseErr = fmt.Errorf("%w: answer too large (max 2 MiB)", answer.ErrInvalid)
		} else {
			parsed, parseErr = answer.Parse([]byte(frame.Candidate), mode == "diagnostic")
		}
		input := store.AnswerInput{TurnID: frame.TurnID, AgentID: frame.AgentID, Generation: frame.Generation,
			AnswerRev: frame.AnswerRev, Digest: frame.Digest, Candidate: []byte(frame.Candidate)}
		if parseErr != nil {
			input.ParseError = parseErr.Error()
		} else {
			input.Parsed = &parsed
		}
		receipt, err := s.Store.ApplyAnswer(ctx, input)
		if errors.Is(err, store.ErrConflict) {
			return c.send(protocol.Frame{V: protocol.Version, Type: protocol.Receipt, TurnID: frame.TurnID, AnswerRev: frame.AnswerRev, Digest: frame.Digest, Result: "conflict"})
		}
		if err != nil {
			return err
		}
		for _, extra := range receipt.Additional {
			if err := c.send(answerReceiptFrame(extra)); err != nil {
				return err
			}
		}
		return c.send(answerReceiptFrame(receipt))
	case protocol.Process:
		if frame.Exit == nil {
			return errors.New("missing process exit")
		}
		if err := s.Store.RecordProcessInfo(ctx, frame.TurnID, frame.AgentID, frame.Generation, *frame.Exit); err != nil {
			return err
		}
		if err := s.Store.ConfirmAgentProcessExit(ctx, frame.AgentID); err != nil {
			return err
		}
		return c.send(protocol.Frame{V: protocol.Version, Type: protocol.Ack, TurnID: frame.TurnID, Level: "process"})
	default:
		return errors.New("unknown agent frame")
	}
}

func answerReceiptFrame(receipt store.AnswerReceipt) protocol.Frame {
	details, _ := json.Marshal(map[string]any{"held_reason": receipt.HeldReason, "error": receipt.Error, "message_ids": receipt.MessageIDs, "warnings": receipt.Warnings})
	return protocol.Frame{V: protocol.Version, Type: protocol.Receipt, TurnID: receipt.TurnID, AnswerRev: receipt.AnswerRev,
		Digest: receipt.Digest, Result: receipt.Result, Details: details}
}

// ErrSupervisorUpdateNeeded is a delivery the machine's supervisor cannot be trusted with: it works on a copy of a Claude Code session, and
// the supervisor does not advertise supports_session_fork.
var ErrSupervisorUpdateNeeded = errors.New("supervisor_update_needed")

// Dispatch is the T6a test hook. T8 will select queued notes and build these
// deliveries; this boundary applies admission checks before enqueueing.
func (s *Server) Dispatch(ctx context.Context, delivery protocol.Delivery) error {
	agent, err := s.Store.AgentRuntimeByID(ctx, delivery.AgentID)
	if err != nil {
		return err
	}
	c, err := s.machineForID(agent.MachineID)
	if err != nil {
		return err
	}
	if err := s.Auth.VerifyMachineKeyHash(ctx, c.id, c.keyHash); err != nil {
		_ = c.conn.Close(websocket.StatusPolicyViolation, "machine credential revoked")
		c.stop()
		return err
	}
	// Checked on every delivery, not once at spawn: the machine may have reconnected with an older supervisor since. One that does not know
	// the flag would run `claude --resume <original>` without --fork-session, writing into the original transcript for good.
	if delivery.ForkSession && !c.supportsSessionFork {
		return ErrSupervisorUpdateNeeded
	}
	if delivery.ImportLegacySession && !c.supportsLegacySessionCopy {
		return ErrSupervisorUpdateNeeded
	}
	// A supervisor that does not know launch_path would start Claude in the canonical folder, where a session made under a link is not found.
	if delivery.LaunchPath != "" && !c.supportsLaunchPath {
		return ErrSupervisorUpdateNeeded
	}
	// A supervisor that does not know providers would start OpenCode without the endpoint its model names.
	if len(delivery.Providers) > 0 && !c.supportsProviders {
		return ErrSupervisorUpdateNeeded
	}
	refused, err := s.refusedAgent(ctx, agent.MachineID, delivery.Agent.Tool, delivery.Agent.Variant, delivery.Agent.LauncherID)
	if err != nil {
		return err
	}
	if refused {
		return errDockerOnly
	}
	delivery.DockerOnly, err = s.dockerOnly(ctx)
	if err != nil {
		return err
	}
	delivery.WarmIdleSeconds, err = s.warmIdleToSend(ctx)
	if err != nil {
		return err
	}
	keep, err := s.keepAliveConfig(ctx)
	if err != nil {
		return err
	}
	delivery.KeepAliveSettings = keep.settingsFor(delivery.Agent.Tool)
	c.boundMu.RLock()
	generation, ok := c.bound[delivery.AgentID]
	c.boundMu.RUnlock()
	if !ok || generation != delivery.Generation {
		return store.ErrStaleGeneration
	}
	for _, note := range delivery.Notes {
		if len(note.Attachments) == 0 {
			continue
		}
		keyHash, revoked, err := s.Store.AgentKeyByID(ctx, delivery.AgentID)
		if err != nil {
			return err
		}
		if revoked || keyHash == "" {
			return store.ErrForbidden
		}
		delivery.AttachmentBearer = attachmentBearer(keyHash, delivery.TurnID)
		break
	}
	noteIDs := make([]string, 0, len(delivery.Notes))
	for _, note := range delivery.Notes {
		noteIDs = append(noteIDs, note.NoteID)
	}
	if err := s.Store.AdmitTurn(ctx, store.Admission{TurnID: delivery.TurnID, AgentID: delivery.AgentID, MachineID: agent.MachineID, Generation: delivery.Generation, DesiredRev: delivery.DesiredRev, NoteIDs: noteIDs}); err != nil {
		return err
	}
	return c.send(protocol.DeliverFrame(delivery))
}

// maxProbeFolders bounds how many of a machine's project folders are handed to its supervisor to ask a container launcher from.
const maxProbeFolders = 8

func (s *Server) machineForID(id string) (*machineConn, error) {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	c := s.machines[id]
	if c == nil {
		return nil, ErrMachineUnavailable
	}
	return c, nil
}
