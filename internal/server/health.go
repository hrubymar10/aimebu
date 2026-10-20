package server

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hrubymar10/aimebu/internal/store"
)

func stuckAfter() time.Duration {
	minutes, err := strconv.Atoi(os.Getenv("AIMEBU2_STUCK_MINUTES"))
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
	projects, err := s.Store.ProjectsForUser(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	byID := map[string]*healthAgent{}
	ordered := []*healthAgent{}
	for _, project := range projects {
		rooms, err := s.Store.RoomsForUserProject(r.Context(), project.ID, userID(r))
		if err != nil {
			fail(w, err)
			return
		}
		for _, room := range rooms {
			cards, err := s.Store.AgentCardsForRoom(r.Context(), room.ID)
			if err != nil {
				fail(w, err)
				return
			}
			for _, card := range cards {
				item := byID[card.ID]
				if item == nil {
					flags, err := s.agentHealth(r.Context(), card)
					if err != nil {
						fail(w, err)
						return
					}
					item = &healthAgent{ID: card.ID, Name: card.Slug, RoomIDs: []string{}, Flags: flags}
					byID[card.ID] = item
					ordered = append(ordered, item)
				}
				item.RoomIDs = append(item.RoomIDs, room.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stuck_minutes": int(stuckAfter() / time.Minute), "agents": ordered})
}
