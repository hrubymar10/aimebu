package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/client"
	"github.com/hrubymar10/aimebu/internal/types"
)

type sessionListRow struct {
	FullID        string
	Origin        string
	Source        string
	Harness       string
	Model         string
	Project       string
	CWD           string
	SessionID     string
	ResumeCommand string
	Seen          time.Time
}

func sessionsCmd(args []string) {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "Unknown sessions command: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "Usage: aimebu sessions")
		os.Exit(1)
	}
	local, err := agentLoadSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to load local sessions: %v\n", err)
	}
	serverRows, err := loadServerAgentSessions(client.DefaultClient())
	if err != nil {
		if client.IsUnreachable(err) {
			fmt.Fprintf(os.Stderr, "warn: server unavailable; showing local sessions only: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "warn: failed to load server sessions: %v\n", err)
		}
	}
	rows := mergeSessionRows(local, serverRows)
	printSessionRows(os.Stdout, rows)
}

func loadServerAgentSessions(c *client.Client) ([]types.AgentSession, error) {
	body, err := c.Get("/agent-sessions")
	if err != nil {
		return nil, err
	}
	var env struct {
		AgentSessions []types.AgentSession `json:"agent_sessions"`
		Error         string               `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return nil, err
	}
	if env.Error != "" {
		return nil, errors.New(env.Error)
	}
	return env.AgentSessions, nil
}

func mergeSessionRows(local []agentSession, serverRows []types.AgentSession) []sessionListRow {
	rows := make(map[string]sessionListRow)
	for _, sess := range local {
		fullID := agentFullID(sess.Name)
		if fullID == "" {
			continue
		}
		rows[fullID] = sessionListRow{
			FullID:        fullID,
			Origin:        "wrapper",
			Source:        "local",
			Harness:       sess.Harness,
			Model:         sess.Model,
			Project:       projectFromFullID(fullID),
			CWD:           sess.CWD,
			SessionID:     sess.SessionID,
			ResumeCommand: agentResumeCommandHint(sess),
			Seen:          sess.LastUsed,
		}
	}
	for _, sess := range serverRows {
		if sess.FullID == "" {
			continue
		}
		row := sessionListRow{
			FullID:        sess.FullID,
			Origin:        sess.Origin,
			Source:        "server",
			Harness:       sess.Harness,
			Model:         sess.Model,
			Project:       sess.Project,
			CWD:           sess.CWD,
			SessionID:     sess.HarnessSessionID,
			ResumeCommand: sess.ResumeCommand,
			Seen:          sess.LastSeen,
		}
		if existing, ok := rows[sess.FullID]; ok {
			row.Source = "local+server"
			if row.Origin == "" {
				row.Origin = existing.Origin
			}
			if row.Harness == "" {
				row.Harness = existing.Harness
			}
			if row.Model == "" {
				row.Model = existing.Model
			}
			if row.Project == "" {
				row.Project = existing.Project
			}
			if row.CWD == "" {
				row.CWD = existing.CWD
			}
			if row.SessionID == "" {
				row.SessionID = existing.SessionID
			}
			if row.ResumeCommand == "" {
				row.ResumeCommand = existing.ResumeCommand
			}
			if row.Seen.IsZero() || existing.Seen.After(row.Seen) {
				row.Seen = existing.Seen
			}
		}
		rows[sess.FullID] = row
	}
	out := make([]sessionListRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seen.Equal(out[j].Seen) {
			return out[i].FullID < out[j].FullID
		}
		return out[i].Seen.After(out[j].Seen)
	})
	return out
}

func printSessionRows(w io.Writer, rows []sessionListRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No agent sessions found.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tORIGIN\tSOURCE\tHARNESS\tPROJECT\tSESSION\tSEEN\tCWD")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.FullID,
			emptyDash(row.Origin),
			emptyDash(row.Source),
			emptyDash(row.Harness),
			emptyDash(row.Project),
			emptyDash(row.SessionID),
			formatSessionSeen(row.Seen),
			emptyDash(row.CWD),
		)
		if row.ResumeCommand != "" {
			fmt.Fprintf(tw, "\t\t\t\t\tresume\t\t%s\n", row.ResumeCommand)
		}
	}
	_ = tw.Flush()
}

func projectFromFullID(fullID string) string {
	idx := strings.LastIndex(fullID, "@")
	if idx < 0 || idx == len(fullID)-1 {
		return ""
	}
	return fullID[idx+1:]
}

func formatSessionSeen(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
