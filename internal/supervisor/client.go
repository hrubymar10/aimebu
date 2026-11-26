package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/agentlog"
	"github.com/hrubymar10/aimebu/v2/internal/answer"
	"github.com/hrubymar10/aimebu/v2/internal/pathlist"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
	"github.com/hrubymar10/aimebu/v2/internal/tlsclient"
	"github.com/hrubymar10/aimebu/v2/internal/worktree"
)

type Client struct {
	MachineID  string
	MachineKey string
	ServerURL  string
	// AllowPlainHTTP lets this supervisor reach a server on another machine over plain http (the --insecure flag); without it only https, or this machine, is used.
	AllowPlainHTTP bool
	// OnTelemetry takes the server's telemetry settings (in hello_ack, and again whenever they change). Nil means this supervisor does not take them: it
	// then does not announce the capability.
	OnTelemetry             func(protocol.TelemetrySettings)
	HTTPClient              *http.Client
	SupervisorVersion       string
	EnvironmentFingerprint  string
	CapabilityReports       []protocol.CapabilityReport
	Harnesses               []protocol.HarnessRow
	Launchers               []protocol.LauncherRow
	harnessProbeDir         string
	harnessProbeMore        []string
	launcherReprobeInterval time.Duration
	codexHomeRetries        map[string]codexHomeRetry
	reprobeRunning          bool
	reprobeDone             chan struct{}
	// ConfigDir is the machine's aimebu config root; model detection uses a private detect/run-* folder unless the request names a folder.
	ConfigDir string
	// ClaudeConfigDir is where Claude Code keeps its sessions for a session scan; empty means CLAUDE_CONFIG_DIR, else ~/.claude (tests set it).
	ClaudeConfigDir string
	Journal         *Journal
	// AgentLogs holds what each agent's harness wrote to stderr, on this machine (supervisor/agent-logs/ beside the journal). The server
	// reads a bounded tail of it for an owner or admin; nothing else leaves the machine.
	AgentLogs *agentlog.Store
	// conversations are the conversation logs of the running turns, by turn id (*turnConversation).
	conversations  sync.Map
	Adapter        adapter.Adapter
	PingInterval   time.Duration
	TurnLimits     TurnLimitOverrides
	ReconnectDelay time.Duration
	// IdleDisconnectLimit lets a detached supervisor retire after the server has
	// been absent for a long time. Zero keeps the standalone behavior.
	IdleDisconnectLimit time.Duration
	lastDisconnected    atomic.Int64
	// NoTurnInterrupt makes the client behave like a build from before turn_interrupt: it does not announce the frame and does not act
	// on one. Tests use it to stand in for the older supervisor a newer server finds running after an upgrade.
	NoTurnInterrupt bool
	// NoAttachmentSave makes the client behave like a build from before attachment_save, for the same kind of test.
	NoAttachmentSave bool
	retireRequested  atomic.Bool
	retired          atomic.Bool // Run ended because a retire request found nothing owed (FinishRetire removes the journal then)
	// DrainOnly marks an instance that only finishes the work a dead predecessor left in its journal: hello says so, so the server sends it nothing
	// new, and it retires as soon as its connection is up.
	DrainOnly bool
	// disconnectedDrops counts the sends that failed for want of a connection since the last one: the first is logged, the rest only
	// counted and summed up in one line when the connection is back (logDisconnectedDrops).
	disconnectedDrops atomic.Int64
	// unknownStarts logs a turn whose process lookup could not tell once per process run (the settlement runs once per start or drain
	// hello); the map never grows beyond the turns it named.
	unknownStarts      sync.Map
	shutdownRequested  atomic.Bool
	restartInterrupted atomic.Bool
	OnError            func(error)
	OnReady            func()
	writeMu            sync.Mutex
	detectMu           sync.Mutex
	mu                 sync.Mutex
	desired            map[string]protocol.DesiredAgent
	active             map[string]string
	running            map[string]adapter.Turn
	pendingStopAck     map[string]protocol.Frame
	activeTurns        map[string]activeTurn     // the turn each agent was last admitted for (see turn_interrupt.go)
	turnInterrupts     map[string]*turnInterrupt // the interrupt asked for an agent's running turn
	agentKeys          map[string]string         // each agent's key, in memory only (see agent_tools.go)
	telemetryEnv       map[string]string         // the reporting variables the agent-tools bridge is started with (SetTelemetryEnv)
	currentConn        *websocket.Conn
	stopping           bool
	// refusedSettled holds the exits a terminal refusal (stale_generation, already_terminal) already settled, so the one log line per turn stays one across repeated
	// refusals of the same turn's frames.
	refusedSettled map[string]bool
	turns          sync.WaitGroup
	// unreported counts the turns that left admission (c.active) but whose process frame was not sent yet: a supervisor retiring at idle
	// waits for them too, or it would close the socket between a turn's release and its exit (see runTurnRestartable).
	unreported atomic.Int64
	// Warm keeps an agent's harness process up for an idle window after a turn (see warm.go); the zero value is off.
	Warm             WarmConfig
	codexWarmEnabled bool
	warmWarnings     map[string]string               // last warm refusal per agent, guarded by mu
	warmRefusals     map[string]protocol.WarmRefusal // last safe card reason per agent, guarded by mu
	// FreeMemory reports free and total memory for the warm memory guard; nil reads the system's (tests set it).
	FreeMemory func() (free, total int64, err error)
	// FreeDockerMemory reports the containers' use and the Docker host's total for the guard; nil asks docker (tests set it).
	FreeDockerMemory func() (used, total int64, err error)
	// ContainerState reports the state of the container with an ID ("" when Docker knows none) for a warm process whose end its launcher did
	// not confirm; nil asks docker (tests set it).
	ContainerState func(id string) (state string, err error)
	// ProcessStart reports a process's own start time for the warm journal; nil asks the system (tests set it).
	ProcessStart func(pid int) string
	// ProcessStartState reports a process's own start with three outcomes (StartKnown with the start string, StartGone, StartUnknown) for
	// the dead-turn settlement; nil asks the system (tests set it).
	ProcessStartState func(pid int) (ProcessStartState, string)
	// CommandLogger resolves, at each job's start, the absolute path of the program that writes a long command's log as a process of the
	// command's own (`aimebu command-log`, see CommandLogMain): set, a native command runs in a session of its own and writes its log and
	// exit marker without this process, so it survives this supervisor's exit and its replacement adopts it. Nil (or an error), the
	// output passes through this process as before.
	CommandLogger   func() (string, error)
	warmPool        *warmPool
	warmReports     chan struct{} // a change of the warm picture, to be reported
	warmReportMu    sync.Mutex    // build and send warm reports in sequence, including turn-end reports
	warmEndsMu      sync.Mutex
	warmEnds        map[string][]time.Time // when each agent's idle warm process last ended by itself, to escalate repeats within the hour
	warmEndClock    func() time.Time       // tests set it; nil means time.Now
	kaMu            sync.Mutex
	ka              map[string]*keepAliveEntry               // each agent's cache keep-alive (see keepalive.go)
	kaPause         keepAlivePause                           // what the server says to hold pings back for
	keepAliveSink   func(protocol.KeepAliveReport)           // tests see each ping's report here
	keepAliveScope  func(agentID, gen string, on bool) error // tests replace the server's answer to a ping's scope request (see keepAliveScopeCall)
	kaScopeMu       sync.Mutex
	kaStuck         map[string]chan struct{}                               // a ping that was told to end and is not confirmed gone yet, by agent (guarded by kaMu)
	kaScopePending  map[string]chan struct{}                               // scope requests waiting for the server's ack, by request id
	kaOwed          map[string]string                                      // agents whose ping's scope lift was not answered and is owed again, with the ping it is for (guarded by kaScopeMu)
	kaScopeLocks    map[string]*sync.Mutex                                 // one scope transition at a time per agent, held across the frame and its ack (guarded by kaScopeMu)
	kaUnconfirmed   atomic.Int64                                           // pings whose end was never confirmed and whose quarantine ran out (see keepAliveQuarantine)
	kaQuarantine    time.Duration                                          // tests shorten keepAliveQuarantine here; zero is the default
	warmSeq         atomic.Uint64                                          // the one counter that orders what the supervisor says of its running processes
	warmLive        atomic.Int64                                           // warm processes alive (counted from the start of one until its end is confirmed)
	warmClaim       func(ctx context.Context, seq uint64, count int) error // tests replace the server's answer to a warm claim (see claimWarm)
	warmEnded       func(agentID, reason string)                           // tests see the real end of a kept warm process here (see noteWarmEnded)
	processSink     func(turnID string, exit adapter.ExitInfo)             // tests see each turn's exit as it is sent to the server
	commands        *commandRunner                                         // long commands this supervisor runs outside turns (see commands.go)
	commandSink     func(protocol.CommandExit)                             // tests see each command's end as it is sent to the server
	kaRenewEvery    time.Duration                                          // tests shorten protocol.KeepAliveScopeRenewEvery here; zero is the default
	kaWaitTerminate time.Duration                                          // tests shorten keepAliveWaitTerminate here; zero is the default
	warmNow         func() time.Time
	warmAfter       func(time.Duration, func()) *time.Timer
	warmStuckWait   time.Duration        // tests shorten warmStuckWaitLimit here; zero is the default
	warmStuckPoll   time.Duration        // tests shorten warmStuckPollEvery here; zero is the default
	backgroundEnded func(agentID string) // tests see a harness's own background query that ended between turns here (see noteBackgroundEnded)
}

func NewClient(machineID, machineKey, serverURL string, journal *Journal, implementation adapter.Adapter) *Client {
	client := &Client{MachineID: machineID, MachineKey: machineKey, ServerURL: serverURL, HTTPClient: tlsclient.NewHTTPClient(0), SupervisorVersion: "aimebu-v2", Journal: journal, Adapter: implementation, PingInterval: 15 * time.Second, ReconnectDelay: time.Second, launcherReprobeInterval: time.Minute, desired: make(map[string]protocol.DesiredAgent), active: make(map[string]string), running: make(map[string]adapter.Turn), commands: newCommandRunner()}
	for _, item := range implementation.Catalog() {
		client.CapabilityReports = append(client.CapabilityReports, protocol.CapabilityReport{Tool: item.Tool, Version: item.Version, Environment: item.Environment, Flags: implementation.Capabilities(item.Version, item.Environment)})
	}
	client.Harnesses = defaultHarnesses()
	client.Warm = WarmConfigFromEnv()
	client.warmReports = make(chan struct{}, 1)
	if journal != nil && journal.path != "" {
		client.AgentLogs = agentlog.New(filepath.Join(filepath.Dir(journal.path), "agent-logs"))
	}
	return client
}

// errDockerOnly is what a supervisor says when the instance runs agents only in docker and the agent's launcher is native.
var errDockerOnly = errors.New("Only run agents in docker is on for this instance (Settings -> General), so a native launcher is refused: use a Docker launcher, or ask an admin to turn the setting off")

func (c *Client) refreshCapabilities() {
	fromRows, ok := c.Adapter.(interface {
		CapabilitiesFor(string, string, string, adapter.Environment) adapter.Capabilities
	})
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Launchers) > 0 {
		reports := make([]protocol.CapabilityReport, 0, len(c.Launchers)+1)
		for _, row := range c.Launchers {
			if row.Version == "" {
				continue
			}
			underlying := "native"
			osName := runtime.GOOS
			if row.Containerized {
				underlying, osName = "docker", "linux"
			}
			base := adapter.Environment{Variant: underlying, OS: osName, Container: row.Containerized}
			alias := row.Name
			if alias == "harness-docker" {
				alias = "docker"
			}
			reports = append(reports, protocol.CapabilityReport{Tool: row.Tool, Version: row.Version,
				Environment: adapter.Environment{Variant: alias, OS: osName, Container: row.Containerized},
				Flags:       fromRows.CapabilitiesFor(row.Tool, underlying, row.Version, base)})
		}
		for _, report := range c.CapabilityReports {
			if report.Tool == "fake" {
				reports = append(reports, report)
			}
		}
		c.CapabilityReports = reports
		return
	}
	reports := make([]protocol.CapabilityReport, 0, len(c.Harnesses))
	listed := make(map[string]bool, len(c.Harnesses))
	for _, row := range c.Harnesses {
		listed[harnessKey(row.Tool, row.Variant)] = true
		if row.Version == "" {
			continue
		}
		environment := adapter.Environment{Variant: row.Variant, OS: runtime.GOOS, Container: row.Variant == "docker"}
		if environment.Container {
			environment.OS = "linux"
		}
		reports = append(reports, protocol.CapabilityReport{Tool: row.Tool, Version: row.Version, Environment: environment,
			Flags: fromRows.CapabilitiesFor(row.Tool, row.Variant, row.Version, environment)})
	}
	for _, report := range c.CapabilityReports {
		if !listed[harnessKey(report.Tool, report.Environment.Variant)] {
			reports = append(reports, report)
		}
	}
	c.CapabilityReports = reports
}

func (c *Client) capabilityReportsSnapshot() []protocol.CapabilityReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.CapabilityReport(nil), c.CapabilityReports...)
}

// reconnectHealthy is how long a connection must have lasted to count as one that worked; reconnectPersistent is how many failed attempts in a row are
// reported.
const (
	reconnectHealthy    = 30 * time.Second
	reconnectPersistent = 5
)

// reconnectOutage is how long a server unreachable (with the failures in a row) must last to be reported; a shorter outage only reaches the local log.
var reconnectOutage = 10 * time.Minute

func (c *Client) Run(ctx context.Context) error {
	c.SweepLegacyKeys()
	c.SweepWarmOrphans(ctx)
	// The turns the previous holder of this journal left running are settled before the first hello, each one only once its recorded
	// process identity has been checked (see settleDeadTurns): a hello never reports a dead predecessor's turn as running when the
	// journal can vouch for its process. Rows without an identity stay for the drain path, which settles them as it always has.
	if n, err := c.settleDeadTurns(ctx, false); err != nil {
		return fmt.Errorf("settle the turns the previous supervisor left running: %w", err)
	} else if n > 0 {
		log.Printf("supervisor startup: settled %d turn(s) whose supervisor died", n)
	}
	telemetry.Go(ctx, func() { c.sweepAgentLogs(ctx) })
	telemetry.Go(ctx, func() { c.warmGuard(ctx) })
	telemetry.Go(ctx, func() { c.pruneJournal(ctx) })
	failures := 0
	var outageSince time.Time
	outageReported := false
	started := time.Now()
	for {
		began := time.Now()
		err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if c.retireRequested.Load() && !c.retireOwes() {
			c.retired.Store(true)
			return nil
		}
		if c.IdleDisconnectLimit > 0 {
			last := started
			if stamp := c.lastDisconnected.Load(); stamp != 0 {
				last = time.Unix(0, stamp)
			}
			if time.Since(last) >= c.IdleDisconnectLimit && !c.hasActiveWork() {
				return nil
			}
		}
		// A connection that drops now and then is the network; one that cannot be had again and again is a fault worth a report (once a day, like every cause).
		if time.Since(began) > reconnectHealthy {
			failures = 0
		}
		failures++
		c.reportLocal(err)
		// A server that is down for a while (a deliberate stop, a deploy) is the network's business and only logged; one that stays unreachable past
		// reconnectOutage, with the failures in a row, is reported.
		if outageSince.IsZero() {
			outageSince = began
		}
		if failures == 1 && time.Since(began) > reconnectHealthy {
			outageSince, outageReported = time.Now(), false // a connection that worked ended: a new outage starts now
		}
		if failures >= reconnectPersistent && time.Since(outageSince) >= reconnectOutage && !outageReported {
			outageReported = true
			telemetry.ReportWarning(ctx, "supervisor", fmt.Errorf("the server connection failed %d times in a row: %w", failures, err))
		}
		delay := c.reconnectDelay()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// MarkRestartInterruption distinguishes an owner-requested immediate
// supervisor replacement from an ordinary stop of an agent. The affected
// turns get the existing one-time interrupted-turn continuation.
func (c *Client) MarkRestartInterruption() {
	c.restartInterrupted.Store(true)
}

func (c *Client) RunOnce(ctx context.Context) error {
	parsed, err := url.Parse(c.ServerURL)
	if err != nil {
		return err
	}
	if err := tlsclient.CheckTransport(c.ServerURL, c.AllowPlainHTTP); err != nil {
		return err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return fmt.Errorf("invalid server URL scheme")
	}
	parsed.Path = "/api/machine"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	header := http.Header{}
	header.Set("X-Machine-ID", c.MachineID)
	header.Set("Authorization", "Bearer "+c.MachineKey)
	conn, _, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{HTTPHeader: header, HTTPClient: c.HTTPClient})
	if err != nil {
		return err
	}
	defer func() { c.lastDisconnected.Store(time.Now().UnixNano()) }()
	defer func() {
		if ctx.Err() != nil {
			_ = conn.CloseNow()
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	probeCtx, cancelProbes := context.WithCancel(ctx)
	defer func() {
		cancelProbes()
		c.mu.Lock()
		done := c.reprobeDone
		c.mu.Unlock()
		if done != nil {
			<-done
		}
	}()
	conn.SetReadLimit(protocol.MaxMachineFrameBytes)
	agents, err := c.Journal.OpenTurns(ctx)
	if err != nil {
		return err
	}
	openCommands, err := c.Journal.OpenCommands(ctx)
	if err != nil {
		return err
	}
	commandExits, err := c.Journal.PendingCommandExits(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	for i := range agents {
		for j := range agents[i].OpenTurns {
			agents[i].OpenTurns[j].ProcessAlive = c.active[agents[i].AgentID] == agents[i].OpenTurns[j].TurnID
		}
	}
	c.mu.Unlock()
	if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: c.MachineID, JournalID: c.Journal.ID, DrainOnly: c.DrainOnly, SupervisorVersion: c.SupervisorVersion, SupervisorEnvironment: c.EnvironmentFingerprint, SupportsRetireAtIdle: true, SupportsPathProbe: true, SupportsDirectoryList: true, SupportsSessionScan: true, SupportsSessionFork: true, SupportsSessionCopy: true, SupportsLegacySessionCopy: true, SupportsSessionAttribution: true, SupportsLaunchPath: true, SupportsWorktree: true, SupportsLaunchers: true, SupportsProviders: true, SupportsDetect: true, SupportsDetectDir: true, SupportsAgentLogs: c.AgentLogs != nil, SupportsAgentLogSize: c.AgentLogs != nil, SupportsAgentConversationLog: c.AgentLogs != nil, SupportsStopReplay: true, SupportsTurnInterrupt: !c.NoTurnInterrupt, SupportsAttachmentSave: !c.NoAttachmentSave, SupportsTelemetryConfig: c.OnTelemetry != nil, SupportsAgentKeys: true, SupportsAgentKeysFrame: true, SupportsCommands: true, SupportsContainerCommands: true, SupportsHandoffOutcome: true, SupportsWarm: c.warmCapable(), HeldKeys: c.heldKeys(), Capabilities: c.capabilityReportsSnapshot(), Harnesses: c.Harnesses, Agents: agents, CommandOpen: openCommands, CommandExits: commandExits}); err != nil {
		return err
	}
	ack, err := readFrame(ctx, conn)
	if err != nil {
		return err
	}
	if ack.V != protocol.Version || ack.Type != protocol.HelloAck {
		return fmt.Errorf("expected hello_ack, got %q", ack.Type)
	}
	c.logDisconnectedDrops()
	// A retire request belongs to the previous server connection. The new
	// server sends another retire frame after this ack if it still needs one.
	if !c.DrainOnly && c.retireRequested.Swap(false) && !c.shutdownRequested.Load() {
		c.mu.Lock()
		c.stopping = false
		c.mu.Unlock()
	}
	// New keys first, before anything that could start a turn: an agent that lost its key with a restart gets another here.
	c.applyAgentKeys(ack.AgentKeys)
	c.mu.Lock()
	c.harnessProbeDir = ack.HarnessProbeDir
	c.harnessProbeMore = ack.HarnessProbeDirs
	// The server's definitions are authoritative after reconnect. Dropped
	// launchers must not remain selectable in this long-running process.
	c.Launchers = nil
	rows := append([]protocol.HarnessRow(nil), c.Harnesses...)
	c.mu.Unlock()
	for _, definition := range ack.LauncherDefinitions {
		if err := c.setLauncher(definition); err != nil {
			c.report("a launcher definition from the server was not applied", err)
		}
	}
	if ack.HarnessProbeDir != "" || len(ack.HarnessProbeDirs) > 0 {
		for _, row := range rows {
			if row.Variant == "docker" {
				if err := c.setHarness(row.HarnessSetting); err != nil {
					c.report("a harness setting was not applied", err)
				}
			}
		}
	}
	for _, setting := range ack.HarnessSettings {
		if err := c.setHarness(setting); err != nil {
			c.report("a harness setting from the server was not applied", err)
		}
	}
	if ack.Telemetry != nil && c.OnTelemetry != nil {
		c.OnTelemetry(*ack.Telemetry)
	}
	c.refreshCapabilities()
	if len(ack.LauncherDefinitions) > 0 {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()}); err != nil {
			return err
		}
	}
	if len(ack.HarnessSettings) > 0 || ack.HarnessProbeDir != "" || len(ack.HarnessProbeDirs) > 0 {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus, Harnesses: c.Harnesses, Capabilities: c.capabilityReportsSnapshot()}); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.currentConn = conn
	stoppedTurns := []adapter.Turn{}
	for _, item := range ack.Desired {
		c.desired[item.AgentID] = item
		if item.Desired == "stopped" && c.running[item.AgentID] != nil {
			stoppedTurns = append(stoppedTurns, c.running[item.AgentID])
		}
	}
	listed := make(map[string]bool, len(ack.Desired))
	for _, item := range ack.Desired {
		listed[item.AgentID] = item.Desired != "stopped"
	}
	for id := range c.warmRefusals {
		if !listed[id] {
			delete(c.warmRefusals, id)
		}
	}
	c.mu.Unlock()
	c.startLauncherReprobe(probeCtx)
	if c.warmCapable() {
		telemetry.Go(probeCtx, func() { c.sendWarmReports(probeCtx) })
		c.warmChanged() // the picture at connect: what a server that restarted does not know
	}
	// A ping's scope lift the last connection lost is owed: sent again now, so the agent's toolbox is not refused for as long as the server's own limit.
	telemetry.Go(probeCtx, func() { _ = c.keepAliveSettleLifts(probeCtx, "") })
	for _, turn := range stoppedTurns {
		_ = turn.Cancel()
	}
	defer func() {
		c.mu.Lock()
		if c.currentConn == conn {
			c.currentConn = nil
		}
		c.mu.Unlock()
	}()
	for _, receipt := range ack.ResendReceipts {
		if err := c.Journal.RecordReceipt(ctx, receipt); err != nil {
			return err
		}
	}
	sessions, err := c.Journal.NativeSessionEvents(ctx)
	if err != nil {
		c.replayReadFailed(ctx, err)
		return err
	}
	for _, session := range sessions {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.NativeSessionStarted,
			AgentID: session.AgentID, Generation: session.Generation, TurnID: session.TurnID,
			SourceEventID: session.SourceEventID, NativeSessionHandle: session.Handle}); err != nil {
			return err
		}
	}
	// Usage and compaction sent while the socket was down were lost. Replay
	// them before the exits so the server's process acknowledgement still
	// follows every event of the turn.
	usage, err := c.Journal.PendingUsageEvents(ctx)
	if err != nil {
		c.replayReadFailed(ctx, err)
		return err
	}
	for _, pending := range usage {
		if err := c.send(ctx, conn, turnEventFrame(pending.AgentID, pending.Generation, pending.TurnID, pending.Event)); err != nil {
			return err
		}
	}
	if c.DrainOnly {
		// The holder that ran these turns is dead and a drain instance starts no process: a turn its journal still lists as running is
		// settled here, each one with its recorded process identity checked first (an orphan is terminated, a row without an identity is
		// ended as the drain always did). Ending them is what lets the journal become empty.
		if n, err := c.settleDeadTurns(ctx, true); err != nil {
			c.replayReadFailed(ctx, err)
			return err
		} else if n > 0 {
			log.Printf("supervisor drain: ended %d turn(s) whose supervisor died", n)
		}
	}
	exits, err := c.Journal.PendingExits(ctx)
	if err != nil {
		c.replayReadFailed(ctx, err)
		return err
	}
	for _, exit := range exits {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: exit.AgentID, Generation: exit.Generation, TurnID: exit.TurnID, Exit: &exit.Exit}); err != nil {
			return err
		}
	}
	for _, exit := range commandExits {
		ending := exit
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.CommandEnded, AgentID: exit.AgentID, Generation: exit.Generation, JobID: exit.JobID, CommandExit: &ending}); err != nil {
			return err
		}
	}
	// The commands of a dead predecessor are watched until their end is known: one still running is not this process's child, so its exit
	// marker (or its disappearance) is the only proof of an end.
	c.reconcileCommands(ctx)
	pending, err := c.Journal.PendingAnswers(ctx)
	if err != nil {
		c.replayReadFailed(ctx, err)
		return err
	}
	for _, answer := range pending {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Answer, AgentID: answer.AgentID, Generation: answer.Generation, TurnID: answer.TurnID, AnswerRev: answer.AnswerRev, Digest: answer.Digest, Candidate: string(answer.Candidate)}); err != nil {
			return err
		}
	}
	for _, delivery := range ack.Redeliver {
		if err := c.handleDeliver(ctx, conn, delivery); err != nil {
			return err
		}
	}
	if c.OnReady != nil {
		c.OnReady()
	}
	if c.DrainOnly {
		c.beginRetire(ctx, conn, probeCtx)
	}
	interval := c.PingInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
		defer telemetry.Recover(ctx)
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if c.send(pingCtx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Ping, MachineID: c.MachineID}) != nil {
					return
				}
			}
		}
	}()
	for {
		frame, err := readFrame(ctx, conn)
		if err != nil {
			return err
		}
		if frame.V != protocol.Version {
			return fmt.Errorf("unsupported protocol version %d", frame.V)
		}
		switch frame.Type {
		case protocol.RetireAtIdle:
			c.beginRetire(ctx, conn, probeCtx)
		case protocol.AgentKeys:
			c.applyAgentKeys(frame.AgentKeys)
		case protocol.TurnInterrupt:
			if c.NoTurnInterrupt {
				continue // a build from before the frame does not know its type
			}
			// Cutting a turn only signals its process (a terminate in a container runs in its own goroutine), so it is answered on this loop.
			result := protocol.Frame{V: protocol.Version, Type: protocol.TurnInterruptResult, ReqID: frame.ReqID, AgentID: frame.AgentID, Generation: frame.Generation, TurnID: frame.TurnID, Result: c.handleTurnInterrupt(frame)}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.AttachmentSave:
			if c.NoAttachmentSave {
				continue // a build from before the frame does not know its type
			}
			// The save downloads the file from the server, so it runs off the read loop.
			go func(request protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := c.saveAttachment(probeCtx, request)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report("an attachment save answer could not be sent", err)
				}
			}(frame)
		case protocol.WorktreeOp:
			result := protocol.Frame{V: protocol.Version, Type: protocol.WorktreeResult, ReqID: frame.ReqID}
			var info worktree.Result
			var opErr error
			opCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			switch frame.WorktreeAction {
			case "validate":
				opErr = worktree.Validate(opCtx, frame.CheckoutPath)
			case "ensure":
				info, opErr = worktree.Ensure(opCtx, frame.CheckoutPath, frame.RoomID, frame.BaseRef)
			case "remove":
				info, opErr = worktree.Remove(opCtx, frame.CheckoutPath, frame.RoomID, frame.BaseRef, frame.WorktreeForce)
			case "inspect":
				info, opErr = worktree.Inspect(opCtx, frame.CheckoutPath, frame.RoomID, frame.BaseRef)
			case "retire":
				info, opErr = worktree.Retire(opCtx, frame.CheckoutPath, frame.RoomID)
			default:
				opErr = errors.New("unknown worktree action")
			}
			cancel()
			if opErr != nil {
				result.Code, result.Message = "worktree_failed", opErr.Error()
				var gitErr *worktree.Error
				if errors.As(opErr, &gitErr) {
					result.Code = gitErr.Code
				}
			} else {
				result.Details, _ = json.Marshal(info)
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.AgentLogRead:
			result := protocol.Frame{V: protocol.Version, Type: protocol.AgentLogResult, ReqID: frame.ReqID, AgentID: frame.AgentID}
			if c.AgentLogs == nil {
				result.Code, result.Message = "no_log", "this machine keeps no agent output"
			} else if stream, err := agentlog.ParseStream(frame.LogStream); err != nil {
				result.Code, result.Message = "invalid_stream", "this supervisor keeps no such output stream"
			} else if page, err := c.AgentLogs.TailStream(frame.AgentID, stream, frame.LogLines, frame.LogMaxBytes); errors.Is(err, agentlog.ErrNoLog) {
				result.Code, result.Message = "no_log", "no output has been captured for this agent on this machine"
			} else if err != nil {
				result.Code, result.Message = "log_failed", "the output could not be read"
				c.report("an agent output log could not be opened", fmt.Errorf("agent %s output log: %w", frame.AgentID, err))
			} else {
				result.Details, _ = json.Marshal(page)
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.AgentLogUsage:
			result := protocol.Frame{V: protocol.Version, Type: protocol.AgentLogUsageResult, ReqID: frame.ReqID}
			if c.AgentLogs == nil {
				result.Code, result.Message = "no_log", "this machine keeps no agent output"
			} else {
				bytes, agents := c.AgentLogs.Usage()
				result.Details, _ = json.Marshal(agentlog.Size{Bytes: bytes, Agents: agents})
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.SessionScan:
			// A scan reads files, never the whole of them; it still runs off the read loop, like a path probe.
			go func(scan protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := c.sessionScanResult(scan)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report("a probe result could not be sent to the server", err)
				}
			}(frame)
		case protocol.RetireWarm:
			// Ending a process can take its grace; keep reading frames meanwhile.
			go func(request protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := protocol.Frame{V: protocol.Version, Type: protocol.RetireWarmResult, ReqID: request.ReqID, Result: "ok"}
				if !c.RetireWarmTool(request.RetireTool, "an account switch") {
					result.Result, result.Message = "error", "a warm process of the tool is not confirmed gone"
				}
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report("a probe result could not be sent to the server", err)
				}
			}(frame)
		case protocol.PathProbe:
			// Launcher version checks can take up to a minute. Keep reading
			// machine frames while the probe runs; send serializes writes.
			go func(probe protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := c.pathProbeResult(probeCtx, probe)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report("a probe result could not be sent to the server", err)
				}
			}(frame)
		case protocol.Detect:
			// Looking around the machine can take a while; keep reading frames meanwhile.
			go func(request protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := c.detectResult(probeCtx, request)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report("a probe result could not be sent to the server", err)
				}
			}(frame)
		case protocol.DirectoryList:
			go func(request protocol.Frame) {
				defer telemetry.Recover(ctx)
				result := protocol.Frame{V: protocol.Version, Type: protocol.DirectoryListResult, ReqID: request.ReqID,
					DirectoryNames: pathlist.Directories(request.CheckoutPath, request.DirectoryPrefix)}
				if probeCtx.Err() == nil {
					if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
						c.report("a probe result could not be sent to the server", err)
					}
				}
			}(frame)
		case protocol.HarnessSet:
			result := protocol.Frame{V: protocol.Version, Type: protocol.HarnessResult, ReqID: frame.ReqID}
			if frame.HarnessSetting == nil {
				result.Code = "invalid_request"
				result.Message = "missing harness setting"
			} else if err := c.setHarness(*frame.HarnessSetting); err != nil {
				result.Code = "invalid_path"
				result.Message = err.Error()
			} else {
				key := harnessKey(frame.HarnessSetting.Tool, frame.HarnessSetting.Variant)
				c.mu.Lock()
				row := c.harnessRow(key)
				c.mu.Unlock()
				result.Harness = &row
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.LauncherValidate, protocol.LauncherSet:
			result := protocol.Frame{V: protocol.Version, Type: protocol.LauncherResult, ReqID: frame.ReqID}
			if frame.LauncherDefinition == nil {
				result.Code, result.Message = "invalid_request", "missing launcher definition"
			} else if frame.Type == protocol.LauncherValidate {
				dirs := c.probeDirs()
				var first string
				var more []string
				if len(dirs) > 0 {
					first, more = dirs[0], dirs[1:]
				}
				if row, err := inspectLauncher(*frame.LauncherDefinition, first, more...); err != nil {
					result.Code, result.Message = "invalid_launcher", err.Error()
				} else {
					row = c.launcherRowForReport(row)
					result.Launcher = &row
				}
			} else if err := c.setLauncher(*frame.LauncherDefinition); err != nil {
				result.Code, result.Message = "invalid_launcher", err.Error()
			} else {
				row := c.launcherRowForReport(c.launcherRow(frame.LauncherDefinition.ID))
				result.Launcher = &row
				c.refreshCapabilities()
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
			if result.Launcher != nil && frame.Type == protocol.LauncherSet {
				if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus,
					Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()}); err != nil {
					return err
				}
				c.startLauncherReprobe(probeCtx)
			}
		case protocol.Spawn:
			result := c.applySpawn(frame)
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
			if result.Result == "ok" && frame.Agent != nil && frame.Agent.LauncherID != "" {
				row := c.launcherRow(frame.Agent.LauncherID)
				c.mu.Lock()
				c.harnessProbeDir = frame.CheckoutPath
				c.mu.Unlock()
				if err := c.setLauncher(row.LauncherDefinition); err == nil {
					c.refreshCapabilities()
					if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus,
						Launchers: c.launcherRows(), Capabilities: c.capabilityReportsSnapshot()}); err != nil {
						return err
					}
					c.startLauncherReprobe(probeCtx)
				}
			} else if result.Result == "ok" && frame.Agent != nil && frame.Agent.Variant == "docker" {
				c.mu.Lock()
				c.harnessProbeDir = frame.CheckoutPath
				row := c.harnessRow(harnessKey(frame.Agent.Tool, "docker"))
				c.mu.Unlock()
				if err := c.setHarness(row.HarnessSetting); err == nil {
					c.refreshCapabilities()
					if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus,
						Harnesses: c.Harnesses, Capabilities: c.capabilityReportsSnapshot()}); err != nil {
						return err
					}
				}
			}
		case protocol.Control:
			if c.handleControl(frame) {
				continue
			}
			if err := c.send(ctx, conn, controlAck(frame)); err != nil {
				return err
			}
		case protocol.Ping:
			if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Pong, MachineID: c.MachineID}); err != nil {
				return err
			}
		case protocol.Pong:
		case protocol.KeepAlivePausedFrame:
			c.applyKeepAlivePaused(frame.PausedTools, frame.PausedAgents)
		case protocol.AgentSettingsFrame:
			// Ending idle processes can take their grace: not on the loop that reads frames. A panic in it is reported
			// (when telemetry is on) and raised again, so the process dies as it always did (telemetry.Go).
			telemetry.Go(ctx, func() { c.applyAgentSettings(frame.WarmIdleSeconds, frame.CodexWarmEnabled, frame.KeepAliveByTool) })
		case protocol.KeepAliveScopeAckFrame, protocol.WarmClaimAckFrame:
			c.keepAliveScopeAcked(frame.ReqID) // both are answers to a request waiting by its id
		case protocol.TelemetryConfig:
			if frame.Telemetry != nil && c.OnTelemetry != nil {
				c.OnTelemetry(*frame.Telemetry)
			}
		case protocol.Deliver:
			if delivery := frame.Delivery(); c.keepAliveBusy(delivery.AgentID) || c.warmStuck(delivery.AgentID) {
				// A ping of the agent runs (or was not confirmed gone), or its last warm process is not confirmed gone (a turn-limit kill in a
				// container): the delivery waits for it to end, which can take as long as a container's cleanup or the warm quarantine, so it does
				// that in a goroutine of its own and the loop goes on reading frames (pings, controls, a stop, the keep-alive pause).
				go func() {
					defer telemetry.Recover(ctx)
					if err := c.handleDeliver(ctx, conn, delivery); err != nil {
						c.report("a delivery could not be handled", fmt.Errorf("agent %s turn %s: %w", delivery.AgentID, delivery.TurnID, err))
						_ = conn.CloseNow() // what returning the error would have done: the loop ends and the connection is made again
					}
				}()
				continue
			}
			if err := c.handleDeliver(ctx, conn, frame.Delivery()); err != nil {
				return err
			}
		case protocol.Receipt:
			receipt := protocol.ReceiptBody{TurnID: frame.TurnID, AnswerRev: frame.AnswerRev, Digest: frame.Digest, Result: frame.Result, Details: frame.Details}
			if err := c.Journal.RecordReceipt(ctx, receipt); err != nil {
				return err
			}
		case protocol.Ack:
			if frame.Level == "command_exit" {
				if frame.JobID == "" {
					return fmt.Errorf("unexpected command acknowledgement")
				}
				if err := c.Journal.AcknowledgeCommandExit(ctx, frame.JobID); err != nil {
					return err
				}
				continue
			}
			if frame.Level != "process" || frame.TurnID == "" {
				return fmt.Errorf("unexpected acknowledgement %q", frame.Level)
			}
			if err := c.Journal.AcknowledgeExit(ctx, frame.TurnID); err != nil {
				return err
			}
		case protocol.CommandRun:
			if frame.Command == nil {
				return errors.New("missing command")
			}
			spec, reqID := *frame.Command, frame.ReqID
			if c.commandInContainer(spec) {
				// A Docker launcher is asked for its features first, which may take seconds: answered aside, so the frame loop goes on.
				// The server takes the answer whenever it comes (applyCommandStarted).
				go func() {
					defer telemetry.Recover(ctx)
					result := c.commandStart(context.WithoutCancel(ctx), spec)
					result.ReqID = reqID
					if err := c.send(ctx, conn, result); err != nil {
						c.report("a command's start could not be answered", fmt.Errorf("job %s: %w", spec.JobID, err))
					}
				}()
				break
			}
			result := c.commandStart(ctx, spec)
			result.ReqID = reqID
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.CommandCancel:
			result := c.commandCancel(frame.JobID)
			result.ReqID = frame.ReqID
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.CommandLogRead:
			result := c.commandLogTail(frame.ReqID, frame.LogPath, frame.LogMaxBytes)
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
		case protocol.Error:
			c.handleServerError(frame)
			c.settleRefusedExit(ctx, frame)
		default:
			return fmt.Errorf("unexpected machine frame %q", frame.Type)
		}
	}
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("folder must be absolute on this machine")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", errors.New("folder does not exist on this machine")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func (c *Client) pathProbeResult(ctx context.Context, probe protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.PathResult, ReqID: probe.ReqID}
	canonical, err := canonicalDirectory(probe.CheckoutPath)
	if err != nil {
		result.Code, result.Message = "invalid_path", err.Error()
		return result
	}
	if probe.LauncherID != "" {
		row := c.launcherRow(probe.LauncherID)
		if row.ID == "" {
			result.Code, result.Message = "launcher_unavailable", "launcher is not configured on this machine"
			return result
		}
		if _, err := preflightLauncher(ctx, row.LauncherDefinition, row, canonical); err != nil {
			result.Code, result.Message = "folder_unverified", err.Error()
			return result
		}
	}
	result.CanonicalPath = canonical
	return result
}

func (c *Client) applySpawn(frame protocol.Frame) protocol.Frame {
	result := protocol.Frame{V: protocol.Version, Type: protocol.SpawnResult, ReqID: frame.ReqID,
		AgentID: frame.AgentID, Generation: frame.Generation, Result: "ok"}
	c.mu.Lock()
	old, exists := c.desired[frame.AgentID]
	stale := exists && (frame.Generation < old.Generation ||
		frame.Generation == old.Generation && (frame.DesiredRev < old.DesiredRev || old.Desired != "active"))
	c.mu.Unlock()
	if stale {
		result.Result, result.Message = "error", "stale spawn generation or revision"
		return result
	}
	if !filepath.IsAbs(frame.CheckoutPath) {
		result.Result, result.Message = "error", "checkout path must be absolute on this machine"
		return result
	}
	info, err := os.Stat(frame.CheckoutPath)
	if err != nil || !info.IsDir() {
		result.Result, result.Message = "error", "checkout folder does not exist on this machine: "+frame.CheckoutPath
		return result
	}
	canonical, err := filepath.EvalSymlinks(frame.CheckoutPath)
	if err != nil || canonical != frame.CheckoutPath {
		result.Result, result.Message = "error", "checkout path must be canonical on this machine: "+frame.CheckoutPath
		return result
	}
	if frame.DockerOnly && frame.Agent != nil && frame.Agent.Tool != "fake" {
		containerized := frame.Agent.Variant == "docker"
		if frame.Agent.LauncherID != "" {
			containerized = c.launcherRow(frame.Agent.LauncherID).Containerized
		}
		if !containerized {
			result.Result, result.Message = "error", errDockerOnly.Error()
			return result
		}
	}
	if frame.Agent != nil && frame.Agent.LauncherID != "" {
		row := c.launcherRow(frame.Agent.LauncherID)
		if row.ID == "" {
			result.Result, result.Message = "error", "launcher not configured on this machine"
			return result
		}
		if !row.Available {
			result.Result, result.Message = "error", "launcher unavailable: "+row.UnavailableReason
			return result
		}
		if _, err := launcherCheckout(row.LauncherDefinition, frame.CheckoutPath); err != nil {
			result.Result, result.Message = "error", err.Error()
			return result
		}
	} else if frame.Agent != nil && frame.Agent.Variant == "docker" {
		c.mu.Lock()
		row := c.harnessRow(harnessKey(frame.Agent.Tool, "docker"))
		c.mu.Unlock()
		if !row.Available {
			result.Result, result.Message = "error", "harness unavailable: "+row.UnavailableReason
			return result
		}
		if err := adapter.DockerPreflight(frame.Agent.Tool, frame.CheckoutPath, row.LauncherPath); err != nil {
			result.Result, result.Message = "error", err.Error()
			return result
		}
	}
	if !c.keepAliveForget(frame.AgentID) || !c.retireWarm(frame.AgentID, "the agent was spawned again") {
		result.Result, result.Message = "error", "the agent's previous process could not be confirmed ended"
		return result
	}
	if err := c.setAgentKey(frame.AgentID, frame.AgentKey); err != nil {
		c.report("the agent tools config could not be written", fmt.Errorf("agent tools config for %s: %w", frame.AgentID, err))
		result.Result, result.Message = "error", err.Error()
		return result
	}
	c.mu.Lock()
	c.desired[frame.AgentID] = protocol.DesiredAgent{AgentID: frame.AgentID, Generation: frame.Generation,
		Desired: "active", DesiredRev: frame.DesiredRev}
	c.mu.Unlock()
	return result
}

func readFrame(ctx context.Context, conn *websocket.Conn) (protocol.Frame, error) {
	_, raw, err := conn.Read(ctx)
	if err != nil {
		return protocol.Frame{}, err
	}
	var frame protocol.Frame
	err = json.Unmarshal(raw, &frame)
	return frame, err
}

func (c *Client) send(ctx context.Context, conn *websocket.Conn, frame protocol.Frame) error {
	raw, err := encodeMachineFrame(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, raw)
}

func encodeMachineFrame(frame protocol.Frame) ([]byte, error) {
	if frame.Type == protocol.Answer && len(frame.Candidate) > answer.MaxAnswerBytes {
		frame.Candidate = ""
		frame.AnswerTooLarge = true
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if frame.Type == protocol.Answer && len(raw) > protocol.MaxMachineFrameBytes {
		frame.Candidate = ""
		frame.AnswerTooLarge = true
		return json.Marshal(frame)
	}
	return raw, nil
}

// unsent is what a failed send of something the supervisor keeps and delivers again (a journaled turn frame or exit, the warm state) gets: the local log
// only. The server being down is not a fault of the supervisor's; a replay that fails after the reconnect is reported where it fails.
func (c *Client) unsent(err error) {
	c.reportLocal(err)
}

// unsentLive is for a frame nothing replays (an activity frame): only logged while there is no connection, reported when the connection is up and the send
// still failed (a write on a live socket, an encode failure).
func (c *Client) unsentLive(cause string, err error) {
	if c.serverUnreachable() {
		c.reportLocal(err)
		return
	}
	c.report(cause, err)
}

// replayReadFailed reports a journal that cannot be read to replay after reconnecting; a shutdown (a cancelled context) is not a failure.
func (c *Client) replayReadFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	c.report("the journal could not be read to replay it after reconnecting", err)
}

// errMachineDisconnected is a send that found no server connection. While the supervisor reconnects every caller that sends meets it, so
// the local log has it once per outage, with a count when the connection is back (reportLocal).
var errMachineDisconnected = errors.New("machine socket disconnected")

// serverUnreachable says whether the supervisor has no live server connection right now.
func (c *Client) serverUnreachable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentConn == nil
}

func (c *Client) sendCurrent(ctx context.Context, frame protocol.Frame) error {
	c.mu.Lock()
	conn := c.currentConn
	c.mu.Unlock()
	if conn == nil {
		return errMachineDisconnected
	}
	return c.send(ctx, conn, frame)
}

func attachmentBaseURL(serverURL, turnID string, docker bool) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", errors.New("invalid attachment server URL")
	}
	if docker && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		port := u.Port()
		u.Host = "host.docker.internal"
		if port != "" {
			u.Host += ":" + port
		}
	}
	u.Path = "/api/agent/turns/" + url.PathEscape(turnID) + "/attachments/"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (c *Client) handleDeliver(ctx context.Context, conn *websocket.Conn, delivery protocol.Delivery) error {
	if delivery.TurnID == "" || delivery.AgentID == "" {
		return errors.New("invalid delivery")
	}
	spec := adapter.TurnSpec{TurnID: delivery.TurnID, Agent: delivery.Agent, NativeSessionHandle: delivery.NativeSessionHandle,
		NativeSessionStarted: delivery.NativeSessionStarted, ForkSession: delivery.ForkSession, CheckoutPath: delivery.CheckoutPath, LaunchPath: launchSpelling(delivery.LaunchPath, delivery.CheckoutPath),
		Mode: delivery.Mode, ContextJSON: delivery.Context, AllowedTools: delivery.AllowedTools, Providers: delivery.Providers}
	containerized := delivery.Agent.Variant == "docker"
	sharesHome := !containerized // a Docker launcher's login is the host's file only when it says it shares the host's Claude home
	launcherConfirmsEnd := false // a Docker launcher's process is kept warm only when its wrapper confirms the end of the session in the container
	if delivery.Agent.LauncherID != "" {
		row := c.launcherRow(delivery.Agent.LauncherID)
		if row.ID == "" || !row.Available {
			return errors.New("launcher unavailable on this machine")
		}
		visible, err := launcherCheckout(row.LauncherDefinition, delivery.CheckoutPath)
		if err != nil {
			return err
		}
		spec.LauncherPath = row.ResolvedCommand
		spec.LauncherArgs = append([]string(nil), row.Args...)
		spec.LauncherEnvRefs = row.EnvRefs
		spec.LauncherContainerized = row.Containerized
		spec.LauncherSharesClaudeHome = row.Containerized && slices.Contains(row.Features, "claude_home_shared")
		for _, mapping := range row.PathMaps {
			spec.LauncherPathMaps = append(spec.LauncherPathMaps, adapter.LauncherPathMap{Host: mapping.Host, Visible: mapping.Visible})
		}
		if row.Containerized && delivery.Agent.Tool == "codex" {
			spec.LauncherCodexHome = row.CodexHome
			if delivery.Mode == adapter.ModeNormal && !delivery.ForkSession {
				c.maybeReprobeCodexHome(row)
			}
		}
		if visible != delivery.CheckoutPath {
			spec.VisibleCheckoutPath = visible
		}
		spec.AgentServerURL = row.AgentServerURL
		containerized = row.Containerized
		feature := "claude_home_shared"
		if delivery.Agent.Tool == "codex" {
			feature = "codex_home_shared"
		}
		sharesHome = !row.Containerized || slices.Contains(row.Features, feature)
		launcherConfirmsEnd = slices.Contains(row.Features, containerEndFeature)
		spec.LauncherReportsContainer = row.Containerized && slices.Contains(row.Features, adapter.FeatureTurnActivity)
		spec.HarnessVersion = row.Version
	}
	if delivery.DockerOnly && !containerized && delivery.Agent.Tool != "fake" {
		c.retireWarm(delivery.AgentID, "native launcher while docker-only is on")
		return errDockerOnly
	}
	if delivery.Agent.Tool == "pi" && containerized && delivery.AttachmentBearer != "" {
		serverURL := c.ServerURL
		if spec.AgentServerURL != "" {
			serverURL = spec.AgentServerURL
		}
		base, err := attachmentBaseURL(serverURL, delivery.TurnID, spec.AgentServerURL == "")
		if err != nil {
			return err
		}
		spec.AttachmentBaseURL = base
		spec.AttachmentBearer = delivery.AttachmentBearer
	}
	if delivery.Agent.LauncherID == "" {
		if _, known := knownHarness(delivery.Agent.Tool, delivery.Agent.Variant); known {
			c.mu.Lock()
			row := c.harnessRow(harnessKey(delivery.Agent.Tool, delivery.Agent.Variant))
			c.mu.Unlock()
			if !row.Available {
				return fmt.Errorf("harness unavailable: %s", row.UnavailableReason)
			}
			spec.LauncherPath = row.LauncherPath
			spec.HarnessVersion = row.Version
		}
	}
	if delivery.Mode == adapter.ModeNormal {
		access, err := c.agentToolsAccess(delivery.AgentID)
		if err != nil {
			return err
		}
		spec.AgentTools = access
	}
	notes, err := json.Marshal(delivery.Notes)
	if err != nil {
		return err
	}
	spec.NotesJSON = notes
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	noteIDs := make([]string, 0, len(delivery.Notes))
	for _, note := range delivery.Notes {
		noteIDs = append(noteIDs, note.NoteID)
	}
	start := StartRecord{TurnID: delivery.TurnID, AgentID: delivery.AgentID, Generation: delivery.Generation, DesiredRev: delivery.DesiredRev, NoteIDs: noteIDs, Mode: string(delivery.Mode), SpecJSON: specJSON}
	if exists, err := c.Journal.HasStart(ctx, delivery.TurnID); err != nil {
		return err
	} else if exists {
		stored, err := c.Journal.TurnSpecJSON(ctx, delivery.TurnID)
		if err != nil {
			return err
		}
		// A pruned spec (an empty one: the turn settled long ago) has nothing to compare; RecordStart still checks the fields that stay.
		if len(stored) > 0 {
			var prior adapter.TurnSpec
			if err := json.Unmarshal(stored, &prior); err != nil {
				return err
			}
			var priorNotes []protocol.DeliveryNote
			if err := json.Unmarshal(prior.NotesJSON, &priorNotes); err != nil {
				return err
			}
			if !reflect.DeepEqual(priorNotes, delivery.Notes) {
				return ErrJournalConflict
			}
			spec.NotesJSON = prior.NotesJSON
			start.SpecJSON, err = json.Marshal(spec)
			if err != nil {
				return err
			}
		}
		if _, err := c.Journal.RecordStart(ctx, start); err != nil {
			return err
		}
		return c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: delivery.AgentID, Generation: delivery.Generation, TurnID: delivery.TurnID, Ref: delivery.TurnID, Level: "accepted"})
	}
	c.mu.Lock()
	if c.stopping {
		c.mu.Unlock()
		return errors.New("supervisor is stopping")
	}
	desired, ok := c.desired[delivery.AgentID]
	var updates struct {
		ControlChanges struct {
			Desired    string `json:"desired"`
			DesiredRev int64  `json:"desired_rev"`
		} `json:"control_changes"`
	}
	if len(delivery.Context) > 0 {
		if err := json.Unmarshal(delivery.Context, &updates); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	if ok && desired.Generation == delivery.Generation && updates.ControlChanges.Desired == "active" &&
		updates.ControlChanges.DesiredRev == delivery.DesiredRev && delivery.DesiredRev > desired.DesiredRev {
		desired.Desired = "active"
		desired.DesiredRev = delivery.DesiredRev
		c.desired[delivery.AgentID] = desired
	}
	busy := c.active[delivery.AgentID] != ""
	if !ok || desired.Generation != delivery.Generation || desired.Desired != "active" || desired.DesiredRev != delivery.DesiredRev || busy {
		c.mu.Unlock()
		return fmt.Errorf("delivery admission rejected for agent %s", delivery.AgentID)
	}
	c.active[delivery.AgentID] = delivery.TurnID
	c.noteActiveTurn(delivery.AgentID, activeTurn{turnID: delivery.TurnID, mode: delivery.Mode, containerized: containerized, confirmsEnd: launcherConfirmsEnd, checkout: delivery.CheckoutPath, launcherID: delivery.Agent.LauncherID})
	c.turns.Add(1)
	c.mu.Unlock()
	release := true
	tracked := true
	defer func() {
		if tracked {
			c.turns.Done()
		}
		if release {
			c.mu.Lock()
			delete(c.active, delivery.AgentID)
			c.mu.Unlock()
		}
	}()
	notes, err = json.Marshal(delivery.Notes)
	if err != nil {
		return err
	}
	spec.NotesJSON = notes
	specJSON, err = json.Marshal(spec)
	if err != nil {
		return err
	}
	start.SpecJSON = specJSON
	if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: delivery.AgentID, Generation: delivery.Generation, TurnID: delivery.TurnID, Ref: delivery.TurnID, Level: "persisted"}); err != nil {
		return err
	}
	if _, err := c.Journal.RecordStart(ctx, start); err != nil {
		return err
	}
	if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: delivery.AgentID, Generation: delivery.Generation, TurnID: delivery.TurnID, Ref: delivery.TurnID, Level: "accepted"}); err != nil {
		return err
	}
	cred := credentials{}
	codexAuth := c.warmCodexAuthPath(spec)
	cred.stamp, cred.ok = warmCredentials(spec, sharesHome, codexAuth)
	cred.containerOK = !containerized || launcherConfirmsEnd
	cred.account = warmAccount(spec, codexAuth)
	cred.now = func() (string, string, bool) {
		path := c.warmCodexAuthPath(spec)
		stamp, ok := warmCredentials(spec, sharesHome, path)
		return stamp, warmAccount(spec, path), ok
	}
	// The harness's stderr is kept in the agent's output log for as long as the turn runs. A log that cannot be opened is reported and the
	// turn runs without one: it is a debugging aid, never a reason not to work.
	var sink *agentlog.Turn
	if c.AgentLogs != nil {
		if opened, err := c.AgentLogs.OpenTurn(delivery.AgentID, delivery.TurnID, delivery.Generation, string(delivery.Mode)); err != nil {
			c.report("a turn output log could not be written", fmt.Errorf("agent %s turn %s: output log: %w", delivery.AgentID, delivery.TurnID, err))
		} else {
			sink = opened
			spec.StderrSink = sink
		}
	}
	// The conversation log is opened the same way and for the same time: the supervisor frames the turn in it (its start, what the agent
	// was asked, its end) and the adapter writes what the harness's event stream says in between.
	closeConversation := c.openConversation(delivery, &spec)
	c.applyWarmIdle(delivery.WarmIdleSeconds)
	c.setCodexWarmEnabled(delivery.CodexWarmEnabled)
	c.keepAliveRemember(spec, delivery.KeepAliveSettings, cred, sharesHome)
	var sinkWriter io.Writer
	if sink != nil {
		sinkWriter = sink
	}
	if err := c.copyLegacySession(ctx, delivery, containerized); err != nil {
		if sink != nil {
			_ = sink.Close()
		}
		closeConversation(err)
		return c.reportStartFailure(ctx, conn, delivery, err)
	}
	turn, err := c.startTurn(ctx, spec, sinkWriter, cred)
	if err != nil {
		if sink != nil {
			_ = sink.Close()
		}
		closeConversation(err)
		if errors.Is(err, errStoppedWhileWaiting) {
			return c.reportStoppedBeforeStart(ctx, conn, delivery)
		}
		return c.reportStartFailure(ctx, conn, delivery, err)
	}
	// The harness process's identity goes into the journal while the turn runs, so a later supervisor can tell that process from a later
	// one that reused its pid before it settles the turn. A write that cannot happen now (the connection died, the turn already ended)
	// costs the row its identity, never the turn its course.
	c.recordTurnProcess(context.WithoutCancel(ctx), delivery.TurnID, turn)
	c.mu.Lock()
	c.running[delivery.AgentID] = turn
	stopNow := c.stopping || c.desired[delivery.AgentID].Desired == "stopped"
	c.mu.Unlock()
	if stopNow {
		_ = turn.Cancel()
	} else {
		c.signalTurnInterrupt(delivery.AgentID, delivery.TurnID, turn) // an interrupt that came while the process was starting
	}
	release = false
	tracked = false
	go func() {
		defer telemetry.Recover(ctx)
		var restart func() (adapter.Turn, error)
		if containerized {
			// A Docker launcher that found its container being replaced started nothing: the same turn is started again (see containerRestart).
			restart = func() (adapter.Turn, error) { return c.startTurn(ctx, spec, sinkWriter, cred) }
		}
		c.runTurnRestartable(ctx, delivery, turn, restart)
		if sink != nil {
			// The adapter closes its event stream only after it has read the harness's stderr to the end, so nothing is written after this.
			_ = sink.Close()
		}
		closeConversation(nil)
	}()
	return nil
}

// reportStartFailure settles a journaled turn whose harness never launched
// (missing executable, unusable Docker checkout, bad MCP config, ...). The
// failure is agent-scoped: it is journaled as a failed exit carrying the
// error, reported like any other exit, and the machine connection stays up
// for the other agents. Returning the error instead would drop the socket and
// leave the turn running in the journal with no evidence of why.
func (c *Client) reportStartFailure(ctx context.Context, conn *websocket.Conn, delivery protocol.Delivery, startErr error) error {
	c.report("harness failed to start", fmt.Errorf("agent %s turn %s: harness failed to start: %w", delivery.AgentID, delivery.TurnID, startErr))
	c.forgetTurnInterrupt(delivery.AgentID, delivery.TurnID) // an interrupt that came while it was starting has nothing left to cut
	reason := adapter.SanitizeStderr(startErr.Error())
	exit := adapter.ExitInfo{ExitCode: 1, StderrTail: "harness failed to start: " + reason,
		ErrorSummary: reason, DefiniteFailure: true, StartFailure: true}
	if errors.Is(startErr, exec.ErrNotFound) || errors.Is(startErr, fs.ErrNotExist) {
		exit.ExitCode = 127
	}
	if err := c.Journal.RecordExitInfo(ctx, delivery.TurnID, exit); err != nil {
		return err
	}
	return c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: delivery.AgentID,
		Generation: delivery.Generation, TurnID: delivery.TurnID, Exit: &exit})
}

// reportStoppedBeforeStart settles a journaled turn whose harness was never started because the agent was stopped (or the supervisor began
// stopping) while the turn waited for the quarantine of its last warm process: the stop won, so the exit is the stop's cancel, not a failure.
func (c *Client) reportStoppedBeforeStart(ctx context.Context, conn *websocket.Conn, delivery protocol.Delivery) error {
	c.forgetTurnInterrupt(delivery.AgentID, delivery.TurnID)
	exit := adapter.ExitInfo{ExitCode: 130, CancelIntent: true, StderrTail: errStoppedWhileWaiting.Error()}
	if err := c.Journal.RecordExitInfo(ctx, delivery.TurnID, exit); err != nil {
		return err
	}
	// The stop's own path (stopWarmUntilGone) acknowledges it once the process is confirmed gone; nothing is acknowledged here.
	return c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: delivery.AgentID,
		Generation: delivery.Generation, TurnID: delivery.TurnID, Exit: &exit})
}

// turnEventFrame builds the wire frame for a turn event. Live sends and
// reconnect replay share it: the server dedupes a replay only when its bytes
// match the frame it already stored.
func turnEventFrame(agentID string, generation int, turnID string, event adapter.Event) protocol.Frame {
	frame := protocol.Frame{V: protocol.Version, AgentID: agentID, Generation: generation, TurnID: turnID, SourceEventID: event.SourceEventID, Kind: string(event.Kind), Summary: event.Summary}
	switch event.Kind {
	case adapter.EventActivity:
		frame.Type = protocol.Activity
	case adapter.EventUsage:
		frame.Type = protocol.Usage
		frame.Usage = event.Usage
	case adapter.EventCompaction:
		frame.Type = protocol.Compaction
		frame.Compaction = event.Compaction
	}
	return frame
}

func (c *Client) runTurn(ctx context.Context, delivery protocol.Delivery, turn adapter.Turn) {
	c.runTurnRestartable(ctx, delivery, turn, nil)
}

// runTurnRestartable runs a turn to its end like runTurn. restart, when it is not nil, starts the turn's process again: it is used for a
// turn whose launcher found its container being replaced and started nothing (see containerRestart).
func (c *Client) runTurnRestartable(ctx context.Context, delivery protocol.Delivery, turn adapter.Turn, restart func() (adapter.Turn, error)) {
	defer c.turns.Done()
	// Counted from before release() to after the process frame (deferred, so every return is covered): until then the turn is retire-
	// blocking work although its agent's admission is already free.
	c.unreported.Add(1)
	defer c.unreported.Add(-1)
	// The connection or process context may be cancelled while the harness is
	// exiting. Keep its final journal writes possible so a replacement can
	// replay the answer and exit after the socket is gone.
	journalCtx := context.WithoutCancel(ctx)
	release := func() {
		c.mu.Lock()
		if c.active[delivery.AgentID] == delivery.TurnID {
			delete(c.active, delivery.AgentID)
			delete(c.running, delivery.AgentID)
		}
		c.mu.Unlock()
	}
	defer release()
	limits := c.TurnLimits.For(delivery.Agent.Tool)
	watch := newTurnWatch(limits, time.Now())
	var stall turnStall
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	rearm := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if stall != "" {
			return
		}
		deadline, _, ok := watch.next()
		if watch.busy {
			// A tool process holds the idle deadline off: look at it again soon, not at the (suspended) deadline.
			if poll := time.Now().Add(min(toolProbePoll, max(limits.Idle, time.Millisecond))); !ok || poll.Before(deadline) {
				deadline, ok = poll, true
			}
		}
		if ok {
			timer.Reset(time.Until(deadline))
		}
	}
	rearm()
	// A journal write failure means this turn's events can no longer be made
	// durable. The harness must not keep running untracked: kill it, drain its
	// events without forwarding them, and still wait for the process so the
	// hello never reports a live harness as gone.
	var journalErr error
	abandon := func(err error) {
		journalErr = err
		c.report("journal write failed; killing harness", fmt.Errorf("agent %s turn %s: journal write failed; killing harness: %w", delivery.AgentID, delivery.TurnID, err))
		if killer, ok := turn.(interface{ Kill() error }); ok {
			_ = killer.Kill()
		} else {
			_ = turn.Cancel()
		}
	}
	var held *adapter.Event
	forward := func(event adapter.Event) {
		if event.Kind == adapter.EventUsage && event.Usage != nil && event.Usage.CostCumulative && event.Usage.CostEstimate != nil {
			c.keepAliveNoteCost(delivery.AgentID, *event.Usage.CostEstimate)
		}
		frame := turnEventFrame(delivery.AgentID, delivery.Generation, delivery.TurnID, event)
		switch event.Kind {
		case adapter.EventActivity, adapter.EventUsage, adapter.EventCompaction:
		case adapter.EventNativeSessionStarted:
			frame.Type = protocol.NativeSessionStarted
			frame.NativeSessionHandle = event.NativeSessionHandle
			c.keepAliveNativeSessionStarted(delivery.AgentID, event.NativeSessionHandle)
		case adapter.EventFinalAnswer:
			frame.Type = protocol.Answer
			if delivery.Mode == adapter.ModeCorrection {
				frame.AnswerRev = 1
			}
			sum := sha256.Sum256(event.Answer)
			frame.Digest = hex.EncodeToString(sum[:])
			frame.Candidate = string(event.Answer)
			if _, err := c.Journal.RecordAnswer(journalCtx, delivery.TurnID, frame.AnswerRev, frame.Digest, event.Answer); err != nil {
				abandon(err)
				return
			}
		default:
			return
		}
		if event.Kind != adapter.EventFinalAnswer {
			payload, err := json.Marshal(event)
			if err == nil {
				err = c.Journal.RecordEvent(journalCtx, delivery.TurnID, event.SourceEventID, string(event.Kind), event.Summary, payload)
			}
			if err != nil {
				abandon(err)
				return
			}
		}
		if err := c.sendCurrent(ctx, frame); err != nil {
			if event.Kind == adapter.EventActivity {
				// Activity is not journaled for replay: while the connection is down it is only logged, but a failed send on a live connection is a fault.
				c.unsentLive("a turn frame could not be sent to the server", err)
			} else {
				c.unsent(err) // journaled before this send (a native session, usage, compaction, the answer): the reconnect replays it
			}
		}
	}
	var exit adapter.ExitInfo
	var retryUntil time.Time // when the wait for a container that is being replaced is over; zero until the first such failure
	for {
		worked := false // the process of this attempt said something: it is not a launcher that started nothing
		events := turn.Events()
	events:
		for {
			var event adapter.Event
			select {
			case e, open := <-events:
				if !open {
					break events
				}
				event = e
				worked = worked || event.Kind != adapter.EventProcessExit
				watch.observe(event.Tools, time.Now())
				if event.Kind == adapter.EventFinalAnswer {
					// The answer is in; whatever the harness does while exiting is
					// no longer a stall of the work itself.
					watch = newTurnWatch(TurnLimits{}, time.Now())
				}
				rearm()
			case <-timer.C:
				// Partial streaming output is not an adapter event, but it proves
				// the harness is alive.
				if lo, ok := turn.(interface{ LastOutput() time.Time }); ok {
					watch.touch(lo.LastOutput())
				}
				// A harness that announces no tool start (OpenCode, Vibe) shows a long command only as a process under it: ask when the
				// idle deadline is due, and again while one runs, so that an end is noticed.
				if prober, ok := turn.(toolProber); ok && (watch.busy || watch.idleDue(time.Now())) {
					watch.setBusy(prober.ToolProcessSince(watch.last), time.Now())
				}
				deadline, reason, ok := watch.next()
				if !ok || time.Now().Before(deadline) {
					rearm()
					continue
				}
				stall = reason
				// A limit kill is the watchdog doing its job, not a failure to triage: the local log keeps the whole error, and telemetry
				// gets it as a warning (a degraded state the system recovers from by continuing the turn once on its own).
				if stall == turnStallMax {
					reason := stall.describe(limits)
					c.degraded(reason, fmt.Errorf("agent %s turn %s %s; killing harness", delivery.AgentID, delivery.TurnID, reason))
				} else {
					c.degraded("a turn stalled; killing harness", fmt.Errorf("agent %s turn %s stalled: %s; killing harness", delivery.AgentID, delivery.TurnID, stall.describe(limits)))
				}
				if killer, ok := turn.(interface{ Kill() error }); ok {
					_ = killer.Kill()
				} else {
					_ = turn.Cancel()
				}
				continue
			}
			if event.Kind == adapter.EventProcessExit || journalErr != nil {
				continue
			}
			if event.Kind == adapter.EventFinalAnswer && emptyAnswer(event.Answer) {
				// An answer with no content cannot be told from a harness cut off mid-turn until its exit is known: hold it.
				held = &event
				continue
			}
			forward(event)
		}
		var err error
		exit, err = turn.Wait()
		if err != nil {
			c.report("waiting for a turn failed", err)
		}
		if restart == nil || worked || held != nil || stall != "" || journalErr != nil || !containerBeingReplaced(exit) {
			break
		}
		next, again := c.containerRestart(ctx, delivery, turn, exit, restart, &retryUntil)
		if next == nil {
			break
		}
		turn = next
		if !again {
			exit, _ = turn.Wait()
			break
		}
		// The attempt that runs now is a new process: its own identity is what the journal must carry.
		c.recordTurnProcess(journalCtx, delivery.TurnID, turn)
		watch = newTurnWatch(limits, time.Now())
		rearm()
	}
	// A turn the supervisor ended on purpose without the agent being stopped (the owner's restart of the local supervisor, or the server's
	// turn interrupt) is reported interrupted, not cancelled. An answer that arrived first wins, and so does a stop of the agent.
	interruptReason := ""
	if requested, ours := c.settleTurnInterrupt(delivery, turn, exit); requested && ours {
		interruptReason = turnInterruptedOnRequest
	}
	defer c.forgetTurnInterrupt(delivery.AgentID, delivery.TurnID)
	if c.restartInterrupted.Load() && exit.CancelIntent {
		interruptReason = "local supervisor restarted by owner"
	}
	if held != nil {
		// The harness ended its turn with an empty answer. If its process was also cut off (killed, container stopped), the turn
		// was interrupted, not answered badly: the empty answer is dropped (a clean exit 0 is the harness's own empty reply). Otherwise it goes to the server as it always did.
		probe := exit
		probe.TerminalEvent = false
		if interruptReason != "" && delivery.Mode == adapter.ModeNormal {
			exit.TerminalEvent = false
		} else if _, cut := interruptedReason(probe); cut && exit.ExitCode != 0 && stall == "" && journalErr == nil {
			exit.TerminalEvent = false
		} else if journalErr == nil {
			forward(*held)
		}
	}
	if interruptReason != "" && !exit.TerminalEvent && delivery.Mode == adapter.ModeNormal {
		exit.CancelIntent = false
		exit.Interrupted = true
		exit.ErrorEvent = interruptReason
		if exit.ExitCode == 0 {
			exit.ExitCode = 130
		}
	}
	if stall != "" && !exit.TerminalEvent {
		// Our own kill is not an owner cancel: leave CancelIntent false so the
		// server settles it as a failed exit (notes unknown, recovery item)
		// rather than a deliberate stop, and carry the reason as evidence.
		reason := stall.describe(limits)
		exit.CancelIntent = false
		if exit.ExitCode == 0 {
			exit.ExitCode = 124
		}
		exit.ErrorEvent = reason
		exit.StderrTail = strings.TrimSpace(exit.StderrTail + "\n" + reason)
		// Marked, not sniffed out of the evidence text: the server continues a
		// limit kill once on its own, and must not confuse one with a crash.
		exit.LimitStall = true
		// What the killed turn left behind: whoever continues it sees at once whether its work was committed.
		if state := workTreeState(journalCtx, delivery.CheckoutPath); state != "" {
			exit.WorkTree = state
			exit.StderrTail = strings.TrimSpace(exit.StderrTail + "\nwork tree at the kill: " + state)
		}
	}
	if exit.WorkTree == "" && !exit.TerminalEvent && adapter.ContextOverflow(exit.ErrorEvent, exit.ErrorSummary, exit.StderrTail) {
		// A session that outgrew its context restarts fresh: its handoff note names the last commit, so the checkout is described here.
		if state := workTreeState(journalCtx, delivery.CheckoutPath); state != "" {
			exit.WorkTree = state
		}
	}
	if reason, cut := interruptedReason(exit); cut && stall == "" && journalErr == nil && delivery.Mode == adapter.ModeNormal {
		// Marked, not sniffed out of the evidence text: the server continues an interrupted turn once on its own, and must not
		// confuse one with a failure the harness explained.
		exit.Interrupted = true
		exit.ErrorEvent = reason
		exit.StderrTail = strings.TrimSpace(exit.StderrTail + "\n" + reason)
	}
	if journalErr != nil {
		// Like a stall, this kill is ours rather than an owner cancel. Events
		// after the failure, including any final answer, never reached the
		// server, so the exit cannot count as terminal either.
		reason := adapter.SanitizeStderr("supervisor journal write failed: " + journalErr.Error())
		exit.TerminalEvent, exit.CancelIntent = false, false
		if exit.ExitCode == 0 {
			exit.ExitCode = 1
		}
		exit.ErrorEvent = reason
		exit.StderrTail = strings.TrimSpace(exit.StderrTail + "\n" + reason)
	}
	if err := c.Journal.RecordExitInfo(journalCtx, delivery.TurnID, exit); err != nil {
		c.report("the exit info of a turn could not be recorded", err)
		unrecorded := exit
		unrecorded.ProcessAlive = c.warmTurnEnded(turn, adapter.ExitInfo{})
		c.conversationTurnEnd(delivery.TurnID, unrecorded)
		c.keepAliveTurnEnded(delivery.AgentID, adapter.ExitInfo{})
		release()
		c.sendDeferredStopAck(ctx, delivery.AgentID)
		return
	}
	// The process is durably finished before the server sees this frame. A warm process is settled (kept idle or ended) first, so that the
	// delivery admitted next finds it in the pool or finds it gone; then local admission is freed so an immediate follow-up delivery is not
	// rejected.
	exit.ProcessAlive = c.warmTurnEnded(turn, exit) // a kept warm process outlives its turn: the server is told the turn finished, not that a process exited
	// The conversation log says how the turn ended only now: whether its process was kept warm is known once the pool has settled it.
	c.conversationTurnEnd(delivery.TurnID, exit)
	// The turn-finished frame refreshes the agent card. Publish the settled
	// process state first so that refresh sees its idle warm process immediately.
	if _, warmTurn := turn.(*warmTurn); warmTurn {
		if err := c.sendWarmReport(ctx); err != nil {
			c.unsent(err) // the warm state is sent whole again when the connection returns
		}
	}
	if delivery.Mode == adapter.ModeNormal {
		c.keepAliveTurnEnded(delivery.AgentID, exit)
	}
	release()
	c.sendDeferredStopAck(ctx, delivery.AgentID)
	if c.processSink != nil {
		c.processSink(delivery.TurnID, exit)
	}
	if err := c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: delivery.AgentID, Generation: delivery.Generation, TurnID: delivery.TurnID, Exit: &exit}); err != nil {
		c.unsent(err) // journaled (RecordExitInfo) before this send: the reconnect replays it
	}
}

func (c *Client) sendDeferredStopAck(ctx context.Context, agentID string) {
	c.mu.Lock()
	ack, pending := c.pendingStopAck[agentID]
	delete(c.pendingStopAck, agentID)
	stopped := c.desired[agentID].Desired == "stopped" && c.running[agentID] == nil
	c.mu.Unlock()
	if stopped {
		if !c.stopWarm(agentID, "the agent was stopped") {
			// A process of the agent is not confirmed gone: the ack and the key's drop wait for it, and it is tried again.
			c.mu.Lock()
			if pending {
				c.pendingStopAck[agentID] = ack
			}
			c.mu.Unlock()
			c.stopWarmUntilGone(agentID, func() { c.sendDeferredStopAck(ctx, agentID) })
			return
		}
		c.forgetAgentKey(agentID)
	}
	if pending {
		if err := c.sendCurrent(ctx, ack); err != nil {
			c.report("a stop acknowledgement after a process exit failed", fmt.Errorf("agent %s stop ack after process exit: %w", agentID, err))
		}
	}
}

// StopAcceptingAndDrain rejects new deliveries, interrupts active turns, and
// waits for their journal writes to finish before the caller closes the journal.
func (c *Client) StopAcceptingAndDrain(ctx context.Context) error {
	c.shutdownRequested.Store(true)
	c.mu.Lock()
	c.stopping = true
	turns := make([]adapter.Turn, 0, len(c.running))
	for _, turn := range c.running {
		turns = append(turns, turn)
	}
	for agentID := range c.turnInterrupts {
		c.clearTurnInterrupt(agentID) // a shutdown does not wait for an interrupted turn's unconfirmed end
	}
	c.mu.Unlock()
	c.shutdownWarm()
	for _, turn := range turns {
		if err := turn.Cancel(); err != nil {
			c.report("a turn could not be cancelled on shutdown", err)
		}
	}
	// A long command that runs without this process (its own session, log writer and exit marker) is left to the replacement, which
	// adopts it from the journal; this process only lets go of it.
	c.detachCommands()
	if c.restartInterrupted.Load() {
		runner := c.runner()
		runner.mu.Lock()
		jobs := make([]string, 0, len(runner.jobs))
		for id := range runner.jobs {
			jobs = append(jobs, id)
		}
		runner.mu.Unlock()
		// A replacement does not wait for a command it cannot leave behind (one whose output passes through this process, as a Docker
		// launcher's does): it is ended, and its end (exit code -1, reason supervisor_replaced) wakes its agent, which can run it again.
		for _, id := range jobs {
			go c.commandReplace(id)
		}
	}
	done := make(chan struct{})
	telemetry.Go(ctx, func() {
		c.turns.Wait()
		if c.restartInterrupted.Load() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for c.hasRetireBlockingWork() {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}
		close(done)
	})
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		remaining := make([]string, 0, len(c.active))
		for agentID, turnID := range c.active {
			remaining = append(remaining, agentID+"/"+turnID)
		}
		c.mu.Unlock()
		sort.Strings(remaining)
		return fmt.Errorf("supervisor drain timed out with turns %s: %w", strings.Join(remaining, ", "), ctx.Err())
	}
}

func controlAck(frame protocol.Frame) protocol.Frame {
	return protocol.Frame{V: protocol.Version, Type: protocol.Ack, AgentID: frame.AgentID, Generation: frame.Generation, Level: "control", Ref: fmt.Sprint(frame.DesiredRev)}
}

// handleControl returns true when the stop ack must wait for process exit.
func (c *Client) handleControl(frame protocol.Frame) bool {
	c.mu.Lock()
	old := c.desired[frame.AgentID]
	if frame.Generation < old.Generation || (frame.Generation == old.Generation &&
		(frame.DesiredRev < old.DesiredRev || (frame.DesiredRev == old.DesiredRev && frame.DesiredState != old.Desired))) {
		c.mu.Unlock()
		return false
	}
	c.desired[frame.AgentID] = protocol.DesiredAgent{AgentID: frame.AgentID, Generation: frame.Generation, Desired: frame.DesiredState, DesiredRev: frame.DesiredRev}
	turn := c.running[frame.AgentID]
	turnID := c.active[frame.AgentID]
	if frame.DesiredState == "stopped" {
		c.clearTurnInterrupt(frame.AgentID) // a stop wins over a turn interrupt: the exit is the stop's cancel
	}
	deferAck := frame.DesiredState == "stopped" && turn != nil
	if deferAck {
		if c.pendingStopAck == nil {
			c.pendingStopAck = make(map[string]protocol.Frame)
		}
		c.pendingStopAck[frame.AgentID] = controlAck(frame)
	} else {
		delete(c.pendingStopAck, frame.AgentID)
	}
	c.mu.Unlock()
	if frame.DesiredState == "stopped" {
		c.clearWarmRefusal(frame.AgentID)
		c.killAgentCommands(frame.AgentID)
	}
	if frame.DesiredState == "stopped" && turn == nil {
		// A warm process is ended and reaped before the ack and before the key goes: nothing is left holding either. If it is not confirmed
		// gone, the ack and the key wait for it and it is tried again.
		if c.stopWarm(frame.AgentID, "the agent was stopped") {
			c.forgetAgentKey(frame.AgentID)
		} else {
			c.mu.Lock()
			if c.pendingStopAck == nil {
				c.pendingStopAck = make(map[string]protocol.Frame)
			}
			c.pendingStopAck[frame.AgentID] = controlAck(frame)
			c.mu.Unlock()
			c.stopWarmUntilGone(frame.AgentID, func() { c.sendDeferredStopAck(context.Background(), frame.AgentID) })
			return true
		}
	}
	if frame.DesiredState == "stopped" && turn != nil {
		_ = turn.Cancel()
		time.AfterFunc(30*time.Second, func() {
			c.mu.Lock()
			stillRunning := c.active[frame.AgentID] == turnID
			c.mu.Unlock()
			if stillRunning {
				if killer, ok := turn.(interface{ Kill() error }); ok {
					_ = killer.Kill()
				} else {
					_ = turn.Cancel()
				}
			}
		})
	}
	return deferAck
}

// handleServerError is what an error frame from the server gets. The refusals the server classifies (a stale generation, a turn already terminal in
// another state, a conflict, a forbidden or invalid transition) are the protocol working: logged only. Any other code is a failure on the server's side of
// the frame, reported to Sentry by its code (the frame's message is the server's own text and stays in the local log).
func (c *Client) handleServerError(frame protocol.Frame) {
	c.reportLocal(fmt.Errorf("server %s: %s", frame.Code, frame.Message))
	switch frame.Code {
	case "stale_generation", "stale_rev", "conflict", "forbidden", "invalid_transition", "already_terminal":
		return
	}
	if frame.Code == "unauthorized" && c.agentStopped(frame.AgentID) {
		return // a late frame (an ack) for an agent this supervisor already stopped: the server refuses it, as expected
	}
	telemetry.ReportCause(context.Background(), "supervisor", "server error frame: "+frame.Code)
}

// settleRefusedExit is what a refusal that says the turn is over gets: for stale_generation the server has already decided that turn on a newer
// generation of the agent (it lost the turn, or settled it from a newer instance's hello); for already_terminal the server's turn is already terminal in the
// other state (the journal says exited where the server holds killed, or the reverse). Either way the late exit report has nowhere to go and
// resending it would keep the journal open forever (the endless drain loop of A13-13). The exit is marked settled in the journal and never sent
// again. The Error frame does not say which frame it answers, so the settle is the journal's to bound: only an exited, unacknowledged row of that
// agent and turn matches, and a turn with none (a live turn whose activity frame was refused) is left alone, with no error for the read loop.
// Only the code is matched, never the message text: the server's refusal paths send different texts. Everything else keeps the exit journaled,
// as today; in particular a plain invalid_transition (an out-of-order frame the server refuses while the turn may still be running) settles
// nothing, so an older server that answers the terminal mismatch with invalid_transition alone leaves the exit journaled.
func (c *Client) settleRefusedExit(ctx context.Context, frame protocol.Frame) {
	if frame.AgentID == "" || frame.TurnID == "" {
		return
	}
	switch frame.Code {
	case "stale_generation", "already_terminal":
	default:
		return
	}
	generation, settled, err := c.Journal.SettleRefusedExit(ctx, frame.AgentID, frame.TurnID)
	if err != nil {
		c.report("a refused exit could not be settled", fmt.Errorf("agent %s turn %s: %w", frame.AgentID, frame.TurnID, err))
		return
	}
	if !settled {
		return
	}
	key := frame.AgentID + "/" + frame.TurnID
	c.mu.Lock()
	once := !c.refusedSettled[key]
	if once {
		if c.refusedSettled == nil {
			c.refusedSettled = make(map[string]bool)
		}
		c.refusedSettled[key] = true
	}
	c.mu.Unlock()
	if once {
		log.Printf("supervisor: agent %s turn %s generation %d: the server refused the late exit with %s; dropping it", frame.AgentID, frame.TurnID, generation, frame.Code)
	}
}

// agentStopped says whether the supervisor has an agent on record that is no longer desired active.
func (c *Client) agentStopped(agentID string) bool {
	if agentID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.desired[agentID]
	return ok && item.Desired != "active"
}

// report says what went wrong: to the local log (OnError), with the whole error, and, while telemetry is on, to Sentry, once per cause, with `cause` alone: a fixed
// phrase of the call site's. The error behind it can carry a harness's own output, a path or a name, and stays in the local log. It is the one place the supervisor's
// own failures go through, so a failed journal write, a stalled turn or a harness that would not start no longer end in a line on stderr that nobody reads.
func (c *Client) report(cause string, err error) {
	if err == nil {
		return
	}
	c.reportLocal(err)
	telemetry.ReportCause(context.Background(), "supervisor", cause)
}

// degraded is report for what the supervisor recovers from by itself (a turn that falls back to cold, a warm process that ended on its own): a warning.
func (c *Client) degraded(cause string, err error) {
	if err == nil {
		return
	}
	c.reportLocal(err)
	telemetry.WarnCause(context.Background(), "supervisor", cause)
}

// reportLocal is for a failure that has its own, classified Sentry report (a warm-process refusal, an error frame from the server): the local log only.
func (c *Client) reportLocal(err error) {
	if c.OnError == nil {
		return
	}
	if errors.Is(err, errMachineDisconnected) {
		if c.disconnectedDrops.Add(1) != 1 {
			return
		}
		err = fmt.Errorf("%w; further sends are counted until it is back (reconnecting every %s)", err, c.reconnectDelay())
	}
	c.OnError(err)
}

// logDisconnectedDrops sums up, once the connection is back, the sends that failed for want of one.
func (c *Client) logDisconnectedDrops() {
	if dropped := c.disconnectedDrops.Swap(0); dropped > 1 && c.OnError != nil {
		c.OnError(fmt.Errorf("machine socket reconnected: %d sends failed while it was disconnected (reconnecting every %s)", dropped, c.reconnectDelay()))
	}
}

// reconnectDelay is the wait between two connection attempts.
func (c *Client) reconnectDelay() time.Duration {
	if c.ReconnectDelay > 0 {
		return c.ReconnectDelay
	}
	return time.Second
}

// sweepAgentLogs removes agents' captured output that is older than its retention, or past the size cap, once at start and then daily.
func (c *Client) sweepAgentLogs(ctx context.Context) {
	if c.AgentLogs == nil {
		return
	}
	run := func() {
		if _, err := c.AgentLogs.Sweep(agentlog.DefaultMaxAge, agentlog.DefaultMaxTotal); err != nil {
			c.report("sweeping the agent output logs failed", fmt.Errorf("agent output logs: sweep: %w", err))
		}
	}
	run()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// launchSpelling is the folder spelling a turn may start the harness in instead of its checkout: the one the server names, when it is an
// absolute path that resolves to exactly the checkout. Anything else is ignored (the turn then starts in the checkout itself): the server
// is trusted to say which spelling a session was saved under, never to point a harness at another folder.
func launchSpelling(launch, checkout string) string {
	if launch == "" || launch == checkout || !filepath.IsAbs(launch) || filepath.Clean(launch) != launch {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(launch)
	if err != nil || filepath.Clean(resolved) != checkout {
		return ""
	}
	return launch
}
