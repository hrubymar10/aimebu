package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// "Only run agents in docker" is an instance setting, off by default. When it is on, an agent whose launcher runs a harness natively (as the
// person that runs the supervisor, with that person's files within reach) is refused: nothing new is spawned, and a native agent that already
// exists gets no turns. The server refuses it when an agent is created or switched and when a turn is dispatched, and every spawn and
// delivery frame carries the setting so a supervisor refuses it as well.
const agentsDockerOnlyKey = "agents_docker_only"

// agentsWarmIdleKey holds how many seconds an agent's harness process is kept up after a turn (see supervisor/warm.go); absent is the default
// (defaultWarmIdleSeconds), a saved 0 is off.
const agentsWarmIdleKey = "agents_warm_idle_seconds"
const codexWarmEnabledKey = "codex_warm_enabled"

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

// keepAliveSettingKeys are the global settings keepAliveConfig reads.
var keepAliveSettingKeys = []string{keepAliveOnKey, keepAliveClaudeKey, keepAliveCodexKey, keepAliveStopKey}

func (s *Server) keepAliveConfig(ctx context.Context) (keepAliveConfig, error) {
	values := map[string]string{}
	for _, key := range keepAliveSettingKeys {
		value, ok, err := s.Store.GlobalSettingValue(ctx, key)
		if err != nil {
			return keepAliveConfigFrom(nil), err
		}
		if ok {
			values[key] = value
		}
	}
	return keepAliveConfigFrom(values), nil
}

// keepAliveConfigFrom is the keep-alive setting as the saved values (a key that is not saved is absent) give it.
func keepAliveConfigFrom(values map[string]string) keepAliveConfig {
	config := keepAliveConfig{Enabled: true, ClaudeMinutes: defaultKeepAliveClaude, CodexMinutes: defaultKeepAliveCodex, StopAfterHrs: defaultKeepAliveStopHrs}
	if value, ok := values[keepAliveOnKey]; ok {
		config.Enabled = value != "0"
	}
	for key, into := range map[string]*int{keepAliveClaudeKey: &config.ClaudeMinutes, keepAliveCodexKey: &config.CodexMinutes, keepAliveStopKey: &config.StopAfterHrs} {
		if value, ok := values[key]; ok {
			if n, convErr := strconv.Atoi(value); convErr == nil && n >= 0 {
				*into = n
			}
		}
	}
	config.ClaudeMinutes = min(config.ClaudeMinutes, maxKeepAliveClaudeMinutes) // a larger value stored before the cap would never read the cache
	config.Minutes = map[string]int{"claude": config.ClaudeMinutes, "codex": config.CodexMinutes}
	return config
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

// defaultWarmIdleSeconds is the warm window when the owner has saved none: five minutes, as decided when warm mode was planned.
const defaultWarmIdleSeconds = 5 * 60

// warmIdleSetting is the owner's saved warm window in seconds (0 is off) and whether one is saved. A value that cannot be read counts as none.
func (s *Server) warmIdleSetting(ctx context.Context) (seconds int, saved bool, err error) {
	value, ok, err := s.Store.GlobalSettingValue(ctx, agentsWarmIdleKey)
	if err != nil || !ok {
		return 0, false, err
	}
	seconds, convErr := strconv.Atoi(value)
	if convErr != nil || seconds < 0 {
		return 0, false, nil
	}
	return seconds, true, nil
}

// warmIdleSeconds is the instance's warm window in seconds as the server knows it: the owner's saved value, else the default (0 is off). A supervisor
// started with its own AIMEBU_WARM_IDLE may work to another when nothing is saved; it says which in its warm report.
func (s *Server) warmIdleSeconds(ctx context.Context) (int, error) {
	seconds, saved, err := s.warmIdleSetting(ctx)
	if err != nil {
		return 0, err
	}
	if !saved {
		return defaultWarmIdleSeconds, nil
	}
	return seconds, nil
}

// warmIdleToSend is what a delivery and an agent_settings frame carry: the owner's saved window, or nil when none is saved (the supervisor's own applies).
func (s *Server) warmIdleToSend(ctx context.Context) (*int, error) {
	seconds, saved, err := s.warmIdleSetting(ctx)
	if err != nil || !saved {
		return nil, err
	}
	return &seconds, nil
}

// agentIsolation answers GET and PUT /api/agent-isolation: any signed-in person reads it, an admin writes it. The same document carries the warm
// window of the agents (`warm_idle_minutes`: the owner's saved value, else the default of 5; `warm_idle_saved` says which; 0 is off), which only the owner changes: it decides how much memory the machines hold between turns. So does the drain grace
// (`supervisor_drain_grace_minutes`, 5 unless saved, 0 waits without limit; see local_supervisor.go), owner only as well.
func (s *Server) agentIsolation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		if !s.adminMachine(w, r) {
			return
		}
		var body struct {
			DockerOnly            *bool `json:"docker_only"`
			ReviewerAgentsEnabled *bool `json:"reviewer_agents_enabled"`
			WarmIdleMinutes       *int  `json:"warm_idle_minutes"`
			DrainGraceMinutes     *int  `json:"supervisor_drain_grace_minutes"`
			CodexWarmEnabled      *bool `json:"codex_warm_enabled"`
			KeepAlive             *struct {
				Enabled       *bool `json:"enabled"`
				ClaudeMinutes *int  `json:"claude_minutes"`
				CodexMinutes  *int  `json:"codex_minutes"`
				StopAfterHrs  *int  `json:"stop_after_hours"`
			} `json:"keep_alive"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.DockerOnly == nil && body.ReviewerAgentsEnabled == nil && body.WarmIdleMinutes == nil && body.DrainGraceMinutes == nil && body.CodexWarmEnabled == nil && body.KeepAlive == nil {
			apiError(w, 400, "invalid_request", "docker_only, reviewer_agents_enabled, warm_idle_minutes, supervisor_drain_grace_minutes, codex_warm_enabled or keep_alive is required")
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
		if body.WarmIdleMinutes != nil || body.DrainGraceMinutes != nil || body.CodexWarmEnabled != nil || body.KeepAlive != nil {
			isOwner, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
			if err != nil {
				fail(w, err)
				return
			}
			if !isOwner {
				apiError(w, 403, "forbidden", "only the owner can change how agents are kept warm or how long a supervisor's turns are waited for")
				return
			}
		}
		if body.WarmIdleMinutes != nil {
			if *body.WarmIdleMinutes < 0 || *body.WarmIdleMinutes > maxWarmIdleSeconds/60 {
				apiError(w, 400, "invalid_request", "warm_idle_minutes must be between 0 (off) and 1440")
				return
			}
		}
		if body.DrainGraceMinutes != nil {
			if *body.DrainGraceMinutes < 0 || *body.DrainGraceMinutes > maxDrainGraceSeconds/60 { // compared in minutes: a huge value must not wrap around in the multiplication
				apiError(w, 400, "invalid_request", supervisorDrainGraceMinutes+" must be between 0 (wait without limit) and 1440")
				return
			}
			if err := s.Store.SetGlobalSettingValue(r.Context(), supervisorDrainGraceKey, strconv.Itoa(*body.DrainGraceMinutes*60)); err != nil {
				fail(w, err)
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
		if body.ReviewerAgentsEnabled != nil {
			value := "0"
			if *body.ReviewerAgentsEnabled {
				value = "1"
			}
			if err := s.Store.SetGlobalSettingValue(r.Context(), store.ReviewerAgentsGlobalKey, value); err != nil {
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
		if body.CodexWarmEnabled != nil {
			value := "0"
			if *body.CodexWarmEnabled {
				value = "1"
			}
			if err := s.Store.SetGlobalSettingValue(r.Context(), codexWarmEnabledKey, value); err != nil {
				fail(w, err)
				return
			}
			s.codexWarmOn.Store(*body.CodexWarmEnabled)
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
	reviewers, _, err := s.Store.GlobalSettingValue(r.Context(), store.ReviewerAgentsGlobalKey)
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
	_, warmSaved, err := s.warmIdleSetting(r.Context())
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
	drainGrace, err := s.drainGraceSeconds(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{supervisorDrainGraceMinutes: drainGrace / 60, "reviewer_agents_enabled": reviewers != "0", "docker_only": on, "can_edit": canEdit, "warm_idle_minutes": warm / 60, "warm_idle_saved": warmSaved, "can_edit_warm": canEditWarm, "codex_warm_enabled": s.codexWarmOn.Load(), "keep_alive": keep})
}
