package server

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

var roomIDUnsafeRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func sanitizeRoomID(roomID string) string {
	return roomIDUnsafeRE.ReplaceAllString(roomID, "_")
}

func exportFilename(roomID, format string) string {
	ext := "json"
	if format == "markdown" {
		ext = "md"
	}
	ts := time.Now().UTC().Format("20060102-150405Z")
	return fmt.Sprintf("%s-%s.%s", sanitizeRoomID(roomID), ts, ext)
}

type exportEnvelope struct {
	Room     exportRoomMeta  `json:"room"`
	Messages []types.Message `json:"messages"`
}

type exportRoomMeta struct {
	ID         string   `json:"id"`
	Members    []string `json:"members"`
	CreatedAt  string   `json:"created_at"`
	ExportedAt string   `json:"exported_at"`
	ExportedBy string   `json:"exported_by"`
}

func renderJSON(w http.ResponseWriter, room *types.Room, msgs []types.Message, agentID string) {
	w.Header().Set("Content-Type", "application/json")
	env := exportEnvelope{
		Room: exportRoomMeta{
			ID:         room.ID,
			Members:    room.Members,
			CreatedAt:  room.CreatedAt,
			ExportedAt: time.Now().UTC().Format(time.RFC3339),
			ExportedBy: agentID,
		},
		Messages: msgs,
	}
	_ = json.NewEncoder(w).Encode(env)
}

func renderMarkdown(w http.ResponseWriter, room *types.Room, msgs []types.Message, agentID string) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	exportedAt := time.Now().UTC().Format(time.RFC3339)

	var b strings.Builder
	fmt.Fprintf(&b, "# Room: %s\n\n", room.ID)
	fmt.Fprintf(&b, "**Exported:** %s by %s\n", exportedAt, agentID)
	fmt.Fprintf(&b, "**Members:** %s\n\n", strings.Join(room.Members, ", "))
	b.WriteString("---\n\n")

	for _, m := range msgs {
		ts := m.CreatedAt
		if t, err := time.Parse(time.RFC3339, m.CreatedAt); err == nil {
			ts = t.UTC().Format("2006-01-02 15:04:05")
		}
		from := m.From
		if m.FromKind == "system" {
			// m.From is "_system"; strip the leading underscore before italicizing
			from = "_" + strings.TrimPrefix(from, "_") + "_"
		}
		fmt.Fprintf(&b, "**[%s] #%d %s:**\n", ts, m.ID, from)
		for line := range strings.SplitSeq(m.Body, "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
		b.WriteString("\n")
	}

	_, _ = w.Write([]byte(b.String()))
}

func handleExportRoom(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		agentID := r.URL.Query().Get("agent_id")
		format := r.URL.Query().Get("format")

		if agentID == "" {
			jsonError(w, "agent_id is required", http.StatusBadRequest)
			return
		}
		if format != "json" && format != "markdown" {
			jsonError(w, "format must be json or markdown", http.StatusBadRequest)
			return
		}

		room := s.getRoom(roomID)
		if room == nil {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}
		if !s.isMember(roomID, agentID) {
			jsonError(w, "agent is not a member of this room", http.StatusForbidden)
			return
		}

		// Full history, sorted oldest-first (sinceID=0 returns all)
		msgs := s.messagesSince(roomID, 0)

		filename := exportFilename(roomID, format)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

		if format == "json" {
			renderJSON(w, room, msgs, agentID)
		} else {
			renderMarkdown(w, room, msgs, agentID)
		}
	}
}
