package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

var ErrMachineUnavailable = errors.New("machine unavailable")
var ErrSlowMachine = errors.New("slow machine")

type machineConn struct {
	id                         string
	journalID                  string // the supervisor instance behind this connection
	build                      string
	retiring                   bool
	warmCount                  int
	keyHash                    string
	keysMu                     sync.Mutex
	keys                       map[string]bool
	conn                       *websocket.Conn
	sendQueue                  chan protocol.Frame
	done                       chan struct{}
	stopOnce                   sync.Once
	connectedAt                atomic.Int64 // unix nanoseconds when this connection replaced the last one: a request older than it went out on another one
	helloAcked                 atomic.Bool
	pendingRetire              *machineConn   // guarded by machineMu; remains dispatchable until this connection is ready
	workers                    sync.WaitGroup // helpers may read the store after the socket handler exits
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
	supportsAgentConversation  bool
	supportsStopReplay         bool
	supportsTurnInterrupt      bool
	supportsAttachmentSave     bool
	supportsTelemetryConfig    bool
	supportsAgentKeys          bool
	supportsAgentKeysFrame     bool
	supportsCommands           bool
	supportsContainerCommands  bool
	supportsHandoffOutcome     bool
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
	ctx, release, ok := s.trackSocket(r.Context(), conn)
	if !ok {
		return
	}
	defer release()
	hello, err := readMachineFrame(ctx, conn)
	if err != nil || hello.V != protocol.Version || hello.Type != protocol.Hello || hello.MachineID != machineID || hello.JournalID == "" {
		conn.Close(websocket.StatusPolicyViolation, "invalid hello")
		return
	}
	localSupervisorID := s.localSupervisorID()
	localSupervisor := machineID == localSupervisorID && localSupervisorID != ""
	if localSupervisor && hello.SupportsRetireAtIdle && s.LocalSupervisorLegacyDrainCancel != nil {
		s.LocalSupervisorLegacyDrainCancel()
	}
	matchingBuild := hello.SupervisorVersion == s.Version
	restartPending := hello.DrainOnly || localSupervisor && (!matchingBuild || hello.SupervisorEnvironment != s.LocalSupervisorEnvironment)
	if !localSupervisor && !matchingBuild && !hello.DrainOnly {
		s.machineMu.Lock()
		current := s.machines[machineID]
		restartPending = current != nil && current.build == s.Version
		s.machineMu.Unlock()
	}
	upgradeRequired := false
	if localSupervisor && restartPending && hello.SupervisorVersion != s.Version && s.LocalSupervisorDiskVersion != nil {
		if diskVersion, probeErr := s.LocalSupervisorDiskVersion(); probeErr != nil {
			log.Printf("local supervisor disk version check: %v", probeErr)
		} else if diskVersion != s.Version && diskVersion == hello.SupervisorVersion {
			upgradeRequired = true
		}
	}
	if localSupervisor {
		s.localSupervisorCanInterrupt.Store(hello.SupportsRetireAtIdle)
		s.localSupervisorUpgradeRequired.Store(upgradeRequired)
		if upgradeRequired {
			s.localSupervisorPending.Store(false)
			log.Printf("local supervisor uses the upgraded binary; restart the server to finish the upgrade")
		}
		if restartPending {
			s.localSupervisorPending.Store(true)
			log.Printf("local supervisor restart pending: build or turn environment differs from server")
		}
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
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: machine state error: %w", err))
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
			owner, err := s.Store.TurnOwner(ctx, turn.TurnID)
			if err != nil || owner != "" && owner != hello.JournalID {
				conn.Close(websocket.StatusPolicyViolation, "invalid journal turn owner")
				return
			}
			if _, duplicate := journaled[turn.TurnID]; duplicate {
				conn.Close(websocket.StatusPolicyViolation, "duplicate journal turn")
				return
			}
			journaled[turn.TurnID] = turnBinding{item.AgentID, item.Generation}
		}
	}
	if !restartPending {
		if err := s.Store.RecordMachineHello(ctx, machineID, hello.SupervisorVersion); err != nil {
			telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: recording the hello: %w", err))
			conn.Close(websocket.StatusInternalError, "machine state error")
			return
		}
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
	if err := s.Store.RecordSupervisorInstance(ctx, machineID, hello.JournalID, hello.SupervisorVersion, hello.SupervisorEnvironment); err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: recording the supervisor instance: %w", err))
		conn.Close(websocket.StatusInternalError, "machine state error")
		return
	}
	if restartPending {
		if err := s.Store.SetSupervisorInstanceState(ctx, machineID, hello.JournalID, "retiring"); err != nil {
			conn.Close(websocket.StatusInternalError, "instance state error")
			return
		}
	}
	records, err := s.Store.ReconcileMachineHelloWithRole(ctx, machineID, hello.JournalID, capabilities, open, !restartPending)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: reconciliation error: %w", err))
		conn.Close(websocket.StatusInternalError, "reconciliation error")
		return
	}
	// A supervisor that runs commands says which it still runs and which ends it still owes: a job it knows nothing of can never be reported
	// now, so it is settled here instead of counting against its agent forever.
	var killCommands []store.CommandJob
	if hello.SupportsCommands {
		killCommands, err = s.Store.ReconcileMachineCommands(ctx, machineID, hello.JournalID, hello.CommandOpen, hello.CommandExits)
		if err != nil {
			telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: command reconciliation error: %w", err))
			conn.Close(websocket.StatusInternalError, "reconciliation error")
			return
		}
	}
	resend := make([]protocol.ReceiptBody, 0, len(records))
	for _, record := range records {
		details, _ := json.Marshal(map[string]string{"held_reason": record.Reason})
		resend = append(resend, protocol.ReceiptBody{TurnID: record.TurnID, AnswerRev: record.AnswerRev, Digest: record.Digest, Result: record.Result, Details: details})
	}
	settings, err := s.Store.HarnessSettings(ctx, machineID)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: harness state error: %w", err))
		conn.Close(websocket.StatusInternalError, "harness state error")
		return
	}
	launchers, err := s.Store.MachineLaunchers(ctx, machineID)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: launcher state error: %w", err))
		conn.Close(websocket.StatusInternalError, "launcher state error")
		return
	}
	definitions := make([]protocol.LauncherDefinition, 0, len(launchers))
	for _, item := range launchers {
		definitions = append(definitions, launcherDefinition(item))
	}
	probeDirs, err := s.Store.MachineProbeFolders(ctx, machineID, maxProbeFolders)
	if err != nil {
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: machine folder error: %w", err))
		conn.Close(websocket.StatusInternalError, "machine folder error")
		return
	}
	probeDir := ""
	if len(probeDirs) > 0 {
		probeDir = probeDirs[0]
	}
	if !restartPending && len(hello.Harnesses) > 0 {
		if err := s.Store.RecordMachineHarnesses(ctx, machineID, hello.Harnesses); err != nil {
			telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: recording the harnesses: %w", err))
			conn.Close(websocket.StatusInternalError, "harness state error")
			return
		}
	}
	c := &machineConn{id: machineID, journalID: hello.JournalID, build: hello.SupervisorVersion, retiring: restartPending, keyHash: keyHash, conn: conn, sendQueue: make(chan protocol.Frame, 32), done: make(chan struct{}), replayDone: make(chan struct{}), bound: bound, journaled: journaled, capabilities: hello.Capabilities, supportsPathProbe: hello.SupportsPathProbe, supportsDirectoryList: hello.SupportsDirectoryList, supportsSessionScan: hello.SupportsSessionScan, supportsSessionFork: hello.SupportsSessionFork, supportsSessionCopy: hello.SupportsSessionCopy, supportsLegacySessionCopy: hello.SupportsLegacySessionCopy, supportsSessionAttribution: hello.SupportsSessionAttribution, supportsLaunchPath: hello.SupportsLaunchPath, supportsWorktree: hello.SupportsWorktree, supportsLaunchers: hello.SupportsLaunchers, supportsProviders: hello.SupportsProviders, supportsDetect: hello.SupportsDetect, supportsDetectDir: hello.SupportsDetectDir, supportsAgentLogs: hello.SupportsAgentLogs, supportsAgentLogSize: hello.SupportsAgentLogSize, supportsAgentConversation: hello.SupportsAgentConversationLog, supportsStopReplay: hello.SupportsStopReplay, supportsTurnInterrupt: hello.SupportsTurnInterrupt, supportsAttachmentSave: hello.SupportsAttachmentSave, supportsTelemetryConfig: hello.SupportsTelemetryConfig, supportsAgentKeys: hello.SupportsAgentKeys, supportsAgentKeysFrame: hello.SupportsAgentKeysFrame, supportsCommands: hello.SupportsCommands, supportsContainerCommands: hello.SupportsContainerCommands, supportsHandoffOutcome: hello.SupportsHandoffOutcome, supportsWarm: hello.SupportsWarm, harnesses: hello.Harnesses, launchers: hello.Launchers, harnessPending: make(map[string]chan protocol.Frame), launcherPending: make(map[string]chan protocol.Frame), pathPending: make(map[string]chan protocol.Frame)}
	c.keys = make(map[string]bool, len(hello.HeldKeys))
	for _, id := range hello.HeldKeys {
		c.keys[id] = true
	}
	c.lastSeen.Store(time.Now().UnixNano())
	s.machineMu.Lock()
	var replaced, retiring *machineConn
	if existing := s.machines[machineID]; existing != nil && existing.journalID == c.journalID {
		replaced = existing
	}
	if others := s.retiringMachines[machineID]; others != nil {
		if existing := others[c.journalID]; existing != nil {
			replaced = existing
			delete(others, c.journalID)
		}
	}
	if replaced != nil {
		replaced.warmMu.Lock()
		c.warm = replaced.warm
		replaced.warmMu.Unlock()
	}
	c.connectedAt.Store(time.Now().UnixNano())
	if c.retiring {
		if replaced != nil && s.machines[machineID] == replaced {
			delete(s.machines, machineID)
		}
		if s.retiringMachines[machineID] == nil {
			s.retiringMachines[machineID] = make(map[string]*machineConn)
		}
		s.retiringMachines[machineID][c.journalID] = c
	} else {
		if previous := s.machines[machineID]; previous != nil && previous != c && previous != replaced {
			if s.retiringMachines[machineID] == nil {
				s.retiringMachines[machineID] = make(map[string]*machineConn)
			}
			previous.retiring = true
			s.retiringMachines[machineID][previous.journalID] = previous
			retiring = previous
			c.pendingRetire = previous
		}
		s.machines[machineID] = c
	}
	active := s.machines[machineID] != nil
	s.machineMu.Unlock()
	if replaced != nil {
		replaced.stop()
	}
	if localSupervisor {
		s.localSupervisorPending.Store(!active)
	}
	if !c.supportsWarm {
		s.applyWarmCount(c, 0, 0, false)
	}
	// A newly connected machine can take its agents queued notes.
	s.wakeDispatch()
	defer func() {
		c.stop()
		c.workers.Wait()
		s.machineMu.Lock()
		c.warmMu.Lock()
		hasWarm := c.warm != nil && len(c.warm.Agents) > 0
		c.warmMu.Unlock()
		if s.machines[machineID] == c {
			delete(s.machines, machineID)
			if !c.helloAcked.Load() && c.pendingRetire != nil && !connectionClosed(c.pendingRetire) {
				previous := c.pendingRetire
				previous.retiring = false
				delete(s.retiringMachines[machineID], previous.journalID)
				s.machines[machineID] = previous
			}
			if hasWarm {
				if s.retiringMachines[machineID] == nil {
					s.retiringMachines[machineID] = make(map[string]*machineConn)
				}
				if s.retiringMachines[machineID][c.journalID] == nil {
					s.retiringMachines[machineID][c.journalID] = c
				}
			}
		}
		if others := s.retiringMachines[machineID]; others != nil && others[c.journalID] == c {
			if !hasWarm {
				delete(others, c.journalID)
			}
			if len(others) == 0 {
				delete(s.retiringMachines, machineID)
			}
		}
		s.machineMu.Unlock()
	}()
	c.workers.Add(1)
	telemetry.Go(ctx, func() { defer c.workers.Done(); s.machineWriter(ctx, c) })
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
		telemetry.ReportError(ctx, "machine", fmt.Errorf("connection setup: agent key error: %w", err))
		conn.Close(websocket.StatusInternalError, "agent key error")
		return
	}
	if localSupervisor && !restartPending {
		// Before the ack, as a pending restart is set before it: a supervisor that sees its hello_ack is told it can work, so local dispatch
		// must not still be held then (a dispatch pass in between would skip it, and a replacement's first turn would wait for the next).
		s.localSupervisorPending.Store(false)
	}
	if err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.HelloAck, Telemetry: s.telemetryForMachine(ctx, c), AgentKeys: grants, Desired: desired, Redeliver: []protocol.Delivery{}, ResendReceipts: resend, HarnessSettings: settings, HarnessProbeDir: probeDir, HarnessProbeDirs: probeDirs, LauncherDefinitions: definitions}); err != nil {
		return
	}
	c.helloAcked.Store(true)
	if retiring != nil {
		_ = s.Store.SetSupervisorInstanceState(ctx, machineID, retiring.journalID, "retiring")
		_ = retiring.send(protocol.Frame{V: protocol.Version, Type: protocol.RetireAtIdle})
	}
	if localSupervisor {
		// Once per build, so the swap shows in the log and the supervisor's regular reconnects do not.
		if previous, _ := s.localSupervisorLoggedBuild.Load().(string); previous != hello.SupervisorVersion {
			s.localSupervisorLoggedBuild.Store(hello.SupervisorVersion)
			log.Printf("supervisor swap: local supervisor connected: build %s", hello.SupervisorVersion)
		}
	}
	if restartPending {
		if hello.SupportsRetireAtIdle {
			if err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.RetireAtIdle}); err != nil {
				return
			}
		}
	}
	for _, job := range killCommands {
		// Cancelled before its start was confirmed, yet the machine runs it: it must not outlive the cancel.
		if err := c.send(protocol.Frame{V: protocol.Version, Type: protocol.CommandCancel, ReqID: newRequestNonce(), JobID: job.ID, AgentID: job.AgentID}); err != nil {
			return
		}
	}
	if !restartPending {
		if err := s.enqueuePendingSpawns(ctx, c); err != nil {
			return
		}
	}
	s.maintenanceMu.RUnlock()
	handshakeLocked = false
	c.workers.Add(3)
	telemetry.Go(ctx, func() { defer c.workers.Done(); s.machinePinger(ctx, c) })
	telemetry.Go(ctx, func() { defer c.workers.Done(); s.watchKeepAlivePause(ctx, c) })
	telemetry.Go(ctx, func() { defer c.workers.Done(); s.replayPendingStops(ctx, c) })
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
			if err := s.Store.TouchMachine(ctx, machineID); err != nil {
				telemetry.ReportWarning(ctx, "machine", fmt.Errorf("recording that the machine is alive: %w", err))
			}
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
				if s.activeConnection(c) {
					if err := s.Store.RecordMachineHarnesses(ctx, machineID, frame.Harnesses); err != nil {
						telemetry.ReportError(ctx, "machine", fmt.Errorf("recording the machine's harnesses: %w", err))
					}
				}
			}
			if len(frame.Capabilities) > 0 {
				c.harnessMu.Lock()
				c.capabilities = frame.Capabilities
				c.harnessMu.Unlock()
				if s.activeConnection(c) {
					if err := s.Store.RecordMachineCapabilities(ctx, machineID, frame.Capabilities); err != nil {
						telemetry.ReportError(ctx, "machine", fmt.Errorf("recording the machine's capabilities: %w", err))
					}
				}
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
			if s.activeConnection(c) && len(frame.Capabilities) > 0 {
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
		if frame.Type == protocol.BackgroundJobEndedFrame {
			s.handleBackgroundJobEnded(ctx, c, frame)
			continue
		}
		if frame.Type == protocol.WarmStateFrame {
			if frame.Warm != nil {
				c.warmMu.Lock()
				previous := c.warm
				c.warm = frame.Warm
				c.warmMu.Unlock()
				s.applyWarmCount(c, frame.Warm.Seq, max(len(frame.Warm.Agents), frame.Warm.Processes)+frame.Warm.PingsActive, false)
				s.notifyWarmWorking(ctx, c, previous, frame.Warm)
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
		if frame.Type == protocol.CommandStarted {
			// Recorded before the waiting call (if any) sees it, so a late start or a refusal is never lost.
			s.applyCommandStarted(ctx, c, frame)
		}
		if frame.Type == protocol.CommandEnded {
			// Authorized by the job it reports on, not the agent's generation: a command outlives a context clear or a harness switch.
			if err := s.handleCommandEnd(ctx, c, frame); err != nil {
				code := "invalid_request"
				if errors.Is(err, store.ErrForbidden) {
					code = "forbidden"
				} else {
					telemetry.ReportError(ctx, "machine", fmt.Errorf("command end failed: %w", err))
				}
				_ = c.send(protocol.Frame{V: protocol.Version, Type: protocol.Error, AgentID: frame.AgentID, JobID: frame.JobID, Code: code, Message: err.Error()})
			}
			continue
		}
		if frame.Type == protocol.PathResult || frame.Type == protocol.DirectoryListResult || frame.Type == protocol.SessionScanResult || frame.Type == protocol.WorktreeResult || frame.Type == protocol.AgentLogResult || frame.Type == protocol.AgentLogUsageResult || frame.Type == protocol.RetireWarmResult || frame.Type == protocol.CommandStarted || frame.Type == protocol.CommandCancelResult || frame.Type == protocol.CommandLogResult || frame.Type == protocol.TurnInterruptResult || frame.Type == protocol.AttachmentSaveResult {
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
		if code, expected := s.authorizeAgentFrame(ctx, c, frame); code != "" {
			log.Printf("machine %s: agent frame rejected: type=%s agent_id=%s code=%s", c.id, frame.Type, frame.AgentID, code)
			if !expected {
				telemetry.ReportWarning(ctx, "machine", fmt.Errorf("agent frame rejected: type=%s code=%s", frame.Type, code))
			}
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
			} else if errors.Is(err, store.ErrStaleRev) {
				// A control (or a spawn) that an older desired_rev left behind: the expected race of a stop or a
				// change arriving while the supervisor acks what it had. The newer control is already on its way, so
				// the machine converges; nothing is reported.
				code = "stale_rev"
			} else if errors.Is(err, store.ErrInvalidTransition) {
				code = "invalid_transition"
			} else {
				// Not one of the classified refusals: a store or logic failure that the supervisor is told about as "invalid_request" and nobody else sees.
				telemetry.ReportError(ctx, "machine", fmt.Errorf("agent frame %s failed: %w", frame.Type, err))
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
			if s.activeConnection(c) {
				if err := s.enqueuePendingSpawns(ctx, c); err != nil {
					c.stop()
					return
				}
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

func (s *Server) authorizeAgentFrame(ctx context.Context, c *machineConn, frame protocol.Frame) (string, bool) {
	if frame.AgentID == "" {
		return "forbidden", false
	}
	c.boundMu.RLock()
	generation, ok := c.bound[frame.AgentID]
	c.boundMu.RUnlock()
	if !ok {
		return "forbidden", false
	}
	agent, err := s.Store.AgentRuntimeByID(ctx, frame.AgentID)
	if err != nil {
		return "unauthorized", false
	}
	if agent.Revoked || agent.MachineID != c.id {
		// A late frame (the supervisor's ack of a stop it applied) for an agent that is already stopped or removed is the protocol
		// racing itself, not a fault: expected, so it is refused without a report. Any other unauthorized frame still reports.
		return "unauthorized", agent.Revoked && agent.MachineID == c.id
	}
	if frame.TurnID != "" && (frame.Type == protocol.Ack || frame.Type == protocol.Activity || frame.Type == protocol.Usage || frame.Type == protocol.Compaction || frame.Type == protocol.NativeSessionStarted || frame.Type == protocol.Answer || frame.Type == protocol.Process) {
		owner, err := s.Store.TurnOwner(ctx, frame.TurnID)
		if err != nil || owner != "" && owner != c.journalID {
			return "forbidden", false
		}
	}
	if frame.Generation != generation || agent.Generation != generation {
		if frame.Type == protocol.Answer || frame.Type == protocol.NativeSessionStarted {
			if old, ok := c.journaled[frame.TurnID]; ok && old.agentID == frame.AgentID && old.generation == frame.Generation {
				return "", false
			}
		}
		// A retiring instance (the one the server holds as retiring, or an older build whose hello said restart-pending) speaks for an
		// agent the newer instance has already taken: its frames of an older generation are the swap racing itself, the same protocol
		// working a stale_rev ack is, and are refused without a report (AIMEBU_V2-Z). A stale generation from the active connection,
		// or one newer than the server knows, still reports: neither is explained by a retire.
		if frame.Generation <= generation && frame.Generation <= agent.Generation && s.connRetiring(c) {
			return "stale_generation", true
		}
		return "stale_generation", false
	}
	return "", false
}

// connRetiring says the connection is the machine's retiring instance. The flag is written by a newer instance's hello under machineMu
// while this connection's frames are being read on another goroutine, so the read takes the lock too. The caller holds no lock.
func (s *Server) connRetiring(c *machineConn) bool {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	return c.retiring
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
			if !s.controlAckOwner(ctx, c, frame.AgentID) {
				return nil
			}
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
		if strings.TrimSpace(frame.Candidate) == "" {
			// An empty answer says nothing about why: keep what the harness reported, so the next one can be diagnosed.
			exitCode, stderrTail, _ := s.Store.TurnExitEvidence(ctx, frame.TurnID)
			log.Printf("machine %s: empty answer: agent=%s turn=%s mode=%s exit_code=%s stderr_tail=%q", c.id, frame.AgentID, frame.TurnID, mode, exitCode, stderrTail)
		}
		receipt, err := s.Store.ApplyAnswer(ctx, input)
		if err == nil {
			// A silent turn's notice may have been written: start reading the end of the turn for the agents it wakes.
			s.prefetchNoticeOutputs(ctx)
		}
		if errors.Is(err, store.ErrConflict) {
			return c.send(protocol.Frame{V: protocol.Version, Type: protocol.Receipt, TurnID: frame.TurnID, AnswerRev: frame.AnswerRev, Digest: frame.Digest, Result: "conflict"})
		}
		if err != nil {
			return err
		}
		// One line per answered turn, so a turn that ended without a message (or was held) can be found in the server log without the database.
		heldReason := receipt.HeldReason
		if heldReason == "" {
			heldReason = "none"
		}
		log.Printf("machine %s: turn answered: agent=%s turn=%s mode=%s result=%s messages=%d held_reason=%s", c.id, frame.AgentID, frame.TurnID, mode, receipt.Result, len(receipt.MessageIDs), heldReason)
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
		exit := *frame.Exit
		if c.id == s.localSupervisorID() {
			exit = s.localSupervisorRestartExit(frame.TurnID, exit)
		}
		if err := s.Store.RecordProcessInfo(ctx, frame.TurnID, frame.AgentID, frame.Generation, exit); err != nil {
			return err
		}
		// A recovery may have been handed to the lead: start reading the end of the failed turn for it.
		s.prefetchNoticeOutputs(ctx)
		s.noteProviderLimit(ctx, frame.AgentID)
		if err := s.Store.ConfirmAgentProcessExit(ctx, frame.AgentID); err != nil {
			return err
		}
		return c.send(protocol.Frame{V: protocol.Version, Type: protocol.Ack, TurnID: frame.TurnID, Level: "process"})
	default:
		return errors.New("unknown agent frame")
	}
}

func (s *Server) controlAckOwner(ctx context.Context, c *machineConn, agentID string) bool {
	owner, err := s.Store.AgentOpenTurnOwner(ctx, agentID)
	if err != nil {
		return false
	}
	if owner != "" {
		return owner == c.journalID
	}
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	for _, retiring := range s.retiringMachines[c.id] {
		retiring.warmMu.Lock()
		warm := retiring.warm
		holds := false
		if warm != nil {
			for _, agent := range warm.Agents {
				if agent.AgentID == agentID {
					holds = true
					break
				}
			}
		}
		retiring.warmMu.Unlock()
		if holds {
			return retiring == c
		}
	}
	return s.machines[c.id] == c
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
	blocked, err := s.handoverBlocked(ctx, agent.MachineID, delivery.AgentID)
	if err != nil {
		return err
	}
	if blocked {
		return ErrMachineUnavailable
	}
	if agent.Tool == "codex" && s.codexWatch.Load() != nil {
		// Before a Codex turn starts on the host, the live login is looked at again: one a running session wrote back after a switch is put right
		// first, so the turn starts on the installed login.
		if host, err := s.Store.HostMachine(ctx); err == nil && host.ID == agent.MachineID {
			s.checkCodexOverwrite()
		}
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
	delivery.CodexWarmEnabled = s.codexWarmOn.Load()
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
	if err := s.grantHandoverKey(ctx, c, delivery.AgentID, delivery.Generation); err != nil {
		return err
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
	// A notice about a worker's turn carries the end of that turn's output; read before the turn is admitted, so a slow machine delays
	// only this delivery and never leaves an admitted turn unsent.
	s.addWorkerOutput(ctx, &delivery)
	noteIDs := make([]string, 0, len(delivery.Notes))
	for _, note := range delivery.Notes {
		noteIDs = append(noteIDs, note.NoteID)
	}
	if err := s.Store.AdmitTurn(ctx, store.Admission{TurnID: delivery.TurnID, AgentID: delivery.AgentID, MachineID: agent.MachineID, Generation: delivery.Generation, DesiredRev: delivery.DesiredRev, NoteIDs: noteIDs,
		HandoffOutcomeRequired: c.supportsHandoffOutcome, JournalID: c.journalID}); err != nil {
		return err
	}
	return c.send(protocol.DeliverFrame(delivery))
}

// maxProbeFolders bounds how many of a machine's project folders are handed to its supervisor to ask a container launcher from.
const maxProbeFolders = 8

// notifyWarmWorking refreshes agent cards when a live warm process starts a
// turn. The settled idle report precedes the process event, which refreshes
// the card at the other edge of the turn.
func (s *Server) notifyWarmWorking(ctx context.Context, c *machineConn, previous, current *protocol.WarmReport) {
	wasBusy := make(map[string]bool)
	if previous != nil {
		for _, agent := range previous.Agents {
			if agent.State == "busy" {
				wasBusy[agent.AgentID] = true
			}
		}
	}
	for _, agent := range current.Agents {
		if agent.State != "busy" || wasBusy[agent.AgentID] {
			continue
		}
		runtime, err := s.Store.AgentRuntimeByID(ctx, agent.AgentID)
		if err != nil || runtime.MachineID != c.id {
			continue
		}
		if err := s.Store.Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.AppendEvent(store.Event{Kind: "agent_warm_working", ProjectID: runtime.ProjectID, AgentID: runtime.ID})
			return err
		}); err != nil {
			telemetry.ReportWarning(ctx, "machine", fmt.Errorf("recording a warm process starting a turn: %w", err))
		}
	}
}

func (s *Server) machineForID(id string) (*machineConn, error) {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	c := s.machines[id]
	if c == nil {
		return nil, ErrMachineUnavailable
	}
	return c, nil
}

func (s *Server) machineConnections(id string) []*machineConn {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	var out []*machineConn
	if c := s.machines[id]; c != nil && !connectionClosed(c) {
		out = append(out, c)
	}
	for _, c := range s.retiringMachines[id] {
		if !connectionClosed(c) {
			out = append(out, c)
		}
	}
	return out
}

func (s *Server) sendMachineControl(id string, frame protocol.Frame) error {
	connections := s.machineConnections(id)
	if len(connections) == 0 {
		return ErrMachineUnavailable
	}
	var first error
	for _, c := range connections {
		c.boundMu.Lock()
		if c.bound == nil {
			c.bound = make(map[string]int)
		}
		c.bound[frame.AgentID] = frame.Generation
		c.boundMu.Unlock()
		if err := c.sendControl(frame); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Server) machineForOwner(id, journalID string) (*machineConn, error) {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	if c := s.machines[id]; c != nil && !connectionClosed(c) && (journalID == "" || c.journalID == journalID) {
		return c, nil
	}
	if c := s.retiringMachines[id][journalID]; c != nil && !connectionClosed(c) {
		return c, nil
	}
	return nil, ErrMachineUnavailable
}

func connectionClosed(c *machineConn) bool {
	if c.done == nil {
		return false
	}
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (s *Server) activeConnection(c *machineConn) bool {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	return s.machines[c.id] == c
}

// handoverBlocked prevents a second process for an agent while an old instance still owns its turn or warm process.
func (s *Server) handoverBlocked(ctx context.Context, machineID, agentID string) (bool, error) {
	owner, err := s.Store.AgentOpenTurnOwner(ctx, agentID)
	if err != nil {
		return false, err
	}
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	if owner != "" && (s.machines[machineID] == nil || owner != s.machines[machineID].journalID) {
		return true, nil
	}
	for journalID, c := range s.retiringMachines[machineID] {
		if owner == journalID {
			return true, nil
		}
		c.warmMu.Lock()
		warm := c.warm
		if warm != nil {
			for _, agent := range warm.Agents {
				if agent.AgentID == agentID {
					c.warmMu.Unlock()
					return true, nil
				}
			}
		}
		c.warmMu.Unlock()
	}
	return false, nil
}

func (s *Server) machineForDispatch(id string) (*machineConn, error) {
	s.machineMu.Lock()
	c := s.machines[id]
	if c != nil && c.conn != nil && !c.helloAcked.Load() && c.pendingRetire != nil && !connectionClosed(c.pendingRetire) {
		c = c.pendingRetire
	}
	s.machineMu.Unlock()
	if c == nil {
		return nil, ErrMachineUnavailable
	}
	if connectionClosed(c) {
		return nil, ErrMachineUnavailable
	}
	if c.conn != nil && !c.helloAcked.Load() {
		return nil, ErrMachineUnavailable
	}
	return c, nil
}
