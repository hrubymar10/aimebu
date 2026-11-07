package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/internal/store"
)

// "Only run agents in docker" is an instance setting, off by default. When it is on, an agent whose launcher runs a harness natively (as the
// person that runs the supervisor, with that person's files within reach) is refused: nothing new is spawned, and a native agent that already
// exists gets no turns. The server refuses it when an agent is created or switched and when a turn is dispatched, and every spawn and
// delivery frame carries the setting so a supervisor refuses it as well.
const agentsDockerOnlyKey = "agents_docker_only"

// errDockerOnly is what a refused native launch says.
var errDockerOnly = errors.New("Only run agents in docker is on for this instance (Settings -> General), so a native launcher is refused: use a Docker launcher, or ask an admin to turn the setting off")

func (s *Server) dockerOnly(ctx context.Context) (bool, error) {
	value, ok, err := s.Store.GlobalSettingValue(ctx, agentsDockerOnlyKey)
	return ok && value == "1", err
}

// refusedNative says whether an agent with this tool, variant and launcher is refused: the setting is on and it does not run in a container.
// The test agent (tool fake) runs inside the process and never a harness.
func (s *Server) refusedNative(ctx context.Context, tool, variant string, launcher *store.Launcher) (bool, error) {
	if tool == "fake" {
		return false, nil
	}
	on, err := s.dockerOnly(ctx)
	if err != nil || !on {
		return false, err
	}
	return !launcherKeepsOwnSessions(launcher, variant), nil
}

// refusedAgent is refusedNative for an agent that exists: its launcher is looked up by id.
func (s *Server) refusedAgent(ctx context.Context, machineID, tool, variant, launcherID string) (bool, error) {
	if tool == "fake" {
		return false, nil
	}
	on, err := s.dockerOnly(ctx)
	if err != nil || !on {
		return false, err
	}
	if launcherID == "" {
		return variant != "docker", nil
	}
	launcher, err := s.Store.LauncherByID(ctx, machineID, launcherID)
	if err != nil {
		return false, err
	}
	return !launcher.Containerized, nil
}

// agentIsolation answers GET and PUT /api/agent-isolation: any signed-in person reads it, an admin writes it.
func (s *Server) agentIsolation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		if !s.adminMachine(w, r) {
			return
		}
		var body struct {
			DockerOnly *bool `json:"docker_only"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.DockerOnly == nil {
			apiError(w, 400, "invalid_request", "docker_only is required")
			return
		}
		value := "0"
		if *body.DockerOnly {
			value = "1"
		}
		if err := s.Store.SetGlobalSettingValue(r.Context(), agentsDockerOnlyKey, value); err != nil {
			fail(w, err)
			return
		}
	}
	on, err := s.dockerOnly(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	canEdit, err := s.Store.IsGlobalAdmin(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"docker_only": on, "can_edit": canEdit})
}
