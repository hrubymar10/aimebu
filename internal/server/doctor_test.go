package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/protocol"
)

func doctorRequest(t *testing.T, f *fixture, mutate func(*http.Request)) (int, protocol.DoctorLive) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.http.URL+"/api/doctor", nil)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var live protocol.DoctorLive
	if resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(&live); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, live
}

func readDoctorKeyFile(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "server", DoctorKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestDoctorKeyFileIsPrivateFreshAndRemoved(t *testing.T) {
	root := t.TempDir()
	f := newFixture(t)
	remove, err := f.srv.EnableDoctorKey(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "server", DoctorKeyFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm()&0o077 != 0 {
		t.Fatalf("server dir: %v %v", dir, err)
	}
	first := readDoctorKeyFile(t, root)
	if len(first) != 64 {
		t.Fatalf("key %q is not 32 random bytes in hex", first)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
	remove()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the key survived shutdown: %v", err)
	}
	if _, err := f.srv.EnableDoctorKey(root); err != nil {
		t.Fatal(err)
	}
	if second := readDoctorKeyFile(t, root); second == first {
		t.Fatal("a new start must write a new key")
	}
}

func TestDoctorLiveAuthentication(t *testing.T) {
	f := newFixture(t)
	if status, _ := doctorRequest(t, f, nil); status != 403 {
		t.Fatalf("no key armed: %d", status)
	}
	root := t.TempDir()
	if _, err := f.srv.EnableDoctorKey(root); err != nil {
		t.Fatal(err)
	}
	key := readDoctorKeyFile(t, root)
	bearer := func(value string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", value) }
	}
	for name, mutate := range map[string]func(*http.Request){
		"no credentials":   nil,
		"wrong key":        bearer("Bearer " + strings.Repeat("0", 64)),
		"key without type": bearer(key),
		"empty bearer":     bearer("Bearer "),
		"relayed by proxy": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+key)
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
		},
		"a login cookie": func(r *http.Request) { r.AddCookie(f.cookie) },
	} {
		if status, _ := doctorRequest(t, f, mutate); status != 403 {
			t.Errorf("%s: status %d, want 403", name, status)
		}
	}
	if status, live := doctorRequest(t, f, bearer("Bearer "+key)); status != 200 || len(live.Machines) == 0 {
		t.Fatalf("right key from loopback: %d %+v", status, live)
	}
}

func TestDoctorLiveRejectsANonLoopbackPeer(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.EnableDoctorKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"192.0.2.7:4000", "[2001:db8::1]:4000", "not-an-address"} {
		req, _ := http.NewRequest(http.MethodGet, "/api/doctor", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+f.srv.doctorKey)
		if f.srv.doctorAuthorized(req) {
			t.Errorf("%s was accepted", remote)
		}
	}
	for _, remote := range []string{"127.0.0.1:4000", "[::1]:4000", "[::ffff:127.0.0.1]:4000"} {
		req, _ := http.NewRequest(http.MethodGet, "/api/doctor", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+f.srv.doctorKey)
		if !f.srv.doctorAuthorized(req) {
			t.Errorf("%s was refused", remote)
		}
	}
}

func TestDoctorLiveReportsCapabilitiesAndLaunchers(t *testing.T) {
	f := newFixture(t)
	root := t.TempDir()
	if _, err := f.srv.EnableDoctorKey(root); err != nil {
		t.Fatal(err)
	}
	key := readDoctorKeyFile(t, root)
	for _, query := range []string{
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled,containerized) VALUES ('l1',?,'opencode','docker','OpenCode (Docker)','opencode-docker',1,1)`,
		`INSERT INTO machine_launcher(id,machine_id,tool,name,label,command,enabled) VALUES ('l2',?,'pi','custom','Pi','pi',0)`,
	} {
		if _, err := f.exec(query, f.machineID); err != nil {
			t.Fatal(err)
		}
	}
	// An old supervisor: it announces launchers and detect, nothing else.
	machine := &machineConn{id: f.machineID, done: make(chan struct{}), supportsLaunchers: true, supportsDetect: true,
		launchers: []protocol.LauncherRow{{LauncherDefinition: protocol.LauncherDefinition{ID: "l1"}, Available: true, Version: "1.0", Features: []string{"forward_env"}, FeatureProblem: "launcher too old"}}}
	f.srv.machineMu.Lock()
	f.srv.machines[f.machineID] = machine
	f.srv.machineMu.Unlock()
	_, live := doctorRequest(t, f, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) })
	var got *protocol.DoctorMachine
	for i := range live.Machines {
		if live.Machines[i].ID == f.machineID {
			got = &live.Machines[i]
		}
	}
	if got == nil || !got.Connected || !got.Approved {
		t.Fatalf("machine: %+v", live)
	}
	supported := map[string]bool{}
	for _, support := range got.Supports {
		supported[support.Flag] = support.Supported
		if support.Degrades == "" {
			t.Errorf("%s says nothing of what degrades", support.Flag)
		}
	}
	if len(supported) != len(doctorCapabilities) || !supported["supports_launchers"] || !supported["supports_detect"] || supported["supports_worktree"] || supported["supports_stop_replay"] {
		t.Fatalf("capability flags: %v", supported)
	}
	byID := map[string]protocol.DoctorLauncher{}
	for _, launcher := range got.Launchers {
		byID[launcher.ID] = launcher
	}
	if l := byID["l1"]; !l.Reported || !l.Available || l.FeatureProblem != "launcher too old" || len(l.Features) != 1 || l.Version != "1.0" || !l.Containerized {
		t.Fatalf("l1: %+v", l)
	}
	if l := byID["l2"]; l.Reported || l.Enabled {
		t.Fatalf("l2: %+v", l)
	}
	// Once the machine is gone only the definitions remain: no flags.
	f.srv.machineMu.Lock()
	delete(f.srv.machines, f.machineID)
	f.srv.machineMu.Unlock()
	_, live = doctorRequest(t, f, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) })
	for _, machine := range live.Machines {
		if machine.ID == f.machineID && (machine.Connected || len(machine.Supports) != 0) {
			t.Fatalf("a disconnected machine reports capabilities: %+v", machine)
		}
	}
}
