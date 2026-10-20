package server

import (
	"bytes"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// A failing CLI's output can reach an alert's detail. Neither the log nor the
// persisted system event (readable through the history API and kept in the
// database) may hold a token-shaped string or the raw CLI text's secrets.
func TestCredentialAlertSinkRedactsDetailBeforeLoggingOrRecording(t *testing.T) {
	const (
		jwt     = "eyJhbGciOiJub25lIn0.eyJlbWFpbCI6ImZha2VAZXhhbXBsZS50ZXN0In0.ZmFrZXNpZw"
		refresh = "rt-FAKE-refresh-token-value-1234567890"
	)
	f := newFixture(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	sink := f.srv.credentialAlertSink(false)
	sink("needs_relogin_soon", `claude/main: the CLI failed: {"refresh_token":"`+refresh+`","id_token":"`+jwt+`"}`)

	resp := f.request("GET", "/api/system-events", nil, false)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("system events: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "credential_alert") || !strings.Contains(string(body), "claude/main") {
		t.Fatalf("the owner still sees the alert and its subject: %s", body)
	}
	for _, where := range []struct{ name, text string }{{"the log", logs.String()}, {"the recorded event", string(body)}} {
		for _, secret := range []string{jwt, refresh, "eyJhbGci"} {
			if strings.Contains(where.text, secret) {
				t.Errorf("%s leaks %q: %s", where.name, secret, where.text)
			}
		}
	}
}
