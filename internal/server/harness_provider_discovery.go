package server

import (
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

// discoverOpenCodeProviders reads the machine's own OpenCode config during a model sync.
// Saved providers remain the owner's override; only new, usable providers are saved.
// The request uses config-listed models rather than old saved model rows so a reload
// can remove detected models that disappeared from both config and endpoint.
func (s *Server) discoverOpenCodeProviders(r *http.Request, conn *machineConn, harness store.Launcher, custom []adapter.CustomProvider) ([]adapter.CustomProvider, []string, error) {
	answer, err := s.requestDetect(r.Context(), conn, protocol.DetectRequest{Kind: "provider_import", LauncherID: harness.ID}, detectKeysWait)
	if err != nil || answer.Code != "" {
		return custom, []string{"Could not read the OpenCode config; saved providers were used."}, nil
	}
	var imported protocol.ProviderImportAnswer
	if err := json.Unmarshal(answer.Details, &imported); err != nil {
		return custom, []string{"Could not read the OpenCode config; saved providers were used."}, nil
	}
	current, err := s.Store.HarnessProviders(r.Context(), harness.ID)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]int, len(custom))
	for i := range custom {
		byID[custom[i].ID] = i
		custom[i].Models = nil
	}
	providers := append([]store.HarnessProvider(nil), current...)
	warnings := []string{}
	for _, suggestion := range imported.Suggestions {
		p := suggestion.Provider
		if suggestion.InlineKey {
			warnings = append(warnings, "Provider "+p.ID+" has an inline key in the OpenCode config; switch it to {env:NAME} or add the key in Providers.")
			continue
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		stored := store.HarnessProvider{ID: p.ID, Name: p.Name, NPM: p.NPM, BaseURL: p.BaseURL, KeyEnv: p.KeyEnv, ContextTokens: p.ContextTokens, OutputTokens: p.OutputTokens}
		if err := store.ValidateHarnessProviders("opencode", []store.HarnessProvider{stored}); err != nil {
			warnings = append(warnings, "Provider "+p.ID+" was skipped: "+err.Error())
			continue
		}
		models := make([]adapter.CustomProviderModel, 0, len(p.Models))
		for _, model := range p.Models {
			if store.ValidHarnessModelSlug(p.ID + "/" + model.ID) {
				models = append(models, model)
			}
		}
		if index, exists := byID[p.ID]; exists {
			custom[index].Models = models
			continue
		}
		if len(providers) >= 16 {
			warnings = append(warnings, "Provider "+p.ID+" was skipped: this harness has the maximum number of providers.")
			continue
		}
		providers = append(providers, stored)
		p.Models = models
		custom = append(custom, p)
		byID[p.ID] = len(custom) - 1
	}
	if len(providers) > len(current) {
		if err := s.Store.ReplaceHarnessProvidersIfRev(r.Context(), userID(r), harness.MachineID, harness.ID, providers, store.HarnessProvidersRev(current), false); err != nil {
			return nil, nil, err
		}
	}
	for _, skipped := range imported.Skipped {
		if skipped.ID != "" && skipped.Reason != "" && !strings.Contains(skipped.Reason, "key") {
			warnings = append(warnings, "Provider "+skipped.ID+" was skipped: "+skipped.Reason)
		}
	}
	return custom, warnings, nil
}
