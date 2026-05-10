package server

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

var macroKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

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

func setupHandlers(mux *http.ServeMux, s *store) {

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
		if err := s.leaveRoom(roomID, req.AgentID); err != nil {
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
		if req.From == "" || req.Body == "" {
			jsonError(w, "from and body are required", http.StatusBadRequest)
			return
		}
		if req.From == "_system" || strings.HasPrefix(roomID, "_") {
			jsonError(w, "cannot send to reserved room or use reserved sender", http.StatusForbidden)
			return
		}
		id, err := s.roomSend(roomID, req.From, req.Body, req.NeedsAttention)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		resp := map[string]any{"id": id, "room": roomID}
		var warnings []string
		if warn := s.legacyPrefixWarn(req.From, req.Body); warn != "" {
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
			_ = jsonOK(w, map[string]any{"messages": annotate(msgs, agentShortName(agentID), s.addressingContext), "room": roomID})
		} else {
			_ = jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
		}
	})

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
				if err := jsonOK(w, map[string]any{"messages": annotate(filtered, agentShortName(agentID), s.addressingContext), "room": roomID}); err == nil {
					if s.advanceCursor(agentID, roomID, last) {
						s.broadcastReadUpdate(agentID, roomID, last)
					}
				}
			} else {
				_ = jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
			}
			return
		}

		// Mark the agent as actively waiting on this room so the UI can
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
					if err := jsonOK(w, map[string]any{"messages": annotate([]types.Message{msg}, agentShortName(agentID), s.addressingContext), "room": roomID}); err == nil {
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
			if err := jsonOK(w, map[string]any{"messages": annotate(filtered, agentShortName(agentID), s.addressingContext), "agent": agentID}); err == nil {
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
				if err := jsonOK(w, map[string]any{"messages": annotate([]types.Message{msg}, agentShortName(agentID), s.addressingContext), "agent": agentID}); err == nil {
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

	// POST /dm — send a DM (auto-creates private room)
	mux.HandleFunc("POST /dm", func(w http.ResponseWriter, r *http.Request) {
		var req types.DMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.From == "" || req.To == "" || req.Body == "" {
			jsonError(w, "from, to, and body are required", http.StatusBadRequest)
			return
		}

		room, err := s.findOrCreateDM(req.From, req.To)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}

		id, err := s.roomSend(room.ID, req.From, req.Body, req.NeedsAttention)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		resp := map[string]any{"id": id, "room": room.ID}
		var warnings []string
		if warn := s.legacyPrefixWarn(req.From, req.Body); warn != "" {
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

		_ = jsonOK(w, types.RegisterResponse{
			ID:        agent.ID,
			Name:      agent.Name,
			Kind:      agent.Kind,
			Model:     agent.Model,
			Harness:   agent.Harness,
			Project:   agent.Project,
			Meta:      agent.Meta,
			Reclaimed: reclaimed,
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
	// Requires the caller to be a member of the message's room; returns a
	// uniform {error:"not_found"} for both missing messages and non-members
	// so sequential IDs can't be enumerated to dump private rooms.
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
		if !s.isMember(msg.RoomID, agentID) {
			jsonError(w, "not_found", http.StatusNotFound)
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
		if set.NotificationVolume != nil {
			v := *set.NotificationVolume
			if v < 0 {
				v = 0
			} else if v > 100 {
				v = 100
			}
			set.NotificationVolume = &v
		}
		s.putSettings(set)
		_ = jsonOK(w, s.getSettings())
	})

	// DELETE /all — clear conversation state; ?include_settings=true also wipes user settings (macros).
	mux.HandleFunc("DELETE /all", func(w http.ResponseWriter, r *http.Request) {
		includeSettings := r.URL.Query().Get("include_settings") == "true"
		s.clearAll(includeSettings)
		_ = jsonOK(w, map[string]string{"status": "cleared"})
	})

	// GET /default-name — returns $AIMEBU_NAME from the server's env so the
	// web UI can prefill the "You are" field for first-time visitors.
	mux.HandleFunc("GET /default-name", func(w http.ResponseWriter, _ *http.Request) {
		_ = jsonOK(w, map[string]string{"name": os.Getenv("AIMEBU_NAME")})
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

	// GET /ws — WebSocket endpoint for real-time push
	mux.HandleFunc("GET /ws", handleWS(s))
}

// Run starts the HTTP server in the foreground with graceful shutdown.
func Run(addr, dataDir string, frontendFS fs.FS) error {
	if err := validateBindAddr(addr); err != nil {
		return err
	}
	allow, err := resolveAllow()
	if err != nil {
		return err
	}

	s, err := newStore(dataDir)
	if err != nil {
		return fmt.Errorf("init store: %w", err)
	}

	// Merge bundled defaults into the global macro map (write-once per key).
	s.applyDefaultMacros()

	// Start background cleanup (stale agents after 30min, empty rooms after 60min)
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	s.startCleanup(cleanupCtx)

	mux := http.NewServeMux()
	setupHandlers(mux, s)

	// Serve embedded frontend at /
	if frontendFS != nil {
		mux.Handle("GET /", http.FileServer(http.FS(frontendFS)))
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

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-done
		log.Println("Shutting down...")
		// best-effort: won't fire on SIGKILL or crash
		s.emitSystemMessage("_system", "server stopping")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("aimebu listening on http://%s (allow: %v, data: %s)", addr, allow, dataDir)
	host, _, _ := net.SplitHostPort(addr)
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.IsUnspecified() {
			log.Printf("WARN: bound to %s (all interfaces) — AIMEBU_ALLOW gates by source IP", host)
		} else if !ip.IsLoopback() {
			log.Printf("WARN: bound to %s — anyone reachable on this interface can attempt to call /; AIMEBU_ALLOW gates by source IP", host)
		}
	}
	s.emitSystemMessage("_system", "server started")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
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

// DefaultDataDir returns the data directory from env vars.
func DefaultDataDir() string {
	dataDir := os.Getenv("AIMEBU_DATA")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".aimebu")
	}
	return dataDir
}
