package server

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/v2/internal/protocol"
)

func TestDirectoryListRequiresAdminAndAbsolutePath(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "candidate"), 0700); err != nil {
		t.Fatal(err)
	}
	path := "/api/machines/directories?path=" + url.QueryEscape(root) + "&prefix=can"
	resp := f.request("GET", path, nil, false)
	var body struct {
		Directories []string `json:"directories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body.Directories) != 1 || body.Directories[0] != "candidate" {
		t.Fatalf("owner listing: %d %+v", resp.StatusCode, body)
	}
	resp = f.request("GET", "/api/machines/directories?path="+url.QueryEscape(root), nil, false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("empty prefix: %d", resp.StatusCode)
	}
	resp = f.request("GET", "/api/machines/directories?path="+url.QueryEscape(root)+"&prefix=.", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("hidden prefix: %d", resp.StatusCode)
	}
	resp = f.request("GET", "/api/machines/directories?path=relative", nil, false)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("relative path: %d", resp.StatusCode)
	}
	cookie, csrf := registerSecondUser(t, f, "viewer")
	resp = f.requestAs(t, cookie, csrf, "GET", path, nil)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("viewer listing: %d", resp.StatusCode)
	}
}

func TestDirectoryListUsesSelectedMachine(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	id := f.remoteMachine(t, false)
	f.srv.machineMu.Lock()
	conn := f.srv.machines[id]
	conn.supportsDirectoryList = true
	f.srv.machineMu.Unlock()
	go func() {
		request := <-conn.sendQueue
		conn.harnessMu.Lock()
		waiter := conn.pathPending[request.ReqID]
		conn.harnessMu.Unlock()
		if request.Type != protocol.DirectoryList || request.CheckoutPath != "/remote/" || request.DirectoryPrefix != "proj" {
			waiter <- protocol.Frame{Type: protocol.DirectoryListResult}
			return
		}
		waiter <- protocol.Frame{Type: protocol.DirectoryListResult, DirectoryNames: []string{"project"}}
	}()
	resp := f.request("GET", "/api/machines/directories?machine_id="+id+"&path=/remote/&prefix=proj", nil, false)
	var body struct {
		Directories []string `json:"directories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body.Directories) != 1 || body.Directories[0] != "project" {
		t.Fatalf("remote listing: %d %+v", resp.StatusCode, body)
	}
}

func TestDirectoryListOlderSupervisorGetsNoFrame(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.seed()
	id := f.remoteMachine(t, false)
	resp := f.request("GET", "/api/machines/directories?machine_id="+id+"&path=/remote/", nil, false)
	var body struct {
		Directories []string `json:"directories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body.Directories) != 0 {
		t.Fatalf("older supervisor listing: %d %+v", resp.StatusCode, body)
	}
	f.srv.machineMu.Lock()
	queued := len(f.srv.machines[id].sendQueue)
	f.srv.machineMu.Unlock()
	if queued != 0 {
		t.Fatal("older supervisor received an unknown frame")
	}
}
