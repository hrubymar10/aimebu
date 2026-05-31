package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/hrubymar10/aimebu/internal/envflags"
)

func pidFile(serverDir string) string {
	return filepath.Join(serverDir, "aimebu.pid")
}

func legacyPIDFile(rootDir string) string {
	return filepath.Join(rootDir, "aimebu.pid")
}

func logFile(serverDir string) string {
	return filepath.Join(serverDir, "aimebu.log")
}

func resolveActivePID(rootDir string) (path string, running bool, pid int, err error) {
	serverPID := pidFile(filepath.Join(rootDir, "server"))
	running, pid, err = daemonStatusFromPIDFile(serverPID)
	if err != nil {
		return "", false, 0, err
	}
	if running {
		return serverPID, true, pid, nil
	}

	legacyPID := legacyPIDFile(rootDir)
	running, pid, err = daemonStatusFromPIDFile(legacyPID)
	if err != nil {
		return "", false, 0, err
	}
	if running {
		return legacyPID, true, pid, nil
	}

	return serverPID, false, 0, nil
}

// DaemonStart launches `aimebu server serve` as a background process.
func DaemonStart(selfBin, addr, rootDir string) error {
	serverDir := filepath.Join(rootDir, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Check if already running
	if running, pid, _ := DaemonStatus(rootDir); running {
		return fmt.Errorf("aimebu already running (pid %d)", pid)
	}
	tlsConfig, err := resolveServerTLSConfig()
	if err != nil {
		return err
	}

	lf, err := os.OpenFile(logFile(serverDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(selfBin, "server", "serve")
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Env = append(os.Environ(),
		"AIMEBU_BIND="+addrHost(addr),
		"AIMEBU_PORT="+addrPort(addr),
		"AIMEBU_CONFIG_DIR="+rootDir,
		"AIMEBU_DAEMON_CHILD=1",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	lf.Close()

	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFile(serverDir), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
	}()

	// Wait briefly and confirm it's alive + healthy
	time.Sleep(200 * time.Millisecond)
	if err := daemonStartupExitError(exited, serverDir); err != nil {
		return err
	}

	// Try hitting health endpoint. When TLS is enabled, the daemon serves
	// HTTPS on AIMEBU_TLS_PORT while keeping HTTP on AIMEBU_PORT.
	scheme := "http"
	healthAddr := addr
	if tlsConfig.Enabled {
		scheme = "https"
		tlsPort, err := resolveServerTLSPort()
		if err != nil {
			return err
		}
		healthAddr = net.JoinHostPort(addrHost(addr), tlsPort)
	}
	healthURL := fmt.Sprintf("%s://%s/health", scheme, healthAddr)
	httpClient := daemonHealthClient()
	for i := 0; i < 10; i++ {
		if err := daemonStartupExitError(exited, serverDir); err != nil {
			return err
		}
		resp, err := httpClient.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				if tlsConfig.Enabled {
					fmt.Printf("aimebu started (pid %d, http://%s + https://%s)\n", pid, addr, healthAddr)
				} else {
					fmt.Printf("aimebu started (pid %d, http://%s)\n", pid, addr)
				}
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("aimebu started (pid %d) — health check pending\n", pid)
	return nil
}

func daemonStartupExitError(exited <-chan error, serverDir string) error {
	select {
	case err := <-exited:
		_ = os.Remove(pidFile(serverDir))
		if err != nil {
			return fmt.Errorf("daemon exited during startup (%v) — check %s", err, logFile(serverDir))
		}
		return fmt.Errorf("daemon exited during startup — check %s", logFile(serverDir))
	default:
		return nil
	}
}

func daemonHealthClient() *http.Client {
	if !daemonInsecureSkipVerifyEnabled() {
		return http.DefaultClient
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // Explicit opt-in development flag for daemon health checks.
	return &http.Client{Transport: tr}
}

func daemonInsecureSkipVerifyEnabled() bool {
	return envflags.Enabled("AIMEBU_INSECURE_SKIP_VERIFY")
}

// DaemonStop sends SIGTERM to the daemon and waits for it to exit.
func DaemonStop(rootDir string) error {
	pidPath, running, pid, err := resolveActivePID(rootDir)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("aimebu is not running")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM: %w", err)
	}

	// Wait up to 5s for exit
	for i := 0; i < 50; i++ {
		if !processAlive(pid) {
			_ = os.Remove(pidPath)
			fmt.Printf("aimebu stopped (was pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill
	_ = proc.Signal(syscall.SIGKILL)
	_ = os.Remove(pidPath)
	fmt.Printf("aimebu killed (was pid %d)\n", pid)
	return nil
}

// DaemonStatus checks if the daemon is running.
func DaemonStatus(rootDir string) (running bool, pid int, err error) {
	_, running, pid, err = resolveActivePID(rootDir)
	return running, pid, err
}

func daemonStatusFromPIDFile(pidPath string) (running bool, pid int, err error) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("read pid file: %w", err)
	}

	pid, err = strconv.Atoi(string(data))
	if err != nil {
		_ = os.Remove(pidPath)
		return false, 0, nil
	}

	if !processAlive(pid) {
		_ = os.Remove(pidPath)
		return false, 0, nil
	}

	return true, pid, nil
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func addrHost(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

func addrPort(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return ""
}
