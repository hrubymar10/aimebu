package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/v2/internal/adapter/vibe"
	"github.com/hrubymar10/aimebu/v2/internal/agenttools"
	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/configroot"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/server"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
	"github.com/hrubymar10/aimebu/v2/internal/tlsclient"
	"github.com/hrubymar10/aimebu/v2/internal/usages"
	"modernc.org/sqlite"
)

// version is the build-time version string. Override with:
//
//	go build -ldflags "-X main.version=aimebu\ v2.0.0" ./cmd/aimebu
//
// Without an override, resolveVersion appends the VCS revision the Go
// toolchain embeds in the binary.
var version = "aimebu v2"

// The longest the server waits for its HTTP listeners to finish their requests, and then for its websockets to close, when it is told to
// stop. A server that was asked to stop can take both in turn, so a test waiting for it to stop must wait longer than their sum
// (serverStopLimit in testwait_test.go is derived from them).
const (
	httpShutdownLimit   = 10 * time.Second
	socketShutdownLimit = 10 * time.Second
)

// reserveV1History runs at the first start, before the owner exists and before any setup event is
// written: it raises the sequence counter to the highest v1 message number so that history keeps
// its numbers. A backup that cannot be read must not stop the server from starting: the wizard
// reports the same problem, and an import that missed the reservation refuses to run rather than
// renumber anything.
func reserveV1History(ctx context.Context, db *store.Store, root string, stderr io.Writer) error {
	backup := filepath.Join(root, v1BackupName)
	if _, statErr := os.Stat(backup); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil
		}
		return statErr
	}
	if overlap, err := db.ReserveV1MessageIDs(ctx, backup); err != nil {
		fmt.Fprintf(stderr, "warning: could not reserve v1 message numbers: %v\n", err)
	} else if overlap {
		fmt.Fprintln(stderr, "warning: v1 message numbers overlap earlier activity in this database; the import will not keep them")
	}
	return nil
}

func main() {
	// The log writer of a long command (supervisor.CommandLogMain) runs in the command's session: nothing else of the CLI starts for it.
	if len(os.Args) > 1 && os.Args[1] == "command-log" {
		os.Exit(supervisor.CommandLogMain(os.Args[2:], os.Stdin))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	startTelemetry(os.LookupEnv, os.Stderr)
	// Registered whether or not reporting is on: Settings can turn it on while the server runs (a statement costs one atomic load while off).
	store.SQLDriverName = telemetry.RegisterSQLDriver(&sqlite.Driver{})
	code := runReporting(ctx, os.Args[1:])
	stop()
	telemetry.Current().Close() // whichever reporter is running now (Settings may have replaced the first)
	os.Exit(code)
}

// runReporting is the entry point of every subcommand (server, supervisor, agent-tools, the CLI): a panic on its goroutine is reported to
// telemetry (when it is on) and then raised again, so the process dies exactly as it would without it.
func runReporting(ctx context.Context, args []string) int {
	defer telemetry.Recover(ctx)
	return runWithContext(ctx, args, os.Stdout, os.Stderr)
}

// startTelemetry turns on error and performance reporting when AIMEBU_SENTRY_DSN is set (see docs/telemetry.md); without it nothing is
// created. A bad setting is said on stderr and the process runs with telemetry off, never refusing to start for it.
func startTelemetry(lookup func(string) (string, bool), stderr io.Writer) *telemetry.Telemetry {
	config, err := telemetry.FromEnv(lookup, resolveVersion())
	if err != nil {
		fmt.Fprintln(stderr, "telemetry off:", err)
		return nil
	}
	tel, err := telemetry.Init(config.Resolve(telemetry.Stored{}), nil) // Settings are not readable yet: the server applies them once its store is open
	if err != nil {
		fmt.Fprintln(stderr, "telemetry off:", err)
		return nil
	}
	return tel
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	warnLegacyEnvironment(stderr)
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aimebu <server|supervisor|agent-tools|switcher|prune|backup|restore|doctor|self-test|sessions|usages|fe|version|help>")
		return 2
	}
	if args[0] != "help" && args[0] != "-h" && args[0] != "--help" && args[0] != "version" {
		if notice := oldDevelopmentRootNotice(); notice != "" {
			fmt.Fprintln(stderr, notice)
		}
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "prune":
		return pruneCommand(ctx, args[1:], os.Stdin, stdout, stderr)
	case "backup":
		return backupCommand(ctx, args[1:], stdout, stderr)
	case "restore":
		return restoreCommand(ctx, args[1:], stdout, stderr)
	case "import-v1":
		if os.Getenv("AIMEBU_DEV_IMPORT_V1") != "1" {
			fmt.Fprintln(stderr, "import-v1 is available only in development tests")
			return 2
		}
		return importV1Command(ctx, args[1:], stdout, stderr)
	case "switcher":
		return switcherCommand(args[1:], os.Stdin, stdout, stderr)
	case "doctor":
		return doctorCommand(ctx, args[1:], stdout, stderr)
	case "self-test":
		return selfTestCommand(ctx, args[1:], os.Stdin, stdout, stderr)
	case "sessions":
		return sessionsCommand(ctx, args[1:], stdout, stderr)
	case "usages":
		return usagesCommand(args[1:], stdout, stderr)
	case "fe":
		return feCommand(args[1:], stdout, stderr)
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu version")
			return 2
		}
		fmt.Fprintln(stdout, resolveVersion())
		return 0
	case "server":
		if len(args) == 3 && args[1] == "stop" && args[2] == "--with-supervisor" {
			return serverDaemonStopWithSupervisor(stdout, stderr)
		}
		if len(args) == 2 {
			return serverDaemonCommand(args[1], stdout, stderr)
		}
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu server [start|stop [--with-supervisor]|status]")
			return 2
		}
		return serverCommand(ctx, stdout, stderr)
	case "supervisor":
		rest, insecure := withoutFlag(args[1:], "--insecure")
		rest, local := withoutFlag(rest, "--local")
		rest, drain := withoutFlag(rest, "--drain")
		rest, journalPath, journalOK := withoutValueFlag(rest, "--journal")
		if !journalOK {
			fmt.Fprintln(stderr, "--journal needs a path")
			return 2
		}
		instance := supervisorInstanceOptions{Journal: journalPath, DrainOnly: drain}
		if (drain || journalPath != "") && len(rest) != 0 {
			fmt.Fprintln(stderr, "usage: aimebu supervisor [--local] [--journal <path>] [--drain] [--insecure]")
			return 2
		}
		if len(rest) == 1 && (rest[0] == "status" || rest[0] == "restart") && !local && !insecure {
			return supervisorControl(rest[0], stdout, stderr)
		}
		if len(rest) == 3 && rest[0] == "--pair" {
			if local {
				fmt.Fprintln(stderr, "--local cannot be paired")
				return 2
			}
			return pairSupervisorCommand(ctx, rest[1], rest[2], insecure, stderr)
		}
		if len(rest) != 0 || local && insecure {
			fmt.Fprintln(stderr, "usage: aimebu supervisor [--local|--insecure] [--journal <path>] [--drain] [--pair <server-url> <token>] | status | restart")
			return 2
		}
		return runSupervisor(ctx, insecure, stderr, local, instance)
	case "agent-tools":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu agent-tools")
			return 2
		}
		if os.Getenv("AIMEBU_URL") == "" || os.Getenv("AIMEBU_AGENT_KEY") == "" {
			fmt.Fprintln(stderr, "agent-tools requires AIMEBU_URL and AIMEBU_AGENT_KEY")
			return 1
		}
		if err := (agenttools.Server{URL: os.Getenv("AIMEBU_URL"), Key: os.Getenv("AIMEBU_AGENT_KEY"), Workdir: os.Getenv(agenttools.WorkdirEnv), Client: tlsclient.NewHTTPClient(0)}).Serve(ctx, os.Stdin, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

// resolveVersion returns the version string plus, when the binary carries VCS
// stamping (go build inside a checkout), the short revision, commit time and a
// dirty marker. A -ldflags override of version is returned unchanged.
func resolveVersion() string {
	const defaultVersion = "aimebu v2"
	if version != defaultVersion {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	var rev, stamp string
	var dirty bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
		case "vcs.time":
			stamp = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	result := version + " " + rev
	if stamp != "" {
		result += " " + stamp
	}
	if dirty {
		result += " dirty"
	}
	return result
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `aimebu — AI Message Bus

Usage: aimebu <command> [args]

Server:
  server                              Run the server in the foreground
  server start|stop [--with-supervisor]|status   Run the server in the background (pid file, log in server/aimebu.log)
  supervisor [--local|--insecure] [--pair <url> <token>]   Run a host supervisor (--local uses the host key and loopback)
  supervisor status|restart           Inspect or replace the detached local supervisor
  agent-tools                         Managed agent toolbox over stdio (needs AIMEBU_URL, AIMEBU_AGENT_KEY)

Utilities:
  doctor [flags]                      Run read-only health checks (--json --strict --live --machine)
  self-test [--full] [--yes] [--targets a,b] [--max-usd N] [--stop-above PCT]
                                      Run the live answer eval on the server's Docker launchers and models (spends quota; asks first)
  sessions                            List agent sessions known to the journal and server
  usages [provider] [--json|--plain]  Print the usage snapshots the server last cached
  switcher <command>                  Manage Claude Code / Codex credential profiles (works with the server stopped)
  prune [-a] [-y]                     Clear runtime agent state, or everything with -a (stop the server first)
  backup [--out f] [--keep N] [--include-v1-backup]  Save a verified data-root archive (contains secrets)
  restore <file> [--yes]               Replace a stopped data root and keep the original
  fe                                  Open the web UI in the browser (prints the address where there is no browser)

Other:
  version                             Print version and VCS revision
  help                                Show this help

`)
	printEnvironment(w)
	fmt.Fprint(w, `
Humans use the web UI for bus conversations. See docs/ for per-command guides.
`)
}

// withoutFlag takes one flag out of an argument list and says whether it was there.
func withoutFlag(args []string, flag string) ([]string, bool) {
	var rest []string
	found := false
	for _, arg := range args {
		if arg == flag {
			found = true
			continue
		}
		rest = append(rest, arg)
	}
	return rest, found
}

// supervisorInstanceOptions is what tells one supervisor instance of a machine from another: the journal file it owns (empty: the legacy
// supervisor/journal.sqlite) and whether it only drains a journal a dead instance left (hello says drain_only).
type supervisorInstanceOptions struct {
	Journal   string
	DrainOnly bool
}

// withoutValueFlag takes a flag and its value out of an argument list; ok is false when the flag is the last argument.
func withoutValueFlag(args []string, flag string) (rest []string, value string, ok bool) {
	ok = true
	for i := 0; i < len(args); i++ {
		if args[i] != flag {
			rest = append(rest, args[i])
			continue
		}
		if i+1 >= len(args) {
			return rest, "", false
		}
		value = args[i+1]
		i++
	}
	return rest, value, ok
}

// chooseJournalPath is the journal this supervisor opens: the one it was told (--journal), else the default one, unless another instance of this
// machine holds that, when it takes a fresh journals/<name>.sqlite next to it so that starting a new build is the whole upgrade.
func chooseJournalPath(instance supervisorInstanceOptions, defaultPath string, now time.Time, stderr io.Writer) (string, error) {
	if instance.Journal != "" {
		return instance.Journal, nil
	}
	if held, _, _, err := supervisor.JournalLockInfo(defaultPath); err != nil || !held {
		return defaultPath, nil
	}
	fresh, err := supervisor.NewInstanceJournalPath(filepath.Dir(defaultPath), now)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(stderr, "supervisor: another instance holds the default journal; starting next to it on %s\n", filepath.Base(fresh))
	return fresh, nil
}

func supervisorCommand(ctx context.Context, insecure bool, stderr io.Writer, localFlag ...bool) int {
	return runSupervisor(ctx, insecure, stderr, len(localFlag) > 0 && localFlag[0], supervisorInstanceOptions{})
}

func runSupervisor(ctx context.Context, insecure bool, stderr io.Writer, local bool, instance supervisorInstanceOptions) int {
	if tlsclient.InsecureSkipVerify() {
		fmt.Fprintln(stderr, "WARNING: AIMEBU_INSECURE_SKIP_VERIFY disables server certificate verification")
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// The same crash file the server installs: a standalone supervisor's fatal errors are recorded beside its
	// journal, and reported (or kept on disk only) once its reporting is decided — from the environment now, or
	// from the server's settings when hello_ack answers.
	pendingCrashLog, err := telemetry.InstallCrashOutput(config.root)
	if err != nil {
		fmt.Fprintln(stderr, "crash log:", err)
	}
	if telemetry.Current().Enabled() {
		if err := telemetry.ReportCrashFile(ctx, pendingCrashLog); err != nil {
			fmt.Fprintln(stderr, "crash log:", err)
		}
		pendingCrashLog = ""
	}
	if err := refuseV1RootWrite(config.root); err != nil {
		fmt.Fprintln(stderr, reportSupervisorRootError(config.root, err))
		return 1
	}
	credentialPath := filepath.Join(config.root, "supervisor", "machine.key")
	if local {
		credentialPath = filepath.Join(config.root, "server", "host-machine.key")
	}
	credential, err := readHostMachineKey(credentialPath)
	if os.IsNotExist(err) && !local {
		credential, err = readHostMachineKey(filepath.Join(config.root, "server", "host-machine.key"))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	journalPath, err := chooseJournalPath(instance, filepath.Join(config.root, "supervisor", "journal.sqlite"), time.Now(), stderr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	journal, err := supervisor.OpenJournal(ctx, journalPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer journal.Close()
	serverURL := os.Getenv("AIMEBU_URL")
	if serverURL == "" {
		if !local {
			serverURL = credential.ServerURL
		}
		if serverURL == "" {
			serverURL = "http://127.0.0.1:" + strconv.Itoa(config.port)
		}
	}
	if local {
		if err := localDialURL(serverURL); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := tlsclient.CheckTransport(serverURL, insecure); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if insecure && tlsclient.PlainHTTPToARemoteHost(serverURL) {
		fmt.Fprintln(stderr, "WARNING: --insecure: the machine key and every agent turn cross the network over plain http to "+serverURL)
	}
	if credential.ServerURL != "" {
		if err := waitForMachineApproval(ctx, serverURL, credential, insecure, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	turnLimits, err := supervisor.TurnLimitsFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fake := localFakeAdapter()
	client := supervisor.NewClient(credential.MachineID, credential.Key, serverURL, journal,
		adapter.NewMulti(fake, claude.New(), claude.NewDocker(), codex.New(filepath.Join(config.root, "supervisor", "codex")), codex.NewDocker(), pi.New(filepath.Join(config.root, "supervisor", "pi")), pi.NewDocker(), opencode.New(filepath.Join(config.root, "supervisor", "opencode")), opencode.NewDocker(), vibe.New(filepath.Join(config.root, "supervisor", "vibe")), vibe.NewDocker()))
	client.OnError = func(err error) { fmt.Fprintln(stderr, err) }
	if supervisorClientHook != nil {
		supervisorClientHook(client)
	}
	client.AllowPlainHTTP = insecure
	client.DrainOnly = instance.DrainOnly
	client.SupervisorVersion = resolveVersion()
	client.EnvironmentFingerprint = supervisor.EnvironmentFingerprint(os.Environ())
	if local {
		client.IdleDisconnectLimit = 24 * time.Hour
		client.ReconnectDelay = 5 * time.Second
	}
	client.TurnLimits = turnLimits
	client.ConfigDir = config.root
	// A long command writes its own log through this executable (its absolute path, resolved at each job's start), so it survives this
	// supervisor's exit and its replacement adopts it.
	client.CommandLogger = daemonExecutable
	// A supervisor reports the way the server tells it to (unless its own AIMEBU_SENTRY_DSN says otherwise).
	client.OnTelemetry = func(settings protocol.TelemetrySettings) {
		applySupervisorTelemetry(settings, resolveVersion(), os.LookupEnv, client, nil, stderr)
		if pendingCrashLog != "" && telemetry.Current().Enabled() {
			if err := telemetry.ReportCrashFile(ctx, pendingCrashLog); err != nil {
				fmt.Fprintln(stderr, "crash log:", err)
			}
			pendingCrashLog = ""
		}
	}
	if own, err := telemetry.FromEnv(os.LookupEnv, resolveVersion()); err == nil && own.EnvSet {
		client.SetTelemetryEnv(telemetryEnvOf(own)) // this machine's own setting, for the bridge too, whatever the server says
	}
	runCtx := ctx
	if local {
		var cancelRestart context.CancelFunc
		runCtx, cancelRestart = context.WithCancel(ctx)
		defer cancelRestart()
		restartSignal := make(chan os.Signal, 1)
		signal.Notify(restartSignal, syscall.SIGUSR1)
		defer signal.Stop(restartSignal)
		go func() {
			select {
			case <-ctx.Done():
			case <-restartSignal:
				client.MarkRestartInterruption()
				cancelRestart()
			}
		}()
	}
	runErr := client.Run(runCtx)
	// A remote supervisor gets the same bounded drain on SIGTERM as a local one: it stops accepting work, lets its running turns finish or
	// ends them, and journals their exits, so it never leaves a turn the journal still lists as running behind.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 40*time.Second)
	if err := client.StopAcceptingAndDrain(drainCtx); err != nil {
		fmt.Fprintln(stderr, "supervisor drain:", err)
		if runErr == nil {
			runErr = err
		}
	}
	cancelDrain()
	if runErr == nil {
		// A supervisor that retired with nothing owed removes its journal: no other instance adopts it.
		if removed, err := client.FinishRetire(); err != nil {
			fmt.Fprintln(stderr, "supervisor journal removal:", err)
		} else if removed {
			fmt.Fprintln(stderr, "supervisor retired: nothing owed, journal removed")
		}
	}
	if runErr != nil {
		fmt.Fprintln(stderr, runErr)
		return 1
	}
	return 0
}

// joinTokenCopies waits for the token-copy goroutines to wind down after their context was cancelled; a test replaces it
// to see that a failed start joins them too.
var joinTokenCopies = func(s *server.Server) { s.WaitTokenCopies() }

// afterV1Move runs right after v1's files moved. It does nothing; a test replaces it to fail a start at the point the
// move has already happened.
var afterV1Move = func(root string) error { return nil }

// serverStart records what a start has already done to the data directory, so a failure after v1's files were
// moved can say where they went instead of leaving the person to find out by starting v1 on an empty folder.
type serverStart struct{ movedV1From string }

func serverCommand(ctx context.Context, stdout, stderr io.Writer) int {
	start := &serverStart{}
	code := runServer(ctx, stdout, stderr, start)
	if code != 0 && start.movedV1From != "" {
		fmt.Fprint(stderr, v1MovedNotice(start.movedV1From))
	}
	return code
}

func runServer(ctx context.Context, stdout, stderr io.Writer, start *serverStart) int {
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	localSupervisor := os.Getenv("AIMEBU_EMBEDDED_SUPERVISOR") != "0"
	if localSupervisor {
		if _, err := localSupervisorURL(config); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	// A root that still holds v1's files is about to have them moved: the log starts after that, so it is never in the way of the move.
	moving, err := v1MovePending(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// What the person at a terminal must see but a log must not hold (the one-time setup token and link) goes to where the server's
	// output went before the log was attached.
	terminal := stderr
	var serverLogs serverLog
	if !moving {
		serverLogs = attachServerLog(config.root, stdout, stderr)
		stdout, stderr = serverLogs.stdout, serverLogs.stderr
		defer serverLogs.close()
	}
	warnIgnoredSupervisorMode(stderr)
	tlsConfig, err := resolveServerTLSConfig(config.port)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if tlsConfig != nil {
		if warning := tlsKeyWarning(os.Getenv("AIMEBU_TLS_KEY")); warning != "" {
			fmt.Fprintln(stderr, warning)
		}
	}
	publicURL, err := resolvePublicURL()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	listener, err := listenTCP(net.JoinHostPort(config.bind, strconv.Itoa(config.port)))
	if err != nil {
		fmt.Fprintln(stderr, listenError(config.bind, config.port, err))
		return 1
	}
	defer listener.Close()
	var localListener net.Listener
	if localSupervisor {
		localURL, _ := localSupervisorURL(config)
		localAddress := strings.TrimPrefix(localURL, "http://")
		if localAddress != listener.Addr().String() && !strings.HasPrefix(config.bind, "0.0.0.0") && config.bind != "::" {
			localListener, err = listenTCP(localAddress)
			if err != nil {
				fmt.Fprintln(stderr, "detached supervisor loopback listener:", err)
				return 1
			}
			defer localListener.Close()
		}
	}
	if hook := listenerHook.Load(); hook != nil {
		(*hook)(listener)
	}
	var tlsListener net.Listener
	if tlsConfig != nil {
		tlsListener, err = listenTLS(config.bind, tlsConfig)
		if err != nil {
			fmt.Fprintln(stderr, listenError(config.bind, tlsConfig.port, err))
			return 1
		}
		defer tlsListener.Close()
	}
	if err := checkV1CutoverPort(config.root, config.port, 9997, stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := checkV1Daemon(config.root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Everything that is certain to fail later (a missing owner login, a bad turn limit, a read-only root) is checked
	// here, before v1's files move: a start that dies after the move leaves a v1 root that looks empty.
	if moving {
		if err := preflightV1Cutover(config.root, os.Getenv); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if err := backupV1Layout(config.root); err != nil {
		fmt.Fprintln(stderr, err)
		// The move can fail half way (or resume a half-done one and fail): whatever it already renamed is in the backup
		// now, so say where, instead of leaving only the error line and a root that looks empty.
		if moving && v1FilesInBackup(config.root) {
			start.movedV1From = config.root
		}
		return 1
	}
	if moving {
		start.movedV1From = config.root
		if err := afterV1Move(config.root); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if _, err := carryV1CredentialTargets(config.root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	dir := filepath.Join(config.root, "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Fatal errors (a runtime throw, which no recover can catch) are written here besides standard error, and the
	// next start reports what a previous run left in the file, once its reporting decision is final below.
	pendingCrashLog, err := telemetry.InstallCrashOutput(config.root)
	if err != nil {
		fmt.Fprintln(stderr, "crash log:", err)
	}
	if moving {
		serverLogs = attachServerLog(config.root, stdout, stderr)
		stdout, stderr = serverLogs.stdout, serverLogs.stderr
		defer serverLogs.close()
	}
	serverLock, err := lockServerDirectory(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer unlockServerDirectory(serverLock)
	// This process is the one server of the directory now: cap its log, and record its own pid (the one the lock belongs to) for
	// `aimebu server stop|status`. The pid file goes before the lock does.
	if serverLogs.enableRotation != nil {
		serverLogs.enableRotation()
	}
	if err := writePIDFile(config.root); err != nil {
		fmt.Fprintf(stderr, "warning: no pid file (aimebu server stop and status cannot find this server): %v\n", err)
	} else {
		defer removePIDFile(config.root)
	}
	path := filepath.Join(dir, "aimebu.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		failStartup(stderr, err)
		return 1
	}
	if err := file.Close(); err != nil {
		failStartup(stderr, err)
		return 1
	}
	if err := os.Chmod(path, 0600); err != nil {
		failStartup(stderr, err)
		return 1
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		failStartup(stderr, err)
		return 1
	}
	defer db.Close()
	keyPath := filepath.Join(dir, "host-machine.key")
	users, err := db.UserCount(context.Background())
	if err != nil {
		failStartup(stderr, err)
		return 1
	}
	if users == 0 {
		if err := reserveV1History(context.Background(), db, config.root, stderr); err != nil {
			failStartup(stderr, err)
			return 1
		}
		if _, err := os.Lstat(keyPath); err == nil {
			machines, err := db.MachineCount(context.Background())
			if err != nil {
				failStartup(stderr, err)
				return 1
			}
			if machines == 0 {
				fmt.Fprintf(stderr, "orphaned host machine key at %s: no owner or machine exists in the database; it is safe to delete this key file and retry\n", keyPath)
				return 1
			}
		} else if !os.IsNotExist(err) {
			failStartup(stderr, err)
			return 1
		}
	}
	host, err := os.Hostname()
	if err != nil {
		failStartup(stderr, err)
		return 1
	}
	createOwner := func(ctx context.Context, login, password string) (string, bool, error) {
		keyWritten := false
		ownerID, machineID, _, created, err := auth.NewService(db).BootstrapWithKeySink(ctx, login, password, host,
			func(machineID, machineKey string) error {
				if err := writeHostMachineKey(keyPath, machineID, machineKey); err != nil {
					return err
				}
				keyWritten = true
				return nil
			})
		if err != nil {
			if keyWritten {
				_ = os.Remove(keyPath)
			}
			return "", false, err
		}
		if created {
			fmt.Fprintf(stdout, "Owner initialized. Host machine: %s\nHost machine key file: %s\n", machineID, keyPath)
		}
		return ownerID, created, nil
	}
	ownerLogin, ownerPassword := os.Getenv("AIMEBU_OWNER_LOGIN"), os.Getenv("AIMEBU_OWNER_PASSWORD")
	var ownerSetup *auth.OwnerSetup
	// startLocalSupervisor is set below; web setup calls it once the host machine and its key exist.
	var startLocalSupervisor func() error
	// The token file sits in the config root, where a person looks first (and the setup screen names its full path).
	setupRoot, absErr := filepath.Abs(config.root)
	if absErr != nil {
		setupRoot = config.root
	}
	setupPath := filepath.Join(setupRoot, setupTokenFile)
	_ = os.Remove(filepath.Join(dir, "setup-token")) // where an earlier build kept it
	if users == 0 && strings.TrimSpace(ownerLogin) == "" && ownerPassword == "" {
		// No owner and no environment to make one from: wait for the person at the keyboard (first-run setup in the web UI).
		var token string
		ownerSetup, token, err = auth.NewOwnerSetup(setupPath, func(ctx context.Context, login, password string) (string, error) {
			ownerID, created, err := createOwner(ctx, login, password)
			if err == nil && !created {
				return "", auth.ErrSetupClosed
			}
			// The local supervisor needs the host machine key that setup just wrote. It starts in the background: the person is
			// waiting for the next screen.
			if err == nil && startLocalSupervisor != nil {
				telemetry.Go(ctx, func() {
					if serr := startLocalSupervisor(); serr != nil {
						fmt.Fprintln(stderr, "supervisor:", serr)
					}
				})
			}
			return ownerID, err
		})
		if err != nil {
			failStartup(stderr, err)
			return 1
		}
		defer ownerSetup.Close()
		fmt.Fprintf(terminal, "No owner exists yet. Open the setup page and enter this setup token (single use, valid until the server restarts):\n  %s\nThe token is also in %s\nShortcut that fills the token in for you:\n  %s\n",
			token, setupPath, setupLink(publicURL, config.bind, config.port, tlsConfig, token))
	} else {
		if users == 0 {
			if err := auth.ValidateOwnerCredentials(ownerLogin, ownerPassword); err != nil {
				fmt.Fprintf(stderr, "AIMEBU_OWNER_LOGIN / AIMEBU_OWNER_PASSWORD: %v\n", err)
				return 1
			}
		}
		if _, _, err := createOwner(context.Background(), ownerLogin, ownerPassword); err != nil {
			failStartup(stderr, err)
			return 1
		}
		// An owner now exists, so a token left by an earlier start without one is dead weight.
		_ = os.Remove(setupPath)
	}
	localEnabled := ctx.Err() == nil && localSupervisor
	if localEnabled {
		if host, err := db.HostMachine(ctx); err == nil && host.Disabled {
			localEnabled = false
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			failStartup(stderr, err)
			return 1
		}
	}
	if localSupervisor {
		// The supervisor reads these itself; a value it would reject fails the server's start, where the person sees it.
		if _, err := supervisor.TurnLimitsFromEnv(os.Getenv); err != nil {
			failStartup(stderr, err)
			return 1
		}
		localURL, _ := localSupervisorURL(config)
		parsed, _ := url.Parse(localURL)
		allowSupervisorAddress(&config, parsed.Hostname())
	}
	service := server.New(db)
	service.Setup = ownerSetup
	service.ImportV1Root = config.root
	service.Version = resolveVersion()
	if err := service.ApplyTelemetry(context.Background()); err != nil {
		fmt.Fprintln(stderr, "telemetry off:", err)
	}
	if telemetry.Current().Enabled() {
		fmt.Fprintln(stderr, "telemetry on: reporting errors and traces to Sentry (see docs/telemetry.md)")
	}
	if err := telemetry.ReportCrashFile(context.Background(), pendingCrashLog); err != nil {
		fmt.Fprintln(stderr, "crash log:", err)
	}
	service.DataRoot = config.root
	var localManager *detachedSupervisorManager
	if localSupervisor {
		localManager = &detachedSupervisorManager{config: config, stderr: stderr, store: db, ctx: ctx, connected: service.LocalSupervisorConnected, canInterrupt: service.LocalSupervisorCanInterrupt, replaceOverdue: service.ReplaceOverdueLocalSupervisor}
		localManager.connections = service.LocalSupervisorConnections
		service.LocalSupervisorEnvironment = supervisor.EnvironmentFingerprint(os.Environ())
		service.LocalSupervisorDiskVersion = onDiskSupervisorVersion
		service.LocalSupervisorRestart = localManager.restartNow
		service.LocalSupervisorLegacyDrain = localManager.retireLegacyAtIdle
		service.LocalSupervisorLegacyDrainCancel = localManager.cancelLegacyDrain
		if credential, keyErr := readHostMachineKey(keyPath); keyErr == nil {
			service.SetLocalSupervisorID(credential.MachineID)
			localManager.machineID = credential.MachineID
		}
	}
	// `aimebu doctor --live` reads this key from the config directory; the server works without it, only --live is then unavailable.
	if removeDoctorKey, err := service.EnableDoctorKey(config.root); err != nil {
		fmt.Fprintln(stderr, "doctor key:", err)
	} else {
		defer removeDoctorKey()
	}
	service.Auth.PublicURL = publicURL
	if tlsConfig != nil {
		service.TLSPort = tlsConfig.port
	}
	switcherRoot := filepath.Join(config.root, "switcher")
	service.Switcher, err = switcher.New(switcherRoot)
	if err != nil {
		failStartup(stderr, err)
		return 1
	}
	service.Allow = config.allow
	service.TrustedProxies = config.trusted
	allowedHosts, err := server.ResolveAllowedHosts(os.Getenv("AIMEBU_ALLOWED_HOSTS"))
	if err != nil {
		fmt.Fprintln(stderr, "AIMEBU_ALLOWED_HOSTS:", err)
		return 1
	}
	service.AllowedHosts = allowedHosts
	logAllowStartup(stderr, config, listener.Addr().String())
	logProxyStartup(stderr, publicURL, config.trusted)
	if tlsListener != nil {
		fmt.Fprintf(stderr, "HTTPS listening on %s\n", tlsListener.Addr())
	}
	if os.Getenv("AIMEBU_USAGES") != "0" {
		usageReader, err := usages.New(filepath.Join(config.root, "usages"))
		if err != nil {
			listener.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		usageReader.SetProfiles(service.Switcher.UsageProfiles, service.Switcher.WithLock)
		service.Usages = usageReader
		if err := service.StartUsageAlerts(ctx, filepath.Join(config.root, "usages")); err != nil {
			listener.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		telemetry.Go(ctx, func() { usageReader.Run(ctx) })
	}
	// After the usage reader exists (the refresher reads its model-flag setting)
	// and before the handler serves, so the Switch hook is in place for every request.
	// Its own cancel: the refresher must stop on every exit path (a failed listener included), not only on a signal,
	// and the shutdown below joins it so no refresh is cut off while it is spending a login.
	tokenCtx, cancelTokenCopies := context.WithCancel(ctx)
	// stopTokenCopies cancels and then joins: every return below this point, the early failures included, waits for an
	// in-flight refresh to commit and for the live-file claims to be released. It is idempotent, so the normal shutdown
	// calls it explicitly and the defer covers the rest.
	stopTokenCopies := func() { cancelTokenCopies(); joinTokenCopies(service) }
	defer stopTokenCopies()
	service.StartTokenCopies(tokenCtx)
	// Human presence: the sweep that announces online/offline transitions of people (their member cards go grey ten minutes after their
	// last real activity). It shares the server's lifetime context.
	service.StartHumanPresence(ctx)
	service.StartQuotaSwitcher(ctx)
	// A pool changed while the server was down, or by an older build, may have left agents on a model it no longer lists: raise the Inbox item.
	if err := service.Store.DetectPoolChanges(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "pool change detection: %v\n", err)
	}
	service.StartUpdateCheck(ctx)
	handler := service.Handler()
	// With https configured the plain listener sends other machines to it; this machine's own clients (the local supervisor, the command
	// line) keep using it.
	httpServer := &http.Server{Handler: service.RedirectPlainToHTTPS(handler), ReadHeaderTimeout: 10 * time.Second}
	var httpsServer *http.Server
	if tlsListener != nil {
		httpsServer = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	}
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	telemetry.Go(ctx, func() {
		defer close(dispatchDone)
		// The dispatcher is woken by every committed change that can create
		// dispatch work; the tick is only the fallback that catches what no write
		// covers (a schedule falling due, a sweep's effect, a lost signal).
		server.NewDispatcher(service).Run(dispatchCtx, 2*time.Second, func(err error) { fmt.Fprintln(stderr, "dispatch:", err) })
	})
	var localMu sync.Mutex
	// stopping is set at shutdown: a start that is still pending (it runs in the background) must not begin after that, or it would start
	// a supervisor for a server that is gone.
	stopping := false
	localStarted := false
	managerRunning := false
	startLocalSupervisor = func() error {
		localMu.Lock()
		defer localMu.Unlock()
		if localStarted || stopping || ctx.Err() != nil {
			return nil
		}
		credential, keyErr := readHostMachineKey(keyPath)
		if keyErr != nil {
			return keyErr
		}
		service.SetLocalSupervisorID(credential.MachineID)
		localManager.machineID = credential.MachineID
		host, err := db.HostMachine(ctx)
		if err != nil {
			return err
		}
		if host.Disabled {
			return nil
		}
		localSupervisorStarts.Add(1)
		if err := startDetachedSupervisor(config); err != nil {
			return err
		}
		localStarted = true
		if !managerRunning {
			managerRunning = true
			telemetry.Go(ctx, func() { localManager.run(ctx) })
		}
		return nil
	}
	if localManager != nil {
		service.LocalSupervisorDisable = func() {
			localMu.Lock()
			localStarted = false
			localMu.Unlock()
			localManager.pass(ctx)
		}
		service.LocalSupervisorEnable = startLocalSupervisor
		if !localEnabled {
			managerRunning = true
			telemetry.Go(ctx, func() { localManager.run(ctx) })
		}
	}
	// With an owner already in place the host key exists, so a missing or unreadable one is still a failed start, and so is a
	// journal lock held by something that is not an aimebu supervisor: both are checked before listening. The supervisor itself starts
	// (or is adopted) after the server is listening, because the pages should not wait for it; in first-run setup it starts when the
	// owner is created, because the key does not exist before that.
	startSupervisorAfterServe := false
	if localEnabled && !ownerSetup.Open() {
		if _, err := readHostMachineKey(keyPath); err != nil {
			stopDispatch()
			<-dispatchDone
			listener.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, _, err := supervisorPID(config.root); err != nil {
			stopDispatch()
			<-dispatchDone
			listener.Close()
			fmt.Fprintln(stderr, err, "Stop the server and run `aimebu server stop --with-supervisor` to clear the local supervisor.")
			return 1
		}
		startSupervisorAfterServe = true
	}
	if !localSupervisor {
		startLocalSupervisor = nil
	}
	serveDone := make(chan error, 3)
	telemetry.Go(ctx, func() { serveDone <- httpServer.Serve(listener) })
	if localListener != nil {
		telemetry.Go(ctx, func() { serveDone <- httpServer.Serve(localListener) })
	}
	if startSupervisorAfterServe {
		telemetry.Go(ctx, func() {
			if hook := supervisorStartHook.Load(); hook != nil {
				(*hook)()
			}
			if err := startLocalSupervisor(); err != nil {
				fmt.Fprintln(stderr, "supervisor:", err)
			}
			if hook := supervisorStartFinished.Load(); hook != nil {
				(*hook)()
			}
		})
	}
	remaining := 1
	if localListener != nil {
		remaining++
	}
	if httpsServer != nil {
		remaining++
		telemetry.Go(ctx, func() { serveDone <- httpsServer.Serve(tlsListener) })
	}
	if hook := servingHook.Load(); hook != nil {
		(*hook)()
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveDone:
		remaining--
	}
	stopDispatch()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), httpShutdownLimit)
	var httpsShutdownDone chan struct{}
	if httpsServer != nil {
		httpsShutdownDone = make(chan struct{})
		go func() {
			_ = httpsServer.Shutdown(shutdownCtx)
			close(httpsShutdownDone)
		}()
	}
	_ = httpServer.Shutdown(shutdownCtx)
	if httpsShutdownDone != nil {
		<-httpsShutdownDone
	}
	cancelShutdown()
	for i := 0; i < remaining; i++ {
		if err := <-serveDone; err != nil && err != http.ErrServerClosed && (serveErr == nil || serveErr == http.ErrServerClosed) {
			serveErr = err
		}
	}
	<-dispatchDone
	stopTokenCopies()
	localMu.Lock()
	stopping = true
	localMu.Unlock()
	// The local supervisor is a process of its own: it keeps its turns running and reconnects to the next server.
	socketShutdownCtx, cancelSocketShutdown := context.WithTimeout(context.Background(), socketShutdownLimit)
	if err := service.ShutdownSockets(socketShutdownCtx); err != nil {
		fmt.Fprintln(stderr, "websocket shutdown:", err)
	}
	cancelSocketShutdown()
	if serveErr != nil && serveErr != http.ErrServerClosed {
		fmt.Fprintln(stderr, serveErr)
		return 1
	}
	return 0
}

type hostMachineCredential struct {
	MachineID string `json:"machine_id"`
	Key       string `json:"key"`
	ServerURL string `json:"server_url,omitempty"`
}

func readHostMachineKey(path string) (hostMachineCredential, error) {
	info, err := os.Stat(path)
	if err != nil {
		return hostMachineCredential{}, err
	}
	if info.Mode().Perm() != 0600 {
		return hostMachineCredential{}, fmt.Errorf("host machine key file must be mode 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return hostMachineCredential{}, err
	}
	var credential hostMachineCredential
	if err := json.Unmarshal(data, &credential); err != nil || credential.MachineID == "" || credential.Key == "" {
		return hostMachineCredential{}, fmt.Errorf("invalid host machine key file")
	}
	return credential, nil
}

type serverConfig struct {
	root, bind   string
	port         int
	allow        []netip.Prefix
	allowDefault bool
	trusted      []netip.Prefix
}

func resolveServerConfig() (serverConfig, error) {
	root, err := configroot.FromEnv()
	if err != nil {
		return serverConfig{}, err
	}
	bind := os.Getenv("AIMEBU_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	if net.ParseIP(bind) == nil {
		return serverConfig{}, fmt.Errorf("AIMEBU_BIND must be an IP address")
	}
	port := 9997
	if raw := os.Getenv("AIMEBU_PORT"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 65535 {
			return serverConfig{}, fmt.Errorf("AIMEBU_PORT must be a TCP port")
		}
		port = n
	}
	allowRaw := os.Getenv("AIMEBU_ALLOW")
	allow, err := server.ResolveAllow(allowRaw)
	if err != nil {
		return serverConfig{}, err
	}
	trusted, err := server.ResolveTrustedProxies(os.Getenv("AIMEBU_TRUSTED_PROXIES"))
	if err != nil {
		return serverConfig{}, err
	}
	return serverConfig{root: root, bind: bind, port: port, allow: allow, allowDefault: allowRaw == "", trusted: trusted}, nil
}

// resolvePublicURL reads AIMEBU_PUBLIC_URL as a bare origin; empty when unset.
func resolvePublicURL() (string, error) {
	raw := os.Getenv("AIMEBU_PUBLIC_URL")
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("AIMEBU_PUBLIC_URL must be an HTTP or HTTPS origin without a path, credentials, query, or fragment")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// proxyThrottleWarning explains, when a public URL says a reverse proxy is in
// front but no proxy is trusted, that every proxied client shares the proxy's
// address for login, invitation and pairing throttling.
func proxyThrottleWarning(publicURL string, trusted []netip.Prefix) string {
	if publicURL == "" || len(trusted) > 0 {
		return ""
	}
	return "AIMEBU_PUBLIC_URL is set but AIMEBU_TRUSTED_PROXIES is empty; if a reverse proxy forwards to this server, all proxied clients share the proxy's IP as one throttle identity, so one client's failed logins can delay everyone's. Set AIMEBU_TRUSTED_PROXIES to the proxy's IPs or CIDRs."
}

func logProxyStartup(w io.Writer, publicURL string, trusted []netip.Prefix) {
	if warning := proxyThrottleWarning(publicURL, trusted); warning != "" {
		fmt.Fprintln(w, "WARNING: "+warning)
	}
}

func logAllowStartup(w io.Writer, config serverConfig, address string) {
	entries := make([]string, len(config.allow))
	for i, prefix := range config.allow {
		entries[i] = prefix.String()
	}
	fmt.Fprintf(w, "Listening on %s; AIMEBU_ALLOW=%s\n", address, strings.Join(entries, ","))
	if config.allowDefault && !net.ParseIP(config.bind).IsLoopback() {
		fmt.Fprintln(w, "WARNING: AIMEBU_BIND is non-loopback but AIMEBU_ALLOW defaults to loopback; remote clients will receive 403. Set AIMEBU_ALLOW to include their IPs or CIDRs.")
	}
}

// allowSupervisorAddress admits the local supervisor's loopback address even when AIMEBU_ALLOW leaves it out.
func allowSupervisorAddress(config *serverConfig, address string) {
	ip := netip.MustParseAddr(address).Unmap()
	for _, prefix := range config.allow {
		if prefix.Contains(ip) {
			return
		}
	}
	bits := 32
	if ip.Is6() {
		bits = 128
	}
	config.allow = append(config.allow, netip.PrefixFrom(ip, bits))
}

func writeHostMachineKey(path, machineID, key string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("host machine key file already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".host-machine-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(map[string]string{"machine_id": machineID, "key": key}); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Link creates the destination only if absent; Rename could replace an
	// existing credential created by another bootstrap process.
	return os.Link(file.Name(), path)
}

// listenError explains a failed listen. "Address already in use" only says some process holds the port; v1's server is the usual
// suspect on 9997 but not the only one, so it is named as a possibility rather than as the cause.
func listenError(bind string, port int, err error) string {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Sprintf("cannot listen on %s: port %d is already in use by another process (%v). If it is the v1 aimebu server, stop it and start v2 again; otherwise stop that process or choose another port with AIMEBU_PORT.", net.JoinHostPort(bind, strconv.Itoa(port)), port, err)
	}
	return fmt.Sprintf("cannot listen on %s: %v", net.JoinHostPort(bind, strconv.Itoa(port)), err)
}

// supervisorStartHook runs just before the local supervisor starts after the server is listening; tests hold it to see the server answer first.
// listenerHook receives the HTTP listener, so a test can close it and make Serve fail. localSupervisorStarts counts starts that really began.
// listenOverride replaces net.Listen for the server's own listeners (HTTP and HTTPS). It is nil in production. A test that needs a port the
// server under test will bind used to find one by listening on :0 and closing, then pass the number on; anything on the machine could take
// the port between the close and the server's bind. The test now keeps its listener open and hands it over here (see holdFreePorts).
var listenOverride atomic.Pointer[func(address string) (net.Listener, error)]

// listenTCP listens on a TCP address, through listenOverride when a test has set one.
func listenTCP(address string) (net.Listener, error) {
	if hook := listenOverride.Load(); hook != nil {
		return (*hook)(address)
	}
	return net.Listen("tcp", address)
}

var (
	supervisorStartHook   atomic.Pointer[func()]
	listenerHook          atomic.Pointer[func(net.Listener)]
	localSupervisorStarts atomic.Int32
	// servingHook runs once every listener is being served: startup (migrations, the owner's password hash) is over and a request is answered.
	servingHook atomic.Pointer[func()]
	// supervisorStartFinished runs when the background start has returned, whatever it did.
	supervisorStartFinished atomic.Pointer[func()]
)

const setupTokenFile = ".setup_token"

// setupLink is the URL printed for the first-run owner setup: the public URL when one is configured, otherwise the
// address the server listens on (HTTPS when TLS is on).
func setupLink(publicURL, bind string, port int, tlsConfig *serverTLSConfig, token string) string {
	base := strings.TrimRight(publicURL, "/")
	if base == "" {
		scheme, p := "http", port
		if tlsConfig != nil {
			scheme, p = "https", tlsConfig.port
		}
		host := bind
		if ip, err := netip.ParseAddr(bind); err == nil && ip.IsUnspecified() {
			host = "localhost"
		}
		base = scheme + "://" + net.JoinHostPort(host, strconv.Itoa(p))
	}
	return base + "/setup#token=" + token
}

// failStartup is how the server says it cannot start: to stderr, and to Sentry. It flushes: the process is about to exit, and an event still queued is lost.
func failStartup(stderr io.Writer, err error) {
	fmt.Fprintln(stderr, err)
	telemetry.ReportError(context.Background(), "startup", err)
	telemetry.Current().Flush(2 * time.Second)
}
