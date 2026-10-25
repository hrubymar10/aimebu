package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
)

// `aimebu server start|stop|status` run the same server as `aimebu server`, detached, with the log file in server/. The foreground command
// stays as it is (it is what a launchd or systemd service runs, see docs/release.md).
//
// Whether a server is running is decided by its lock: a running server holds an exclusive lock on server/.writer.lock for as long as it
// lives (prune and import take the same lock), so a server started in the foreground, or by a service manager, is seen as running too. The
// pid file, server/aimebu.pid, is written by the server itself once it holds that lock (in every mode) and removed when it exits; it says
// whom to signal and is trusted only while the lock is held, and only for a process that is an aimebu server. Nothing else writes it, and
// a stale one is removed only while its lock is taken, so a server that is just starting is never robbed of its file.

const pidFileName = "aimebu.pid"

var (
	// daemonStartLimit is how long start waits for the new server to prove it is up: a first start moves v1's files, opens and migrates
	// the database and starts the embedded supervisor before it listens. Without the proof it has failed, however alive the process is.
	daemonStartLimit = 30 * time.Second
	// daemonStopLimit is how long stop waits for a clean shutdown (agents are stopped and the database closed). It never kills: a
	// server killed in the middle of a write is worth more waiting than a faster command.
	daemonStopLimit = 60 * time.Second
)

func pidFilePath(root string) string { return filepath.Join(root, "server", pidFileName) }

// withServerLockFree runs fn while holding the writer lock of root's server directory, if nothing else holds it, and reports whether it
// ran (it did not when a server is running). A server takes the lock before it writes its pid file, so whatever fn sees under the lock
// is nobody's current state.
func withServerLockFree(root string, fn func()) (bool, error) {
	dir := filepath.Join(root, "server")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		// No server directory: no server, and nothing of one to clean up.
		return true, nil
	}
	// The lock file is made if it is not there, so what fn does always happens under the lock (a server takes it before it writes its
	// pid file, and creates it the same way).
	path := filepath.Join(dir, ".writer.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	if fn != nil {
		fn()
	}
	return true, nil
}

// serverLockHeld reports whether a server holds the writer lock, without creating the lock file (which a root that is still v1's must
// not get from a status question).
func serverLockHeld(root string) bool {
	path := filepath.Join(root, "server", ".writer.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

// serverRunning reports whether a server holds the writer lock of root's server directory.
func serverRunning(root string) (bool, error) {
	free, err := withServerLockFree(root, nil)
	return !free, err
}

// removeStalePID removes a pid file nobody's server owns: with the lock free, no running server wrote it.
func removeStalePID(root string) {
	_, _ = withServerLockFree(root, func() { _ = os.Remove(pidFilePath(root)) })
}

// writePIDFile records this process as the server of root. It is called with the writer lock held.
func writePIDFile(root string) error {
	path := pidFilePath(root)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// removePIDFile removes the pid file if it still names this process.
func removePIDFile(root string) {
	if readPID(root) == os.Getpid() {
		_ = os.Remove(pidFilePath(root))
	}
}

// readPID reads the pid file; 0 when there is none or it is not a pid.
func readPID(root string) int {
	data, err := os.ReadFile(pidFilePath(root))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// isAimebuServer says whether pid is an aimebu server, by the same test that tells a v1 server from a stranger (looksLikeV1Server: a
// program named aimebu, or the aimebu-<hash> build the wrapper makes, run with `server`; its path may contain spaces). known is false
// when the process cannot be inspected, and then nothing is signalled.
func isAimebuServer(pid int) (ok, known bool) {
	exe, command, err := processInfo(pid)
	if err != nil || command == "" {
		return false, false
	}
	return looksLikeV1Server(exe, command), true
}

// serverPID is the pid of the running server of root, or why there is none to be had: the lock says a server runs, and the pid file the
// server wrote says which process it is, as long as that is a live process that can be verified as an aimebu server.
func serverPID(root string) (int, error) {
	pid := readPID(root)
	if pid == 0 {
		return 0, errors.New("a server is running on this config directory, but it left no pid file (an older build, or the file was removed): stop it where it runs")
	}
	if !processAlive(pid) {
		return 0, fmt.Errorf("a server is running on this config directory, but the pid file names a process (%d) that is gone: stop it where it runs", pid)
	}
	ok, known := isAimebuServer(pid)
	if !known {
		return 0, fmt.Errorf("a server is running on this config directory, and the pid file names process %d, but it cannot be inspected to confirm that it is the server, so nothing is signalled: if it is, stop it yourself (kill %d)", pid, pid)
	}
	if !ok {
		return 0, fmt.Errorf("a server is running on this config directory, but the pid file names a process (%d) that is not an aimebu server, so it is left alone: stop the server where it runs", pid)
	}
	return pid, nil
}

// localAddress is where this machine reaches the HTTP listener: an unspecified bind address is reached as loopback.
func localAddress(config serverConfig) string {
	host := config.bind
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.Is6() {
			host = "::1"
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(config.port))
}

// daemonExecutable is the program `server start` runs; tests point it at the test binary.
var daemonExecutable = os.Executable

// serverDaemonCommand implements `aimebu server start|stop|status`.
func serverDaemonCommand(action string, stdout, stderr io.Writer) int {
	config, err := resolveServerConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// A root that still holds v1's data is v1's: v1 keeps its own pid file in the same place, which this command must never delete. A
	// server that holds the lock is a v2 server (one that is starting has a database that is not yet identifiable as either), so v1's
	// data is looked for only when none does.
	if (action == "status" || action == "stop") && !serverLockHeld(config.root) {
		if moving, err := v1MovePending(config.root); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		} else if moving {
			fmt.Fprintf(stdout, "%s still holds v1's data and is not a v2 config directory yet, so there is no v2 server to ask about (run `aimebu server` in the foreground to move it, see docs/cutover-runbook.md)\n", config.root)
			return 1
		}
	}
	switch action {
	case "start":
		return daemonStart(config, stdout, stderr)
	case "stop":
		return daemonStop(config, stdout, stderr)
	case "status":
		return daemonStatus(config, stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: aimebu server [start|stop|status]")
	return 2
}

func daemonStatus(config serverConfig, stdout, stderr io.Writer) int {
	running, err := serverRunning(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !running {
		removeStalePID(config.root) // the leftover of a server that is gone
		fmt.Fprintln(stdout, "aimebu is not running")
		return 1
	}
	if pid, err := serverPID(config.root); err == nil {
		fmt.Fprintf(stdout, "aimebu is running (pid %d, http://%s)\nlog: %s\n", pid, localAddress(config), serverLogPath(config.root))
	} else {
		fmt.Fprintf(stdout, "aimebu is running (http://%s); %v\nlog: %s\n", localAddress(config), err, serverLogPath(config.root))
	}
	return 0
}

func daemonStop(config serverConfig, stdout, stderr io.Writer) int {
	running, err := serverRunning(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !running {
		removeStalePID(config.root)
		fmt.Fprintln(stderr, "aimebu is not running")
		return 1
	}
	pid, err := serverPID(config.root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintf(stderr, "could not signal the server (pid %d): %v\n", pid, err)
		return 1
	}
	deadline := time.Now().Add(daemonStopLimit)
	for time.Now().Before(deadline) {
		if held, err := serverRunning(config.root); err == nil && !held {
			removeStalePID(config.root)
			fmt.Fprintf(stdout, "aimebu stopped (was pid %d)\n", pid)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintf(stderr, "the server (pid %d) is still shutting down after %s; it was not killed. Check %s, and send it SIGKILL yourself only if it is stuck\n", pid, daemonStopLimit, serverLogPath(config.root))
	return 1
}

// answersAsAimebu asks the unauthenticated setup route of the server at address and says whether the answer is an aimebu server's:
// a JSON object with a boolean "open". A 403 also counts: the server's allowlist does not include the machine asking.
func answersAsAimebu(client *http.Client, address string) bool {
	response, err := client.Get("http://" + address + "/api/setup/status")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden {
		return true
	}
	if response.StatusCode != http.StatusOK {
		return false
	}
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&body); err != nil {
		return false
	}
	_, isBool := body["open"].(bool)
	return isBool
}

func daemonStart(config serverConfig, stdout, stderr io.Writer) int {
	if running, err := serverRunning(config.root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	} else if running {
		if pid, err := serverPID(config.root); err == nil {
			fmt.Fprintf(stderr, "aimebu is already running (pid %d)\n", pid)
		} else {
			fmt.Fprintln(stderr, "aimebu is already running on this config directory")
		}
		return 1
	}
	// What would make the server refuse to start is told here, in the foreground, where the person is looking, rather than found in a log.
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
	// A root that still holds v1's files is moved by the first start, with notices and preflight checks that a detached process would only
	// write to a log: that first start is run in the foreground.
	if moving, err := v1MovePending(config.root); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	} else if moving {
		fmt.Fprintf(stderr, "%s still holds v1's data, which the first start moves: run `aimebu server` in the foreground once (see docs/cutover-runbook.md), then use start\n", config.root)
		return 1
	}
	executable, err := daemonExecutable()
	if err != nil {
		fmt.Fprintf(stderr, "cannot find the aimebu executable: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Join(config.root, "server"), 0700); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer null.Close()
	// The server writes its own log (server/aimebu.log, rotated) and its own pid file, so its standard streams go nowhere.
	command := exec.Command(executable, "server")
	command.Stdin, command.Stdout, command.Stderr = null, null, null
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The server keeps nothing of its caller: a descriptor this process inherited (a lock held by flock, a pipe of a script) must not be
	// carried into a process that lives on after the caller is gone and would hold it for ever.
	markInheritedFDsCloseOnExec()
	if err := command.Start(); err != nil {
		fmt.Fprintf(stderr, "could not start the server: %v\n", err)
		return 1
	}
	pid := command.Process.Pid
	if daemonSpawned != nil {
		daemonSpawned(pid)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()

	// Up means: this child is alive and holds the lock (its pid file is there, written under it), and something answers as an aimebu
	// server on its address. Another service on the port, or another start that won the lock, is neither.
	address := localAddress(config)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(daemonStartLimit)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			reason := "the server exited during startup"
			if err != nil {
				reason = fmt.Sprintf("the server exited during startup (%v)", err)
			}
			fmt.Fprintf(stderr, "%s; the end of %s:\n%s", reason, serverLogPath(config.root), tailOfFile(serverLogPath(config.root), 10))
			return 1
		default:
		}
		// The pid file is written by the server once it holds the lock, so it is looked at first: only then is the lock probed (a probe
		// takes the lock for an instant), and by then it is the child's.
		if readPID(config.root) == pid && processAlive(pid) {
			if held, err := serverRunning(config.root); err == nil && held && answersAsAimebu(client, address) {
				fmt.Fprintf(stdout, "aimebu started (pid %d, http://%s)\nlog: %s\n", pid, address, serverLogPath(config.root))
				if token, err := os.ReadFile(filepath.Join(config.root, setupTokenFile)); err == nil && strings.TrimSpace(string(token)) != "" {
					fmt.Fprintf(stdout, "No owner exists yet. Open this link to create it (single use):\n  %s\n", setupLink(publicURL, config.bind, config.port, tlsConfig, strings.TrimSpace(string(token))))
				}
				return 0
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Alive is not up: without proof the start has failed, and the process it left is its to report (it is not killed: it may be a
	// server that is only slow, and stop will find it if it is one).
	fmt.Fprintf(stderr, "the server (pid %d) did not come up within %s: it does not hold the lock and answer on http://%s yet. Check %s; `aimebu server status` shows whether it gets there\n", pid, daemonStartLimit, address, serverLogPath(config.root))
	return 1
}

// daemonSpawned is told the pid of each detached server `start` launches; only tests set it, so that they can kill by pid whatever they
// started, even when its pid file was never written or is gone.
var daemonSpawned func(pid int)

// markInheritedFDsCloseOnExec sets close-on-exec on every descriptor above stderr. Go opens its own with it, but a descriptor inherited from
// whoever ran this process (flock hands its lock to the command it runs, a shell may leave pipes) is passed on to every child by exec, and a
// detached server that outlives the caller would then hold it for ever. Setting the flag here changes nothing for this process, which keeps
// using what it was given until it exits.
func markInheritedFDsCloseOnExec() {
	if entries, err := os.ReadDir("/dev/fd"); err == nil {
		for _, entry := range entries {
			if fd, err := strconv.Atoi(entry.Name()); err == nil && fd > 2 {
				syscall.CloseOnExec(fd)
			}
		}
		return
	}
	for fd := 3; fd < 1024; fd++ {
		syscall.CloseOnExec(fd)
	}
}

// tailOfFile returns the last n lines of a file, or a note that there is nothing to show.
func tailOfFile(path string, n int) string {
	file, err := os.Open(path)
	if err != nil {
		return "(no log)\n"
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if len(lines) == 0 {
		return "(the log is empty)\n"
	}
	return strings.Join(lines, "\n") + "\n"
}
