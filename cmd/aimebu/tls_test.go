package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/v2/internal/tlsclient"
)

func TestServerRejectsPublicURLWithPath(t *testing.T) {
	t.Setenv("AIMEBU_CONFIG_DIR", t.TempDir())
	useFreePort(t)
	t.Setenv("AIMEBU_PUBLIC_URL", "https://example.test/app")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"server"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "AIMEBU_PUBLIC_URL") {
		t.Fatalf("invalid public URL: code=%d stderr=%q", code, stderr.String())
	}
}

func testCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// freeTCPPorts holds n distinct loopback ports for the server under test (see holdFreePorts): all are open at once, so a test that needs two
// (HTTP and HTTPS) is never handed one twice, and none is released before the server binds it.
func freeTCPPorts(t *testing.T, n int) []int { return holdFreePorts(t, n) }

// Several ports picked at once are distinct, however many.
func TestFreeTCPPortsAreDistinct(t *testing.T) {
	t.Parallel()
	for round := 0; round < 50; round++ {
		ports := freeTCPPorts(t, 4)
		seen := map[int]bool{}
		for _, port := range ports {
			if port <= 0 || seen[port] {
				t.Fatalf("round %d: ports %v are not distinct and valid", round, ports)
			}
			seen[port] = true
		}
	}
}

func TestResolveServerTLSConfig(t *testing.T) {
	cert, key := testCertificate(t)
	t.Setenv("AIMEBU_TLS_CERT", "")
	t.Setenv("AIMEBU_TLS_KEY", "")
	t.Setenv("AIMEBU_TLS_PORT", "")
	if config, err := resolveServerTLSConfig(9897); err != nil || config != nil {
		t.Fatalf("TLS off = (%v, %v)", config, err)
	}
	t.Setenv("AIMEBU_TLS_CERT", cert)
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted certificate without key")
	}
	t.Setenv("AIMEBU_TLS_KEY", key)
	if config, err := resolveServerTLSConfig(9897); err != nil || config.port != 9996 {
		t.Fatalf("TLS default = (%v, %v)", config, err)
	}
	for _, value := range []string{"0", "65536", "invalid", "9897"} {
		t.Setenv("AIMEBU_TLS_PORT", value)
		if _, err := resolveServerTLSConfig(9897); err == nil {
			t.Fatalf("accepted TLS port %q", value)
		}
	}
	t.Setenv("AIMEBU_TLS_PORT", "")
	t.Setenv("AIMEBU_TLS_KEY", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted missing key file")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(invalid, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU_TLS_KEY", invalid)
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted invalid certificate pair")
	}
}

func TestServerServesHTTPAndHTTPSWithSameAllowlist(t *testing.T) {
	cert, key := testCertificate(t)
	t.Setenv("AIMEBU_TLS_CERT", cert)
	t.Setenv("AIMEBU_TLS_KEY", key)
	t.Setenv("AIMEBU_EMBEDDED_SUPERVISOR", "0")
	t.Setenv("AIMEBU_USAGES", "0")
	t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "1")
	t.Setenv("AIMEBU_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU_OWNER_PASSWORD", "test-password-123")
	for _, tc := range []struct {
		name, allow string
		status      int
	}{
		{"allowed", "127.0.0.0/8", http.StatusUnauthorized},
		{"blocked", "192.0.2.0/24", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ports := freeTCPPorts(t, 2)
			httpPort, httpsPort := ports[0], ports[1]
			useMigratedConfigDir(t, t.TempDir())
			t.Setenv("AIMEBU_PORT", fmt.Sprint(httpPort))
			t.Setenv("AIMEBU_TLS_PORT", fmt.Sprint(httpsPort))
			t.Setenv("AIMEBU_ALLOW", tc.allow)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			var stdout, stderr bytes.Buffer
			ready := watchServing(t)
			go func() { done <- serverCommand(ctx, &stdout, &stderr) }()
			client := tlsclient.NewHTTPClient(10 * time.Second)
			origins := []string{fmt.Sprintf("http://127.0.0.1:%d", httpPort), fmt.Sprintf("https://127.0.0.1:%d", httpsPort)}
			for _, origin := range origins {
				waitServerStatus(t, origin+"/api/me", client, tc.status, ready, done, &stderr)
			}
			if tc.name == "allowed" {
				// A self-signed server is accepted only under the explicit escape hatch.
				t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "")
				if response, err := tlsclient.NewHTTPClient(2 * time.Second).Get(origins[1] + "/api/me"); err == nil {
					response.Body.Close()
					t.Fatal("trusted client accepted a self-signed server")
				}
				t.Setenv("AIMEBU_INSECURE_SKIP_VERIFY", "1")
				// Password verification is intentionally expensive. Give the one
				// login request time to finish under the race detector and host load.
				loginClient := tlsclient.NewHTTPClient(30 * time.Second)
				for _, origin := range origins {
					resp, err := loginClient.Post(origin+"/api/login", "application/json", strings.NewReader(`{"login":"owner","password":"test-password-123"}`))
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 || resp.Cookies()[0].Secure != strings.HasPrefix(origin, "https:") {
						t.Fatalf("%s login status=%d cookies=%v", origin, resp.StatusCode, resp.Cookies())
					}
				}
			}
			cancel()
			if code := receive(t, "the server stopping", done, serverStopLimit); code != 0 {
				t.Fatalf("server exit=%d stderr=%s", code, stderr.String())
			}
		})
	}
}

// The page is ~240 modules: over HTTP/1.1 a browser fetches them six at a time and waits for every round trip, so the HTTPS listener must offer h2
// (ALPN), and net/http must then serve it on the same listener the server uses.
func TestHTTPSListenerNegotiatesHTTP2(t *testing.T) {
	cert, key := testCertificate(t)
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenTLS("127.0.0.1", &serverTLSConfig{certificate: pair, port: freeTCPPorts(t, 1)[0]})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Proto) }), ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	for _, tc := range []struct {
		name   string
		protos []string
		want   string
	}{
		{"a browser", []string{"h2", "http/1.1"}, "HTTP/2.0"},
		{"a client that only speaks HTTP/1.1", []string{"http/1.1"}, "HTTP/1.1"},
	} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: tc.protos}, ForceAttemptHTTP2: len(tc.protos) > 1}
		response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Get("https://" + listener.Addr().String() + "/")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		transport.CloseIdleConnections()
		if string(body) != tc.want {
			t.Fatalf("%s was served as %q, want %q", tc.name, body, tc.want)
		}
	}
}

// A private key that any user can read is warned about, never refused; a key only its owner or its group can read is not.
func TestTLSKeyWarning(t *testing.T) {
	_, keyPath := testCertificate(t)
	for _, tc := range []struct {
		mode os.FileMode
		warn bool
	}{{0o600, false}, {0o640, false}, {0o660, false}, {0o644, true}, {0o604, true}, {0o666, true}} {
		if err := os.Chmod(keyPath, tc.mode); err != nil {
			t.Fatal(err)
		}
		warning := tlsKeyWarning(keyPath)
		if (warning != "") != tc.warn || tc.warn && !strings.Contains(warning, keyPath) {
			t.Fatalf("mode %04o: warning %q, want warning=%v", tc.mode, warning, tc.warn)
		}
	}
	if warning := tlsKeyWarning(filepath.Join(t.TempDir(), "missing.pem")); warning != "" {
		t.Fatalf("a missing key is the resolver's error, not a warning: %q", warning)
	}
}
