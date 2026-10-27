package server

import (
	"net/http"
	"runtime"
	"testing"

	"github.com/goccy/go-json"
)

// The header of the page shows the version the server was built as: a signed-in account reads it, nobody else does.
func TestBuildInfoGivesTheVersionToSignedInAccountsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.srv.Version = "aimebu v9.8.7-3-gabc123"
	resp := f.request("GET", "/api/buildinfo", nil, false)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}
	var info map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info["version"] != "aimebu v9.8.7-3-gabc123" || info["go_version"] != runtime.Version() || len(info) != 2 {
		t.Fatalf("answer %v", info)
	}
	anonymous, err := http.Get(f.http.URL + "/api/buildinfo")
	if err != nil {
		t.Fatal(err)
	}
	anonymous.Body.Close()
	if anonymous.StatusCode != 401 {
		t.Fatalf("without a session: %d, want 401", anonymous.StatusCode)
	}
}
