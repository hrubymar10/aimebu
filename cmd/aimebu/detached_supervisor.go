package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

var fakeAdapterHook func(*adapter.Fake)

// supervisorClientHook lets a test change the supervisor's client before it runs (to stand in for an older build).
var supervisorClientHook func(*supervisor.Client)

func localFakeAdapter() *adapter.Fake {
	fake := adapter.NewFake()
	fake.SetEchoDefault(true)
	if fakeAdapterHook != nil {
		fakeAdapterHook(fake)
	}
	return fake
}

func supervisorMode() (string, error) {
	mode := os.Getenv("AIMEBU_SUPERVISOR")
	if mode == "" {
		mode = "embedded"
	}
	if mode != "embedded" && mode != "detached" {
		return "", fmt.Errorf("AIMEBU_SUPERVISOR must be embedded or detached")
	}
	return mode, nil
}

func localSupervisorURL(config serverConfig) (string, error) {
	address, err := netip.ParseAddr(config.bind)
	if err != nil {
		return "", fmt.Errorf("detached supervisor needs a loopback or wildcard AIMEBU_BIND: %w", err)
	}
	host := "127.0.0.1"
	if address.Is6() && (address.IsLoopback() || address.IsUnspecified()) {
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(config.port)), nil
}

func localDialURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(parsed.Hostname())
	if err != nil || !ip.IsLoopback() || parsed.User != nil || parsed.Port() == "" || parsed.Path != "" || parsed.RawQuery != "" {
		return errors.New("--local requires a loopback server URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("--local requires an http or https server URL")
	}
	return nil
}

func supervisorJournalPath(root string) string {
	return filepath.Join(root, "supervisor", "journal.sqlite")
}

func startDetachedSupervisor(config serverConfig) error {
	pid, held, err := supervisorPID(config.root)
	if err != nil {
		return err
	}
	if held {
		fmt.Fprintf(os.Stderr, "adopting local supervisor pid %d; waiting for its machine connection\n", pid)
		return nil
	}
	serverURL, err := localSupervisorURL(config)
	if err != nil {
		return err
	}
	executable, err := daemonExecutable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(config.root, "supervisor", "aimebu.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer null.Close()
	command := exec.Command(executable, "supervisor", "--local")
	command.Stdin, command.Stdout, command.Stderr = null, log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Env = append(os.Environ(), "AIMEBU_URL="+serverURL)
	markInheritedFDsCloseOnExec()
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

// The running server may outlive replacement of the executable on disk. Ask
// that executable for its build before retiring a connected child.
func onDiskSupervisorVersion() (string, error) {
	executable, err := daemonExecutable()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func supervisorPID(root string) (int, bool, error) {
	var held bool
	var owner string
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		held, owner, err = supervisor.JournalOwner(supervisorJournalPath(root))
		if err != nil || !held || owner != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || !held {
		return 0, held, err
	}
	pid, err := strconv.Atoi(owner)
	if err != nil || pid <= 1 || !processAlive(pid) {
		return 0, true, fmt.Errorf("supervisor lock is held but its pid %q cannot be verified", owner)
	}
	exe, command, err := processInfo(pid)
	if err != nil || !looksLikeAimebuSupervisor(exe, command) {
		return 0, true, fmt.Errorf("supervisor lock is held by pid %d but its command cannot be verified", pid)
	}
	return pid, true, nil
}

type detachedSupervisorManager struct {
	config       serverConfig
	legacyMu     sync.Mutex
	legacyDrain  *legacyDrainState
	stderr       io.Writer
	store        *store.Store
	machineID    string
	ctx          context.Context
	connected    func() bool
	canInterrupt func() bool
	// replaceOverdue asks the server to replace a supervisor that was told to retire, whose grace is over and which has no connection
	// (server.ReplaceOverdueLocalSupervisor). It says whether it did.
	replaceOverdue func(context.Context) (bool, error)
	stopMu         sync.Mutex
	stopWatched    map[int]bool // the pids a stop was sent to and whose exit is being waited for
	// Tests replace these: who holds the journal (supervisorPID), how a process is killed, and how long a stop may take (supervisorStopBound).
	holder    func() (int, bool, error)
	kill      func(int) error
	stopBound time.Duration
}

// supervisorStopBound is how long a supervisor that was told to stop gets to release its journal before it is killed. Its own drain is
// limited to 40 seconds (supervisorCommand), so one that still holds the journal after this is not draining: it hangs.
var supervisorStopBound = 75 * time.Second

// killIfStuck waits for a supervisor that was told to stop. One that still holds the journal when the bound is over is killed, so that
// its replacement can start: the journal keeps what its turns reported, and the replacement delivers it.
func (m *detachedSupervisorManager) killIfStuck(pid int) {
	m.stopMu.Lock()
	if m.stopWatched == nil {
		m.stopWatched = make(map[int]bool)
	}
	if m.stopWatched[pid] {
		m.stopMu.Unlock()
		return
	}
	m.stopWatched[pid] = true
	m.stopMu.Unlock()
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	holder, kill, bound := m.holder, m.kill, m.stopBound
	if holder == nil {
		holder = func() (int, bool, error) { return supervisorPID(m.config.root) }
	}
	if kill == nil {
		kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	}
	if bound <= 0 {
		bound = supervisorStopBound
	}
	go func() {
		defer func() {
			m.stopMu.Lock()
			delete(m.stopWatched, pid)
			m.stopMu.Unlock()
		}()
		deadline := time.NewTimer(bound)
		defer deadline.Stop()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if current, held, err := holder(); err == nil && (!held || current != pid) {
				return // it exited (and another may already have taken over)
			}
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				// Only the process that verifiably still holds the journal is killed, and only while it is the one the stop went to.
				if current, held, err := holder(); err != nil || !held || current != pid {
					return
				}
				fmt.Fprintf(m.stderr, "supervisor pid %d still holds the journal %s after it was told to stop; killing it\n", pid, bound)
				if err := kill(pid); err != nil {
					fmt.Fprintln(m.stderr, "supervisor kill:", err)
				}
				return
			case <-ticker.C:
			}
		}
	}()
}

type legacyDrainState struct {
	cancel context.CancelFunc
}

func (m *detachedSupervisorManager) retireLegacyAtIdle() {
	m.legacyMu.Lock()
	if m.legacyDrain != nil {
		m.legacyMu.Unlock()
		return
	}
	drainCtx, cancel := context.WithCancel(m.ctx)
	state := &legacyDrainState{cancel: cancel}
	m.legacyDrain = state
	m.legacyMu.Unlock()
	go func() {
		defer func() {
			m.legacyMu.Lock()
			if m.legacyDrain == state {
				m.legacyDrain = nil
			}
			m.legacyMu.Unlock()
			cancel()
		}()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			active, err := m.store.MachineActiveWork(drainCtx, m.machineID)
			if err == nil && !active {
				m.legacyMu.Lock()
				if drainCtx.Err() == nil {
					if err := m.restartNow(); err != nil {
						fmt.Fprintln(m.stderr, "supervisor idle restart:", err)
					}
				}
				m.legacyMu.Unlock()
				return
			}
			select {
			case <-drainCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *detachedSupervisorManager) cancelLegacyDrain() {
	m.legacyMu.Lock()
	if m.legacyDrain != nil {
		m.legacyDrain.cancel()
		m.legacyDrain = nil
	}
	m.legacyMu.Unlock()
}

func (m *detachedSupervisorManager) restartNow() error {
	pid, held, err := supervisorPID(m.config.root)
	if err != nil {
		return err
	}
	if !held {
		return startDetachedSupervisor(m.config)
	}
	stop := syscall.SIGTERM
	if m.canInterrupt != nil && m.canInterrupt() {
		stop = syscall.SIGUSR1
	}
	if err := syscall.Kill(pid, stop); err != nil {
		return err
	}
	m.killIfStuck(pid)
	return nil
}

func (m *detachedSupervisorManager) run(ctx context.Context) {
	backoff := time.Second
	var disconnectedAt time.Time
	everConnected := false
	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}
	for ctx.Err() == nil {
		if m.connected != nil && m.connected() {
			disconnectedAt = time.Time{}
			everConnected = true
			backoff = time.Second
		} else if _, held, err := supervisorPID(m.config.root); err != nil {
			fmt.Fprintln(m.stderr, "supervisor:", err)
		} else if !held {
			disconnectedAt = time.Time{}
			everConnected = false
			if err := startDetachedSupervisor(m.config); err != nil {
				fmt.Fprintln(m.stderr, "supervisor respawn:", err)
			}
			backoff = min(backoff*2, 30*time.Second)
		} else if disconnectedAt.IsZero() {
			disconnectedAt = time.Now()
		} else if replaced, err := m.overdue(ctx, disconnectedAt); err != nil {
			fmt.Fprintln(m.stderr, "supervisor replacement:", err)
			disconnectedAt = time.Now()
		} else if replaced {
			fmt.Fprintln(m.stderr, "supervisor was told to retire, did not exit within the grace and is not connected; requested its restart")
			disconnectedAt = time.Now()
		} else if !everConnected && time.Since(disconnectedAt) > 30*time.Second {
			active, err := m.store.MachineActiveWork(ctx, m.machineID)
			if err != nil {
				fmt.Fprintln(m.stderr, "supervisor restart check:", err)
			} else if !active {
				fmt.Fprintln(m.stderr, "supervisor is holding the journal without connecting to this server; requesting its restart")
				if err := m.restartNow(); err != nil {
					fmt.Fprintln(m.stderr, "supervisor restart:", err)
				}
			}
			disconnectedAt = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// supervisorDisconnectedLimit is how long a supervisor may hold the journal without a connection before the server is asked whether its
// replacement is overdue. A healthy one reconnects every five seconds.
var supervisorDisconnectedLimit = 30 * time.Second

func (m *detachedSupervisorManager) overdue(ctx context.Context, disconnectedAt time.Time) (bool, error) {
	if m.replaceOverdue == nil || time.Since(disconnectedAt) < supervisorDisconnectedLimit {
		return false, nil
	}
	return m.replaceOverdue(ctx)
}

func looksLikeAimebuSupervisor(exe, command string) bool {
	if exe != "" {
		if info, err := os.Stat(exe); err == nil && info.Mode().IsRegular() && !aimebuName(filepath.Base(exe)) {
			return false
		}
	}
	fields := strings.Fields(command)
	for i := 1; i < len(fields); i++ {
		if aimebuName(filepath.Base(strings.Join(fields[:i], " "))) && fields[i] == "supervisor" {
			return true
		}
	}
	return false
}

func stopDetachedSupervisor(root string, stdout, stderr io.Writer) int {
	pid, held, err := supervisorPID(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !held {
		fmt.Fprintln(stdout, "supervisor is not running")
		return 0
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deadline := time.Now().Add(daemonStopLimit)
	for time.Now().Before(deadline) {
		if running, _, err := supervisor.JournalOwner(supervisorJournalPath(root)); err == nil && !running {
			fmt.Fprintf(stdout, "supervisor stopped (was pid %d)\n", pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "supervisor pid %d is still stopping; check %s\n", pid, filepath.Join(root, "supervisor", "aimebu.log"))
	return 1
}

func supervisorControl(action string, stdout, stderr io.Writer) int {
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch action {
	case "status":
		pid, held, err := supervisorPID(config.root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !held {
			fmt.Fprintln(stdout, "supervisor is not running")
			return 1
		}
		fmt.Fprintf(stdout, "supervisor is running (pid %d)\n", pid)
		return 0
	case "restart":
		answered, err := askServerToRestartSupervisor(config)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if answered {
			fmt.Fprintln(stdout, "supervisor restart requested from the server")
			return 0
		}
		if code := stopDetachedSupervisor(config.root, io.Discard, stderr); code != 0 {
			return code
		}
		if err := startDetachedSupervisor(config); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "supervisor starting")
		return 0
	}
	return 2
}

// askServerToRestartSupervisor uses the same same-host credential as doctor.
// A responding server is authoritative even when it refuses the request.
func askServerToRestartSupervisor(config serverConfig) (bool, error) {
	running, err := serverRunning(config.root)
	if err != nil || !running {
		return false, err
	}
	key, check, ok := readDoctorKey(config.root)
	if !ok {
		return true, errors.New(check.Description)
	}
	base, err := localSupervisorURL(config)
	if err != nil {
		return true, err
	}
	request, err := http.NewRequest(http.MethodPost, base+"/api/local-supervisor/restart", nil)
	if err != nil {
		return true, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := doctorHTTPClient(5 * time.Second).Do(request)
	if err != nil {
		return true, fmt.Errorf("the server at %s didn't answer the restart request; try again or restart the server: %w", base, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return true, fmt.Errorf("server refused supervisor restart: HTTP %d", response.StatusCode)
	}
	return true, nil
}

func serverDaemonStopWithSupervisor(stdout, stderr io.Writer) int {
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	running, err := serverRunning(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if running {
		if code := daemonStop(config, stdout, stderr); code != 0 {
			return code
		}
	} else {
		if moving, err := v1MovePending(config.root); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		} else if moving {
			fmt.Fprintln(stderr, "this is still a v1 config directory")
			return 1
		}
		fmt.Fprintln(stdout, "aimebu is not running")
	}
	return stopDetachedSupervisor(config.root, stdout, stderr)
}
