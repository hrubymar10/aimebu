package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	aimebu "github.com/hrubymar10/aimebu"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/config"
	"github.com/hrubymar10/aimebu/internal/mcp"
	"github.com/hrubymar10/aimebu/internal/server"
)

// version is the build-time version string. Override with:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/aimebu
var version = "v0.0.0"

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "server":
		serverCmd(os.Args[2:])
	case "prune":
		pruneCmd(os.Args[2:])
	case "usages":
		usagesCmd(os.Args[2:])
	case "fleet":
		fleetCmd(os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "mcp":
		mcpCmd()
	case "fe":
		feCmd()
	case "version":
		fmt.Println("aimebu", resolveVersion())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
	}
}

func prepareServerOwnership(rootDir string) error {
	running, pid, err := server.DaemonStatus(rootDir)
	if err != nil {
		return err
	}
	if running {
		return fmt.Errorf("aimebu is already running (pid %d)", pid)
	}
	return config.MigrateServer(rootDir)
}

// ── Server ─────────────────────────────────────────────────────────

func serverCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu server <serve|start|stop|status>")
		os.Exit(1)
	}

	rootDir := config.Root()
	addr := server.DefaultAddr()

	switch args[0] {
	case "serve":
		if os.Getenv("AIMEBU_DAEMON_CHILD") == "" {
			if err := prepareServerOwnership(rootDir); err != nil {
				fatal("serve", err)
			}
		}
		frontendFS, _ := fs.Sub(aimebu.FrontendFS, "frontend")
		promptDefaults := mcp.BuiltinPromptDefaults()
		for k, v := range AgentBuiltinSpawnDefaults() {
			promptDefaults[k] = v
		}
		build := server.BuildInfo{
			Version:   resolveVersion(),
			GoVersion: runtime.Version(),
		}
		if err := server.Run(addr, rootDir, frontendFS, promptDefaults, build); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "start":
		if err := prepareServerOwnership(rootDir); err != nil {
			fatal("start", err)
		}
		selfBin, err := os.Executable()
		if err != nil {
			fatal("resolve executable path", err)
		}
		if err := server.DaemonStart(selfBin, addr, rootDir); err != nil {
			fatal("start", err)
		}

	case "stop":
		if err := server.DaemonStop(rootDir); err != nil {
			fatal("stop", err)
		}

	case "status":
		running, pid, err := server.DaemonStatus(rootDir)
		if err != nil {
			fatal("status", err)
		}
		if running {
			fmt.Printf("aimebu is running (pid %d)\n", pid)
		} else {
			fmt.Println("aimebu is not running")
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown server command: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: aimebu server <serve|start|stop|status>")
		os.Exit(1)
	}
}

func pruneCmd(args []string) {
	var autoYes, all bool
	for _, a := range args {
		switch a {
		case "-y":
			autoYes = true
		case "-a":
			all = true
		}
	}

	if !autoYes {
		stat, err := os.Stdin.Stat()
		if err != nil || stat.Mode()&fs.ModeCharDevice == 0 {
			fatal("prune", fmt.Errorf("stdin is not a terminal; use -y to bypass confirmation"))
		}
		rootDir := config.Root()
		if all {
			fmt.Printf("This will permanently delete EVERYTHING under %s:\n", rootDir)
			fmt.Println("  • server/rooms.json          (all rooms and membership)")
			fmt.Println("  • server/messages.json       (full conversation history)")
			fmt.Println("  • server/agents.json         (all registered agents)")
			fmt.Println("  • agents/agent-sessions.json (aimebu agent resume state)")
			fmt.Println("  • agents/agent-warning-acknowledged (first-run warning acknowledgement)")
			fmt.Println("  • server/macros.json         (global + per-room macros)")
			fmt.Println("  • server/fleet.json          (fleet command bundles)")
			fmt.Println("  • agents/agent-logs/*        (runtime diagnostics, opt-in via AIMEBU_AGENT_DEBUG)")
			fmt.Println()
			fmt.Println("Preserved:")
			fmt.Println("  • server/aimebu.pid, server/aimebu.log (runtime artifacts)")
		} else {
			fmt.Println("This will permanently delete:")
			fmt.Println("  • server/rooms.json          (all rooms and membership)")
			fmt.Println("  • server/messages.json       (full conversation history)")
			fmt.Println("  • server/agents.json         (all registered agents)")
			fmt.Println("  • agents/agent-sessions.json (aimebu agent resume state)")
			fmt.Println("  • agents/agent-logs/*        (runtime diagnostics, opt-in via AIMEBU_AGENT_DEBUG)")
			fmt.Println()
			fmt.Println("Preserved:")
			fmt.Println("  • agents/agent-warning-acknowledged (first-run warning acknowledgement)")
			fmt.Println("  • server/macros.json         (global + per-room macros)")
			fmt.Println("  • server/fleet.json          (fleet command bundles)")
			fmt.Println("  • server/aimebu.pid, server/aimebu.log (runtime artifacts)")
		}
		fmt.Print("\nAre you sure? [y/N]: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		if strings.ToLower(strings.TrimSpace(scanner.Text())) != "y" {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
	}

	rootDir := config.Root()
	c := client.DefaultClient()
	result, err := pruneViaServerOrLocal(c, rootDir, all)
	if err != nil {
		fatal("prune", err)
	}
	fmt.Println(client.PrettyJSON(result))

	// agent-sessions.json and debug logs are always removed; user-setting
	// markers are only removed with -a.
	if err := pruneLocalSidecars(rootDir, all); err != nil {
		fmt.Fprintf(os.Stderr, "warn: %v\n", err)
	}
}

func pruneViaServerOrLocal(c *client.Client, rootDir string, all bool) (string, error) {
	path := "/all"
	if all {
		path = "/all?include_settings=true"
	}
	result, err := c.Delete(path)
	if err == nil {
		return result, nil
	}
	if !client.IsUnreachable(err) || !pruneCanUseOfflineFallback(c.BaseURL) {
		return "", err
	}
	if err := config.MigrateServer(rootDir); err != nil {
		return "", fmt.Errorf("offline prune: migrate server config: %w", err)
	}
	if err := config.MigrateAgents(rootDir); err != nil {
		return "", fmt.Errorf("offline prune: migrate agent config: %w", err)
	}
	serverDir := filepath.Join(rootDir, "server")
	if err := server.PruneDataDir(serverDir, all); err != nil {
		return "", fmt.Errorf("offline prune: %w", err)
	}
	return `{"status":"cleared","mode":"offline"}`, nil
}

func pruneLocalSidecars(rootDir string, includeSettings bool) error {
	for _, sessPath := range []string{
		filepath.Join(rootDir, "agent-sessions.json"),
		filepath.Join(rootDir, "agents", "agent-sessions.json"),
	} {
		if err := os.Remove(sessPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", sessPath, err)
		}
	}

	if err := removeDirContents(filepath.Join(rootDir, "agents", "agent-logs")); err != nil {
		return fmt.Errorf("could not remove agent logs: %w", err)
	}

	if !includeSettings {
		return nil
	}

	for _, markerPath := range []string{
		filepath.Join(rootDir, "agent-warning-acknowledged"),
		filepath.Join(rootDir, "agents", "agent-warning-acknowledged"),
	} {
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", markerPath, err)
		}
	}

	return nil
}

func removeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func pruneCanUseOfflineFallback(baseURL string) bool {
	u, err := neturl.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ── MCP ────────────────────────────────────────────────────────────

func mcpCmd() {
	url := os.Getenv("AIMEBU_URL")
	if url == "" {
		url = "http://localhost:9997"
	}
	httpc := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpc.Get(url + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "aimebu server is not running. Start it first:")
		fmt.Fprintln(os.Stderr, "  aimebu server start")
		fmt.Fprintln(os.Stderr, "  or: brew services start aimebu")
		os.Exit(1)
	}
	resp.Body.Close()

	c := client.DefaultClient()
	if err := mcp.Run(c); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ── Frontend (open in browser) ─────────────────────────────────────

func feCmd() {
	url := os.Getenv("AIMEBU_URL")
	if url == "" {
		url = "http://localhost:9997"
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default: // linux, *bsd
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Open %s in your browser\n", url)
	}
}

// ── Helpers ────────────────────────────────────────────────────────

// resolveVersion returns the best available version string. Build-time
// ldflags take highest priority. For plain `go install` builds, it falls back
// to the VCS info embedded by the Go toolchain via runtime/debug.ReadBuildInfo.
func resolveVersion() string {
	if version != "v0.0.0" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" && strings.HasPrefix(v, "v") {
		return v
	}
	var rev, ts string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				rev = s.Value[:12]
			} else {
				rev = s.Value
			}
		case "vcs.time":
			ts = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	result := rev
	if ts != "" {
		result += " " + ts
	}
	result += " (devel)"
	if dirty {
		result += " dirty"
	}
	return result
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "Error: %s: %v\n", msg, err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintf(os.Stderr, `aimebu — AI Message Bus

Usage: aimebu <command> [args]

Server:
  server serve                        Run server in foreground
  server start                        Start server as daemon
  server stop                         Stop the daemon
  server status                       Check if daemon is running

Utilities:
  fleet [name] [path]                 List fleets, or launch one against path/cwd
  prune [-y] [-a]                     Prune conversation history with confirmation prompt
                                        -y  skip confirmation
                                        -a  also wipe macros and fleets (user settings)
                                        falls back to direct local cleanup when
                                        AIMEBU_URL is loopback and the server is down
  usages [provider] [--plain|--json]  Show provider usage snapshots

Integration:
  agent [--harness h] [--room r...] [--auto-room] -- <cmd>
                                                Wrap a harness CLI with session-lifecycle management
  mcp                                           Start MCP stdio server (for AI assistants)
  fe                                            Open the web UI in your browser

Other:
  version                             Print version
  help                                Show this help

Environment:
  AIMEBU_URL       Server URL (default: http://localhost:9997)
  AIMEBU_PORT      Server listen port (default: 9997)
  AIMEBU_BIND      Server bind address (default: 127.0.0.1)
  AIMEBU_ALLOW     Comma-separated IPs/CIDRs allowed to connect (default: 127.0.0.0/8,::1/128)
  AIMEBU_TLS_CERT  TLS certificate PEM path; set with AIMEBU_TLS_KEY for HTTPS
  AIMEBU_TLS_KEY   TLS private key PEM path; set with AIMEBU_TLS_CERT for HTTPS
  AIMEBU_TLS_PORT  HTTPS listen port when TLS is configured (default: 9996)
  AIMEBU_CONFIG_DIR  Config root directory (default: ~/.aimebu)
  AIMEBU_INSECURE_SKIP_VERIFY  Disable client TLS verification for development self-signed certs
  AIMEBU_USAGES_REFRESH  Override usage refresh interval in seconds (minimum 15)

Note: Humans use the web UI for bus conversations. AI assistants use the MCP
server (aimebu mcp), which assigns names automatically via bus_register.

Web UI: http://localhost:9997 (when server is running)
`)
	os.Exit(0)
}
