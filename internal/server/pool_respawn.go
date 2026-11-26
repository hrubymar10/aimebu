package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/auth"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// advancePoolRespawns starts the respawns an owner or admin asked for on a pool_change item. Each queued agent has no turn running (the
// store hands over only those), and is switched to its role's next pool choice through the same route as a person's harness switch, so a
// leader or any agent gets the switch's handoff and keeps its room. The decision was authorised when it was made, so the switch runs on the
// server's behalf (serverSwitchKey) with the decider recorded as its actor, and skips the membership check of the person's own route. Each
// respawn takes the role's next pool entry and advances the rotation cursor as a spawn does, so several agents of a role spread over the
// pool. A respawn that cannot begin is not lost: FailPoolRespawn raises the item again with the reason, and the agent stays as it is.
// serverSwitchKey marks a switch request the server makes for an authorised decision (see respawnOnPool).
type serverSwitchKey struct{}

func (s *Server) advancePoolRespawns(ctx context.Context) error {
	if queued, err := s.Store.PoolRespawnsQueued(ctx); err != nil || !queued {
		return err
	}
	ready, err := s.Store.PoolRespawnsReady(ctx)
	if err != nil {
		return err
	}
	for _, item := range ready {
		// The decision was authorised when it was made; before the switch begins, check again that its decider is still an owner or admin
		// of the project. One who is not any more cannot start the switch: the agent stays as it is, and the card carries the reason.
		if allowed, err := s.Store.PoolRespawnDeciderAllowed(ctx, item.ProjectID, item.DecidedBy); err != nil {
			return err
		} else if !allowed {
			log.Printf("pool respawn %s: the decider is no longer an owner or admin", item.AgentID)
			if err := s.Store.FailPoolRespawn(ctx, item.AgentID, "the decider of the change is no longer an owner or admin of the project"); err != nil {
				return err
			}
			continue
		}
		if err := s.respawnOnPool(ctx, item.AgentID, item.ProjectID, item.DecidedBy); err != nil {
			log.Printf("pool respawn %s: %v", item.AgentID, err)
			if err := s.Store.FailPoolRespawn(ctx, item.AgentID, err.Error()); err != nil {
				return err
			}
			continue
		}
		if err := s.Store.FinishPoolRespawn(ctx, item.AgentID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) respawnOnPool(ctx context.Context, agentID, projectID, actorID string) error {
	card, err := s.Store.AgentCardByID(ctx, agentID)
	if err != nil {
		return err
	}
	selection, ok, err := s.nextRoleSpawnDefault(ctx, projectID, card.RoleKey, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no pool entry of %s can be started now", card.RoleKey)
	}
	if card.RoomID == "" {
		return fmt.Errorf("the agent is in no room")
	}
	body, err := json.Marshal(map[string]any{"room_id": card.RoomID, "tool": selection.Request.Tool, "variant": selection.Request.Variant,
		"launcher_id": selection.Request.LauncherID, "model": selection.Request.Model, "machine_id": selection.Request.MachineID, "rev": card.DesiredRev})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.WithValue(auth.ContextWithUserID(ctx, actorID), serverSwitchKey{}, true), http.MethodPost, "/api/agents/"+agentID+"/switch", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetPathValue("id", agentID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", fmt.Sprintf("pool-respawn:%s:%d", agentID, card.DesiredRev))
	rec := httptest.NewRecorder()
	s.agentSwitch(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		var refused struct {
			Code    string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &refused) == nil && refused.Message != "" {
			return fmt.Errorf("%s (%s)", refused.Message, refused.Code)
		}
		return fmt.Errorf("the switch was refused (%d)", rec.Code)
	}
	if selection.Defaults.PickMode == "in_turn" {
		err := s.Store.Tx(ctx, func(tx *store.Tx) error {
			return tx.AdvanceRoleSpawnCursor(projectID, selection.Defaults.RoleID, selection.Defaults.NextPosition, selection.Steps)
		})
		if err != nil && !errors.Is(err, store.ErrRoleSpawnCursorChanged) {
			return err
		}
	}
	return s.Store.MarkPoolOrigin(ctx, agentID, selection.Request.LauncherID, selection.Request.Model)
}
