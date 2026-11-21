package server

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func stuckAfter() time.Duration {
	minutes, err := strconv.Atoi(os.Getenv("AIMEBU_STUCK_MINUTES"))
	if err != nil || minutes < 1 || minutes > 1440 {
		minutes = 10
	}
	return time.Duration(minutes) * time.Minute
}

func (s *Server) agentHealth(ctx context.Context, card store.AgentCard) ([]store.HealthFlag, error) {
	flags, err := s.Store.AgentHealth(ctx, card.ID, time.Now(), stuckAfter())
	if err != nil {
		return nil, err
	}
	if card.Desired != "stopped" {
		if _, err := s.machineForID(card.MachineID); err != nil {
			flags = append(flags, store.HealthFlag{Code: "machine_offline"})
		}
	}
	return flags, nil
}

type healthAgent struct {
	ID      string             `json:"id"`
	Name    string             `json:"name"`
	RoomIDs []string           `json:"room_ids"`
	Flags   []store.HealthFlag `json:"flags"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ordered, err := s.Store.HealthForUser(r.Context(), userID(r), time.Now(), stuckAfter())
	if err != nil {
		fail(w, err)
		return
	}
	for i := range ordered {
		if ordered[i].Desired != "stopped" {
			if _, err := s.machineForID(ordered[i].MachineID); err != nil {
				ordered[i].Flags = append(ordered[i].Flags, store.HealthFlag{Code: "machine_offline"})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stuck_minutes": int(stuckAfter() / time.Minute), "agents": ordered, "supervisor_restart_pending": s.localSupervisorPending.Load(), "supervisor_upgrade_required": s.localSupervisorUpgradeRequired.Load()})
}
