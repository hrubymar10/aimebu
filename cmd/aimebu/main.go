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
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/adapter/claude"
	"github.com/hrubymar10/aimebu/internal/adapter/codex"
	"github.com/hrubymar10/aimebu/internal/adapter/opencode"
	"github.com/hrubymar10/aimebu/internal/adapter/pi"
	"github.com/hrubymar10/aimebu/internal/agenttools"
	"github.com/hrubymar10/aimebu/internal/auth"
	"github.com/hrubymar10/aimebu/internal/server"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/supervisor"
	"github.com/hrubymar10/aimebu/internal/switcher"
	"github.com/hrubymar10/aimebu/internal/tlsclient"
	"github.com/hrubymar10/aimebu/internal/usages"
)

// version is the build-time version string. Override with:
//
//	go build -ldflags "-X main.version=aimebu\ v2.0.0" ./cmd/aimebu
//
// Without an override, resolveVersion appends the VCS revision the Go
// toolchain embeds in the binary.
var version = "aimebu v2 (development)"

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aimebu <server|supervisor|agent-tools|switcher|prune|doctor|sessions|usages|version|help>")
		return 2
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "prune":
		return pruneCommand(ctx, args[1:], os.Stdin, stdout, stderr)
	case "import-v1":
		if os.Getenv("AIMEBU2_DEV_IMPORT_V1") != "1" {
			fmt.Fprintln(stderr, "import-v1 is available only in development tests")
			return 2
		}
		return importV1Command(ctx, args[1:], stdout, stderr)
	case "switcher":
		return switcherCommand(args[1:], os.Stdin, stdout, stderr)
	case "doctor":
		return doctorCommand(ctx, args[1:], stdout, stderr)
	case "sessions":
		return sessionsCommand(ctx, args[1:], stdout, stderr)
	case "usages":
		return usagesCommand(args[1:], stdout, stderr)
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu version")
			return 2
		}
		fmt.Fprintln(stdout, resolveVersion())
		return 0
	case "server":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu server")
			return 2
		}
		return serverCommand(ctx, stdout, stderr)
	case "supervisor":
		if len(args) == 4 && args[1] == "--pair" {
			return pairSupervisorCommand(ctx, args[2], args[3], stderr)
		}
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu supervisor [--pair <server-url> <token>]")
			return 2
		}
		return supervisorCommand(ctx, stderr)
	case "agent-tools":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: aimebu agent-tools")
			return 2
		}
		if os.Getenv("AIMEBU2_URL") == "" || os.Getenv("AIMEBU2_AGENT_KEY") == "" {
			fmt.Fprintln(stderr, "agent-tools requires AIMEBU2_URL and AIMEBU2_AGENT_KEY")
			return 1
		}
		if err := (agenttools.Server{URL: os.Getenv("AIMEBU2_URL"), Key: os.Getenv("AIMEBU2_AGENT_KEY"), Client: tlsclient.NewHTTPClient(0)}).Serve(ctx, os.Stdin, stdout); err != nil {
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
	const defaultVersion = "aimebu v2 (development)"
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
	fmt.Fprint(w, `aimebu v2 — AI Message Bus

Usage: aimebu <command> [args]

Server:
  server                              Run the server in the foreground (embedded supervisor)
  supervisor [--pair <url> <token>]   Run a standalone host supervisor, or pair it with a server
  agent-tools                         Managed agent toolbox over stdio (needs AIMEBU2_URL, AIMEBU2_AGENT_KEY)

Utilities:
  doctor [--json]                     Run read-only health checks and print a report
  sessions                            List agent sessions known to the journal and server
  usages [provider] [--json|--plain]  Print the usage snapshots the server last cached
  switcher <command>                  Manage Claude Code / Codex credential profiles (works with the server stopped)
  prune [-a] [-y]                     Clear runtime agent state, or everything with -a (stop the server first)

Other:
  version                             Print version and VCS revision
  help                                Show this help

`)
	printEnvironment(w)
	fmt.Fprint(w, `
Humans use the web UI for bus conversations. See docs/ for per-command guides.
`)
}

func supervisorCommand(ctx context.Context, stderr io.Writer) int {
	if tlsclient.InsecureSkipVerify() {
		fmt.Fprintln(stderr, "WARNING: AIMEBU2_INSECURE_SKIP_VERIFY disables server certificate verification")
	}
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	credentialPath := filepath.Join(config.root, "supervisor", "machine.key")
	credential, err := readHostMachineKey(credentialPath)
	if os.IsNotExist(err) {
		credential, err = readHostMachineKey(filepath.Join(config.root, "server", "host-machine.key"))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	journal, err := supervisor.OpenJournal(ctx, filepath.Join(config.root, "supervisor", "journal.sqlite"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer journal.Close()
	serverURL := os.Getenv("AIMEBU2_URL")
	if serverURL == "" {
		serverURL = credential.ServerURL
		if serverURL == "" {
			serverURL = "http://127.0.0.1:" + strconv.Itoa(config.port)
		}
	}
	if credential.ServerURL != "" {
		if err := waitForMachineApproval(ctx, serverURL, credential, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	turnLimits, err := supervisor.TurnLimitsFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fake := adapter.NewFake()
	fake.SetEchoDefault(true)
	client := supervisor.NewClient(credential.MachineID, credential.Key, serverURL, journal,
		adapter.NewMulti(fake, claude.New(), claude.NewDocker(), codex.New(filepath.Join(config.root, "supervisor", "codex")), codex.NewDocker(), pi.New(filepath.Join(config.root, "supervisor", "pi")), pi.NewDocker(), opencode.New(filepath.Join(config.root, "supervisor", "opencode"))))
	client.OnError = func(err error) { fmt.Fprintln(stderr, err) }
	client.TurnLimits = turnLimits
	client.ConfigDir = config.root
	if err := client.Run(ctx); err != nil {
		fmt.Fprintln(stderr, err)
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
	tlsConfig, err := resolveServerTLSConfig(config.port)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	publicURL, err := resolvePublicURL()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(config.bind, strconv.Itoa(config.port)))
	if err != nil {
		fmt.Fprintln(stderr, listenError(config.bind, config.port, err))
		return 1
	}
	defer listener.Close()
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
	moving, err := v1MovePending(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
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
	serverLock, err := lockServerDirectory(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer unlockServerDirectory(serverLock)
	path := filepath.Join(dir, "aimebu.sqlite")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := file.Close(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.Chmod(path, 0600); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer db.Close()
	keyPath := filepath.Join(dir, "host-machine.key")
	users, err := db.UserCount(context.Background())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if users == 0 {
		if err := reserveV1History(context.Background(), db, config.root, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, err := os.Lstat(keyPath); err == nil {
			machines, err := db.MachineCount(context.Background())
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if machines == 0 {
				fmt.Fprintf(stderr, "orphaned host machine key at %s: no owner or machine exists in the database; it is safe to delete this key file and retry\n", keyPath)
				return 1
			}
		} else if !os.IsNotExist(err) {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	host, err := os.Hostname()
	if err != nil {
		fmt.Fprintln(stderr, err)
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
	ownerLogin, ownerPassword := os.Getenv("AIMEBU2_OWNER_LOGIN"), os.Getenv("AIMEBU2_OWNER_PASSWORD")
	var ownerSetup *auth.OwnerSetup
	// startEmbedded is set below; web setup calls it once the host machine and its key exist.
	var startEmbedded func() error
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
			// The embedded supervisor needs the host machine key that setup just wrote. It starts in the background: reading each
			// tool's version and opening its journal take seconds, and the person is waiting for the next screen.
			if err == nil && startEmbedded != nil {
				go func() {
					if serr := startEmbedded(); serr != nil {
						fmt.Fprintln(stderr, "supervisor:", serr)
					}
				}()
			}
			return ownerID, err
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer ownerSetup.Close()
		fmt.Fprintf(stderr, "No owner exists yet. Open the setup page and enter this setup token (single use, valid until the server restarts):\n  %s\nThe token is also in %s\nShortcut that fills the token in for you:\n  %s\n",
			token, setupPath, setupLink(publicURL, config.bind, config.port, tlsConfig, token))
	} else {
		if users == 0 {
			if err := auth.ValidateOwnerCredentials(ownerLogin, ownerPassword); err != nil {
				fmt.Fprintf(stderr, "AIMEBU2_OWNER_LOGIN / AIMEBU2_OWNER_PASSWORD: %v\n", err)
				return 1
			}
		}
		if _, _, err := createOwner(context.Background(), ownerLogin, ownerPassword); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		// An owner now exists, so a token left by an earlier start without one is dead weight.
		_ = os.Remove(setupPath)
	}
	embeddedEnabled := ctx.Err() == nil && os.Getenv("AIMEBU2_EMBEDDED_SUPERVISOR") != "0"
	var turnLimits supervisor.TurnLimitOverrides
	if embeddedEnabled {
		turnLimits, err = supervisor.TurnLimitsFromEnv(os.Getenv)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	embeddedAddress := ""
	if embeddedEnabled {
		embeddedAddress = allowEmbeddedSupervisor(&config)
	}
	service := server.New(db)
	service.Setup = ownerSetup
	service.ImportV1Root = config.root
	service.DataRoot = config.root
	service.Auth.PublicURL = publicURL
	if tlsConfig != nil {
		service.TLSPort = tlsConfig.port
	}
	switcherRoot := filepath.Join(config.root, "switcher")
	service.Switcher, err = switcher.New(switcherRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	service.Allow = config.allow
	service.TrustedProxies = config.trusted
	logAllowStartup(stderr, config, listener.Addr().String())
	logProxyStartup(stderr, publicURL, config.trusted)
	if tlsListener != nil {
		fmt.Fprintf(stderr, "HTTPS listening on %s\n", tlsListener.Addr())
	}
	if os.Getenv("AIMEBU2_USAGES") != "0" {
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
		go usageReader.Run(ctx)
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
	service.StartQuotaSwitcher(ctx)
	handler := service.Handler()
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	var httpsServer *http.Server
	if tlsListener != nil {
		httpsServer = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	}
	dispatchCtx, stopDispatch := context.WithCancel(context.Background())
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		server.NewDispatcher(service).Run(dispatchCtx, 100*time.Millisecond, func(err error) { fmt.Fprintln(stderr, "dispatch:", err) })
	}()
	var embedded *supervisor.Client
	var journal *supervisor.Journal
	var stopEmbedded context.CancelFunc
	var embeddedDone chan error
	var embeddedMu sync.Mutex
	// stopping is set at shutdown, before the final read of the supervisor's state: a start that is still pending (it runs in the
	// background) must not begin after that read, or it would be left running with nobody to stop it.
	stopping := false
	var preparedJournal *supervisor.Journal
	startEmbedded = func() error {
		embeddedMu.Lock()
		defer embeddedMu.Unlock()
		if embedded != nil || stopping || ctx.Err() != nil {
			return nil
		}
		credential, err := readHostMachineKey(keyPath)
		if err != nil {
			return err
		}
		// A normal start opened the journal before listening (a journal that cannot be opened is a failed start); first-run setup
		// has not yet, and opens it here.
		opened := preparedJournal
		if opened == nil {
			if opened, err = supervisor.OpenJournal(ctx, filepath.Join(config.root, "supervisor", "journal.sqlite")); err != nil {
				return err
			}
		}
		fake := adapter.NewFake()
		fake.SetEchoDefault(true)
		client := supervisor.NewClient(credential.MachineID, credential.Key,
			"http://"+net.JoinHostPort(embeddedAddress, strconv.Itoa(config.port)), opened,
			adapter.NewMulti(fake, claude.New(), claude.NewDocker(), codex.New(filepath.Join(config.root, "supervisor", "codex")), codex.NewDocker(), pi.New(filepath.Join(config.root, "supervisor", "pi")), pi.NewDocker(), opencode.New(filepath.Join(config.root, "supervisor", "opencode"))))
		client.OnError = func(err error) { fmt.Fprintln(stderr, "supervisor:", err) }
		client.TurnLimits = turnLimits
		client.ConfigDir = config.root
		var embeddedCtx context.Context
		embeddedStarts.Add(1)
		embeddedCtx, stopEmbedded = context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- client.Run(embeddedCtx) }()
		embedded, journal, embeddedDone = client, opened, done
		return nil
	}
	defer func() {
		embeddedMu.Lock()
		defer embeddedMu.Unlock()
		if stopEmbedded != nil {
			stopEmbedded()
		}
	}()
	// With an owner already in place the host key exists, so a missing or unreadable one is still a failed start, and so is a
	// journal that cannot be opened: both are checked before listening. The supervisor itself (reading each tool's version, then
	// connecting) starts after the server is listening, because the pages should not wait for it; in first-run setup everything
	// starts when the owner is created, because the key does not exist before that.
	startSupervisorAfterServe := false
	if embeddedEnabled && !ownerSetup.Open() {
		if _, err := readHostMachineKey(keyPath); err != nil {
			stopDispatch()
			<-dispatchDone
			listener.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		var journalErr error
		if preparedJournal, journalErr = supervisor.OpenJournal(ctx, filepath.Join(config.root, "supervisor", "journal.sqlite")); journalErr != nil {
			stopDispatch()
			<-dispatchDone
			listener.Close()
			fmt.Fprintln(stderr, journalErr)
			return 1
		}
		startSupervisorAfterServe = true
	}
	if !embeddedEnabled {
		startEmbedded = nil
	}
	serveDone := make(chan error, 2)
	go func() { serveDone <- httpServer.Serve(listener) }()
	if startSupervisorAfterServe {
		go func() {
			if hook := embeddedStartHook.Load(); hook != nil {
				(*hook)()
			}
			if err := startEmbedded(); err != nil {
				fmt.Fprintln(stderr, "supervisor:", err)
			}
			if hook := embeddedStartFinished.Load(); hook != nil {
				(*hook)()
			}
		}()
	}
	remaining := 1
	if httpsServer != nil {
		remaining++
		go func() { serveDone <- httpsServer.Serve(tlsListener) }()
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
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
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
	embeddedMu.Lock()
	stopping = true
	finalClient, finalJournal, finalStop, finalDone := embedded, journal, stopEmbedded, embeddedDone
	unusedJournal := preparedJournal
	embeddedMu.Unlock()
	if finalClient == nil && unusedJournal != nil {
		// Shut down before the supervisor started: the journal opened for it still has to be closed.
		_ = unusedJournal.Close()
	}
	if finalClient != nil {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 40*time.Second)
		if err := finalClient.StopAcceptingAndDrain(drainCtx); err != nil {
			fmt.Fprintln(stderr, "supervisor drain:", err)
		}
		cancelDrain()
		finalStop()
		<-finalDone
		if err := finalJournal.Close(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
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
	root := os.Getenv("AIMEBU2_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return serverConfig{}, err
		}
		root = filepath.Join(home, ".aimebu2")
	}
	bind := os.Getenv("AIMEBU2_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	if net.ParseIP(bind) == nil {
		return serverConfig{}, fmt.Errorf("AIMEBU2_BIND must be an IP address")
	}
	port := 9897
	if raw := os.Getenv("AIMEBU2_PORT"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 65535 {
			return serverConfig{}, fmt.Errorf("AIMEBU2_PORT must be a TCP port")
		}
		port = n
	}
	allowRaw := os.Getenv("AIMEBU2_ALLOW")
	allow, err := server.ResolveAllow(allowRaw)
	if err != nil {
		return serverConfig{}, err
	}
	trusted, err := server.ResolveTrustedProxies(os.Getenv("AIMEBU2_TRUSTED_PROXIES"))
	if err != nil {
		return serverConfig{}, err
	}
	return serverConfig{root: root, bind: bind, port: port, allow: allow, allowDefault: allowRaw == "", trusted: trusted}, nil
}

// resolvePublicURL reads AIMEBU2_PUBLIC_URL as a bare origin; empty when unset.
func resolvePublicURL() (string, error) {
	raw := os.Getenv("AIMEBU2_PUBLIC_URL")
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("AIMEBU2_PUBLIC_URL must be an HTTP or HTTPS origin without a path, credentials, query, or fragment")
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
	return "AIMEBU2_PUBLIC_URL is set but AIMEBU2_TRUSTED_PROXIES is empty; if a reverse proxy forwards to this server, all proxied clients share the proxy's IP as one throttle identity, so one client's failed logins can delay everyone's. Set AIMEBU2_TRUSTED_PROXIES to the proxy's IPs or CIDRs."
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
	fmt.Fprintf(w, "Listening on %s; AIMEBU2_ALLOW=%s\n", address, strings.Join(entries, ","))
	if config.allowDefault && !net.ParseIP(config.bind).IsLoopback() {
		fmt.Fprintln(w, "WARNING: AIMEBU2_BIND is non-loopback but AIMEBU2_ALLOW defaults to loopback (plus the embedded supervisor); remote clients will receive 403. Set AIMEBU2_ALLOW to include their IPs or CIDRs.")
	}
}

// The embedded supervisor must reach its own listener even when remote peers
// are excluded by the configured allowlist.
func allowEmbeddedSupervisor(config *serverConfig) string {
	address := config.bind
	switch address {
	case "0.0.0.0":
		address = "127.0.0.1"
	case "::":
		address = "::1"
	}
	ip := netip.MustParseAddr(address).Unmap()
	for _, prefix := range config.allow {
		if prefix.Contains(ip) {
			return address
		}
	}
	bits := 32
	if ip.Is6() {
		bits = 128
	}
	config.allow = append(config.allow, netip.PrefixFrom(ip, bits))
	return address
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
		return fmt.Sprintf("cannot listen on %s: port %d is already in use by another process (%v). If it is the v1 aimebu server, stop it and start v2 again; otherwise stop that process or choose another port with AIMEBU2_PORT.", net.JoinHostPort(bind, strconv.Itoa(port)), port, err)
	}
	return fmt.Sprintf("cannot listen on %s: %v", net.JoinHostPort(bind, strconv.Itoa(port)), err)
}

// embeddedStartHook runs just before the embedded supervisor starts after the server is listening; tests hold it to see the server answer first.
// listenerHook receives the HTTP listener, so a test can close it and make Serve fail. embeddedStarts counts supervisors that really started.
var (
	embeddedStartHook atomic.Pointer[func()]
	listenerHook      atomic.Pointer[func(net.Listener)]
	embeddedStarts    atomic.Int32
	// servingHook runs once every listener is being served: startup (migrations, the owner's password hash) is over and a request is answered.
	servingHook atomic.Pointer[func()]
	// embeddedStartFinished runs when the background start has returned, whatever it did.
	embeddedStartFinished atomic.Pointer[func()]
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
	return base + "/setup?token=" + token
}
