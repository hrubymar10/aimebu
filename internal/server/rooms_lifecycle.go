package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func (s *Server) closeRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	room, err := s.Store.RoomByID(r.Context(), roomID)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectAccess(w, r, room.ProjectID) {
		return
	}
	var req struct {
		LifecycleRev int64 `json:"lifecycle_rev"`
		Cascade      bool  `json:"cascade"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	room, controls, err := s.Store.BeginRoomClose(r.Context(), roomID, userID(r), req.LifecycleRev, req.Cascade)
	if err != nil {
		roomLifecycleError(w, err)
		return
	}
	s.sendRoomControls(r.Context(), controls)
	writeJSON(w, http.StatusAccepted, room)
}

func (s *Server) reopenRoom(w http.ResponseWriter, r *http.Request) {
	roomID := r.PathValue("room")
	room, err := s.Store.RoomByID(r.Context(), roomID)
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectAccess(w, r, room.ProjectID) {
		return
	}
	var req struct {
		LifecycleRev int64 `json:"lifecycle_rev"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	room, err = s.Store.ReopenRoom(r.Context(), roomID, userID(r), req.LifecycleRev)
	if err != nil {
		roomLifecycleError(w, err)
		return
	}
	if item, found, err := s.Store.RoomWorktree(r.Context(), roomID, ""); err == nil && found {
		host, hostErr := s.Store.HostMachine(r.Context())
		if hostErr == nil {
			item, hostErr = s.ensureRoomWorktree(r.Context(), room, host, item.BaseRef)
		}
		if hostErr != nil {
			item.State, item.Reason = "failed", hostErr.Error()
		}
		room.Worktree = &item
	}
	writeJSON(w, http.StatusOK, room)
}

func roomLifecycleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrHomeRoom):
		apiError(w, 409, "home_room", "project home room cannot be closed")
	case errors.Is(err, store.ErrFlowRoom):
		apiError(w, 409, "flow_room", "the team flow's general, routing and advice rooms cannot be closed")
	case errors.Is(err, store.ErrOpenChildren):
		apiError(w, 409, "open_children", "close child rooms first or confirm cascade")
	case errors.Is(err, store.ErrStaleRev):
		apiError(w, 409, "stale_rev", "room lifecycle revision changed")
	case errors.Is(err, store.ErrInvalidTransition):
		apiError(w, 409, "invalid_transition", "room state disallows this operation")
	default:
		fail(w, err)
	}
}

// sendRoomControls mirrors the normal agent-control route. It is also used
// by the dispatcher as closure advances after an HTTP request or restart.
func (s *Server) sendRoomControls(ctx context.Context, controls []store.AgentCard) {
	for _, control := range controls {
		card, err := s.Store.AgentCardByID(ctx, control.ID)
		if err != nil {
			continue
		}
		machine, err := s.machineForID(card.MachineID)
		if err != nil {
			continue
		}
		machine.boundMu.Lock()
		machine.bound[control.ID] = control.Generation
		machine.boundMu.Unlock()
		_ = machine.sendControl(protocol.Frame{V: protocol.Version, Type: protocol.Control, AgentID: control.ID,
			Generation: control.Generation, DesiredRev: control.DesiredRev, DesiredState: control.Desired})
		if control.Desired != "stopped" {
			if pending, ok, err := s.Store.PendingSpawnByID(ctx, control.ID); err == nil && ok {
				_ = s.sendPendingSpawn(ctx, machine, pending)
			}
		}
	}
}

// archiveRoomForDeletion starts the ordinary close and advances it once. A
// running turn may keep the room closing; the caller then gets the same
// not-closed conflict as an explicit archive followed by delete.
func (s *Server) archiveRoomForDeletion(ctx context.Context, room store.Room, actor string) (store.Room, error) {
	if room.Lifecycle == "open" {
		closed, controls, err := s.Store.BeginRoomClose(ctx, room.ID, actor, room.LifecycleRev, false)
		if err != nil {
			return room, err
		}
		room = closed
		s.sendRoomControls(ctx, controls)
	}
	if room.Lifecycle == "closing" {
		controls, _, err := s.Store.AdvanceRoomClosures(ctx, 30*time.Second)
		if err != nil {
			return room, err
		}
		s.sendRoomControls(ctx, controls)
		room, err = s.Store.RoomByID(ctx, room.ID)
		if err != nil {
			return room, err
		}
	}
	if room.Lifecycle != "closed" {
		return room, store.ErrRoomNotClosed
	}
	return room, nil
}

// DELETE /api/rooms/{room}: the project owner archives a live task room and
// removes it when its close can finish immediately.
func (s *Server) removeRoom(w http.ResponseWriter, r *http.Request) {
	room, err := s.Store.RoomByID(r.Context(), r.PathValue("room"))
	if err != nil {
		fail(w, err)
		return
	}
	if !s.projectAccess(w, r, room.ProjectID) {
		return
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID)
	if err != nil {
		userError(w, err)
		return
	}
	if role != "owner" {
		apiError(w, 403, "forbidden", "project owner required")
		return
	}
	if room.Kind == "home" {
		roomRemoveError(w, store.ErrHomeRoom)
		return
	}
	if room.Kind == "dm" {
		roomRemoveError(w, store.ErrRoomNotRemovable)
		return
	}
	room, err = s.archiveRoomForDeletion(r.Context(), room, userID(r))
	if err != nil {
		roomRemoveError(w, err)
		return
	}
	if err := s.Store.RemoveRoom(r.Context(), room.ID); err != nil {
		roomRemoveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func roomRemoveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrHomeRoom):
		apiError(w, 409, "home_room", "project home room cannot be removed")
	case errors.Is(err, store.ErrRoomNotClosed):
		apiError(w, 409, "room_not_closed", "room archive is still in progress; retry when it closes")
	case errors.Is(err, store.ErrOpenChildren):
		apiError(w, 409, "has_children", "remove the child rooms first")
	case errors.Is(err, store.ErrRoomNotRemovable):
		apiError(w, 409, "room_not_removable", "private chats and rooms with a live worktree cannot be removed")
	case errors.Is(err, store.ErrStaleRev), errors.Is(err, store.ErrInvalidTransition):
		roomLifecycleError(w, err)
	default:
		fail(w, err)
	}
}
