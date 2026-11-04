package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/internal/protocol"
	"github.com/hrubymar10/aimebu/internal/store"
)

// providerConn finds the connected supervisor of a machine that knows custom providers, or answers why not.
func (s *Server) providerConn(w http.ResponseWriter, machineID string) (*machineConn, bool) {
	conn, ok := s.detectConn(w, machineID)
	if !ok {
		return nil, false
	}
	if !conn.supportsProviders {
		apiError(w, 503, "machine_unavailable", "the machine needs an updated supervisor to work with custom providers")
		return nil, false
	}
	return conn, true
}

// openCodeHarness loads the harness of a request and answers 404 or 400 when it is not an OpenCode harness of that machine.
func (s *Server) openCodeHarness(w http.ResponseWriter, r *http.Request) (store.Launcher, bool) {
	harness, err := s.Store.LauncherByID(r.Context(), r.PathValue("id"), r.PathValue("launcher"))
	if errors.Is(err, sql.ErrNoRows) {
		apiError(w, 404, "not_found", "harness not found")
		return harness, false
	}
	if err != nil {
		fail(w, err)
		return harness, false
	}
	if harness.Tool != "opencode" {
		apiError(w, 400, "invalid_providers", "custom providers are only for OpenCode harnesses")
		return harness, false
	}
	return harness, true
}

// machineProviderKeys answers, for each provider of an OpenCode harness that names a key variable, whether the variable has a value on the
// machine that runs the turns: `set` is true or false, and null while that cannot be known (the machine is offline, or its supervisor
// predates providers, or it did not answer). Only whether: no key value ever reaches the server.
func (s *Server) machineProviderKeys(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	harness, ok := s.openCodeHarness(w, r)
	if !ok {
		return
	}
	providers, err := s.Store.HarnessProviders(r.Context(), harness.ID)
	if err != nil {
		fail(w, err)
		return
	}
	type keyStatus struct {
		ProviderID string `json:"provider_id"`
		KeyEnv     string `json:"key_env"`
		Set        *bool  `json:"set"`
	}
	statuses := []keyStatus{}
	var names []string
	for _, p := range providers {
		if p.KeyEnv != "" {
			statuses = append(statuses, keyStatus{ProviderID: p.ID, KeyEnv: p.KeyEnv})
			names = append(names, p.KeyEnv)
		}
	}
	answer := map[string]any{"machine_online": false, "supervisor_supported": false, "keys": statuses}
	if conn, err := s.machineForID(harness.MachineID); err == nil {
		answer["machine_online"] = true
		answer["supervisor_supported"] = conn.supportsProviders && conn.supportsDetect
		if conn.supportsProviders && conn.supportsDetect && len(names) > 0 {
			reply, err := s.requestDetect(r.Context(), conn, protocol.DetectRequest{Kind: "provider_keys", LauncherID: harness.ID, KeyNames: names}, detectKeysWait)
			var keys protocol.ProviderKeysAnswer
			if err == nil && reply.Code == "" && json.Unmarshal(reply.Details, &keys) == nil {
				for i := range statuses {
					if set, known := keys.Set[statuses[i].KeyEnv]; known {
						statuses[i].Set = &set
					}
				}
			} else if err != nil {
				answer["error"] = "the supervisor did not answer"
			}
		}
	}
	writeJSON(w, 200, answer)
}

// machineProviderImport asks the machine's supervisor for the custom providers the person's own OpenCode config defines and answers them as
// suggestions for the harness, each checked against the rules a saved provider must meet (`problem` says why one cannot be taken as it
// is) and marked `already_added` when the harness has that id. Nothing is saved, and no key is ever copied: a key that is a reference to a
// variable arrives as that variable's name, and a key written into the config as text only sets `inline_key`.
func (s *Server) machineProviderImport(w http.ResponseWriter, r *http.Request) {
	if !s.adminMachine(w, r) {
		return
	}
	harness, ok := s.openCodeHarness(w, r)
	if !ok {
		return
	}
	conn, ok := s.providerConn(w, harness.MachineID)
	if !ok {
		return
	}
	reply, err := s.requestDetect(r.Context(), conn, protocol.DetectRequest{Kind: "provider_import", LauncherID: harness.ID}, detectKeysWait)
	if err != nil {
		apiError(w, 502, "import_failed", "Couldn't read the OpenCode config on that machine: "+err.Error())
		return
	}
	if reply.Code != "" {
		apiError(w, 502, "import_failed", "Couldn't read the OpenCode config on that machine: "+strings.TrimRight(reply.Message, "."))
		return
	}
	var imported protocol.ProviderImportAnswer
	if err := json.Unmarshal(reply.Details, &imported); err != nil {
		apiError(w, 502, "import_failed", "The supervisor's answer could not be read")
		return
	}
	have, err := s.Store.HarnessProviders(r.Context(), harness.ID)
	if err != nil {
		fail(w, err)
		return
	}
	added := map[string]bool{}
	for _, p := range have {
		added[p.ID] = true
	}
	type modelSuggestion struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
	}
	type suggestion struct {
		Provider     store.HarnessProvider `json:"provider"`
		Models       []modelSuggestion     `json:"models"`
		InlineKey    bool                  `json:"inline_key,omitempty"`
		Problem      string                `json:"problem,omitempty"`
		AlreadyAdded bool                  `json:"already_added,omitempty"`
	}
	suggestions := []suggestion{}
	for _, item := range imported.Suggestions {
		p := store.HarnessProvider{ID: item.Provider.ID, Name: item.Provider.Name, NPM: item.Provider.NPM, BaseURL: item.Provider.BaseURL, KeyEnv: item.Provider.KeyEnv,
			ContextTokens: item.Provider.ContextTokens, OutputTokens: item.Provider.OutputTokens}
		if p.Name == "" {
			p.Name = p.ID
		}
		entry := suggestion{Provider: p, Models: []modelSuggestion{}, InlineKey: item.InlineKey, AlreadyAdded: added[p.ID]}
		if err := store.ValidateHarnessProviders("opencode", []store.HarnessProvider{p}); err != nil {
			entry.Problem = err.Error()
		}
		for _, model := range item.Provider.Models {
			slug := p.ID + "/" + model.ID
			if store.ValidHarnessModelSlug(slug) {
				label := model.Name
				if label == "" {
					label = model.ID
				}
				entry.Models = append(entry.Models, modelSuggestion{Slug: slug, Label: truncateLabel(label)})
			}
		}
		suggestions = append(suggestions, entry)
	}
	skipped := imported.Skipped
	if skipped == nil {
		skipped = []protocol.ProviderSkipped{}
	}
	writeJSON(w, 200, map[string]any{"source": imported.Source, "suggestions": suggestions, "skipped": skipped})
}

func truncateLabel(label string) string {
	if runes := []rune(label); len(runes) > 80 {
		return string(runes[:80])
	}
	return label
}
