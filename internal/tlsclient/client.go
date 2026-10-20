// Package tlsclient configures optional self-signed HTTPS access for v2 clients.
package tlsclient

import (
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"time"
)

func InsecureSkipVerify() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AIMEBU2_INSECURE_SKIP_VERIFY"))) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

func NewHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	if InsecureSkipVerify() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // explicit local-development escape hatch
		client.Transport = transport
	}
	return client
}
