package tlsclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlainHTTPToARemoteHost(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]bool{
		"http://127.0.0.1:9897":         false,
		"http://127.0.0.2:9897":         false, // all of 127.0.0.0/8
		"http://[::1]:9897":             false,
		"http://localhost:9897":         false,
		"http://LOCALHOST.:9897":        false,
		"https://example.com":           false, // https is always fine
		"https://10.0.0.5:9896":         false,
		"http://10.0.0.5:9897":          true,
		"http://192.168.1.20":           true,
		"http://example.com":            true,
		"http://host.docker.internal:1": true, // not this machine as far as a network is concerned
		"http://localhost.evil.example": true,
		"http://127.0.0.1.evil.example": true,
		"http://[2001:db8::1]:9897":     true,
		"ftp://10.0.0.5":                false, // not an origin the client uses at all
		"::not a url::":                 false,
	} {
		if got := PlainHTTPToARemoteHost(url); got != want {
			t.Errorf("PlainHTTPToARemoteHost(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestCheckTransportRefusesPlainHTTPToAnotherMachineUnlessTold(t *testing.T) {
	t.Parallel()
	if err := CheckTransport("http://10.0.0.5:9897", false); err == nil || !strings.Contains(err.Error(), "--insecure") || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("a plain-http remote origin is refused with what to do about it: %v", err)
	}
	for _, ok := range []struct {
		url      string
		insecure bool
	}{{"http://10.0.0.5:9897", true}, {"http://127.0.0.1:9897", false}, {"https://10.0.0.5:9896", false}} {
		if err := CheckTransport(ok.url, ok.insecure); err != nil {
			t.Errorf("CheckTransport(%q, %v) = %v", ok.url, ok.insecure, err)
		}
	}
}

// A redirect is a new request: one that sends the client from this machine, or from https, to plain http on another host is refused before it
// is made, so a token or key in the first request is not resent in the clear.
func TestRedirectToPlainHTTPOnAnotherHostIsRefused(t *testing.T) {
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.7:9/pair", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	client := NewTransportCheckedClient(5*time.Second, false)
	req, _ := http.NewRequest(http.MethodPost, redirecting.URL, strings.NewReader(`{"token":"secret"}`))
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "plain http to another machine") {
		t.Fatalf("a redirect to plain http on another host: %v", err)
	}
	// The check itself, for the hops that stay on this machine or are https, and with the risk accepted.
	check := client.CheckRedirect
	for _, target := range []string{"http://127.0.0.1:9/x", "https://example.com/x", "http://localhost:9/x"} {
		next, _ := http.NewRequest(http.MethodGet, target, nil)
		if err := check(next, nil); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	accepting := NewTransportCheckedClient(5*time.Second, true)
	next, _ := http.NewRequest(http.MethodGet, "http://203.0.113.7:9/x", nil)
	if err := accepting.CheckRedirect(next, nil); err != nil {
		t.Fatalf("--insecure accepts it: %v", err)
	}
	via := make([]*http.Request, 10)
	if err := client.CheckRedirect(next, via); err == nil {
		t.Fatal("redirects are not bounded")
	}
}
