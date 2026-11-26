package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
	"github.com/hrubymar10/aimebu/v2/internal/server"
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

// warnIgnoredSupervisorMode notes an AIMEBU_SUPERVISOR left over from when the local supervisor could also run inside the server. The local
// supervisor is now always a process of its own, so the setting changes nothing; the start goes on.
func warnIgnoredSupervisorMode(w io.Writer) {
	if mode := os.Getenv("AIMEBU_SUPERVISOR"); mode != "" {
		fmt.Fprintf(w, "AIMEBU_SUPERVISOR=%q is ignored: the local supervisor always runs as its own process (AIMEBU_EMBEDDED_SUPERVISOR=0 turns it off)\n", mode)
	}
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
	_, err = startSupervisorInstance(config, "", false)
	return err
}

// startSupervisorInstance starts one detached local supervisor instance. journal is the journal it owns ("" is the legacy
// supervisor/journal.sqlite); drain starts it with --drain, so it only finishes what a dead instance left in that journal.
func startSupervisorInstance(config serverConfig, journal string, drain bool) (int, error) {
	serverURL, err := localSupervisorURL(config)
	if err != nil {
		return 0, err
	}
	executable, err := daemonExecutable()
	if err != nil {
		return 0, err
	}
	logPath := filepath.Join(config.root, "supervisor", "aimebu.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer null.Close()
	args := []string{"supervisor", "--local"}
	if journal != "" {
		args = append(args, "--journal", journal)
	}
	if drain {
		args = append(args, "--drain")
	}
	command := exec.Command(executable, args...)
	command.Stdin, command.Stdout, command.Stderr = null, logFile, logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Env = append(os.Environ(), "AIMEBU_URL="+serverURL)
	markInheritedFDsCloseOnExec()
	if err := command.Start(); err != nil {
		return 0, err
	}
	if daemonSpawned != nil {
		daemonSpawned(command.Process.Pid)
	}
	switch {
	case drain:
		log.Printf("supervisor swap: started drain instance pid %d to finish the journal %s", command.Process.Pid, filepath.Base(journal))
	case journal != "":
		log.Printf("supervisor swap: started supervisor instance pid %d on journal %s", command.Process.Pid, filepath.Base(journal))
	default:
		log.Printf("supervisor swap: started supervisor pid %d", command.Process.Pid)
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
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
	return journalHolderPID(supervisorJournalPath(root))
}

// journalHolderPID is the verified pid of the supervisor that holds one journal's lock.
func journalHolderPID(journal string) (int, bool, error) {
	var held bool
	var owner string
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		held, owner, err = supervisor.JournalOwner(journal)
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
	// instances is what the manager keeps of each supervisor instance of the machine, one journal each (supervisor_instances.go); the
	// functions after it are what its tests replace.
	instancesMu sync.Mutex
	instances   map[string]*instanceTrack
	lastSpawn   time.Time
	connections func() map[string]bool // journal ID of every connected local instance, active or retiring
	spawn       func(journal string, drain bool) error
	clock       func() time.Time
	scan        func() []instanceView
	states      func() map[string]string          // journal ID to the server's state of the instance
	recordsOf   func() []store.SupervisorInstance // the server's record of each instance, build included, for identifying a holder without an ID in its lock
	busy        func(string) (bool, error)        // whether the server sees work owned by the instance
	// Tests replace these: who holds the journal (supervisorPID), how a process is killed, when a process started, and the handover's
	// waits, clock and signal (server.SupervisorHandover).
	holder   func() (int, bool, error)
	kill     func(int) error
	startOf  func(int) string
	handover server.SupervisorHandover
	// stuckNotice stands in for the store's stuck-journal system event in tests that have no store.
	stuckNotice func(journal, debt string)
}

// killIfStuck runs the bounded handover of a supervisor that was told to stop (server.SupervisorHandover): it gets 30 seconds to release
// the journal, then SIGTERM, and 15 seconds after that it is killed, with the processes it started (killSupervisorTree), so that its
// replacement can start: the journal keeps what its turns reported, and the replacement delivers it. start is the process's start time
// when the stop was sent: a pid that belongs to another process by the time a signal is due is never signalled.
func (m *detachedSupervisorManager) killIfStuck(pid int, start string) {
	m.killIfStuckOn(pid, start, supervisorJournalPath(m.config.root))
}

// killIfStuckOn is killIfStuck for the instance that holds the given journal.
func (m *detachedSupervisorManager) killIfStuckOn(pid int, start, journal string) {
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
	handover := m.handover
	holder := m.holder
	if holder == nil {
		holder = func() (int, bool, error) { return journalHolderPID(journal) }
	}
	handover.Holds = func(p int) (bool, error) {
		current, held, err := holder()
		if err != nil {
			return false, err
		}
		return held && current == p, nil
	}
	if handover.StartOf == nil {
		handover.StartOf = m.processStart
	}
	if handover.Signal == nil {
		handover.Signal = func(p int, sig syscall.Signal) error { return syscall.Kill(p, sig) }
	}
	kill := m.kill
	if kill == nil {
		kill = func(p int) error { return killSupervisorTree(p, m.detachableJobs(ctx)) }
	}
	handover.Kill = func(p int) error {
		err := kill(p)
		if err != nil {
			fmt.Fprintln(m.stderr, "supervisor kill:", err)
		}
		return err
	}
	go func() {
		defer func() {
			m.stopMu.Lock()
			delete(m.stopWatched, pid)
			m.stopMu.Unlock()
		}()
		handover.Run(ctx, pid, start)
	}()
}

// detachableJobs are the jobs the server counts as open on this machine, for the kill of a hung supervisor to spare (killSupervisorTree).
// The supervisor's journal is not read: the server's own record gives each job's log path, and the process table (argument list and
// start time) tells whether a process is that job's. Where the list cannot be read nothing is spared.
func (m *detachedSupervisorManager) detachableJobs(ctx context.Context) []detachableJob {
	if m.store == nil || m.machineID == "" {
		return nil
	}
	open, err := m.store.OpenCommandJobs(ctx, m.machineID)
	if err != nil {
		fmt.Fprintln(m.stderr, "supervisor kill: the open jobs could not be listed, none is spared:", err)
		return nil
	}
	var jobs []detachableJob
	for _, job := range open {
		since, err := time.Parse(time.RFC3339Nano, job.StartedAt)
		if err != nil {
			since, err = time.Parse(time.RFC3339Nano, job.CreatedAt)
		}
		if err != nil {
			continue
		}
		jobs = append(jobs, detachableJob{logPath: job.LogPath, since: since})
	}
	return jobs
}

// processStart is the system's record of when pid started (supervisor.ProcessStart), unless a test replaces it.
func (m *detachedSupervisorManager) processStart(pid int) string {
	if m.startOf != nil {
		return m.startOf(pid)
	}
	return supervisor.ProcessStart(pid)
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

// restartNow is the owner's Restart now: every supervisor instance of the machine is asked to stop, the bounded handover follows each, and the
// manager's next pass starts the replacement. Without a running instance it starts one.
func (m *detachedSupervisorManager) restartNow() error {
	held := m.heldInstances()
	if len(held) == 0 {
		return startDetachedSupervisor(m.config)
	}
	var first error
	for _, v := range held {
		if err := m.stopInstance(v, "retire requested"); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// stopInstance asks one instance to exit (SIGUSR1: retire at idle with turn interrupt, SIGTERM for one that does not announce an ID, so an
// older build) and starts its bounded handover.
func (m *detachedSupervisorManager) stopInstance(v instanceView, why string) error {
	return m.stopInstanceSignal(v, why, v.journalID != "" || m.canInterrupt != nil && m.canInterrupt())
}

// stopInstanceSignal is stopInstance with the exit request chosen by the caller. A liveness handover never interrupts: a holder that
// announces no ID is an older build, and the SIGUSR1 it would otherwise get is its restart signal (it would mark its running turns as
// interrupted by the owner and stop accepting work on a connection the server has already retired), so it gets SIGTERM, a plain stop.
func (m *detachedSupervisorManager) stopInstanceSignal(v instanceView, why string, interrupt bool) error {
	stop := syscall.SIGTERM
	if interrupt {
		stop = syscall.SIGUSR1
	}
	start := m.processStart(v.pid) // read before the signal: the handover signals this very process and no later owner of its pid
	if err := syscall.Kill(v.pid, stop); err != nil {
		return err
	}
	signalName := "SIGTERM"
	if stop == syscall.SIGUSR1 {
		signalName = "SIGUSR1, retire at idle with turn interrupt"
	}
	log.Printf("supervisor swap: %s: pid %d, journal %s, stop signal %s, %s", why, v.pid, filepath.Base(v.path), signalName, m.openTurnsNote())
	m.killIfStuckOn(v.pid, start, v.path)
	return nil
}

// openTurnsNote says how many turns the local supervisor runs, for the swap log.
func (m *detachedSupervisorManager) openTurnsNote() string {
	if m.store == nil || m.machineID == "" {
		return "open turns unknown"
	}
	ids, err := m.store.MachineRunningTurnIDs(m.ctxOrBackground(), m.machineID)
	if err != nil {
		return "open turns unknown"
	}
	return fmt.Sprintf("%d open turn(s)", len(ids))
}

func (m *detachedSupervisorManager) ctxOrBackground() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

// run keeps the machine's supervisor instances going until the server stops: every pass looks at the journals and the server's view of them
// (supervisor_instances.go).
func (m *detachedSupervisorManager) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	for ctx.Err() == nil {
		m.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(instancePassEvery):
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

// heldSupervisors are the verified supervisor processes of this config directory, one per instance journal.
func heldSupervisors(root string) ([]instanceView, error) {
	var out []instanceView
	for _, path := range supervisor.InstanceJournals(filepath.Join(root, "supervisor")) {
		pid, held, err := journalHolderPID(path)
		if err != nil {
			return nil, err
		}
		if held {
			out = append(out, instanceView{path: path, held: true, pid: pid})
		}
	}
	// A legacy journal that does not exist yet is probed too, as before.
	if len(out) == 0 {
		if pid, held, err := supervisorPID(root); err != nil {
			return nil, err
		} else if held {
			out = append(out, instanceView{path: supervisorJournalPath(root), held: true, pid: pid})
		}
	}
	return out, nil
}

func stopDetachedSupervisor(root string, stdout, stderr io.Writer) int {
	held, err := heldSupervisors(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(held) == 0 {
		fmt.Fprintln(stdout, "supervisor is not running")
		return 0
	}
	for _, v := range held {
		if err := syscall.Kill(v.pid, syscall.SIGTERM); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	deadline := time.Now().Add(daemonStopLimit)
	for time.Now().Before(deadline) {
		all := true
		for _, v := range held {
			if running, _, err := supervisor.JournalOwner(v.path); err != nil || running {
				all = false
			}
		}
		if all {
			for _, v := range held {
				fmt.Fprintf(stdout, "supervisor stopped (was pid %d)\n", v.pid)
			}
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "supervisor is still stopping; check %s\n", filepath.Join(root, "supervisor", "aimebu.log"))
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
		held, err := heldSupervisors(config.root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if len(held) == 0 {
			fmt.Fprintln(stdout, "supervisor is not running")
			printWarmOrphans(stdout, config.root, time.Now())
			return 1
		}
		if len(held) == 1 {
			fmt.Fprintf(stdout, "supervisor is running (pid %d)\n", held[0].pid)
			printWarmOrphans(stdout, config.root, time.Now())
			return 0
		}
		fmt.Fprintf(stdout, "supervisor is running (%d instances side by side)\n", len(held))
		for _, v := range held {
			fmt.Fprintf(stdout, "  pid %d, journal %s\n", v.pid, filepath.Base(v.path))
		}
		printWarmOrphans(stdout, config.root, time.Now())
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

var openWarmOrphanJournal = supervisor.OpenJournal

func printWarmOrphans(out io.Writer, root string, now time.Time) {
	for _, path := range supervisor.InstanceJournals(filepath.Join(root, "supervisor")) {
		held, _, _, err := supervisor.JournalLockInfo(path)
		if err != nil {
			continue
		}
		if held {
			printHeldWarmOrphans(out, path, now)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		journal, err := openWarmOrphanJournal(ctx, path)
		if errors.Is(err, supervisor.ErrJournalInUse) {
			cancel()
			printHeldWarmOrphans(out, path, now)
			continue
		}
		if err != nil {
			cancel()
			continue
		}
		alerts, err := journal.WarmOrphansAwaitingProof(ctx, now)
		_ = journal.Close()
		cancel()
		if err != nil {
			continue
		}
		for _, alert := range alerts {
			fmt.Fprintf(out, "warm orphan awaiting proof, age %s (agent %s, journal %s)\n", alert.Age.Truncate(time.Second), alert.AgentID, filepath.Base(path))
		}
	}
}

func printHeldWarmOrphans(out io.Writer, path string, now time.Time) {
	held, pid, journalID, err := supervisor.JournalLockInfo(path)
	if err != nil || !held {
		return
	}
	status, err := supervisor.ReadWarmOrphanStatus(path)
	if err != nil || strconv.Itoa(status.PID) != pid || status.JournalID != journalID {
		return
	}
	for _, row := range status.WarmOrphans {
		age := now.Sub(time.Unix(row.SinceUnix, 0))
		if age >= time.Hour {
			fmt.Fprintf(out, "warm orphan awaiting proof, age %s (agent %s, journal %s)\n", age.Truncate(time.Second), row.AgentID, row.Journal)
		}
	}
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
