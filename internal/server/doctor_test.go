package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestSameHostPeer(t *testing.T) {
	for _, tt := range []struct {
		name        string
		peer, local string
		want        bool
	}{
		{"loopback v4", "127.0.0.1", "10.8.0.1", true},
		{"loopback v6", "::1", "10.8.0.1", true},
		{"v4-mapped loopback", "::ffff:127.0.0.1", "10.8.0.1", true},
		{"the server's own VPN address", "10.8.0.1", "10.8.0.1", true},
		{"the server's own v6 address", "fd00::1", "fd00::1", true},
		{"mapped form of the own address", "::ffff:10.8.0.1", "10.8.0.1", true},
		{"another host on the same network", "10.8.0.2", "10.8.0.1", false},
		{"a remote host and a loopback-accepted connection", "203.0.113.9", "127.0.0.1", false},
		{"no local address known", "10.8.0.1", "", false},
	} {
		peer := netip.MustParseAddr(tt.peer)
		var local netip.Addr
		if tt.local != "" {
			local = netip.MustParseAddr(tt.local)
		}
		if got := sameHostPeer(peer, local); got != tt.want {
			t.Errorf("%s: sameHostPeer(%s, %s) = %v, want %v", tt.name, tt.peer, tt.local, got, tt.want)
		}
	}
}

func TestDoctorAuthorizedUsesTheConnectionsLocalAddress(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.EnableDoctorKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	request := func(remote, local string) *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "/api/doctor", nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer "+f.srv.doctorKey)
		if local != "" {
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(&net.TCPAddr{IP: net.ParseIP(local), Port: 9997})))
		}
		return req
	}
	// AIMEBU2_BIND set to a VPN address: the CLI dials it, so peer and local address are the same.
	if !f.srv.doctorAuthorized(request("10.8.0.1:51000", "10.8.0.1")) {
		t.Error("a peer that connected from the server's own bind address was refused")
	}
	if f.srv.doctorAuthorized(request("10.8.0.2:51000", "10.8.0.1")) {
		t.Error("another host on the VPN was accepted")
	}
	if f.srv.doctorAuthorized(request("10.8.0.1:51000", "")) {
		t.Error("with no local address known only loopback may pass")
	}
	req := request("10.8.0.1:51000", "10.8.0.1")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if f.srv.doctorAuthorized(req) {
		t.Error("a relayed request passed because its peer was the server's own address")
	}
}

// ownNonLoopbackAddress returns an address of this machine that is not loopback, or "" when it has none (a container without a network).
func ownNonLoopbackAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() && !ipnet.IP.IsLinkLocalUnicast() {
			return ipnet.IP.String()
		}
	}
	return ""
}

// The real thing: a server listening on a non-loopback address of this machine, called through that address, as `doctor --live` does
// with AIMEBU2_BIND set to a VPN address.
func TestDoctorLiveOverANonLoopbackBind(t *testing.T) {
	address := ownNonLoopbackAddress()
	if address == "" {
		t.Skip("this machine has no non-loopback IPv4 address")
	}
	f := newFixture(t)
	if _, err := f.srv.EnableDoctorKey(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Binding to such an address needs AIMEBU2_ALLOW to let it in, as the setup docs say; the allowlist is a separate gate from the key.
	f.srv.Allow = append(f.srv.Allow, netip.MustParsePrefix(address+"/32"))
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", address, err)
	}
	server := httptest.NewUnstartedServer(f.srv.Handler())
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/doctor", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := get(f.srv.doctorKey); status != 200 {
		t.Fatalf("right key through %s: %d", server.URL, status)
	}
	if status := get(strings.Repeat("0", 64)); status != 403 {
		t.Fatalf("wrong key through %s: %d", server.URL, status)
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
