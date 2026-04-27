package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	aimebu "github.com/hrubymar10/aimebu"
	"github.com/hrubymar10/aimebu/internal/client"
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
	case "clear":
		clearCmd()
	case "mcp":
		mcpCmd()
	case "fe":
		feCmd()
	case "version":
		fmt.Println("aimebu", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
	}
}

// ── Server ─────────────────────────────────────────────────────────

func serverCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: aimebu server <serve|start|stop|status>")
		os.Exit(1)
	}

	addr := server.DefaultAddr()
	dataDir := server.DefaultDataDir()

	switch args[0] {
	case "serve":
		frontendFS, _ := fs.Sub(aimebu.FrontendFS, "frontend")
		if err := server.Run(addr, dataDir, frontendFS); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "start":
		selfBin, err := os.Executable()
		if err != nil {
			fatal("resolve executable path", err)
		}
		if err := server.DaemonStart(selfBin, addr, dataDir); err != nil {
			fatal("start", err)
		}

	case "stop":
		if err := server.DaemonStop(dataDir); err != nil {
			fatal("stop", err)
		}

	case "status":
		running, pid, err := server.DaemonStatus(dataDir)
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

	meta := map[string]string{}
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

	meta := make(map[string]string)
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

func clearCmd() {
	c := client.DefaultClient()
	result, err := c.Delete("/all")
	if err != nil {
		fatal("clear", err)
	}
	fmt.Println(client.PrettyJSON(result))
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
  clear                               Clear all rooms, messages, and agents

Integration:
  mcp                                 Start MCP stdio server (for AI assistants)
  fe                                  Open the web UI in your browser

Other:
  version                             Print version
  help                                Show this help

Environment:
  AIMEBU_URL       Server URL (default: http://localhost:9997)
  AIMEBU_NAME      Your human name (e.g. "martin"). Used instead of --name.
  AIMEBU_PORT      Server listen port (default: 9997)
  AIMEBU_BIND      Server bind address (default: 127.0.0.1)
  AIMEBU_DATA      Server data directory (default: ~/.aimebu)

Note: The CLI is for humans. AI assistants use the MCP server (aimebu mcp),
which assigns names automatically via bus_register.

Web UI: http://localhost:9997 (when server is running)
`)
	os.Exit(0)
}
