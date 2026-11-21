package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func launcherDefinition(l store.Launcher) protocol.LauncherDefinition {
	maps := make([]protocol.LauncherPathMap, 0, len(l.PathMaps))
	for _, mapping := range l.PathMaps {
		maps = append(maps, protocol.LauncherPathMap{Host: mapping.Host, Visible: mapping.Visible})
	}
	return protocol.LauncherDefinition{
		ID: l.ID, Tool: l.Tool, Name: l.Name, Command: l.Command,
		Args: l.Args, EnvRefs: l.EnvRefs, Enabled: l.Enabled,
		Containerized: l.Containerized, AgentServerURL: l.AgentServerURL,
		PathMaps: maps, VisibleRoots: l.VisibleRoots, Toolbox: l.Toolbox,
	}
}

func (s *Server) requestLauncher(ctx context.Context, conn *machineConn, kind string, definition protocol.LauncherDefinition) (protocol.Frame, error) {
	reqID := fmt.Sprintf("launcher-%d", time.Now().UnixNano())
	result := make(chan protocol.Frame, 1)
	conn.harnessMu.Lock()
	conn.launcherPending[reqID] = result
	conn.harnessMu.Unlock()
	defer func() { conn.harnessMu.Lock(); delete(conn.launcherPending, reqID); conn.harnessMu.Unlock() }()
	if err := conn.send(protocol.Frame{V: protocol.Version, Type: kind, ReqID: reqID, LauncherDefinition: &definition}); err != nil {
		return protocol.Frame{}, err
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case answer := <-result:
		return answer, nil
	case <-conn.done:
		return protocol.Frame{}, errors.New("machine disconnected")
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	case <-timer.C:
		return protocol.Frame{}, errors.New("supervisor did not answer launcher request")
	}
}

func (s *Server) validateLauncherCheckout(ctx context.Context, machine store.Machine, launcherID, path string) error {
	if launcherID == "" {
		return nil
	}
	if machine.IsHost {
		launcher, err := s.Store.LauncherByID(ctx, machine.ID, launcherID)
		if err != nil {
			return err
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || canonical != path {
			return fmt.Errorf("checkout path is not canonical")
		}
		allowed := len(launcher.VisibleRoots) == 0
		for _, root := range launcher.VisibleRoots {
			if store.WithinFolder(root, canonical) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("checkout is outside launcher visible roots")
		}
		verified, err := s.probeMachinePathForLauncher(ctx, machine.ID, path, launcherID)
		if err != nil {
			return err
		}
		if verified != path {
			return fmt.Errorf("checkout path changed on host machine")
		}
		return nil
	}
	canonical, err := s.probeMachinePathForLauncher(ctx, machine.ID, path, launcherID)
	if err != nil {
		return err
	}
	if canonical != path {
		return fmt.Errorf("checkout path changed on remote machine")
	}
	return nil
}

func (s *Server) machineLaunchers(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.Store.MachineByID(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	} else if err != nil {
		fail(w, err)
		return
	}
	definitions, err := s.Store.MachineLaunchers(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	useCounts, err := s.Store.LauncherUseCounts(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	models, err := s.Store.MachineHarnessModels(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	providers, err := s.Store.MachineHarnessProviders(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	rows := make([]map[string]any, 0, len(definitions))
	observed := map[string]protocol.LauncherRow{}
	if conn, err := s.machineForID(id); err == nil {
		conn.harnessMu.Lock()
		for _, row := range conn.launchers {
			observed[row.ID] = row
		}
		conn.harnessMu.Unlock()
	}
	for _, definition := range definitions {
		row := map[string]any{"config": definition, "available": false, "unavailable_reason": "machine offline", "active_agents": useCounts[definition.ID], "models": models[definition.ID]}
		if row["models"] == nil {
			row["models"] = []store.HarnessModel{}
		}
		row["models_rev"] = store.HarnessModelsRev(models[definition.ID])
		row["providers"] = providers[definition.ID]
		if len(providers[definition.ID]) == 0 {
			row["providers"] = []store.HarnessProvider{}
		}
		row["providers_rev"] = store.HarnessProvidersRev(providers[definition.ID])
		if report, ok := observed[definition.ID]; ok {
			row["available"] = report.Available
			row["version"] = report.Version
			row["resolved_command"] = report.ResolvedCommand
			row["unavailable_reason"] = report.UnavailableReason
			if report.Features != nil {
				row["features"] = report.Features
			}
			if report.FeatureProblem != "" {
				row["feature_problem"] = report.FeatureProblem
			}
			if report.VersionProblem != "" && report.Version == "" {
				row["version_problem"] = report.VersionProblem
			}
		}
		rows = append(rows, row)
	}
	activity, err := s.Store.LauncherActivity(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"machine_id": id, "launchers": rows, "activity": activity})
}

// machineLauncher saves or removes a launcher: the command a machine runs for an agent. That is code on the host, so it is the owner's alone; an admin
// the owner appointed can see launchers (machineLaunchers) but not change them.
func (s *Server) machineLauncher(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	machineID, launcherID := r.PathValue("id"), r.PathValue("launcher")
	machine, err := s.Store.MachineByID(r.Context(), machineID)
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "machine not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if !machine.Approved || machine.RevokedAt.Valid {
		apiError(w, 409, "machine_unavailable", "machine is not approved")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.Store.DeleteLauncher(r.Context(), machineID, launcherID, userID(r)); err != nil {
			s.launcherMutationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "removed"})
		return
	}
	var item store.Launcher
	if !readJSON(w, r, &item) {
		return
	}
	item.MachineID = machineID
	if r.Method == http.MethodPut {
		item.ID = launcherID
		old, err := s.Store.LauncherByID(r.Context(), machineID, launcherID)
		if err != nil {
			s.launcherMutationError(w, err)
			return
		}
		item.Preset = old.Preset
		if item.Label == "" { // an older caller that does not know labels keeps the one the owner set
			item.Label = old.Label
		}
		if old.Preset && (item.Tool != old.Tool || item.Name != old.Name) {
			apiError(w, 400, "invalid_launcher", "preset tool and name cannot change")
			return
		}
	} else {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			fail(w, err)
			return
		}
		item.ID = "mln_" + hex.EncodeToString(nonce[:])
		item.Preset = false
		if item.Tool == "claude" || item.Tool == "codex" || item.Tool == "pi" || item.Tool == "opencode" || item.Tool == "vibe" {
			if item.Label == "" {
				item.Label = defaultHarnessLabel(item.Tool, item.Containerized)
			}
			if item.Name == "" {
				name, err := s.newHarnessName(r.Context(), machineID, item)
				if err != nil {
					fail(w, err)
					return
				}
				item.Name = name
			}
		}
	}
	if item.Tool != "claude" && item.Tool != "codex" && item.Tool != "pi" && item.Tool != "opencode" && item.Tool != "vibe" {
		apiError(w, 400, "invalid_launcher", "tool must be claude, codex, pi, opencode, or vibe")
		return
	}
	if used, err := s.Store.LauncherInUse(r.Context(), item.ID); err != nil {
		fail(w, err)
		return
	} else if used {
		apiError(w, 409, "launcher_in_use", "stop agents using this launcher before editing it")
		return
	}
	conn, err := s.machineForID(machineID)
	if err != nil || !conn.supportsLaunchers {
		apiError(w, 503, "machine_unavailable", "machine needs an updated connected supervisor")
		return
	}
	definition := launcherDefinition(item)
	conn.harnessSetMu.Lock()
	defer conn.harnessSetMu.Unlock()
	checked, err := s.requestLauncher(r.Context(), conn, protocol.LauncherValidate, definition)
	if err != nil {
		apiError(w, 503, "machine_unavailable", err.Error())
		return
	}
	if checked.Code != "" || checked.Launcher == nil {
		apiError(w, 400, "invalid_launcher", checked.Message)
		return
	}
	item, err = s.Store.SaveLauncher(r.Context(), userID(r), item)
	if err != nil {
		s.launcherMutationError(w, err)
		return
	}
	answer, err := s.requestLauncher(r.Context(), conn, protocol.LauncherSet, definition)
	if err != nil {
		apiError(w, 503, "machine_unavailable", "launcher saved; supervisor will apply it on reconnect: "+err.Error())
		return
	}
	if answer.Code != "" || answer.Launcher == nil {
		apiError(w, 503, "machine_unavailable", "launcher saved; supervisor could not apply it: "+answer.Message)
		return
	}
	conn.harnessMu.Lock()
	updated := false
	for i := range conn.launchers {
		if conn.launchers[i].ID == item.ID {
			conn.launchers[i] = *answer.Launcher
			updated = true
			break
		}
	}
	if !updated {
		conn.launchers = append(conn.launchers, *answer.Launcher)
	}
	conn.harnessMu.Unlock()
	writeJSON(w, 200, map[string]any{"config": item, "observed": answer.Launcher})
}

func (s *Server) launcherMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrLauncherInUse):
		apiError(w, 409, "launcher_in_use", "stop agents using this launcher first")
	case errors.Is(err, store.ErrConflict):
		apiError(w, 409, "launcher_conflict", "launcher name or ID already exists")
	case errors.Is(err, sql.ErrNoRows):
		apiError(w, 404, "not_found", "launcher not found")
	default:
		fail(w, err)
	}
}

func defaultHarnessLabel(tool string, containerized bool) string {
	label := map[string]string{"claude": "Claude Code", "codex": "Codex", "pi": "Pi", "opencode": "OpenCode", "vibe": "Mistral Vibe"}[tool]
	if containerized {
		label += " (Docker)"
	}
	return label
}

var harnessNameUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// newHarnessName picks the internal name of a new harness, which the spawn API's "variant" resolves against: "native" or "harness-docker"
// when this machine has no such harness for the tool yet, otherwise a slug of the label with a number to make it unique.
func (s *Server) newHarnessName(ctx context.Context, machineID string, item store.Launcher) (string, error) {
	existing, err := s.Store.MachineLaunchers(ctx, machineID)
	if err != nil {
		return "", err
	}
	taken := func(name string) bool {
		for _, other := range existing {
			if other.Tool == item.Tool && other.Name == name {
				return true
			}
		}
		return name == "docker"
	}
	base := "native"
	if item.Containerized {
		base = "harness-docker"
	}
	if !taken(base) {
		return base, nil
	}
	slug := strings.Trim(harnessNameUnsafe.ReplaceAllString(strings.ToLower(item.Label), "-"), "-")
	if slug == "" {
		slug = base
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	name := slug
	for n := 2; taken(name); n++ {
		name = fmt.Sprintf("%s-%d", slug, n)
	}
	return name, nil
}

// machineHarnessModels replaces a harness's whole model list (owner or admin). The list is validated against the harness's family and has
// exactly one default; a refused list changes nothing. With expected_rev (the models_rev of the list the edit was based on) an edit over a
// list that has changed since is refused with 409 stale_models; without it the list is replaced unconditionally.
func (s *Server) machineHarnessModels(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	machineID, launcherID := r.PathValue("id"), r.PathValue("launcher")
	var body struct {
		Models      []store.HarnessModel `json:"models"`
		ExpectedRev string               `json:"expected_rev"`
	}
	if !readJSONLimit(w, r, &body, 2<<20) {
		return
	}
	if body.Models == nil {
		body.Models = []store.HarnessModel{}
	}
	if err := s.Store.ReplaceHarnessModelsIfRev(r.Context(), userID(r), machineID, launcherID, body.Models, body.ExpectedRev); err != nil {
		switch {
		case errors.Is(err, store.ErrStaleHarnessModels):
			apiError(w, 409, "stale_models", "someone changed this harness's models in the meantime; reload the list and make the change again")
		case errors.Is(err, store.ErrInvalidHarnessModels):
			apiError(w, 400, "invalid_models", strings.TrimPrefix(err.Error(), store.ErrInvalidHarnessModels.Error()+": "))
		case errors.Is(err, sql.ErrNoRows):
			apiError(w, 404, "not_found", "harness not found")
		default:
			fail(w, err)
		}
		return
	}
	models, err := s.Store.HarnessModels(r.Context(), launcherID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"models": models, "models_rev": store.HarnessModelsRev(models)})
}

// machineHarnessProviders replaces an OpenCode harness's whole list of custom providers (owner or admin), on the same terms as the model
// list: validated as a whole, atomic, and with expected_rev (the providers_rev of the list the edit was based on) a list that has changed
// since is refused with 409 stale_providers. A provider that is dropped while models of the harness still start with "<id>/" is refused
// with 409 provider_in_use (naming every such provider and its number of models) unless remove_models is true, which deletes those models in the
// same transaction. The answer carries both lists, since the models may have changed. Only names of key variables are stored: no key
// ever passes through here.
func (s *Server) machineHarnessProviders(w http.ResponseWriter, r *http.Request) {
	if !s.ownerMachine(w, r) {
		return
	}
	machineID, launcherID := r.PathValue("id"), r.PathValue("launcher")
	var body struct {
		Providers    []store.HarnessProvider `json:"providers"`
		ExpectedRev  string                  `json:"expected_rev"`
		RemoveModels bool                    `json:"remove_models"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Providers == nil {
		body.Providers = []store.HarnessProvider{}
	}
	if err := s.Store.ReplaceHarnessProvidersIfRev(r.Context(), userID(r), machineID, launcherID, body.Providers, body.ExpectedRev, body.RemoveModels); err != nil {
		var inUse *store.HarnessProviderInUseError
		switch {
		case errors.Is(err, store.ErrStaleHarnessProviders):
			apiError(w, 409, "stale_providers", "someone changed this harness's providers in the meantime; reload the list and make the change again")
		case errors.As(err, &inUse):
			// The first provider keeps the original fields; in_use lists every one the replace would drop with models, as remove_models would delete them all.
			uses := make([]map[string]any, 0, len(inUse.All))
			for _, use := range inUse.All {
				uses = append(uses, map[string]any{"provider_id": use.ProviderID, "model_count": use.Models})
			}
			writeJSON(w, 409, map[string]any{"error": "provider_in_use", "message": err.Error() + "; remove those models first, or remove the providers together with them",
				"provider_id": inUse.ProviderID, "model_count": inUse.Models, "in_use": uses})
		case errors.Is(err, store.ErrInvalidHarnessProviders):
			apiError(w, 400, "invalid_providers", strings.TrimPrefix(err.Error(), store.ErrInvalidHarnessProviders.Error()+": "))
		case errors.Is(err, sql.ErrNoRows):
			apiError(w, 404, "not_found", "harness not found")
		default:
			fail(w, err)
		}
		return
	}
	s.writeHarnessProviders(w, r, launcherID)
}

// machineHarnessProvidersGet answers a harness's providers and models with their revisions.
func (s *Server) machineHarnessProvidersGet(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	launcher, err := s.Store.LauncherByID(r.Context(), r.PathValue("id"), r.PathValue("launcher"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "harness not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.writeHarnessProviders(w, r, launcher.ID)
}

func (s *Server) writeHarnessProviders(w http.ResponseWriter, r *http.Request, launcherID string) {
	providers, err := s.Store.HarnessProviders(r.Context(), launcherID)
	if err != nil {
		fail(w, err)
		return
	}
	models, err := s.Store.HarnessModels(r.Context(), launcherID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"providers": providers, "providers_rev": store.HarnessProvidersRev(providers), "models": models, "models_rev": store.HarnessModelsRev(models)})
}

// checkSpawnModel resolves the model an agent will start with: the harness it runs on (the launcher chosen, or the machine's launcher
// named like the variant) must list it, and an empty model means that harness's default. Without a harness (the fake test adapter, or a
// machine that has none stored) only the id's shape is checked.
func (s *Server) checkSpawnModel(ctx context.Context, machineID, tool, variant, launcherID, model string) (string, bool, error) {
	if tool == "fake" {
		return model, true, nil
	}
	if launcherID == "" {
		launcher, err := s.Store.LauncherByName(ctx, machineID, tool, variant)
		if err == nil {
			launcherID = launcher.ID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	if launcherID != "" {
		return s.modelListedByHarness(ctx, launcherID, model)
	}
	return model, model != "" && store.ValidHarnessModelSlug(model), nil
}
