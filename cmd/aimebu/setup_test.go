package main

import (
	"bytes"
	"context"
	json "github.com/goccy/go-json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

func setupPost(t *testing.T, base, token, login, password string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token, "login": login, "password": password})
	resp, err := http.Post(base+"/api/setup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func postToken(t *testing.T, base, token string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := http.Post(base+"/api/setup/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A start with no owner and no owner environment serves first-run setup: a one-time link, the token in a 0600 file,
// an owner created through the browser path, and a /setup that is gone afterwards.
func TestFirstRunSetupWithoutOwnerEnvironment(t *testing.T) {
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "0")
	runFirstRunSetup(t, t.TempDir(), false)
}

// The default configuration runs the embedded supervisor, which needs the host machine key that only setup creates: the server
// must come up without it and start the supervisor once the owner exists.
func TestFirstRunSetupStartsTheEmbeddedSupervisorAfterTheOwner(t *testing.T) {
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "1")
	runFirstRunSetup(t, t.TempDir(), true)
}

// A v1 cutover with no owner environment must not move v1's files and then die: it moves them, starts, and waits for setup.
func TestV1CutoverWithoutOwnerEnvironmentWaitsForSetup(t *testing.T) {
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "1")
	root := makeV1Root(t)
	runFirstRunSetup(t, root, true)
	if _, err := os.Stat(filepath.Join(root, v1BackupName)); err != nil {
		t.Fatalf("v1's files were not backed up: %v", err)
	}
}

func runFirstRunSetup(t *testing.T, root string, embedded bool) {
	t.Helper()
	t.Setenv("AIMEBU2_USAGES", "0")
	useFreePort(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "")
	base := "http://127.0.0.1:" + os.Getenv("AIMEBU2_PORT")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case code := <-done:
		t.Fatalf("the server exited with %d before setup: %s", code, stderr.String())
	case <-time.After(50 * time.Millisecond):
	}
	tokenPath := filepath.Join(root, ".setup_token")
	var token string
	waitFor(t, "setup token", func() bool {
		raw, err := os.ReadFile(tokenPath)
		token = strings.TrimSpace(string(raw))
		return err == nil && token != "" && strings.Contains(stderr.String(), "/setup?token="+token)
	})
	// The terminal gets the token itself (not only a link that carries it) and the full path of the file that holds it.
	if out := stderr.String(); !strings.Contains(out, "\n  "+token+"\n") || !strings.Contains(out, "The token is also in "+tokenPath) {
		t.Fatalf("the startup message must print the token and name its file %s: %s", tokenPath, out)
	}
	if _, err := os.Stat(filepath.Join(root, "server", "setup-token")); !os.IsNotExist(err) {
		t.Fatalf("the old token location must not be used: %v", err)
	}
	if info, err := os.Stat(tokenPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file: %v %v", info, err)
	}
	waitFor(t, "server", func() bool {
		resp, err := http.Get(base + "/setup")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	if resp, err := http.Get(base + "/setup?token=" + token); err != nil || resp.StatusCode != 200 {
		t.Fatalf("/setup while open: %v %v", resp, err)
	}
	// The setup screen asks where the token is, then checks it before it asks for a login.
	if code, out := getJSON(t, base+"/api/setup/status"); code != 200 || out["open"] != true || out["token_path"] != tokenPath {
		t.Fatalf("setup status: %d %v (want path %s)", code, out, tokenPath)
	}
	if code, _ := postToken(t, base, "setup_wrong"); code != 403 {
		t.Fatalf("a wrong token on the check: %d", code)
	}
	if code, _ := postToken(t, base, token+"\n"); code != 204 {
		t.Fatalf("the right token (with the newline of a paste) on the check: %d", code)
	}
	if code, _ := postToken(t, base, token); code != 204 {
		t.Fatalf("checking a right token must not spend it: %d", code)
	}
	if code, _ := setupPost(t, base, "setup_wrong", "owner", "correct horse battery staple"); code != 403 {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := setupPost(t, base, token, "owner", "short"); code != 400 {
		t.Fatalf("weak password: %d", code)
	}
	code, out := setupPost(t, base, token, "owner", "correct horse battery staple")
	if code != 200 || out["csrf_token"] == "" || out["first_run"] != true {
		t.Fatalf("setup: %d %v", code, out)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token file survives: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "server", "host-machine.key")); err != nil {
		t.Fatalf("host machine key: %v", err)
	}
	if resp, err := http.Get(base + "/setup?token=" + token); err != nil || resp.StatusCode != 404 {
		t.Fatalf("/setup after setup: %v %v", resp, err)
	}
	if code, _ := setupPost(t, base, token, "second", "correct horse battery staple"); code != 404 {
		t.Fatalf("second setup: %d", code)
	}
	if code, _ := getJSON(t, base+"/api/setup/status"); code != 200 {
		t.Fatalf("setup status after setup: %d, want 200 {open:false}", code)
	}
	if code, _ := postToken(t, base, token); code != 404 {
		t.Fatalf("token check after setup: %d", code)
	}
	if embedded {
		waitFor(t, "the embedded supervisor", func() bool { _, err := os.Stat(filepath.Join(root, "supervisor", "journal.sqlite")); return err == nil })
		select {
		case code := <-done:
			t.Fatalf("the server exited with %d after setup: %s", code, stderr.String())
		default:
		}
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), "correct horse") {
		t.Fatalf("a secret reached the log: %q %q", stdout.String(), stderr.String())
	}
}

func TestSetupLink(t *testing.T) {
	if got := setupLink("", "0.0.0.0", 9897, nil, "t"); got != "http://localhost:9897/setup?token=t" {
		t.Fatal(got)
	}
	if got := setupLink("https://aimebu.example/", "127.0.0.1", 9897, nil, "t"); got != "https://aimebu.example/setup?token=t" {
		t.Fatal(got)
	}
	if got := setupLink("", "::1", 9897, nil, "t"); got != "http://[::1]:9897/setup?token=t" {
		t.Fatal(got)
	}
}

func TestEnvironmentOwnerIsHeldToTheSameRules(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	for _, tc := range []struct{ login, password, want string }{
		{"owner", "short", "at least 12 characters"},
		{"Bad Login", "correct horse battery staple", "3-32 lowercase letters"},
	} {
		useFreePort(t)
		t.Setenv("AIMEBU2_CONFIG_DIR", t.TempDir())
		t.Setenv("AIMEBU2_OWNER_LOGIN", tc.login)
		t.Setenv("AIMEBU2_OWNER_PASSWORD", tc.password)
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if code := serverCommand(ctx, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("%s/%s: code=%d stderr=%q", tc.login, tc.password, code, stderr.String())
		}
	}
}

// A normal start answers requests before the embedded supervisor is up: building its tool list takes seconds, and the pages must not
// wait for it. The test holds the supervisor's start, shows the server answering, then lets it go.
func TestServerAnswersBeforeTheEmbeddedSupervisorIsUp(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "1")
	useFreePort(t)
	root := t.TempDir()
	t.Setenv("AIMEBU2_CONFIG_DIR", root)
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	release := make(chan struct{})
	hold := func() { <-release }
	embeddedStartHook.Store(&hold)
	t.Cleanup(func() { embeddedStartHook.Store(nil) })
	base := "http://127.0.0.1:" + os.Getenv("AIMEBU2_PORT")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		cancel()
		<-done
	})
	waitFor(t, "the server to answer", func() bool {
		resp, err := http.Get(base + "/")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	hostConnected := func() bool {
		body, _ := json.Marshal(map[string]string{"login": "owner", "password": "correct horse battery staple"})
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		login, err := client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
		if err != nil {
			return false
		}
		login.Body.Close()
		resp, err := client.Get(base + "/api/machines")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var listed struct {
			Machines []struct {
				IsHost    bool `json:"is_host"`
				Connected bool `json:"connected"`
			} `json:"machines"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
			return false
		}
		for _, machine := range listed.Machines {
			if machine.IsHost && machine.Connected {
				return true
			}
		}
		return false
	}
	// The check must be able to see a connected host at all, or "not connected yet" proves nothing: hold the supervisor back, look
	// twice, then let it go and look until it is there.
	for i := 0; i < 3; i++ {
		if hostConnected() {
			t.Fatal("the supervisor was up before the server answered")
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(release)
	waitFor(t, "the embedded supervisor to connect", hostConnected)
}

// A shutdown that is not a signal (the HTTP server fails while the context is still live) must not leave a supervisor starting behind it:
// the start is still pending in the background, and it would run with nobody left to stop it.
func TestAServeErrorDoesNotLetAPendingSupervisorStart(t *testing.T) {
	t.Setenv("AIMEBU2_USAGES", "0")
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "1")
	useFreePort(t)
	t.Setenv("AIMEBU2_CONFIG_DIR", t.TempDir())
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "correct horse battery staple")
	release := make(chan struct{})
	hold := func() { <-release }
	embeddedStartHook.Store(&hold)
	var captured net.Listener
	capture := func(l net.Listener) { captured = l }
	listenerHook.Store(&capture)
	finished := make(chan struct{})
	onFinished := func() { close(finished) }
	embeddedStartFinished.Store(&onFinished)
	t.Cleanup(func() { embeddedStartHook.Store(nil); listenerHook.Store(nil); embeddedStartFinished.Store(nil) })
	before := embeddedStarts.Load()
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() { done <- serverCommand(context.Background(), &stdout, &stderr) }()
	base := "http://127.0.0.1:" + os.Getenv("AIMEBU2_PORT")
	waitFor(t, "the server to answer", func() bool {
		resp, err := http.Get(base + "/")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	// Serve fails while the context is live; the supervisor's start is still held back.
	captured.Close()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the server did not stop after its listener failed")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the pending supervisor start never returned")
	}
	if got := embeddedStarts.Load() - before; got != 0 {
		t.Fatalf("a supervisor started after the server had shut down (%d)", got)
	}
}
