// Package tlsclient configures optional self-signed HTTPS access for v2 clients.
package tlsclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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

// NewTransportCheckedClient is NewHTTPClient that applies CheckTransport to every redirect it is sent along, so a server's 307/308 from https to
// plain http on another host cannot make it resend a token or key in the clear.
func NewTransportCheckedClient(timeout time.Duration, allowInsecure bool) *http.Client {
	client := NewHTTPClient(timeout)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return CheckTransport(req.URL.String(), allowInsecure)
	}
	return client
}

// loopbackHost says whether a host (with or without a port) is this machine: localhost, 127.0.0.0/8 or ::1.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(strings.TrimSuffix(host, ".")), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PlainHTTPToARemoteHost says whether a server origin is plain http to a machine that is not this one: the machine key, and everything an agent says, would
// cross the network readable by anyone on the way.
func PlainHTTPToARemoteHost(serverURL string) bool {
	parsed, err := url.Parse(serverURL)
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	return !loopbackHost(parsed.Host)
}

// CheckTransport refuses a plain-http origin on another machine unless the caller was told to allow it (the --insecure flag): https, or this machine, are
// always fine.
func CheckTransport(serverURL string, allowInsecure bool) error {
	if allowInsecure || !PlainHTTPToARemoteHost(serverURL) {
		return nil
	}
	return fmt.Errorf("%s is plain http to another machine: the machine key and everything an agent says would cross the network unencrypted. Serve the server over https (docs/tls.md), or pass --insecure if you accept that on this network", serverURL)
}
