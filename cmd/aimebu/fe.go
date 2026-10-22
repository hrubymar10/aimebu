package main

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

// webURL is where the web UI of the server this configuration describes is: the public origin when one is set, otherwise the HTTP address
// the server binds (an unspecified bind address, "0.0.0.0", is reached as localhost).
func webURL() (string, error) {
	public, err := resolvePublicURL()
	if err != nil {
		return "", err
	}
	if public != "" {
		return public, nil
	}
	config, err := resolveServerConfig()
	if err != nil {
		return "", err
	}
	host := config.bind
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(config.port)), nil
}

// browserCommand is the program that opens a URL on this operating system, or "" when there is none (or nobody to show it to: a session
// over SSH, or a Linux machine with no display).
func browserCommand(goos string, getenv func(string) string) string {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return ""
	}
	switch goos {
	case "darwin":
		return "open"
	case "linux", "freebsd", "openbsd", "netbsd":
		if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
			return ""
		}
		return "xdg-open"
	}
	return ""
}

// openBrowser and feGOOS are replaced by tests: they must never start a real browser, and must not depend on the machine they run on.
var (
	openBrowser = func(program, url string) error { return exec.Command(program, url).Run() }
	feGOOS      = runtime.GOOS
)

// feCommand implements `aimebu fe`: open the web UI of the configured server in the browser. Where there is no browser to open (SSH, no
// display) or opening fails, it only prints the address.
func feCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: aimebu fe")
		return 2
	}
	url, err := webURL()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	program := browserCommand(feGOOS, os.Getenv)
	if program == "" {
		fmt.Fprintf(stdout, "Open %s in your browser\n", url)
		return 0
	}
	if err := openBrowser(program, url); err != nil {
		fmt.Fprintf(stdout, "Could not run %s (%v). Open %s in your browser\n", program, err, url)
		return 0
	}
	fmt.Fprintf(stdout, "Opened %s\n", url)
	return 0
}
