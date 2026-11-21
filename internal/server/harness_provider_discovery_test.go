package server

import (
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"

	"github.com/hrubymar10/aimebu/v2/internal/adapter"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
	"github.com/hrubymar10/aimebu/v2/internal/store"
)

func TestOpenCodeSyncImportsConfigProvidersAndModels(t *testing.T) {
	f := newFixture(t)
	f.seed()
	harness := f.harness(t, "opencode", "native")
	base := "/api/machines/" + f.machineID + "/launchers/" + harness.ID + "/models/sync"
	var second atomic.Bool
	imported := protocol.ProviderImportAnswer{Suggestions: []protocol.ProviderSuggestion{
		{Provider: adapter.CustomProvider{ID: "local-api", Name: "Local API", NPM: adapter.CustomProviderNPM, BaseURL: "http://127.0.0.1:8080/v1", Models: []adapter.CustomProviderModel{{ID: "configured", Name: "Configured"}}}},
		{Provider: adapter.CustomProvider{ID: "inline-api", Name: "Inline API", NPM: adapter.CustomProviderNPM, BaseURL: "http://127.0.0.1:8081/v1"}, InlineKey: true},
	}}
	fakeProviderMachine(t, f, harness.ID, true, func(request protocol.DetectRequest) protocol.Frame {
		switch request.Kind {
		case "provider_import":
			details, _ := json.Marshal(imported)
			return protocol.Frame{Details: details}
		case "models":
			if len(request.Providers) != 1 || request.Providers[0].ID != "local-api" || len(request.Providers[0].Models) != 1 || request.Providers[0].Models[0].ID != "configured" {
				t.Errorf("the private listing should use config models, got %+v", request.Providers)
			}
			models := []protocol.ModelCandidate{{Slug: "local-api/configured", Label: "Configured"}, {Slug: "local-api/served", Label: "Served"}}
			if second.Load() {
				models = models[:1]
			}
			return protocol.Frame{ModelCandidates: models}
		}
		return protocol.Frame{Code: "invalid_request"}
	})
	code, out := jsonCall(t, f, "POST", base, map[string]any{})
	if code != 200 || out["added"] != float64(2) || len(out["provider_warnings"].([]any)) != 1 {
		t.Fatalf("first sync: %d %+v", code, out)
	}
	providers, err := f.db.HarnessProviders(t.Context(), harness.ID)
	if err != nil || len(providers) != 1 || providers[0].ID != "local-api" {
		t.Fatalf("saved providers: %+v, %v", providers, err)
	}
	second.Store(true)
	code, out = jsonCall(t, f, "POST", base, map[string]any{})
	if code != 200 || out["removed"] != float64(1) {
		t.Fatalf("second sync: %d %+v", code, out)
	}
	models, err := f.db.HarnessModels(t.Context(), harness.ID)
	if err != nil || len(models) != 2 || models[0].Source != "manual" || models[1].Slug != "local-api/configured" || models[1].Source != "detected" {
		t.Fatalf("saved models: %+v, %v", models, err)
	}
	if err := store.ValidateHarnessProviders("opencode", providers); err != nil {
		t.Fatal(err)
	}
}
