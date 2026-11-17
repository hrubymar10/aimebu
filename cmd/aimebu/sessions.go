package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/adapter"
	"github.com/hrubymar10/aimebu/internal/configroot"
	"github.com/hrubymar10/aimebu/internal/sqliteutil"
)

type sessionRow struct {
	ID, Source, State, Tool, Variant, Launcher, Room, LastTurn, Resume, When, Checkout string
	Generation                                                                         int
	Remote, Containerized                                                              bool
}

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
	db, err := sql.Open("sqlite", sqliteutil.DSN(path, true))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
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
	db, err := sessionDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := queryRetry(ctx, db, `SELECT a.id,a.desired,a.observed,a.tool,a.variant,COALESCE(a.launcher_id,''),a.generation,c.canonical_path,machine.is_host=0,COALESCE(l.containerized,0),
		COALESCE((SELECT m.room_id FROM room_member m WHERE m.member_kind='agent' AND m.member_id=a.id AND m.left_at IS NULL ORDER BY m.joined_at,m.room_id LIMIT 1),''),
		COALESCE((SELECT t.id FROM turn t WHERE t.agent_id=a.id ORDER BY COALESCE(t.started_at,'' ) DESC,t.id DESC LIMIT 1),''),
		COALESCE((SELECT t.started_at FROM turn t WHERE t.agent_id=a.id ORDER BY COALESCE(t.started_at,'' ) DESC,t.id DESC LIMIT 1),''),
		COALESCE(s.native_session_id,'')
		FROM agent a JOIN checkout c ON c.id=a.checkout_id JOIN machine ON machine.id=a.machine_id
		LEFT JOIN machine_launcher l ON l.id=a.launcher_id AND l.machine_id=a.machine_id
		LEFT JOIN session s ON s.agent_id=a.id AND s.generation=a.generation AND s.ended_at IS NULL AND s.native_session_started_at IS NOT NULL
		ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sessionRow{}
	for rows.Next() {
		var row sessionRow
		var desired, observed, handle string
		if err := rows.Scan(&row.ID, &desired, &observed, &row.Tool, &row.Variant, &row.Launcher, &row.Generation, &row.Checkout, &row.Remote, &row.Containerized, &row.Room, &row.LastTurn, &row.When, &handle); err != nil {
			return nil, err
		}
		row.Source = "server"
		row.State = observed
		if desired != "active" {
			row.State += "/" + desired
		}
		row.Resume = resumeHint(row, handle, filepath.Dir(filepath.Dir(path)))
		out = append(out, row)
	}
	return out, rows.Err()
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
