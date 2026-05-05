package server

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

var macroKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

const busWaitTimeoutHint = "keep_waiting=true (status=\"still_waiting\") means no messages arrived yet, not that listening is over. Call bus_wait again now. Only return to the user if they explicitly told you to stop."

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
		jsonOK(w, room)
	})

	// GET /rooms — list all rooms
	mux.HandleFunc("GET /rooms", func(w http.ResponseWriter, _ *http.Request) {
		rooms := s.listRooms()
		jsonOK(w, map[string]any{"rooms": rooms})
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
		jsonOK(w, map[string]any{
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
		jsonOK(w, map[string]string{"status": "deleted"})
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
		jsonOK(w, room)
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
		jsonOK(w, map[string]string{"status": "left", "room": roomID})
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
		id, err := s.roomSend(roomID, req.From, req.Body)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		jsonOK(w, map[string]any{"id": id, "room": roomID})
	})

	// GET /rooms/{room_id}/messages
	mux.HandleFunc("GET /rooms/{room_id}/messages", func(w http.ResponseWriter, r *http.Request) {
		roomID := r.PathValue("room_id")
		if agentID := r.URL.Query().Get("agent_id"); agentID != "" {
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
		jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
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
				if s.advanceCursor(agentID, roomID, last) {
					s.broadcastReadUpdate(agentID, roomID, last)
				}
			}
			jsonOK(w, map[string]any{"messages": msgs, "room": roomID})
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
					if s.advanceCursor(agentID, roomID, msg.ID) {
						s.broadcastReadUpdate(agentID, roomID, msg.ID)
					}
				}
				jsonOK(w, map[string]any{"messages": []types.Message{msg}, "room": roomID})
				return
			case <-timer.C:
				jsonOK(w, map[string]any{
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
			if !sinceExplicit {
				// Advance cursors for every room we returned messages from.
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
			jsonOK(w, map[string]any{"messages": msgs, "agent": agentID})
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
				if !sinceExplicit {
					if s.advanceCursor(agentID, msg.RoomID, msg.ID) {
						s.broadcastReadUpdate(agentID, msg.RoomID, msg.ID)
					}
				}
				jsonOK(w, map[string]any{"messages": []types.Message{msg}, "agent": agentID})
				return
			case <-timer.C:
				jsonOK(w, map[string]any{
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

		id, err := s.roomSend(room.ID, req.From, req.Body)
		if err != nil {
			jsonError(w, err.Error(), http.StatusForbidden)
			return
		}
		jsonOK(w, map[string]any{"id": id, "room": room.ID})
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
			agent, err = s.registerAI(req.Model, req.Harness, req.Project, req.Meta, forceName)
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

		jsonOK(w, types.RegisterResponse{
			ID:      agent.ID,
			Name:    agent.Name,
			Kind:    agent.Kind,
			Model:   agent.Model,
			Harness: agent.Harness,
			Project: agent.Project,
			Meta:    agent.Meta,
		})
	})

	// GET /agents — list
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		agents := s.listAgents()
		jsonOK(w, map[string]any{"agents": agents})
	})

	// GET /agents/{id}/rooms — rooms an agent is in, with per-agent unread
	// counts and read cursor. Returns AgentRoomView (Room + unread_count +
	// last_id + read_cursor) per room.
	mux.HandleFunc("GET /agents/{id}/rooms", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		rooms := s.agentRoomViews(agentID)
		jsonOK(w, map[string]any{"rooms": rooms, "agent": agentID})
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
		jsonOK(w, map[string]any{"agent": agentID, "room": req.Room, "read_cursor": target})
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
		jsonOK(w, msg)
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
		jsonOK(w, map[string]any{"messages": msgs})
	})

	// ── Macros ─────────────────────────────────────────────────────

	// GET /macros — fetch global + per-room macros
	mux.HandleFunc("GET /macros", func(w http.ResponseWriter, _ *http.Request) {
		env := s.getEnvelope()
		jsonOK(w, map[string]any{"macros": env.Macros, "rooms": env.Rooms})
	})

	// PUT /macros — full replace of global + per-room macros
	mux.HandleFunc("PUT /macros", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Macros map[string]string            `json:"macros"`
			Rooms  map[string]map[string]string `json:"rooms"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
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
		totalRoom := 0
		for rid, rm := range payload.Rooms {
			if len(rm) > 256 {
				jsonError(w, "too many macros in room "+rid+" (max 256)", http.StatusBadRequest)
				return
			}
			for k, v := range rm {
				if !macroKeyRE.MatchString(k) {
					jsonError(w, "invalid macro key in room "+rid+": "+k, http.StatusBadRequest)
					return
				}
				if len(v) > 16*1024 {
					jsonError(w, "macro body too large in room "+rid+" (max 16KB): "+k, http.StatusBadRequest)
					return
				}
				totalRoom++
			}
		}
		if totalRoom > 4096 {
			jsonError(w, "too many room macros total (max 4096)", http.StatusBadRequest)
			return
		}
		s.setEnvelope(macrosEnvelope{Macros: payload.Macros, Rooms: payload.Rooms})
		jsonOK(w, map[string]string{"status": "ok"})
	})

	// DELETE /all — clear everything
	mux.HandleFunc("DELETE /all", func(w http.ResponseWriter, _ *http.Request) {
		s.clearAll()
		jsonOK(w, map[string]string{"status": "cleared"})
	})

	// GET /default-name — returns $AIMEBU_NAME from the server's env so the
	// web UI can prefill the "You are" field for first-time visitors.
	mux.HandleFunc("GET /default-name", func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w, map[string]string{"name": os.Getenv("AIMEBU_NAME")})
	})

	// GET /health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w, map[string]string{"status": "ok"})
	})

	// GET /ws — WebSocket endpoint for real-time push
	mux.HandleFunc("GET /ws", handleWS(s))
}

// Run starts the HTTP server in the foreground with graceful shutdown.
func Run(addr, dataDir string, frontendFS fs.FS) error {
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

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-done
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("aimebu listening on http://%s (data: %s)", addr, dataDir)
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
