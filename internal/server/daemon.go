package server

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
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

func resolvePIDFile(rootDir string) string {
	serverPID := pidFile(filepath.Join(rootDir, "server"))
	if _, err := os.Stat(serverPID); err == nil {
		return serverPID
	}
	return legacyPIDFile(rootDir)
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

	// Wait briefly and confirm it's alive + healthy
	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		_ = os.Remove(pidFile(serverDir))
		return fmt.Errorf("daemon exited immediately — check %s", logFile(serverDir))
	}

	// Try hitting health endpoint
	healthURL := fmt.Sprintf("http://%s/health", addr)
	for i := 0; i < 10; i++ {
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Printf("aimebu started (pid %d, http://%s)\n", pid, addr)
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("aimebu started (pid %d) — health check pending\n", pid)
	return nil
}

// DaemonStop sends SIGTERM to the daemon and waits for it to exit.
func DaemonStop(rootDir string) error {
	pidPath := resolvePIDFile(rootDir)
	running, pid, err := daemonStatusFromPIDFile(pidPath)
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
	return daemonStatusFromPIDFile(resolvePIDFile(rootDir))
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
