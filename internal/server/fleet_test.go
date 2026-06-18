package server

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestFleetsPersistInSQLiteDBWith0600Mode(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.setFleet("default", Fleet{Agents: []FleetAgent{{Command: "echo ${AIMEBU_FLEET_PATH}"}}}); err != nil {
		t.Fatalf("setFleet: %v", err)
	}

	info, err := os.Stat(s.sqlitePath())
	if err != nil {
		t.Fatalf("stat sqlite db: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("sqlite db mode = %o, want 600", got)
	}

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	fleet, ok := reloaded.getFleet("default")
	if !ok {
		t.Fatal("reloaded fleet not found")
	}
	if got := fleet.Agents[0].Command; got != "echo ${AIMEBU_FLEET_PATH}" {
		t.Fatalf("command = %q", got)
	}
}

func TestFleetsHTTPImportRenamesCollisions(t *testing.T) {
	_, srv := setupTestServer(t)

	putBody := `{"agents":[{"command":"echo one"}]}`
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", strings.NewReader(putBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /fleets/default status = %d", resp.StatusCode)
	}

	importBody := `{"version":1,"fleets":{"default":{"agents":[{"command":"echo two"}]}}}`
	resp, err = http.Post(srv.URL+"/fleets/import", "application/json", strings.NewReader(importBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /fleets/import status = %d", resp.StatusCode)
	}
	var imported fleetsEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&imported); err != nil {
		t.Fatal(err)
	}
	if _, ok := imported.Fleets["default-2"]; !ok {
		t.Fatalf("imported fleets = %#v, want default-2", imported.Fleets)
	}
}

func TestPruneAllClearsFleetButPlainPrunePreserves(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setFleet("default", Fleet{Agents: []FleetAgent{{Command: "echo hi"}}}); err != nil {
		t.Fatalf("setFleet: %v", err)
	}

	s.clearAll(false)
	if _, ok := s.getFleet("default"); !ok {
		t.Fatal("plain clearAll should preserve fleets")
	}

	s.clearAll(true)
	if env := s.listFleets(); len(env.Fleets) != 0 {
		t.Fatalf("clearAll(includeSettings) fleets = %#v, want empty", env.Fleets)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "fleet.json")); !os.IsNotExist(err) {
		t.Fatalf("fleet.json should be removed by includeSettings prune, got err=%v", err)
	}
}

func TestFleetsRejectInvalidPayload(t *testing.T) {
	_, srv := setupTestServer(t)
	body := bytes.NewBufferString(`{"agents":[{"command":""}]}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFleetAgentDefaultsToBothTrue(t *testing.T) {
	s, srv := setupTestServer(t)
	body := bytes.NewBufferString(`{"agents":[{"command":"echo hi"}]}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /fleets/default status = %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/fleets/default")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fleets/default status = %d", resp.StatusCode)
	}
	var got struct {
		Fleet Fleet `json:"fleet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	assertFleetAgentOptionsTrue(t, got.Fleet.Agents[0])

	reloaded, err := newStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	fleet, ok := reloaded.getFleet("default")
	if !ok {
		t.Fatal("reloaded fleet not found")
	}
	assertFleetAgentOptionsTrue(t, fleet.Agents[0])
}

func TestFleetAgentRejectsExplicitFalseWrap(t *testing.T) {
	_, srv := setupTestServer(t)
	body := bytes.NewBufferString(`{"agents":[{"command":"echo hi","wrap_terminal":false,"auto_set_cwd":true}]}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFleetAgentAcceptsExplicitFalseCwd(t *testing.T) {
	_, srv := setupTestServer(t)
	body := bytes.NewBufferString(`{"agents":[{"command":"echo hi","wrap_terminal":true,"auto_set_cwd":false}]}`)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /fleets/default status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/fleets/default")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /fleets/default status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Fleet Fleet `json:"fleet"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Fleet.Agents[0].AutoSetCwd == nil || *got.Fleet.Agents[0].AutoSetCwd {
		t.Fatalf("auto_set_cwd = %v, want false", got.Fleet.Agents[0].AutoSetCwd)
	}
}

func assertFleetAgentOptionsTrue(t *testing.T, agent FleetAgent) {
	t.Helper()
	if agent.WrapTerminal == nil || !*agent.WrapTerminal {
		t.Fatalf("wrap_terminal = %v, want true", agent.WrapTerminal)
	}
	if agent.AutoSetCwd == nil || !*agent.AutoSetCwd {
		t.Fatalf("auto_set_cwd = %v, want true", agent.AutoSetCwd)
	}
}

func TestFleetsRejectUnsupportedEnvelopeVersion(t *testing.T) {
	_, srv := setupTestServer(t)
	resp, err := http.Post(srv.URL+"/fleets/import", "application/json", strings.NewReader(`{"version":2,"fleets":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFleetsCapRejected(t *testing.T) {
	_, srv := setupTestServer(t)
	env := fleetsEnvelope{Version: 1, Fleets: make(map[string]Fleet)}
	for i := 0; i < maxFleets+1; i++ {
		env.Fleets[fmt.Sprintf("fleet-%02d", i)] = Fleet{Agents: []FleetAgent{{Command: "echo hi"}}}
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets", bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFleetAgentsCapRejected(t *testing.T) {
	_, srv := setupTestServer(t)
	fleet := Fleet{Agents: make([]FleetAgent, maxFleetAgents+1)}
	for i := range fleet.Agents {
		fleet.Agents[i] = FleetAgent{Command: "echo hi"}
	}
	data, err := json.Marshal(fleet)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets/default", bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestFleetNameRegexRejected(t *testing.T) {
	_, srv := setupTestServer(t)
	for _, name := range []string{"", "-bad", "has space", "weird/slash"} {
		t.Run(name, func(t *testing.T) {
			env := fleetsEnvelope{Version: 1, Fleets: map[string]Fleet{
				name: {Agents: []FleetAgent{{Command: "echo hi"}}},
			}}
			data, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPut, srv.URL+"/fleets", bytes.NewReader(data))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}
