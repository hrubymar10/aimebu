package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	json "github.com/goccy/go-json"
	"github.com/hrubymar10/aimebu/internal/auth"
)

func TestSetupStatusShowsTheFullPathOnlyToThisMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".setup_token")
	setup, token, err := auth.NewOwnerSetup(path, func(context.Context, string, string) (string, error) { return "usr", nil })
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Setup: setup}
	ask := func(remote string, headers ...string) (int, map[string]any, string) {
		req := httptest.NewRequest(http.MethodGet, "/api/setup/status", nil)
		req.RemoteAddr = remote
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		s.setupStatus(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	for _, loopback := range []string{"127.0.0.1:5000", "[::1]:5000", "[::ffff:127.0.0.1]:5000"} {
		if code, out, _ := ask(loopback); code != 200 || out["token_path"] != path {
			t.Fatalf("%s: %d %v, want the full path %s", loopback, code, out, path)
		}
	}
	for _, remote := range []string{"10.1.2.3:5000", "[2001:db8::1]:5000", "192.168.0.9:1"} {
		code, out, raw := ask(remote)
		if code != 200 || out["token_path"] != "<config dir>/.setup_token" {
			t.Fatalf("%s: %d %v, want only the place inside the config directory", remote, code, out)
		}
		if strings.Contains(raw, filepath.Dir(path)) || strings.Contains(raw, token) {
			t.Fatalf("%s: the answer leaks the absolute path or the token: %s", remote, raw)
		}
	}
	// A proxy on this machine (Caddy, nginx) connects from loopback too, and what it relays may be anyone: a forwarding header, with no
	// trusted proxy configured, means the person is not known to be on this machine.
	for _, header := range [][]string{{"X-Forwarded-For", "203.0.113.7"}, {"X-Forwarded-Proto", "https"}, {"X-Real-Ip", "203.0.113.7"}, {"Forwarded", "for=203.0.113.7"}, {"Via", "1.1 caddy"}} {
		code, out, raw := ask("127.0.0.1:5000", header...)
		if code != 200 || out["token_path"] != "<config dir>/.setup_token" || strings.Contains(raw, filepath.Dir(path)) {
			t.Fatalf("loopback peer with %s: %d %v, want the placeholder", header[0], code, out)
		}
	}
	if len(s.TrustedProxies) != 0 {
		t.Fatal("this test is about a proxy that is not configured as trusted")
	}
	setup.Close()
	if code, out, raw := ask("127.0.0.1:5000"); code != 200 || out["open"] != false || out["token_path"] != nil {
		t.Fatalf("after setup closes: %d %s, want {open:false} and no path", code, raw)
	}
}
