package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWebURLFollowsTheServerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, bind, port, public, want string
	}{
		{"defaults", "", "", "", "http://127.0.0.1:9897"},
		{"port", "", "9911", "", "http://127.0.0.1:9911"},
		{"all interfaces are reached as localhost", "0.0.0.0", "9911", "", "http://localhost:9911"},
		{"ipv6 loopback", "::1", "9911", "", "http://[::1]:9911"},
		{"unspecified ipv6", "::", "9911", "", "http://localhost:9911"},
		{"a public origin wins", "0.0.0.0", "9911", "https://bus.example.test", "https://bus.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIMEBU2_BIND", tc.bind)
			t.Setenv("AIMEBU2_PORT", tc.port)
			t.Setenv("AIMEBU2_PUBLIC_URL", tc.public)
			got, err := webURL()
			if err != nil || got != tc.want {
				t.Fatalf("webURL = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Setenv("AIMEBU2_BIND", "not-an-ip")
	t.Setenv("AIMEBU2_PUBLIC_URL", "")
	if _, err := webURL(); err == nil {
		t.Fatal("a bad bind address was accepted")
	}
}

func TestBrowserCommandOnlyWhereSomeoneCanSeeIt(t *testing.T) {
	env := func(pairs ...string) func(string) string {
		values := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			values[pairs[i]] = pairs[i+1]
		}
		return func(name string) string { return values[name] }
	}
	for _, tc := range []struct {
		name, goos string
		getenv     func(string) string
		want       string
	}{
		{"mac", "darwin", env(), "open"},
		{"mac over ssh", "darwin", env("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22"), ""},
		{"linux with a display", "linux", env("DISPLAY", ":0"), "xdg-open"},
		{"linux with wayland", "linux", env("WAYLAND_DISPLAY", "wayland-0"), "xdg-open"},
		{"linux with no display", "linux", env(), ""},
		{"linux over ssh with a forwarded display", "linux", env("DISPLAY", "localhost:10", "SSH_TTY", "/dev/pts/1"), ""},
		{"an unknown system", "plan9", env("DISPLAY", ":0"), ""},
	} {
		if got := browserCommand(tc.goos, tc.getenv); got != tc.want {
			t.Errorf("%s: browserCommand = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFeOpensTheBrowserOrPrintsTheAddress(t *testing.T) {
	t.Setenv("AIMEBU2_BIND", "")
	t.Setenv("AIMEBU2_PORT", "9912")
	t.Setenv("AIMEBU2_PUBLIC_URL", "")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	const want = "http://127.0.0.1:9912"
	var opened []string
	previousOpen, previousGOOS := openBrowser, feGOOS
	t.Cleanup(func() { openBrowser, feGOOS = previousOpen, previousGOOS })
	openBrowser = func(program, url string) error { opened = append(opened, program+" "+url); return nil }
	feGOOS = "linux"

	var stdout, stderr bytes.Buffer
	if code := run([]string{"fe"}, &stdout, &stderr); code != 0 || len(opened) != 1 || opened[0] != "xdg-open "+want || !strings.Contains(stdout.String(), "Opened "+want) {
		t.Fatalf("fe = %d, opened %v, printed %q, %q", code, opened, stdout.String(), stderr.String())
	}

	// Over ssh nothing is started; the address is printed.
	opened, stdout = nil, bytes.Buffer{}
	t.Setenv("SSH_CONNECTION", "1.2.3.4 1 5.6.7.8 22")
	if code := run([]string{"fe"}, &stdout, &stderr); code != 0 || len(opened) != 0 || !strings.Contains(stdout.String(), "Open "+want+" in your browser") {
		t.Fatalf("over ssh: code %d, opened %v, printed %q", code, opened, stdout.String())
	}

	// A browser that cannot be started leaves the address, and is not a failure of the command.
	t.Setenv("SSH_CONNECTION", "")
	openBrowser = func(program, url string) error { return errors.New("not installed") }
	stdout = bytes.Buffer{}
	if code := run([]string{"fe"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), want) || !strings.Contains(stdout.String(), "not installed") {
		t.Fatalf("a failing browser: code %d, printed %q", code, stdout.String())
	}

	if code := run([]string{"fe", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("fe with an argument = %d", code)
	}
	t.Setenv("AIMEBU2_BIND", "not-an-ip")
	if code := run([]string{"fe"}, &stdout, &stderr); code != 1 {
		t.Fatalf("fe with a bad configuration = %d", code)
	}
}
