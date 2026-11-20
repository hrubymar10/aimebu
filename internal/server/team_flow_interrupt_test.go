package server

import (
	"net/http"
	"testing"
)

// The interrupt endpoint of a running team flow change answers a request with no agent, and one while no change is running, with the
// documented errors instead of touching any agent.
func TestTeamFlowMigrationInterruptRefusals(t *testing.T) {
	f := newLegacyFixture(t)
	path := "/api/projects/" + f.project.ID + "/team-flow/migration/interrupt"
	if resp := f.request(http.MethodPost, path, map[string]any{}, true); resp.StatusCode != 400 {
		t.Fatalf("no agent_id: %d", resp.StatusCode)
	}
	if resp := f.request(http.MethodPost, path, map[string]any{"agent_id": "agt_none"}, true); resp.StatusCode != 404 {
		t.Fatalf("no migration: %d", resp.StatusCode)
	}
}
