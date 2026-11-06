package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
)

type serverTLSConfig struct {
	certificate tls.Certificate
	port        int
}

func resolveServerTLSConfig(httpPort int) (*serverTLSConfig, error) {
	certPath, keyPath := os.Getenv("AIMEBU2_TLS_CERT"), os.Getenv("AIMEBU2_TLS_KEY")
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("AIMEBU2_TLS_CERT and AIMEBU2_TLS_KEY must be set together")
	}
	for _, item := range []struct{ name, path string }{{"AIMEBU2_TLS_CERT", certPath}, {"AIMEBU2_TLS_KEY", keyPath}} {
		info, err := os.Stat(item.path)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", item.name, item.path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s=%q must be a regular file", item.name, item.path)
		}
		file, err := os.Open(item.path)
		if err != nil {
			return nil, fmt.Errorf("%s=%q is not readable: %w", item.name, item.path, err)
		}
		file.Close()
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("AIMEBU2_TLS_CERT/AIMEBU2_TLS_KEY are not a usable certificate pair: %w", err)
	}
	port := 9896
	if raw := os.Getenv("AIMEBU2_TLS_PORT"); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("AIMEBU2_TLS_PORT must be a TCP port")
		}
	}
	if port == httpPort {
		return nil, fmt.Errorf("AIMEBU2_TLS_PORT must differ from AIMEBU2_PORT")
	}
	return &serverTLSConfig{certificate: certificate, port: port}, nil
}

func listenTLS(bind string, config *serverTLSConfig) (net.Listener, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(config.port)))
	if err != nil {
		return nil, err
	}
	// h2 is offered so a browser fetches the page's ~240 modules over one multiplexed connection: over HTTP/1.1 it opens six and waits for each
	// round trip, which makes a cold load of the app several seconds on a link with any latency. net/http serves h2 on its own once the
	// handshake negotiates it; without NextProtos here the handshake never does.
	return tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{config.certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}), nil
}
