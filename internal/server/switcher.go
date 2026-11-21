package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/hrubymar10/aimebu/v2/internal/store"
	"github.com/hrubymar10/aimebu/v2/internal/switcher"
	"github.com/hrubymar10/aimebu/v2/internal/telemetry"
)

func (s *Server) switcherReady(w http.ResponseWriter) bool {
	if s.Switcher == nil {
		apiError(w, 503, "unavailable", "account switcher unavailable")
		return false
	}
	return true
}

func switcherError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, switcher.ErrProfileNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, switcher.ErrProfileExists) || errors.Is(err, switcher.ErrNoActiveProfile) || errors.Is(err, switcher.ErrActiveProfile) || errors.Is(err, switcher.ErrActiveChanged) || errors.Is(err, switcher.ErrDisabled) || errors.Is(err, switcher.ErrNotEligible) || errors.Is(err, switcher.ErrTargetCopy) || errors.Is(err, switcher.ErrMaintenanceBusy) || errors.Is(err, switcher.ErrLoginPending) || errors.Is(err, switcher.ErrIncomingRefresh) || errors.Is(err, switcher.ErrLinkedProfile) || errors.Is(err, switcher.ErrMigrationRaced) || errors.Is(err, switcher.ErrCodexRunning) || errors.Is(err, switcher.ErrInvalidCredentials) {
		status = http.StatusConflict
	}
	apiError(w, status, "switcher_error", err.Error())
}

func (s *Server) switcherImpact(r *http.Request, tool string) ([]store.SwitcherAgent, error) {
	host, err := s.Store.HostMachine(r.Context())
	if err != nil {
		return nil, err
	}
	return s.Store.SwitcherAgents(r.Context(), host.ID, tool)
}

func (s *Server) switcherStatus(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	owner, err := s.Store.IsGlobalOwner(r.Context(), userID(r))
	if err != nil {
		fail(w, err)
		return
	}
	enabled, err := s.Switcher.Enabled()
	if err != nil {
		fail(w, err)
		return
	}
	profiles, err := s.Switcher.List()
	if err != nil {
		fail(w, err)
		return
	}
	eligibility, err := s.Switcher.Eligibility()
	if err != nil {
		fail(w, err)
		return
	}
	affected := map[string][]store.SwitcherAgent{}
	for _, tool := range switcher.Tools() {
		affected[tool], err = s.switcherImpact(r, tool)
		if err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"enabled": enabled, "owner": owner, "profiles": profiles, "eligibility": eligibility, "affected_agents": affected})
}

func (s *Server) switcherEnabled(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Switcher.SetEnabled(req.Enabled); err != nil {
		switcherError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"enabled": req.Enabled})
}

func (s *Server) switcherProfile(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Switcher.Add(r.PathValue("tool"), req.Name); err != nil {
		switcherError(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"name": req.Name})
}

func (s *Server) switcherImport(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Switcher.Import(r.PathValue("tool"), req.Name); err != nil {
		switcherError(w, err)
		return
	}
	s.refreshSwitchedUsage(r.PathValue("tool"))
	writeJSON(w, 201, map[string]string{"active": req.Name})
}

func (s *Server) switcherUse(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// No new turn of the tool starts from here until the switch is over (the host's agents of the tool wait in the queue), so none can start, end and
	// park an idle warm process on the login that is about to go after the processes below were ended. The check for running turns comes after the
	// hold: a turn that was being dispatched when it was taken is recorded by then and refuses the switch.
	releaseDispatch := s.Store.HoldDispatch(r.PathValue("tool"))
	defer releaseDispatch()
	if !s.switcherQuiescent(w, r, r.PathValue("tool")) {
		return
	}
	// Agents kept warm hold the login they started with: they are ended, and confirmed gone, before the swap. Their turns are held by the
	// switch's own guard below too, which is taken right before the swap and covers the swap itself.
	releaseWarm, err := s.retireHostWarm(r.Context(), r.PathValue("tool"))
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": "warm_agents", "message": err.Error()})
		return
	}
	defer releaseWarm() // cache pings of the tool stay held back until the switch is over, whatever it came to
	if s.switcherAfterPreCheck != nil {
		s.switcherAfterPreCheck()
	}
	// The check above is an early refusal: the switch can wait up to 45s for the account lock and the incoming
	// refresh, and a turn may start meanwhile. Quiescence is checked again under the switcher lock.
	switchGuard := switcher.SwitchGuard{Quiet: s.quietGuard(r.Context(), r.PathValue("tool")), Hold: s.Store.HoldTurns}
	var active string
	var changed bool
	if req.Force {
		active, changed, err = s.Switcher.SwitchForceGuarded(r.PathValue("tool"), req.Name, switchGuard)
	} else {
		active, changed, err = s.Switcher.SwitchGuarded(r.PathValue("tool"), req.Name, switchGuard)
	}
	var running agentsRunningError
	if errors.As(err, &running) {
		writeJSON(w, 409, map[string]any{"error": "agents_running", "message": "wait for affected agents to finish or stop them before switching", "agents": running.agents})
		return
	}
	var guard guardFailure
	if errors.As(err, &guard) {
		fail(w, guard.err)
		return
	}
	if err != nil {
		switcherError(w, err)
		return
	}
	if changed {
		s.refreshSwitchedUsage(r.PathValue("tool"))
	}
	writeJSON(w, 200, map[string]any{"active": active, "switched": changed})
}

func (s *Server) switcherRemove(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	tool, name := r.PathValue("tool"), r.PathValue("name")
	active, err := s.Switcher.Active(tool)
	if err != nil {
		switcherError(w, err)
		return
	}
	if active == name {
		// Removing the active profile deletes the live login the host's warm processes of the tool hold: same as a switch away from it.
		releaseDispatch := s.Store.HoldDispatch(tool)
		defer releaseDispatch()
		if !s.switcherQuiescent(w, r, tool) {
			return
		}
		releaseWarm, err := s.retireHostWarm(r.Context(), tool)
		if err != nil {
			writeJSON(w, 409, map[string]any{"error": "warm_agents", "message": err.Error()})
			return
		}
		defer releaseWarm()
	}
	if err := s.Switcher.Remove(tool, name, active == name); err != nil {
		switcherError(w, err)
		return
	}
	if active == name {
		s.refreshSwitchedUsage(tool)
	} else if s.Usages != nil {
		s.Usages.ForgetProfile(usageProvider(tool), name)
	}
	w.WriteHeader(204)
}

// switcherRename renames a profile in place. The live credential file is not
// touched, so running harnesses are unaffected and no quiescence check applies.
func (s *Server) switcherRename(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) || !s.switcherReady(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tool, oldName := r.PathValue("tool"), r.PathValue("name")
	if err := s.Switcher.Rename(tool, oldName, req.Name); err != nil {
		switcherError(w, err)
		return
	}
	s.refreshSwitchedUsage(tool)
	writeJSON(w, 200, map[string]string{"name": req.Name})
}

// agentsRunningError is what the switcher's quiescence guard returns when agents of the tool are running.
type agentsRunningError struct{ agents []store.SwitcherAgent }

func (agentsRunningError) Error() string { return "agents of the tool are running" }

// guardFailure wraps an error from the quiescence check itself (a store failure), so a handler can tell it from a
// switcher refusal and answer it as an internal error instead of echoing its text as a bad request.
type guardFailure struct{ err error }

func (e guardFailure) Error() string { return e.err.Error() }
func (e guardFailure) Unwrap() error { return e.err }

// quietGuard is SwitchGuard.Quiet for tool: it runs under the switcher lock, right before the swap.
func (s *Server) quietGuard(ctx context.Context, tool string) func() error {
	return func() error {
		running, err := s.runningAgents(ctx, tool)
		if err != nil {
			return guardFailure{err}
		}
		if len(running) > 0 {
			return agentsRunningError{agents: running}
		}
		return nil
	}
}

func (s *Server) refreshSwitchedUsage(tool string) {
	if s.Usages == nil {
		return
	}
	s.Usages.Invalidate(usageProvider(tool))
	s.usageRefreshes.Add(1)
	// A refresh runs while the request is answered: a panic in it is reported (when telemetry is on) and raised
	// again, so the process dies as it always did (telemetry.Go).
	telemetry.Go(context.Background(), func() {
		defer s.usageRefreshes.Done()
		s.Usages.Refresh(context.Background())
	})
}

// WaitUsageRefreshes blocks until the usage refreshes that switches started in the background have finished. A refresh writes the usage
// cache into the usages directory, so whoever is about to remove or replace that directory (a test's cleanup) waits first; a refresh that was
// not waited for can recreate the directory, or land a file in it while it is being removed ("directory not empty").
func (s *Server) WaitUsageRefreshes() { s.usageRefreshes.Wait() }

func (s *Server) switcherQuiescent(w http.ResponseWriter, r *http.Request, tool string) bool {
	running, err := s.runningAgents(r.Context(), tool)
	if err != nil {
		fail(w, err)
		return false
	}
	if len(running) > 0 {
		writeJSON(w, 409, map[string]any{"error": "agents_running", "message": "wait for affected agents to finish or stop them before switching", "agents": running})
		return false
	}
	return true
}

// runningAgents lists the agents of tool on the host that a login swap would hit and
// that are in a turn (or being interrupted). The web switch and the automatic quota
// switcher share it, so both wait for the same thing.
func (s *Server) runningAgents(ctx context.Context, tool string) ([]store.SwitcherAgent, error) {
	host, err := s.Store.HostMachine(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := s.Store.SwitcherAgents(ctx, host.ID, tool)
	if err != nil {
		return nil, err
	}
	running := []store.SwitcherAgent{}
	for _, agent := range agents {
		if agent.Running || agent.Observed == "in_turn" || agent.Observed == "interrupting" {
			running = append(running, agent)
		}
	}
	return running, nil
}

// usageProvider maps a switcher tool to its usage provider name.
func usageProvider(tool string) string {
	if tool == switcher.ToolClaude {
		return "claude-code"
	}
	return tool
}
