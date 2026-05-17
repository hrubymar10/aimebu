package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveServerTLSConfigDisabledByDefault(t *testing.T) {
	t.Setenv("AIMEBU_TLS_CERT", "")
	t.Setenv("AIMEBU_TLS_KEY", "")

	cfg, err := resolveServerTLSConfig()
	if err != nil {
		t.Fatalf("resolveServerTLSConfig returned error: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("TLS should be disabled by default: %+v", cfg)
	}
}

func TestResolveServerTLSConfigRequiresPair(t *testing.T) {
	cases := []struct {
		name string
		cert string
		key  string
	}{
		{name: "cert only", cert: "/tmp/cert.pem"},
		{name: "key only", key: "/tmp/key.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIMEBU_TLS_CERT", tc.cert)
			t.Setenv("AIMEBU_TLS_KEY", tc.key)

			_, err := resolveServerTLSConfig()
			if err == nil || !strings.Contains(err.Error(), "must be set together") {
				t.Fatalf("expected paired env error, got %v", err)
			}
		})
	}
}

func TestResolveServerTLSConfigRequiresUsableCertificatePair(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	t.Setenv("AIMEBU_TLS_CERT", cert)
	t.Setenv("AIMEBU_TLS_KEY", key)

	_, err := resolveServerTLSConfig()
	if err == nil || !strings.Contains(err.Error(), "AIMEBU_TLS_CERT") {
		t.Fatalf("expected cert readability error, got %v", err)
	}

	writeTLSFile(t, cert, "not a cert")
	writeTLSFile(t, key, "not a key")
	_, err = resolveServerTLSConfig()
	if err == nil || !strings.Contains(err.Error(), "not a usable certificate pair") {
		t.Fatalf("expected malformed pair error, got %v", err)
	}

	writeTestCertificatePair(t, cert, key)
	cfg, err := resolveServerTLSConfig()
	if err != nil {
		t.Fatalf("resolveServerTLSConfig returned error: %v", err)
	}
	if !cfg.Enabled || cfg.CertFile != cert || cfg.KeyFile != key {
		t.Fatalf("unexpected TLS config: %+v", cfg)
	}
}

func TestResolveServerTLSConfigRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	writeTLSFile(t, key, "key")
	t.Setenv("AIMEBU_TLS_CERT", dir)
	t.Setenv("AIMEBU_TLS_KEY", key)

	_, err := resolveServerTLSConfig()
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

func TestResolveServerTLSPort(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "default", want: defaultTLSPort},
		{name: "custom", raw: "9443", want: "9443"},
		{name: "service name", raw: "https", want: "https"},
		{name: "bad", raw: "not-a-port", wantErr: "AIMEBU_TLS_PORT invalid port"},
		{name: "out of range", raw: "99999", wantErr: "AIMEBU_TLS_PORT invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIMEBU_TLS_PORT", tc.raw)
			got, err := resolveServerTLSPort()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q error, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveServerTLSPort returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func writeTLSFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeTestCertificatePair(t *testing.T, certPath, keyPath string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certFile, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("write cert file: %v", err)
	}
	if err := certFile.Close(); err != nil {
		t.Fatalf("close cert file: %v", err)
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	if err := keyFile.Close(); err != nil {
		t.Fatalf("close key file: %v", err)
	}
}
