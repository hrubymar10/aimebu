package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
)

const defaultTLSPort = "9996"

type serverTLSConfig struct {
	Enabled  bool
	CertFile string
	KeyFile  string
}

func resolveServerTLSConfig() (serverTLSConfig, error) {
	certFile := os.Getenv("AIMEBU_TLS_CERT")
	keyFile := os.Getenv("AIMEBU_TLS_KEY")
	if certFile == "" && keyFile == "" {
		return serverTLSConfig{}, nil
	}
	if certFile == "" || keyFile == "" {
		return serverTLSConfig{}, fmt.Errorf("AIMEBU_TLS_CERT and AIMEBU_TLS_KEY must be set together")
	}
	if err := validateReadableFile("AIMEBU_TLS_CERT", certFile); err != nil {
		return serverTLSConfig{}, err
	}
	if err := validateReadableFile("AIMEBU_TLS_KEY", keyFile); err != nil {
		return serverTLSConfig{}, err
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return serverTLSConfig{}, fmt.Errorf("AIMEBU_TLS_CERT/AIMEBU_TLS_KEY are not a usable certificate pair: %w", err)
	}
	return serverTLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile}, nil
}

func resolveServerTLSPort() (string, error) {
	port := os.Getenv("AIMEBU_TLS_PORT")
	if port == "" {
		port = defaultTLSPort
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", fmt.Errorf("AIMEBU_TLS_PORT invalid port %q: %w", port, err)
	}
	return port, nil
}

func validateReadableFile(envName, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s=%q is not readable: %w", envName, path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s=%q could not be closed after validation: %w", envName, path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s=%q could not be statted: %w", envName, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s=%q must be a file, not a directory", envName, path)
	}
	return nil
}
