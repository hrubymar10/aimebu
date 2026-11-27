package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/configroot"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/sqliteutil"
	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/supervisor"
)

type sessionRow struct {
	ID, Source, State, Tool, Variant, Launcher, Room, LastTurn, Resume, When, Checkout string
	Generation                                                                         int
	Remote, Containerized                                                              bool
}

var sessionsOpenStoreReadOnly = store.OpenReadOnly

func sessionsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: aimebu sessions")
		return 2
	}
	root, err := sessionsConfigRoot()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	local, localErr := localSessionRows(ctx, filepath.Join(root, "supervisor", "journal.sqlite"))
	server, serverErr := serverSessionRows(ctx, filepath.Join(root, "server", "aimebu.sqlite"))
	if os.IsNotExist(localErr) {
		localErr = nil
	}
	if os.IsNotExist(serverErr) {
		serverErr = nil
	}
	if localErr != nil {
		fmt.Fprintf(stderr, "warn: local sessions: %v\n", localErr)
	}
	if serverErr != nil {
		fmt.Fprintf(stderr, "warn: server sessions: %v\n", serverErr)
	}
	rows := mergeSessionRows(local, server)
	printSessionRows(stdout, rows)
	if localErr != nil && serverErr != nil {
		return 1
	}
	return 0
}

func sessionsConfigRoot() (string, error) {
	return configroot.FromEnv()
}

// Open the two databases independently and read-only. Neither the server store
// nor the supervisor journal opener is suitable here: both migrate or write.
// queryRetry is QueryContext with sqliteutil.Retry: a read-only connection that races another process opening the database
// (WAL recovery) is told "database is locked" at once, whatever the busy timeout.
func queryRetry(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := sqliteutil.Retry(func() error {
		var queryErr error
		rows, queryErr = db.QueryContext(ctx, query, args...)
		return queryErr
	})
	return rows, err
}

func sessionDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return supervisor.OpenJournalReadOnly(path)
}

func localSessionRows(ctx context.Context, path string) ([]sessionRow, error) {
	db, err := sessionDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := queryRetry(ctx, db, `WITH latest AS (SELECT t.*,
		ROW_NUMBER() OVER (PARTITION BY agent_id ORDER BY started_at DESC,turn_id DESC) AS rn FROM turn_start t)
		SELECT t.agent_id,t.turn_id,t.generation,t.exec,t.started_at,t.spec_json,
		COALESCE((SELECT json_extract(e.payload_json,'$.native_session_handle') FROM journal_event e
		WHERE e.turn_id=t.turn_id AND e.kind='native_session_started' ORDER BY e.created_at DESC LIMIT 1),'')
		FROM latest t WHERE t.rn=1 ORDER BY t.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sessionRow{}
	for rows.Next() {
		var row sessionRow
		var raw []byte
		var handle string
		if err := rows.Scan(&row.ID, &row.LastTurn, &row.Generation, &row.State, &row.When, &raw, &handle); err != nil {
			return nil, err
		}
		var spec adapter.TurnSpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, fmt.Errorf("turn %s: %w", row.LastTurn, err)
		}
		if handle == "" && spec.NativeSessionStarted {
			handle = spec.NativeSessionHandle
		}
		row.Source = "local"
		row.Tool = spec.Agent.Tool
		row.Variant = spec.Agent.Variant
		row.Launcher = spec.Agent.LauncherID
		row.Containerized = spec.LauncherContainerized
		row.Checkout = spec.CheckoutPath
		row.Resume = resumeHint(row, handle, filepath.Dir(filepath.Dir(path)))
		out = append(out, row)
	}
	return out, rows.Err()
}

func serverSessionRows(ctx context.Context, path string) ([]sessionRow, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	// v1's database lives at this same path: neither the lock file nor an open is ours to make there.
	if err := refuseV1RootWrite(filepath.Dir(filepath.Dir(path))); err != nil {
		return nil, err
	}
	lock, err := lockServerDirectory(filepath.Dir(path))
	var source []protocol.DoctorSession
	if err != nil {
		source, err = fetchDoctorSessions(ctx, filepath.Dir(filepath.Dir(path)))
		if err != nil {
			return nil, fmt.Errorf("server running or lock held; server sessions unavailable: %w", err)
		}
	} else {
		defer unlockServerDirectory(lock)
		db, openErr := sessionsOpenStoreReadOnly(path)
		if openErr != nil {
			return nil, openErr
		}
		defer db.Close()
		source, err = store.ReadDoctorSessions(ctx, db)
		if err != nil {
			return nil, err
		}
	}
	out := make([]sessionRow, 0, len(source))
	root := filepath.Dir(filepath.Dir(path))
	for _, item := range source {
		row := sessionRow{ID: item.ID, Source: "server", State: item.Observed, Tool: item.Tool, Variant: item.Variant,
			Launcher: item.Launcher, Room: item.Room, LastTurn: item.LastTurn, When: item.When, Checkout: item.Checkout,
			Generation: item.Generation, Remote: item.Remote, Containerized: item.Containerized}
		if item.Desired != "active" {
			row.State += "/" + item.Desired
		}
		row.Resume = resumeHint(row, item.NativeSessionID, root)
		out = append(out, row)
	}
	return out, nil
}

func fetchDoctorSessions(ctx context.Context, root string) ([]protocol.DoctorSession, error) {
	config, err := resolveServerConfig()
	if err != nil {
		return nil, err
	}
	if config.root != root {
		return nil, fmt.Errorf("doctor root does not match session root")
	}
	if health := checkServerHealth(ctx, config.bind, config.port); health.Status != statusOK {
		return nil, fmt.Errorf("doctor route unavailable: %s", health.Description)
	}
	key, finding, ok := readDoctorKey(root)
	if !ok {
		return nil, fmt.Errorf("doctor key: %s", finding.Description)
	}
	address := net.JoinHostPort(config.bind, strconv.Itoa(config.port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/doctor/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := doctorHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doctor sessions route: HTTP %d", resp.StatusCode)
	}
	var answer struct {
		Sessions []protocol.DoctorSession `json:"sessions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&answer); err != nil {
		return nil, err
	}
	return answer.Sessions, nil
}

func resumeHint(row sessionRow, handle, root string) string {
	if handle == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(row.ID))
	agentHash := hex.EncodeToString(hash[:])
	switch row.Tool {
	case "claude":
		return "claude --resume " + shellSessionArg(handle)
	case "codex":
		if row.Remote || row.Variant == "docker" || row.Containerized {
			return ""
		}
		runtimeRoot := filepath.Join(root, "supervisor", "codex")
		return "CODEX_HOME=" + shellSessionArg(filepath.Join(runtimeRoot, agentHash)) + " codex exec resume " + shellSessionArg(handle)
	case "pi":
		if row.Remote {
			return ""
		}
		if row.Checkout == "" {
			return "session " + handle + " (checkout path unavailable)"
		}
		return "pi --session-dir " + shellSessionArg(filepath.Join(row.Checkout, ".aimebu", "pi-sessions", agentHash)) + " --session-id " + shellSessionArg(handle)
	case "opencode":
		if row.Remote || row.Variant == "docker" || row.Containerized {
			return "" // a Docker agent's session database is in its checkout, under the container's paths: no command is guessed
		}
		runtimeRoot := filepath.Join(root, "supervisor", "opencode")
		return "OPENCODE_DB=" + shellSessionArg(filepath.Join(runtimeRoot, agentHash, "opencode.db")) + " opencode --session " + shellSessionArg(handle)
	case "vibe":
		if row.Remote || row.Variant == "docker" || row.Containerized {
			return "" // a Docker agent's sessions are in its checkout, under the container's paths: no command is guessed
		}
		runtimeDir := filepath.Join(root, "supervisor", "vibe", agentHash)
		return "VIBE_HOME=" + shellSessionArg(filepath.Join(runtimeDir, "home")) + " VIBE_SESSION_LOGGING__SAVE_DIR=" + shellSessionArg(filepath.Join(runtimeDir, "sessions")) + " vibe --resume " + shellSessionArg(handle)
	default:
		return "session " + handle
	}
}

func shellSessionArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func mergeSessionRows(local, server []sessionRow) []sessionRow {
	byID := make(map[string]sessionRow, len(local)+len(server))
	for _, row := range local {
		byID[row.ID] = row
	}
	for _, row := range server {
		if old, ok := byID[row.ID]; ok {
			row.Source = "local+server"
			if row.LastTurn == "" || old.When > row.When {
				row.LastTurn, row.When = old.LastTurn, old.When
			}
			if row.Resume == "" && row.Generation == old.Generation && !row.Remote &&
				!(row.Tool == "codex" && (row.Variant == "docker" || row.Containerized)) {
				row.Resume = old.Resume
			}
			if row.Launcher == "" {
				row.Launcher = old.Launcher
			}
		}
		byID[row.ID] = row
	}
	out := make([]sessionRow, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].When != out[j].When {
			return out[i].When > out[j].When
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func printSessionRows(w io.Writer, rows []sessionRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No agent sessions found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tSOURCE\tSTATE\tTOOL\tLAUNCHER\tROOM\tLAST TURN\tWHEN\tRESUME")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Source,
			sessionDash(row.State), sessionDash(row.Tool), sessionDash(row.Launcher),
			sessionDash(row.Room), sessionDash(row.LastTurn), sessionDash(row.When), sessionDash(row.Resume))
	}
	_ = tw.Flush()
}

func sessionDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
