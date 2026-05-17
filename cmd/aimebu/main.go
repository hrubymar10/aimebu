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
	case "create-room":
		createRoomCmd(os.Args[2:])
	case "delete-room":
		deleteRoomCmd(os.Args[2:])
	case "join":
		joinCmd(os.Args[2:])
	case "leave":
		leaveCmd(os.Args[2:])
	case "say":
		sayCmd(os.Args[2:])
	case "read":
		readCmd(os.Args[2:])
	case "rooms":
		roomsCmd(os.Args[2:])
	case "dm":
		dmCmd(os.Args[2:])
	case "register":
		registerCmd(os.Args[2:])
	case "agents":
		agentsCmd()
	case "sniff":
		sniffCmd(os.Args[2:])
	case "prune":
		pruneCmd(os.Args[2:])
	case "usages":
		usagesCmd(os.Args[2:])
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

// ── Identity helpers (humans only) ─────────────────────────────────

// extractName pulls `--name foo` / `--name=foo` out of args, returning the
// name plus the filtered args. Falls back to $AIMEBU_NAME if the flag isn't
// present. Exits with an error if no name is resolved — human CLI usage
// always needs a name.
func extractName(args []string) (string, []string) {
	name := ""
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			name = args[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			name = strings.TrimPrefix(a, "--name=")
		default:
			out = append(out, a)
		}
	}
	if name == "" {
		name = os.Getenv("AIMEBU_NAME")
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: --name <your-name> is required (or set $AIMEBU_NAME).")
		fmt.Fprintln(os.Stderr, "Example: aimebu say general --name martin \"hello\"")
		os.Exit(1)
	}
	return name, out
}

// humanClient builds a Client, extracts --name from args, and ensures the
// human is registered on the server (idempotent — repeated calls just
// refresh last_seen). Returns the client and the remaining args.
func humanClient(args []string) (*client.Client, []string) {
	name, rest := extractName(args)
	c := client.DefaultClient()
	c.AgentID = name

	project := ""
	if cwd, err := os.Getwd(); err == nil {
		project = filepath.Base(cwd)
	}

	meta := map[string]string{"protocol": "cli"}
	if cwd, err := os.Getwd(); err == nil {
		meta["cwd"] = cwd
	}
	if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		meta["branch"] = strings.TrimSpace(string(out))
	}

	_, err := c.Post("/agents", map[string]any{
		"kind":    "human",
		"name":    name,
		"project": project,
		"meta":    meta,
	})
	if err != nil {
		fatal("register (auto)", err)
	}
	return c, rest
}

// ── Room Commands ──────────────────────────────────────────────────

func createRoomCmd(args []string) {
	c, rest := humanClient(args)
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu create-room <room> --name <your-name>")
		os.Exit(1)
	}
	result, err := c.Post("/rooms", map[string]string{
		"id":         rest[0],
		"created_by": c.AgentID,
	})
	if err != nil {
		fatal("create-room", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func deleteRoomCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu delete-room <room>")
		os.Exit(1)
	}
	c := client.DefaultClient()
	result, err := c.Delete("/rooms/" + args[0])
	if err != nil {
		fatal("delete-room", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func joinCmd(args []string) {
	c, rest := humanClient(args)
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu join <room> --name <your-name>")
		os.Exit(1)
	}
	result, err := c.Post("/rooms/"+rest[0]+"/join", map[string]string{
		"agent_id": c.AgentID,
	})
	if err != nil {
		fatal("join", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func leaveCmd(args []string) {
	c, rest := humanClient(args)
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu leave <room> --name <your-name>")
		os.Exit(1)
	}
	result, err := c.Post("/rooms/"+rest[0]+"/leave", map[string]string{
		"agent_id": c.AgentID,
	})
	if err != nil {
		fatal("leave", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func sayCmd(args []string) {
	c, rest := humanClient(args)
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu say <room> <message> --name <your-name>")
		os.Exit(1)
	}
	room := rest[0]
	body := strings.Join(rest[1:], " ")

	// Ensure the human is a member before sending — the server requires it.
	if _, err := c.Post("/rooms/"+room+"/join", map[string]string{"agent_id": c.AgentID}); err != nil {
		fatal("auto-join", err)
	}

	result, err := c.Post("/rooms/"+room+"/send", map[string]string{
		"from": c.AgentID,
		"body": body,
	})
	if err != nil {
		fatal("say", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func readCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu read <room> [--limit N]")
		os.Exit(1)
	}
	c := client.DefaultClient()
	room := args[0]
	limit := "50"
	for i, arg := range args[1:] {
		if arg == "--limit" && i+2 < len(args) {
			limit = args[i+2]
		}
	}
	result, err := c.Get("/rooms/" + room + "/messages?limit=" + limit)
	if err != nil {
		fatal("read", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func roomsCmd(args []string) {
	c, _ := humanClient(args)
	result, err := c.Get("/agents/" + c.AgentID + "/rooms")
	if err != nil {
		fatal("rooms", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func dmCmd(args []string) {
	c, rest := humanClient(args)
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu dm <recipient> <message> --name <your-name>")
		os.Exit(1)
	}
	to := rest[0]
	body := strings.Join(rest[1:], " ")
	result, err := c.Post("/dm", map[string]string{
		"from": c.AgentID,
		"to":   to,
		"body": body,
	})
	if err != nil {
		fatal("dm", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

// ── Agents ─────────────────────────────────────────────────────────

// register explicitly registers a human. Mostly useful for setting extra
// metadata (key=val args). Regular use doesn't need this — say/join/dm all
// auto-register.
func registerCmd(args []string) {
	c, rest := humanClient(args)

	project := ""
	if cwd, err := os.Getwd(); err == nil {
		project = filepath.Base(cwd)
	}

	meta := map[string]string{"protocol": "cli"}
	for _, arg := range rest {
		if k, v, ok := strings.Cut(arg, "="); ok {
			meta[k] = v
		}
	}
	result, err := c.Post("/agents", map[string]any{
		"kind":    "human",
		"name":    c.AgentID,
		"project": project,
		"meta":    meta,
	})
	if err != nil {
		fatal("register", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func agentsCmd() {
	c := client.DefaultClient()
	result, err := c.Get("/agents")
	if err != nil {
		fatal("agents", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

// ── Monitoring ─────────────────────────────────────────────────────

func sniffCmd(args []string) {
	follow := false
	room := ""
	limit := "100"
	for _, arg := range args {
		if arg == "-f" || arg == "--follow" {
			follow = true
		} else if room == "" {
			room = arg
		} else {
			limit = arg
		}
	}

	c := client.DefaultClient()

	if follow {
		sniffFollow(c, room)
		return
	}

	var path string
	if room != "" {
		path = "/rooms/" + room + "/messages?limit=" + limit
	} else {
		path = "/messages?limit=" + limit
	}
	result, err := c.Get(path)
	if err != nil {
		fatal("sniff", err)
	}
	fmt.Println(client.PrettyJSON(result))
}

func sniffFollow(c *client.Client, room string) {
	url := c.BaseURL + "/firehose"
	if room != "" {
		url = c.BaseURL + "/rooms/" + room + "/firehose"
	}

	resp, err := http.Get(url)
	if err != nil {
		fatal("firehose", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		raw := line[len("data: "):]
		fmt.Println(client.PrettyJSON(raw))
	}
	if err := scanner.Err(); err != nil {
		fatal("firehose stream", err)
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
			fmt.Println()
			fmt.Println("Preserved:")
			fmt.Println("  • server/aimebu.pid, server/aimebu.log (runtime artifacts)")
			fmt.Println("  • agents/agent-logs/                   (runtime diagnostics, opt-in via AIMEBU_AGENT_DEBUG)")
		} else {
			fmt.Println("This will permanently delete:")
			fmt.Println("  • server/rooms.json          (all rooms and membership)")
			fmt.Println("  • server/messages.json       (full conversation history)")
			fmt.Println("  • server/agents.json         (all registered agents)")
			fmt.Println("  • agents/agent-sessions.json (aimebu agent resume state)")
			fmt.Println()
			fmt.Println("Preserved:")
			fmt.Println("  • agents/agent-warning-acknowledged (first-run warning acknowledgement)")
			fmt.Println("  • server/macros.json         (global + per-room macros)")
			fmt.Println("  • server/aimebu.pid, server/aimebu.log (runtime artifacts)")
			fmt.Println("  • agents/agent-logs/                   (runtime diagnostics, opt-in via AIMEBU_AGENT_DEBUG)")
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

	// agent-sessions.json is always removed (conversation state); user-setting
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

Rooms (humans — require --name or $AIMEBU_NAME):
  create-room <room> --name N         Create a new room
  delete-room <room>                  Delete a room and its messages
  join <room> --name N                Join a room (auto-creates if needed)
  leave <room> --name N               Leave a room
  say <room> <msg> --name N           Send a message to a room
  read <room> [--limit N]             Read messages from a room (no name needed)
  rooms --name N                      List rooms you're in
  dm <recipient> <msg> --name N       Direct message

Agents:
  register [key=val ...] --name N     Explicitly register a human with metadata
  agents                              List registered agents

Monitoring:
  sniff [room] [limit]                Show recent messages (default: 100)
  sniff -f [room]                     Follow mode: stream messages in real time
  prune [-y] [-a]                     Prune conversation history with confirmation prompt
                                        -y  skip confirmation
                                        -a  also wipe macros (user settings)
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
  AIMEBU_NAME      Your human name (e.g. "martin"). Used instead of --name.
  AIMEBU_PORT      Server listen port (default: 9997)
  AIMEBU_BIND      Server bind address (default: 127.0.0.1)
  AIMEBU_ALLOW     Comma-separated IPs/CIDRs allowed to connect (default: 127.0.0.0/8,::1/128)
  AIMEBU_CONFIG_DIR  Config root directory (default: ~/.aimebu)
  AIMEBU_USAGES_REFRESH  Override usage refresh interval in seconds (minimum 15)

Note: The CLI is for humans. AI assistants use the MCP server (aimebu mcp),
which assigns names automatically via bus_register.

Web UI: http://localhost:9997 (when server is running)
`)
	os.Exit(0)
}
