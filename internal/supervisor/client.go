package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/agentlog"
	"github.com/hrubymar10/aimebu/internal/answer"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/tlsclient"
	"github.com/hrubymar10/aimebu/internal/worktree"
)

type Client struct {
	MachineID         string
	MachineKey        string
	ServerURL         string
	HTTPClient        *http.Client
	SupervisorVersion string
	CapabilityReports []protocol.CapabilityReport
	Harnesses         []protocol.HarnessRow
	Launchers         []protocol.LauncherRow
	harnessProbeDir   string
	// ConfigDir is the machine's aimebu config root; model detection uses a private detect/run-* folder unless the request names a folder.
	ConfigDir string
	// ClaudeConfigDir is where Claude Code keeps its sessions for a session scan; empty means CLAUDE_CONFIG_DIR, else ~/.claude (tests set it).
	ClaudeConfigDir string
	Journal         *Journal
	// AgentLogs holds what each agent's harness wrote to stderr, on this machine (supervisor/agent-logs/ beside the journal). The server
	// reads a bounded tail of it for an owner or admin; nothing else leaves the machine.
	AgentLogs      *agentlog.Store
	Adapter        adapter.Adapter
	PingInterval   time.Duration
	TurnLimits     TurnLimitOverrides
	ReconnectDelay time.Duration
	OnError        func(error)
	OnReady        func()
	writeMu        sync.Mutex
	detectMu       sync.Mutex
	mu             sync.Mutex
	desired        map[string]protocol.DesiredAgent
	active         map[string]string
	running        map[string]adapter.Turn
	pendingStopAck map[string]protocol.Frame
	currentConn    *websocket.Conn
	stopping       bool
	turns          sync.WaitGroup
}

func NewClient(machineID, machineKey, serverURL string, journal *Journal, implementation adapter.Adapter) *Client {
	client := &Client{MachineID: machineID, MachineKey: machineKey, ServerURL: serverURL, HTTPClient: tlsclient.NewHTTPClient(0), SupervisorVersion: "aimebu-v2-development", Journal: journal, Adapter: implementation, PingInterval: 15 * time.Second, ReconnectDelay: time.Second, desired: make(map[string]protocol.DesiredAgent), active: make(map[string]string), running: make(map[string]adapter.Turn)}
	for _, item := range implementation.Catalog() {
		client.CapabilityReports = append(client.CapabilityReports, protocol.CapabilityReport{Tool: item.Tool, Version: item.Version, Environment: item.Environment, Flags: implementation.Capabilities(item.Version, item.Environment)})
	}
	client.Harnesses = defaultHarnesses()
	if journal != nil && journal.path != "" {
		client.AgentLogs = agentlog.New(filepath.Join(filepath.Dir(journal.path), "agent-logs"))
	}
	return client
}

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

func (c *Client) Run(ctx context.Context) error {
	go c.sweepAgentLogs(ctx)
	for {
		err := c.RunOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if c.OnError != nil {
			c.OnError(err)
		}
		delay := c.ReconnectDelay
		if delay <= 0 {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

func (c *Client) RunOnce(ctx context.Context) error {
	parsed, err := url.Parse(c.ServerURL)
	if err != nil {
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
	defer func() {
		if ctx.Err() != nil {
			_ = conn.CloseNow()
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()
	probeCtx, cancelProbes := context.WithCancel(ctx)
	defer cancelProbes()
	conn.SetReadLimit(protocol.MaxMachineFrameBytes)
	agents, err := c.Journal.OpenTurns(ctx)
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
	if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Hello, MachineID: c.MachineID, JournalID: c.Journal.ID, SupervisorVersion: c.SupervisorVersion, SupportsPathProbe: true, SupportsSessionScan: true, SupportsSessionFork: true, SupportsWorktree: true, SupportsLaunchers: true, SupportsDetect: true, SupportsDetectDir: true, SupportsAgentLogs: c.AgentLogs != nil, SupportsAgentLogSize: c.AgentLogs != nil, SupportsStopReplay: true, Capabilities: c.CapabilityReports, Harnesses: c.Harnesses, Agents: agents}); err != nil {
		return err
	}
	ack, err := readFrame(ctx, conn)
	if err != nil {
		return err
	}
	if ack.V != protocol.Version || ack.Type != protocol.HelloAck {
		return fmt.Errorf("expected hello_ack, got %q", ack.Type)
	}
	c.mu.Lock()
	c.harnessProbeDir = ack.HarnessProbeDir
	// The server's definitions are authoritative after reconnect. Dropped
	// launchers must not remain selectable in this long-running process.
	c.Launchers = nil
	rows := append([]protocol.HarnessRow(nil), c.Harnesses...)
	c.mu.Unlock()
	for _, definition := range ack.LauncherDefinitions {
		if err := c.setLauncher(definition); err != nil && c.OnError != nil {
			c.OnError(err)
		}
	}
	if ack.HarnessProbeDir != "" {
		for _, row := range rows {
			if row.Variant == "docker" {
				if err := c.setHarness(row.HarnessSetting); err != nil && c.OnError != nil {
					c.OnError(err)
				}
			}
		}
	}
	for _, setting := range ack.HarnessSettings {
		if err := c.setHarness(setting); err != nil && c.OnError != nil {
			c.OnError(err)
		}
	}
	c.refreshCapabilities()
	if len(ack.LauncherDefinitions) > 0 {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus, Launchers: c.launcherRows(), Capabilities: c.CapabilityReports}); err != nil {
			return err
		}
	}
	if len(ack.HarnessSettings) > 0 || ack.HarnessProbeDir != "" {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus, Harnesses: c.Harnesses, Capabilities: c.CapabilityReports}); err != nil {
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
	c.mu.Unlock()
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
		return err
	}
	for _, pending := range usage {
		if err := c.send(ctx, conn, turnEventFrame(pending.AgentID, pending.Generation, pending.TurnID, pending.Event)); err != nil {
			return err
		}
	}
	exits, err := c.Journal.PendingExits(ctx)
	if err != nil {
		return err
	}
	for _, exit := range exits {
		if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: exit.AgentID, Generation: exit.Generation, TurnID: exit.TurnID, Exit: &exit.Exit}); err != nil {
			return err
		}
	}
	pending, err := c.Journal.PendingAnswers(ctx)
	if err != nil {
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
	interval := c.PingInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go func() {
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
			} else if page, err := c.AgentLogs.Tail(frame.AgentID, frame.LogLines, frame.LogMaxBytes); errors.Is(err, agentlog.ErrNoLog) {
				result.Code, result.Message = "no_log", "no output has been captured for this agent on this machine"
			} else if err != nil {
				result.Code, result.Message = "log_failed", "the output could not be read"
				c.report(fmt.Errorf("agent %s output log: %w", frame.AgentID, err))
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
				result := c.sessionScanResult(scan)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report(err)
				}
			}(frame)
		case protocol.PathProbe:
			// Launcher version checks can take up to a minute. Keep reading
			// machine frames while the probe runs; send serializes writes.
			go func(probe protocol.Frame) {
				result := c.pathProbeResult(probeCtx, probe)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report(err)
				}
			}(frame)
		case protocol.Detect:
			// Looking around the machine can take a while; keep reading frames meanwhile.
			go func(request protocol.Frame) {
				result := c.detectResult(probeCtx, request)
				if probeCtx.Err() != nil {
					return
				}
				if err := c.send(probeCtx, conn, result); err != nil && probeCtx.Err() == nil {
					c.report(err)
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
				c.mu.Lock()
				probeDir := c.harnessProbeDir
				c.mu.Unlock()
				if row, err := inspectLauncher(*frame.LauncherDefinition, probeDir); err != nil {
					result.Code, result.Message = "invalid_launcher", err.Error()
				} else {
					result.Launcher = &row
				}
			} else if err := c.setLauncher(*frame.LauncherDefinition); err != nil {
				result.Code, result.Message = "invalid_launcher", err.Error()
			} else {
				row := c.launcherRow(frame.LauncherDefinition.ID)
				result.Launcher = &row
				c.refreshCapabilities()
			}
			if err := c.send(ctx, conn, result); err != nil {
				return err
			}
			if result.Launcher != nil && frame.Type == protocol.LauncherSet {
				if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.LauncherStatus,
					Launchers: c.launcherRows(), Capabilities: c.CapabilityReports}); err != nil {
					return err
				}
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
						Launchers: c.launcherRows(), Capabilities: c.CapabilityReports}); err != nil {
						return err
					}
				}
			} else if result.Result == "ok" && frame.Agent != nil && frame.Agent.Variant == "docker" {
				c.mu.Lock()
				c.harnessProbeDir = frame.CheckoutPath
				row := c.harnessRow(harnessKey(frame.Agent.Tool, "docker"))
				c.mu.Unlock()
				if err := c.setHarness(row.HarnessSetting); err == nil {
					c.refreshCapabilities()
					if err := c.send(ctx, conn, protocol.Frame{V: protocol.Version, Type: protocol.HarnessStatus,
						Harnesses: c.Harnesses, Capabilities: c.CapabilityReports}); err != nil {
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
		case protocol.Deliver:
			if err := c.handleDeliver(ctx, conn, frame.Delivery()); err != nil {
				return err
			}
		case protocol.Receipt:
			receipt := protocol.ReceiptBody{TurnID: frame.TurnID, AnswerRev: frame.AnswerRev, Digest: frame.Digest, Result: frame.Result, Details: frame.Details}
			if err := c.Journal.RecordReceipt(ctx, receipt); err != nil {
				return err
			}
		case protocol.Ack:
			if frame.Level != "process" || frame.TurnID == "" {
				return fmt.Errorf("unexpected acknowledgement %q", frame.Level)
			}
			if err := c.Journal.AcknowledgeExit(ctx, frame.TurnID); err != nil {
				return err
			}
		case protocol.Error:
			if c.OnError != nil {
				c.OnError(fmt.Errorf("server %s: %s", frame.Code, frame.Message))
			}
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
	if err := c.writeAgentToolsConfig(frame.AgentID, frame.AgentKey); err != nil {
		c.report(fmt.Errorf("agent tools config for %s: %w", frame.AgentID, err))
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

func (c *Client) sendCurrent(ctx context.Context, frame protocol.Frame) error {
	c.mu.Lock()
	conn := c.currentConn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("machine socket disconnected")
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
		NativeSessionStarted: delivery.NativeSessionStarted, ForkSession: delivery.ForkSession, CheckoutPath: delivery.CheckoutPath,
		Mode: delivery.Mode, ContextJSON: delivery.Context, AllowedTools: delivery.AllowedTools}
	containerized := delivery.Agent.Variant == "docker"
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
		if visible != delivery.CheckoutPath {
			spec.VisibleCheckoutPath = visible
		}
		spec.AgentServerURL = row.AgentServerURL
		containerized = row.Containerized
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
		}
	}
	if delivery.Mode == adapter.ModeNormal {
		path := c.agentToolsPath(delivery.AgentID)
		if _, err := os.Stat(path); err == nil {
			spec.AgentToolsConfigPath = path
		} else if !os.IsNotExist(err) {
			return err
		}
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
	// The harness's stderr is kept in the agent's output log for as long as the turn runs. A log that cannot be opened is reported and the
	// turn runs without one: it is a debugging aid, never a reason not to work.
	var sink *agentlog.Turn
	if c.AgentLogs != nil {
		if opened, err := c.AgentLogs.OpenTurn(delivery.AgentID, delivery.TurnID, delivery.Generation, string(delivery.Mode)); err != nil {
			c.report(fmt.Errorf("agent %s turn %s: output log: %w", delivery.AgentID, delivery.TurnID, err))
		} else {
			sink = opened
			spec.StderrSink = sink
		}
	}
	turn, err := c.Adapter.Start(ctx, spec)
	if err != nil {
		if sink != nil {
			_ = sink.Close()
		}
		return c.reportStartFailure(ctx, conn, delivery, err)
	}
	c.mu.Lock()
	c.running[delivery.AgentID] = turn
	stopNow := c.stopping || c.desired[delivery.AgentID].Desired == "stopped"
	c.mu.Unlock()
	if stopNow {
		_ = turn.Cancel()
	}
	release = false
	tracked = false
	go func() {
		c.runTurn(ctx, delivery, turn)
		if sink != nil {
			// The adapter closes its event stream only after it has read the harness's stderr to the end, so nothing is written after this.
			_ = sink.Close()
		}
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
	c.report(fmt.Errorf("agent %s turn %s: harness failed to start: %w", delivery.AgentID, delivery.TurnID, startErr))
	exit := adapter.ExitInfo{ExitCode: 1, StderrTail: adapter.SanitizeStderr("harness failed to start: " + startErr.Error())}
	if errors.Is(startErr, exec.ErrNotFound) || errors.Is(startErr, fs.ErrNotExist) {
		exit.ExitCode = 127
	}
	if err := c.Journal.RecordExitInfo(ctx, delivery.TurnID, exit); err != nil {
		return err
	}
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
	defer c.turns.Done()
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
		if deadline, _, ok := watch.next(); ok {
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
		c.report(fmt.Errorf("agent %s turn %s: journal write failed; killing harness: %w", delivery.AgentID, delivery.TurnID, err))
		if killer, ok := turn.(interface{ Kill() error }); ok {
			_ = killer.Kill()
		} else {
			_ = turn.Cancel()
		}
	}
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
			deadline, reason, ok := watch.next()
			if !ok || time.Now().Before(deadline) {
				rearm()
				continue
			}
			stall = reason
			c.report(fmt.Errorf("agent %s turn %s stalled: %s; killing harness", delivery.AgentID, delivery.TurnID, stall.describe(limits)))
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
		frame := turnEventFrame(delivery.AgentID, delivery.Generation, delivery.TurnID, event)
		switch event.Kind {
		case adapter.EventActivity, adapter.EventUsage, adapter.EventCompaction:
		case adapter.EventNativeSessionStarted:
			frame.Type = protocol.NativeSessionStarted
			frame.NativeSessionHandle = event.NativeSessionHandle
		case adapter.EventFinalAnswer:
			frame.Type = protocol.Answer
			if delivery.Mode == adapter.ModeCorrection {
				frame.AnswerRev = 1
			}
			sum := sha256.Sum256(event.Answer)
			frame.Digest = hex.EncodeToString(sum[:])
			frame.Candidate = string(event.Answer)
			if _, err := c.Journal.RecordAnswer(ctx, delivery.TurnID, frame.AnswerRev, frame.Digest, event.Answer); err != nil {
				abandon(err)
				continue
			}
		default:
			continue
		}
		if event.Kind != adapter.EventFinalAnswer {
			payload, err := json.Marshal(event)
			if err == nil {
				err = c.Journal.RecordEvent(ctx, delivery.TurnID, event.SourceEventID, string(event.Kind), event.Summary, payload)
			}
			if err != nil {
				abandon(err)
				continue
			}
		}
		if err := c.sendCurrent(ctx, frame); err != nil {
			c.report(err)
		}
	}
	exit, err := turn.Wait()
	if err != nil {
		c.report(err)
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
	if err := c.Journal.RecordExitInfo(ctx, delivery.TurnID, exit); err != nil {
		c.report(err)
		release()
		c.sendDeferredStopAck(ctx, delivery.AgentID)
		return
	}
	// The process is durably finished before the server sees this frame. Free
	// local admission now so an immediate follow-up delivery is not rejected.
	release()
	c.sendDeferredStopAck(ctx, delivery.AgentID)
	if err := c.sendCurrent(ctx, protocol.Frame{V: protocol.Version, Type: protocol.Process, AgentID: delivery.AgentID, Generation: delivery.Generation, TurnID: delivery.TurnID, Exit: &exit}); err != nil {
		c.report(err)
	}
}

func (c *Client) sendDeferredStopAck(ctx context.Context, agentID string) {
	c.mu.Lock()
	ack, pending := c.pendingStopAck[agentID]
	delete(c.pendingStopAck, agentID)
	c.mu.Unlock()
	if pending {
		if err := c.sendCurrent(ctx, ack); err != nil {
			c.report(fmt.Errorf("agent %s stop ack after process exit: %w", agentID, err))
		}
	}
}

// StopAcceptingAndDrain rejects new deliveries, interrupts active turns, and
// waits for their journal writes to finish before the caller closes the journal.
func (c *Client) StopAcceptingAndDrain(ctx context.Context) error {
	c.mu.Lock()
	c.stopping = true
	turns := make([]adapter.Turn, 0, len(c.running))
	for _, turn := range c.running {
		turns = append(turns, turn)
	}
	c.mu.Unlock()
	for _, turn := range turns {
		if err := turn.Cancel(); err != nil {
			c.report(err)
		}
	}
	done := make(chan struct{})
	go func() { c.turns.Wait(); close(done) }()
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

func (c *Client) report(err error) {
	if c.OnError != nil {
		c.OnError(err)
	}
}

// sweepAgentLogs removes agents' captured output that is older than its retention, or past the size cap, once at start and then daily.
func (c *Client) sweepAgentLogs(ctx context.Context) {
	if c.AgentLogs == nil {
		return
	}
	run := func() {
		if _, err := c.AgentLogs.Sweep(agentlog.DefaultMaxAge, agentlog.DefaultMaxTotal); err != nil {
			c.report(fmt.Errorf("agent output logs: sweep: %w", err))
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
