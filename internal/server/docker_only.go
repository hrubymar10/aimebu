package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// "Only run agents in docker" is an instance setting, off by default. When it is on, an agent whose launcher runs a harness natively (as the
// person that runs the supervisor, with that person's files within reach) is refused: nothing new is spawned, and a native agent that already
// exists gets no turns. The server refuses it when an agent is created or switched and when a turn is dispatched, and every spawn and
// delivery frame carries the setting so a supervisor refuses it as well.
const agentsDockerOnlyKey = "agents_docker_only"

// agentsWarmIdleKey holds how many seconds an agent's harness process is kept up after a turn (see supervisor/warm.go); absent or 0 is off.
const agentsWarmIdleKey = "agents_warm_idle_seconds"

// The cache keep-alive settings (see supervisor/keepalive.go). On by default: a ping every fifty minutes keeps Claude Code's one-hour prompt cache
// (about four for Codex, whose cache lifetime is not stated), until a day has passed without real work.
const (
	keepAliveOnKey          = "agents_keepalive_on"
	keepAliveClaudeKey      = "agents_keepalive_claude_minutes"
	keepAliveCodexKey       = "agents_keepalive_codex_minutes"
	keepAliveStopKey        = "agents_keepalive_stop_hours"
	defaultKeepAliveClaude  = 50
	defaultKeepAliveCodex   = 10
	defaultKeepAliveStopHrs = 24
)

// keepAliveConfig is the instance's keep-alive setting; Minutes is per tool (0 is off for that tool).
type keepAliveConfig struct {
	Enabled       bool           `json:"enabled"`
	Minutes       map[string]int `json:"-"`
	ClaudeMinutes int            `json:"claude_minutes"`
	CodexMinutes  int            `json:"codex_minutes"`
	StopAfterHrs  int            `json:"stop_after_hours"`
}

func (s *Server) keepAliveConfig(ctx context.Context) (keepAliveConfig, error) {
	config := keepAliveConfig{Enabled: true, ClaudeMinutes: defaultKeepAliveClaude, CodexMinutes: defaultKeepAliveCodex, StopAfterHrs: defaultKeepAliveStopHrs}
	read := func(key string, into *int) error {
		value, ok, err := s.Store.GlobalSettingValue(ctx, key)
		if err != nil || !ok {
			return err
		}
		if n, convErr := strconv.Atoi(value); convErr == nil && n >= 0 {
			*into = n
		}
		return nil
	}
	if value, ok, err := s.Store.GlobalSettingValue(ctx, keepAliveOnKey); err != nil {
		return config, err
	} else if ok {
		config.Enabled = value != "0"
	}
	for key, into := range map[string]*int{keepAliveClaudeKey: &config.ClaudeMinutes, keepAliveCodexKey: &config.CodexMinutes, keepAliveStopKey: &config.StopAfterHrs} {
		if err := read(key, into); err != nil {
			return config, err
		}
	}
	config.ClaudeMinutes = min(config.ClaudeMinutes, maxKeepAliveClaudeMinutes) // a larger value stored before the cap would never read the cache
	config.Minutes = map[string]int{"claude": config.ClaudeMinutes, "codex": config.CodexMinutes}
	return config, nil
}

// settingsFor is what a delivery of an agent of this tool carries: its interval (0 when keep-alive is off, or the tool is not one that is kept) and
// how long after the last real work the pings stop.
func (c keepAliveConfig) settingsFor(tool string) *protocol.KeepAliveSettings {
	interval := 0
	if c.Enabled {
		interval = c.Minutes[tool] * 60
	}
	return &protocol.KeepAliveSettings{IntervalSeconds: interval, StopAfterSeconds: c.StopAfterHrs * 3600}
}

// maxKeepAliveClaudeMinutes caps the Claude interval: Claude Code writes its prompt cache with a one-hour lifetime, so a ping later than that always
// finds it expired and only writes it again. Five minutes of margin are left for the ping itself and the clock.
const maxKeepAliveClaudeMinutes = 55

// maxWarmIdleSeconds bounds the setting: a day is already longer than any prompt cache.
const maxWarmIdleSeconds = 24 * 3600

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

// warmIdleSeconds is the instance's warm window in seconds; 0 is off (the default).
func (s *Server) warmIdleSeconds(ctx context.Context) (int, error) {
	value, ok, err := s.Store.GlobalSettingValue(ctx, agentsWarmIdleKey)
	if err != nil || !ok {
		return 0, err
	}
	seconds, convErr := strconv.Atoi(value)
	if convErr != nil || seconds < 0 {
		return 0, nil
	}
	return seconds, nil
}

// agentIsolation answers GET and PUT /api/agent-isolation: any signed-in person reads it, an admin writes it. The same document carries the warm
// window of the agents (`warm_idle_minutes`, 0 is off), which only the owner changes: it decides how much memory the machines hold between turns.
func (s *Server) agentIsolation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		if !s.adminMachine(w, r) {
			return
		}
		var body struct {
			DockerOnly      *bool `json:"docker_only"`
			WarmIdleMinutes *int  `json:"warm_idle_minutes"`
			KeepAlive       *struct {
				Enabled       *bool `json:"enabled"`
				ClaudeMinutes *int  `json:"claude_minutes"`
				CodexMinutes  *int  `json:"codex_minutes"`
				StopAfterHrs  *int  `json:"stop_after_hours"`
			} `json:"keep_alive"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.DockerOnly == nil && body.WarmIdleMinutes == nil && body.KeepAlive == nil {
			apiError(w, 400, "invalid_request", "docker_only, warm_idle_minutes or keep_alive is required")
			return
		}
		if body.KeepAlive != nil {
			for _, bound := range []struct {
				name  string
				value *int
				max   int
			}{{"claude_minutes", body.KeepAlive.ClaudeMinutes, maxKeepAliveClaudeMinutes}, {"codex_minutes", body.KeepAlive.CodexMinutes, 1440}, {"stop_after_hours", body.KeepAlive.StopAfterHrs, 24 * 30}} {
				if bound.value != nil && (*bound.value < 0 || *bound.value > bound.max) {
					apiError(w, 400, "invalid_request", "keep_alive."+bound.name+" must be between 0 and "+strconv.Itoa(bound.max))
					return
				}
			}
		}
		if body.WarmIdleMinutes != nil || body.KeepAlive != nil {
			isOwner, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
			if err != nil {
				fail(w, err)
				return
			}
			if !isOwner {
				apiError(w, 403, "forbidden", "only the owner can change how agents are kept warm")
				return
			}
		}
		if body.WarmIdleMinutes != nil {
			if *body.WarmIdleMinutes < 0 || *body.WarmIdleMinutes*60 > maxWarmIdleSeconds {
				apiError(w, 400, "invalid_request", "warm_idle_minutes must be between 0 (off) and 1440")
				return
			}
		}
		if body.DockerOnly != nil {
			value := "0"
			if *body.DockerOnly {
				value = "1"
			}
			if err := s.Store.SetGlobalSettingValue(r.Context(), agentsDockerOnlyKey, value); err != nil {
				fail(w, err)
				return
			}
		}
		if body.KeepAlive != nil {
			for key, value := range map[string]*int{keepAliveClaudeKey: body.KeepAlive.ClaudeMinutes, keepAliveCodexKey: body.KeepAlive.CodexMinutes, keepAliveStopKey: body.KeepAlive.StopAfterHrs} {
				if value == nil {
					continue
				}
				if err := s.Store.SetGlobalSettingValue(r.Context(), key, strconv.Itoa(*value)); err != nil {
					fail(w, err)
					return
				}
			}
			if body.KeepAlive.Enabled != nil {
				value := "0"
				if *body.KeepAlive.Enabled {
					value = "1"
				}
				if err := s.Store.SetGlobalSettingValue(r.Context(), keepAliveOnKey, value); err != nil {
					fail(w, err)
					return
				}
			}
		}
		if body.WarmIdleMinutes != nil {
			if err := s.Store.SetGlobalSettingValue(r.Context(), agentsWarmIdleKey, strconv.Itoa(*body.WarmIdleMinutes*60)); err != nil {
				fail(w, err)
				return
			}
		}
	}
	if r.Method == http.MethodPut {
		s.pushAgentSettings(r.Context()) // an idle agent gets no delivery: a setting turned off reaches it now
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
	warm, err := s.warmIdleSeconds(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	canEditWarm, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	keep, err := s.keepAliveConfig(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"docker_only": on, "can_edit": canEdit, "warm_idle_minutes": warm / 60, "can_edit_warm": canEditWarm, "keep_alive": keep})
}
