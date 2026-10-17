package server

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/credtarget"
	"github.com/hrubymar10/aimebu/internal/switcher"
)

func fullClaudeCreds(access, refresh string, expires time.Time) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d}}`, access, refresh, expires.UnixMilli())
}

// The real Manager, Registry, Writer and FanOut (as startTokenCopies wires them):
// migrate a linked install, then race Switch A→B against a refresher fan-out of
// A. The live file must end token-only and hold B's access token, and nothing
// may deadlock (FanOut takes the switcher lock, Switch holds it for its writes).
func TestTokenCopiesSwitchRacingFanOutNeverLeavesTheOldAccountLive(t *testing.T) {
	m, home := newSwitcherTestEnv(t)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(6 * time.Hour)
	live := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(fullClaudeCreds("tok-A", "ref-A", expiry)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(switcher.ToolClaude, "A"); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(switcher.ToolClaude, "B"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.StoredCredPath(switcher.ToolClaude, "B"), []byte(fullClaudeCreds("tok-B", "ref-B", expiry)), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := credtarget.NewRegistry(t.TempDir(), home)
	reg.LivePath = m.LiveCredPath
	fan := &credtarget.FanOut{Registry: reg, Writer: &credtarget.Writer{}, Active: m.Active, WithLock: m.WithLock}
	m.SetTokenCopyHooks(switcher.TokenCopyHooks{FanOutActive: fan.FanOutActive, ReplaceActive: fan.FanOutActiveReplacing})
	if _, err := m.MigrateToTokenCopies(switcher.MigrateOptions{Tools: []string{switcher.ToolClaude}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	refreshedA := []byte(fullClaudeCreds("tok-A2", "ref-A2", expiry.Add(time.Hour)))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = fan.FanOut(switcher.ToolClaude, "A", refreshedA)
		}
	}()
	go func() {
		defer wg.Done()
		if _, _, err := m.Switch(switcher.ToolClaude, "B"); err != nil {
			t.Errorf("Switch: %v", err)
		}
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock between Switch and FanOut")
	}

	data, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "refreshToken") {
		t.Fatalf("live file holds a refresh token: %s", got)
	}
	if !strings.Contains(got, "tok-B") || strings.Contains(got, "tok-A") {
		t.Fatalf("live file is not B's after the switch race: %s", got)
	}
}

func credentialMessages(s *store, room string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, msg := range s.messages[room] {
		if strings.Contains(msg.Body, "Credential alert") {
			out = append(out, msg.Body)
		}
	}
	return out
}

// Edge kinds that end in "log in again" reach the humans as a persisted system
// message; other kinds only log/broadcast. The refresher's sink has no dedupe
// (it re-arms its own edges), the reconciler's sink does.
func TestCredentialAlertSinkPostsHumanEdgesAndDedupesOnlyWhenAsked(t *testing.T) {
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("alex", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}

	refresher := s.credentialAlertSink(false)
	refresher("reauth_required", "claude/main: log in again")
	refresher("reauth_required", "claude/main: log in again") // flap: failure, recovery, failure
	refresher("fanout_failed", "claude/main: disk full")      // informational
	if got := credentialMessages(s, "general"); len(got) != 2 {
		t.Fatalf("refresher alerts = %v, want the two reauth edges and no fanout_failed", got)
	}

	reconciler := s.credentialAlertSink(true)
	reconciler("expired", "codex/main: expired")
	reconciler("expired", "codex/main: expired")
	if got := credentialMessages(s, "general"); len(got) != 3 {
		t.Fatalf("after a deduped repeat there are %d alerts, want 3", len(got))
	}
}

// A failing CLI's output can reach an alert's detail. The log gets it redacted,
// the websocket frame gets the kind alone, and the persisted system message
// (readable by every room member) gets a fixed sentence plus the subject.
func TestCredentialAlertSinkNeverLeaksDetailText(t *testing.T) {
	const (
		jwt     = "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ImZha2VAZXhhbXBsZS50ZXN0In0.ZmFrZXNpZw"
		refresh = "rt-FAKE-refresh-token-value-1234567890"
	)
	s, _ := setupTestServer(t)
	human, err := s.registerHuman("alex", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.createRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.joinRoom("general", human.ID); err != nil {
		t.Fatal(err)
	}
	frames := s.subscribeMeta()
	defer s.unsubscribeMeta(frames)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	sink := s.credentialAlertSink(false)
	hostile := "claude/main: the CLI failed after starting: harness-docker run failed: exit status 1: " +
		`{"refresh_token":"` + refresh + `","id_token":"` + jwt + `"} open /root/.claude/.credentials.json`
	sink("needs_relogin_soon", hostile)
	sink("reauth_required", jwt+": rejected "+refresh) // a subject that is itself a secret
	sink("fanout_failed", "claude/main: write "+refresh)

	tokens := []string{jwt, refresh, "eyJhbGci"}
	freeText := append([]string{"exit status 1", "harness-docker", "/root/"}, tokens...)
	check := func(where, text string, secrets []string) {
		t.Helper()
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %q:\n%s", where, secret, text)
			}
		}
	}
	check("the log", logs.String(), tokens)
	if !strings.Contains(logs.String(), "[redacted]") {
		t.Errorf("the log keeps the alert, redacted: %s", logs.String())
	}
	for len(frames) > 0 {
		frame := <-frames
		if frame.Type != "credential_alert" {
			continue
		}
		data, _ := frame.Data.(map[string]string)
		if _, has := data["detail"]; has || data["kind"] == "" {
			t.Errorf("the meta frame carries only the kind: %v", frame.Data)
		}
	}
	posted := credentialMessages(s, "general")
	if len(posted) != 2 {
		t.Fatalf("system messages = %v", posted)
	}
	for _, body := range posted {
		check("a system message", body, freeText)
	}
	if !strings.Contains(posted[0], "claude/main: a refresh may have been spent") {
		t.Errorf("the message names the subject and says what to do: %s", posted[0])
	}
}
