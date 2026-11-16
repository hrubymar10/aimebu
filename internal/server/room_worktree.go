package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
	"github.com/hrubymar10/aimebu/internal/worktree"
)

// errWorktreeUnsupported is a machine whose supervisor predates worktree operations.
var errWorktreeUnsupported = errors.New("remote worktree requires an updated supervisor")

type worktreeRequest struct {
	Enabled bool   `json:"enabled"`
	BaseRef string `json:"base_ref,omitempty"`
}

func (s *Server) machineWorktree(ctx context.Context, machine store.Machine, root, roomID, baseRef, action string, force bool) (worktree.Result, error) {
	if machine.IsHost {
		if action == "validate" {
			return worktree.Result{}, worktree.Validate(ctx, root)
		}
		if action == "remove" {
			return worktree.Remove(ctx, root, roomID, baseRef, force)
		}
		if action == "inspect" {
			return worktree.Inspect(ctx, root, roomID, baseRef)
		}
		if action == "retire" {
			return worktree.Retire(ctx, root, roomID)
		}
		return worktree.Ensure(ctx, root, roomID, baseRef)
	}
	conn, err := s.machineForID(machine.ID)
	if err != nil {
		return worktree.Result{}, err
	}
	if !conn.supportsWorktree {
		return worktree.Result{}, errWorktreeUnsupported
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return worktree.Result{}, err
	}
	id := hex.EncodeToString(nonce[:])
	channel := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	if conn.pathPending == nil {
		conn.pathPending = make(map[string]chan protocol.Frame)
	}
	conn.pathPending[id] = channel
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.pathPending, id); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: protocol.WorktreeOp, ReqID: id,
		CheckoutPath: root, RoomID: roomID, BaseRef: baseRef, WorktreeAction: action, WorktreeForce: force}); err != nil {
		return worktree.Result{}, err
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case frame := <-channel:
		if frame.Code != "" {
			return worktree.Result{}, &worktree.Error{Code: frame.Code, Err: errors.New(frame.Message)}
		}
		var result worktree.Result
		if err := json.Unmarshal(frame.Details, &result); err != nil {
			return result, err
		}
		return result, nil
	case <-conn.done:
		return worktree.Result{}, fmt.Errorf("%w: supervisor disconnected during worktree operation", ErrMachineUnavailable)
	case <-ctx.Done():
		return worktree.Result{}, ctx.Err()
	case <-timer.C:
		return worktree.Result{}, fmt.Errorf("%w: supervisor did not answer worktree operation", ErrSlowMachine)
	}
}

func (s *Server) ensureRoomWorktree(ctx context.Context, room store.Room, machine store.Machine, baseRef string) (store.RoomWorktree, error) {
	// A worktree is made in the project's folder on the machine: not before the owner has admitted that machine for the project.
	if admitted, err := s.machineAdmitted(ctx, room.ProjectID, machine); err != nil {
		return store.RoomWorktree{}, err
	} else if !admitted {
		return store.RoomWorktree{}, errMachineNotAdmitted
	}
	root, err := s.selectedMachinePath(ctx, room.ProjectID, machine)
	if err != nil {
		return store.RoomWorktree{}, err
	}
	if _, err := s.machineWorktree(ctx, machine, root, room.ID, baseRef, "validate", false); err != nil {
		return store.RoomWorktree{}, err
	}
	item, err := s.Store.RequestRoomWorktree(ctx, room.ID, baseRef)
	if err != nil {
		return item, err
	}
	result, err := s.machineWorktree(ctx, machine, root, room.ID, item.BaseRef, "ensure", false)
	if err != nil {
		prior, found, lookupErr := s.Store.RoomWorktree(ctx, room.ID, machine.ID)
		state := "failed"
		baseCommit := item.BaseCommit
		if lookupErr == nil && found && prior.Path != "" && prior.State == "ready" {
			state = "ready"
			baseCommit = prior.BaseCommit
		}
		_ = s.Store.RecordRoomWorktree(ctx, room.ID, machine.ID, "", item.BaseRef, baseCommit, state, err.Error(), 0)
		return item, err
	}
	if prior, found, priorErr := s.Store.RoomWorktree(ctx, room.ID, machine.ID); priorErr == nil && found && prior.BaseCommit != "" {
		result.BaseCommit = prior.BaseCommit
	}
	if err := s.Store.RecordRoomWorktree(ctx, room.ID, machine.ID, result.Path, result.BaseRef, result.BaseCommit, "ready", "", result.DirtyBase); err != nil {
		return item, err
	}
	if _, err := s.Store.DecideRoomCheckout(ctx, room.ID, "worktree", true); err != nil {
		return item, err
	}
	item, _, err = s.Store.RoomWorktree(ctx, room.ID, machine.ID)
	return item, err
}

// roomCheckoutPath says where an agent spawned into a room works. For a task room the first spawn decides (a worktree, or the project
// folder) and the decision is kept, so a room is never split between the two: a repository that gets its first commit after an agent
// fell back to the project folder does not give the next agent a worktree. Task rooms get a worktree by default (project worktree_mode
// "auto", room created after that took effect); if one cannot be made, for any reason, the room uses the project folder and the
// half-made request is dropped. An explicit request still reports its failure.
func (s *Server) roomCheckoutPath(ctx context.Context, room store.Room, machine store.Machine, optIn bool, baseRef string) (string, *store.RoomWorktree, error) {
	if room.Kind != "task" {
		return s.checkoutPathFor(ctx, room, machine, optIn, baseRef)
	}
	// One room's first spawn does not wait for another room's git work.
	lock, _ := s.checkoutLocks.LoadOrStore(room.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	mode, err := s.Store.RoomCheckoutMode(ctx, room.ID)
	if err != nil {
		return "", nil, err
	}
	item, exists, err := s.Store.RoomWorktree(ctx, room.ID, machine.ID)
	if err != nil {
		return "", nil, err
	}
	switch {
	case optIn || exists || mode == "worktree":
		item, err = s.ensureRoomWorktree(ctx, room, machine, baseRef)
		if err != nil {
			return "", nil, err
		}
		return item.Path, &item, nil
	case mode == "project_folder":
		return s.projectCheckoutPath(ctx, room, machine)
	}
	// Undecided: this spawn decides.
	auto := false
	if room.Lifecycle == "open" {
		if auto, err = s.Store.AutoWorktreeApplies(ctx, room.ID); err != nil {
			return "", nil, err
		}
	}
	if auto {
		item, err = s.ensureRoomWorktree(ctx, room, machine, baseRef)
		if err == nil {
			return item.Path, &item, nil
		}
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if cancelErr := s.Store.CancelRoomWorktree(ctx, room.ID); cancelErr != nil && !errors.Is(cancelErr, store.ErrConflict) {
			return "", nil, cancelErr
		}
	}
	path, item2, err := s.projectCheckoutPath(ctx, room, machine)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.Store.DecideRoomCheckout(ctx, room.ID, "project_folder", false); err != nil {
		return "", nil, err
	}
	return path, item2, nil
}

// existingCheckoutPath is where an agent of the room goes when its own worktree is gone: the room's ready worktree if it has one, else the
// project folder. It only reads, so it works for a room that is archived and never makes a worktree as a side effect.
func (s *Server) existingCheckoutPath(ctx context.Context, room store.Room, machine store.Machine) (string, error) {
	item, exists, err := s.Store.RoomWorktree(ctx, room.ID, machine.ID)
	if err != nil {
		return "", err
	}
	if exists && item.State == "ready" && item.Path != "" {
		return item.Path, nil
	}
	path, _, err := s.projectCheckoutPath(ctx, room, machine)
	return path, err
}

func (s *Server) projectCheckoutPath(ctx context.Context, room store.Room, machine store.Machine) (string, *store.RoomWorktree, error) {
	path, err := s.selectedMachinePath(ctx, room.ProjectID, machine)
	return path, nil, err
}

// checkoutPathFor is the checkout of a room that is not a task room: its worktree if it has one or one is asked for, else the project folder.
func (s *Server) checkoutPathFor(ctx context.Context, room store.Room, machine store.Machine, optIn bool, baseRef string) (string, *store.RoomWorktree, error) {
	item, exists, err := s.Store.RoomWorktree(ctx, room.ID, machine.ID)
	if err != nil {
		return "", nil, err
	}
	if optIn || exists {
		item, err = s.ensureRoomWorktree(ctx, room, machine, baseRef)
		if err != nil {
			return "", nil, err
		}
		return item.Path, &item, nil
	}
	return s.projectCheckoutPath(ctx, room, machine)
}

func (s *Server) roomWorktree(w http.ResponseWriter, r *http.Request) {
	room, ok := s.roomAccess(w, r, r.PathValue("room"))
	if !ok {
		return
	}
	if room.Kind != "task" {
		apiError(w, 409, "invalid_kind", "only task rooms can have worktrees")
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		host, err := s.Store.HostMachine(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		machineID = host.ID
	}
	if r.Method == http.MethodGet {
		item, found, err := s.Store.RoomWorktree(r.Context(), room.ID, machineID)
		if err != nil {
			fail(w, err)
			return
		}
		if !found {
			writeJSON(w, 200, map[string]any{"enabled": false})
			return
		}
		// A viewer sees where the worktree is under the project (relative_path), not the absolute path on the machine's disk.
		if role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID); err != nil {
			fail(w, err)
			return
		} else if role == "viewer" {
			item.Path = ""
		}
		writeJSON(w, 200, item)
		return
	}
	role, err := s.Store.UserProjectRole(r.Context(), userID(r), room.ProjectID)
	if err != nil {
		fail(w, err)
		return
	}
	if role != "owner" && role != "operator" {
		apiError(w, 403, "forbidden", "project owner or operator required")
		return
	}
	var machine store.Machine
	machine, err = s.Store.MachineByID(r.Context(), machineID)
	if err != nil {
		fail(w, err)
		return
	}
	if r.Method == http.MethodPost {
		var req worktreeRequest
		if !readJSON(w, r, &req) {
			return
		}
		if !req.Enabled {
			apiError(w, 400, "invalid_request", "enabled must be true")
			return
		}
		item, err := s.ensureRoomWorktree(r.Context(), room, machine, req.BaseRef)
		if err != nil {
			worktreeError(w, err)
			return
		}
		writeJSON(w, 200, item)
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
		Force   bool   `json:"force"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Force && req.Confirm != "REMOVE "+room.ID {
		apiError(w, 400, "confirmation_required", "confirm with REMOVE followed by the room ID")
		return
	}
	item, found, err := s.Store.RoomWorktree(r.Context(), room.ID, machineID)
	if err != nil || !found {
		if err != nil {
			fail(w, err)
		} else {
			apiError(w, 404, "not_found", "room has no worktree")
		}
		return
	}
	if room.Lifecycle != "closed" {
		if item.Path == "" {
			if err := s.Store.CancelRoomWorktree(r.Context(), room.ID); err != nil {
				worktreeError(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"enabled": false})
			return
		}
		room, err = s.archiveRoomForDeletion(r.Context(), room, userID(r))
		if err != nil {
			roomRemoveError(w, err)
			return
		}
	}
	if item.Path == "" {
		apiError(w, 404, "not_found", "worktree was not created on this machine")
		return
	}
	inUse, err := s.Store.RoomWorktreeInUse(r.Context(), machine.ID, item.Path)
	if err != nil {
		fail(w, err)
		return
	}
	if inUse {
		apiError(w, 409, "worktree_in_use", "an active agent still uses this worktree")
		return
	}
	root, err := s.selectedMachinePath(r.Context(), room.ProjectID, machine)
	if err != nil {
		worktreeError(w, err)
		return
	}
	result, err := s.machineWorktree(r.Context(), machine, root, room.ID, item.BaseRef, "remove", req.Force)
	if err != nil {
		worktreeError(w, err)
		return
	}
	if err := s.Store.RecordRoomWorktree(r.Context(), room.ID, machine.ID, result.Path, item.BaseRef, item.BaseCommit, result.State, result.Reason, item.DirtyBaseCount); err != nil {
		fail(w, err)
		return
	}
	item, _, err = s.Store.RoomWorktree(r.Context(), room.ID, machineID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func worktreeError(w http.ResponseWriter, err error) {
	var gitErr *worktree.Error
	if errors.As(err, &gitErr) {
		apiError(w, 409, gitErr.Code, gitErr.Error())
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "room or machine not found")
		return
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		apiError(w, 409, "invalid_transition", "room is not open or is not a task room")
		return
	}
	if errors.Is(err, errMachineNotAdmitted) {
		apiError(w, 403, "machine_not_admitted", err.Error())
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		apiError(w, 403, "forbidden", "not allowed to manage this room worktree")
		return
	}
	if errors.Is(err, errWorktreeUnsupported) {
		apiError(w, 409, "supervisor_update_needed", "this machine's supervisor cannot manage worktrees; update it and retry")
		return
	}
	if errors.Is(err, ErrMachineUnavailable) || errors.Is(err, ErrSlowMachine) {
		apiError(w, 503, "machine_unavailable", "the supervisor could not answer the worktree operation; reconnect it and retry")
		return
	}
	// SQLite failures are internal; path checks, git commands and request
	// cancellation remain fixable worktree errors with their useful message.
	var databaseError interface{ Code() int }
	if errors.As(err, &databaseError) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		fail(w, fmt.Errorf("worktree operation: %w", err))
		return
	}
	apiError(w, 409, "worktree_failed", fmt.Sprint(err))
}
