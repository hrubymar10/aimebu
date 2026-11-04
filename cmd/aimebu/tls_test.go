package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrubymar10/aimebu/internal/tlsclient"
)

func TestServerRejectsPublicURLWithPath(t *testing.T) {
	t.Setenv("AIMEBU2_CONFIG_DIR", t.TempDir())
	useFreePort(t)
	t.Setenv("AIMEBU2_PUBLIC_URL", "https://example.test/app")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"server"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "AIMEBU2_PUBLIC_URL") {
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

// freeTCPPorts picks n distinct free loopback ports. The listeners stay open until every port is picked: closing each one right away
// lets the next pick get the same port back, and a test that needs two ports (HTTP and HTTPS) would be handed one twice.
func freeTCPPorts(t *testing.T, n int) []int {
	t.Helper()
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	ports := make([]int, 0, n)
	seen := map[int]bool{}
	for range n {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		port := listener.Addr().(*net.TCPAddr).Port
		if seen[port] {
			t.Fatalf("port %d was picked twice while its listener was open", port)
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports
}

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
	t.Setenv("AIMEBU2_TLS_CERT", "")
	t.Setenv("AIMEBU2_TLS_KEY", "")
	t.Setenv("AIMEBU2_TLS_PORT", "")
	if config, err := resolveServerTLSConfig(9897); err != nil || config != nil {
		t.Fatalf("TLS off = (%v, %v)", config, err)
	}
	t.Setenv("AIMEBU2_TLS_CERT", cert)
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted certificate without key")
	}
	t.Setenv("AIMEBU2_TLS_KEY", key)
	if config, err := resolveServerTLSConfig(9897); err != nil || config.port != 9896 {
		t.Fatalf("TLS default = (%v, %v)", config, err)
	}
	for _, value := range []string{"0", "65536", "invalid", "9897"} {
		t.Setenv("AIMEBU2_TLS_PORT", value)
		if _, err := resolveServerTLSConfig(9897); err == nil {
			t.Fatalf("accepted TLS port %q", value)
		}
	}
	t.Setenv("AIMEBU2_TLS_PORT", "")
	t.Setenv("AIMEBU2_TLS_KEY", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted missing key file")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(invalid, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEBU2_TLS_KEY", invalid)
	if _, err := resolveServerTLSConfig(9897); err == nil {
		t.Fatal("accepted invalid certificate pair")
	}
}

func TestServerServesHTTPAndHTTPSWithSameAllowlist(t *testing.T) {
	cert, key := testCertificate(t)
	t.Setenv("AIMEBU2_TLS_CERT", cert)
	t.Setenv("AIMEBU2_TLS_KEY", key)
	t.Setenv("AIMEBU2_EMBEDDED_SUPERVISOR", "0")
	t.Setenv("AIMEBU2_USAGES", "0")
	t.Setenv("AIMEBU2_INSECURE_SKIP_VERIFY", "1")
	t.Setenv("AIMEBU2_OWNER_LOGIN", "owner")
	t.Setenv("AIMEBU2_OWNER_PASSWORD", "test-password-123")
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
			t.Setenv("AIMEBU2_PORT", fmt.Sprint(httpPort))
			t.Setenv("AIMEBU2_TLS_PORT", fmt.Sprint(httpsPort))
			t.Setenv("AIMEBU2_ALLOW", tc.allow)
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
				t.Setenv("AIMEBU2_INSECURE_SKIP_VERIFY", "")
				if response, err := tlsclient.NewHTTPClient(2 * time.Second).Get(origins[1] + "/api/me"); err == nil {
					response.Body.Close()
					t.Fatal("trusted client accepted a self-signed server")
				}
				t.Setenv("AIMEBU2_INSECURE_SKIP_VERIFY", "1")
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
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("server exit=%d stderr=%s", code, stderr.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("server did not stop")
			}
		})
	}
}
