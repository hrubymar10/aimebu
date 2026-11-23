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
	// A switch never waits for agents: a running turn keeps the token it holds, and the swap itself keeps what protects the credentials (the account
	// lock, the incoming refresh and, for Claude, its own refresh locks). The warm processes of the tool are ended in the background afterwards.
	var active string
	var changed bool
	var err error
	if req.Force {
		active, changed, err = s.Switcher.SwitchForceGuarded(r.PathValue("tool"), req.Name, switcher.SwitchGuard{})
	} else {
		active, changed, err = s.Switcher.SwitchGuarded(r.PathValue("tool"), req.Name, switcher.SwitchGuard{})
	}
	if err != nil {
		switcherError(w, err)
		return
	}
	if changed {
		s.accountSwitched(r.Context(), r.PathValue("tool"))
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
		// Removing the active profile deletes the live login: like a switch, it does not wait for agents, and the warm processes of the tool
		// are ended in the background afterwards.
		defer s.retireWarmAfterSwitch(tool)
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
func (s *Server) WaitUsageRefreshes() {
	s.switchBackgroundContext() // so cancelling below always has a context to cancel
	s.switchBgMu.Lock()
	cancel := s.switchBgCancel
	s.switchBgMu.Unlock()
	cancel() // the waits of a switch's background work end here: a Codex check does not outlive its test
	s.switchBg.Wait()
	s.usageRefreshes.Wait()
}

// usageProvider maps a switcher tool to its usage provider name.
func usageProvider(tool string) string {
	if tool == switcher.ToolClaude {
		return "claude-code"
	}
	return tool
}
