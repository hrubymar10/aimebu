package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/types"
)

// wsCmd is a command from the WebSocket client.
type wsCmd struct {
	Type    string   `json:"type"`              // "hello", "subscribe", "unsubscribe"
	AgentID string   `json:"agent_id,omitempty"` // for "hello"
	Rooms   []string `json:"rooms,omitempty"`    // for "subscribe"/"unsubscribe"
}

// wsEvent is a JSON frame sent to the WebSocket client.
type wsEvent struct {
	Type string `json:"type"` // "message", "room_update", "agent_update"
	Data any    `json:"data"`
}

// wsConn represents a single WebSocket connection.
type wsConn struct {
	conn *websocket.Conn
	s    *store

	mu        sync.Mutex
	agentID   string // set by "hello" command
	roomChans map[string]chan types.Message
	// Shared channel: room forwarder goroutines send messages here
	msgCh chan types.Message
}

func handleWS(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // allow any origin (same as CORS *)
		})
		if err != nil {
			log.Printf("ws accept error: %v", err)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		wc := &wsConn{
			conn:      conn,
			s:         s,
			roomChans: make(map[string]chan types.Message),
			msgCh:     make(chan types.Message, 128),
		}

		// Subscribe to meta events (room/agent changes)
		metaCh := s.subscribeMeta()
		defer s.unsubscribeMeta(metaCh)

		// Send initial state snapshot
		wc.sendEvent(ctx, wsEvent{Type: "room_update", Data: map[string]any{"rooms": s.listRooms()}})
		wc.sendEvent(ctx, wsEvent{Type: "agent_update", Data: map[string]any{"agents": s.listAgents()}})

		// Start reader goroutine (handles subscribe/unsubscribe commands)
		cmdCh := make(chan wsCmd, 16)
		go wc.readLoop(ctx, cmdCh)

		// Writer loop
		wc.writeLoop(ctx, metaCh, cmdCh)

		// Clean up room subscriptions
		wc.mu.Lock()
		for roomID, ch := range wc.roomChans {
			s.unsubscribeRoom(roomID, ch)
		}
		wc.roomChans = nil
		wc.mu.Unlock()

		conn.Close(websocket.StatusNormalClosure, "bye")
	}
}

// readLoop reads JSON commands from the WebSocket client.
func (wc *wsConn) readLoop(ctx context.Context, cmdCh chan<- wsCmd) {
	defer close(cmdCh)
	for {
		_, data, err := wc.conn.Read(ctx)
		if err != nil {
			return
		}
		var cmd wsCmd
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}
		select {
		case cmdCh <- cmd:
		case <-ctx.Done():
			return
		}
	}
}

// writeLoop is the main event loop for a WebSocket connection.
// It selects over: meta events, incoming commands, forwarded room messages,
// and a heartbeat ticker that keeps the agent alive.
func (wc *wsConn) writeLoop(ctx context.Context, metaCh <-chan MetaEvent, cmdCh <-chan wsCmd) {
	heartbeat := time.NewTicker(2 * time.Minute)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case cmd, ok := <-cmdCh:
			if !ok {
				return // client disconnected
			}
			wc.handleCmd(ctx, cmd)

		case evt, ok := <-metaCh:
			if !ok {
				return
			}
			// For attention_event, skip if this WS is already subscribed to the room:
			// the message will arrive via the room subscription channel so the FE
			// handles it through handleWSMessage instead (avoids double-counting).
			if evt.Type == "attention_event" {
				if data, ok := evt.Data.(map[string]any); ok {
					if roomID, ok := data["room_id"].(string); ok {
						wc.mu.Lock()
						_, subscribed := wc.roomChans[roomID]
						wc.mu.Unlock()
						if subscribed {
							continue
						}
					}
				}
			}
			wc.sendEvent(ctx, wsEvent{Type: evt.Type, Data: evt.Data})

		case msg := <-wc.msgCh:
			wc.sendEvent(ctx, wsEvent{Type: "message", Data: msg})

		case <-heartbeat.C:
			wc.mu.Lock()
			aid := wc.agentID
			wc.mu.Unlock()
			if aid != "" {
				wc.s.touchAgent(aid)
			}
		}
	}
}

// forwardRoom reads messages from a room subscription channel and forwards them
// to the shared msgCh. Exits when the room channel is closed or ctx is cancelled.
func (wc *wsConn) forwardRoom(ctx context.Context, roomCh chan types.Message) {
	for {
		select {
		case msg, ok := <-roomCh:
			if !ok {
				return
			}
			select {
			case wc.msgCh <- msg:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// handleCmd processes a hello/subscribe/unsubscribe command.
func (wc *wsConn) handleCmd(ctx context.Context, cmd wsCmd) {
	switch cmd.Type {
	case "hello":
		if cmd.AgentID != "" {
			wc.mu.Lock()
			wc.agentID = cmd.AgentID
			wc.mu.Unlock()
			// Touch immediately so the agent stays alive
			wc.s.touchAgent(cmd.AgentID)
		}

	case "subscribe":
		wc.mu.Lock()
		for _, roomID := range cmd.Rooms {
			if _, ok := wc.roomChans[roomID]; ok {
				continue // already subscribed
			}
			ch := wc.s.subscribeRoom(roomID)
			wc.roomChans[roomID] = ch
			go wc.forwardRoom(ctx, ch)
		}
		wc.mu.Unlock()

	case "unsubscribe":
		wc.mu.Lock()
		for _, roomID := range cmd.Rooms {
			if ch, ok := wc.roomChans[roomID]; ok {
				wc.s.unsubscribeRoom(roomID, ch)
				delete(wc.roomChans, roomID)
			}
		}
		wc.mu.Unlock()
	}
}

// sendEvent marshals and writes a JSON event to the WebSocket.
func (wc *wsConn) sendEvent(ctx context.Context, evt wsEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	_ = wc.conn.Write(ctx, websocket.MessageText, data)
}
