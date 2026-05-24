package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-json"
)

const (
	maxFleets         = 32
	maxFleetAgents    = 16
	maxFleetCommandKB = 16
)

var fleetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

type FleetAgent struct {
	Command      string `json:"command"`
	WrapTerminal *bool  `json:"wrap_terminal,omitempty"`
	AutoSetCwd   *bool  `json:"auto_set_cwd,omitempty"`
}

type Fleet struct {
	Agents []FleetAgent `json:"agents"`
}

type fleetsEnvelope struct {
	Version int              `json:"version,omitempty"`
	Fleets  map[string]Fleet `json:"fleets"`
}

func normalizeFleetEnvelope(env fleetsEnvelope) fleetsEnvelope {
	if env.Fleets == nil {
		env.Fleets = make(map[string]Fleet)
	}
	for name, fleet := range env.Fleets {
		env.Fleets[name] = normalizeFleetDefaults(fleet)
	}
	return env
}

func normalizeFleetDefaults(fleet Fleet) Fleet {
	out := copyFleet(fleet)
	for i := range out.Agents {
		if out.Agents[i].WrapTerminal == nil {
			out.Agents[i].WrapTerminal = boolPtr(true)
		}
		if out.Agents[i].AutoSetCwd == nil {
			out.Agents[i].AutoSetCwd = boolPtr(true)
		}
	}
	return out
}

func boolPtr(v bool) *bool {
	return &v
}

func fleetAgentBool(v *bool) bool {
	return v == nil || *v
}

func validateFleetName(name string) error {
	if !fleetNameRE.MatchString(name) {
		return fmt.Errorf("invalid fleet name %q: use 1-64 lowercase letters, numbers, dot, underscore, or dash; first character must be alphanumeric", name)
	}
	return nil
}

func validateFleet(name string, fleet Fleet) error {
	if err := validateFleetName(name); err != nil {
		return err
	}
	if len(fleet.Agents) > maxFleetAgents {
		return fmt.Errorf("fleet %q has too many agents (max %d)", name, maxFleetAgents)
	}
	for i, agent := range fleet.Agents {
		if strings.TrimSpace(agent.Command) == "" {
			return fmt.Errorf("fleet %q agent %d command is required", name, i)
		}
		if len([]byte(agent.Command)) > maxFleetCommandKB*1024 {
			return fmt.Errorf("fleet %q agent %d command too large (max %dKB)", name, i, maxFleetCommandKB)
		}
		if !fleetAgentBool(agent.WrapTerminal) {
			return fmt.Errorf("fleet %q agent %d: wrap_terminal must be true (v1)", name, i)
		}
	}
	return nil
}

func validateFleetsEnvelope(env fleetsEnvelope) error {
	env = normalizeFleetEnvelope(env)
	if len(env.Fleets) > maxFleets {
		return fmt.Errorf("too many fleets (max %d)", maxFleets)
	}
	if env.Version != 0 && env.Version != 1 {
		return fmt.Errorf("unsupported fleet envelope version %d (expected 0 or 1)", env.Version)
	}
	for name, fleet := range env.Fleets {
		if err := validateFleet(name, fleet); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) fleetPath() string {
	return filepath.Join(s.dir, "fleet.json")
}

func (s *store) loadFleets() {
	data, err := os.ReadFile(s.fleetPath())
	if err != nil {
		return
	}
	var env fleetsEnvelope
	if json.Unmarshal(data, &env) != nil {
		return
	}
	env = normalizeFleetEnvelope(env)
	if validateFleetsEnvelope(env) != nil {
		return
	}
	s.fleetsMu.Lock()
	s.fleets = env.Fleets
	s.fleetsMu.Unlock()
}

func (s *store) listFleets() fleetsEnvelope {
	s.fleetsMu.RLock()
	defer s.fleetsMu.RUnlock()
	out := make(map[string]Fleet, len(s.fleets))
	for name, fleet := range s.fleets {
		out[name] = copyFleet(fleet)
	}
	return fleetsEnvelope{Version: 1, Fleets: out}
}

func (s *store) getFleet(name string) (Fleet, bool) {
	s.fleetsMu.RLock()
	defer s.fleetsMu.RUnlock()
	fleet, ok := s.fleets[name]
	if !ok {
		return Fleet{}, false
	}
	return copyFleet(fleet), true
}

func (s *store) replaceFleets(env fleetsEnvelope) error {
	env = normalizeFleetEnvelope(env)
	if err := validateFleetsEnvelope(env); err != nil {
		return err
	}
	next := make(map[string]Fleet, len(env.Fleets))
	for name, fleet := range env.Fleets {
		next[name] = normalizeFleetDefaults(fleet)
	}
	s.fleetsMu.Lock()
	s.fleets = next
	s.fleetsMu.Unlock()
	s.persistFleets()
	s.broadcastFleetsUpdated()
	return nil
}

func (s *store) setFleet(name string, fleet Fleet) error {
	if err := validateFleet(name, fleet); err != nil {
		return err
	}
	s.fleetsMu.Lock()
	if _, exists := s.fleets[name]; !exists && len(s.fleets) >= maxFleets {
		s.fleetsMu.Unlock()
		return fmt.Errorf("too many fleets (max %d)", maxFleets)
	}
	s.fleets[name] = normalizeFleetDefaults(fleet)
	s.fleetsMu.Unlock()
	s.persistFleets()
	s.broadcastFleetsUpdated()
	return nil
}

func (s *store) deleteFleet(name string) bool {
	s.fleetsMu.Lock()
	if _, ok := s.fleets[name]; !ok {
		s.fleetsMu.Unlock()
		return false
	}
	delete(s.fleets, name)
	s.fleetsMu.Unlock()
	s.persistFleets()
	s.broadcastFleetsUpdated()
	return true
}

func (s *store) importFleets(env fleetsEnvelope) (fleetsEnvelope, error) {
	env = normalizeFleetEnvelope(env)
	if err := validateFleetsEnvelope(env); err != nil {
		return fleetsEnvelope{}, err
	}
	s.fleetsMu.Lock()
	if len(s.fleets)+len(env.Fleets) > maxFleets {
		s.fleetsMu.Unlock()
		return fleetsEnvelope{}, fmt.Errorf("import would exceed fleet limit (max %d)", maxFleets)
	}
	imported := make(map[string]Fleet, len(env.Fleets))
	names := make([]string, 0, len(env.Fleets))
	for name := range env.Fleets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		nextName := uniqueFleetNameLocked(s.fleets, name)
		normalized := normalizeFleetDefaults(env.Fleets[name])
		s.fleets[nextName] = normalized
		imported[nextName] = copyFleet(normalized)
	}
	s.fleetsMu.Unlock()
	s.persistFleets()
	s.broadcastFleetsUpdated()
	return fleetsEnvelope{Version: 1, Fleets: imported}, nil
}

func (s *store) clearFleets() {
	s.fleetsMu.Lock()
	s.fleets = make(map[string]Fleet)
	s.fleetsMu.Unlock()
	_ = os.Remove(s.fleetPath())
	s.broadcastFleetsUpdated()
}

func (s *store) persistFleets() {
	env := s.listFleets()
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return
	}
	atomicWriteMode(s.fleetPath(), data, 0o600)
}

func (s *store) broadcastFleetsUpdated() {
	evt := MetaEvent{Type: "fleets_updated", Data: map[string]any{}}
	s.subMu.Lock()
	for _, ch := range s.metaSubs {
		select {
		case ch <- evt:
		default:
		}
	}
	s.subMu.Unlock()
}

func copyFleet(fleet Fleet) Fleet {
	out := Fleet{Agents: make([]FleetAgent, len(fleet.Agents))}
	for i, agent := range fleet.Agents {
		out.Agents[i] = copyFleetAgent(agent)
	}
	return out
}

func copyFleetAgent(agent FleetAgent) FleetAgent {
	out := agent
	if agent.WrapTerminal != nil {
		out.WrapTerminal = boolPtr(*agent.WrapTerminal)
	}
	if agent.AutoSetCwd != nil {
		out.AutoSetCwd = boolPtr(*agent.AutoSetCwd)
	}
	return out
}

func uniqueFleetNameLocked(existing map[string]Fleet, name string) string {
	if _, ok := existing[name]; !ok {
		return name
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		base := name
		if len(base)+len(suffix) > 64 {
			base = base[:64-len(suffix)]
		}
		candidate := base + suffix
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
	}
}
