package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/testenv"
)

// daemonHelperEnv makes this test binary behave as `aimebu` (see TestMain): `server start` runs os.Executable, which in a test is the test
// binary, so the detached server in these tests is a real server process.
const daemonHelperEnv = "AIMEBU2_TEST_RUN_AS_AIMEBU"

// daemonWatchEnv names the pid of the test process that started the detached server: the server exits when that process is gone, so a test
// run that is killed leaves nothing behind (see TestMain).
const daemonWatchEnv = "AIMEBU2_TEST_WATCH_PID"

var (
	aimebuCopyOnce sync.Once
	aimebuCopyPath string
	aimebuCopyErr  error
)

// aimebuCopy is this test binary under the name `aimebu`: a server is recognised by its program name (see looksLikeV1Server, which
// resolves the executable itself), and the test binary is named after the package, so the detached server of these tests runs from a copy.
func aimebuCopy(t *testing.T) string {
	t.Helper()
	aimebuCopyOnce.Do(func() {
		source, err := os.Executable()
		if err != nil {
			aimebuCopyErr = err
			return
		}
		in, err := os.Open(source)
		if err != nil {
			aimebuCopyErr = err
			return
		}
		defer in.Close()
		dir := filepath.Join(packageTestRoot, "bin")
		if aimebuCopyErr = os.MkdirAll(dir, 0700); aimebuCopyErr != nil {
			return
		}
		aimebuCopyPath = filepath.Join(dir, "aimebu")
		out, err := os.OpenFile(aimebuCopyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
		if err != nil {
			aimebuCopyErr = err
			return
		}
		if _, err := io.Copy(out, in); err != nil {
			aimebuCopyErr = err
		}
		if err := out.Close(); aimebuCopyErr == nil {
			aimebuCopyErr = err
		}
	})
	if aimebuCopyErr != nil {
		t.Fatal(aimebuCopyErr)
	}
	return aimebuCopyPath
}

// daemonFixture is a config root, an owner, a free port and the environment of a detached server that stays off the machine's other state.
func daemonFixture(t *testing.T) (root string, port int) {
	t.Helper()
	t.Setenv(daemonHelperEnv, "1")
	copyPath := aimebuCopy(t)
	previous := daemonExecutable
	daemonExecutable = func() (string, error) { return copyPath, nil }
	t.Cleanup(func() { daemonExecutable = previous })
	t.Setenv("AIMEBU2_USAGES", "0")
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "0")
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	root = t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_BIND", "127.0.0.1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	t.Setenv("AIMEBU2_PORT", strconv.Itoa(port))
	// Whatever a test leaves running is killed by pid, whether it passed, failed or never got a pid file written: every server `start`
	// launches is recorded as it is spawned, and the pid file is looked at too. If the test binary itself dies (a timeout, a signal), no
	// cleanup runs, so the detached server also watches the test process and exits when it is gone.
	var spawnedMu sync.Mutex
	var spawned []int
	previousSpawned := daemonSpawned
	daemonSpawned = func(pid int) { spawnedMu.Lock(); spawned = append(spawned, pid); spawnedMu.Unlock() }
	t.Setenv(daemonWatchEnv, strconv.Itoa(os.Getpid()))
	t.Cleanup(func() {
		daemonSpawned = previousSpawned
		spawnedMu.Lock()
		defer spawnedMu.Unlock()
		if pid := readPID(root); pid != 0 {
			spawned = append(spawned, pid)
		}
		for _, pid := range spawned {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return root, port
}

func runServerAction(action string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"server", action}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestServerDaemonStartStatusStop(t *testing.T) {
	root, port := daemonFixture(t)

	if code, out, _ := runServerAction("status"); code != 1 || !strings.Contains(out, "not running") {
		t.Fatalf("status of nothing: %d %q", code, out)
	}
	if code, _, errOut := runServerAction("stop"); code != 1 || !strings.Contains(errOut, "not running") {
		t.Fatalf("stop of nothing: %d %q", code, errOut)
	}

	code, out, errOut := runServerAction("start")
	if code != 0 || !strings.Contains(out, "aimebu started (pid ") || !strings.Contains(out, fmt.Sprintf("http://127.0.0.1:%d", port)) || !strings.Contains(out, serverLogPath(root)) {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	pid := readPID(root)
	if pid == 0 || !processAlive(pid) {
		t.Fatalf("pid file %d, alive %v", pid, processAlive(pid))
	}
	if info, err := os.Stat(pidFilePath(root)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("pid file mode: %v %v", info, err)
	}

	if code, out, _ := runServerAction("status"); code != 0 || !strings.Contains(out, fmt.Sprintf("running (pid %d", pid)) {
		t.Fatalf("status of a running server: %d %q", code, out)
	}
	if code, _, errOut := runServerAction("start"); code != 1 || !strings.Contains(errOut, fmt.Sprintf("already running (pid %d)", pid)) {
		t.Fatalf("a second start: %d %q", code, errOut)
	}
	// It writes its own log, with what it printed.
	log, err := os.ReadFile(serverLogPath(root))
	if err != nil || !strings.Contains(string(log), "Owner initialized") {
		t.Fatalf("log: %v\n%s", err, log)
	}

	code, out, errOut = runServerAction("stop")
	if code != 0 || !strings.Contains(out, fmt.Sprintf("aimebu stopped (was pid %d)", pid)) {
		t.Fatalf("stop: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the pid file stayed: %v", err)
	}
	if running, err := serverRunning(root); err != nil || running {
		t.Fatalf("running after stop: %v %v", running, err)
	}
	if code, out, _ := runServerAction("status"); code != 1 || !strings.Contains(out, "not running") {
		t.Fatalf("status after stop: %d %q", code, out)
	}
	// The port is free again, so it can be started once more.
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("restart: %d %q %q", code, out, errOut)
	}
	if code, _, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("second stop: %d %q", code, errOut)
	}
}

func TestServerDaemonStartReportsAServerThatCannotStart(t *testing.T) {
	root, port := daemonFixture(t)
	// The port is taken, so the detached server dies at once: the command says so and shows the end of its log.
	holder, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	code, out, errOut := runServerAction("start")
	if code != 1 || out != "" || !strings.Contains(errOut, "exited during startup") || !strings.Contains(errOut, serverLogPath(root)) || !strings.Contains(errOut, fmt.Sprint(port)) {
		t.Fatalf("start with the port taken: %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("a pid file of a server that is gone stayed: %v", err)
	}
}

func TestServerDaemonRefusesWhatForegroundShouldDo(t *testing.T) {
	// v1's files are moved by the first start, with notices a detached process would hide in a log.
	root := makeV1Root(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	code, _, errOut := runServerAction("start")
	if code != 1 || !strings.Contains(errOut, "foreground") || !strings.Contains(errOut, "v1") {
		t.Fatalf("start over v1 data: %d %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "server", "marker.txt")); err != nil {
		t.Fatalf("v1 data was touched: %v", err)
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("a pid file: %v", err)
	}

	// A configuration the server would refuse is refused before anything is started.
	t.Setenv("AIMEBU2_CONFIG_DIR", t.TempDir())
	t.Setenv("AIMEBU2_TLS_CERT", filepath.Join(t.TempDir(), "only-a-cert.pem"))
	t.Setenv("AIMEBU2_TLS_KEY", "")
	if code, _, errOut := runServerAction("start"); code != 1 || errOut == "" {
		t.Fatalf("start with half a TLS configuration: %d %q", code, errOut)
	}

	if code, _, errOut := runServerAction("restart"); code != 2 || !strings.Contains(errOut, "usage: aimebu server") {
		t.Fatalf("an unknown action: %d %q", code, errOut)
	}
}

func TestServerDaemonSeesAForegroundServerAndLeavesItAlone(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "0")
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	root := t.TempDir()
	useMigratedConfigDir(t, root)
	port := holdPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var stdout, stderr bytes.Buffer
	ready := watchServing(t)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	waitServerListening(t, fmt.Sprintf("http://127.0.0.1:%d/api/me", port), ready, done, &stderr)

	// The foreground server wrote its own pid file under the lock, and the process is this test binary, which is not an aimebu server
	// (no `server` in its command line): status still reports the running server, and stop and start leave it alone.
	if got := readPID(root); got != os.Getpid() {
		t.Fatalf("the foreground server's pid file names %d, want %d", got, os.Getpid())
	}
	code, out, _ := runServerAction("status")
	if code != 0 || !strings.Contains(out, "running") {
		t.Fatalf("status of a foreground server: %d %q", code, out)
	}
	if code, _, errOut := runServerAction("stop"); code != 1 || !strings.Contains(errOut, "not an aimebu server") {
		t.Fatalf("stop of a foreground server whose process is not an aimebu server: %d %q", code, errOut)
	}
	if code, _, errOut := runServerAction("start"); code != 1 || !strings.Contains(errOut, "already running") {
		t.Fatalf("start over a foreground server: %d %q", code, errOut)
	}
	cancel()
	if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
		t.Fatalf("serve code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the server left its pid file behind: %v", err)
	}
	// A pid file nobody's server owns (a server that was killed) is removed by status, under the lock.
	if err := os.WriteFile(pidFilePath(root), []byte(strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runServerAction("status"); code != 1 || !strings.Contains(out, "not running") {
		t.Fatalf("status with a stale pid file: %d %q", code, out)
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the stale pid file stayed: %v", err)
	}
	if !processAlive(os.Getpid()) {
		t.Fatal("the test process was signalled")
	}
}

func TestLocalAddressReachesTheListener(t *testing.T) {
	for _, tc := range []struct {
		bind string
		port int
		want string
	}{
		{"127.0.0.1", 9897, "127.0.0.1:9897"},
		{"0.0.0.0", 9897, "127.0.0.1:9897"},
		{"::", 9897, "[::1]:9897"},
		{"::1", 9897, "[::1]:9897"},
		{"192.168.1.5", 9000, "192.168.1.5:9000"},
	} {
		if got := localAddress(serverConfig{bind: tc.bind, port: tc.port}); got != tc.want {
			t.Errorf("localAddress(%s) = %q, want %q", tc.bind, got, tc.want)
		}
	}
}

func TestTailOfFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if got := tailOfFile(path, 3); got != "(no log)\n" {
		t.Fatalf("missing: %q", got)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := tailOfFile(path, 3); got != "(the log is empty)\n" {
		t.Fatalf("empty: %q", got)
	}
	if err := os.WriteFile(path, []byte("1\n2\n3\n4\n5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := tailOfFile(path, 3); got != "3\n4\n5\n" {
		t.Fatalf("tail: %q", got)
	}
}

// holdServerLock takes the writer lock of root as a running server does, without being one: the stand-in for a server of an older build,
// or one that a service manager runs, whose pid file is missing or wrong.
func holdServerLock(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := lockServerDirectory(filepath.Join(root, "server"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unlockServerDirectory(lock) })
}

// stop signals only a process that is an aimebu server: a pid file that is stale, or names something else, while a server holds the lock
// must never get the process it names killed.
func TestServerStopNeverSignalsAnUnrelatedProcess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	holdServerLock(t, root)
	bystander := exec.Command("sleep", "120")
	if err := bystander.Start(); err != nil {
		t.Skipf("no sleep to stand in for a bystander: %v", err)
	}
	gone := make(chan error, 1)
	go func() { gone <- bystander.Wait() }() // a signalled process is seen to have died by Wait (a zombie still answers signal 0)
	t.Cleanup(func() { _ = bystander.Process.Kill() })
	pidFile := pidFilePath(root)

	for name, tc := range map[string]struct {
		pidFile string // empty: no pid file
		want    string
	}{
		"a pid file naming an unrelated live process": {strconv.Itoa(bystander.Process.Pid) + "\n", "not an aimebu server"},
		"no pid file": {"", "no pid file"},
		"a pid file naming a process that is gone": {"2147483646\n", "gone"},
		"a pid file that is not a pid":             {"banana\n", "no pid file"},
	} {
		_ = os.Remove(pidFile)
		if tc.pidFile != "" {
			if err := os.WriteFile(pidFile, []byte(tc.pidFile), 0600); err != nil {
				t.Fatal(err)
			}
		}
		code, _, errOut := runServerAction("stop")
		if code != 1 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: stop = %d %q, want a refusal mentioning %q", name, code, errOut, tc.want)
		}
		select {
		case err := <-gone:
			t.Fatalf("%s: the unrelated process died: %v", name, err)
		case <-time.After(200 * time.Millisecond):
		}
		// With a server holding the lock, status and stop never remove the pid file: it may be that server's.
		if tc.pidFile != "" {
			if _, err := os.Stat(pidFile); err != nil {
				t.Errorf("%s: the pid file was removed while a server held the lock: %v", name, err)
			}
		}
		if code, out, _ := runServerAction("status"); code != 0 || !strings.Contains(out, "running") {
			t.Errorf("%s: status = %d %q", name, code, out)
		}
	}
}

// Another service on the port, answering HTTP (even like an aimebu setup route), must not make start report success: the child it started
// dies on the port, holds no lock and wrote no pid file.
func TestServerStartIsNotFooledByAnotherServiceOnThePort(t *testing.T) {
	root, port := daemonFixture(t)
	other := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"open":false}`))
	})}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	go other.Serve(listener)
	defer other.Close()
	code, out, errOut := runServerAction("start")
	if code != 1 || strings.Contains(out, "aimebu started") || !strings.Contains(errOut, "exited during startup") {
		t.Fatalf("start with another service on the port: %d %q %q", code, out, errOut)
	}
	if running, _ := serverRunning(root); running {
		t.Fatal("a server is running")
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("a pid file: %v", err)
	}
}

// Two starts at once run one server: the loser's server dies on the lock and its start says so, and neither start touches the winner's
// pid file.
func TestServerConcurrentStartsRunOneServer(t *testing.T) {
	root, _ := daemonFixture(t)
	type result struct {
		code        int
		out, errOut string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out, errOut := runServerAction("start")
			results[i] = result{code, out, errOut}
		}()
	}
	wg.Wait()
	winners := 0
	for _, r := range results {
		if r.code == 0 {
			winners++
			if !strings.Contains(r.out, "aimebu started") {
				t.Errorf("a successful start printed %q", r.out)
			}
		} else if !strings.Contains(r.errOut, "already running") && !strings.Contains(r.errOut, "exited during startup") {
			t.Errorf("a refused start said %q", r.errOut)
		}
	}
	if winners != 1 {
		t.Fatalf("%d starts succeeded, want 1: %+v", winners, results)
	}
	pid := readPID(root)
	if pid == 0 || !processAlive(pid) {
		t.Fatalf("the winner's pid file: %d", pid)
	}
	if ok, known := isAimebuServer(pid); known && !ok {
		t.Fatalf("pid %d is not an aimebu server", pid)
	}
	if code, out, _ := runServerAction("status"); code != 0 || !strings.Contains(out, fmt.Sprintf("pid %d", pid)) {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, _, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop: %d %q", code, errOut)
	}
}

// A process that cannot be inspected is not signalled: the pid file is the server's own, but a signal that goes to the wrong process cannot
// be taken back, so the message says what to do by hand instead.
func TestServerStopSignalsNothingItCannotVerify(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	holdServerLock(t, root)
	bystander := exec.Command("sleep", "120")
	if err := bystander.Start(); err != nil {
		t.Skipf("no sleep to stand in for the server: %v", err)
	}
	gone := make(chan error, 1)
	go func() { gone <- bystander.Wait() }()
	t.Cleanup(func() { _ = bystander.Process.Kill() })
	if err := os.WriteFile(pidFilePath(root), []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := processInfo
	processInfo = func(int) (string, string, error) { return "", "", fmt.Errorf("no ps") }
	t.Cleanup(func() { processInfo = previous })
	code, _, errOut := runServerAction("stop")
	if code != 1 || !strings.Contains(errOut, "cannot be inspected") || !strings.Contains(errOut, fmt.Sprintf("kill %d", bystander.Process.Pid)) {
		t.Fatalf("stop of a process that cannot be verified: %d %q", code, errOut)
	}
	select {
	case err := <-gone:
		t.Fatalf("the process died: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// Where there is no ps, the command line is read from /proc.
func TestProcessInfoFallsBackToProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc")
	}
	t.Setenv("PATH", t.TempDir()) // no ps on it
	_, command, err := processInfo(os.Getpid())
	if err != nil || !strings.Contains(command, filepath.Base(os.Args[0])) {
		t.Fatalf("processInfo without ps = %q, %v", command, err)
	}
}

// status and stop never touch a config directory that still holds v1's data: v1 keeps its own pid file in the same place.
func TestServerDaemonLeavesV1sPidFileAlone(t *testing.T) {
	root := makeV1Root(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	v1PID := filepath.Join(root, "server", pidFileName)
	if err := os.WriteFile(v1PID, []byte("2147483646\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"status", "stop"} {
		code, out, errOut := runServerAction(action)
		if code != 1 || !strings.Contains(out+errOut, "still holds v1's data") {
			t.Fatalf("%s over v1 data: %d %q %q", action, code, out, errOut)
		}
		if data, err := os.ReadFile(v1PID); err != nil || string(data) != "2147483646\n" {
			t.Fatalf("%s touched v1's pid file: %q %v", action, data, err)
		}
	}
}

// A start whose server never proves it is up has failed, however alive the process it left is.
func TestServerStartFailsWithoutProofThatTheServerIsUp(t *testing.T) {
	root, _ := daemonFixture(t)
	idle := filepath.Join(t.TempDir(), "idle")
	if err := testenv.WriteExecutable(idle, []byte("#!/bin/sh\nexec sleep 120\n"), 0700); err != nil {
		t.Fatal(err)
	}
	daemonExecutable = func() (string, error) { return idle, nil }
	previous := daemonStartLimit
	daemonStartLimit = time.Second
	t.Cleanup(func() { daemonStartLimit = previous })
	code, out, errOut := runServerAction("start")
	pid := 0
	fmt.Sscanf(errOut[strings.Index(errOut, "(pid ")+len("(pid "):], "%d", &pid)
	if pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}
	if code != 1 || strings.Contains(out, "aimebu started") || !strings.Contains(errOut, "did not come up") || pid == 0 {
		t.Fatalf("start of a server that never comes up: %d %q %q", code, out, errOut)
	}
	if !processAlive(pid) {
		t.Fatal("start killed the process it had started")
	}
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("a pid file nobody's server wrote: %v", err)
	}
}

// What a stale pid file's cleanup does, it does under the lock, creating the lock file if it is not there; with no server directory there
// is nothing to clean and no server.
func TestStalePidCleanupRunsUnderTheLock(t *testing.T) {
	root := t.TempDir()
	if free, err := withServerLockFree(root, func() { t.Fatal("ran with no server directory to clean") }); !free || err != nil {
		t.Fatalf("no server directory: %v %v", free, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFilePath(root), []byte("2147483646\n"), 0600); err != nil {
		t.Fatal(err)
	}
	removeStalePID(root)
	if _, err := os.Stat(pidFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("the stale pid file stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "server", ".writer.lock")); err != nil {
		t.Fatalf("the lock was not created for the cleanup: %v", err)
	}
	// Under a held lock the cleanup does not run.
	holdServerLock(t, root)
	if err := os.WriteFile(pidFilePath(root), []byte("2147483646\n"), 0600); err != nil {
		t.Fatal(err)
	}
	removeStalePID(root)
	if _, err := os.Stat(pidFilePath(root)); err != nil {
		t.Fatalf("a server's pid file was removed under its lock: %v", err)
	}
}

// A probe takes the lock for an instant, so a server starting at that instant waits it out instead of dying; a lock that is really held
// still refuses it.
func TestServerLockOutlastsAProbeButNotAServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "server")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	probe, err := lockServerDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(60 * time.Millisecond)
		unlockServerDirectory(probe)
	}()
	lock, err := lockServerDirectory(dir)
	if err != nil {
		t.Fatalf("a server starting during a probe was refused: %v", err)
	}
	defer unlockServerDirectory(lock)
	started := time.Now()
	if _, err := lockServerDirectory(dir); err == nil || !strings.Contains(err.Error(), "in use by a running v2 server") {
		t.Fatalf("a second server was not refused: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the refusal took %s", elapsed)
	}
}

// A server that is just starting has a database that is empty or half made, which is neither v1's nor yet identifiable as v2's: status and
// stop go by its lock and do not ask whether the root is v1's.
func TestServerDaemonAsksAboutAStartingServerByItsLock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, "server"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server", "aimebu.sqlite"), nil, 0600); err != nil { // created, nothing in it yet
		t.Fatal(err)
	}
	holdServerLock(t, root)
	code, out, errOut := runServerAction("status")
	if code != 0 || !strings.Contains(out, "running") || strings.Contains(errOut+out, "cannot identify") {
		t.Fatalf("status of a server that is starting: %d %q %q", code, out, errOut)
	}
	// And a status question does not give a v1 root a lock file.
	v1 := makeV1Root(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", v1)
	if code, out, _ := runServerAction("status"); code != 1 || !strings.Contains(out, "still holds v1's data") {
		t.Fatalf("status over v1 data: %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(v1, "server", ".writer.lock")); !os.IsNotExist(err) {
		t.Fatalf("a status question made a lock file in a v1 root: %v", err)
	}
}

// fdFlags reads a descriptor's flags; leakFD clears close-on-exec on one, which is what a descriptor handed down by flock or a shell looks
// like to this process.
func fdFlags(t *testing.T, fd uintptr) uintptr {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags
}

func leakFD(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "inherited-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_SETFD, 0); errno != 0 {
		t.Fatal(errno)
	}
	if fdFlags(t, file.Fd())&syscall.FD_CLOEXEC != 0 {
		t.Fatal("could not make a descriptor that is inherited")
	}
	return file
}

func TestMarkInheritedFDsCloseOnExec(t *testing.T) {
	leaked := leakFD(t)
	ordinary, err := os.CreateTemp(t.TempDir(), "ordinary-*")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	markInheritedFDsCloseOnExec()
	if fdFlags(t, leaked.Fd())&syscall.FD_CLOEXEC == 0 {
		t.Error("an inherited descriptor would still be passed to a child")
	}
	if fdFlags(t, ordinary.Fd())&syscall.FD_CLOEXEC == 0 {
		t.Error("a descriptor Go opened lost close-on-exec")
	}
	// This process still has it: the flag changes what a child gets, not what the process may do.
	if _, err := leaked.WriteString("still usable"); err != nil {
		t.Errorf("the descriptor stopped working in the process that had it: %v", err)
	}
}

// A descriptor the caller hands down (flock gives its lock to the command it runs) is not in the detached server, which outlives the caller:
// the server of a test that was started under the gate lock once held that lock after the test was gone.
func TestDaemonStartDoesNotPassOnAnInheritedDescriptor(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc to look at the server's descriptors")
	}
	root, _ := daemonFixture(t)
	leaked := leakFD(t)
	target, err := os.Readlink(filepath.Join("/proc/self/fd", strconv.Itoa(int(leaked.Fd()))))
	if err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runServerAction("start"); code != 0 {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	pid := readPID(root)
	if pid == 0 {
		t.Fatal("no pid")
	}
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if link, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name())); err == nil && link == target {
			t.Errorf("the detached server holds descriptor %s (%s), inherited from its caller", entry.Name(), link)
		}
	}
	if code, _, errOut := runServerAction("stop"); code != 0 {
		t.Fatalf("stop: %d %q", code, errOut)
	}
}

// A detached test server exits by itself when the test process that started it is gone, so a killed test run leaves no server behind.
func TestTestServerExitsWhenItsTestProcessIsGone(t *testing.T) {
	daemonFixture(t)
	watched := exec.Command("sleep", "3")
	if err := watched.Start(); err != nil {
		t.Fatal(err)
	}
	go watched.Wait()
	server := exec.Command(aimebuCopy(t), "server")
	server.Env = append(os.Environ(), daemonHelperEnv+"=1", daemonWatchEnv+"="+strconv.Itoa(watched.Process.Pid))
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill() })
	exited := make(chan error, 1)
	go func() { exited <- server.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("the server left while its test process was still there: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not leave after its test process was gone")
	}
}
