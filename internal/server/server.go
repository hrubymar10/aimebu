package server

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/config"
	"github.com/hrubymar10/aimebu/internal/types"
	"github.com/hrubymar10/aimebu/internal/usages"
)

var macroKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func registerStaticMimeTypes() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

func frontendFileServer(frontendFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(frontendFS))
	etag, hasETag := frontendBundleETag(frontendFS)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isMutableFrontendPath(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-cache")
			if hasETag {
				w.Header().Set("ETag", etag)
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

func isMutableFrontendPath(path string) bool {
	if path == "" || path == "/" {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".js", ".css":
		return true
	default:
		return false
	}
}

func frontendBundleETag(frontendFS fs.FS) (string, bool) {
	var paths []string
	if err := fs.WalkDir(frontendFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		log.Printf("Warning: could not hash frontend bundle: %v", err)
		return "", false
	}
	sort.Strings(paths)

	h := sha1.New()
	for _, path := range paths {
		data, err := fs.ReadFile(frontendFS, path)
		if err != nil {
			log.Printf("Warning: could not hash frontend bundle: %v", err)
			return "", false
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return `"fe-` + sum[:12] + `"`, true
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, data any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(data)
}

const busWaitTimeoutHint = "keep_waiting=true (status=\"still_waiting\") means no messages arrived yet, not that listening is over. Call bus_wait again now. Only return to the user if they explicitly told you to stop."

// validMP3Header checks that the first bytes of data look like an MP3 file:
// either an ID3v2 tag ("ID3") or an MPEG sync word (0xFF followed by 0xE0–0xFF).
func validMP3Header(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	if data[0] == 'I' && data[1] == 'D' && data[2] == '3' {
		return true // ID3v2 tag
	}
	return data[0] == 0xFF && data[1] >= 0xE0 // MPEG sync word
}

// validWAVHeader checks that data starts with a RIFF/WAVE container header.
func validWAVHeader(data []byte) bool {
	return len(data) >= 12 &&
		data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'A' && data[10] == 'V' && data[11] == 'E'
}

func allowedAttachmentMime(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func sanitizeAttachmentName(filename, ext string) string {
	name := filepath.Base(filename)
	if name == "." || strings.TrimSpace(name) == "" {
		name = "attachment." + ext
	}
	if utf8.RuneCountInString(name) > 96 {
		name = string([]rune(name)[:96])
	}
	return name
}

func attachmentDimensions(mimeType string, data []byte) (int, int, error) {
	if mimeType == "image/webp" {
		return webpDimensions(data)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("invalid webp header")
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, errors.New("short vp8x header")
		}
		w := 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16
		h := 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16
		return w, h, nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, errors.New("short vp8l header")
		}
		b0, b1, b2, b3 := int(data[21]), int(data[22]), int(data[23]), int(data[24])
		w := 1 + (((b1 & 0x3f) << 8) | b0)
		h := 1 + ((b3 << 6) | (b2 >> 2) | ((b1 & 0xc0) << 2))
		return w, h, nil
	case "VP8 ":
		if len(data) < 30 {
			return 0, 0, errors.New("short vp8 header")
		}
		w := int(data[26]) | int(data[27]&0x3f)<<8
		h := int(data[28]) | int(data[29]&0x3f)<<8
		return w, h, nil
	default:
		return 0, 0, errors.New("unsupported webp header")
	}
}

// BuildInfo carries the version and runtime details exposed via GET /buildinfo.
type BuildInfo struct {
	Version   string `json:"version"`
	GoVersion string `json:"go_version"`
}

func setupHandlers(mux *http.ServeMux, s *store, build BuildInfo, usageManager *usages.Manager) {

	// ── Rooms ──────────────────────────────────────────────────────

	// POST /rooms — create a room
	mux.HandleFunc("POST /rooms", func(w http.ResponseWriter, r *http.Request) {
		var req types.CreateRoomRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		room, err := s.createRoom(req.ID, req.CreatedBy)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = jsonOK(w, room)
	})

	// GET /rooms — list all rooms
	mux.HandleFunc("GET /rooms", func(w http.ResponseWriter, _ *http.Request) {
		rooms := s.listRooms()
		_ = jsonOK(w, map[string]any{"rooms": rooms})
	})

	// GET /rooms/{room_id} — room details + recent messages + per-member
	// presence (cursor + waiting). `members_presence` is additive; existing
	// fields are untouched.
	mux.HandleFunc("GET /rooms/{room_id}", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		room := s.getRoom(roomID)
		if room == nil {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}
		msgs := s.roomMessages(roomID, 50, 0)
		presence := s.roomPresence(roomID)
		_ = jsonOK(w, map[string]any{
			"room":             room,
			"messages":         msgs,
			"members_presence": presence,
		})
	})

	// DELETE /rooms/{room_id} — delete a room
	mux.HandleFunc("DELETE /rooms/{room_id}", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		if !s.deleteRoom(roomID) {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "deleted"})
	})

	// ── Room membership ────────────────────────────────────────────

	// POST /rooms/{room_id}/join
	mux.HandleFunc("POST /rooms/{room_id}/join", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		var req types.JoinRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.AgentID == "" {
			jsonError(w, "agent_id is required", http.StatusBadRequest)
			return
		}
		room, err := s.joinRoom(roomID, req.AgentID)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		_ = jsonOK(w, room)
	})

	// POST /rooms/{room_id}/leave
	mux.HandleFunc("POST /rooms/{room_id}/leave", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		var req types.LeaveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.AgentID == "" {
			jsonError(w, "agent_id is required", http.StatusBadRequest)
			return
		}
		if err := s.leaveRoomWithEvent(roomID, req.AgentID, req.Kicked); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "left", "room": roomID})
	})

	// ── Room messages ──────────────────────────────────────────────

	// POST /rooms/{room_id}/send
	mux.HandleFunc("POST /rooms/{room_id}/send", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		var req types.RoomSendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.From == "" || (req.Body == "" && len(req.Attachments) == 0) {
			jsonError(w, "from and body or attachments are required", http.StatusBadRequest)
			return
		}
		if req.From == "_system" || strings.HasPrefix(roomID, "_") {
			jsonError(w, "cannot send to reserved room or use reserved sender", http.StatusForbidden)
			return
		}
		id, err := s.roomSend(roomID, req.From, req.Body, req.NeedsAttention, req.ProposedAnswers, req.OpenQuestions, req.Attachments)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		resp := map[string]any{"id": id, "room": roomID}
		var warnings []string
		if warn := s.legacyPrefixWarn(req.From, req.Body); warn != "" {
			warnings = append(warnings, warn)
		}
		if warn := s.ambiguousMentionWarn(roomID, req.From, req.Body); warn != "" {
			warnings = append(warnings, warn)
		}
		if warn := s.attentionMissWarn(roomID, req.From, req.Body, req.NeedsAttention, nil); warn != "" {
			warnings = append(warnings, warn)
		}
		if len(warnings) > 0 {
			resp["warnings"] = warnings
		}
		_ = jsonOK(w, resp)
	})

	// GET /rooms/{room_id}/messages
	mux.HandleFunc("GET /rooms/{room_id}/messages", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		agentID := r.URL.Query().Get("agent_id")
		if agentID != "" {
			s.touchAgent(agentID)
		}
		if s.getRoom(roomID) == nil {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}

		limit := 50
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		var sinceID int64
		if sid := r.URL.Query().Get("since_id"); sid != "" {
			sinceID, _ = strconv.ParseInt(sid, 10, 64)
		}

		msgs := s.roomMessages(roomID, limit, sinceID)
		if agentID != "" {
			_ = jsonOK(w, map[string]any{"messages": annotate(msgs, agentID, s.addressingContext), "room": roomID})
		} else {
			_ = jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
		}
	})

	// GET /rooms/{room_id}/export — download full room history as JSON or Markdown
	mux.HandleFunc("GET /rooms/{room_id}/export", handleExportRoom(s))

	// GET /rooms/{room_id}/wait — long-poll: block until a new message arrives
	// in this room (messages with ID > since_id), or until timeout.
	// Query: since_id (int, default 0), timeout (seconds, default 30, max 600).
	// Returns {messages: [...], room: id} on success. On timeout:
	// {messages: [], room: id, status: "still_waiting", keep_waiting: true, hint: "..."}.
	mux.HandleFunc("GET /rooms/{room_id}/wait", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		agentID := r.URL.Query().Get("agent_id")
		if agentID != "" {
			s.touchAgent(agentID)
		}
		if s.getRoom(roomID) == nil {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}

		// Resolve since_id. If agent_id is present and the client didn't pass
		// an explicit since_id, fall back to the agent's read cursor so a
		// wait that returns after some time-away picks up unread messages.
		var sinceID int64
		sinceExplicit := false
		if sid := r.URL.Query().Get("since_id"); sid != "" {
			n, err := strconv.ParseInt(sid, 10, 64)
			if err == nil && n > 0 {
				sinceID = n
				sinceExplicit = true
			}
		}
		if !sinceExplicit && agentID != "" {
			sinceID = s.ensureRoomCursor(agentID, roomID)
		}

		timeoutSec := 30
		if t := r.URL.Query().Get("timeout"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 600 {
				timeoutSec = n
			}
		}

		// Fast path: messages already present. Don't mark the agent as
		// "waiting" — we're about to return immediately.
		if msgs := s.messagesSince(roomID, sinceID); len(msgs) > 0 {
			if agentID != "" {
				last := msgs[len(msgs)-1].ID
				// Filter own messages; cursor advances past them even without delivery.
				var filtered []types.Message
				for _, m := range msgs {
					if m.From != agentID {
						filtered = append(filtered, m)
					}
				}
				if err := jsonOK(w, map[string]any{"messages": annotate(filtered, agentID, s.addressingContext), "room": roomID}); err == nil {
					if s.advanceCursor(agentID, roomID, last) {
						s.broadcastReadUpdate(agentID, roomID, last)
					}
				}
			} else {
				_ = jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
			}
			return
		}

		// Mark presence as actively waiting on this room so the UI can
		// show a live-listening indicator. Defer the decrement so a
		// cancelled/panicking handler still cleans up.
		s.enterWait(agentID, roomID)
		defer s.leaveWait(agentID, roomID)

		ch := s.subscribeRoom(roomID)
		defer s.unsubscribeRoom(roomID, ch)

		timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
		defer timer.Stop()

		for {
			select {
			case msg := <-ch:
				if msg.ID <= sinceID {
					continue
				}
				if agentID != "" {
					s.touchAgent(agentID)
					if msg.From == agentID {
						// Self-message: advance cursor but don't deliver.
						if s.advanceCursor(agentID, roomID, msg.ID) {
							s.broadcastReadUpdate(agentID, roomID, msg.ID)
						}
						continue
					}
				}
				if r.Context().Err() != nil {
					return // client disconnected; preserve cursor so next reconnect replays
				}
				if agentID != "" {
					if err := jsonOK(w, map[string]any{"messages": annotate([]types.Message{msg}, agentID, s.addressingContext), "room": roomID}); err == nil {
						if s.advanceCursor(agentID, roomID, msg.ID) {
							s.broadcastReadUpdate(agentID, roomID, msg.ID)
						}
					}
				} else {
					_ = jsonOK(w, map[string]any{"messages": []types.Message{msg}, "room": roomID})
				}
				return
			case <-timer.C:
				_ = jsonOK(w, map[string]any{
					"messages":     []types.Message{},
					"room":         roomID,
					"status":       "still_waiting",
					"keep_waiting": true,
					"hint":         busWaitTimeoutHint,
				})
				return
			case <-r.Context().Done():
				return
			}
		}
	})

	// GET /agents/{id}/wait — long-poll: block until a new message arrives in
	// any room the agent is a member of (messages with ID > since_id), or
	// until timeout. Query: since_id, timeout (see /rooms/{id}/wait).
	mux.HandleFunc("GET /agents/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		s.touchAgent(agentID)

		// If since_id is explicitly provided use it, otherwise fall back to
		// the agent's per-room cursors (agentMessagesFromCursors handles
		// per-room filtering against the stored cursor and initializes new
		// cursors to HEAD-5).
		var sinceID int64
		sinceExplicit := false
		if sid := r.URL.Query().Get("since_id"); sid != "" {
			n, err := strconv.ParseInt(sid, 10, 64)
			if err == nil && n > 0 {
				sinceID = n
				sinceExplicit = true
			}
		}

		timeoutSec := 30
		if t := r.URL.Query().Get("timeout"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 && n <= 600 {
				timeoutSec = n
			}
		}

		var (
			msgs    []types.Message
			cursors map[string]int64
		)
		if sinceExplicit {
			msgs = s.agentMessagesSince(agentID, sinceID)
		} else {
			msgs, cursors = s.agentUnreadSinceCursor(agentID)
			_ = cursors
		}
		if len(msgs) > 0 {
			// Filter own messages — applies regardless of sinceExplicit.
			var filtered []types.Message
			for _, m := range msgs {
				if m.From != agentID {
					filtered = append(filtered, m)
				}
			}
			if err := jsonOK(w, map[string]any{"messages": annotate(filtered, agentID, s.addressingContext), "agent": agentID}); err == nil {
				if !sinceExplicit {
					// Advance cursors for every room we returned messages from
					// (including own messages — they advance the cursor but aren't
					// delivered to the sender).
					maxPerRoom := make(map[string]int64)
					for _, m := range msgs {
						if m.ID > maxPerRoom[m.RoomID] {
							maxPerRoom[m.RoomID] = m.ID
						}
					}
					for roomID, top := range maxPerRoom {
						if s.advanceCursor(agentID, roomID, top) {
							s.broadcastReadUpdate(agentID, roomID, top)
						}
					}
				}
			}
			return
		}

		// Agent-wide wait: presence is scoped to the agent, not to any one
		// room (roomID=""). FE treats the empty room as "applies to every
		// room this agent is in".
		s.enterWait(agentID, "")
		defer s.leaveWait(agentID, "")

		ch := s.subscribeGlobal()
		defer s.unsubscribeGlobal(ch)

		timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
		defer timer.Stop()

		for {
			select {
			case msg := <-ch:
				if sinceExplicit && msg.ID <= sinceID {
					continue
				}
				if !s.isMember(msg.RoomID, agentID) {
					continue
				}
				if !sinceExplicit {
					cur := s.ensureRoomCursor(agentID, msg.RoomID)
					if msg.ID <= cur {
						continue
					}
				}
				s.touchAgent(agentID)
				if msg.From == agentID {
					// Self-message: advance cursor but don't deliver.
					if !sinceExplicit {
						if s.advanceCursor(agentID, msg.RoomID, msg.ID) {
							s.broadcastReadUpdate(agentID, msg.RoomID, msg.ID)
						}
					}
					continue
				}
				if r.Context().Err() != nil {
					return // client disconnected; preserve cursor so next reconnect replays
				}
				if err := jsonOK(w, map[string]any{"messages": annotate([]types.Message{msg}, agentID, s.addressingContext), "agent": agentID}); err == nil {
					if !sinceExplicit {
						if s.advanceCursor(agentID, msg.RoomID, msg.ID) {
							s.broadcastReadUpdate(agentID, msg.RoomID, msg.ID)
						}
					}
				}
				return
			case <-timer.C:
				_ = jsonOK(w, map[string]any{
					"messages":     []types.Message{},
					"agent":        agentID,
					"status":       "still_waiting",
					"keep_waiting": true,
					"hint":         busWaitTimeoutHint,
				})
				return
			case <-r.Context().Done():
				return
			}
		}
	})

	// GET /rooms/{room_id}/firehose — per-room SSE
	mux.HandleFunc("GET /rooms/{room_id}/firehose", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch := s.subscribeRoom(roomID)
		defer s.unsubscribeRoom(roomID, ch)

		for {
			select {
			case msg := <-ch:
				data, _ := json.Marshal(msg)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// ── DM ─────────────────────────────────────────────────────────

	// POST /dm — open or send a DM (auto-creates private room).
	// body is optional: omit or send "" to create/return the DM room without
	// sending a message (useful for opening a DM channel from the UI).
	mux.HandleFunc("POST /dm", func(w http.ResponseWriter, r *http.Request) {
		var req types.DMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.From == "" || req.To == "" {
			jsonError(w, "from and to are required", http.StatusBadRequest)
			return
		}

		room, err := s.findOrCreateDM(req.From, req.To)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}

		if req.Body == "" && len(req.Attachments) == 0 {
			_ = jsonOK(w, map[string]any{"room": room.ID})
			return
		}

		id, err := s.roomSend(room.ID, req.From, req.Body, req.NeedsAttention, req.ProposedAnswers, req.OpenQuestions, req.Attachments)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		resp := map[string]any{"id": id, "room": room.ID}
		var warnings []string
		if warn := s.legacyPrefixWarn(req.From, req.Body); warn != "" {
			warnings = append(warnings, warn)
		}
		if warn := s.ambiguousMentionWarn(room.ID, req.From, req.Body); warn != "" {
			warnings = append(warnings, warn)
		}
		if warn := s.attentionMissWarn(room.ID, req.From, req.Body, req.NeedsAttention, []string{agentShortName(req.To)}); warn != "" {
			warnings = append(warnings, warn)
		}
		if len(warnings) > 0 {
			resp["warnings"] = warnings
		}
		_ = jsonOK(w, resp)
	})

	// ── Agents ─────────────────────────────────────────────────────

	// POST /agents — register. For kind=ai the server assigns a random name
	// and assembles the full ID (e.g. alice:opus4.7-claude-code@aimebu). For
	// kind=human the caller provides an explicit name (e.g. martin) which
	// becomes the ID verbatim.
	mux.HandleFunc("POST /agents", func(w http.ResponseWriter, r *http.Request) {
		var req types.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		kind := req.Kind
		if kind == "" {
			kind = "ai"
		}

		var agent *types.Agent
		var reclaimed bool
		var err error
		switch kind {
		case "ai":
			// Default protocol to "mcp" for AI agents that don't set it.
			// The aimebu agent wrapper sets protocol="agent" via meta + env var.
			if req.Meta == nil {
				req.Meta = map[string]string{}
			}
			if req.Meta["protocol"] == "" {
				req.Meta["protocol"] = "mcp"
			}
			forceName := ""
			if req.Force {
				forceName = req.Name
			}
			agent, reclaimed, err = s.registerAI(req.Model, req.Harness, req.Project, req.Meta, forceName)
		case "human":
			agent, err = s.registerHuman(req.Name, req.Project, req.Meta)
		default:
			jsonError(w, "invalid kind (must be 'ai' or 'human')", http.StatusBadRequest)
			return
		}
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}

		var warnings []string
		if agent.Kind == "ai" && s.roleKeyExists(agent.Name) {
			warnings = append(warnings, roleNameCollisionWarning(agent.Name))
		}

		_ = jsonOK(w, types.RegisterResponse{
			ID:        agent.ID,
			Name:      agent.Name,
			Kind:      agent.Kind,
			Model:     agent.Model,
			Harness:   agent.Harness,
			Project:   agent.Project,
			Meta:      agent.Meta,
			Reclaimed: reclaimed,
			Warnings:  warnings,
		})
	})

	// GET /agents — list
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		agents := s.listAgents()
		_ = jsonOK(w, map[string]any{"agents": agents})
	})

	// DELETE /agents/{id} — forced deregistration. Removes the agent from the
	// registry and all room memberships, then broadcasts updated room/agent
	// state. Used by aimebu agent for fast Ctrl-C teardown.
	mux.HandleFunc("DELETE /agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if !s.deregisterAgent(agentID) {
			jsonError(w, "agent not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /agents/{id}/state — wrapper-pushed live activity state.
	mux.HandleFunc("POST /agents/{id}/state", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		var req struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !isValidAgentState(req.State) {
			jsonError(w, "unknown state: "+req.State, http.StatusBadRequest)
			return
		}
		if !s.setAgentState(agentID, req.State) {
			jsonError(w, "agent not found", http.StatusNotFound)
			return
		}
		_ = jsonOK(w, map[string]any{"agent": agentID, "state": req.State})
	})

	// GET /agents/{id}/rooms — rooms an agent is in, with per-agent unread
	// counts and read cursor. Returns AgentRoomView (Room + unread_count +
	// last_id + read_cursor) per room.
	mux.HandleFunc("GET /agents/{id}/rooms", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		rooms := s.agentRoomViews(agentID)
		_ = jsonOK(w, map[string]any{"rooms": rooms, "agent": agentID})
	})

	// POST /agents/{id}/read — explicit mark-read. Body: {room, message_id}.
	// message_id=0 means "current HEAD of the room" (useful for the UI when
	// the user opens a room). Advances the cursor only forward; older values
	// are a no-op. Broadcasts a read_update meta event on change.
	mux.HandleFunc("POST /agents/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		var req types.MarkReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Room == "" {
			jsonError(w, "room is required", http.StatusBadRequest)
			return
		}
		target := req.MessageID
		if target == 0 {
			target = s.roomHead(req.Room)
		}
		if s.advanceCursor(agentID, req.Room, target) {
			s.broadcastReadUpdate(agentID, req.Room, target)
		}
		_ = jsonOK(w, map[string]any{"agent": agentID, "room": req.Room, "read_cursor": target})
	})

	// ── Global ─────────────────────────────────────────────────────

	// GET /firehose — global SSE (all rooms)
	mux.HandleFunc("GET /firehose", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch := s.subscribeGlobal()
		defer s.unsubscribeGlobal(ch)

		for {
			select {
			case msg := <-ch:
				data, _ := json.Marshal(msg)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// GET /messages/{id} — fetch a single message by its global ID.
	// When agent_id is provided, the response includes viewer-annotated
	// fields for that registered agent's POV, even if the agent is not a
	// member of the room. This keeps the per-message debug inspector aligned
	// with the bus-wide agent picker in the web UI.
	mux.HandleFunc("GET /messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			jsonError(w, "not_found", http.StatusNotFound)
			return
		}
		msg, ok := s.messageByID(id)
		if !ok {
			jsonError(w, "not_found", http.StatusNotFound)
			return
		}
		agentID := r.URL.Query().Get("agent_id")
		if agentID != "" && !s.hasAgent(agentID) {
			jsonError(w, "not_found", http.StatusNotFound)
			return
		}
		annotated := annotate([]types.Message{msg}, agentID, s.addressingContext)
		if len(annotated) == 1 {
			_ = jsonOK(w, annotated[0])
			return
		}
		_ = jsonOK(w, msg)
	})

	// GET /messages — all messages (for sniff/monitoring)
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if l := r.URL.Query().Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		msgs := s.allMessages(limit)
		_ = jsonOK(w, map[string]any{"messages": msgs})
	})

	// ── Macros ─────────────────────────────────────────────────────

	// GET /macros — fetch global macros
	mux.HandleFunc("GET /macros", func(w http.ResponseWriter, _ *http.Request) {
		env := s.getEnvelope()
		_ = jsonOK(w, map[string]any{"macros": env.Macros})
	})

	// PUT /macros — full replace of global macros.
	mux.HandleFunc("PUT /macros", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Macros map[string]string            `json:"macros"`
			Rooms  map[string]map[string]string `json:"rooms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(payload.Rooms) > 0 {
			jsonError(w, "per-room macros are not supported; use global macros only", http.StatusBadRequest)
			return
		}
		if len(payload.Macros) > 256 {
			jsonError(w, "too many global macros (max 256)", http.StatusBadRequest)
			return
		}
		for k, v := range payload.Macros {
			if !macroKeyRE.MatchString(k) {
				jsonError(w, "invalid macro key: "+k, http.StatusBadRequest)
				return
			}
			if len(v) > 16*1024 {
				jsonError(w, "macro body too large (max 16KB): "+k, http.StatusBadRequest)
				return
			}
		}
		s.setEnvelope(macrosEnvelope{Macros: payload.Macros})
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// ── Fleets ─────────────────────────────────────────────────────

	// GET /fleets — fetch all configured fleets.
	mux.HandleFunc("GET /fleets", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, s.listFleets())
	})

	// PUT /fleets — full replace of all fleets.
	mux.HandleFunc("PUT /fleets", func(w http.ResponseWriter, r *http.Request) {
		var payload fleetsEnvelope
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.replaceFleets(payload); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// GET /fleets/{name} — fetch one fleet.
	mux.HandleFunc("GET /fleets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		fleet, ok := s.getFleet(name)
		if !ok {
			jsonError(w, "fleet not found", http.StatusNotFound)
			return
		}
		_ = jsonOK(w, map[string]any{"name": name, "fleet": fleet})
	})

	// PUT /fleets/{name} — upsert one fleet.
	mux.HandleFunc("PUT /fleets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var payload Fleet
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.setFleet(name, payload); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /fleets/{name} — remove one fleet.
	mux.HandleFunc("DELETE /fleets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.deleteFleet(r.PathValue("name")) {
			jsonError(w, "fleet not found", http.StatusNotFound)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /fleets — clear all fleets.
	mux.HandleFunc("DELETE /fleets", func(w http.ResponseWriter, _ *http.Request) {
		s.clearFleets()
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// GET /fleets/{name}/export — download one fleet in the importable envelope.
	mux.HandleFunc("GET /fleets/{name}/export", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		fleet, ok := s.getFleet(name)
		if !ok {
			jsonError(w, "fleet not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="aimebu-fleet-%s.json"`, name))
		_ = jsonOK(w, fleetsEnvelope{Version: 1, Fleets: map[string]Fleet{name: fleet}})
	})

	// POST /fleets/import — merge an importable fleet envelope.
	mux.HandleFunc("POST /fleets/import", func(w http.ResponseWriter, r *http.Request) {
		var payload fleetsEnvelope
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		imported, err := s.importFleets(payload)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = jsonOK(w, imported)
	})

	// GET /settings — fetch user preferences
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, s.getSettings())
	})

	// PUT /settings — update user preferences
	mux.HandleFunc("PUT /settings", func(w http.ResponseWriter, r *http.Request) {
		var set Settings
		if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !validThemes[set.Theme] {
			jsonError(w, `invalid theme: must be "dark", "light", or ""`, http.StatusBadRequest)
			return
		}
		if err := validateRetentionSettings(set); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if set.NotificationVolume != nil {
			v := *set.NotificationVolume
			if v < 0 {
				v = 0
			} else if v > 100 {
				v = 100
			}
			set.NotificationVolume = &v
		}
		oldCleanupInterval := s.cleanupInterval()
		s.putSettings(set)
		if s.cleanupInterval() != oldCleanupInterval {
			s.requestCleanupReset()
		}
		_ = jsonOK(w, s.getSettings())
	})

	// ── Prompts ─────────────────────────────────────────────────────

	// GET /settings/prompts — list all prompts with effective bodies + metadata
	mux.HandleFunc("GET /settings/prompts", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, s.listPrompts())
	})

	// PUT /settings/prompts/{key} — set a user override for a prompt
	mux.HandleFunc("PUT /settings/prompts/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		var payload struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !s.setPrompt(key, payload.Value) {
			jsonError(w, "unknown prompt key", http.StatusNotFound)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /settings/prompts/{key} — revert a single prompt to its compiled default
	mux.HandleFunc("DELETE /settings/prompts/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if !promptCatalogSet[key] {
			jsonError(w, "unknown prompt key", http.StatusNotFound)
			return
		}
		s.deletePrompt(key)
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /settings/prompts — revert all prompts to compiled defaults
	mux.HandleFunc("DELETE /settings/prompts", func(w http.ResponseWriter, _ *http.Request) {
		s.deleteAllPrompts()
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// ── Roles ─────────────────────────────────────────────────────────

	// GET /roles — list all roles with effective bodies + metadata
	mux.HandleFunc("GET /roles", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, s.listRoles())
	})

	// PUT /roles — full-replace. Catalog keys take a string body or a structured
	// {"description","emoji","body","cardinality","extends"} object. Custom keys
	// accept the same structured object. Atomically replaces all overrides and
	// custom roles; validates everything before applying any change.
	mux.HandleFunc("PUT /roles", func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Roles map[string]json.RawMessage `json:"roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(raw.Roles) > 64 {
			jsonError(w, "too many roles (max 64)", http.StatusBadRequest)
			return
		}
		// Validate all entries upfront
		entries := make([]replaceRolesEntry, 0, len(raw.Roles))
		for k, v := range raw.Roles {
			if !roleKeyRE.MatchString(k) {
				jsonError(w, "invalid role key: "+k, http.StatusBadRequest)
				return
			}
			var entry replaceRolesEntry
			entry.key = k
			entry.isCustom = !roleCatalogSet[k]
			// Try string first, then structured object
			var bodyStr string
			if err := json.Unmarshal(v, &bodyStr); err == nil {
				entry.body = bodyStr
				if catalogEntry, ok := roleCatalogEntryFor(k); ok {
					entry.description = catalogEntry.Description
					if catalogEntry.Emoji != "" {
						entry.emoji = catalogEntry.Emoji
					} else {
						entry.emoji = catalogEntry.Icon
					}
					entry.cardinality = catalogEntry.Cardinality
					entry.extends = catalogEntry.Extends
				}
			} else {
				var obj struct {
					Label       string `json:"label"`
					Description string `json:"description"`
					Emoji       string `json:"emoji"`
					Icon        string `json:"icon"`
					Body        string `json:"body"`
					Cardinality string `json:"cardinality"`
					Extends     string `json:"extends"`
				}
				if err := json.Unmarshal(v, &obj); err != nil {
					jsonError(w, "invalid value for key "+k+": must be a string body or {description,emoji,body,cardinality,extends}", http.StatusBadRequest)
					return
				}
				entry.body = obj.Body
				entry.description = obj.Description
				if obj.Emoji != "" {
					entry.emoji = obj.Emoji
				} else {
					entry.emoji = obj.Icon
				}
				entry.cardinality = obj.Cardinality
				entry.extends = obj.Extends
				if !entry.isCustom {
					if catalogEntry, ok := roleCatalogEntryFor(k); ok {
						if entry.emoji == "" {
							if catalogEntry.Emoji != "" {
								entry.emoji = catalogEntry.Emoji
							} else {
								entry.emoji = catalogEntry.Icon
							}
						}
						if entry.cardinality == "" {
							entry.cardinality = catalogEntry.Cardinality
						}
					}
				}
			}
			if len(entry.body) > 16*1024 {
				jsonError(w, "role body too large (max 16KB): "+k, http.StatusBadRequest)
				return
			}
			icon, err := normalizeRoleEmoji(entry.emoji)
			if err != nil {
				jsonError(w, "invalid role emoji for key "+k+": "+err.Error(), http.StatusBadRequest)
				return
			}
			entry.emoji = icon
			cardinality, err := normalizeRoleCardinality(entry.cardinality)
			if err != nil {
				jsonError(w, "invalid role cardinality for key "+k+": "+err.Error(), http.StatusBadRequest)
				return
			}
			entry.cardinality = cardinality
			if entry.extends != "" && !roleKeyRE.MatchString(entry.extends) {
				jsonError(w, "invalid role extends for key "+k, http.StatusBadRequest)
				return
			}
			entries = append(entries, entry)
		}
		// Apply atomically
		force := r.URL.Query().Get("force") == "true"
		if err := s.replaceRoles(entries, force); err != nil {
			if errors.Is(err, ErrRolesConflict) {
				jsonError(w, err.Error(), http.StatusConflict)
			} else {
				jsonError(w, err.Error(), http.StatusBadRequest)
			}
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /roles/{key} — revert a catalog override or delete a custom role
	mux.HandleFunc("DELETE /roles/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		force := r.URL.Query().Get("force") == "true"
		if !s.roleExists(key) {
			jsonError(w, "unknown role key", http.StatusNotFound)
			return
		}
		if err := s.deleteRoleOverride(key, force); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /roles — clear all role overrides and custom roles.
	// Respects ?force=true to cascade-unassign; without it returns 409 if any
	// room has role assignments.
	mux.HandleFunc("DELETE /roles", func(w http.ResponseWriter, r *http.Request) {
		force := r.URL.Query().Get("force") == "true"
		if err := s.deleteAllRoleOverrides(force); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// POST /rooms/{id}/roles — assign or unassign a role for an AI agent.
	// Returns the updated room so callers can re-render without a second fetch.
	mux.HandleFunc("POST /rooms/{id}/roles", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("id")
		var req struct {
			AgentID string `json:"agent_id"`
			RoleKey string `json:"role_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.AgentID == "" {
			jsonError(w, "agent_id is required", http.StatusBadRequest)
			return
		}
		if req.RoleKey != "" && !s.roleExists(req.RoleKey) {
			jsonError(w, "unknown role key: "+req.RoleKey, http.StatusBadRequest)
			return
		}
		if err := s.assignRole(roomID, req.AgentID, req.RoleKey); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrRoomNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, ErrRoleAssignmentConflict) {
				status = http.StatusConflict
			}
			jsonError(w, err.Error(), status)
			return
		}
		room := s.getRoom(roomID)
		_ = jsonOK(w, room)
	})

	// GET /rooms/{id}/roles/{agentID} — get the role for a specific agent in a room
	mux.HandleFunc("GET /rooms/{id}/roles/{agentID}", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("id")
		agentID := r.PathValue("agentID")
		room := s.getRoom(roomID)
		if room == nil {
			jsonError(w, "room not found", http.StatusNotFound)
			return
		}
		roleKey := room.Roles[agentID]
		resp := map[string]any{
			"room":     roomID,
			"agent_id": agentID,
			"role_key": roleKey,
			"label":    "",
			"icon":     "",
			"emoji":    "",
			"body":     "",
		}
		if roleKey != "" {
			body, _ := s.getRole(roleKey)
			resp["body"] = body
			label := s.getRoleLabel(roleKey)
			resp["label"] = label
			emoji := s.getRoleIcon(roleKey)
			resp["icon"] = emoji
			resp["emoji"] = emoji
		}
		_ = jsonOK(w, resp)
	})

	// DELETE /all — clear conversation state; ?include_settings=true also wipes user settings.
	mux.HandleFunc("DELETE /all", func(w http.ResponseWriter, r *http.Request) {
		includeSettings := r.URL.Query().Get("include_settings") == "true"
		s.clearAll(includeSettings)
		_ = jsonOK(w, map[string]string{"status": "cleared"})
	})

	// ── Image attachments ───────────────────────────────────────────

	mux.HandleFunc("POST /api/attachments", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentFileSize+4096)
		if err := r.ParseMultipartForm(maxAttachmentFileSize + 4096); err != nil {
			jsonError(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "missing 'file' field", http.StatusBadRequest)
			return
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxAttachmentFileSize+1))
		if err != nil {
			jsonError(w, "read error: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(data) > maxAttachmentFileSize {
			jsonError(w, fmt.Sprintf("file too large (max %d MB)", maxAttachmentFileSize/1024/1024), http.StatusRequestEntityTooLarge)
			return
		}
		sniffLen := min(len(data), 512)
		mimeType := http.DetectContentType(data[:sniffLen])
		if !allowedAttachmentMime(mimeType) {
			jsonError(w, "only png, jpeg, gif, and webp images are accepted", http.StatusBadRequest)
			return
		}
		width, height, err := attachmentDimensions(mimeType, data)
		if err != nil {
			jsonError(w, "image dimensions could not be decoded", http.StatusBadRequest)
			return
		}
		name := sanitizeAttachmentName(hdr.Filename, attachmentExt(mimeType))
		attachment, err := s.addAttachment(name, mimeType, data, width, height)
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(attachment)
	})

	mux.HandleFunc("DELETE /api/attachments/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		found, deleted := s.deleteAttachment(uuid)
		if !found {
			jsonError(w, "attachment not found", http.StatusNotFound)
			return
		}
		if !deleted {
			jsonError(w, "attachment is referenced by a message", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/attachments/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		if !validUUID(uuid) {
			jsonError(w, "attachment not found", http.StatusNotFound)
			return
		}
		entry, path, ok := s.attachmentFilePath(uuid)
		if !ok {
			jsonError(w, "attachment not found", http.StatusNotFound)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			jsonError(w, "attachment not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", entry.Mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, strings.ReplaceAll(entry.Name, `"`, "")))
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(data)
	})

	// ── User notification sounds ────────────────────────────────────

	// GET /api/sounds — list bundled (built-in) + user-uploaded sounds.
	mux.HandleFunc("GET /api/sounds", func(w http.ResponseWriter, _ *http.Request) {
		builtins := []map[string]string{
			{"id": "builtin:chime", "name": "Chime", "kind": "builtin"},
			{"id": "builtin:ding", "name": "Ding", "kind": "builtin"},
			{"id": "builtin:beep", "name": "Beep", "kind": "builtin"},
			{"id": "builtin:knock", "name": "Knock", "kind": "builtin"},
		}
		userSounds := s.listSounds()
		var all []any
		for _, b := range builtins {
			all = append(all, b)
		}
		for _, u := range userSounds {
			uExt := u.Ext
			if uExt == "" {
				uExt = "mp3"
			}
			all = append(all, map[string]any{
				"id":          "user:" + u.UUID,
				"name":        u.Name,
				"kind":        "user",
				"size":        u.Size,
				"uploaded_at": u.UploadedAt,
				"ext":         uExt,
			})
		}
		_ = jsonOK(w, map[string]any{"sounds": all})
	})

	// POST /api/sounds — upload a custom notification sound (.mp3, max 1 MB).
	// Multipart form field: "file" (the MP3). Returns the new sound entry.
	mux.HandleFunc("POST /api/sounds", func(w http.ResponseWriter, r *http.Request) {
		// Limit total body to slightly over 1 MB so we can give a clean error
		// before reading the whole stream.
		r.Body = http.MaxBytesReader(w, r.Body, maxSoundFileSize+4096)
		if err := r.ParseMultipartForm(maxSoundFileSize + 4096); err != nil {
			jsonError(w, "upload too large or malformed: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "missing 'file' field", http.StatusBadRequest)
			return
		}
		defer f.Close()

		// Size guard (second line of defence after MaxBytesReader).
		if hdr.Size > maxSoundFileSize {
			jsonError(w, fmt.Sprintf("file too large (max %d KB)", maxSoundFileSize/1024), http.StatusRequestEntityTooLarge)
			return
		}

		// Extension check.
		lowerName := strings.ToLower(hdr.Filename)
		isWAV := strings.HasSuffix(lowerName, ".wav")
		isMP3 := strings.HasSuffix(lowerName, ".mp3")
		if !isMP3 && !isWAV {
			jsonError(w, "only .mp3 / .wav files are accepted", http.StatusBadRequest)
			return
		}

		// Read file data.
		data := make([]byte, hdr.Size)
		if _, err := io.ReadFull(f, data); err != nil {
			jsonError(w, "read error: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Validate file header matches declared extension.
		if isWAV {
			if !validWAVHeader(data) {
				jsonError(w, "file does not appear to be a valid WAV", http.StatusBadRequest)
				return
			}
		} else {
			if !validMP3Header(data) {
				jsonError(w, "file does not appear to be a valid MP3", http.StatusBadRequest)
				return
			}
		}

		ext := "mp3"
		if isWAV {
			ext = "wav"
		}

		// Sanitize display name: strip directory components, keep base name only.
		displayName := filepath.Base(hdr.Filename)
		if displayName == "." || displayName == "" {
			displayName = "custom." + ext
		}
		// Truncate long names by rune count to avoid splitting multibyte sequences.
		if utf8.RuneCountInString(displayName) > 64 {
			displayName = string([]rune(displayName)[:64])
		}

		entry, err := s.addSound(displayName, data, ext)
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = jsonOK(w, map[string]any{
			"id":          "user:" + entry.UUID,
			"name":        entry.Name,
			"kind":        "user",
			"size":        entry.Size,
			"uploaded_at": entry.UploadedAt,
			"ext":         entry.Ext,
		})
	})

	// DELETE /api/sounds/{uuid} — delete a user-uploaded sound by UUID.
	mux.HandleFunc("DELETE /api/sounds/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		if !s.deleteSound(uuid) {
			jsonError(w, "sound not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /api/sounds/{uuid} — serve a user-uploaded sound file.
	mux.HandleFunc("GET /api/sounds/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		uuid := r.PathValue("uuid")
		path, ext := s.soundFilePath(uuid)
		if path == "" {
			jsonError(w, "sound not found", http.StatusNotFound)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			jsonError(w, "sound not found", http.StatusNotFound)
			return
		}
		contentType := "audio/mpeg"
		if ext == "wav" {
			contentType = "audio/wav"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		_, _ = w.Write(data)
	})

	// GET /health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, map[string]string{"status": "ok"})
	})

	// GET /buildinfo — server version and Go runtime version
	mux.HandleFunc("GET /buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, build)
	})

	// GET /ws — WebSocket endpoint for real-time push
	mux.HandleFunc("GET /ws", handleWS(s))

	if usageManager != nil {
		usages.Routes{Manager: usageManager}.Mount(mux)
	}
}

func defaultRoleBodies() map[string]string {
	return map[string]string{
		"leader": `You are the leader for this room.

Your job is to keep the collaboration process orderly and decision-ready. For new tasks, first frame the problem by describing symptoms, gaps, scope hints, and relevant files without proposing solutions, numbered options, preferred directions, or "I'm leaning" language. After that framing kickoff, post your own independent initial plan. Do not read the other roles' plans before posting yours. If you arrive late and another plan is already on the bus, post yours anyway without editing toward convergence. Wait for the initial plans of all others in room to be posted before discussion starts. Always wait for all other agents to finish their current response before sending your next message during planning and code review. Do not start a new round while another agent is still mid-message.

During discussion, drive the team toward a concrete plan. If no consensus emerges after five rounds, summarize each position for the human. Once a plan is finalized, hand it to the human for approval with needs_attention=true, explicitly noting how the finalized plan diverged from each of the three initial plans so the human can audit the consolidation.

After approval, hand implementation to the appropriate implementer role or agent. During code review, post your review independently of any assigned review roles, then consolidate all reviews into one prioritized fix list. If reviewers disagree, surface the tie to the human.

Keep history clean: one commit per feature unless the human approves otherwise. Push only when explicitly instructed by the human. After sign-off, hand the final status to the human with needs_attention=true when their next action is required.

Do not casually defer scope to 'v2', 'v3', 'follow-up', or 'out of scope'. Deferral is only legitimate when (a) the work needs a new dependency that requires human approval, (b) it requires information or research the team does not currently have, or (c) it would inflate the diff by more than roughly 30 percent beyond the original intent. 'Risk' or 'complexity' alone is not a reason — spell out the specific risk and its mitigation; if the mitigation is straightforward, just do it. Default for borderline items is 'include now', not 'defer'.

For multi-question choice asks directed to a human, attach an open_questions array to bus_say or bus_dm instead of writing Q1/Q2 option blocks in prose. Include question text, 2-8 option strings, and optional description text when the question needs context; the UI derives numbering/letters and adds Other.

Set needs_attention=true only when a message asks the human for a blocking decision, approval, review, or next action — i.e. progress stalls until the human responds. For those human-blocking decision asks, include 2-4 short proposed_answers such as "Proceed", "Revise: ...", or "Hold" when using bus_say or bus_dm. Do not set needs_attention for status updates, acknowledgements, or information-only replies.`,

		"worker": `You are the worker for this room.

After the room leader's framing kickoff, post your own independent initial plan before implementation is approved. Do not read the other roles' plans before posting yours. If you arrive late and another plan is already on the bus, post yours anyway without editing toward convergence. Wait for the initial plans of all others in room to be posted before discussion starts. Always wait for all other agents to finish their current response before sending your next message during planning and code review. Do not start a new round while another agent is still mid-message.

Your job is to turn the approved plan into correct, working code. Do not start implementation until the room leader posts the human-approved final plan and explicitly hands implementation to you. Keep edits focused on that plan and the supporting code needed to make it work.

During multi-step implementation, stay reachable with quick non-blocking bus_read checkpoints at natural breakpoints: after each meaningful edit or implementation step, before starting any command you expect to run longer than roughly one minute, and after that command returns. If a checkpoint shows that the human or room leader directly addressed you, sent needs_attention, or asked something clearly blocking your current work, answer concisely and then resume. Otherwise keep working without posting status acknowledgements. For long validation, prefer smaller commands or backgrounding where practical so you can checkpoint between them; a single foreground command can still be a temporary blind spot.

If implementation reveals surprises, fallbacks, or necessary deviations, report them as implementation notes, not review findings. When ready for review, report the exact branch/commit state, tests run, and any implementation notes, then ping the room leader and others for review. Do not self-review.

While reviews are pending, you may answer factual clarification questions. Wait until all independent reviews are posted before applying fixes. Apply the consolidated fix list from the room leader. If a feature commit already exists, amend it so the branch keeps one clean feature commit. Push only when explicitly instructed by the human or room leader.

Do not casually defer scope to 'v2', 'v3', 'follow-up', or 'out of scope'. Deferral is only legitimate when (a) the work needs a new dependency that requires human approval, (b) it requires information or research the team does not currently have, or (c) it would inflate the diff by more than roughly 30 percent beyond the original intent. 'Risk' or 'complexity' alone is not a reason — spell out the specific risk and its mitigation; if the mitigation is straightforward, just do it. Default for borderline items is 'include now', not 'defer'.

For multi-question choice asks directed to a human, attach an open_questions array to bus_say or bus_dm instead of writing Q1/Q2 option blocks in prose. Include question text, 2-8 option strings, and optional description text when the question needs context; the UI derives numbering/letters and adds Other.

Set needs_attention=true only when a message asks the human for a blocking decision, approval, review, or next action — i.e. progress stalls until the human responds. For those human-blocking decision asks, include 2-4 short proposed_answers such as "Proceed", "Revise: ...", or "Hold" when using bus_say or bus_dm. Do not set needs_attention for status updates, acknowledgements, or information-only replies.`,

		"reviewer": `You are the reviewer for this room.

After the room leader's framing kickoff, post your own independent initial plan before review-only duties begin. Do not read the other roles' plans before posting yours. If you arrive late and another plan is already on the bus, post yours anyway without editing toward convergence. Wait for the initial plans of all others in room to be posted before discussion starts. Always wait for all other agents to finish their current response before sending your next message during planning and code review. Do not start a new round while another agent is still mid-message.

Your job is to provide an independent assessment of correctness and risk. Review only after an implementer asks for code review, and do not read other reviews before posting your own. Code review is performed by review roles and coordination roles; implementers do not self-review.

Lead with actionable findings, ordered by severity, with concrete file/line references or behavior descriptions. Focus on bugs, regressions, missing tests, docs drift, integration risks, and divergence from the approved plan. Distinguish blockers from non-blocking concerns. If no blockers remain, say that clearly and note any residual risk.

During long review passes, stay reachable with quick non-blocking bus_read checkpoints at natural breaks. Answer concise direct human, leader, or needs_attention messages, then resume review; ignore non-blocking chatter until your review is posted.

After fixes, re-review until the consolidated fix list is addressed without introducing unrelated changes.

Do not casually defer scope to 'v2', 'v3', 'follow-up', or 'out of scope'. Deferral is only legitimate when (a) the work needs a new dependency that requires human approval, (b) it requires information or research the team does not currently have, or (c) it would inflate the diff by more than roughly 30 percent beyond the original intent. 'Risk' or 'complexity' alone is not a reason — spell out the specific risk and its mitigation; if the mitigation is straightforward, just do it. Default for borderline items is 'include now', not 'defer'.

For multi-question choice asks directed to a human, attach an open_questions array to bus_say or bus_dm instead of writing Q1/Q2 option blocks in prose. Include question text, 2-8 option strings, and optional description text when the question needs context; the UI derives numbering/letters and adds Other.

Set needs_attention=true only when a message asks the human for a blocking decision, approval, review, or next action — i.e. progress stalls until the human responds. For those human-blocking decision asks, include 2-4 short proposed_answers such as "Proceed", "Revise: ...", or "Hold" when using bus_say or bus_dm. Do not set needs_attention for status updates, acknowledgements, or information-only replies.`,

		"sec-reviewer": `Additional focus: security.

Look specifically for authorization bypasses, privilege boundary mistakes, injection or parsing hazards, unsafe file/network access, secret exposure, unsafe defaults, and abuse cases. Keep the review tied to exploitable or realistically risky behavior, and distinguish concrete security findings from general hardening ideas.`,

		"test-reviewer": `Additional focus: tests and verification.

Look specifically for missing regression coverage, weak assertions, untested edge cases, flaky or slow test design, fixture drift, and gaps between the claimed verification and the behavior changed. Recommend focused tests that would catch the failure mode rather than broad coverage for its own sake.`,

		"ux-reviewer": `Additional focus: user experience.

Look specifically at user-facing flows, accessibility, responsive layout, visual hierarchy, copy clarity, error states, and interaction feedback. Call out confusing or cramped UI and behavior that makes the user's next action unclear.`,
	}
}

// Run starts the HTTP server in the foreground with graceful shutdown.
// promptDefaults is the compiled-in default body for each prompt key; pass nil
// to use empty defaults (useful in tests).
func Run(addr, rootDir string, frontendFS fs.FS, promptDefaults map[string]string, build BuildInfo) error {
	if err := validateBindAddr(addr); err != nil {
		return err
	}
	if err := config.MigrateServer(rootDir); err != nil {
		return fmt.Errorf("migrate server config: %w", err)
	}
	dataDir := filepath.Join(rootDir, "server")
	allow, err := resolveAllow()
	if err != nil {
		return err
	}

	s, err := newStore(dataDir)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	// Register compiled-in prompt defaults so the store can serve them.
	if promptDefaults != nil {
		SetPromptDefaults(promptDefaults)
	}

	// Register compiled-in role defaults; these bodies are the canonical
	// room-collaboration protocol returned by bus_role_get unless overridden.
	SetRoleDefaults(defaultRoleBodies())

	// Migrate uncustomized stale defaults before the write-once default seeding.
	s.migrateStaleDefaultMacros()

	// Merge bundled defaults into the global macro map (write-once per key).
	s.applyDefaultMacros()

	// Start background cleanup using settings-backed retention windows.
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	s.startCleanup(cleanupCtx)
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()

	mux := http.NewServeMux()
	usageManager := usages.NewManager(usages.NewStoreAt(filepath.Join(rootDir, "usages")), usages.DefaultRegistry())
	usageManager.SetUpdateHook(func(resp usages.Response) {
		s.broadcastMeta(MetaEvent{Type: "usages_updated", Data: resp})
	})
	usageManager.Start(cleanupCtx)
	setupHandlers(mux, s, build, usageManager)

	// Serve embedded frontend at /
	if frontendFS != nil {
		registerStaticMimeTypes()
		mux.Handle("GET /", frontendFileServer(frontendFS))
	}

	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		mux.ServeHTTP(w, r)
	})
	handler = allowMiddleware(handler, allow)

	httpSrv := &http.Server{
		Addr:        addr,
		Handler:     handler,
		BaseContext: serverBaseContext(serverCtx),
	}
	tlsConfig, err := resolveServerTLSConfig()
	if err != nil {
		return err
	}
	servers := []serverListener{
		{srv: httpSrv, scheme: "http"},
	}
	if tlsConfig.Enabled {
		tlsPort, err := resolveServerTLSPort()
		if err != nil {
			return err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("split listen address %q: %w", addr, err)
		}
		servers = append(servers, serverListener{
			srv: &http.Server{
				Addr:        net.JoinHostPort(host, tlsPort),
				Handler:     handler,
				BaseContext: serverBaseContext(serverCtx),
			},
			scheme:   "https",
			certFile: tlsConfig.CertFile,
			keyFile:  tlsConfig.KeyFile,
		})
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-done
		log.Println("Shutting down...")
		// best-effort: won't fire on SIGKILL or crash
		s.emitSystemMessage("_system", "server stopping")
		serverCancel()
		shutdownServers(servers)
	}()

	log.Printf("aimebu listening on %s (allow: %v, data: %s)", listenerSummary(servers), allow, dataDir)
	host, _, _ := net.SplitHostPort(addr)
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsUnspecified() {
			log.Printf("WARN: bound to %s (all interfaces) — AIMEBU_ALLOW gates by source IP", host)
		} else if !ip.IsLoopback() {
			log.Printf("WARN: bound to %s — anyone reachable on this interface can attempt to call /; AIMEBU_ALLOW gates by source IP", host)
		}
	}
	s.emitSystemMessage("_system", "server started")

	errCh := make(chan error, len(servers))
	var wg sync.WaitGroup
	for _, listener := range servers {
		listener := listener
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- listener.serve()
		}()
	}
	go func() {
		wg.Wait()
		close(errCh)
	}()

	var firstErr error
	for err := range errCh {
		if err == nil || err == http.ErrServerClosed {
			continue
		}
		if firstErr == nil {
			firstErr = err
			serverCancel()
			shutdownServers(servers)
		}
	}
	return firstErr
}

func serverBaseContext(ctx context.Context) func(net.Listener) context.Context {
	return func(_ net.Listener) context.Context {
		return ctx
	}
}

// DefaultAddr returns the listen address from env vars.
func DefaultAddr() string {
	port := os.Getenv("AIMEBU_PORT")
	if port == "" {
		port = "9997"
	}
	bind := os.Getenv("AIMEBU_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	return bind + ":" + port
}

type serverListener struct {
	srv      *http.Server
	scheme   string
	certFile string
	keyFile  string
}

func (l serverListener) serve() error {
	if l.scheme == "https" {
		return l.srv.ListenAndServeTLS(l.certFile, l.keyFile)
	}
	return l.srv.ListenAndServe()
}

func shutdownServers(listeners []serverListener) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, listener := range listeners {
		_ = listener.srv.Shutdown(ctx)
	}
}

func listenerSummary(listeners []serverListener) string {
	parts := make([]string, 0, len(listeners))
	for _, listener := range listeners {
		parts = append(parts, listener.scheme+"://"+listener.srv.Addr)
	}
	return strings.Join(parts, " + ")
}
