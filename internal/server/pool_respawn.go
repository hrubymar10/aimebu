package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/auth"
)

// advancePoolRespawns starts the respawns an owner or admin asked for on a pool_change item. Each queued agent has no turn running (the
// store hands over only those), and is switched to its role's next pool choice through the same route as a person's harness switch, so a
// leader or any agent gets the switch's handoff and keeps its room. A respawn that cannot begin is logged and dropped from the queue: the
// agent stays as it is, and the next pool change or server start raises the item again.
func (s *Server) advancePoolRespawns(ctx context.Context) error {
	if queued, err := s.Store.PoolRespawnsQueued(ctx); err != nil || !queued {
		return err
	}
	ready, err := s.Store.PoolRespawnsReady(ctx)
	if err != nil {
		return err
	}
	for _, item := range ready {
		if err := s.respawnOnPool(ctx, item.AgentID, item.ProjectID, item.DecidedBy); err != nil {
			log.Printf("pool respawn %s: %v", item.AgentID, err)
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
	req, err := http.NewRequestWithContext(auth.ContextWithUserID(ctx, actorID), http.MethodPost, "/api/agents/"+agentID+"/switch", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetPathValue("id", agentID)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("pool-respawn:%s:%d", agentID, card.DesiredRev))
	rec := httptest.NewRecorder()
	s.agentSwitch(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		return fmt.Errorf("switch refused (%d): %s", rec.Code, bytes.TrimSpace(rec.Body.Bytes()))
	}
	return s.Store.MarkPoolOrigin(ctx, agentID, selection.Request.LauncherID, selection.Request.Model)
}
